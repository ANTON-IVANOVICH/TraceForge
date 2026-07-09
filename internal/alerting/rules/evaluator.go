package rules

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"text/template"
	"time"

	"metrics-system/internal/alerting/alert"
)

const (
	// DefaultResendDelay throttles re-emission of an alert that is already firing.
	// The grouper dedups too, but an unthrottled re-emit on every evaluation
	// would flood the channel between the two halves of the system.
	DefaultResendDelay = time.Minute

	// resolvedGrace is how long a resolved state is remembered. Keeping it lets
	// a flapping condition be recognised as a resurrection of the same alert
	// rather than a brand-new one; dropping it eventually keeps the store bounded.
	resolvedGrace = 15 * time.Minute

	// maxAnnotationLen caps a rendered annotation. Annotations end up in emails
	// and webhooks; a template that expands without bound must not.
	maxAnnotationLen = 4096
)

// Evaluator turns one evaluation of a rule's expression into alert lifecycle
// transitions. It holds no rules of its own — the manager owns scheduling — so
// it is trivially testable: give it a rule, a querier and an instant.
type Evaluator struct {
	states StateStore
	out    chan<- *alert.Alert
	logger *slog.Logger

	// ResendDelay is how often a still-firing alert is re-announced.
	ResendDelay time.Duration
}

// NewEvaluator wires an evaluator to its state store and output channel.
func NewEvaluator(states StateStore, out chan<- *alert.Alert, logger *slog.Logger) *Evaluator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Evaluator{states: states, out: out, logger: logger, ResendDelay: DefaultResendDelay}
}

// Evaluate runs one iteration of a rule: evaluate the expression, reconcile the
// resulting vector against the remembered states, and emit the alerts that
// crossed a lifecycle boundary.
func (e *Evaluator) Evaluate(ctx context.Context, r *Rule, q Querier, now time.Time) error {
	expr := r.Compiled()
	if expr == nil {
		return fmt.Errorf("rule %s is not compiled", r.ID)
	}
	vector, err := expr.Eval(ctx, q, now)
	if err != nil {
		return fmt.Errorf("eval expression: %w", err)
	}

	existing, err := e.states.GetByRule(r.ID)
	if err != nil {
		return fmt.Errorf("load alert state: %w", err)
	}
	byFingerprint := make(map[string]*AlertState, len(existing))
	for _, st := range existing {
		byFingerprint[st.Fingerprint] = st
	}

	active := make(map[string]bool, len(vector))
	for _, sample := range vector {
		labels := e.alertLabels(r, sample.Labels)
		fp := alert.Fingerprint(r.ID, labels)

		// Two samples can collapse to one fingerprint (an `or` that duplicates a
		// series, or an aggregation that drops the labels telling them apart).
		// Without this guard the second occurrence would look like a brand-new
		// alert and fire again in the same evaluation.
		if active[fp] {
			continue
		}
		active[fp] = true

		st, ok := byFingerprint[fp]
		if !ok {
			st = &AlertState{
				RuleID:      r.ID,
				Fingerprint: fp,
				Labels:      labels,
				State:       StatePending,
				ActiveAt:    now,
			}
			byFingerprint[fp] = st
		}
		st.Labels = labels
		st.Value = sample.Value
		st.LastEvalAt = now

		// A condition that had gone away and came back starts its `for` window
		// over: an alert must hold continuously, not cumulatively.
		if st.State == StateResolved || st.State == StateInactive {
			st.State = StatePending
			st.ActiveAt = now
			st.ResolvedAt = nil
			st.FiredAt = nil
		}

		switch {
		case st.State == StatePending && now.Sub(st.ActiveAt) >= r.For.D():
			// `for` has elapsed (a zero `for` fires on the first evaluation).
			t := now
			st.State = StateFiring
			st.FiredAt = &t
			st.LastSentAt = now
			if err := e.emit(ctx, e.buildAlert(r, st, alert.StatusFiring, now)); err != nil {
				return err
			}
		case st.State == StateFiring && now.Sub(st.LastSentAt) >= e.resendDelay():
			st.LastSentAt = now
			if err := e.emit(ctx, e.buildAlert(r, st, alert.StatusFiring, now)); err != nil {
				return err
			}
		}

		if err := e.states.Put(st); err != nil {
			return fmt.Errorf("save alert state: %w", err)
		}
	}

	// Whatever the vector no longer contains has stopped breaching the condition.
	for fp, st := range byFingerprint {
		if active[fp] {
			continue
		}
		switch st.State {
		case StateFiring:
			// Resolution must be announced. An alerting system that never says
			// "it is over" is just a spam generator.
			t := now
			st.State = StateResolved
			st.ResolvedAt = &t
			st.LastEvalAt = now
			if err := e.emit(ctx, e.buildAlert(r, st, alert.StatusResolved, now)); err != nil {
				return err
			}
			if err := e.states.Put(st); err != nil {
				return fmt.Errorf("save alert state: %w", err)
			}
		case StatePending:
			// It never fired, so nobody was told about it; forget it silently.
			if err := e.states.Delete(r.ID, fp); err != nil {
				return err
			}
		case StateResolved:
			if st.ResolvedAt == nil || now.Sub(*st.ResolvedAt) > resolvedGrace {
				if err := e.states.Delete(r.ID, fp); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// ResolveAll emits a resolution for every alert of a rule that is currently
// firing, then forgets the rule's state. It is what a rule being deleted or
// disabled must do: the alerts it already announced would otherwise never be
// resolved, so the notifier's group would never empty and would keep reminding
// receivers about an incident nobody is watching any more.
func (e *Evaluator) ResolveAll(ctx context.Context, r *Rule, now time.Time) error {
	states, err := e.states.GetByRule(r.ID)
	if err != nil {
		return fmt.Errorf("load alert state: %w", err)
	}
	for _, st := range states {
		if st.State != StateFiring {
			continue
		}
		t := now
		st.State = StateResolved
		st.ResolvedAt = &t
		if err := e.emit(ctx, e.buildAlert(r, st, alert.StatusResolved, now)); err != nil {
			return err
		}
	}
	return e.states.DeleteRule(r.ID)
}

func (e *Evaluator) resendDelay() time.Duration {
	if e.ResendDelay <= 0 {
		return DefaultResendDelay
	}
	return e.ResendDelay
}

// emit hands an alert to the notifier. It blocks while the channel is full —
// backpressure onto evaluation is correct here, because dropping an alert is
// worse than evaluating a rule late — but never past the evaluation deadline.
func (e *Evaluator) emit(ctx context.Context, a *alert.Alert) error {
	select {
	case e.out <- a:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// alertLabels merges the series labels with the rule's own, then supplies
// alertname and severity unless the rule already set them explicitly.
//
// The tenant label is re-stamped last and unconditionally, because it is the key
// every downstream isolation check reads (ActiveAlerts, the dashboard hub,
// silences, inhibition). Two ways it would otherwise be wrong: a rule could
// carry `labels: {"tenant": "other"}` and forge the attribution of its alerts,
// and an aggregation such as `max by (agent_id) (cpu)` drops the label entirely,
// leaving the alert invisible to the very tenant that owns it.
func (e *Evaluator) alertLabels(r *Rule, sampleLabels map[string]string) map[string]string {
	labels := alert.MergeLabels(sampleLabels, r.Labels)
	if _, ok := labels["alertname"]; !ok {
		labels["alertname"] = r.Name
	}
	if _, ok := labels["severity"]; !ok {
		labels["severity"] = string(r.Severity)
	}
	if r.TenantID != "" {
		labels["tenant"] = r.TenantID
	} else {
		delete(labels, "tenant")
	}
	return labels
}

func (e *Evaluator) buildAlert(r *Rule, st *AlertState, status alert.Status, now time.Time) *alert.Alert {
	a := &alert.Alert{
		Fingerprint: st.Fingerprint,
		RuleID:      r.ID,
		RuleName:    r.Name,
		Status:      status,
		Severity:    string(r.Severity),
		Labels:      alert.CloneLabels(st.Labels),
		Annotations: e.renderAnnotations(r, st),
		StartsAt:    st.ActiveAt,
		Value:       st.Value,
		Receivers:   append([]string(nil), r.Receivers...),
	}
	if status == alert.StatusResolved {
		end := now
		if st.ResolvedAt != nil {
			end = *st.ResolvedAt
		}
		a.EndsAt = &end
	}
	return a
}

// renderAnnotations expands each annotation as a text/template over the alert's
// value and labels, e.g. "CPU is {{ .Value }}% on {{ .Labels.agent_id }}".
// A broken template must not break evaluation: the raw text is passed through
// and the failure is logged, because a typo in a summary is not a reason to
// stop alerting about a real outage.
func (e *Evaluator) renderAnnotations(r *Rule, st *AlertState) map[string]string {
	if len(r.Annotations) == 0 {
		return nil
	}
	data := struct {
		Value  float64
		Labels map[string]string
	}{Value: st.Value, Labels: st.Labels}

	out := make(map[string]string, len(r.Annotations))
	for k, text := range r.Annotations {
		tmpl, err := template.New(k).Option("missingkey=zero").Parse(text)
		if err != nil {
			e.logger.Debug("annotation template parse failed", "rule", r.ID, "annotation", k, "error", err)
			out[k] = text
			continue
		}
		var b strings.Builder
		if err := tmpl.Execute(&b, data); err != nil {
			e.logger.Debug("annotation template execute failed", "rule", r.ID, "annotation", k, "error", err)
			out[k] = text
			continue
		}
		s := b.String()
		if len(s) > maxAnnotationLen {
			s = s[:maxAnnotationLen]
		}
		out[k] = s
	}
	return out
}
