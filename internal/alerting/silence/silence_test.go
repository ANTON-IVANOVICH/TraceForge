package silence

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"metrics-system/internal/alerting/alert"
	"metrics-system/internal/clock"
)

var epoch = time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

func mustMatcher(t *testing.T, name string, op MatchOp, value string) Matcher {
	t.Helper()
	m, err := NewMatcher(name, op, value)
	if err != nil {
		t.Fatalf("NewMatcher(%s%s%q): %v", name, op, value, err)
	}
	return m
}

func TestSilenceValidate(t *testing.T) {
	t.Parallel()
	m := mustMatcher(t, "alertname", MatchEqual, "CPUHigh")
	cases := []struct {
		name    string
		sil     *Silence
		wantErr bool
	}{
		{"ok", &Silence{ID: "s1", Matchers: []Matcher{m}, StartsAt: epoch, EndsAt: epoch.Add(time.Hour)}, false},
		{"no id", &Silence{Matchers: []Matcher{m}, StartsAt: epoch, EndsAt: epoch.Add(time.Hour)}, true},
		{"no matchers", &Silence{ID: "s", StartsAt: epoch, EndsAt: epoch.Add(time.Hour)}, true},
		{"ends before starts", &Silence{ID: "s", Matchers: []Matcher{m}, StartsAt: epoch.Add(time.Hour), EndsAt: epoch}, true},
		{"ends equals starts", &Silence{ID: "s", Matchers: []Matcher{m}, StartsAt: epoch, EndsAt: epoch}, true},
		{"uncompiled regex matcher", &Silence{ID: "s", Matchers: []Matcher{{Name: "x", Op: MatchRegexp, Value: "y"}}, StartsAt: epoch, EndsAt: epoch.Add(time.Hour)}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.sil.Validate(); (err != nil) != c.wantErr {
				t.Errorf("Validate() err=%v wantErr=%v", err, c.wantErr)
			}
		})
	}
}

func TestSilenceActive(t *testing.T) {
	t.Parallel()
	sil := &Silence{StartsAt: epoch, EndsAt: epoch.Add(time.Hour)}
	if sil.Active(epoch.Add(-time.Second)) {
		t.Error("before StartsAt should be inactive")
	}
	if !sil.Active(epoch) {
		t.Error("StartsAt is inclusive")
	}
	if !sil.Active(epoch.Add(30 * time.Minute)) {
		t.Error("mid-window should be active")
	}
	if sil.Active(epoch.Add(time.Hour)) {
		t.Error("EndsAt is exclusive")
	}
}

func TestSilencerMutesAndTenantIsolation(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(epoch.Add(30 * time.Minute))
	s := NewSilencer(clk)

	m := mustMatcher(t, "alertname", MatchEqual, "CPUHigh")
	if err := s.Set(&Silence{ID: "a1", TenantID: "a", Matchers: []Matcher{m}, StartsAt: epoch, EndsAt: epoch.Add(time.Hour)}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	alertA := &alert.Alert{Labels: map[string]string{"alertname": "CPUHigh", "tenant": "a"}}
	alertB := &alert.Alert{Labels: map[string]string{"alertname": "CPUHigh", "tenant": "b"}}

	if !s.Mutes(alertA) {
		t.Error("tenant-a silence should mute tenant-a alert")
	}
	// SECURITY-CRITICAL: a tenant's silence must not reach across the boundary.
	if s.Mutes(alertB) {
		t.Error("tenant-a silence must not mute tenant-b alert")
	}

	// A silence with no tenant (auth off) mutes any tenant's matching alert.
	if err := s.Set(&Silence{ID: "g1", Matchers: []Matcher{m}, StartsAt: epoch, EndsAt: epoch.Add(time.Hour)}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !s.Mutes(alertB) {
		t.Error("global silence should mute any tenant")
	}

	// A non-matching alert is not muted.
	if s.Mutes(&alert.Alert{Labels: map[string]string{"alertname": "DiskFull", "tenant": "a"}}) {
		t.Error("non-matching alert should not be muted")
	}
}

func TestSilencerInactiveDoesNotMute(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(epoch.Add(2 * time.Hour)) // past EndsAt
	s := NewSilencer(clk)
	m := mustMatcher(t, "alertname", MatchEqual, "CPUHigh")
	if err := s.Set(&Silence{ID: "s1", Matchers: []Matcher{m}, StartsAt: epoch, EndsAt: epoch.Add(time.Hour)}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if s.Mutes(&alert.Alert{Labels: map[string]string{"alertname": "CPUHigh"}}) {
		t.Error("an expired silence must not mute")
	}
}

func TestSilencerCRUDClones(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(epoch)
	s := NewSilencer(clk)
	m := mustMatcher(t, "alertname", MatchEqual, "CPUHigh")

	if err := s.Set(&Silence{ID: "a1", TenantID: "a", Matchers: []Matcher{m}, StartsAt: epoch, EndsAt: epoch.Add(time.Hour), Comment: "orig"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set(&Silence{ID: "b1", TenantID: "b", Matchers: []Matcher{m}, StartsAt: epoch, EndsAt: epoch.Add(time.Hour)}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, ok := s.Get("a1")
	if !ok {
		t.Fatal("Get(a1) missing")
	}
	// Mutating the returned value must not leak into the store.
	got.Comment = "tampered"
	again, _ := s.Get("a1")
	if again.Comment != "orig" {
		t.Error("Get must return a clone the caller cannot mutate")
	}

	// List is tenant-scoped and sorted by ID.
	if tenantA := s.List("a"); len(tenantA) != 1 || tenantA[0].ID != "a1" {
		t.Errorf("List(a) = %+v", tenantA)
	}
	all := s.List("")
	if len(all) != 2 || all[0].ID != "a1" || all[1].ID != "b1" {
		t.Errorf("List(\"\") not sorted/complete: %+v", all)
	}

	if !s.Delete("a1") {
		t.Error("Delete(a1) should report true")
	}
	if s.Delete("a1") {
		t.Error("second Delete(a1) should report false")
	}
	if _, ok := s.Get("a1"); ok {
		t.Error("a1 should be gone")
	}
}

func TestSilencerSetRejectsInvalid(t *testing.T) {
	t.Parallel()
	s := NewSilencer(clock.NewFake(epoch))
	m := mustMatcher(t, "alertname", MatchEqual, "CPUHigh")
	if err := s.Set(&Silence{ID: "", Matchers: []Matcher{m}, StartsAt: epoch, EndsAt: epoch.Add(time.Hour)}); err == nil {
		t.Error("Set should reject an invalid silence")
	}
}

func TestSilencerGC(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(epoch)
	s := NewSilencer(clk)
	m := mustMatcher(t, "alertname", MatchEqual, "CPUHigh")

	if err := s.Set(&Silence{ID: "expired", Matchers: []Matcher{m}, StartsAt: epoch.Add(-2 * time.Hour), EndsAt: epoch.Add(-time.Hour)}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set(&Silence{ID: "live", Matchers: []Matcher{m}, StartsAt: epoch.Add(-time.Hour), EndsAt: epoch.Add(time.Hour)}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	s.GC()
	if _, ok := s.Get("expired"); ok {
		t.Error("GC should drop an expired silence")
	}
	if _, ok := s.Get("live"); !ok {
		t.Error("GC dropped a live silence")
	}
}

func TestSilencerConcurrent(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(epoch.Add(30 * time.Minute))
	s := NewSilencer(clk)
	m := mustMatcher(t, "alertname", MatchEqual, "CPUHigh")
	a := &alert.Alert{Labels: map[string]string{"alertname": "CPUHigh", "tenant": "a"}}

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("s-%d", i)
			sil := &Silence{ID: id, TenantID: "a", Matchers: []Matcher{m}, StartsAt: epoch, EndsAt: epoch.Add(time.Hour)}
			if err := s.Set(sil); err != nil {
				t.Errorf("Set: %v", err)
			}
			s.Mutes(a)
			s.List("")
			s.List("a")
			s.GC()
			s.Delete(id)
		}(i)
	}
	wg.Wait()
}
