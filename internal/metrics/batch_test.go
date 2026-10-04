package metrics

import (
	"math"
	"testing"
	"time"

	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/mach4-braai/gauger/internal/procfs"
)

func sample(sec int64, cpu procfs.CPU, readBytes, rx uint64) procfs.Sample {
	return procfs.Sample{
		Time:       time.Unix(sec, 0),
		CPU:        cpu,
		Memory:     procfs.Memory{Total: 1000, Free: 300, Available: 600, Buffers: 50, Cached: 100, SReclaimable: 50},
		Disks:      []procfs.Disk{{Name: "sda", ReadBytes: readBytes, WriteBytes: 10, ReadOps: 1, WriteOps: 1}},
		Interfaces: []procfs.Interface{{Name: "eth0", RxBytes: rx, TxBytes: 5}},
	}
}

func decode(t *testing.T, data []byte) (map[string]string, map[string]*metricspb.Metric) {
	t.Helper()
	var req colmetricspb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(data, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.ResourceMetrics) != 1 {
		t.Fatalf("got %d resource metrics", len(req.ResourceMetrics))
	}
	rm := req.ResourceMetrics[0]
	attrs := map[string]string{}
	for _, kv := range rm.Resource.Attributes {
		attrs[kv.Key] = kv.Value.GetStringValue()
	}
	byName := map[string]*metricspb.Metric{}
	for _, m := range rm.ScopeMetrics[0].Metrics {
		byName[m.Name] = m
	}
	return attrs, byName
}

func point(t *testing.T, m *metricspb.Metric, key, value string, at int64) *metricspb.NumberDataPoint {
	t.Helper()
	var points []*metricspb.NumberDataPoint
	if s := m.GetSum(); s != nil {
		points = s.DataPoints
	} else {
		points = m.GetGauge().DataPoints
	}
	for _, p := range points {
		if p.TimeUnixNano != uint64(time.Unix(at, 0).UnixNano()) {
			continue
		}
		for _, kv := range p.Attributes {
			if kv.Key == key && kv.Value.GetStringValue() == value {
				return p
			}
		}
	}
	t.Fatalf("%s has no point %s=%s at %d", m.Name, key, value, at)
	return nil
}

func TestFlushTagsTheJobAndCountsFromTheFirstSample(t *testing.T) {
	id := Identity{RunID: "11", RunAttempt: "2", Repository: "mach4-braai/gauger", Workflow: "CI", Job: "build", RunnerName: "GitHub Actions 7"}
	b := NewBatcher(id.Attributes(), 4, "v1.2.3")
	b.Add(sample(100, procfs.CPU{User: 100, Idle: 100}, 1000, 500))
	b.Add(sample(101, procfs.CPU{User: 130, Idle: 170}, 4000, 900))

	data, err := b.Flush()
	if err != nil {
		t.Fatal(err)
	}
	attrs, ms := decode(t, data)

	for k, v := range map[string]string{
		KeyRunID: "11", KeyRunAttempt: "2", KeyRepository: "mach4-braai/gauger",
		KeyWorkflow: "CI", KeyJob: "build", KeyRunnerName: "GitHub Actions 7", KeyScope: "runner",
		"service.name": "gauger", "service.version": "v1.2.3",
	} {
		if attrs[k] != v {
			t.Errorf("resource %s = %q, want %q", k, attrs[k], v)
		}
	}
	if _, ok := attrs[KeyCheckRunID]; ok {
		t.Errorf("empty check run ID was sent as %q", attrs[KeyCheckRunID])
	}

	if got := point(t, ms["system.disk.io"], "disk.io.direction", "read", 100).GetAsInt(); got != 0 {
		t.Errorf("disk read at first sample = %d, want 0", got)
	}
	if got := point(t, ms["system.disk.io"], "disk.io.direction", "read", 101).GetAsInt(); got != 3000 {
		t.Errorf("disk read at second sample = %d, want 3000", got)
	}
	if got := point(t, ms["system.network.io"], "network.io.direction", "receive", 101).GetAsInt(); got != 400 {
		t.Errorf("network receive = %d, want 400", got)
	}
	if !ms["system.disk.io"].GetSum().IsMonotonic || ms["system.memory.usage"].GetSum().IsMonotonic {
		t.Error("counters must be monotonic and memory usage must not be")
	}
	if got := cpuAt(t, ms, 101); math.Abs(got-0.3) > 1e-9 {
		t.Errorf("busy utilization = %v, want 0.3", got)
	}
	if got := point(t, ms["system.memory.usage"], "system.memory.state", "used", 101).GetAsInt(); got != 500 {
		t.Errorf("used memory = %d, want 500", got)
	}
	if got := ms["system.memory.limit"].GetSum().DataPoints[0].GetAsInt(); got != 1000 {
		t.Errorf("memory limit = %d, want 1000", got)
	}
	if got := ms["system.cpu.logical.count"].GetSum().DataPoints[0].GetAsInt(); got != 4 {
		t.Errorf("cpu count = %d, want 4", got)
	}
	if _, ok := ms["system.paging.usage"]; ok {
		t.Error("system.paging.usage sent with no swap")
	}
}

func TestLaterFlushesKeepTheBaseline(t *testing.T) {
	b := NewBatcher(nil, 1, "dev")
	b.Add(sample(100, procfs.CPU{Idle: 100}, 1000, 0))
	if _, err := b.Flush(); err != nil {
		t.Fatal(err)
	}
	b.Add(sample(105, procfs.CPU{User: 50, Idle: 150}, 1500, 0))
	data, err := b.Flush()
	if err != nil {
		t.Fatal(err)
	}
	_, ms := decode(t, data)
	p := point(t, ms["system.disk.io"], "disk.io.direction", "read", 105)
	if p.GetAsInt() != 500 || p.StartTimeUnixNano != uint64(time.Unix(100, 0).UnixNano()) {
		t.Errorf("second flush read = %d from %d, want 500 from the first sample", p.GetAsInt(), p.StartTimeUnixNano)
	}
	if got := cpuAt(t, ms, 105); got != 0.5 {
		t.Errorf("utilization across flushes = %v, want 0.5", got)
	}
}

func TestCPUUtilizationIsOneBusySeries(t *testing.T) {
	b := NewBatcher(nil, 2, "dev")
	b.Add(sample(100, procfs.CPU{User: 10, Idle: 10}, 0, 0))
	b.Add(sample(101, procfs.CPU{User: 70, System: 10, Idle: 20, IOWait: 10, Steal: 10}, 0, 0))
	data, err := b.Flush()
	if err != nil {
		t.Fatal(err)
	}
	_, ms := decode(t, data)
	points := ms["system.cpu.utilization"].GetGauge().DataPoints
	if len(points) != 1 || len(points[0].Attributes) != 0 {
		t.Fatalf("got %d points with attributes %v, want one point without attributes", len(points), points[0].Attributes)
	}
	if got := points[0].GetAsDouble(); math.Abs(got-0.8) > 1e-9 {
		t.Errorf("busy = %v, want 0.8: user, system and steal count, idle and iowait do not", got)
	}
}

func cpuAt(t *testing.T, ms map[string]*metricspb.Metric, at int64) float64 {
	t.Helper()
	for _, p := range ms["system.cpu.utilization"].GetGauge().DataPoints {
		if p.TimeUnixNano == uint64(time.Unix(at, 0).UnixNano()) {
			return p.GetAsDouble()
		}
	}
	t.Fatalf("no CPU utilization at %d", at)
	return 0
}

func TestFlushWithNothingQueued(t *testing.T) {
	data, err := NewBatcher(nil, 1, "dev").Flush()
	if data != nil || err != nil {
		t.Fatalf("Flush = %v, %v; want nil, nil", data, err)
	}
}

func TestProcessCountIsAGaugePerState(t *testing.T) {
	b := NewBatcher(nil, 2, "dev")
	s := sample(100, procfs.CPU{User: 10, Idle: 10}, 0, 0)
	s.Processes = procfs.Processes{Running: 3, Blocked: 1}
	b.Add(s)
	data, err := b.Flush()
	if err != nil {
		t.Fatal(err)
	}
	_, ms := decode(t, data)
	m := ms["system.process.count"]
	if m.Unit != "{process}" {
		t.Fatalf("unit = %q, want {process}", m.Unit)
	}
	if got := point(t, m, "process.state", "running", 100).GetAsInt(); got != 3 {
		t.Errorf("running = %d, want 3", got)
	}
	if got := point(t, m, "process.state", "blocked", 100).GetAsInt(); got != 1 {
		t.Errorf("blocked = %d, want 1", got)
	}
}

func TestFlushReportsPagingUsage(t *testing.T) {
	b := NewBatcher(nil, 1, "dev")
	b.Add(procfs.Sample{
		Time:   time.Unix(100, 0),
		Memory: procfs.Memory{Total: 1000, Free: 300, Available: 600, Buffers: 50, Cached: 100, SReclaimable: 50, SwapTotal: 400, SwapFree: 150},
	})
	data, err := b.Flush()
	if err != nil {
		t.Fatal(err)
	}
	_, ms := decode(t, data)
	paging := ms["system.paging.usage"]
	if paging == nil {
		t.Fatal("system.paging.usage not sent with swap in use")
	}
	if paging.GetSum().IsMonotonic {
		t.Error("paging usage must not be monotonic")
	}
	if got := point(t, paging, "system.paging.state", "used", 100).GetAsInt(); got != 250 {
		t.Errorf("used swap = %d, want 250", got)
	}
	if got := point(t, paging, "system.paging.state", "free", 100).GetAsInt(); got != 150 {
		t.Errorf("free swap = %d, want 150", got)
	}
}

func TestFilesystemUsageHasMountpointAndState(t *testing.T) {
	b := NewBatcher(nil, 1, "dev")
	b.Add(procfs.Sample{
		Time:        time.Unix(100, 0),
		Memory:      procfs.Memory{Total: 1000, Free: 300, Available: 600, Buffers: 50, Cached: 100, SReclaimable: 50},
		Filesystems: []procfs.Filesystem{{Mountpoint: "/", UsedBytes: 2000, FreeBytes: 8000}},
	})
	data, err := b.Flush()
	if err != nil {
		t.Fatal(err)
	}
	_, ms := decode(t, data)
	used := point(t, ms["system.filesystem.usage"], "system.filesystem.state", "used", 100)
	if used.GetAsInt() != 2000 {
		t.Errorf("used = %d, want 2000", used.GetAsInt())
	}
	free := point(t, ms["system.filesystem.usage"], "system.filesystem.state", "free", 100)
	if free.GetAsInt() != 8000 {
		t.Errorf("free = %d, want 8000", free.GetAsInt())
	}
	hasMount := false
	for _, kv := range used.Attributes {
		if kv.Key == "system.filesystem.mountpoint" && kv.Value.GetStringValue() == "/" {
			hasMount = true
		}
	}
	if !hasMount {
		t.Error("used point missing mountpoint attribute")
	}
	if ms["system.filesystem.usage"].GetSum().IsMonotonic {
		t.Error("filesystem usage must not be monotonic")
	}
}

func TestNoFilesystemMetricWhenStatfsFailed(t *testing.T) {
	b := NewBatcher(nil, 1, "dev")
	b.Add(sample(100, procfs.CPU{Idle: 100}, 0, 0))
	data, err := b.Flush()
	if err != nil {
		t.Fatal(err)
	}
	_, ms := decode(t, data)
	if _, ok := ms["system.filesystem.usage"]; ok {
		t.Error("got system.filesystem.usage with no filesystems in the sample")
	}
}

func TestFlushIncludesPressureStallTime(t *testing.T) {
	memFull1, memFull2 := uint64(50), uint64(70)
	b := NewBatcher(nil, 1, "dev")
	b.Add(procfs.Sample{
		Time:   time.Unix(100, 0),
		CPU:    procfs.CPU{Idle: 100},
		Memory: procfs.Memory{Total: 1000},
		Pressure: []procfs.Pressure{
			{Resource: "cpu", Some: 500000},
			{Resource: "memory", Some: 300, Full: &memFull1},
		},
	})
	b.Add(procfs.Sample{
		Time:   time.Unix(101, 0),
		CPU:    procfs.CPU{User: 1, Idle: 100},
		Memory: procfs.Memory{Total: 1000},
		Pressure: []procfs.Pressure{
			{Resource: "cpu", Some: 500200},
			{Resource: "memory", Some: 340, Full: &memFull2},
		},
	})
	data, err := b.Flush()
	if err != nil {
		t.Fatal(err)
	}
	_, ms := decode(t, data)
	m := ms["system.linux.pressure.stall.time"]
	if m == nil {
		t.Fatal("no system.linux.pressure.stall.time metric")
	}
	if m.Unit != "us" {
		t.Errorf("unit = %q, want us", m.Unit)
	}
	if !m.GetSum().IsMonotonic {
		t.Error("pressure stall time must be monotonic")
	}
	if got := point(t, m, "system.pressure.resource", "cpu", 100).GetAsInt(); got != 0 {
		t.Errorf("cpu some at first sample = %d, want 0", got)
	}
	if got := point(t, m, "system.pressure.resource", "cpu", 101).GetAsInt(); got != 200 {
		t.Errorf("cpu some at second sample = %d, want 200", got)
	}
	if got := point(t, m, "system.pressure.resource", "memory", 101).GetAsInt(); got != 40 {
		t.Errorf("memory some at second sample = %d, want 40", got)
	}
	cpuPoints := 0
	for _, p := range m.GetSum().DataPoints {
		for _, kv := range p.Attributes {
			if kv.Key == "system.pressure.resource" && kv.Value.GetStringValue() == "cpu" {
				cpuPoints++
			}
		}
	}
	if cpuPoints != 2 {
		t.Errorf("cpu has %d pressure points, want 2 (one some per sample, no full)", cpuPoints)
	}
}

func TestPressureCounterResetsWhenItGoesBackwards(t *testing.T) {
	b := NewBatcher(nil, 1, "dev")
	b.Add(procfs.Sample{
		Time:     time.Unix(100, 0),
		CPU:      procfs.CPU{Idle: 100},
		Memory:   procfs.Memory{Total: 1000},
		Pressure: []procfs.Pressure{{Resource: "io", Some: 900000}},
	})
	b.Add(procfs.Sample{
		Time:     time.Unix(101, 0),
		CPU:      procfs.CPU{User: 1, Idle: 100},
		Memory:   procfs.Memory{Total: 1000},
		Pressure: []procfs.Pressure{{Resource: "io", Some: 100}},
	})
	data, err := b.Flush()
	if err != nil {
		t.Fatal(err)
	}
	_, ms := decode(t, data)
	m := ms["system.linux.pressure.stall.time"]
	if got := point(t, m, "system.pressure.resource", "io", 101).GetAsInt(); got != 0 {
		t.Errorf("io some after a backwards counter = %d, want 0 (base reset)", got)
	}
}
