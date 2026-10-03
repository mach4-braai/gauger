// Command gauger samples runner metrics for one GitHub Actions job and streams
// them to gauger-server over the tailnet. The action's main.js starts it and
// post.js stops it with SIGTERM.
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
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/mach4-braai/gauger/internal/agent"
	"github.com/mach4-braai/gauger/internal/metrics"
	"github.com/mach4-braai/gauger/internal/oidc"
	"github.com/mach4-braai/gauger/internal/spool"
	"github.com/mach4-braai/gauger/internal/tailnet"
	"github.com/mach4-braai/gauger/internal/upload"
)

var version = "dev"

const requestTimeout = 10 * time.Second

type options struct {
	stateDir      string
	server        string
	checkRunID    string
	clientID      string
	audience      string
	oidcAudience  string
	hostname      string
	noTailnet     bool
	maxSpoolBytes int64
	sampleEvery   time.Duration
	flushEvery    time.Duration
	finalBudget   time.Duration
	verbose       bool
}

// Status is the file post.js reads after gauger exits.
type Status struct {
	agent.Result
	Version   string `json:"version"`
	JoinMS    int64  `json:"join_ms,omitempty"`
	LoggedOut bool   `json:"logged_out"`
}

func main() {
	var o options
	flag.StringVar(&o.stateDir, "state-dir", "", "directory for the spool, tsnet state and status.json (required)")
	flag.StringVar(&o.server, "server", "http://gauger-server:4318", "gauger-server base URL")
	flag.StringVar(&o.checkRunID, "check-run-id", "", "the job's check run ID")
	flag.StringVar(&o.clientID, "ts-client-id", "", "Tailscale federated identity client ID")
	flag.StringVar(&o.audience, "ts-audience", "", "OIDC audience for the Tailscale federated identity")
	flag.StringVar(&o.oidcAudience, "oidc-audience", "gauger-server", "OIDC audience of the bearer token sent to gauger-server")
	flag.StringVar(&o.hostname, "hostname", "", "tailnet hostname (default: derived from the job)")
	flag.BoolVar(&o.noTailnet, "no-tailnet", false, "dial -server directly instead of through the tailnet")
	flag.Int64Var(&o.maxSpoolBytes, "max-spool-bytes", 64<<20, "size limit of the on-disk buffer")
	flag.DurationVar(&o.sampleEvery, "sample-every", time.Second, "sampling interval")
	flag.DurationVar(&o.flushEvery, "flush-every", 5*time.Second, "batch interval")
	flag.DurationVar(&o.finalBudget, "final-budget", 20*time.Second, "time from SIGTERM to the end of the final upload")
	flag.BoolVar(&o.verbose, "verbose", false, "log tsnet's own messages")
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
	sp, err := spool.Open(filepath.Join(o.stateDir, "spool"), o.maxSpoolBytes)
	if err != nil {
		status.Warnings = []string{fmt.Sprintf("gauger could not open its buffer: %v", err)}
		return status
	}
	tokens, tokenErr := oidc.FromEnv(os.Getenv, o.oidcAudience)
	client := func(hc *http.Client, server string) agent.Uploader {
		hc.Timeout = requestTimeout
		return &upload.Client{BaseURL: server, HTTP: hc, Tokens: tokens}
	}

	var node *tailnet.Node
	joinCtx, cancelJoin := context.WithCancel(context.Background())
	defer cancelJoin()
	var connect agent.Connect
	switch {
	case tokenErr != nil:
		connect = func(context.Context) (agent.Uploader, error) { return nil, tokenErr }
	case o.noTailnet:
		connect = func(context.Context) (agent.Uploader, error) { return client(&http.Client{}, o.server), nil }
	default:
		hostname := o.hostname
		if hostname == "" {
			hostname = defaultHostname(identity)
		}
		node = tailnet.Join(joinCtx, tailnet.Config{
			Dir:       filepath.Join(o.stateDir, "tsnet"),
			Hostname:  hostname,
			ClientID:  o.clientID,
			Audience:  o.audience,
			UpTimeout: time.Minute,
			Log:       logger,
			Verbose:   o.verbose,
		})
		go func() {
			<-ctx.Done()
			node.StopRetrying()
		}()
		connect = func(ctx context.Context) (agent.Uploader, error) {
			hc, err := node.Wait(ctx)
			if err != nil {
				return nil, err
			}
			server, err := node.Qualify(ctx, o.server)
			if err != nil {
				logger.Printf("keeping %s as given: %v", o.server, err)
				server = o.server
			}
			logger.Printf("sending to %s", server)
			return client(hc, server), nil
		}
	}

	sampler, err := newSampler()
	if err != nil {
		status.Warnings = []string{fmt.Sprintf("gauger could not start its sampler: %v", err)}
		return status
	}
	a := &agent.Agent{
		Config: agent.Config{
			SampleEvery: o.sampleEvery,
			FlushEvery:  o.flushEvery,
			FinalBudget: o.finalBudget,
			Attrs:       attrs,
		},
		Sampler: sampler,
		Batcher: metrics.NewBatcher(attrs, runtime.NumCPU(), version, runtime.GOOS),
		Spool:   sp,
		Connect: connect,
		Log:     logger,
	}
	logger.Printf("gauger %s sampling for run %s attempt %s", version, identity.RunID, identity.RunAttempt)
	status.Result = a.Run(ctx)
	logger.Printf("stopped: %d batches sent, %d unsent", status.SentBatches, status.UnsentBatches)

	if node != nil {
		status.JoinMS = node.JoinTime().Milliseconds()
		cancelJoin()
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := node.Close(closeCtx); err != nil {
			status.Warnings = append(status.Warnings, fmt.Sprintf("gauger could not log out of the tailnet: %v", err))
		} else {
			status.LoggedOut = status.Joined
		}
	}
	return status
}

var nonHostname = regexp.MustCompile(`[^a-z0-9-]+`)

// defaultHostname is unique per job attempt, so parallel jobs never share a node name.
func defaultHostname(id metrics.Identity) string {
	job := id.CheckRunID
	if job == "" {
		job = id.RunnerName
	}
	name := strings.ToLower(strings.Join([]string{"gauger", id.RunID, id.RunAttempt, job}, "-"))
	name = strings.Trim(nonHostname.ReplaceAllString(name, "-"), "-")
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-")
	}
	return name
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
