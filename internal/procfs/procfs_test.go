package procfs

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

const stat = `cpu  100 5 50 1000 20 3 2 1 0 0
cpu0 50 2 25 500 10 1 1 0 0 0
intr 12345
procs_running 4
procs_blocked 1
`

const meminfo = `MemTotal:       16384000 kB
MemFree:         8000000 kB
MemAvailable:   12000000 kB
Buffers:          100000 kB
Cached:          3000000 kB
SwapCached:            0 kB
SReclaimable:     200000 kB
SwapTotal:       2000000 kB
SwapFree:         500000 kB
`

const cpuinfoX86 = `processor	: 0
vendor_id	: GenuineIntel
model name	: Intel(R) Xeon(R) Platinum 8272CL CPU @ 2.60GHz
`

const cpuinfoArm64 = `processor	: 0
BogoMIPS	: 50.00
CPU implementer	: 0x41
CPU architecture: 8
CPU variant	: 0x3
CPU part	: 0xd0c
CPU revision	: 1
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
		Time:      now,
		CPU:       CPU{User: 100, Nice: 5, System: 50, Idle: 1000, IOWait: 20, IRQ: 3, SoftIRQ: 2, Steal: 1},
		Processes: Processes{Running: 4, Blocked: 1},
		Memory: Memory{
			Total:        16384000 * 1024,
			Free:         8000000 * 1024,
			Available:    12000000 * 1024,
			Buffers:      100000 * 1024,
			Cached:       3000000 * 1024,
			SReclaimable: 200000 * 1024,
			SwapTotal:    2000000 * 1024,
			SwapFree:     500000 * 1024,
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

func TestReadSkipsEnslavedInterfaces(t *testing.T) {
	root := t.TempDir()
	const netdevVF = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:    5000      50    0    0    0     0          0         0     5000      50    0    0    0     0       0          0
  eth0: 1000000    800    0    0    0     0          0         0   200000     600    0    0    0     0       0          0
enP1s1: 1000000    800    0    0    0     0          0         0   200000     600    0    0    0     0       0          0
`
	if err := os.MkdirAll(filepath.Join(root, "proc", "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc", "net", "dev"), []byte(netdevVF), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"sys/class/net/lo", "sys/class/net/eth0/device", "sys/class/net/enP1s1/device"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../eth0", filepath.Join(root, "sys/class/net/enP1s1/master")); err != nil {
		t.Fatal(err)
	}
	r := &Reader{Proc: filepath.Join(root, "proc"), Sys: filepath.Join(root, "sys")}
	got, err := r.netdev()
	if err != nil {
		t.Fatal(err)
	}
	want := []Interface{{Name: "eth0", RxBytes: 1000000, TxBytes: 200000}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("netdev = %+v, want %+v", got, want)
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

func TestReadFailsOnMalformedProcessCounts(t *testing.T) {
	r := fixture(t, true)
	bad := "cpu  100 5 50 1000 20 3 2 1 0 0\nprocs_running x\n"
	if err := os.WriteFile(filepath.Join(r.Proc, "stat"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(time.Now()); err == nil {
		t.Fatal("Read succeeded on a malformed procs_running line")
	}
}

func TestReadWithNoSwap(t *testing.T) {
	r := fixture(t, true)
	noSwap := `MemTotal:       16384000 kB
MemFree:         8000000 kB
MemAvailable:   12000000 kB
Buffers:          100000 kB
Cached:          3000000 kB
SReclaimable:     200000 kB
SwapTotal:             0 kB
SwapFree:              0 kB
`
	if err := os.WriteFile(filepath.Join(r.Proc, "meminfo"), []byte(noSwap), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := r.Read(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got.Memory.SwapTotal != 0 || got.Memory.SwapFree != 0 {
		t.Fatalf("Memory = %+v, want no swap", got.Memory)
	}
}

func TestCPUModelReadsModelName(t *testing.T) {
	r := fixture(t, true)
	if err := os.WriteFile(filepath.Join(r.Proc, "cpuinfo"), []byte(cpuinfoX86), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := r.CPUModel()
	if err != nil {
		t.Fatal(err)
	}
	want := "Intel(R) Xeon(R) Platinum 8272CL CPU @ 2.60GHz"
	if got != want {
		t.Fatalf("CPUModel = %q, want %q", got, want)
	}
}

func TestCPUModelFallsBackToImplementerAndPartOnArm64(t *testing.T) {
	r := fixture(t, true)
	if err := os.WriteFile(filepath.Join(r.Proc, "cpuinfo"), []byte(cpuinfoArm64), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := r.CPUModel()
	if err != nil {
		t.Fatal(err)
	}
	want := "0x41 0xd0c"
	if got != want {
		t.Fatalf("CPUModel = %q, want %q", got, want)
	}
}

func TestCPUModelEmptyWhenNeitherFieldIsPresent(t *testing.T) {
	r := fixture(t, true)
	if err := os.WriteFile(filepath.Join(r.Proc, "cpuinfo"), []byte("processor\t: 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := r.CPUModel()
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("CPUModel = %q, want empty", got)
	}
}
