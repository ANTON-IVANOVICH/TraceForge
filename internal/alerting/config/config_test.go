package config

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

const goodConfig = `{
  "group_by": ["alertname"],
  "group_wait": "10s",
  "repeat_interval": "1h",
  "default_receivers": ["log"],
  "receivers": [
    {"name": "log", "type": "log"},
    {"name": "hook", "type": "webhook", "url": "https://example.com/h", "secret": "s"}
  ],
  "inhibit_rules": [
    {"source_matchers": [{"name": "alertname", "op": "=", "value": "HostDown"}],
     "target_matchers": [{"name": "severity", "op": "=~", "value": "warning|critical"}],
     "equal": ["agent_id"]}
  ]
}`

func TestLoadConfig(t *testing.T) {
	t.Parallel()
	f, err := Load(writeFile(t, "a.json", goodConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if f.GroupWait.D() != 10*time.Second || f.RepeatInterval.D() != time.Hour {
		t.Fatalf("durations = %v, %v", f.GroupWait, f.RepeatInterval)
	}
	if len(f.Receivers) != 2 || len(f.InhibitRules) != 1 {
		t.Fatalf("receivers = %d, inhibit rules = %d", len(f.Receivers), len(f.InhibitRules))
	}

	recvs, err := BuildReceivers(f, quietLogger())
	if err != nil {
		t.Fatalf("BuildReceivers: %v", err)
	}
	if len(recvs) != 2 || recvs[0].Name() != "log" || recvs[1].Name() != "hook" {
		t.Fatalf("built %d receivers", len(recvs))
	}
}

// A typo in a notification config must fail at startup, not be silently ignored
// and discovered during an outage.
func TestLoadRejectsUnknownFields(t *testing.T) {
	t.Parallel()
	bad := `{"receivers":[{"name":"log","type":"log"}],"default_receivers":["log"],"typo":true}`
	if _, err := Load(writeFile(t, "a.json", bad)); err == nil {
		t.Fatal("an unknown field was accepted")
	}
}

func TestLoadRejectsTrailingData(t *testing.T) {
	t.Parallel()
	bad := `{"receivers":[{"name":"log","type":"log"}],"default_receivers":["log"]} {"more":1}`
	if _, err := Load(writeFile(t, "a.json", bad)); err == nil {
		t.Fatal("trailing data was accepted")
	}
}

func TestConfigValidation(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		json    string
		wantSub string
	}{
		"no receivers": {
			`{"default_receivers":["log"],"receivers":[]}`, "at least one receiver",
		},
		"nameless receiver": {
			`{"default_receivers":["log"],"receivers":[{"name":"","type":"log"}]}`, "name is required",
		},
		"duplicate name": {
			`{"default_receivers":["log"],"receivers":[{"name":"log","type":"log"},{"name":"log","type":"log"}]}`, "duplicate receiver name",
		},
		"unknown default": {
			`{"default_receivers":["nope"],"receivers":[{"name":"log","type":"log"}]}`, `unknown receiver "nope"`,
		},
		"no default": {
			`{"receivers":[{"name":"log","type":"log"}]}`, "default_receivers must name",
		},
		"empty inhibit matchers": {
			`{"default_receivers":["log"],"receivers":[{"name":"log","type":"log"}],
			  "inhibit_rules":[{"source_matchers":[],"target_matchers":[],"equal":[]}]}`, "inhibit_rules[0]",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeFile(t, "a.json", tc.json))
			if err == nil {
				t.Fatal("invalid config was accepted")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.wantSub)
			}
		})
	}
}

func TestBuildReceiversRejectsUnknownType(t *testing.T) {
	t.Parallel()
	f := &File{DefaultReceivers: []string{"x"}, Receivers: []ReceiverConfig{{Name: "x", Type: "carrier-pigeon"}}}
	if _, err := BuildReceivers(f, quietLogger()); err == nil {
		t.Fatal("an unknown receiver type was accepted")
	}
}

func TestDefaultConfigIsUsable(t *testing.T) {
	t.Parallel()
	f := Default()
	if err := f.Validate(); err != nil {
		t.Fatalf("the default config is invalid: %v", err)
	}
	recvs, err := BuildReceivers(f, quietLogger())
	if err != nil || len(recvs) != 1 {
		t.Fatalf("BuildReceivers on the default: %v, %d receivers", err, len(recvs))
	}
}

// A bad expression must fail at load, not on the first evaluation at 3am.
func TestLoadRulesCompilesEagerly(t *testing.T) {
	t.Parallel()
	good := `{"rules":[{"id":"a","name":"CPUHigh","expression":"cpu_usage_percent > 90","for":"1m","interval":"15s","enabled":true}]}`
	rs, err := LoadRules(writeFile(t, "r.json", good))
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	if len(rs) != 1 || rs[0].Compiled() == nil {
		t.Fatal("rule was not compiled at load time")
	}
	if rs[0].For.D() != time.Minute {
		t.Fatalf("for = %v", rs[0].For)
	}

	bad := `{"rules":[{"id":"a","name":"X","expression":"cpu >","enabled":true}]}`
	if _, err := LoadRules(writeFile(t, "bad.json", bad)); err == nil {
		t.Fatal("a rule with a bad expression was accepted")
	}
}

func TestLoadRulesRejectsDuplicateAndMissingIDs(t *testing.T) {
	t.Parallel()
	dup := `{"rules":[
	  {"id":"a","name":"X","expression":"cpu > 1","enabled":true},
	  {"id":"a","name":"Y","expression":"cpu > 2","enabled":true}]}`
	if _, err := LoadRules(writeFile(t, "d.json", dup)); err == nil {
		t.Fatal("duplicate rule ids were accepted")
	}

	noID := `{"rules":[{"name":"X","expression":"cpu > 1","enabled":true}]}`
	if _, err := LoadRules(writeFile(t, "n.json", noID)); err == nil {
		t.Fatal("a rule without an id was accepted")
	}
}

func TestLoadMissingFile(t *testing.T) {
	t.Parallel()
	if _, err := Load(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("a missing file was accepted")
	}
	if _, err := LoadRules(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("a missing rules file was accepted")
	}
}

// The examples shipped in the repository must actually load, or they are a lie.
func TestShippedExamplesAreValid(t *testing.T) {
	t.Parallel()
	f, err := Load("../../../examples/alerting/receivers.json")
	if err != nil {
		t.Fatalf("examples/alerting/receivers.json: %v", err)
	}
	if _, err := BuildReceivers(f, quietLogger()); err != nil {
		t.Fatalf("BuildReceivers on the shipped example: %v", err)
	}
	if _, err := LoadRules("../../../examples/alerting/rules.json"); err != nil {
		t.Fatalf("examples/alerting/rules.json: %v", err)
	}
}
