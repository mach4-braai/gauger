package procfs

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"
)

const stat = `cpu  100 5 50 1000 20 3 2 1 0 0
cpu0 50 2 25 500 10 1 1 0 0 0
intr 12345
`

const meminfo = `MemTotal:       16384000 kB
MemFree:         8000000 kB
MemAvailable:   12000000 kB
Buffers:          100000 kB
Cached:          3000000 kB
SwapCached:            0 kB
SReclaimable:     200000 kB
`

const diskstats = `   7       0 loop0 90 0 1800 10 0 0 0 0 0 20 10 0 0 0 0 0 0
   8       0 sda 1000 10 20000 300 500 20 8000 400 0 700 700 0 0 0 0 0 0
   8       1 sda1 900 10 18000 290 480 20 7900 390 0 690 680 0 0 0 0 0 0
 259       0 nvme0n1 10 0 80 1 20 0 160 2 0 3 3 0 0 0 0 0 0
`

const netdev = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:    5000      50    0    0    0     0          0         0     5000      50    0    0    0     0       0          0
  eth0: 1000000    800    0    0    0     0          0         0   200000     600    0    0    0     0       0          0
docker0:   3000      30    0    0    0     0          0         0     4000      40    0    0    0     0       0          0
`

func fixture(t *testing.T, withDevices bool) *Reader {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"proc/stat":      stat,
		"proc/meminfo":   meminfo,
		"proc/diskstats": diskstats,
		"proc/net/dev":   netdev,
	}
	for path, content := range files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dirs := []string{"sys/block/loop0", "sys/block/sda", "sys/block/nvme0n1", "sys/class/net/lo", "sys/class/net/docker0", "sys/class/net/eth0"}
	if withDevices {
		dirs = append(dirs, "sys/class/net/eth0/device")
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &Reader{Proc: filepath.Join(root, "proc"), Sys: filepath.Join(root, "sys")}
}

func TestRead(t *testing.T) {
	now := time.Unix(1700000000, 0)
	got, err := fixture(t, true).Read(now)
	if err != nil {
		t.Fatal(err)
	}
	want := Sample{
		Time: now,
		CPU:  CPU{User: 100, Nice: 5, System: 50, Idle: 1000, IOWait: 20, IRQ: 3, SoftIRQ: 2, Steal: 1},
		Memory: Memory{
			Total:        16384000 * 1024,
			Free:         8000000 * 1024,
			Available:    12000000 * 1024,
			Buffers:      100000 * 1024,
			Cached:       3000000 * 1024,
			SReclaimable: 200000 * 1024,
		},
		Disks: []Disk{
			{Name: "sda", ReadOps: 1000, ReadBytes: 20000 * 512, WriteOps: 500, WriteBytes: 8000 * 512},
			{Name: "nvme0n1", ReadOps: 10, ReadBytes: 80 * 512, WriteOps: 20, WriteBytes: 160 * 512},
		},
		Interfaces: []Interface{{Name: "eth0", RxBytes: 1000000, TxBytes: 200000}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Read:\n got %+v\nwant %+v", got, want)
	}
}

func TestReadKeepsVirtualInterfacesWhenNoneHasADevice(t *testing.T) {
	got, err := fixture(t, false).Read(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	want := []Interface{
		{Name: "eth0", RxBytes: 1000000, TxBytes: 200000},
		{Name: "docker0", RxBytes: 3000, TxBytes: 4000},
	}
	if !reflect.DeepEqual(got.Interfaces, want) {
		t.Fatalf("Interfaces = %+v, want %+v", got.Interfaces, want)
	}
}

func TestReadFailsOnMalformedStat(t *testing.T) {
	r := fixture(t, true)
	if err := os.WriteFile(filepath.Join(r.Proc, "stat"), []byte("cpu 1 2 x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(time.Now()); err == nil {
		t.Fatal("Read succeeded on a malformed /proc/stat")
	}
}

func addProcess(t *testing.T, r *Reader, pid int, comm string, utime, stime, rssKiB uint64) {
	t.Helper()
	dir := filepath.Join(r.Proc, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stat := fmt.Sprintf("%d (%s) R 1 1 1 0 -1 0 0 0 0 0 %d %d 0 0 20 0 1 0 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0 0 0 0 0 0 0 0 0\n", pid, comm, utime, stime)
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	status := fmt.Sprintf("Name:\t%s\nVmRSS:\t %d kB\n", comm, rssKiB)
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProcessesRanksByCPUDeltaAndByMemory(t *testing.T) {
	r := fixture(t, true)
	addProcess(t, r, 100, "compile", 500, 100, 4000)
	addProcess(t, r, 200, "bash", 50, 10, 1000)
	addProcess(t, r, 300, "go", 10, 5, 9000)

	topCPU, topMemory := r.processes()
	wantCPU := []Process{
		{Executable: "compile", CPUSeconds: 6, RSSBytes: 4000 * 1024},
		{Executable: "bash", CPUSeconds: 0.6, RSSBytes: 1000 * 1024},
		{Executable: "go", CPUSeconds: 0.15, RSSBytes: 9000 * 1024},
	}
	if !reflect.DeepEqual(topCPU, wantCPU) {
		t.Fatalf("first walk topCPU = %+v, want %+v", topCPU, wantCPU)
	}
	wantMemory := []Process{
		{Executable: "go", CPUSeconds: 0.15, RSSBytes: 9000 * 1024},
		{Executable: "compile", CPUSeconds: 6, RSSBytes: 4000 * 1024},
		{Executable: "bash", CPUSeconds: 0.6, RSSBytes: 1000 * 1024},
	}
	if !reflect.DeepEqual(topMemory, wantMemory) {
		t.Fatalf("topMemory = %+v, want %+v", topMemory, wantMemory)
	}

	addProcess(t, r, 100, "compile", 510, 100, 4000)
	addProcess(t, r, 200, "bash", 550, 200, 1000)
	addProcess(t, r, 300, "go", 10, 5, 9000)

	topCPU, _ = r.processes()
	if len(topCPU) == 0 || topCPU[0].Executable != "bash" {
		t.Fatalf("second walk topCPU = %+v, want bash ranked first by its CPU delta", topCPU)
	}
}

func TestProcessesSkipsAProcessMissingStatus(t *testing.T) {
	r := fixture(t, true)
	addProcess(t, r, 100, "compile", 500, 100, 4000)
	dir := filepath.Join(r.Proc, "200")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stat := "200 (bash) R 1 1 1 0 -1 0 0 0 0 0 50 10 0 0 20 0 1 0 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0 0 0 0 0 0 0 0 0\n"
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}

	topCPU, topMemory := r.processes()
	if len(topCPU) != 1 || topCPU[0].Executable != "compile" {
		t.Fatalf("topCPU = %+v, want only compile: a vanished PID must not appear or fail the walk", topCPU)
	}
	if len(topMemory) != 1 || topMemory[0].Executable != "compile" {
		t.Fatalf("topMemory = %+v, want only compile", topMemory)
	}
}

func TestProcessesSkipsAPIDWithNoStatFile(t *testing.T) {
	r := fixture(t, true)
	if err := os.MkdirAll(filepath.Join(r.Proc, "999"), 0o755); err != nil {
		t.Fatal(err)
	}
	topCPU, topMemory := r.processes()
	if len(topCPU) != 0 || len(topMemory) != 0 {
		t.Fatalf("topCPU = %v, topMemory = %v, want neither: a PID with no stat file must not fail the walk", topCPU, topMemory)
	}
}

func TestReadWalksProcessesEveryProcessWalkEvery(t *testing.T) {
	r := fixture(t, true)
	addProcess(t, r, 100, "compile", 500, 100, 4000)

	for i := range processWalkEvery * 2 {
		got, err := r.Read(time.Unix(int64(i), 0))
		if err != nil {
			t.Fatal(err)
		}
		wantWalk := i%processWalkEvery == 0
		if gotWalk := len(got.TopCPU) > 0; gotWalk != wantWalk {
			t.Errorf("Read #%d: walked = %v, want %v", i, gotWalk, wantWalk)
		}
	}
}
