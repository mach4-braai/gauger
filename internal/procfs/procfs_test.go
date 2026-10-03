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

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

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
		writeFile(t, filepath.Join(root, path), content)
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

func TestReadIncludesPressureWhenPresent(t *testing.T) {
	r := fixture(t, true)
	writeFile(t, filepath.Join(r.Proc, "pressure/cpu"), "some avg10=4.50 avg60=0.91 avg300=0.00 total=681245\n")
	writeFile(t, filepath.Join(r.Proc, "pressure/memory"),
		"some avg10=2.30 avg60=0.50 avg300=0.00 total=100000\nfull avg10=1.20 avg60=0.10 avg300=0.00 total=50000\n")
	writeFile(t, filepath.Join(r.Proc, "pressure/io"),
		"some avg10=10.00 avg60=5.00 avg300=0.00 total=900000\nfull avg10=3.00 avg60=1.00 avg300=0.00 total=300000\n")
	got, err := r.Read(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	memFull, ioFull := uint64(50000), uint64(300000)
	want := []Pressure{
		{Resource: "cpu", Some: 681245},
		{Resource: "memory", Some: 100000, Full: &memFull},
		{Resource: "io", Some: 900000, Full: &ioFull},
	}
	if !reflect.DeepEqual(got.Pressure, want) {
		t.Fatalf("Pressure = %+v, want %+v", got.Pressure, want)
	}
}

func TestReadSkipsPressureFileThatIsMissing(t *testing.T) {
	r := fixture(t, true)
	writeFile(t, filepath.Join(r.Proc, "pressure/cpu"), "some avg10=4.50 avg60=0.91 avg300=0.00 total=681245\n")
	got, err := r.Read(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	want := []Pressure{{Resource: "cpu", Some: 681245}}
	if !reflect.DeepEqual(got.Pressure, want) {
		t.Fatalf("Pressure = %+v, want %+v", got.Pressure, want)
	}
}

func TestReadFailsOnMalformedPressure(t *testing.T) {
	r := fixture(t, true)
	writeFile(t, filepath.Join(r.Proc, "pressure/cpu"), "some avg10=4.50 avg60=0.91 avg300=0.00\n")
	if _, err := r.Read(time.Now()); err == nil {
		t.Fatal("Read succeeded on a malformed /proc/pressure/cpu")
	}
}
