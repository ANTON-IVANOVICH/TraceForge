//go:build e2e

package e2e

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"metrics-system/internal/testutil"
)

// webhookSink is an HTTP server the test runs and the server binary calls. It is
// the only way to observe a notification: everything after the rule fires — the
// grouping, the retry queue, the circuit breaker, the HMAC — happens inside the
// server process and leaves no trace in its API.
type webhookSink struct {
	*httptest.Server

	mu       sync.Mutex
	requests []webhookRequest
}

type webhookRequest struct {
	body      []byte
	signature string
	timestamp string
}

func newWebhookSink(t *testing.T) *webhookSink {
	t.Helper()
	sink := &webhookSink{}
	sink.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sink.mu.Lock()
		sink.requests = append(sink.requests, webhookRequest{
			body:      body,
			signature: r.Header.Get("X-TraceForge-Signature"),
			timestamp: r.Header.Get("X-TraceForge-Timestamp"),
		})
		sink.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(sink.Close)
	return sink
}

func (s *webhookSink) received() []webhookRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]webhookRequest(nil), s.requests...)
}

const webhookSecret = "e2e-shared-secret"

// A rule that fires on the first breaching sample (no `for`), grouped with no
// wait, so the test does not spend thirty seconds proving grouping works —
// grouping has its own unit tests.
func alertRules() string {
	return `{
  "rules": [
  {
    "id": "cpu-high",
    "name": "CPUHigh",
    "expression": "e2e_cpu > 90",
    "interval": "200ms",
    "severity": "critical",
    "receivers": ["sink"],
    "annotations": {"summary": "cpu is {{ .Value }}"},
    "enabled": true
  }
  ]
}`
}

func alertConfig(sinkURL string) string {
	return fmt.Sprintf(`{
  "group_by": ["alertname"],
  "group_wait": "100ms",
  "group_interval": "200ms",
  "repeat_interval": "1h",
  "default_receivers": ["sink"],
  "receivers": [
    {"name": "sink", "type": "webhook", "url": %q, "secret": %q, "timeout": "5s"}
  ]
}`, sinkURL+"/alerts", webhookSecret)
}

// The whole alerting path, through two processes: ingest a breaching sample,
// wait for the rule to evaluate, and catch the notification on a real socket —
// with its signature, which is the only thing that proves the payload was not
// tampered with in transit.
func TestAlertFiresAndNotifiesASignedWebhook(t *testing.T) {
	sink := newWebhookSink(t)

	rules := writeFile(t, "rules.json", alertRules())
	config := writeFile(t, "receivers.json", alertConfig(sink.URL))

	_, httpAddr, _ := startServer(t,
		"-alerting",
		"-alert-rules="+rules,
		"-alert-config="+config,
		"-alert-lookback=1m",
	)

	// Keep feeding breaching samples: the rule's instant selector only looks back
	// over -alert-lookback, and a single sample would age out mid-test.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			batch := testutil.NewBatch().
				WithAgentID("e2e-alert-agent").
				WithMetrics(testutil.NewMetric().WithName("e2e_cpu").WithValue(99).WithTimestamp(time.Now().UTC()).Build()).
				Build()
			postBatch(t, httpAddr, batch, nil)
			time.Sleep(200 * time.Millisecond)
		}
	}()

	testutil.Eventually(t, 30*time.Second, 200*time.Millisecond, func() bool {
		return len(sink.received()) > 0
	}, "the webhook receiver should be called once the rule fires")

	req := sink.received()[0]

	// The signature covers "<timestamp>.<body>", so replaying an old body under a
	// new timestamp does not verify.
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write([]byte(req.timestamp + "." + string(req.body)))
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(strings.TrimPrefix(req.signature, "sha256="))) {
		t.Errorf("webhook signature does not verify:\n got %q\nwant %q", req.signature, want)
	}

	var payload struct {
		Alerts []struct {
			Status      string            `json:"status"`
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		} `json:"alerts"`
	}
	if err := json.Unmarshal(req.body, &payload); err != nil {
		t.Fatalf("decode webhook payload %s: %v", req.body, err)
	}
	if len(payload.Alerts) == 0 {
		t.Fatalf("webhook payload carried no alerts: %s", req.body)
	}
	if got := payload.Alerts[0].Labels["alertname"]; got != "CPUHigh" {
		t.Errorf("alertname: want CPUHigh, got %q", got)
	}
	if got := payload.Alerts[0].Status; got != "firing" {
		t.Errorf("status: want firing, got %q", got)
	}

	// The alert is also visible through the API, which is what the dashboard and
	// metricsctl read.
	resp, err := http.Get("http://" + httpAddr + "/api/v1/alerts")
	if err != nil {
		t.Fatalf("GET /api/v1/alerts: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "CPUHigh") {
		t.Errorf("/api/v1/alerts does not list the firing alert: %s", body)
	}
}

// An unreachable receiver must not take the server down, stall evaluation, or
// leak a goroutine per attempt. The retry queue and circuit breaker exist for
// this, and only a real process can show that the rest of the server keeps
// serving while they work.
func TestAnUnreachableReceiverDoesNotStallTheServer(t *testing.T) {
	rules := writeFile(t, "rules.json", alertRules())
	// Port 1 is reserved and refuses connections immediately.
	config := writeFile(t, "receivers.json", alertConfig("http://127.0.0.1:1"))

	_, httpAddr, _ := startServer(t,
		"-alerting",
		"-alert-rules="+rules,
		"-alert-config="+config,
		"-alert-lookback=1m",
	)

	batch := testutil.NewBatch().
		WithAgentID("e2e-alert-agent").
		WithMetrics(testutil.NewMetric().WithName("e2e_cpu").WithValue(99).WithTimestamp(time.Now().UTC()).Build()).
		Build()
	if code := postBatch(t, httpAddr, batch, nil); code != http.StatusAccepted {
		t.Fatalf("ingest: want 202, got %d", code)
	}

	testutil.Eventually(t, 30*time.Second, 200*time.Millisecond, func() bool {
		resp, err := http.Get("http://" + httpAddr + "/api/v1/alerts")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return strings.Contains(string(body), "CPUHigh")
	}, "the rule should still evaluate and fire while its receiver is down")

	// The API is still healthy after the delivery failures.
	resp, err := http.Get("http://" + httpAddr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz after delivery failures: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz after delivery failures: want 200, got %d", resp.StatusCode)
	}
}
