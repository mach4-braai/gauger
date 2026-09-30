//go:build e2e && linux

// Package e2e runs the real gauger binary against a fake gauger-server and a
// fake GitHub OIDC endpoint, dialing directly instead of through the tailnet.
package e2e

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
)

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gauger-e2e")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "gauger")
	build := exec.Command("go", "build", "-o", binary, "../cmd/gauger")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type lifecycle struct {
	Attributes    map[string]string `json:"attributes"`
	Time          time.Time         `json:"time"`
	UnsentBatches *int              `json:"unsent_batches"`
}

type fakeServer struct {
	*httptest.Server
	token string

	mu      sync.Mutex
	starts  []lifecycle
	dones   []lifecycle
	batches []*colmetricspb.ExportMetricsServiceRequest
	got     chan struct{}
}

func newFakeServer(t *testing.T) *fakeServer {
	payload, _ := json.Marshal(map[string]any{"aud": "gauger-server", "exp": time.Now().Add(5 * time.Minute).Unix()})
	f := &fakeServer{token: "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig", got: make(chan struct{}, 100)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /token", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "bearer request-token" || r.URL.Query().Get("audience") != "gauger-server" {
			http.Error(w, "bad token request", http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, `{"value":%q}`, f.token)
	})
	authed := func(h func(w http.ResponseWriter, body []byte)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+f.token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			body, _ := io.ReadAll(r.Body)
			h(w, body)
		}
	}
	lifecycleHandler := func(into *[]lifecycle) http.HandlerFunc {
		return authed(func(w http.ResponseWriter, body []byte) {
			var l lifecycle
			if err := json.Unmarshal(body, &l); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			*into = append(*into, l)
			f.mu.Unlock()
		})
	}
	mux.HandleFunc("POST /v1/jobs/start", lifecycleHandler(&f.starts))
	mux.HandleFunc("POST /v1/jobs/done", lifecycleHandler(&f.dones))
	mux.HandleFunc("POST /v1/metrics", authed(func(w http.ResponseWriter, body []byte) {
		var req colmetricspb.ExportMetricsServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.batches = append(f.batches, &req)
		f.mu.Unlock()
		f.got <- struct{}{}
	}))
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func start(t *testing.T, server, stateDir string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(binary,
		"-state-dir", stateDir,
		"-server", server,
		"-no-tailnet",
		"-check-run-id", "4242",
		"-flush-every", "500ms",
		"-final-budget", "5s",
	)
	cmd.Env = append(os.Environ(),
		"GITHUB_RUN_ID=777",
		"GITHUB_RUN_ATTEMPT=1",
		"GITHUB_REPOSITORY=mach4-braai/gauger",
		"GITHUB_WORKFLOW=CI",
		"GITHUB_JOB=e2e",
		"RUNNER_NAME=GitHub Actions 1",
		"ACTIONS_ID_TOKEN_REQUEST_URL="+server+"/token?api-version=2.0",
		"ACTIONS_ID_TOKEN_REQUEST_TOKEN=request-token",
	)
	log, err := os.Create(filepath.Join(stateDir, "gauger.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func stop(t *testing.T, cmd *exec.Cmd, stateDir string) status {
	t.Helper()
	begin := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("gauger exited with %v", err)
	}
	if took := time.Since(begin); took > 8*time.Second {
		t.Fatalf("gauger took %s to stop", took)
	}
	data, err := os.ReadFile(filepath.Join(stateDir, "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s status
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	if t.Failed() || testing.Verbose() {
		logs, _ := os.ReadFile(filepath.Join(stateDir, "gauger.log"))
		t.Logf("gauger log:\n%s", logs)
	}
	return s
}

type status struct {
	Joined        bool     `json:"joined"`
	DoneSent      bool     `json:"done_sent"`
	SentBatches   int      `json:"sent_batches"`
	UnsentBatches int      `json:"unsent_batches"`
	Warnings      []string `json:"warnings"`
}

func resourceAttrs(req *colmetricspb.ExportMetricsServiceRequest) map[string]string {
	attrs := map[string]string{}
	for _, kv := range req.ResourceMetrics[0].Resource.Attributes {
		attrs[kv.Key] = kv.Value.GetStringValue()
	}
	return attrs
}

func spooled(t *testing.T, stateDir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(stateDir, "spool", "*.pb"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func waitForBatch(t *testing.T, f *fakeServer) {
	t.Helper()
	select {
	case <-f.got:
	case <-time.After(10 * time.Second):
		t.Fatal("no batch arrived within 10 s")
	}
}

func TestStreamsAndFlushesOnSIGTERM(t *testing.T) {
	f := newFakeServer(t)
	dir := t.TempDir()
	cmd := start(t, f.URL, dir)
	waitForBatch(t, f)
	time.Sleep(1500 * time.Millisecond)
	s := stop(t, cmd, dir)

	if !s.Joined || !s.DoneSent || s.UnsentBatches != 0 || len(s.Warnings) != 0 {
		t.Fatalf("status = %+v", s)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.starts) != 1 || len(f.dones) != 1 {
		t.Fatalf("got %d starts and %d dones, want 1 each", len(f.starts), len(f.dones))
	}
	if got := f.dones[0].Attributes["cicd.pipeline.task.run.id"]; got != "4242" {
		t.Fatalf("done check run ID = %q", got)
	}
	if len(f.batches) != s.SentBatches {
		t.Fatalf("server got %d batches, status says %d", len(f.batches), s.SentBatches)
	}
	want := map[string]string{
		"cicd.pipeline.run.id":      "777",
		"github.run_attempt":        "1",
		"cicd.pipeline.task.run.id": "4242",
		"vcs.owner.name":            "mach4-braai",
		"vcs.repository.name":       "gauger",
		"cicd.pipeline.name":        "CI",
		"cicd.pipeline.task.name":   "e2e",
		"cicd.worker.name":          "GitHub Actions 1",
	}
	names := map[string]bool{}
	for _, b := range f.batches {
		attrs := resourceAttrs(b)
		for k, v := range want {
			if attrs[k] != v {
				t.Fatalf("batch resource %s = %q, want %q", k, attrs[k], v)
			}
		}
		for _, m := range b.ResourceMetrics[0].ScopeMetrics[0].Metrics {
			names[m.Name] = true
		}
	}
	for _, name := range []string{"system.cpu.utilization", "system.memory.usage", "system.memory.limit", "system.cpu.logical.count", "system.network.io"} {
		if !names[name] {
			t.Errorf("no %s in any batch; got %v", name, names)
		}
	}
	if left := spooled(t, dir); len(left) != 0 {
		t.Fatalf("%d batches left in the spool", len(left))
	}
}

func TestKilledServerLeavesTheFallbackBatches(t *testing.T) {
	f := newFakeServer(t)
	dir := t.TempDir()
	cmd := start(t, f.URL, dir)
	waitForBatch(t, f)
	f.Close()
	time.Sleep(2 * time.Second)
	s := stop(t, cmd, dir)

	if s.UnsentBatches == 0 || !strings.Contains(strings.Join(s.Warnings, "\n"), "fallback artifact") {
		t.Fatalf("status = %+v", s)
	}
	files := spooled(t, dir)
	if len(files) != s.UnsentBatches {
		t.Fatalf("spool has %d batches, status says %d", len(files), s.UnsentBatches)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var req colmetricspb.ExportMetricsServiceRequest
		if err := proto.Unmarshal(data, &req); err != nil {
			t.Fatalf("%s is not an OTLP request: %v", file, err)
		}
		if got := resourceAttrs(&req)["cicd.pipeline.task.run.id"]; got != "4242" {
			t.Fatalf("%s check run ID = %q", file, got)
		}
	}
}
