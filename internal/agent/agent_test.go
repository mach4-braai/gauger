package agent

import (
	"context"
	"errors"
	"io"
	"log"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mach4-braai/gauger/internal/metrics"
	"github.com/mach4-braai/gauger/internal/procfs"
	"github.com/mach4-braai/gauger/internal/spool"
	"github.com/mach4-braai/gauger/internal/upload"
)

type fakeSampler struct{ n uint64 }

func (f *fakeSampler) Read(now time.Time) (procfs.Sample, error) {
	f.n++
	return procfs.Sample{Time: now, CPU: procfs.CPU{User: f.n, Idle: 10 * f.n}, Memory: procfs.Memory{Total: 100}}, nil
}

type fakeServer struct {
	mu        sync.Mutex
	calls     []string
	done      *upload.Lifecycle
	metricErr error
}

func (f *fakeServer) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeServer) Start(context.Context, upload.Lifecycle) error {
	f.record("start")
	return nil
}

func (f *fakeServer) Done(_ context.Context, body upload.Lifecycle) error {
	f.record("done")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.done = &body
	return nil
}

func (f *fakeServer) Metrics(context.Context, []byte) error {
	f.mu.Lock()
	err := f.metricErr
	f.mu.Unlock()
	if err != nil {
		return err
	}
	f.record("metrics")
	return nil
}

func newAgent(t *testing.T, connect Connect) *Agent {
	t.Helper()
	sp, err := spool.Open(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	attrs := []metrics.Attribute{{Key: metrics.KeyRunID, Value: "1"}}
	return &Agent{
		Config:  Config{SampleEvery: 5 * time.Millisecond, FlushEvery: 20 * time.Millisecond, FinalBudget: 400 * time.Millisecond, Attrs: attrs},
		Sampler: &fakeSampler{},
		Batcher: metrics.NewBatcher(attrs, 1, "test"),
		Spool:   sp,
		Connect: connect,
		Log:     log.New(io.Discard, "", 0),
	}
}

func runFor(a *Agent, d time.Duration) (Result, time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	start := time.Now()
	r := a.Run(ctx)
	return r, time.Since(start) - d
}

func TestHealthyRunSendsStartMetricsDone(t *testing.T) {
	srv := &fakeServer{}
	a := newAgent(t, func(context.Context) (Uploader, error) { return srv, nil })
	r, _ := runFor(a, 100*time.Millisecond)

	if !r.Joined || !r.StartSent || !r.DoneSent || r.SentBatches == 0 || r.UnsentBatches != 0 || len(r.Warnings) != 0 {
		t.Fatalf("result = %+v", r)
	}
	if srv.calls[0] != "start" || srv.calls[len(srv.calls)-1] != "done" {
		t.Fatalf("calls = %v, want start first and done last", srv.calls)
	}
	if *srv.done.UnsentBatches != 0 {
		t.Fatalf("done reported %d unsent batches", *srv.done.UnsentBatches)
	}
}

func TestBatchesWaitForALateJoin(t *testing.T) {
	srv := &fakeServer{}
	joined := make(chan struct{})
	a := newAgent(t, func(ctx context.Context) (Uploader, error) {
		<-joined
		return srv, nil
	})
	go func() {
		time.Sleep(150 * time.Millisecond)
		close(joined)
	}()
	r, _ := runFor(a, 100*time.Millisecond)

	if !r.DoneSent || r.UnsentBatches != 0 || r.SentBatches < 4 {
		t.Fatalf("result = %+v, want every batch from before the join sent", r)
	}
}

func TestUnreachableServerLeavesBatchesForTheArtifact(t *testing.T) {
	srv := &fakeServer{metricErr: errors.New("connection refused")}
	a := newAgent(t, func(context.Context) (Uploader, error) { return srv, nil })
	a.Config.FinalBudget = 20 * time.Second
	r, over := runFor(a, 100*time.Millisecond)

	if r.UnsentBatches == 0 || r.UnsentBatches != a.Spool.Len() {
		t.Fatalf("result = %+v, want the batches left in the spool", r)
	}
	if *srv.done.UnsentBatches != r.UnsentBatches {
		t.Fatalf("done reported %d unsent, spool has %d", *srv.done.UnsentBatches, r.UnsentBatches)
	}
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "fallback artifact") {
		t.Fatalf("warnings = %q", r.Warnings)
	}
	if over > time.Duration(FinalAttempts)*time.Second {
		t.Fatalf("stopping took %s past the signal; a dead server should cost about %d attempts, not the %s budget", over, FinalAttempts, a.Config.FinalBudget)
	}
}

func TestNeverJoiningKeepsEveryBatch(t *testing.T) {
	a := newAgent(t, func(ctx context.Context) (Uploader, error) {
		<-ctx.Done()
		return nil, errors.New("join the tailnet: no route to control")
	})
	r, over := runFor(a, 60*time.Millisecond)

	if r.Joined || r.UnsentBatches == 0 {
		t.Fatalf("result = %+v", r)
	}
	if !strings.Contains(strings.Join(r.Warnings, "\n"), "no route to control") {
		t.Fatalf("warnings = %q, want the join error", r.Warnings)
	}
	if over > a.Config.FinalBudget+100*time.Millisecond {
		t.Fatalf("stopping took %s past the signal, budget is %s", over, a.Config.FinalBudget)
	}
}

func TestRejectedBatchesAreDropped(t *testing.T) {
	srv := &fakeServer{metricErr: &upload.StatusError{Path: "/v1/metrics", Code: 400, Body: "bad batch"}}
	a := newAgent(t, func(context.Context) (Uploader, error) { return srv, nil })
	r, _ := runFor(a, 60*time.Millisecond)

	if r.RejectedBatches == 0 || r.UnsentBatches != 0 {
		t.Fatalf("result = %+v, want rejected batches removed from the spool", r)
	}
	if !strings.Contains(strings.Join(r.Warnings, "\n"), "rejected") {
		t.Fatalf("warnings = %q", r.Warnings)
	}
}

func TestUnauthorizedIsRetried(t *testing.T) {
	err := &upload.StatusError{Code: 401}
	if err.Permanent() {
		t.Fatal("401 must be retried, a later token can fix it")
	}
	if !(&upload.StatusError{Code: 422}).Permanent() {
		t.Fatal("422 must not be retried")
	}
}

type scriptedSampler struct {
	samples []procfs.Sample
	n       int
}

func (s *scriptedSampler) Read(now time.Time) (procfs.Sample, error) {
	sample := s.samples[s.n]
	sample.Time = now
	s.n++
	return sample, nil
}

func disk(name string, read, write uint64) procfs.Disk {
	return procfs.Disk{Name: name, ReadBytes: read, WriteBytes: write}
}

func iface(name string, rx, tx uint64) procfs.Interface {
	return procfs.Interface{Name: name, RxBytes: rx, TxBytes: tx}
}

// TestPeaksCoverFirstSampleAndACounterReset checks that the run's peaks
// start cold at the first sample (no prior point to diff against) and that
// a disk or interface counter going backwards contributes nothing for that
// tick instead of wrapping into a huge total.
func TestPeaksCoverFirstSampleAndACounterReset(t *testing.T) {
	sampler := &scriptedSampler{samples: []procfs.Sample{
		// First sample: establishes the baseline only.
		{
			CPU:        procfs.CPU{User: 100, Idle: 100},
			Memory:     procfs.Memory{Total: 1000, Available: 400},
			Disks:      []procfs.Disk{disk("sda", 1000, 500)},
			Interfaces: []procfs.Interface{iface("eth0", 200, 100)},
		},
		// Normal increase: sets the peaks.
		{
			CPU:        procfs.CPU{User: 150, Idle: 130},
			Memory:     procfs.Memory{Total: 1000, Available: 300},
			Disks:      []procfs.Disk{disk("sda", 1500, 600)},
			Interfaces: []procfs.Interface{iface("eth0", 500, 150)},
		},
		// Counter reset: every disk and interface counter goes backwards.
		{
			CPU:        procfs.CPU{User: 160, Idle: 140},
			Memory:     procfs.Memory{Total: 1000, Available: 650},
			Disks:      []procfs.Disk{disk("sda", 200, 50)},
			Interfaces: []procfs.Interface{iface("eth0", 100, 20)},
		},
		// Normal increase after the reset.
		{
			CPU:        procfs.CPU{User: 170, Idle: 150},
			Memory:     procfs.Memory{Total: 1000, Available: 500},
			Disks:      []procfs.Disk{disk("sda", 400, 80)},
			Interfaces: []procfs.Interface{iface("eth0", 300, 40)},
		},
	}}

	sp, err := spool.Open(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{
		Sampler: sampler,
		Batcher: metrics.NewBatcher(nil, 1, "test"),
		Spool:   sp,
		Log:     log.New(io.Discard, "", 0),
	}
	start := time.Now()
	for i := range sampler.samples {
		a.sample(start.Add(time.Duration(i) * time.Second))
	}
	peaks := a.result(sendResult{}).Peaks

	if got, want := peaks.CPUUtilization, 0.625; math.Abs(got-want) > 1e-9 {
		t.Errorf("peak CPU utilization = %v, want %v (the later, lower-busy samples must not raise it)", got, want)
	}
	if peaks.MemoryUsed != 700 {
		t.Errorf("peak memory used = %d, want 700", peaks.MemoryUsed)
	}
	if peaks.DiskRead != 700 || peaks.DiskWrite != 130 {
		t.Errorf("disk read/write = %d/%d, want 700/130 (reset tick must add 0, not the backward delta)", peaks.DiskRead, peaks.DiskWrite)
	}
	if peaks.NetworkRx != 500 || peaks.NetworkTx != 70 {
		t.Errorf("network rx/tx = %d/%d, want 500/70 (reset tick must add 0, not the backward delta)", peaks.NetworkRx, peaks.NetworkTx)
	}
}
