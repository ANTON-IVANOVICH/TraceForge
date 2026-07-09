//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"metrics-system/internal/testutil"
)

// The CLI's exit codes are a contract that shell scripts depend on. A unit test
// can check what RunE returns; only the real binary can check what the shell
// sees.
const (
	exitOK       = 0
	exitError    = 1
	exitUsage    = 2
	exitAuth     = 3
	exitNotFound = 4
)

func TestCtlQueriesALiveServer(t *testing.T) {
	_, httpAddr, _ := startServer(t)

	batch := testutil.NewBatch().
		WithAgentID("e2e-ctl-agent").
		WithMetrics(testutil.NewMetric().WithName("ctl_metric").WithValue(3.5).Build()).
		Build()
	if code := postBatch(t, httpAddr, batch, nil); code != http.StatusAccepted {
		t.Fatalf("ingest: want 202, got %d", code)
	}

	server := "--server=http://" + httpAddr
	testutil.Eventually(t, 15*time.Second, 200*time.Millisecond, func() bool {
		out, _, code := runCtl(t, server, "query", "ctl_metric", "-o", "json")
		return code == exitOK && strings.Contains(out, "ctl_metric")
	}, "metricsctl query should return the ingested metric")

	out, _, code := runCtl(t, server, "query", "ctl_metric", "-o", "json")
	if code != exitOK {
		t.Fatalf("query exit code: want 0, got %d", code)
	}
	var metrics []map[string]any
	if err := json.Unmarshal([]byte(out), &metrics); err != nil {
		t.Fatalf("-o json must emit the raw API object, got %q: %v", out, err)
	}
	if len(metrics) == 0 || metrics[0]["name"] != "ctl_metric" {
		t.Errorf("unexpected query output: %s", out)
	}

	// A pipe is not a terminal: no colour, no cursor control, nothing a shell
	// script has to strip.
	table, _, _ := runCtl(t, server, "query", "ctl_metric")
	if strings.Contains(table, "\x1b[") {
		t.Errorf("table output into a pipe contains ANSI escapes: %q", table)
	}
}

func TestCtlExitCodesAgainstALiveServer(t *testing.T) {
	keys := writeFile(t, "api-keys.json", e2eAPIKeys)
	_, httpAddr, _ := startServer(t, "-auth", "-api-keys="+keys, "-alerting")
	server := "--server=http://" + httpAddr

	tests := []struct {
		name string
		args []string
		want int
	}{
		{"no credentials against an authenticated server", []string{server, "query", "anything"}, exitAuth},
		{"a mistyped subcommand is a usage error, not a help screen", []string{server, "rules", "deletee", "x"}, exitUsage},
		{"an unknown top-level command", []string{server, "nonsense"}, exitUsage},
		{"two credential flags at once", []string{server, "--api-key=a", "--token=b", "stats"}, exitUsage},
		{"an unknown context", []string{"--context=nope", "stats"}, exitNotFound},
		{"an unreachable server", []string{"--server=http://127.0.0.1:1", "stats"}, exitError},
		{"version needs no server at all", []string{"version"}, exitOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, stderr, code := runCtl(t, tt.args...)
			if code != tt.want {
				t.Errorf("exit code: want %d, got %d\nstderr: %s", tt.want, code, stderr)
			}
		})
	}
}

const ctlRuleManifest = `apiVersion: v1
kind: Rule
metadata:
  name: ctl-cpu-high
spec:
  expression: cpu_usage_percent > 90
  for: 1m
  interval: 15s
  severity: warning
  receivers: [log]
  annotations:
    summary: "CPU is high"
`

// `rules apply` is the command CI runs on every push. It has to be idempotent,
// or the second push rewrites rules that did not change and resets their `for`
// state, silencing an alert that was about to fire.
func TestCtlRulesApplyIsIdempotent(t *testing.T) {
	keys := writeFile(t, "api-keys.json", e2eAPIKeys)
	_, httpAddr, _ := startServer(t, "-auth", "-api-keys="+keys, "-alerting")

	server := "--server=http://" + httpAddr
	admin := "--api-key=admin-key"
	manifest := writeFile(t, "rule.yaml", ctlRuleManifest)

	// A writer may ingest but not manage rules. That is an authorization failure,
	// and `apply` must not flatten it into a generic error — a CI job that treats
	// exit 1 as "retry" would loop forever on a permission problem.
	if _, _, code := runCtl(t, server, "--api-key=writer-key", "rules", "apply", "-f", manifest); code != exitAuth {
		t.Fatalf("apply as a writer: want exit %d, got %d", exitAuth, code)
	}

	stdout, stderr, code := runCtl(t, server, admin, "rules", "apply", "-f", manifest)
	if code != exitOK {
		t.Fatalf("first apply: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "created") {
		t.Errorf("first apply should report `created`, got: %s", stdout)
	}

	stdout, _, code = runCtl(t, server, admin, "rules", "apply", "-f", manifest)
	if code != exitOK {
		t.Fatalf("second apply: exit %d", code)
	}
	if !strings.Contains(stdout, "unchanged") {
		t.Errorf("re-applying an unchanged manifest should report `unchanged`, got: %s", stdout)
	}

	// --dry-run must not write, on the update path as much as on create.
	changed := writeFile(t, "rule2.yaml", strings.Replace(ctlRuleManifest, "severity: warning", "severity: critical", 1))
	if _, _, code := runCtl(t, server, admin, "rules", "apply", "-f", changed, "--dry-run"); code != exitOK {
		t.Fatalf("dry-run apply: exit %d", code)
	}
	stdout, _, _ = runCtl(t, server, admin, "rules", "get", "ctl-cpu-high", "-o", "json")
	if strings.Contains(stdout, "critical") {
		t.Error("--dry-run wrote the change it promised not to write")
	}

	// -o name is the format that feeds xargs.
	stdout, _, code = runCtl(t, server, admin, "rules", "list", "-o", "name")
	if code != exitOK || strings.TrimSpace(stdout) != "ctl-cpu-high" {
		t.Errorf("rules list -o name: exit %d, output %q", code, stdout)
	}

	// A rule that does not exist is a 404, and 404 is exit 4.
	if _, _, code := runCtl(t, server, admin, "rules", "get", "no-such-rule"); code != exitNotFound {
		t.Errorf("get a missing rule: want exit %d, got %d", exitNotFound, code)
	}
}

// A destructive command without a terminal and without --yes must refuse rather
// than hang on a prompt nobody can answer.
func TestCtlRefusesToDeleteWithoutATerminalOrYes(t *testing.T) {
	_, httpAddr, _ := startServer(t, "-alerting")
	server := "--server=http://" + httpAddr

	_, stderr, code := runCtl(t, server, "rules", "delete", "whatever")
	if code != exitUsage {
		t.Errorf("delete without a TTY and without --yes: want exit %d, got %d (stderr: %s)", exitUsage, code, stderr)
	}
}

func TestCtlWritesItsConfigWithOwnerOnlyPermissions(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")

	cmdEnv := []string{"--config=" + cfg}
	args := append(cmdEnv, "config", "set-context", "prod",
		"--server=https://metrics.example.com", "--api-key=super-secret", "--use")
	if _, stderr, code := runCtl(t, args...); code != exitOK {
		t.Fatalf("set-context: exit %d, stderr %s", code, stderr)
	}

	info, err := os.Stat(cfg)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("a config holding an API key must be 0600, got %o", perm)
	}

	// The secret must not be printed back unless asked for. The table format
	// prints a summary, so ask for the format that prints the whole config.
	stdout, _, _ := runCtl(t, "--config="+cfg, "config", "view", "-o", "yaml")
	if strings.Contains(stdout, "super-secret") {
		t.Error("config view leaked the API key without --show-secrets")
	}
	if !strings.Contains(stdout, "REDACTED") {
		t.Errorf("config view should redact credentials, got: %s", stdout)
	}
}
