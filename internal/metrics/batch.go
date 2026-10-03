package metrics

import (
	"time"

	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/mach4-braai/gauger/internal/procfs"
)

// Batcher collects samples and encodes them as one OTLP export request per flush.
//
// Counters are sent as cumulative sums since the first sample, so each job's
// totals start at zero. CPU is sent as utilization between consecutive samples.
type Batcher struct {
	resource *resourcepb.Resource
	scope    *commonpb.InstrumentationScope
	nproc    int64

	start     uint64
	prevCPU   *procfs.CPU
	diskBase  map[string]procfs.Disk
	ifaceBase map[string]procfs.Interface
	pending   []procfs.Sample
}

// NewBatcher returns a Batcher that tags every export with attrs.
func NewBatcher(attrs []Attribute, nproc int, version string) *Batcher {
	kvs := []*commonpb.KeyValue{
		stringKV("service.name", "gauger"),
		stringKV("service.version", version),
		stringKV("os.type", "linux"),
	}
	for _, a := range attrs {
		kvs = append(kvs, stringKV(a.Key, a.Value))
	}
	return &Batcher{
		resource:  &resourcepb.Resource{Attributes: kvs},
		scope:     &commonpb.InstrumentationScope{Name: "github.com/mach4-braai/gauger", Version: version},
		nproc:     int64(nproc),
		diskBase:  map[string]procfs.Disk{},
		ifaceBase: map[string]procfs.Interface{},
	}
}

// Add queues a sample for the next flush.
func (b *Batcher) Add(s procfs.Sample) {
	if b.start == 0 {
		b.start = nanos(s.Time)
	}
	b.pending = append(b.pending, s)
}

// Len is the number of samples waiting for a flush.
func (b *Batcher) Len() int { return len(b.pending) }

// Flush encodes the queued samples and clears the queue. It returns nil when
// nothing is queued.
func (b *Batcher) Flush() ([]byte, error) {
	if len(b.pending) == 0 {
		return nil, nil
	}
	req := b.request()
	b.pending = b.pending[:0]
	return proto.Marshal(req)
}

func (b *Batcher) request() *colmetricspb.ExportMetricsServiceRequest {
	var (
		cpu       []*metricspb.NumberDataPoint
		memUsage  []*metricspb.NumberDataPoint
		memAvail  []*metricspb.NumberDataPoint
		diskIO    []*metricspb.NumberDataPoint
		diskOps   []*metricspb.NumberDataPoint
		netIO     []*metricspb.NumberDataPoint
		fsUsage   []*metricspb.NumberDataPoint
		lastTime  uint64
		lastLimit int64
	)
	for _, s := range b.pending {
		t := nanos(s.Time)
		lastTime = t
		lastLimit = int64(s.Memory.Total)

		if prev := b.prevCPU; prev != nil {
			cpu = append(cpu, b.cpuPoints(t, *prev, s.CPU)...)
		}
		c := s.CPU
		b.prevCPU = &c

		m := s.Memory
		cached := m.Cached + m.SReclaimable
		used := sub(m.Total, m.Free+m.Buffers+cached)
		for _, st := range []struct {
			state string
			value uint64
		}{{"used", used}, {"free", m.Free}, {"buffers", m.Buffers}, {"cached", cached}} {
			memUsage = append(memUsage, b.intPoint(t, int64(st.value), stringKV("system.memory.state", st.state)))
		}
		memAvail = append(memAvail, b.intPoint(t, int64(m.Available)))

		for _, d := range s.Disks {
			base := b.baseDisk(d)
			device := stringKV("system.device", d.Name)
			diskIO = append(diskIO,
				b.intPoint(t, int64(d.ReadBytes-base.ReadBytes), device, stringKV("disk.io.direction", "read")),
				b.intPoint(t, int64(d.WriteBytes-base.WriteBytes), device, stringKV("disk.io.direction", "write")))
			diskOps = append(diskOps,
				b.intPoint(t, int64(d.ReadOps-base.ReadOps), device, stringKV("disk.io.direction", "read")),
				b.intPoint(t, int64(d.WriteOps-base.WriteOps), device, stringKV("disk.io.direction", "write")))
		}
		for _, n := range s.Interfaces {
			base := b.baseIface(n)
			name := stringKV("network.interface.name", n.Name)
			netIO = append(netIO,
				b.intPoint(t, int64(n.RxBytes-base.RxBytes), name, stringKV("network.io.direction", "receive")),
				b.intPoint(t, int64(n.TxBytes-base.TxBytes), name, stringKV("network.io.direction", "transmit")))
		}
		for _, fs := range s.Filesystems {
			mount := stringKV("system.filesystem.mountpoint", fs.Mountpoint)
			fsUsage = append(fsUsage,
				b.intPoint(t, int64(fs.UsedBytes), mount, stringKV("system.filesystem.state", "used")),
				b.intPoint(t, int64(fs.FreeBytes), mount, stringKV("system.filesystem.state", "free")))
		}
	}

	ms := []*metricspb.Metric{
		upDown("system.cpu.logical.count", "{cpu}", b.intPoint(lastTime, b.nproc)),
		upDown("system.memory.limit", "By", b.intPoint(lastTime, lastLimit)),
		upDown("system.memory.usage", "By", memUsage...),
		upDown("system.linux.memory.available", "By", memAvail...),
	}
	if len(cpu) > 0 {
		ms = append(ms, &metricspb.Metric{
			Name: "system.cpu.utilization",
			Unit: "1",
			Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: cpu}},
		})
	}
	if len(diskIO) > 0 {
		ms = append(ms, counter("system.disk.io", "By", diskIO), counter("system.disk.operations", "{operation}", diskOps))
	}
	if len(netIO) > 0 {
		ms = append(ms, counter("system.network.io", "By", netIO))
	}
	if len(fsUsage) > 0 {
		ms = append(ms, upDown("system.filesystem.usage", "By", fsUsage...))
	}

	return &colmetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			Resource:     b.resource,
			ScopeMetrics: []*metricspb.ScopeMetrics{{Scope: b.scope, Metrics: ms}},
		}},
	}
}

// cpuPoints returns the share of all CPUs that was busy since the previous
// sample: every mode except idle and iowait. gauger-server reads it as one
// series, so it carries no cpu.mode.
func (b *Batcher) cpuPoints(t uint64, prev, cur procfs.CPU) []*metricspb.NumberDataPoint {
	if cur.Total() <= prev.Total() {
		return nil
	}
	total := cur.Total() - prev.Total()
	waiting := sub(cur.Idle, prev.Idle) + sub(cur.IOWait, prev.IOWait)
	return []*metricspb.NumberDataPoint{{
		TimeUnixNano: t,
		Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: float64(sub(total, waiting)) / float64(total)},
	}}
}

// baseDisk returns the counters a disk had when it was first seen. A counter
// that went backwards resets the base.
func (b *Batcher) baseDisk(d procfs.Disk) procfs.Disk {
	base, ok := b.diskBase[d.Name]
	if !ok || d.ReadBytes < base.ReadBytes || d.WriteBytes < base.WriteBytes || d.ReadOps < base.ReadOps || d.WriteOps < base.WriteOps {
		base = d
		b.diskBase[d.Name] = d
	}
	return base
}

func (b *Batcher) baseIface(n procfs.Interface) procfs.Interface {
	base, ok := b.ifaceBase[n.Name]
	if !ok || n.RxBytes < base.RxBytes || n.TxBytes < base.TxBytes {
		base = n
		b.ifaceBase[n.Name] = n
	}
	return base
}

func (b *Batcher) intPoint(t uint64, v int64, attrs ...*commonpb.KeyValue) *metricspb.NumberDataPoint {
	return &metricspb.NumberDataPoint{
		StartTimeUnixNano: b.start,
		TimeUnixNano:      t,
		Value:             &metricspb.NumberDataPoint_AsInt{AsInt: v},
		Attributes:        attrs,
	}
}

func upDown(name, unit string, points ...*metricspb.NumberDataPoint) *metricspb.Metric {
	return &metricspb.Metric{
		Name: name,
		Unit: unit,
		Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
			AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
			DataPoints:             points,
		}},
	}
}

func counter(name, unit string, points []*metricspb.NumberDataPoint) *metricspb.Metric {
	m := upDown(name, unit, points...)
	m.GetSum().IsMonotonic = true
	return m
}

func stringKV(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func nanos(t time.Time) uint64 { return uint64(t.UnixNano()) }

func sub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}
