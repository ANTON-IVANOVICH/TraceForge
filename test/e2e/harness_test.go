//go:build e2e

// Package e2e runs the real binaries as separate processes and checks that the
// system, assembled the way an operator assembles it, actually works.
//
// Everything below the e2e layer tests the code. This layer tests the product:
// the flags parse, the binaries find each other, a metric collected by the agent
// process appears in a query answered by the server process, and SIGTERM leaves
// the data on disk. Those are the failures that survive a green unit suite and
// meet the user on day one.
//
// It is the smallest layer of the pyramid on purpose. Every test here starts
// processes, waits on the network and takes seconds. Run with:
//
//	go test -tags=e2e -timeout=10m ./test/e2e/...
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"metrics-system/internal/model"
	"metrics-system/internal/testutil"
)

// binaries are built once for the whole package, into a directory that outlives
// every test. Building per test would triple the suite's runtime for no gain.
var binaries struct {
	dir     string
	server  string
	agent   string
	ctl     string
	buildOK bool
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "traceforge-e2e-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}
	binaries.dir = dir

	code := 1
	defer func() {
		os.RemoveAll(dir)
		os.Exit(code)
	}()

	for name, pkg := range map[string]string{
		"server":     "./cmd/server",
		"agent":      "./cmd/agent",
		"metricsctl": "./cmd/metricsctl",
	} {
		out := filepath.Join(dir, name)
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Dir = repoRoot()
		if b, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "e2e: build %s: %v\n%s", pkg, err, b)
			return
		}
		switch name {
		case "server":
			binaries.server = out
		case "agent":
			binaries.agent = out
		case "metricsctl":
			binaries.ctl = out
		}
	}
	binaries.buildOK = true

	code = m.Run()
}

func repoRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return filepath.Join(wd, "..", "..") // test/e2e -> repo root
}

// syncBuffer is a bytes.Buffer that a test may read while the child process is
// still writing to it. bytes.Buffer is not safe for that, and the race detector
// says so — loudly, in a test whose actual subject is something else entirely.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// process is a running binary plus its captured output.
type process struct {
	t    *testing.T
	cmd  *exec.Cmd
	out  *syncBuffer
	name string
	done chan struct{}
}

// start launches a binary and registers a cleanup that stops it. The cleanup
// dumps the process's output when the test failed: an e2e failure is otherwise a
// timeout with no explanation, and the explanation is always in the child's log.
func start(t *testing.T, name, bin string, args ...string) *process {
	t.Helper()
	if !binaries.buildOK {
		t.Fatal("binaries were not built")
	}

	out := &syncBuffer{}
	cmd := exec.Command(bin, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}

	p := &process{t: t, cmd: cmd, out: out, name: name, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()

	t.Cleanup(func() {
		p.stop()
		if t.Failed() {
			t.Logf("---- %s output ----\n%s", name, out.String())
		}
	})
	return p
}

// stop asks politely, then insists. A binary that ignores SIGTERM has a bug this
// harness must not paper over, so the kill is reported.
func (p *process) stop() {
	select {
	case <-p.done:
		return // already exited
	default:
	}

	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		p.t.Errorf("%s ignored SIGTERM for 10s; killing", p.name)
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

// stopAndWait sends SIGTERM and returns the process's exit error, so a test can
// assert on a clean shutdown rather than merely on the absence of a hang.
func (p *process) stopAndWait() error {
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
		return fmt.Errorf("%s did not exit within 15s of SIGTERM", p.name)
	}
	if code := p.cmd.ProcessState.ExitCode(); code != 0 {
		return fmt.Errorf("%s exited with code %d:\n%s", p.name, code, p.out.String())
	}
	return nil
}

// crash kills the process without giving it a chance to flush, which is what a
// power loss or an OOM kill looks like.
func (p *process) crash() {
	_ = p.cmd.Process.Kill()
	<-p.done
}

// waitForAddrs reads the server's structured startup log and returns the
// addresses the kernel actually assigned.
//
// Binding ":0" and reading the port back is what lets every test here run its
// own server concurrently, with no port registry and no flaky "address already
// in use" on a busy CI machine.
func waitForAddrs(t *testing.T, p *process) (httpAddr, grpcAddr string) {
	t.Helper()

	testutil.Eventually(t, 30*time.Second, 20*time.Millisecond, func() bool {
		for _, line := range strings.Split(p.out.String(), "\n") {
			var rec map[string]any
			if json.Unmarshal([]byte(line), &rec) != nil {
				continue
			}
			if rec["msg"] != "server started" {
				continue
			}
			httpAddr, _ = rec["http_addr"].(string)
			grpcAddr, _ = rec["grpc_addr"].(string)
			return httpAddr != ""
		}
		return false
	}, "server should log its bound addresses; output so far:\n%s", p.out.String())

	// The log line is written before Serve accepts, so wait for the listener to
	// answer before handing the address to a test.
	testutil.Eventually(t, 30*time.Second, 20*time.Millisecond, func() bool {
		resp, err := http.Get("http://" + httpAddr + "/healthz")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, "server at %s should answer /healthz", httpAddr)

	return httpAddr, grpcAddr
}

// startServer runs the server on kernel-assigned ports with the given extra
// flags and returns it along with its addresses.
func startServer(t *testing.T, extra ...string) (p *process, httpAddr, grpcAddr string) {
	t.Helper()
	args := append([]string{"-addr=127.0.0.1:0", "-grpc-addr=127.0.0.1:0"}, extra...)
	p = start(t, "server", binaries.server, args...)
	httpAddr, grpcAddr = waitForAddrs(t, p)
	return p, httpAddr, grpcAddr
}

// queryMetrics calls the read API. It returns an empty slice rather than an
// error for a 404-shaped answer, because callers poll it inside Eventually.
func queryMetrics(t *testing.T, httpAddr, name string, header http.Header) ([]model.Metric, int) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, "http://"+httpAddr+"/api/v1/query?name="+name, nil)
	if err != nil {
		t.Fatalf("build query request: %v", err)
	}
	for k, vs := range header {
		req.Header[k] = vs
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode
	}
	var out []model.Metric
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode query response %q: %v", body, err)
	}
	return out, resp.StatusCode
}

// postBatch ingests one batch over HTTP and returns the status code.
func postBatch(t *testing.T, httpAddr string, batch model.Batch, header http.Header) int {
	t.Helper()

	body, err := json.Marshal(batch)
	if err != nil {
		t.Fatalf("encode batch: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+httpAddr+"/api/v1/metrics", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build ingest request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range header {
		req.Header[k] = vs
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/metrics: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// runCtl invokes metricsctl and returns stdout, stderr and the exit code. Exit
// codes are part of the CLI's contract, so the harness surfaces them rather than
// failing on a non-zero exit.
func runCtl(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var outBuf, errBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, binaries.ctl, args...)
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	// An empty config dir keeps a developer's real ~/.metricsctl out of the test.
	cmd.Env = append(os.Environ(), "METRICSCTL_CONFIG="+filepath.Join(t.TempDir(), "config.yaml"), "NO_COLOR=1")

	err := cmd.Run()
	code = cmd.ProcessState.ExitCode()
	if err != nil && code < 0 {
		t.Fatalf("metricsctl %v: %v", args, err)
	}
	return outBuf.String(), errBuf.String(), code
}

// writeFile drops a fixture into the test's temp dir and returns its path.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}
