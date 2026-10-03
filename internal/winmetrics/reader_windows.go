package winmetrics

import (
	"fmt"
	"strconv"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/mach4-braai/gauger/internal/procfs"
)

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procGetSystemTimes       = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
)

type filetime struct {
	Low, High uint32
}

func (f filetime) ticks() uint64 { return uint64(f.High)<<32 | uint64(f.Low) }

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

type Reader struct{}

func NewReader() (*Reader, error) {
	return &Reader{}, nil
}

func (r *Reader) Read(now time.Time) (procfs.Sample, error) {
	s := procfs.Sample{Time: now}
	var err error
	if s.CPU, err = cpu(); err != nil {
		return procfs.Sample{}, err
	}
	if s.Memory, err = memory(); err != nil {
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

func cpu() (procfs.CPU, error) {
	var idle, kernel, user filetime
	ret, _, callErr := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if ret == 0 {
		return procfs.CPU{}, fmt.Errorf("GetSystemTimes: %w", callErr)
	}
	idleTicks := idle.ticks()
	return procfs.CPU{
		User:   user.ticks(),
		Idle:   idleTicks,
		System: kernel.ticks() - idleTicks,
	}, nil
}

func memory() (procfs.Memory, error) {
	m := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	ret, _, callErr := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if ret == 0 {
		return procfs.Memory{}, fmt.Errorf("GlobalMemoryStatusEx: %w", callErr)
	}
	return procfs.Memory{Total: m.TotalPhys, Free: m.AvailPhys, Available: m.AvailPhys}, nil
}

const (
	ioctlDiskPerformance = 0x70020
	maxPhysicalDrives    = 16
)

type diskPerformance struct {
	BytesRead           int64
	BytesWritten        int64
	ReadTime            int64
	WriteTime           int64
	IdleTime            int64
	ReadCount           uint32
	WriteCount          uint32
	QueueDepth          uint32
	SplitCount          uint32
	QueryTime           int64
	StorageDeviceNumber uint32
	StorageManagerName  [8]uint16
}

func diskstats() ([]procfs.Disk, error) {
	var disks []procfs.Disk
	for i := range maxPhysicalDrives {
		name := "PhysicalDrive" + strconv.Itoa(i)
		path, err := windows.UTF16PtrFromString(`\\.\` + name)
		if err != nil {
			continue
		}
		h, err := windows.CreateFile(path, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
		if err != nil {
			continue
		}
		var perf diskPerformance
		var returned uint32
		ioErr := windows.DeviceIoControl(h, ioctlDiskPerformance, nil, 0, (*byte)(unsafe.Pointer(&perf)), uint32(unsafe.Sizeof(perf)), &returned, nil)
		windows.CloseHandle(h)
		if ioErr != nil {
			continue
		}
		disks = append(disks, procfs.Disk{
			Name:       name,
			ReadBytes:  uint64(perf.BytesRead),
			WriteBytes: uint64(perf.BytesWritten),
			ReadOps:    uint64(perf.ReadCount),
			WriteOps:   uint64(perf.WriteCount),
		})
	}
	return disks, nil
}

const (
	ifTypeSoftwareLoopback = 24
	ifTypeTunnel           = 131
)

func netdev() ([]procfs.Interface, error) {
	var table *windows.MibIfTable2
	if err := windows.GetIfTable2Ex(windows.MibIfTableNormal, &table); err != nil {
		return nil, fmt.Errorf("GetIfTable2Ex: %w", err)
	}
	defer windows.FreeMibTable(unsafe.Pointer(table))

	rows := unsafe.Slice((*windows.MibIfRow2)(unsafe.Pointer(&table.Table[0])), table.NumEntries)
	var all, hardware []procfs.Interface
	for _, row := range rows {
		iface := procfs.Interface{Name: windows.UTF16ToString(row.Alias[:]), RxBytes: row.InOctets, TxBytes: row.OutOctets}
		all = append(all, iface)
		if row.Type != ifTypeSoftwareLoopback && row.Type != ifTypeTunnel && row.PhysicalAddressLength > 0 {
			hardware = append(hardware, iface)
		}
	}
	if len(hardware) == 0 {
		return all, nil
	}
	return hardware, nil
}
