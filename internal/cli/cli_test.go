package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"metrics-system/internal/buildinfo"
)

// -update rewrites the golden files. Run `go test ./internal/cli -update` after
// deliberately changing a table's shape; the diff then shows up in the commit.
var update = flag.Bool("update", false, "update golden files")

// fakeServer stands in for a TraceForge server. Handlers are per-path so a test
// only defines the endpoint it cares about.
type fakeServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string // "METHOD /path"
}

func newFakeServer(t *testing.T, routes map[string]http.HandlerFunc) *fakeServer {
	t.Helper()
	fs := &fakeServer{}
	mux := http.NewServeMux()
	for pattern, h := range routes {
		handler := h
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			fs.mu.Lock()
			fs.requests = append(fs.requests, r.Method+" "+r.URL.Path)
			fs.mu.Unlock()
			handler(w, r)
		})
	}
	fs.Server = httptest.NewServer(mux)
	t.Cleanup(fs.Close)
	return fs
}

func (fs *fakeServer) seen() []string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]string(nil), fs.requests...)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// run executes the command tree against a server, capturing both streams. This
// is only possible because no command writes to os.Stdout directly.
func run(t *testing.T, server string, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := "current-context: test\ncontexts:\n  test:\n    server: " + server + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var out, errBuf bytes.Buffer
	opts := &Options{Stdout: &out, Stderr: &errBuf, Stdin: strings.NewReader(stdin)}
	root := NewRootCmd(testBuild("v-test"), opts)
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs(append([]string{"--config", cfgPath, "--no-color"}, args...))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = root.ExecuteContext(ctx)
	return out.String(), errBuf.String(), err
}

// golden compares got against testdata/<name>.golden, rewriting it under -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}
	if got != string(want) {
		t.Fatalf("output does not match %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

// ---------------------------------------------------------------------------

const rulesJSON = `[
  {"id":"cpu-high","tenant_id":"","name":"CPUHigh","expression":"cpu_usage_percent > 90",
   "for":"1m0s","interval":"15s","severity":"critical","enabled":true,
   "created_at":"2026-07-09T10:00:00Z","updated_at":"2026-07-09T10:00:00Z"},
  {"id":"mem-high","tenant_id":"","name":"MemHigh","expression":"memory_used_percent > 95",
   "for":"2m0s","interval":"30s","severity":"warning","enabled":false,
   "created_at":"2026-07-09T10:00:00Z","updated_at":"2026-07-09T10:00:00Z"}
]`

func TestRulesListTable(t *testing.T) {
	fs := newFakeServer(t, map[string]http.HandlerFunc{
		"GET /api/v1/rules": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(rulesJSON))
		},
	})

	out, _, err := run(t, fs.URL, "", "rules", "list")
	if err != nil {
		t.Fatalf("rules list: %v", err)
	}
	golden(t, "rules_list", out)
}

func TestRulesListJSONPassesTheObjectThrough(t *testing.T) {
	fs := newFakeServer(t, map[string]http.HandlerFunc{
		"GET /api/v1/rules": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(rulesJSON))
		},
	})

	out, _, err := run(t, fs.URL, "", "rules", "list", "-o", "json")
	if err != nil {
		t.Fatalf("rules list: %v", err)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if len(got) != 2 || got[0]["id"] != "cpu-high" || got[0]["expression"] != "cpu_usage_percent > 90" {
		t.Fatalf("json output lost fields: %+v", got)
	}
}

func TestRulesListNamePipesToXargs(t *testing.T) {
	fs := newFakeServer(t, map[string]http.HandlerFunc{
		"GET /api/v1/rules": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(rulesJSON))
		},
	})

	out, _, err := run(t, fs.URL, "", "rules", "list", "-o", "name")
	if err != nil {
		t.Fatalf("rules list: %v", err)
	}
	if out != "cpu-high\nmem-high\n" {
		t.Fatalf("name output = %q", out)
	}
}

func TestEmptyListIsSuccess(t *testing.T) {
	fs := newFakeServer(t, map[string]http.HandlerFunc{
		"GET /api/v1/alerts": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`[]`))
		},
	})

	out, _, err := run(t, fs.URL, "", "alerts", "list")
	if err != nil {
		t.Fatalf("an empty list must exit 0: %v", err)
	}
	if !strings.Contains(out, "No resources found.") {
		t.Fatalf("output = %q", out)
	}
}

func TestAlertsListSortsFiringFirst(t *testing.T) {
	fs := newFakeServer(t, map[string]http.HandlerFunc{
		"GET /api/v1/alerts": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`[
              {"rule_id":"r2","fingerprint":"b","state":"pending","value":91,
               "labels":{"alertname":"MemHigh","severity":"warning","agent_id":"web-2"},
               "active_at":"2026-07-09T10:00:00Z","last_eval_at":"2026-07-09T10:00:00Z"},
              {"rule_id":"r1","fingerprint":"a","state":"firing","value":99,
               "labels":{"alertname":"CPUHigh","severity":"critical","agent_id":"web-1"},
               "active_at":"2026-07-09T10:00:00Z","last_eval_at":"2026-07-09T10:00:00Z"}]`))
		},
	})

	out, _, err := run(t, fs.URL, "", "alerts", "list", "-o", "name")
	if err != nil {
		t.Fatalf("alerts list: %v", err)
	}
	if out != "CPUHigh\nMemHigh\n" {
		t.Fatalf("output = %q, want the firing alert first", out)
	}
}

func TestAlertsListStateFilter(t *testing.T) {
	fs := newFakeServer(t, map[string]http.HandlerFunc{
		"GET /api/v1/alerts": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`[
              {"rule_id":"r1","fingerprint":"a","state":"firing","value":99,"labels":{"alertname":"A"},"active_at":"2026-07-09T10:00:00Z"},
              {"rule_id":"r2","fingerprint":"b","state":"pending","value":91,"labels":{"alertname":"B"},"active_at":"2026-07-09T10:00:00Z"}]`))
		},
	})

	out, _, err := run(t, fs.URL, "", "alerts", "list", "--state", "firing", "-o", "name")
	if err != nil {
		t.Fatalf("alerts list: %v", err)
	}
	if out != "A\n" {
		t.Fatalf("output = %q", out)
	}
	if _, _, err := run(t, fs.URL, "", "alerts", "list", "--state", "bogus"); ExitCode(err) != ExitUsage {
		t.Fatalf("a bad --state must be a usage error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Exit codes are the CLI's contract with shell scripts.
// ---------------------------------------------------------------------------

func TestExitCodes(t *testing.T) {
	fs := newFakeServer(t, map[string]http.HandlerFunc{
		"GET /api/v1/rules/missing": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		},
		"GET /api/v1/rules/forbidden": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		},
		"GET /api/v1/rules/broken": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "boom"})
		},
	})

	tests := map[string]struct {
		args []string
		want int
	}{
		"not found":    {[]string{"rules", "get", "missing"}, ExitNotFound},
		"forbidden":    {[]string{"rules", "get", "forbidden"}, ExitAuth},
		"server error": {[]string{"rules", "get", "broken"}, ExitError},
		"bad output":   {[]string{"rules", "list", "-o", "xml"}, ExitUsage},
		"bad args":     {[]string{"rules", "get"}, ExitUsage},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, err := run(t, fs.URL, "", tc.args...)
			if got := ExitCode(err); got != tc.want {
				t.Fatalf("exit code = %d, want %d (err: %v)", got, tc.want, err)
			}
		})
	}
}

func TestUnknownContextIsNotFound(t *testing.T) {
	_, _, err := run(t, "http://127.0.0.1:1", "", "--context", "nope", "stats")
	if got := ExitCode(err); got != ExitNotFound {
		t.Fatalf("exit code = %d, want %d", got, ExitNotFound)
	}
}

// A mistyped subcommand must not print help and exit 0: a script doing
// `metricsctl rules deletee x && echo ok` would report success while nothing
// happened.
func TestMistypedSubcommandIsAUsageError(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t, map[string]http.HandlerFunc{})

	for _, args := range [][]string{
		{"bogus"},                 // unknown top-level command
		{"rules", "deletee", "x"}, // typo of a rules subcommand
		{"alerts", "bogus"},
		{"silences", "bogus"},
		{"agents", "bogus"},
		{"config", "bogus"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, _, err := run(t, fs.URL, "", args...)
			if got := ExitCode(err); got != ExitUsage {
				t.Fatalf("exit code = %d, want %d (err: %v)", got, ExitUsage, err)
			}
		})
	}
}

// A bare noun prints its help and succeeds.
func TestBareNounPrintsHelp(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t, map[string]http.HandlerFunc{})

	out, _, err := run(t, fs.URL, "", "rules")
	if err != nil {
		t.Fatalf("bare noun: %v", err)
	}
	if !strings.Contains(out, "apply") || !strings.Contains(out, "Available Commands") {
		t.Fatalf("output does not look like help:\n%s", out)
	}
}

// A 401 during apply must still exit 3: the loop must not flatten the typed
// error into a generic one, or `apply || reauth` never reauthenticates.
func TestApplyPreservesTypedErrors(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t, map[string]http.HandlerFunc{
		"GET /api/v1/rules/{id}": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		},
	})

	_, _, err := run(t, fs.URL, manifestYAML, "rules", "apply", "-f", "-")
	if got := ExitCode(err); got != ExitAuth {
		t.Fatalf("exit code = %d, want %d (err: %v)", got, ExitAuth, err)
	}
}

// The manifest is the desired state in full: dropping a field must reconcile it
// back to the server's default, not silently leave the drift in place.
func TestApplyReconcilesOmittedFields(t *testing.T) {
	t.Parallel()
	// The server has the rule disabled and with a non-default severity; the
	// manifest omits both, meaning "enabled, warning".
	existing := map[string]string{
		"cpu-high": `{"id":"cpu-high","name":"cpu-high","expression":"cpu_usage_percent > 90",
                     "for":"1m0s","interval":"15s","severity":"critical","enabled":false,"receivers":["log"]}`,
	}
	fs := applyServer(t, existing)

	minimal := "kind: Rule\nmetadata:\n  name: cpu-high\nspec:\n" +
		"  expression: cpu_usage_percent > 90\n  for: 1m\n  interval: 15s\n  receivers: [log]\n"

	out, _, err := run(t, fs.URL, minimal, "rules", "apply", "-f", "-")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.Contains(out, "updated rule/cpu-high") {
		t.Fatalf("output = %q, want the drift to be reconciled", out)
	}
	var puts int
	for _, req := range fs.seen() {
		if strings.HasPrefix(req, "PUT") {
			puts++
		}
	}
	if puts != 1 {
		t.Fatalf("%d PUTs, want exactly 1", puts)
	}
}

// A rule whose `for` lives inside its expression must still be idempotent: the
// server resolves it, the manifest never restates it.
func TestApplyIdempotentWithForInsideTheExpression(t *testing.T) {
	t.Parallel()
	existing := map[string]string{
		"mem": `{"id":"mem","name":"mem","expression":"memory_used_percent > 95 for 2m",
                "for":"2m0s","interval":"15s","severity":"warning","enabled":true}`,
	}
	fs := applyServer(t, existing)

	manifest := "kind: Rule\nmetadata:\n  name: mem\nspec:\n  expression: memory_used_percent > 95 for 2m\n"
	out, _, err := run(t, fs.URL, manifest, "rules", "apply", "-f", "-")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.Contains(out, "unchanged rule/mem") {
		t.Fatalf("output = %q, want unchanged", out)
	}
}

// A bad expression is caught locally, so --dry-run means something on the
// update path too, where the server is never asked.
func TestApplyDryRunValidatesLocally(t *testing.T) {
	t.Parallel()
	existing := map[string]string{
		"cpu-high": `{"id":"cpu-high","name":"cpu-high","expression":"cpu_usage_percent > 90","enabled":true}`,
	}
	fs := applyServer(t, existing)

	bad := "kind: Rule\nmetadata:\n  name: cpu-high\nspec:\n  expression: rate(cpu_usage_percent)\n"
	_, _, err := run(t, fs.URL, bad, "rules", "apply", "-f", "-", "--dry-run")
	if err == nil {
		t.Fatal("a rule with an invalid expression passed --dry-run")
	}
	if !strings.Contains(err.Error(), "range vector") {
		t.Fatalf("error = %v, want the expression error", err)
	}
	for _, req := range fs.seen() {
		if strings.HasPrefix(req, "PUT") || req == "POST /api/v1/rules" {
			t.Fatalf("--dry-run wrote to the server: %s", req)
		}
	}
}

func TestApplyRejectsDuplicateNames(t *testing.T) {
	t.Parallel()
	fs := applyServer(t, nil)

	dup := "kind: Rule\nmetadata:\n  name: a\nspec:\n  expression: cpu > 1\n" +
		"---\nkind: Rule\nmetadata:\n  name: a\nspec:\n  expression: cpu > 2\n"
	_, _, err := run(t, fs.URL, dup, "rules", "apply", "-f", "-")
	if ExitCode(err) != ExitUsage {
		t.Fatalf("duplicate names must be a usage error, got %v", err)
	}
}

// A credential flag replaces the context's credential; merging them would send
// the context's API key to whatever --server now points at.
func TestCredentialFlagReplacesTheContextCredential(t *testing.T) {
	t.Parallel()
	var gotKey, gotAuth string
	fs := newFakeServer(t, map[string]http.HandlerFunc{
		"GET /api/v1/rules": func(w http.ResponseWriter, r *http.Request) {
			gotKey = r.Header.Get("X-API-Key")
			gotAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`[]`))
		},
	})

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := "current-context: test\ncontexts:\n  test:\n    server: " + fs.URL + "\n    auth:\n      api-key: PRODKEY\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var out, errBuf bytes.Buffer
	root := NewRootCmd(testBuild("v-test"), &Options{Stdout: &out, Stderr: &errBuf, Stdin: strings.NewReader("")})
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"--config", cfgPath, "--no-color", "--token", "STAGETOKEN", "rules", "list"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("rules list: %v", err)
	}

	if gotKey != "" {
		t.Fatalf("the context's API key leaked alongside --token: %q", gotKey)
	}
	if gotAuth != "Bearer STAGETOKEN" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
}

func TestCredentialFlagsAreMutuallyExclusive(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t, map[string]http.HandlerFunc{})
	_, _, err := run(t, fs.URL, "", "--api-key", "k", "--token", "t", "rules", "list")
	if ExitCode(err) != ExitUsage {
		t.Fatalf("two credential flags must be a usage error, got %v", err)
	}
}

// time.NewTicker panics on a non-positive interval; a flag must never do that.
func TestWatchRejectsNonPositiveInterval(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t, map[string]http.HandlerFunc{
		"GET /api/v1/alerts": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`[]`)) },
	})
	_, _, err := run(t, fs.URL, "", "alerts", "list", "--watch", "--interval", "0s")
	if ExitCode(err) != ExitUsage {
		t.Fatalf("a zero --interval must be a usage error, got %v", err)
	}
}

func TestQueryRejectsReservedLabel(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t, map[string]http.HandlerFunc{
		"GET /api/v1/query": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`[]`)) },
	})
	for _, label := range []string{"name=x", "agg=sum", "limit=1"} {
		if _, _, err := run(t, fs.URL, "", "query", "cpu", "-l", label); ExitCode(err) != ExitUsage {
			t.Fatalf("label %q must be rejected as reserved, got %v", label, err)
		}
	}
}

func TestTruncateIsRuneSafe(t *testing.T) {
	t.Parallel()
	if got := truncate("héllo wörld", 6); !utf8.ValidString(got) {
		t.Fatalf("truncate produced invalid UTF-8: %q", got)
	}
	if got := truncate("héllo", 3); got != "hé…" {
		t.Fatalf("truncate = %q, want %q", got, "hé…")
	}
	if got := truncate("abc", 10); got != "abc" {
		t.Fatalf("truncate = %q", got)
	}
	if got := truncate("abc", 0); got != "" {
		t.Fatalf("truncate = %q", got)
	}
}

// ---------------------------------------------------------------------------
// apply
// ---------------------------------------------------------------------------

const manifestYAML = `
apiVersion: v1
kind: Rule
metadata:
  name: cpu-high
spec:
  expression: cpu_usage_percent > 90
  for: 1m
  interval: 15s
  severity: critical
  receivers: [log]
---
apiVersion: v1
kind: Rule
metadata:
  name: mem-high
spec:
  expression: memory_used_percent > 95
  for: 2m
  severity: warning
`

// applyServer answers GET with 404 for unknown rules, echoing whatever is created.
func applyServer(t *testing.T, existing map[string]string) *fakeServer {
	t.Helper()
	return newFakeServer(t, map[string]http.HandlerFunc{
		"GET /api/v1/rules/{id}": func(w http.ResponseWriter, r *http.Request) {
			body, ok := existing[r.PathValue("id")]
			if !ok {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
				return
			}
			_, _ = w.Write([]byte(body))
		},
		"POST /api/v1/rules": func(w http.ResponseWriter, r *http.Request) {
			var spec map[string]any
			_ = json.NewDecoder(r.Body).Decode(&spec)
			writeJSON(w, http.StatusCreated, spec)
		},
		"PUT /api/v1/rules/{id}": func(w http.ResponseWriter, r *http.Request) {
			var spec map[string]any
			_ = json.NewDecoder(r.Body).Decode(&spec)
			writeJSON(w, http.StatusOK, spec)
		},
		"POST /api/v1/rules/preview": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"count": 0, "results": []any{}})
		},
	})
}

func TestApplyCreatesFromStdin(t *testing.T) {
	fs := applyServer(t, nil)

	out, _, err := run(t, fs.URL, manifestYAML, "rules", "apply", "-f", "-")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if want := "created rule/cpu-high\ncreated rule/mem-high\n"; out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}

	var posts int
	for _, req := range fs.seen() {
		if req == "POST /api/v1/rules" {
			posts++
		}
	}
	if posts != 2 {
		t.Fatalf("posted %d rules, want 2", posts)
	}
}

// Applying the same file twice must be a no-op: that is what makes `apply` safe
// to run from CI on every push.
func TestApplyIsIdempotent(t *testing.T) {
	existing := map[string]string{
		"cpu-high": `{"id":"cpu-high","name":"cpu-high","expression":"cpu_usage_percent > 90",
                     "for":"1m0s","interval":"15s","severity":"critical","enabled":true,"receivers":["log"]}`,
	}
	fs := applyServer(t, existing)

	out, _, err := run(t, fs.URL, manifestYAML, "rules", "apply", "-f", "-")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.HasPrefix(out, "unchanged rule/cpu-high\n") {
		t.Fatalf("output = %q, want the first rule to be unchanged", out)
	}
	for _, req := range fs.seen() {
		if strings.HasPrefix(req, "PUT") {
			t.Fatal("an unchanged rule must not be written back")
		}
	}
}

func TestApplyUpdatesAChangedRule(t *testing.T) {
	existing := map[string]string{
		"cpu-high": `{"id":"cpu-high","name":"cpu-high","expression":"cpu_usage_percent > 80",
                     "for":"1m0s","interval":"15s","severity":"critical","enabled":true,"receivers":["log"]}`,
	}
	fs := applyServer(t, existing)

	out, _, err := run(t, fs.URL, manifestYAML, "rules", "apply", "-f", "-")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.HasPrefix(out, "updated rule/cpu-high\n") {
		t.Fatalf("output = %q, want an update", out)
	}
}

// --dry-run must not write anything.
func TestApplyDryRun(t *testing.T) {
	fs := applyServer(t, nil)

	out, _, err := run(t, fs.URL, manifestYAML, "rules", "apply", "-f", "-", "--dry-run")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.Contains(out, "created rule/cpu-high (dry run)") {
		t.Fatalf("output = %q", out)
	}
	for _, req := range fs.seen() {
		if req == "POST /api/v1/rules" || strings.HasPrefix(req, "PUT") {
			t.Fatalf("--dry-run wrote to the server: %s", req)
		}
	}
}

func TestApplyRejectsBadManifests(t *testing.T) {
	fs := applyServer(t, nil)

	tests := map[string]string{
		"wrong kind":    "kind: Deployment\nmetadata:\n  name: x\nspec:\n  expression: a > 1\n",
		"no name":       "kind: Rule\nspec:\n  expression: a > 1\n",
		"no expression": "kind: Rule\nmetadata:\n  name: x\n",
		"bad for":       "kind: Rule\nmetadata:\n  name: x\nspec:\n  expression: a > 1\n  for: forever\n",
		"bad yaml":      "kind: Rule\n\tbad indent\n",
	}
	for name, manifest := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, err := run(t, fs.URL, manifest, "rules", "apply", "-f", "-")
			if err == nil {
				t.Fatal("a bad manifest was accepted")
			}
		})
	}
}

func TestApplyRequiresFilename(t *testing.T) {
	fs := applyServer(t, nil)
	if _, _, err := run(t, fs.URL, "", "rules", "apply"); ExitCode(err) != ExitUsage {
		t.Fatalf("a missing -f must be a usage error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// query, agents, silences, config
// ---------------------------------------------------------------------------

func TestQueryBuildsTheRequest(t *testing.T) {
	var got string
	fs := newFakeServer(t, map[string]http.HandlerFunc{
		"GET /api/v1/query": func(w http.ResponseWriter, r *http.Request) {
			got = r.URL.RawQuery
			_, _ = w.Write([]byte(`[]`))
		},
	})

	if _, _, err := run(t, fs.URL, "", "query", "cpu_usage_percent",
		"-l", "agent_id=web-1", "--agg", "avg", "--step", "1m", "--limit", "10"); err != nil {
		t.Fatalf("query: %v", err)
	}
	for _, want := range []string{"name=cpu_usage_percent", "agent_id=web-1", "agg=avg", "step=1m0s", "limit=10"} {
		if !strings.Contains(got, want) {
			t.Fatalf("query string %q is missing %q", got, want)
		}
	}
}

func TestQueryStepRequiresAgg(t *testing.T) {
	fs := newFakeServer(t, map[string]http.HandlerFunc{
		"GET /api/v1/query": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`[]`)) },
	})
	if _, _, err := run(t, fs.URL, "", "query", "cpu", "--step", "1m"); ExitCode(err) != ExitUsage {
		t.Fatalf("--step without --agg must be a usage error, got %v", err)
	}
}

func TestQueryRejectsBadLabel(t *testing.T) {
	fs := newFakeServer(t, map[string]http.HandlerFunc{
		"GET /api/v1/query": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`[]`)) },
	})
	if _, _, err := run(t, fs.URL, "", "query", "cpu", "-l", "nope"); ExitCode(err) != ExitUsage {
		t.Fatalf("a malformed label must be a usage error, got %v", err)
	}
}

func TestAgentsListDerivesFromHeartbeat(t *testing.T) {
	now := time.Now().UTC()
	fs := newFakeServer(t, map[string]http.HandlerFunc{
		"GET /api/v1/query": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("name") != "uptime_seconds" {
				t.Errorf("queried %q, want the heartbeat metric", r.URL.Query().Get("name"))
			}
			writeJSON(w, http.StatusOK, []map[string]any{
				{"name": "uptime_seconds", "type": "gauge", "value": 3600, "timestamp": now.Add(-10 * time.Second),
					"labels": map[string]string{"agent_id": "web-1"}},
				{"name": "uptime_seconds", "type": "gauge", "value": 60, "timestamp": now.Add(-2 * time.Minute),
					"labels": map[string]string{"agent_id": "web-1"}},
				{"name": "uptime_seconds", "type": "gauge", "value": 100, "timestamp": now.Add(-time.Hour),
					"labels": map[string]string{"agent_id": "web-2"}},
			})
		},
	})

	out, _, err := run(t, fs.URL, "", "agents", "list", "-o", "json")
	if err != nil {
		t.Fatalf("agents list: %v", err)
	}
	var got []struct {
		AgentID string  `json:"agent_id"`
		Status  string  `json:"status"`
		Uptime  float64 `json:"uptime_seconds"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if len(got) != 2 {
		t.Fatalf("got %d agents, want 2", len(got))
	}
	if got[0].AgentID != "web-1" || got[0].Status != "healthy" || got[0].Uptime != 3600 {
		t.Fatalf("web-1 = %+v, want the newest heartbeat", got[0])
	}
	if got[1].Status != "stale" {
		t.Fatalf("web-2 = %+v, want stale", got[1])
	}
}

func TestSilencesCreateParsesMatchers(t *testing.T) {
	var body map[string]any
	fs := newFakeServer(t, map[string]http.HandlerFunc{
		"POST /api/v1/silences": func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&body)
			writeJSON(w, http.StatusCreated, map[string]any{"id": "s1", "matchers": body["matchers"],
				"starts_at": time.Now(), "ends_at": time.Now().Add(time.Hour)})
		},
	})

	out, _, err := run(t, fs.URL, "", "silences", "create",
		"-m", "agent_id=web-1", "-m", `env=~prod.*`, "--duration", "2h", "--comment", "maintenance")
	if err != nil {
		t.Fatalf("silences create: %v", err)
	}
	if !strings.Contains(out, "created silence/s1") {
		t.Fatalf("output = %q", out)
	}
	matchers, _ := body["matchers"].([]any)
	if len(matchers) != 2 {
		t.Fatalf("sent %d matchers, want 2: %v", len(matchers), body)
	}
	if body["duration"] != "2h0m0s" {
		t.Fatalf("duration = %v", body["duration"])
	}
}

// A silence with no matchers would mute everything, so the CLI refuses before
// the server has to.
func TestSilencesCreateRequiresMatcher(t *testing.T) {
	fs := newFakeServer(t, map[string]http.HandlerFunc{})
	if _, _, err := run(t, fs.URL, "", "silences", "create", "--duration", "1h"); ExitCode(err) != ExitUsage {
		t.Fatalf("a matcher-less silence must be a usage error, got %v", err)
	}
	if _, _, err := run(t, fs.URL, "", "silences", "create", "-m", "a=b"); ExitCode(err) != ExitUsage {
		t.Fatalf("a silence without a duration must be a usage error, got %v", err)
	}
}

// A destructive command must never proceed unattended: stdin is not a terminal
// in a script, and blocking on a prompt would hang it.
func TestDeleteRefusesWithoutTerminalOrYes(t *testing.T) {
	var deleted int
	fs := newFakeServer(t, map[string]http.HandlerFunc{
		"DELETE /api/v1/rules/{id}": func(w http.ResponseWriter, _ *http.Request) {
			deleted++
			w.WriteHeader(http.StatusNoContent)
		},
	})

	_, _, err := run(t, fs.URL, "", "rules", "delete", "cpu-high")
	if ExitCode(err) != ExitUsage {
		t.Fatalf("delete without a terminal must be a usage error, got %v", err)
	}
	if deleted != 0 {
		t.Fatal("delete proceeded without confirmation")
	}

	out, _, err := run(t, fs.URL, "", "rules", "delete", "cpu-high", "--yes")
	if err != nil {
		t.Fatalf("delete --yes: %v", err)
	}
	if deleted != 1 || !strings.Contains(out, "deleted rule/cpu-high") {
		t.Fatalf("deleted=%d output=%q", deleted, out)
	}
}

func TestConfigContexts(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	exec := func(args ...string) (string, error) {
		var out, errBuf bytes.Buffer
		opts := &Options{Stdout: &out, Stderr: &errBuf, Stdin: strings.NewReader("")}
		root := NewRootCmd(testBuild("v-test"), opts)
		root.SetOut(&out)
		root.SetErr(&errBuf)
		root.SetArgs(append([]string{"--config", cfgPath, "--no-color"}, args...))
		err := root.ExecuteContext(context.Background())
		return out.String(), err
	}

	if _, err := exec("config", "set-context", "prod", "--server", "https://metrics.example", "--api-key", "k"); err != nil {
		t.Fatalf("set-context: %v", err)
	}
	if _, err := exec("config", "set-context", "local", "--server", "http://localhost:8080", "--use"); err != nil {
		t.Fatalf("set-context: %v", err)
	}

	out, err := exec("config", "current-context")
	if err != nil || strings.TrimSpace(out) != "local" {
		t.Fatalf("current-context = %q (%v)", out, err)
	}
	if _, err := exec("config", "use-context", "prod"); err != nil {
		t.Fatalf("use-context: %v", err)
	}
	out, _ = exec("config", "current-context")
	if strings.TrimSpace(out) != "prod" {
		t.Fatalf("current-context = %q", out)
	}

	// The credential must never be printed by default.
	out, err = exec("config", "view", "-o", "yaml")
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if strings.Contains(out, "\"k\"") || strings.Contains(out, "api-key: k") {
		t.Fatalf("config view leaked a credential:\n%s", out)
	}
	if !strings.Contains(out, "REDACTED") {
		t.Fatalf("view did not redact:\n%s", out)
	}

	if _, err := exec("config", "use-context", "absent"); ExitCode(err) != ExitNotFound {
		t.Fatalf("unknown context must exit %d, got %v", ExitNotFound, err)
	}

	out, err = exec("config", "get-contexts")
	if err != nil {
		t.Fatalf("get-contexts: %v", err)
	}
	if !strings.Contains(out, "prod") || !strings.Contains(out, "api-key") || strings.Contains(out, " k ") {
		t.Fatalf("get-contexts output = %q", out)
	}

	// A config that did not exist starts from the built-in "default" context, so
	// three contexts exist here: default, prod and local.
	if _, err := exec("config", "delete-context", "local"); err != nil {
		t.Fatalf("delete-context: %v", err)
	}
	// Deleting the current context must not leave the config dangling.
	if _, err := exec("config", "delete-context", "prod"); err != nil {
		t.Fatalf("delete-context: %v", err)
	}
	out, _ = exec("config", "current-context")
	if strings.TrimSpace(out) != "default" {
		t.Fatalf("after deleting the current context, current-context = %q", out)
	}
	if _, err := exec("config", "delete-context", "default"); ExitCode(err) != ExitUsage {
		t.Fatal("deleting the only remaining context must be refused")
	}
}

// testBuild is the build identity the CLI tests run against. Assembling it here
// rather than calling buildinfo.Get() keeps the tests independent of whether the
// test binary was linked with ldflags or built inside a git tree.
func testBuild(version string) buildinfo.Info {
	return buildinfo.Info{
		Version:   version,
		Commit:    "0123456789ab",
		Date:      "2026-07-10T00:00:00Z",
		GoVersion: "go1.26.2",
		Platform:  "linux/amd64",
	}
}

// version and completion must work with no config and no reachable server.
func TestVersionAndCompletionNeedNoServer(t *testing.T) {
	t.Setenv("METRICSCTL_CONFIG", filepath.Join(t.TempDir(), "absent.yaml"))

	exec := func(args ...string) (string, error) {
		var out bytes.Buffer
		root := NewRootCmd(testBuild("v1.2.3"), &Options{Stdout: &out, Stderr: &out, Stdin: strings.NewReader("")})
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(args)
		err := root.ExecuteContext(context.Background())
		return out.String(), err
	}

	out, err := exec("version")
	if err != nil || !strings.Contains(out, "v1.2.3") {
		t.Fatalf("version = %q (%v)", out, err)
	}

	out, err = exec("version", "-o", "json")
	if err != nil {
		t.Fatalf("version -o json: %v", err)
	}
	var v Version
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.Version != "v1.2.3" {
		t.Fatalf("version json = %q (%v)", out, err)
	}

	out, err = exec("completion", "bash")
	if err != nil {
		t.Fatalf("completion bash: %v", err)
	}
	if !strings.Contains(out, "metricsctl") {
		t.Fatalf("completion script looks wrong:\n%s", out[:min(200, len(out))])
	}
	if _, err := exec("completion", "klingon"); err == nil {
		t.Fatal("an unsupported shell was accepted")
	}
}

func TestParseTime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

	if got, _ := parseTime("", now); !got.IsZero() {
		t.Fatalf("empty = %v, want the zero time", got)
	}
	if got, _ := parseTime("now", now); !got.Equal(now) {
		t.Fatalf("now = %v", got)
	}
	if got, _ := parseTime("-1h", now); !got.Equal(now.Add(-time.Hour)) {
		t.Fatalf("-1h = %v", got)
	}
	if got, _ := parseTime("2026-07-09T10:00:00Z", now); got.UTC().Hour() != 10 {
		t.Fatalf("rfc3339 = %v", got)
	}
	for _, bad := range []string{"-1x", "yesterday", "10:00"} {
		if _, err := parseTime(bad, now); err == nil {
			t.Fatalf("parseTime(%q) unexpectedly succeeded", bad)
		}
	}
}
