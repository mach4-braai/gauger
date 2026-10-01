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
