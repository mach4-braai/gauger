package metrics

import (
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/encoding/protowire"
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

	start            uint64
	prevCPU          *procfs.CPU
	diskBase         map[string]procfs.Disk
	ifaceBase        map[string]procfs.Interface
	pressureBase     map[string]uint64
	containerCPUBase map[string]uint64
	pending          []procfs.Sample

	lastCPU     *procfs.CPU
	lastDisks   map[string]procfs.Disk
	lastIfaces  map[string]procfs.Interface
	peakCPU     float64
	peakMemUsed uint64
	diskRead    uint64
	diskWrite   uint64
	netRx       uint64
	netTx       uint64
}

type Peaks struct {
	CPUUtilization float64 `json:"cpu_utilization"`
	MemoryUsed     uint64  `json:"memory_used_bytes"`
	DiskRead       uint64  `json:"disk_read_bytes"`
	DiskWrite      uint64  `json:"disk_write_bytes"`
	NetworkRx      uint64  `json:"network_rx_bytes"`
	NetworkTx      uint64  `json:"network_tx_bytes"`
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
		resource:         &resourcepb.Resource{Attributes: kvs},
		scope:            &commonpb.InstrumentationScope{Name: "github.com/mach4-braai/gauger", Version: version},
		nproc:            int64(nproc),
		diskBase:         map[string]procfs.Disk{},
		ifaceBase:        map[string]procfs.Interface{},
		pressureBase:     map[string]uint64{},
		containerCPUBase: map[string]uint64{},
		lastDisks:        map[string]procfs.Disk{},
		lastIfaces:       map[string]procfs.Interface{},
	}
}

// Add queues a sample for the next flush.
func (b *Batcher) Add(s procfs.Sample) {
	if b.start == 0 {
		b.start = nanos(s.Time)
	}
	b.trackPeaks(s)
	b.pending = append(b.pending, s)
}

func (b *Batcher) trackPeaks(s procfs.Sample) {
	if prev := b.lastCPU; prev != nil && s.CPU.Total() > prev.Total() {
		total := s.CPU.Total() - prev.Total()
		waiting := sub(s.CPU.Idle, prev.Idle) + sub(s.CPU.IOWait, prev.IOWait)
		if util := float64(sub(total, waiting)) / float64(total); util > b.peakCPU {
			b.peakCPU = util
		}
	}
	cpu := s.CPU
	b.lastCPU = &cpu

	if used := sub(s.Memory.Total, s.Memory.Available); used > b.peakMemUsed {
		b.peakMemUsed = used
	}

	for _, d := range s.Disks {
		if prev, ok := b.lastDisks[d.Name]; ok {
			b.diskRead += sub(d.ReadBytes, prev.ReadBytes)
			b.diskWrite += sub(d.WriteBytes, prev.WriteBytes)
		}
		b.lastDisks[d.Name] = d
	}
	for _, n := range s.Interfaces {
		if prev, ok := b.lastIfaces[n.Name]; ok {
			b.netRx += sub(n.RxBytes, prev.RxBytes)
			b.netTx += sub(n.TxBytes, prev.TxBytes)
		}
		b.lastIfaces[n.Name] = n
	}
}

func (b *Batcher) Peaks() Peaks {
	return Peaks{
		CPUUtilization: b.peakCPU,
		MemoryUsed:     b.peakMemUsed,
		DiskRead:       b.diskRead,
		DiskWrite:      b.diskWrite,
		NetworkRx:      b.netRx,
		NetworkTx:      b.netTx,
	}
}

// Len is the number of samples waiting for a flush.
func (b *Batcher) Len() int { return len(b.pending) }

// Flush encodes the queued samples and clears the queue. It returns nil when
// nothing is queued.
func (b *Batcher) Flush() ([]byte, error) {
	if len(b.pending) == 0 {
		return nil, nil
	}
	rm := b.request()
	b.pending = b.pending[:0]
	body, err := proto.Marshal(rm)
	if err != nil {
		return nil, err
	}
	buf := protowire.AppendTag(nil, 1, protowire.BytesType)
	buf = protowire.AppendBytes(buf, body)
	return buf, nil
}

func (b *Batcher) request() *metricspb.ResourceMetrics {
	var (
		cpu          []*metricspb.NumberDataPoint
		processes    []*metricspb.NumberDataPoint
		memUsage     []*metricspb.NumberDataPoint
		memAvail     []*metricspb.NumberDataPoint
		paging       []*metricspb.NumberDataPoint
		diskIO       []*metricspb.NumberDataPoint
		diskOps      []*metricspb.NumberDataPoint
		netIO        []*metricspb.NumberDataPoint
		pressure     []*metricspb.NumberDataPoint
		fsUsage      []*metricspb.NumberDataPoint
		containerCPU []*metricspb.NumberDataPoint
		containerMem []*metricspb.NumberDataPoint
		lastTime     uint64
		lastLimit    int64
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

		processes = append(processes, b.processPoints(t, s.Processes)...)

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

		if m.SwapTotal > 0 {
			swapUsed := sub(m.SwapTotal, m.SwapFree)
			paging = append(paging,
				b.intPoint(t, int64(swapUsed), stringKV("system.paging.state", "used")),
				b.intPoint(t, int64(m.SwapFree), stringKV("system.paging.state", "free")))
		}

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
		for _, p := range s.Pressure {
			resource := stringKV("system.pressure.resource", p.Resource)
			someBase := b.basePressure(p.Resource+":some", p.Some)
			pressure = append(pressure, b.intPoint(t, int64(p.Some-someBase), resource, stringKV("system.pressure.type", "some")))
			if p.Full != nil {
				fullBase := b.basePressure(p.Resource+":full", *p.Full)
				pressure = append(pressure, b.intPoint(t, int64(*p.Full-fullBase), resource, stringKV("system.pressure.type", "full")))
			}
		}
		for _, fs := range s.Filesystems {
			mount := stringKV("system.filesystem.mountpoint", fs.Mountpoint)
			fsUsage = append(fsUsage,
				b.intPoint(t, int64(fs.UsedBytes), mount, stringKV("system.filesystem.state", "used")),
				b.intPoint(t, int64(fs.FreeBytes), mount, stringKV("system.filesystem.state", "free")))
		}
		for _, cnt := range s.Containers {
			base := b.baseContainerCPU(cnt)
			attrs := []*commonpb.KeyValue{stringKV("container.id", cnt.ID)}
			if cnt.ImageName != "" {
				attrs = append(attrs, stringKV("container.image.name", cnt.ImageName))
			}
			containerCPU = append(containerCPU, b.floatPoint(t, float64(cnt.CPUUsec-base)/1e6, attrs...))
			containerMem = append(containerMem, b.intPoint(t, int64(cnt.MemoryBytes), attrs...))
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
	if len(processes) > 0 {
		ms = append(ms, &metricspb.Metric{
			Name: "system.process.count",
			Unit: "{process}",
			Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: processes}},
		})
	}
	if len(diskIO) > 0 {
		ms = append(ms, counter("system.disk.io", "By", diskIO), counter("system.disk.operations", "{operation}", diskOps))
	}
	if len(netIO) > 0 {
		ms = append(ms, counter("system.network.io", "By", netIO))
	}
	if len(pressure) > 0 {
		ms = append(ms, counter("system.linux.pressure.stall.time", "us", pressure))
	}
	if len(fsUsage) > 0 {
		ms = append(ms, upDown("system.filesystem.usage", "By", fsUsage...))
	}
	if len(paging) > 0 {
		ms = append(ms, upDown("system.paging.usage", "By", paging...))
	}
	if len(containerCPU) > 0 {
		ms = append(ms, counter("container.cpu.time", "s", containerCPU), upDown("container.memory.usage", "By", containerMem...))
	}

	return &metricspb.ResourceMetrics{
		Resource:     b.resource,
		ScopeMetrics: []*metricspb.ScopeMetrics{{Scope: b.scope, Metrics: ms}},
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

func (b *Batcher) processPoints(t uint64, p procfs.Processes) []*metricspb.NumberDataPoint {
	return []*metricspb.NumberDataPoint{
		{TimeUnixNano: t, Value: &metricspb.NumberDataPoint_AsInt{AsInt: int64(p.Running)}, Attributes: []*commonpb.KeyValue{stringKV("process.state", "running")}},
		{TimeUnixNano: t, Value: &metricspb.NumberDataPoint_AsInt{AsInt: int64(p.Blocked)}, Attributes: []*commonpb.KeyValue{stringKV("process.state", "blocked")}},
	}
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

func (b *Batcher) basePressure(key string, v uint64) uint64 {
	base, ok := b.pressureBase[key]
	if !ok || v < base {
		base = v
		b.pressureBase[key] = v
	}
	return base
}

func (b *Batcher) baseContainerCPU(c procfs.Container) uint64 {
	base, ok := b.containerCPUBase[c.ID]
	if !ok || c.CPUUsec < base {
		base = c.CPUUsec
		b.containerCPUBase[c.ID] = base
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

func (b *Batcher) floatPoint(t uint64, v float64, attrs ...*commonpb.KeyValue) *metricspb.NumberDataPoint {
	return &metricspb.NumberDataPoint{
		StartTimeUnixNano: b.start,
		TimeUnixNano:      t,
		Value:             &metricspb.NumberDataPoint_AsDouble{AsDouble: v},
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
