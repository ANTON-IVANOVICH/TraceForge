package rules

import (
	"errors"
	"testing"
	"time"
)

func compiledRule(t *testing.T, id, tenant string) *Rule {
	t.Helper()
	r := &Rule{ID: id, TenantID: tenant, Name: "N" + id, Expression: "cpu > 1", Enabled: true}
	if err := r.Compile(); err != nil {
		t.Fatalf("compile: %v", err)
	}
	return r
}

func TestMemoryRuleStoreCRUD(t *testing.T) {
	t.Parallel()
	s := NewMemoryRuleStore()

	if _, err := s.Get("nope"); !errors.Is(err, ErrRuleNotFound) {
		t.Fatalf("Get on a missing rule = %v, want ErrRuleNotFound", err)
	}
	if err := s.Delete("nope"); !errors.Is(err, ErrRuleNotFound) {
		t.Fatalf("Delete on a missing rule = %v, want ErrRuleNotFound", err)
	}

	if err := s.Put(compiledRule(t, "a", "t1")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get("a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != "a" {
		t.Fatalf("got %q", got.ID)
	}
	if err := s.Delete("a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get("a"); !errors.Is(err, ErrRuleNotFound) {
		t.Fatal("rule survived deletion")
	}
}

// Scheduling an uncompiled rule would panic on its first evaluation, so the
// store refuses it outright.
func TestMemoryRuleStoreRejectsUncompiled(t *testing.T) {
	t.Parallel()
	s := NewMemoryRuleStore()
	if err := s.Put(&Rule{ID: "a", Name: "N", Expression: "cpu > 1"}); err == nil {
		t.Fatal("an uncompiled rule was accepted")
	}
	if err := s.Put(&Rule{Name: "N"}); err == nil {
		t.Fatal("a rule without an ID was accepted")
	}
}

func TestMemoryRuleStoreListIsTenantScopedAndSorted(t *testing.T) {
	t.Parallel()
	s := NewMemoryRuleStore()
	for _, r := range []*Rule{
		compiledRule(t, "c", "t2"),
		compiledRule(t, "a", "t1"),
		compiledRule(t, "b", "t1"),
	} {
		if err := s.Put(r); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	all, _ := s.List("")
	if len(all) != 3 || all[0].ID != "a" || all[2].ID != "c" {
		t.Fatalf("List(\"\") = %v, want every rule sorted by ID", ids(all))
	}
	t1, _ := s.List("t1")
	if len(t1) != 2 || t1[0].ID != "a" || t1[1].ID != "b" {
		t.Fatalf("List(t1) = %v", ids(t1))
	}
}

// The store hands out clones; a caller mutating one must not corrupt a runner
// that is mid-evaluation.
func TestMemoryRuleStoreReturnsClones(t *testing.T) {
	t.Parallel()
	s := NewMemoryRuleStore()
	r := compiledRule(t, "a", "t1")
	r.Labels = map[string]string{"k": "v"}
	if err := s.Put(r); err != nil {
		t.Fatalf("Put: %v", err)
	}

	r.Labels["k"] = "mutated-after-put"
	got, _ := s.Get("a")
	if got.Labels["k"] != "v" {
		t.Fatal("Put stored the caller's map by reference")
	}
	got.Labels["k"] = "mutated-after-get"
	again, _ := s.Get("a")
	if again.Labels["k"] != "v" {
		t.Fatal("Get returned the store's map by reference")
	}
}

func ids(rs []*Rule) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

func TestMemoryStateStore(t *testing.T) {
	t.Parallel()
	s := NewMemoryStateStore()

	if err := s.Put(&AlertState{RuleID: "", Fingerprint: "f"}); err == nil {
		t.Fatal("a state without a rule id was accepted")
	}

	st := &AlertState{RuleID: "r1", Fingerprint: "f1", State: StateFiring, Labels: map[string]string{"host": "a"}}
	if err := s.Put(st); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Put(&AlertState{RuleID: "r1", Fingerprint: "f2", State: StatePending}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Put(&AlertState{RuleID: "r2", Fingerprint: "f3", State: StateFiring}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, _ := s.GetByRule("r1")
	if len(got) != 2 || got[0].Fingerprint != "f1" {
		t.Fatalf("GetByRule(r1) = %d states, want 2 sorted by fingerprint", len(got))
	}

	// Clones, not aliases.
	st.Labels["host"] = "mutated"
	got, _ = s.GetByRule("r1")
	if got[0].Labels["host"] != "a" {
		t.Fatal("Put stored the caller's state by reference")
	}

	if err := s.Delete("r1", "f2"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got, _ := s.GetByRule("r1"); len(got) != 1 {
		t.Fatalf("Delete removed %d states", 2-len(got))
	}

	if err := s.DeleteRule("r1"); err != nil {
		t.Fatalf("DeleteRule: %v", err)
	}
	if got, _ := s.GetByRule("r1"); len(got) != 0 {
		t.Fatal("DeleteRule left state behind")
	}

	all, _ := s.All()
	if len(all) != 1 || all[0].RuleID != "r2" {
		t.Fatalf("All() = %d states, want only r2's", len(all))
	}
}

func TestRuleCompileDefaultsAndValidation(t *testing.T) {
	t.Parallel()

	// A `for` inside the expression populates the rule's For.
	r := &Rule{ID: "a", Name: "N", Expression: "cpu > 90 for 3m"}
	if err := r.Compile(); err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if r.For.D() != 3*time.Minute {
		t.Fatalf("For = %v, want 3m from the expression's for clause", r.For)
	}
	if r.Interval.D() != DefaultInterval {
		t.Fatalf("Interval = %v, want the default", r.Interval)
	}
	if r.Severity != SeverityWarning {
		t.Fatalf("Severity = %q, want the warning default", r.Severity)
	}

	// An explicit For wins over the expression's clause.
	r2 := &Rule{ID: "a", Name: "N", Expression: "cpu > 90 for 3m", For: Duration(time.Minute)}
	if err := r2.Compile(); err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if r2.For.D() != time.Minute {
		t.Fatalf("For = %v, want the rule's explicit 1m", r2.For)
	}

	for name, rule := range map[string]*Rule{
		"no name":       {ID: "a", Expression: "cpu > 1"},
		"no expression": {ID: "a", Name: "N"},
		"bad severity":  {ID: "a", Name: "N", Expression: "cpu > 1", Severity: "urgent"},
		"bad syntax":    {ID: "a", Name: "N", Expression: "cpu >"},
		"negative for":  {ID: "a", Name: "N", Expression: "cpu > 1", For: Duration(-time.Second)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := rule.Compile(); err == nil {
				t.Fatal("Compile unexpectedly succeeded")
			}
		})
	}
}

func TestDurationJSON(t *testing.T) {
	t.Parallel()
	var d Duration
	if err := d.UnmarshalJSON([]byte(`"90s"`)); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if d.D() != 90*time.Second {
		t.Fatalf("d = %v", d)
	}
	if err := d.UnmarshalJSON([]byte(`1000000000`)); err != nil {
		t.Fatalf("UnmarshalJSON(nanos): %v", err)
	}
	if d.D() != time.Second {
		t.Fatalf("d = %v, want 1s", d)
	}
	if err := d.UnmarshalJSON([]byte(`"forever"`)); err == nil {
		t.Fatal("an unparsable duration was accepted")
	}
	b, err := Duration(2 * time.Minute).MarshalJSON()
	if err != nil || string(b) != `"2m0s"` {
		t.Fatalf("MarshalJSON = %s, %v", b, err)
	}
}
