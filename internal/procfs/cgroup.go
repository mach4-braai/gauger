package procfs

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Container holds the cumulative cgroup v2 usage of one container running
// directly on the runner, found under
// /sys/fs/cgroup/system.slice/docker-<id>.scope.
type Container struct {
	ID          string
	CPUUsec     uint64
	MemoryBytes uint64
	// ImageName is empty when the Docker API is not reachable without root.
	ImageName string
}

// containers reads every docker-<id>.scope cgroup under system.slice. It
// never fails: a runner without Docker, or without the cgroup v2 unified
// hierarchy, has no such cgroups, and a container that exits mid-read is
// skipped rather than failing the whole sample.
func (r *Reader) containers() []Container {
	cgroupRoot := filepath.Join(r.Sys, "fs/cgroup")
	if _, err := os.Stat(filepath.Join(cgroupRoot, "cgroup.controllers")); err != nil {
		return nil // no cgroup v2 unified hierarchy
	}
	entries, err := os.ReadDir(filepath.Join(cgroupRoot, "system.slice"))
	if err != nil {
		return nil // no system.slice, so no Docker on this host
	}
	var out []Container
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !strings.HasPrefix(name, "docker-") || !strings.HasSuffix(name, ".scope") {
			continue
		}
		dir := filepath.Join(cgroupRoot, "system.slice", name)
		usec, err := cpuUsageUsec(dir)
		if err != nil {
			continue
		}
		mem, err := memoryCurrent(dir)
		if err != nil {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, "docker-"), ".scope")
		c := Container{ID: id, CPUUsec: usec, MemoryBytes: mem}
		if image, ok := r.dockerImageName(id); ok {
			c.ImageName = image
		}
		out = append(out, c)
	}
	return out
}

// cpuUsageUsec reads the cumulative usage_usec field of a cgroup's cpu.stat.
func cpuUsageUsec(dir string) (uint64, error) {
	path := filepath.Join(dir, "cpu.stat")
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[0] == "usage_usec" {
			return strconv.ParseUint(fields[1], 10, 64)
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("no usage_usec in %s", path)
}

// memoryCurrent reads a cgroup's memory.current, in bytes.
func memoryCurrent(dir string) (uint64, error) {
	data, err := os.ReadFile(filepath.Join(dir, "memory.current"))
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
}
