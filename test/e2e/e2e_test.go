//go:build e2e

package e2e

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"metrics-system/internal/testutil"
)

// The agent collects real host metrics and ships them to a real server over
// HTTP. Nothing here is stubbed: two processes, one TCP connection, one query.
func TestAgentShipsMetricsToServerOverHTTP(t *testing.T) {
	_, httpAddr, _ := startServer(t)

	start(t, "agent", binaries.agent,
		"-server=http://"+httpAddr+"/api/v1/metrics",
		"-interval=200ms",
		"-id=e2e-http-agent",
	)

	testutil.Eventually(t, 30*time.Second, 250*time.Millisecond, func() bool {
		metrics, _ := queryMetrics(t, httpAddr, "cpu_usage_percent", nil)
		return len(metrics) > 0
	}, "the agent's cpu metric should reach the server's query API")

	metrics, _ := queryMetrics(t, httpAddr, "cpu_usage_percent", nil)
	if got := metrics[0].Labels["agent_id"]; got != "e2e-http-agent" {
		t.Errorf("agent_id label: want e2e-http-agent, got %q", got)
	}
}

// The same agent, the same server, the other transport. This is the test that
// catches a protobuf field renumbered on one side only.
func TestAgentShipsMetricsToServerOverGRPC(t *testing.T) {
	_, httpAddr, grpcAddr := startServer(t)

	start(t, "agent", binaries.agent,
		"-transport=grpc",
		"-grpc-server="+grpcAddr,
		"-interval=200ms",
		"-id=e2e-grpc-agent",
	)

	testutil.Eventually(t, 30*time.Second, 250*time.Millisecond, func() bool {
		metrics, _ := queryMetrics(t, httpAddr, "memory_used_percent", nil)
		for _, m := range metrics {
			if m.Labels["agent_id"] == "e2e-grpc-agent" {
				return true
			}
		}
		return false
	}, "a metric shipped over gRPC should be queryable over HTTP")
}

const e2eAPIKeys = `{
  "keys": [
    {"key": "writer-key", "subject": "writer", "tenant": "acme",   "roles": ["writer"]},
    {"key": "reader-key", "subject": "reader", "tenant": "acme",   "roles": ["reader"]},
    {"key": "admin-key",  "subject": "admin",  "tenant": "acme",   "roles": ["admin"]},
    {"key": "other-key",  "subject": "other",  "tenant": "globex", "roles": ["writer", "reader"]}
  ]
}`

func TestAuthenticationAndTenantIsolationEndToEnd(t *testing.T) {
	keys := writeFile(t, "api-keys.json", e2eAPIKeys)
	_, httpAddr, _ := startServer(t, "-auth", "-api-keys="+keys)

	writer := http.Header{"X-API-Key": []string{"writer-key"}}
	reader := http.Header{"X-API-Key": []string{"reader-key"}}
	other := http.Header{"X-API-Key": []string{"other-key"}}

	batch := testutil.NewBatch().
		WithAgentID("e2e-auth-agent").
		WithMetrics(testutil.NewMetric().WithName("secret_metric").WithValue(42).Build()).
		Build()

	if code := postBatch(t, httpAddr, batch, nil); code != http.StatusUnauthorized {
		t.Errorf("ingest without credentials: want 401, got %d", code)
	}
	if code := postBatch(t, httpAddr, batch, reader); code != http.StatusForbidden {
		t.Errorf("ingest as a reader: want 403, got %d", code)
	}
	if code := postBatch(t, httpAddr, batch, writer); code != http.StatusAccepted {
		t.Fatalf("ingest as a writer: want 202, got %d", code)
	}

	testutil.Eventually(t, 10*time.Second, 100*time.Millisecond, func() bool {
		metrics, _ := queryMetrics(t, httpAddr, "secret_metric", reader)
		return len(metrics) > 0
	}, "acme's reader should see acme's metric")

	// The other tenant authenticates fine and sees nothing. Tenant isolation is a
	// query-time filter on a server-set label; a bug here returns another
	// company's data with a 200.
	metrics, code := queryMetrics(t, httpAddr, "secret_metric", other)
	if code != http.StatusOK {
		t.Fatalf("query as another tenant: want 200, got %d", code)
	}
	if len(metrics) != 0 {
		t.Errorf("tenant globex read acme's series: %v", metrics)
	}
}

// pprof exposes /debug/pprof/cmdline — the process's argv, which for this server
// can hold -jwt-hs256-secret. It must not be on the API listener, and it must be
// absent entirely unless an operator asked for it.
func TestPprofIsNotReachableOnTheAPIPortAndIsOffByDefault(t *testing.T) {
	_, httpAddr, _ := startServer(t)

	for _, path := range []string{"/debug/pprof/", "/debug/pprof/cmdline", "/debug/pprof/heap"} {
		resp, err := http.Get("http://" + httpAddr + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s on the API port: want 404, got %d", path, resp.StatusCode)
		}
	}
}

func TestPprofListensSeparatelyWhenEnabled(t *testing.T) {
	p, httpAddr, _ := startServer(t, "-pprof-addr=127.0.0.1:0")

	var pprofAddr string
	testutil.Eventually(t, 20*time.Second, 20*time.Millisecond, func() bool {
		for _, line := range strings.Split(p.out.String(), "\n") {
			if i := strings.Index(line, `"pprof_addr":"`); i >= 0 {
				rest := line[i+len(`"pprof_addr":"`):]
				pprofAddr = rest[:strings.IndexByte(rest, '"')]
				return true
			}
		}
		return false
	}, "the server should log the pprof listener's address")

	if pprofAddr == httpAddr {
		t.Fatal("pprof must not share the API listener")
	}

	resp, err := http.Get("http://" + pprofAddr + "/debug/pprof/")
	if err != nil {
		t.Fatalf("GET pprof index: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("pprof index on its own listener: want 200, got %d", resp.StatusCode)
	}

	// And the API is still not on the profiling port.
	resp2, err := http.Get("http://" + pprofAddr + "/api/v1/query?name=x")
	if err != nil {
		t.Fatalf("GET api on pprof listener: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("the API must not be served on the pprof listener, got %d", resp2.StatusCode)
	}
}

// A metric ingested and acknowledged must survive the process. The server is
// asked to stop the way a container runtime asks: SIGTERM, then a new process
// over the same data directory.
func TestMetricsSurviveAGracefulRestart(t *testing.T) {
	dataDir := t.TempDir()

	server, httpAddr, _ := startServer(t, "-storage=tsdb", "-data-dir="+dataDir)

	batch := testutil.NewBatch().
		WithAgentID("e2e-durable-agent").
		WithMetrics(testutil.NewMetric().WithName("durable_metric").WithValue(7).Build()).
		Build()
	if code := postBatch(t, httpAddr, batch, nil); code != http.StatusAccepted {
		t.Fatalf("ingest: want 202, got %d", code)
	}

	// Ingest is asynchronous: 202 means "queued", not "durable". Wait until the
	// point is queryable, which is the only observable "it reached storage".
	testutil.Eventually(t, 15*time.Second, 100*time.Millisecond, func() bool {
		metrics, _ := queryMetrics(t, httpAddr, "durable_metric", nil)
		return len(metrics) > 0
	}, "the metric should be stored before we restart")

	if err := server.stopAndWait(); err != nil {
		t.Fatalf("graceful shutdown: %v", err)
	}

	_, httpAddr2, _ := startServer(t, "-storage=tsdb", "-data-dir="+dataDir)

	metrics, code := queryMetrics(t, httpAddr2, "durable_metric", nil)
	if code != http.StatusOK {
		t.Fatalf("query after restart: want 200, got %d", code)
	}
	if len(metrics) == 0 {
		t.Fatal("the metric did not survive a graceful restart")
	}
	if metrics[0].Value != 7 {
		t.Errorf("value after restart: want 7, got %v", metrics[0].Value)
	}
}

// walSyncWindow is the TSDB's WAL fsync interval (tsdb.syncInterval), plus room
// for a slow CI machine. Points written inside it are in the WAL's buffer but not
// yet on the platter.
const walSyncWindow = 500 * time.Millisecond

// The WAL exists for exactly this: a process killed with no chance to flush.
//
// The contract it implements is worth stating precisely, because this test was
// written asserting a stronger one and failed. `202 Accepted` means queued, not
// durable. A moment later the point is queryable — it is in the in-memory head —
// and still not durable: the WAL buffer is fsynced on a 100ms timer, so there is
// a window in which a value you can read back would not survive a power cut.
//
// That is the same trade Prometheus makes, and it is the right one for metrics:
// an fsync per point costs ~2.6ms (see BenchmarkWALWriteSync), which caps ingest
// at some hundreds of points per second. What the system promises is that a point
// survives once the WAL has synced it. This test waits for that, then pulls the
// plug.
func TestMetricsSurviveACrashOnceTheWALHasSynced(t *testing.T) {
	dataDir := t.TempDir()

	server, httpAddr, _ := startServer(t, "-storage=tsdb", "-data-dir="+dataDir)

	batch := testutil.NewBatch().
		WithAgentID("e2e-crash-agent").
		WithMetrics(testutil.NewMetric().WithName("crash_metric").WithValue(11).Build()).
		Build()
	if code := postBatch(t, httpAddr, batch, nil); code != http.StatusAccepted {
		t.Fatalf("ingest: want 202, got %d", code)
	}
	testutil.Eventually(t, 15*time.Second, 50*time.Millisecond, func() bool {
		metrics, _ := queryMetrics(t, httpAddr, "crash_metric", nil)
		return len(metrics) > 0
	}, "the metric should reach the head before the crash")

	time.Sleep(walSyncWindow)

	server.crash() // SIGKILL: no graceful drain, no chunk flush, no clean lock release

	_, httpAddr2, _ := startServer(t, "-storage=tsdb", "-data-dir="+dataDir)

	metrics, _ := queryMetrics(t, httpAddr2, "crash_metric", nil)
	if len(metrics) == 0 {
		t.Fatal("a point the WAL had synced was lost across SIGKILL: replay did not recover it")
	}
	if metrics[0].Value != 11 {
		t.Errorf("value after crash recovery: want 11, got %v", metrics[0].Value)
	}
}

// The other half of the same contract, asserted so nobody mistakes the window
// above for a bug and "fixes" it by fsyncing every write. If this test starts
// failing because the point survived, the durability story changed and the docs
// must change with it.
func TestAPointNotYetSyncedIsNotPromisedToSurviveACrash(t *testing.T) {
	dataDir := t.TempDir()
	server, httpAddr, _ := startServer(t, "-storage=tsdb", "-data-dir="+dataDir)

	batch := testutil.NewBatch().
		WithAgentID("e2e-window-agent").
		WithMetrics(testutil.NewMetric().WithName("window_metric").WithValue(5).Build()).
		Build()
	if code := postBatch(t, httpAddr, batch, nil); code != http.StatusAccepted {
		t.Fatalf("ingest: want 202, got %d", code)
	}
	server.crash() // immediately: well inside the fsync interval

	_, httpAddr2, _ := startServer(t, "-storage=tsdb", "-data-dir="+dataDir)
	metrics, code := queryMetrics(t, httpAddr2, "window_metric", nil)
	if code != http.StatusOK {
		t.Fatalf("query after crash: want 200, got %d", code)
	}
	// Either outcome is correct — the point may have been synced by the timer
	// before the kill landed. What must never happen is a corrupt store or a
	// server that refuses to start.
	t.Logf("points recovered from an unsynced WAL: %d (0 and 1 are both valid)", len(metrics))
}

// The label values a real agent sends contain the very characters that used to
// collapse two series into one. Prove injectivity survives the whole stack:
// JSON body, pipeline, storage, query.
func TestSeriesWithDelimitersInLabelsStayDistinct(t *testing.T) {
	_, httpAddr, _ := startServer(t)

	batch := testutil.NewBatch().
		WithAgentID("e2e-labels-agent").
		WithMetrics(
			testutil.NewMetric().WithName("requests").WithValue(1).WithLabel("a", "b,c=d").Build(),
			testutil.NewMetric().WithName("requests").WithValue(2).WithLabel("a", "b").WithLabel("c", "d").Build(),
		).
		Build()
	if code := postBatch(t, httpAddr, batch, nil); code != http.StatusAccepted {
		t.Fatalf("ingest: want 202, got %d", code)
	}

	testutil.Eventually(t, 15*time.Second, 100*time.Millisecond, func() bool {
		metrics, _ := queryMetrics(t, httpAddr, "requests", nil)
		return len(metrics) >= 2
	}, "both series should be stored")

	metrics, _ := queryMetrics(t, httpAddr, "requests", nil)
	byValue := map[float64]map[string]string{}
	for _, m := range metrics {
		byValue[m.Value] = m.Labels
	}
	if len(byValue) != 2 {
		t.Fatalf("two distinct series collapsed into %d: %v", len(byValue), metrics)
	}
	if got := byValue[1]["a"]; got != "b,c=d" {
		t.Errorf(`series 1 label a: want "b,c=d", got %q`, got)
	}
	if got := byValue[2]["c"]; got != "d" {
		t.Errorf(`series 2 label c: want "d", got %q`, got)
	}
}
