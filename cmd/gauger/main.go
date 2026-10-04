// Command gauger samples runner metrics for one GitHub Actions job and streams
// them to gauger-server over HTTPS. The action's main.js starts it and post.js
// stops it with SIGTERM.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/mach4-braai/gauger/internal/agent"
	"github.com/mach4-braai/gauger/internal/metrics"
	"github.com/mach4-braai/gauger/internal/oidc"
	"github.com/mach4-braai/gauger/internal/procfs"
	"github.com/mach4-braai/gauger/internal/spool"
	"github.com/mach4-braai/gauger/internal/upload"
)

var version = "dev"

const requestTimeout = 10 * time.Second

type options struct {
	stateDir      string
	server        string
	checkRunID    string
	oidcAudience  string
	maxSpoolBytes int64
	sampleEvery   time.Duration
	flushEvery    time.Duration
	finalBudget   time.Duration
}

// Status is the file post.js reads after gauger exits.
type Status struct {
	agent.Result
	Version string `json:"version"`
}

func main() {
	var o options
	flag.StringVar(&o.stateDir, "state-dir", "", "directory for the spool and status.json (required)")
	flag.StringVar(&o.server, "server", "https://gauger-server.taila8b8af.ts.net:10000", "gauger-server base URL")
	flag.StringVar(&o.checkRunID, "check-run-id", "", "the job's check run ID")
	flag.StringVar(&o.oidcAudience, "oidc-audience", "gauger-server", "OIDC audience of the bearer token sent to gauger-server")
	flag.Int64Var(&o.maxSpoolBytes, "max-spool-bytes", 64<<20, "size limit of the on-disk buffer")
	flag.DurationVar(&o.sampleEvery, "sample-every", time.Second, "sampling interval")
	flag.DurationVar(&o.flushEvery, "flush-every", 5*time.Second, "batch interval")
	flag.DurationVar(&o.finalBudget, "final-budget", 20*time.Second, "time from SIGTERM to the end of the final upload")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if o.stateDir == "" {
		fmt.Fprintln(os.Stderr, "gauger: -state-dir is required")
		os.Exit(2)
	}
	logger := log.New(os.Stderr, "gauger: ", log.LstdFlags|log.Lmicroseconds|log.LUTC)
	status := run(o, logger)
	if err := writeStatus(filepath.Join(o.stateDir, "status.json"), status); err != nil {
		logger.Printf("write status: %v", err)
	}
}

func run(o options, logger *log.Logger) Status {
	status := Status{Version: version}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	identity := metrics.IdentityFromEnv(os.Getenv, o.checkRunID)
	attrs := identity.Attributes()
	reader := procfs.NewReader(os.Getenv("GITHUB_WORKSPACE"))
	cpuModel, cpuErr := reader.CPUModel()
	if cpuErr != nil {
		logger.Printf("cpu model: %v", cpuErr)
	}
	runnerAttrs := metrics.RunnerAttributesFromEnv(os.Getenv, cpuModel)
	batchAttrs := append(append([]metrics.Attribute{}, attrs...), runnerAttrs.Attributes()...)
	sp, err := spool.Open(filepath.Join(o.stateDir, "spool"), o.maxSpoolBytes)
	if err != nil {
		status.Warnings = []string{fmt.Sprintf("gauger could not open its buffer: %v", err)}
		return status
	}
	tokens, tokenErr := oidc.FromEnv(os.Getenv, o.oidcAudience)
	connect := func(context.Context) (agent.Uploader, error) {
		if tokenErr != nil {
			return nil, tokenErr
		}
		logger.Printf("sending to %s", o.server)
		return &upload.Client{BaseURL: o.server, HTTP: &http.Client{Timeout: requestTimeout}, Tokens: tokens}, nil
	}

	a := &agent.Agent{
		Config: agent.Config{
			SampleEvery: o.sampleEvery,
			FlushEvery:  o.flushEvery,
			FinalBudget: o.finalBudget,
			Attrs:       attrs,
		},
		Sampler: reader,
		Batcher: metrics.NewBatcher(batchAttrs, runtime.NumCPU(), version),
		Spool:   sp,
		Connect: connect,
		Log:     logger,
	}
	logger.Printf("gauger %s sampling for run %s attempt %s", version, identity.RunID, identity.RunAttempt)
	status.Result = a.Run(ctx)
	logger.Printf("stopped: %d batches sent, %d unsent", status.SentBatches, status.UnsentBatches)
	return status
}

func writeStatus(path string, s Status) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
