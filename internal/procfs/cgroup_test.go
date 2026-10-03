package procfs

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

// cgroupV2Fixture lays out a cgroup v2 unified hierarchy with no containers
// in it yet, and returns a Reader pointed at the tree.
func cgroupV2Fixture(t *testing.T) *Reader {
	t.Helper()
	root := t.TempDir()
	cgroupRoot := filepath.Join(root, "sys/fs/cgroup")
	if err := os.MkdirAll(cgroupRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cgroupRoot, "cgroup.controllers"), []byte("cpu memory io\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Reader{Proc: filepath.Join(root, "proc"), Sys: filepath.Join(root, "sys")}
}

// writeScope adds a docker-<id>.scope directory with cpu.stat and memory.current.
func writeScope(t *testing.T, r *Reader, id string, usec, bytes uint64) {
	t.Helper()
	dir := filepath.Join(r.Sys, "fs/cgroup/system.slice", "docker-"+id+".scope")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cpuStat := "usage_usec " + strconv.FormatUint(usec, 10) + "\nuser_usec 0\nsystem_usec 0\n"
	if err := os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte(cpuStat), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.current"), []byte(strconv.FormatUint(bytes, 10)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestContainersReadsCgroupV2Tree(t *testing.T) {
	r := cgroupV2Fixture(t)
	writeScope(t, r, "abc123", 1500000, 41943040)
	writeScope(t, r, "def456", 200000, 10485760)

	got := r.containers()
	want := []Container{
		{ID: "abc123", CPUUsec: 1500000, MemoryBytes: 41943040},
		{ID: "def456", CPUUsec: 200000, MemoryBytes: 10485760},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("containers = %+v, want %+v", got, want)
	}
}

func TestContainersSkipsWithoutCgroupV2(t *testing.T) {
	root := t.TempDir()
	r := &Reader{Proc: filepath.Join(root, "proc"), Sys: filepath.Join(root, "sys")}
	if got := r.containers(); got != nil {
		t.Fatalf("containers = %+v, want nil", got)
	}
}

func TestContainersSkipsWithoutDockerSlice(t *testing.T) {
	r := cgroupV2Fixture(t)
	if got := r.containers(); got != nil {
		t.Fatalf("containers = %+v, want nil", got)
	}
}

func TestContainersSkipsAScopeMissingAFile(t *testing.T) {
	r := cgroupV2Fixture(t)
	writeScope(t, r, "abc123", 1500000, 41943040)
	dir := filepath.Join(r.Sys, "fs/cgroup/system.slice", "docker-removed.scope")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	} // cpu.stat and memory.current missing, as if the container exited mid-read

	got := r.containers()
	want := []Container{{ID: "abc123", CPUUsec: 1500000, MemoryBytes: 41943040}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("containers = %+v, want %+v", got, want)
	}
}

func TestContainersIncludesImageNameWhenDockerReachable(t *testing.T) {
	r := cgroupV2Fixture(t)
	writeScope(t, r, "abc123", 1500000, 41943040)
	r.DockerSocket = serveDockerSocket(t, `{"Config":{"Image":"postgres:16"}}`)

	got := r.containers()
	want := []Container{{ID: "abc123", CPUUsec: 1500000, MemoryBytes: 41943040, ImageName: "postgres:16"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("containers = %+v, want %+v", got, want)
	}
}

func TestDockerImageNameWithoutSocket(t *testing.T) {
	socket, err := os.MkdirTemp("", "gauger")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(socket) })
	r := &Reader{DockerSocket: filepath.Join(socket, "missing.sock")}
	if _, ok := r.dockerImageName("abc123"); ok {
		t.Fatal("dockerImageName succeeded without a socket")
	}
}

func TestDockerImageNameOnUnknownContainer(t *testing.T) {
	socket := shortSocketPath(t)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	r := &Reader{DockerSocket: socket}
	if _, ok := r.dockerImageName("abc123"); ok {
		t.Fatal("dockerImageName succeeded on a 404")
	}
}

// serveDockerSocket starts a fake Docker API on a Unix socket that answers
// every request with body, and returns the socket path.
func serveDockerSocket(t *testing.T, body string) string {
	t.Helper()
	socket := shortSocketPath(t)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(body))
	}))
	return socket
}

// shortSocketPath returns a path for a Unix socket outside of t.TempDir(),
// whose nesting under the test name can exceed the platform's sun_path
// length limit.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gauger")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "docker.sock")
}
