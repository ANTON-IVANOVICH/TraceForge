//go:build e2e

package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"metrics-system/internal/model"
	"metrics-system/internal/testutil"
)

// get performs a GET and returns the status and the body. It exists because
// every assertion below is "what did this endpoint say", and the plumbing is the
// same every time.
func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, string(body)
}

// TestProbesReportTheLifecycle pins the contract the kubelet relies on.
func TestProbesReportTheLifecycle(t *testing.T) {
	t.Parallel()
	p, _, _, telAddr := startServerWithTelemetry(t, "-ui=false")
	defer p.stop()

	if telAddr == "" {
		t.Fatal("the server did not log a telemetry address")
	}
	base := "http://" + telAddr

	// Startup and liveness are unconditional once the process is serving.
	if code, body := get(t, base+"/startupz"); code != http.StatusOK {
		t.Errorf("/startupz = %d %q, want 200 after the server logged that it started", code, body)
	}
	if code, _ := get(t, base+"/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", code)
	}

	// Readiness names the dependency it checked, on success as well as failure.
	code, body := get(t, base+"/readyz")
	if code != http.StatusOK {
		t.Fatalf("/readyz = %d %q, want 200", code, body)
	}
	var status struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		t.Fatalf("/readyz body is not JSON: %v (%q)", err, body)
	}
	if status.Status != "ok" {
		t.Errorf("/readyz status = %q, want ok", status.Status)
	}
	if got := status.Checks["storage"]; got != "ok" {
		t.Errorf("/readyz storage check = %q, want ok (body: %s)", got, body)
	}

	// A cached probe response is a probe that lies. The header is what stops an
	// intermediary from serving a stale 200 to a draining pod.
	resp, err := http.Get(base + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("/readyz Cache-Control = %q, want no-store", cc)
	}
}

// TestReadinessFailsBeforeTheListenerCloses is the reason the whole shutdown
// sequence was reworked.
//
// http.Server.Shutdown closes the listener at once and then waits for in-flight
// requests. What it cannot do is tell the load balancer. Kubernetes dispatches
// SIGTERM and the endpoint removal concurrently, and the removal takes a moment
// to reach every kube-proxy. A server that closes its listener on the first
// instant of SIGTERM spends that moment refusing connections that are still being
// routed to it — one burst of 502s per pod, on every single deploy.
//
// So the order must be: fail readiness, wait, then close. This test asserts the
// window exists: after SIGTERM, /readyz says 503 while the API is still serving
// 200. With -shutdown-delay=0 (the old behaviour) that window is empty and the
// test fails, because by the time /readyz answers 503 the API is already refusing
// connections.
func TestReadinessFailsBeforeTheListenerCloses(t *testing.T) {
	t.Parallel()
	const drain = 3 * time.Second

	p, httpAddr, _, telAddr := startServerWithTelemetry(t,
		"-ui=false", "-shutdown-delay="+drain.String())

	// Prime the store so the query below has an unambiguous 200 to return.
	postBatch(t, httpAddr, model.Batch{AgentID: "drain", Metrics: []model.Metric{
		{Name: "drain_probe", Type: model.MetricTypeGauge, Value: 1, Timestamp: time.Now().UTC()},
	}}, nil)

	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signalling: %v", err)
	}

	// Readiness must go red promptly — well inside the drain window.
	testutil.Eventually(t, drain-time.Second, 20*time.Millisecond, func() bool {
		code, _ := get(t, "http://"+telAddr+"/readyz")
		return code == http.StatusServiceUnavailable
	}, "readiness should fail as soon as SIGTERM arrives, before any listener closes")

	// And the body must still say the checks are fine. That is how an operator
	// tells "this pod is draining" from "this pod is broken" — the distinction the
	// readiness gate exists to express.
	code, body := get(t, "http://"+telAddr+"/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d, want 503 while draining", code)
	}
	var status struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		t.Fatalf("/readyz body is not JSON: %v", err)
	}
	if status.Status != "not ready" {
		t.Errorf("/readyz status = %q, want %q", status.Status, "not ready")
	}
	if got := status.Checks["storage"]; got != "ok" {
		t.Errorf("a draining pod reported its storage as %q; a draining pod is not a broken one", got)
	}

	// The API is still open and still answering. This is the assertion that fails
	// if the delay is removed.
	if _, code := queryMetrics(t, httpAddr, "drain_probe", nil); code != http.StatusOK {
		t.Errorf("the API returned %d while draining; new connections were refused before the "+
			"load balancer could have noticed", code)
	}

	// And liveness stays green: a draining process is not a wedged one, and a
	// failing liveness probe here would make the kubelet restart the pod it is in
	// the middle of terminating.
	if code, _ := get(t, "http://"+telAddr+"/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d while draining, want 200", code)
	}

	if err := p.stopAndWait(); err != nil {
		t.Fatalf("clean shutdown: %v", err)
	}
}

// TestMetricsEndpointAccountsForEveryIngestedMetric scrapes the real endpoint and
// checks the pipeline's accounting identity end to end.
//
// `ingested == stored + invalid + failed` is the promise that nothing vanishes
// between the door and the disk. A unit test can assert it on counters; this
// asserts it on the bytes an operator's Prometheus will actually read.
func TestMetricsEndpointAccountsForEveryIngestedMetric(t *testing.T) {
	t.Parallel()
	p, httpAddr, _, telAddr := startServerWithTelemetry(t, "-ui=false")
	defer p.stop()

	now := time.Now().UTC()
	valid := []model.Metric{
		{Name: "scrape_probe", Type: model.MetricTypeGauge, Value: 1, Timestamp: now},
		{Name: "scrape_probe", Type: model.MetricTypeGauge, Value: 2, Timestamp: now.Add(time.Second)},
		{Name: "scrape_probe", Type: model.MetricTypeGauge, Value: 3, Timestamp: now.Add(2 * time.Second)},
	}
	postBatch(t, httpAddr, model.Batch{AgentID: "scraper", Metrics: valid}, nil)

	// Wait for the pipeline to drain, which is the only state in which the
	// identity is required to hold.
	var fams map[string]float64
	testutil.Eventually(t, 10*time.Second, 100*time.Millisecond, func() bool {
		fams = scrape(t, "http://"+telAddr+"/metrics")
		return fams["traceforge_pipeline_stored_total"] >= float64(len(valid))
	}, "the pipeline should store the metrics we posted")

	ingested := fams["traceforge_pipeline_ingested_total"]
	sum := fams["traceforge_pipeline_stored_total"] +
		fams["traceforge_pipeline_invalid_total"] +
		fams["traceforge_pipeline_failed_total"]
	if ingested != sum {
		t.Errorf("ingested=%v but stored+invalid+failed=%v; %v metrics are unaccounted for",
			ingested, sum, ingested-sum)
	}

	// The RED counter must have seen the ingest request itself, labelled with the
	// mux pattern rather than the raw path.
	if _, ok := fams[`traceforge_http_requests_total{method="POST",route="POST /api/v1/metrics",status="202"}`]; !ok {
		t.Errorf("no RED counter for the ingest request; got:\n%s", strings.Join(sortedKeys(fams), "\n"))
	}

	// The build is exported as a constant-1 gauge with the version in its labels.
	found := false
	for name := range fams {
		if strings.HasPrefix(name, "traceforge_build_info{") {
			found = true
		}
	}
	if !found {
		t.Error("traceforge_build_info is absent; a rollout cannot be observed without it")
	}
}

// TestUnmatchedPathsShareOneSeries pins the cardinality bound. The route label is
// the mux's pattern, so a path nobody registered — and an attacker can invent an
// unbounded number of those — must not mint a series.
func TestUnmatchedPathsShareOneSeries(t *testing.T) {
	t.Parallel()
	p, httpAddr, _, telAddr := startServerWithTelemetry(t, "-ui=false")
	defer p.stop()

	for i := 0; i < 5; i++ {
		resp, err := http.Get(fmt.Sprintf("http://%s/no/such/path/%d", httpAddr, i))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}

	fams := scrape(t, "http://"+telAddr+"/metrics")
	const wantSeries = `traceforge_http_requests_total{method="GET",route="other",status="404"}`
	if got := fams[wantSeries]; got != 5 {
		t.Errorf("%s = %v, want 5", wantSeries, got)
	}
	for name := range fams {
		if strings.Contains(name, "/no/such/path/") {
			t.Errorf("a request path leaked into a metric label: %s", name)
		}
	}
}

// TestHealthCheckFlagProbesItsOwnServer covers the flag that makes HEALTHCHECK
// possible in an image with no shell and no curl.
func TestHealthCheckFlagProbesItsOwnServer(t *testing.T) {
	t.Parallel()
	p, _, _, telAddr := startServerWithTelemetry(t, "-ui=false")
	defer p.stop()

	// Against the running server: exit 0.
	cmd := exec.Command(binaries.server, "-health-check", "-telemetry-addr="+telAddr)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("-health-check against a healthy server failed: %v\n%s", err, out)
	}

	// Against a port nobody is listening on: exit non-zero, with a reason. A
	// health check that cannot fail is decoration.
	cmd = exec.Command(binaries.server, "-health-check", "-telemetry-addr=127.0.0.1:1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Error("-health-check succeeded against a closed port")
	}
	if !strings.Contains(string(out), "health check failed") {
		t.Errorf("-health-check said %q, which does not explain itself", out)
	}
}

// TestVersionFlagWorksWithoutAnything: -version must answer before storage is
// opened or a port is bound, because that is how it is run.
func TestVersionFlagWorksWithoutAnything(t *testing.T) {
	t.Parallel()
	for _, bin := range []struct{ name, path string }{
		{"server", binaries.server},
		{"agent", binaries.agent},
	} {
		out, err := exec.Command(bin.path, "-version").CombinedOutput()
		if err != nil {
			t.Errorf("%s -version: %v\n%s", bin.name, err, out)
			continue
		}
		got := string(out)
		if !strings.Contains(got, "traceforge-"+bin.name) {
			t.Errorf("%s -version = %q, want it to name itself", bin.name, got)
		}
		if !strings.Contains(got, "go1.") {
			t.Errorf("%s -version = %q, want the Go version an operator would ask for", bin.name, got)
		}
	}
}

// TestAgentExportsItsOwnMetrics: an agent whose collectors all fail keeps running
// and keeps looking healthy. Its readiness deliberately ignores the server, so
// its own counters are the only thing that can say otherwise.
func TestAgentExportsItsOwnMetrics(t *testing.T) {
	t.Parallel()
	srv, httpAddr, _, _ := startServerWithTelemetry(t, "-ui=false")
	defer srv.stop()

	a := start(t, "agent", binaries.agent,
		"-server=http://"+httpAddr+"/api/v1/metrics",
		"-interval=200ms",
		"-telemetry-addr=127.0.0.1:0",
	)
	defer a.stop()
	agentTel := waitForAgentTelemetry(t, a)

	// Readiness is 503 until a tick has actually collected something, and 200 after.
	testutil.Eventually(t, 20*time.Second, 100*time.Millisecond, func() bool {
		code, _ := get(t, "http://"+agentTel+"/readyz")
		return code == http.StatusOK
	}, "the agent should become ready once it has collected")

	fams := scrape(t, "http://"+agentTel+"/metrics")
	if fams["traceforge_agent_ticks_total"] < 1 {
		t.Errorf("agent ticks = %v, want at least one", fams["traceforge_agent_ticks_total"])
	}
	if fams["traceforge_agent_batches_sent_total"] < 1 {
		t.Errorf("agent batches sent = %v, want at least one", fams["traceforge_agent_batches_sent_total"])
	}
	if _, ok := fams["traceforge_agent_last_collect_timestamp_seconds"]; !ok {
		t.Error("the agent never published a last-collect timestamp, so no staleness alert could fire")
	}
}

// waitForAgentTelemetry reads the agent's startup log for the port the kernel
// assigned it, the same way waitForAddrs does for the server.
func waitForAgentTelemetry(t *testing.T, p *process) string {
	t.Helper()
	var addr string
	testutil.Eventually(t, 30*time.Second, 20*time.Millisecond, func() bool {
		for _, line := range strings.Split(p.out.String(), "\n") {
			var rec map[string]any
			if json.Unmarshal([]byte(line), &rec) != nil {
				continue
			}
			if rec["msg"] != "telemetry listening" {
				continue
			}
			addr, _ = rec["telemetry_addr"].(string)
			return addr != ""
		}
		return false
	}, "the agent should log its telemetry address; output so far:\n%s", p.out.String())
	return addr
}

// scrape fetches an exposition endpoint and flattens it into series -> value.
// Sample lines keep their label set in the key, so a test can assert on one
// series rather than on a whole family.
func scrape(t *testing.T, url string) map[string]float64 {
	t.Helper()
	code, body := get(t, url)
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d", url, code)
	}

	out := make(map[string]float64)
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// The value is everything after the last space; a label value may contain
		// spaces, so splitting on the first one would be wrong.
		i := strings.LastIndexByte(line, ' ')
		if i < 0 {
			t.Fatalf("unparseable exposition line: %q", line)
		}
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err != nil {
			t.Fatalf("unparseable value in %q: %v", line, err)
		}
		out[line[:i]] = v
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanning %s: %v", url, err)
	}
	return out
}

// sortedKeys is for failure messages only.
func sortedKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
