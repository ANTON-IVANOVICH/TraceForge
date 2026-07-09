package inhibit

import (
	"testing"

	"metrics-system/internal/alerting/alert"
	"metrics-system/internal/alerting/silence"
)

func matchers(t *testing.T, name string, op silence.MatchOp, value string) []silence.Matcher {
	t.Helper()
	m, err := silence.NewMatcher(name, op, value)
	if err != nil {
		t.Fatalf("NewMatcher(%s%s%q): %v", name, op, value, err)
	}
	return []silence.Matcher{m}
}

func mkAlert(name, tenant, host string, status alert.Status) *alert.Alert {
	labels := map[string]string{"alertname": name, "tenant": tenant, "host": host}
	return &alert.Alert{
		Fingerprint: alert.Fingerprint(name, labels),
		RuleName:    name,
		Status:      status,
		Labels:      labels,
	}
}

func TestRuleValidate(t *testing.T) {
	t.Parallel()
	src := matchers(t, "alertname", silence.MatchEqual, "HostDown")
	tgt := matchers(t, "alertname", silence.MatchEqual, "CPUHigh")

	if err := (Rule{SourceMatchers: src, TargetMatchers: tgt}).Validate(); err != nil {
		t.Errorf("valid rule rejected: %v", err)
	}
	if err := (Rule{TargetMatchers: tgt}).Validate(); err == nil {
		t.Error("rule with no source matcher should be rejected")
	}
	if err := (Rule{SourceMatchers: src}).Validate(); err == nil {
		t.Error("rule with no target matcher should be rejected")
	}
}

// TestInhibitedClassic is the canonical case: HostDown on a host suppresses
// CPUHigh on the same host, but not on a different one.
func TestInhibitedClassic(t *testing.T) {
	t.Parallel()
	r := Rule{
		SourceMatchers: matchers(t, "alertname", silence.MatchEqual, "HostDown"),
		TargetMatchers: matchers(t, "alertname", silence.MatchEqual, "CPUHigh"),
		Equal:          []string{"host"},
	}
	inh := New([]Rule{r})

	hostDown := mkAlert("HostDown", "t", "web-1", alert.StatusFiring)
	cpuSameHost := mkAlert("CPUHigh", "t", "web-1", alert.StatusFiring)
	cpuOtherHost := mkAlert("CPUHigh", "t", "web-2", alert.StatusFiring)

	if !inh.Inhibited(cpuSameHost, []*alert.Alert{hostDown}) {
		t.Error("HostDown web-1 should inhibit CPUHigh web-1")
	}
	if inh.Inhibited(cpuOtherHost, []*alert.Alert{hostDown}) {
		t.Error("Equal-label mismatch (host) must not inhibit")
	}
}

func TestInhibitedOnlyFiringSources(t *testing.T) {
	t.Parallel()
	r := Rule{
		SourceMatchers: matchers(t, "alertname", silence.MatchEqual, "HostDown"),
		TargetMatchers: matchers(t, "alertname", silence.MatchEqual, "CPUHigh"),
		Equal:          []string{"host"},
	}
	inh := New([]Rule{r})

	cpu := mkAlert("CPUHigh", "t", "web-1", alert.StatusFiring)
	resolvedSource := mkAlert("HostDown", "t", "web-1", alert.StatusResolved)

	if inh.Inhibited(cpu, []*alert.Alert{resolvedSource}) {
		t.Error("a resolved alert must not act as an inhibition source")
	}
}

func TestInhibitedCrossTenant(t *testing.T) {
	t.Parallel()
	r := Rule{
		SourceMatchers: matchers(t, "alertname", silence.MatchEqual, "HostDown"),
		TargetMatchers: matchers(t, "alertname", silence.MatchEqual, "CPUHigh"),
		Equal:          []string{"host"},
	}
	inh := New([]Rule{r})

	// Everything matches (host, alertnames) except the tenant differs.
	cpuTenantA := mkAlert("CPUHigh", "a", "web-1", alert.StatusFiring)
	hostDownTenantB := mkAlert("HostDown", "b", "web-1", alert.StatusFiring)

	if inh.Inhibited(cpuTenantA, []*alert.Alert{hostDownTenantB}) {
		t.Error("a source must not inhibit a target of a different tenant")
	}
}

func TestInhibitedSelf(t *testing.T) {
	t.Parallel()
	// No Equal labels, so a HostDown could inhibit any HostDown — which makes the
	// self-guard the only thing standing between an alert and inhibiting itself.
	r := Rule{
		SourceMatchers: matchers(t, "alertname", silence.MatchEqual, "HostDown"),
		TargetMatchers: matchers(t, "alertname", silence.MatchEqual, "HostDown"),
	}
	inh := New([]Rule{r})

	hd1 := mkAlert("HostDown", "t", "web-1", alert.StatusFiring)
	hd2 := mkAlert("HostDown", "t", "web-2", alert.StatusFiring)

	if inh.Inhibited(hd1, []*alert.Alert{hd1}) {
		t.Error("an alert must not inhibit itself")
	}
	if !inh.Inhibited(hd1, []*alert.Alert{hd2}) {
		t.Error("a distinct firing source should still inhibit")
	}
}

func TestInhibitedNoRules(t *testing.T) {
	t.Parallel()
	cpu := mkAlert("CPUHigh", "t", "web-1", alert.StatusFiring)
	hostDown := mkAlert("HostDown", "t", "web-1", alert.StatusFiring)
	if New(nil).Inhibited(cpu, []*alert.Alert{hostDown}) {
		t.Error("no rules means nothing is inhibited")
	}
}

func TestInhibitedEqualAbsentBothSides(t *testing.T) {
	t.Parallel()
	// When the Equal label is absent on both source and target it reads as equal,
	// so inhibition still applies.
	r := Rule{
		SourceMatchers: matchers(t, "alertname", silence.MatchEqual, "HostDown"),
		TargetMatchers: matchers(t, "alertname", silence.MatchEqual, "CPUHigh"),
		Equal:          []string{"rack"}, // neither alert carries a rack label
	}
	inh := New([]Rule{r})

	hostDown := mkAlert("HostDown", "t", "web-1", alert.StatusFiring)
	cpu := mkAlert("CPUHigh", "t", "web-1", alert.StatusFiring)

	if !inh.Inhibited(cpu, []*alert.Alert{hostDown}) {
		t.Error("an Equal label absent on both sides should count as equal")
	}
}
