// Package agent runs the sampling loop and the sender that drains the spool.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/mach4-braai/gauger/internal/metrics"
	"github.com/mach4-braai/gauger/internal/procfs"
	"github.com/mach4-braai/gauger/internal/spool"
	"github.com/mach4-braai/gauger/internal/upload"
)

// Sampler reads one sample.
type Sampler interface {
	Read(now time.Time) (procfs.Sample, error)
}

// Uploader sends to gauger-server.
type Uploader interface {
	Start(ctx context.Context, body upload.Lifecycle) error
	Done(ctx context.Context, body upload.Lifecycle) error
	Metrics(ctx context.Context, batch []byte) error
}

// Connect blocks until gauger-server is reachable, which for tsnet means the
// node has joined, and returns the uploader to use.
type Connect func(ctx context.Context) (Uploader, error)

// FinalAttempts is how many drain attempts the post step gets before the
// rest goes to the fallback artifact, so a dead server costs about 3 s, not
// the whole final budget.
const FinalAttempts = 3

// Config sets the loop's timing.
type Config struct {
	SampleEvery time.Duration
	FlushEvery  time.Duration
	// FinalBudget bounds the time from the stop signal to the end of Run.
	FinalBudget time.Duration
	Attrs       []metrics.Attribute
}

// Result is what happened, for the status file that post.js reads.
type Result struct {
	Joined          bool     `json:"joined"`
	StartSent       bool     `json:"start_sent"`
	DoneSent        bool     `json:"done_sent"`
	SentBatches     int      `json:"sent_batches"`
	UnsentBatches   int      `json:"unsent_batches"`
	DroppedBatches  int      `json:"dropped_batches"`
	RejectedBatches int      `json:"rejected_batches"`
	SampleErrors    int      `json:"sample_errors"`
	Warnings        []string `json:"warnings"`
}

// Agent samples on a timer, spools a batch on every flush and sends in the
// background, so a slow or missing network never delays sampling.
type Agent struct {
	Config  Config
	Sampler Sampler
	Batcher *metrics.Batcher
	Spool   *spool.Spool
	Connect Connect
	Log     *log.Logger

	sampleErrors  int
	lastSampleErr error
}

type sendResult struct {
	joined, startSent, doneSent bool
	sent, rejected              int
	joinErr, uploadErr, doneErr error
}

// Run samples until ctx is cancelled, then flushes, drains and sends done
// within Config.FinalBudget.
func (a *Agent) Run(ctx context.Context) Result {
	started := time.Now()
	kick := make(chan struct{}, 1)
	final := make(chan struct{})
	sendCtx, cancelSend := context.WithCancel(context.Background())
	defer cancelSend()
	drainCtx, cancelDrain := context.WithCancel(sendCtx)
	defer cancelDrain()
	results := make(chan sendResult, 1)
	go func() { results <- a.send(sendCtx, drainCtx, started, kick, final) }()

	sampleT := time.NewTicker(a.Config.SampleEvery)
	defer sampleT.Stop()
	flushT := time.NewTicker(a.Config.FlushEvery)
	defer flushT.Stop()

	a.sample(started)
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case now := <-sampleT.C:
			a.sample(now)
		case <-flushT.C:
			a.flush()
			select {
			case kick <- struct{}{}:
			default:
			}
		}
	}

	a.sample(time.Now())
	a.flush()
	close(final)
	// Keep a quarter of the budget for done.
	stopDrain := time.AfterFunc(a.Config.FinalBudget*3/4, cancelDrain)
	defer stopDrain.Stop()
	budget := time.NewTimer(a.Config.FinalBudget)
	defer budget.Stop()
	var r sendResult
	select {
	case r = <-results:
	case <-budget.C:
		cancelSend()
		r = <-results
	}
	return a.result(r)
}

func (a *Agent) sample(now time.Time) {
	s, err := a.Sampler.Read(now)
	if err != nil {
		a.sampleErrors++
		a.lastSampleErr = err
		a.Log.Printf("sample: %v", err)
		return
	}
	a.Batcher.Add(s)
}

func (a *Agent) flush() {
	data, err := a.Batcher.Flush()
	if err != nil {
		a.Log.Printf("encode batch: %v", err)
		return
	}
	if data == nil {
		return
	}
	if err := a.Spool.Put(data); err != nil {
		a.Log.Printf("spool batch: %v", err)
	}
}

// send connects, drains the spool on every kick, and after final drains once
// more and sends done. drainCtx ends before ctx so that done still has time.
func (a *Agent) send(ctx, drainCtx context.Context, started time.Time, kick, final <-chan struct{}) (r sendResult) {
	up, err := a.Connect(drainCtx)
	if err != nil {
		r.joinErr = err
		return r
	}
	r.joined = true
	a.Log.Printf("connected to gauger-server")

	finalPhase, finalTries := false, 0
	for {
		if !r.startSent {
			switch err := up.Start(drainCtx, upload.NewLifecycle(a.Config.Attrs, started)); {
			case err == nil:
				r.startSent = true
			case permanent(err):
				r.startSent = true
				r.uploadErr = err
				a.Log.Printf("start rejected: %v", err)
			default:
				r.uploadErr = err
				a.Log.Printf("start: %v", err)
			}
		}
		drained := a.drain(drainCtx, up, &r)
		if finalPhase {
			finalTries++
		}

		if finalPhase && (drained || finalTries >= FinalAttempts || drainCtx.Err() != nil) {
			unsent, dropped := a.Spool.Len(), a.Spool.Dropped()
			done := upload.NewLifecycle(a.Config.Attrs, time.Now())
			done.UnsentBatches, done.DroppedBatches = &unsent, &dropped
			if err := up.Done(ctx, done); err != nil {
				r.doneErr = err
				a.Log.Printf("done: %v", err)
			} else {
				r.doneSent = true
			}
			return r
		}
		if finalPhase {
			wait(drainCtx, time.Second)
			continue
		}

		select {
		case <-ctx.Done():
			return r
		case <-kick:
		case <-final:
			finalPhase = true
		}
	}
}

// drain sends batches oldest first and reports whether the spool is empty.
// It stops at the first failure that a retry could fix.
func (a *Agent) drain(ctx context.Context, up Uploader, r *sendResult) bool {
	for {
		b, ok, err := a.Spool.Oldest()
		if err != nil {
			r.uploadErr = err
			return false
		}
		if !ok {
			return true
		}
		err = up.Metrics(ctx, b.Data)
		switch {
		case err == nil:
			r.sent++
		case permanent(err):
			r.rejected++
			r.uploadErr = err
			a.Log.Printf("batch %d rejected: %v", b.Seq, err)
		default:
			r.uploadErr = err
			a.Log.Printf("batch %d: %v", b.Seq, err)
			return false
		}
		if err := a.Spool.Remove(b.Seq); err != nil {
			r.uploadErr = err
			return false
		}
	}
}

func (a *Agent) result(r sendResult) Result {
	res := Result{
		Joined:          r.joined,
		StartSent:       r.startSent,
		DoneSent:        r.doneSent,
		SentBatches:     r.sent,
		UnsentBatches:   a.Spool.Len(),
		DroppedBatches:  a.Spool.Dropped(),
		RejectedBatches: r.rejected,
		SampleErrors:    a.sampleErrors,
		Warnings:        []string{},
	}
	warn := func(format string, args ...any) {
		res.Warnings = append(res.Warnings, fmt.Sprintf(format, args...))
	}
	if !r.joined {
		cause := r.joinErr
		if cause == nil || errors.Is(cause, context.Canceled) {
			cause = errors.New("the job ended before the node joined")
		}
		warn("gauger could not reach gauger-server: %v", cause)
	}
	if res.UnsentBatches > 0 {
		if r.uploadErr != nil {
			warn("gauger could not upload %d batches (last error: %v); they go to the fallback artifact", res.UnsentBatches, r.uploadErr)
		} else {
			warn("gauger could not upload %d batches; they go to the fallback artifact", res.UnsentBatches)
		}
	}
	if res.DroppedBatches > 0 {
		warn("gauger dropped the %d oldest batches to keep its buffer under the size limit", res.DroppedBatches)
	}
	if res.RejectedBatches > 0 {
		warn("gauger-server rejected %d batches (last error: %v)", res.RejectedBatches, r.uploadErr)
	}
	if r.joined && !r.doneSent && r.doneErr != nil {
		warn("gauger could not send the done event: %v", r.doneErr)
	}
	if a.sampleErrors > 0 {
		warn("gauger failed %d samples (last error: %v)", a.sampleErrors, a.lastSampleErr)
	}
	return res
}

func permanent(err error) bool {
	var se *upload.StatusError
	return errors.As(err, &se) && se.Permanent()
}

func wait(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
