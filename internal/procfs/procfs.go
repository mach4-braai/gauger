// Package procfs reads runner-wide CPU, memory, disk and network counters from /proc.
package procfs

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const sectorBytes = 512

// CPU holds the aggregate "cpu" line of /proc/stat, in clock ticks.
type CPU struct {
	User, Nice, System, Idle, IOWait, IRQ, SoftIRQ, Steal uint64
}

// Total is the sum of every mode. Guest time is already counted in User and Nice.
func (c CPU) Total() uint64 {
	return c.User + c.Nice + c.System + c.Idle + c.IOWait + c.IRQ + c.SoftIRQ + c.Steal
}

type Processes struct {
	Running, Blocked uint64
}

// Memory holds /proc/meminfo values in bytes.
type Memory struct {
	Total, Free, Available, Buffers, Cached, SReclaimable uint64
	SwapTotal, SwapFree                                   uint64
}

// Disk holds the cumulative counters of one whole disk from /proc/diskstats.
type Disk struct {
	Name                  string
	ReadBytes, WriteBytes uint64
	ReadOps, WriteOps     uint64
}

// Interface holds the cumulative byte counters of one interface from /proc/net/dev.
type Interface struct {
	Name             string
	RxBytes, TxBytes uint64
}

// Sample is one reading of every source.
type Sample struct {
	Time       time.Time
	CPU        CPU
	Processes  Processes
	Memory     Memory
	Disks      []Disk
	Interfaces []Interface
}

// Reader reads samples from a proc and sys tree. Tests point it at fixtures.
type Reader struct {
	Proc string
	Sys  string

	disks      map[string]bool
	interfaces map[string]bool
}

// NewReader returns a Reader for the live /proc and /sys.
func NewReader() *Reader {
	return &Reader{Proc: "/proc", Sys: "/sys"}
}

// Read takes one sample. A failure in one source fails the whole sample, so a
// batch never mixes complete and partial readings.
func (r *Reader) Read(now time.Time) (Sample, error) {
	s := Sample{Time: now}
	var err error
	if s.CPU, s.Processes, err = r.stat(); err != nil {
		return Sample{}, err
	}
	if s.Memory, err = r.memory(); err != nil {
		return Sample{}, err
	}
	if s.Disks, err = r.diskstats(); err != nil {
		return Sample{}, err
	}
	if s.Interfaces, err = r.netdev(); err != nil {
		return Sample{}, err
	}
	return s, nil
}

func (r *Reader) stat() (CPU, Processes, error) {
	data, err := os.ReadFile(filepath.Join(r.Proc, "stat"))
	if err != nil {
		return CPU{}, Processes{}, err
	}
	line, rest, _ := bytes.Cut(data, []byte("\n"))
	fields := strings.Fields(string(line))
	if len(fields) < 9 || fields[0] != "cpu" {
		return CPU{}, Processes{}, fmt.Errorf("unexpected first line in /proc/stat: %q", line)
	}
	var v [8]uint64
	for i := range v {
		if v[i], err = strconv.ParseUint(fields[i+1], 10, 64); err != nil {
			return CPU{}, Processes{}, fmt.Errorf("parse /proc/stat: %w", err)
		}
	}
	cpu := CPU{User: v[0], Nice: v[1], System: v[2], Idle: v[3], IOWait: v[4], IRQ: v[5], SoftIRQ: v[6], Steal: v[7]}

	var procs Processes
	dst := map[string]*uint64{
		"procs_running": &procs.Running,
		"procs_blocked": &procs.Blocked,
	}
	scanner := bufio.NewScanner(bytes.NewReader(rest))
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) < 2 {
			continue
		}
		p, ok := dst[parts[0]]
		if !ok {
			continue
		}
		if *p, err = strconv.ParseUint(parts[1], 10, 64); err != nil {
			return CPU{}, Processes{}, fmt.Errorf("parse /proc/stat %s: %w", parts[0], err)
		}
	}
	if err := scanner.Err(); err != nil {
		return CPU{}, Processes{}, err
	}
	return cpu, procs, nil
}

func (r *Reader) memory() (Memory, error) {
	f, err := os.Open(filepath.Join(r.Proc, "meminfo"))
	if err != nil {
		return Memory{}, err
	}
	defer f.Close()
	var m Memory
	fields := map[string]*uint64{
		"MemTotal:":     &m.Total,
		"MemFree:":      &m.Free,
		"MemAvailable:": &m.Available,
		"Buffers:":      &m.Buffers,
		"Cached:":       &m.Cached,
		"SReclaimable:": &m.SReclaimable,
		"SwapTotal:":    &m.SwapTotal,
		"SwapFree:":     &m.SwapFree,
	}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) < 2 {
			continue
		}
		dst, ok := fields[parts[0]]
		if !ok {
			continue
		}
		kib, err := strconv.ParseUint(parts[1], 10, 64)
		if err != nil {
			return Memory{}, fmt.Errorf("parse /proc/meminfo %s: %w", parts[0], err)
		}
		*dst = kib * 1024
	}
	if err := scanner.Err(); err != nil {
		return Memory{}, err
	}
	if m.Total == 0 {
		return Memory{}, errors.New("no MemTotal in /proc/meminfo")
	}
	return m, nil
}

func (r *Reader) CPUModel() (string, error) {
	f, err := os.Open(filepath.Join(r.Proc, "cpuinfo"))
	if err != nil {
		return "", err
	}
	defer f.Close()
	var modelName, implementer, part string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "model name":
			if modelName == "" {
				modelName = value
			}
		case "CPU implementer":
			if implementer == "" {
				implementer = value
			}
		case "CPU part":
			if part == "" {
				part = value
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if modelName != "" {
		return modelName, nil
	}
	if implementer != "" && part != "" {
		return implementer + " " + part, nil
	}
	return "", nil
}

// wholeDisk reports whether name is a disk rather than a partition or a
// loop or RAM device. Only whole disks appear in /sys/block.
func (r *Reader) wholeDisk(name string) bool {
	if known, ok := r.disks[name]; ok {
		return known
	}
	if r.disks == nil {
		r.disks = map[string]bool{}
	}
	whole := !strings.HasPrefix(name, "loop") && !strings.HasPrefix(name, "ram") && !strings.HasPrefix(name, "zram")
	if whole {
		_, err := os.Stat(filepath.Join(r.Sys, "block", name))
		whole = err == nil
	}
	r.disks[name] = whole
	return whole
}

func (r *Reader) diskstats() ([]Disk, error) {
	data, err := os.ReadFile(filepath.Join(r.Proc, "diskstats"))
	if err != nil {
		return nil, err
	}
	var disks []Disk
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 || !r.wholeDisk(fields[2]) {
			continue
		}
		var v [4]uint64
		for i, idx := range [4]int{3, 5, 7, 9} {
			if v[i], err = strconv.ParseUint(fields[idx], 10, 64); err != nil {
				return nil, fmt.Errorf("parse /proc/diskstats %s: %w", fields[2], err)
			}
		}
		disks = append(disks, Disk{
			Name:       fields[2],
			ReadOps:    v[0],
			ReadBytes:  v[1] * sectorBytes,
			WriteOps:   v[2],
			WriteBytes: v[3] * sectorBytes,
		})
	}
	return disks, nil
}

// hardware reports whether an interface is backed by a device, which leaves
// out lo, docker0, veth pairs and other bridges that would count traffic
// twice. It also leaves out interfaces enslaved to another interface.
func (r *Reader) hardware(name string) bool {
	if known, ok := r.interfaces[name]; ok {
		return known
	}
	if r.interfaces == nil {
		r.interfaces = map[string]bool{}
	}
	_, err := os.Stat(filepath.Join(r.Sys, "class", "net", name, "device"))
	hasDevice := err == nil
	_, err = os.Lstat(filepath.Join(r.Sys, "class", "net", name, "master"))
	enslaved := err == nil
	known := hasDevice && !enslaved
	r.interfaces[name] = known
	return known
}

func (r *Reader) netdev() ([]Interface, error) {
	data, err := os.ReadFile(filepath.Join(r.Proc, "net", "dev"))
	if err != nil {
		return nil, err
	}
	var all, hardware []Interface
	for _, line := range strings.Split(string(data), "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		fields := strings.Fields(rest)
		if len(fields) < 9 || name == "lo" {
			continue
		}
		rx, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse /proc/net/dev %s: %w", name, err)
		}
		tx, err := strconv.ParseUint(fields[8], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse /proc/net/dev %s: %w", name, err)
		}
		iface := Interface{Name: name, RxBytes: rx, TxBytes: tx}
		all = append(all, iface)
		if r.hardware(name) {
			hardware = append(hardware, iface)
		}
	}
	if len(hardware) == 0 {
		return all, nil
	}
	return hardware, nil
}
