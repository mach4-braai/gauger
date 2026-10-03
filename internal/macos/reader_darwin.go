package macos

import (
	"bufio"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"

	"github.com/mach4-braai/gauger/internal/procfs"
)

const (
	hostVMInfo       = 2
	hostCPULoadInfo  = 3
	vmStatisticsSize = 0xf

	cpuStateUser   = 0
	cpuStateSystem = 1
	cpuStateIdle   = 2
	cpuStateNice   = 3

	kernSuccess = 0
)

type cpuLoadInfo struct {
	Ticks [4]uint32
}

type vmStatistics struct {
	Free, Active, Inactive, Wire uint32
	_                            [vmStatisticsSize - 4]uint32
}

type Reader struct {
	machHostSelf   func() uint32
	hostStatistics func(host uint32, flavor int32, out unsafe.Pointer, count *uint32) int32
	pageSize       uint64
}

func NewReader() (*Reader, error) {
	handle, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, fmt.Errorf("open libSystem: %w", err)
	}
	r := &Reader{}
	purego.RegisterLibFunc(&r.machHostSelf, handle, "mach_host_self")
	purego.RegisterLibFunc(&r.hostStatistics, handle, "host_statistics")
	pageSize, err := unix.SysctlUint32("hw.pagesize")
	if err != nil {
		return nil, fmt.Errorf("sysctl hw.pagesize: %w", err)
	}
	r.pageSize = uint64(pageSize)
	return r, nil
}

func (r *Reader) Read(now time.Time) (procfs.Sample, error) {
	s := procfs.Sample{Time: now}
	var err error
	if s.CPU, err = r.cpu(); err != nil {
		return procfs.Sample{}, err
	}
	if s.Memory, err = r.memory(); err != nil {
		return procfs.Sample{}, err
	}
	if s.Disks, err = diskstats(); err != nil {
		return procfs.Sample{}, err
	}
	if s.Interfaces, err = netdev(); err != nil {
		return procfs.Sample{}, err
	}
	return s, nil
}

func (r *Reader) cpu() (procfs.CPU, error) {
	var info cpuLoadInfo
	count := uint32(len(info.Ticks))
	if status := r.hostStatistics(r.machHostSelf(), hostCPULoadInfo, unsafe.Pointer(&info), &count); status != kernSuccess {
		return procfs.CPU{}, fmt.Errorf("host_statistics HOST_CPU_LOAD_INFO: status %d", status)
	}
	return procfs.CPU{
		User:   uint64(info.Ticks[cpuStateUser]),
		Nice:   uint64(info.Ticks[cpuStateNice]),
		System: uint64(info.Ticks[cpuStateSystem]),
		Idle:   uint64(info.Ticks[cpuStateIdle]),
	}, nil
}

func (r *Reader) memory() (procfs.Memory, error) {
	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return procfs.Memory{}, fmt.Errorf("sysctl hw.memsize: %w", err)
	}
	var vm vmStatistics
	count := uint32(vmStatisticsSize)
	if status := r.hostStatistics(r.machHostSelf(), hostVMInfo, unsafe.Pointer(&vm), &count); status != kernSuccess {
		return procfs.Memory{}, fmt.Errorf("host_statistics HOST_VM_INFO: status %d", status)
	}
	free := uint64(vm.Free) * r.pageSize
	active := uint64(vm.Active) * r.pageSize
	inactive := uint64(vm.Inactive) * r.pageSize
	return procfs.Memory{
		Total:     total,
		Free:      free,
		Available: free + inactive,
		Cached:    active + inactive,
	}, nil
}

var (
	topLevelDisk  = regexp.MustCompile(`(?m)^\+-o `)
	statisticsRe  = regexp.MustCompile(`"Statistics" = \{([^}]*)\}`)
	bsdNameRe     = regexp.MustCompile(`"BSD Name" = "([^"]+)"`)
	diskStatField = regexp.MustCompile(`"(Bytes \(Read\)|Bytes \(Write\)|Operations \(Read\)|Operations \(Write\))"=(\d+)`)
)

func diskstats() ([]procfs.Disk, error) {
	out, err := exec.Command("ioreg", "-c", "IOBlockStorageDriver", "-r", "-l", "-w0").Output()
	if err != nil {
		return nil, fmt.Errorf("ioreg: %w", err)
	}
	var disks []procfs.Disk
	for _, block := range topLevelDisk.Split(string(out), -1)[1:] {
		stats := statisticsRe.FindStringSubmatch(block)
		name := bsdNameRe.FindStringSubmatch(block)
		if stats == nil || name == nil {
			continue
		}
		d := procfs.Disk{Name: name[1]}
		for _, f := range diskStatField.FindAllStringSubmatch(stats[1], -1) {
			v, err := strconv.ParseUint(f[2], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parse ioreg statistic %s: %w", f[1], err)
			}
			switch f[1] {
			case "Bytes (Read)":
				d.ReadBytes = v
			case "Bytes (Write)":
				d.WriteBytes = v
			case "Operations (Read)":
				d.ReadOps = v
			case "Operations (Write)":
				d.WriteOps = v
			}
		}
		disks = append(disks, d)
	}
	return disks, nil
}

func netdev() ([]procfs.Interface, error) {
	out, err := exec.Command("netstat", "-ibn").Output()
	if err != nil {
		return nil, fmt.Errorf("netstat: %w", err)
	}
	var all, hardware []procfs.Interface
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 || len(fields) > 11 || !strings.HasPrefix(fields[2], "<Link#") {
			continue
		}
		name := strings.TrimSuffix(fields[0], "*")
		if name == "lo0" {
			continue
		}
		base := len(fields) - 7
		rx, err := strconv.ParseUint(fields[base+2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse netstat %s: %w", name, err)
		}
		tx, err := strconv.ParseUint(fields[base+5], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse netstat %s: %w", name, err)
		}
		iface := procfs.Interface{Name: name, RxBytes: rx, TxBytes: tx}
		all = append(all, iface)
		if strings.HasPrefix(name, "en") {
			hardware = append(hardware, iface)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(hardware) == 0 {
		return all, nil
	}
	return hardware, nil
}
