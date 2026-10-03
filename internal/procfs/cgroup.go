package procfs

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Container struct {
	ID          string
	CPUUsec     uint64
	MemoryBytes uint64
	ImageName   string
}

func (r *Reader) containers() []Container {
	cgroupRoot := filepath.Join(r.Sys, "fs/cgroup")
	if _, err := os.Stat(filepath.Join(cgroupRoot, "cgroup.controllers")); err != nil {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(cgroupRoot, "system.slice"))
	if err != nil {
		return nil
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

func memoryCurrent(dir string) (uint64, error) {
	data, err := os.ReadFile(filepath.Join(dir, "memory.current"))
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
}
