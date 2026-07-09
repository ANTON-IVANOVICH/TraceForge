package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"metrics-system/internal/alerting"
	"metrics-system/internal/alerting/rules"
	"metrics-system/internal/alerting/silence"
	"metrics-system/internal/auth"
)

// alertRoutes registers the alerting API. It is only called when alerting is
// enabled, so the default build exposes no extra surface.
func (h *Handler) alertRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/rules", h.listRules)
	mux.HandleFunc("POST /api/v1/rules", h.createRule)
	mux.HandleFunc("POST /api/v1/rules/preview", h.previewRule)
	mux.HandleFunc("GET /api/v1/rules/{id}", h.getRule)
	mux.HandleFunc("PUT /api/v1/rules/{id}", h.updateRule)
	mux.HandleFunc("DELETE /api/v1/rules/{id}", h.deleteRule)

	mux.HandleFunc("GET /api/v1/alerts", h.listAlerts)

	mux.HandleFunc("GET /api/v1/silences", h.listSilences)
	mux.HandleFunc("POST /api/v1/silences", h.createSilence)
	mux.HandleFunc("DELETE /api/v1/silences/{id}", h.deleteSilence)
}

// tenantOf returns the caller's tenant, or "" when auth is disabled. Every
// alerting operation is scoped by it; it is never read from the request body.
func tenantOf(r *http.Request) string {
	if p, ok := auth.FromContext(r.Context()); ok {
		return p.Tenant
	}
	return ""
}

// ruleRequest is the wire shape for creating or updating a rule. Enabled is a
// pointer so that an omitted field means "enabled" rather than "disabled" — a
// rule nobody asked to disable should run.
type ruleRequest struct {
	ID          string            `json:"id,omitempty"`
	Name        string            `json:"name"`
	Expression  string            `json:"expression"`
	For         rules.Duration    `json:"for,omitempty"`
	Interval    rules.Duration    `json:"interval,omitempty"`
	Severity    rules.Severity    `json:"severity,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Receivers   []string          `json:"receivers,omitempty"`
	Enabled     *bool             `json:"enabled,omitempty"`
}

func (req ruleRequest) toRule() *rules.Rule {
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	return &rules.Rule{
		ID:          req.ID,
		Name:        req.Name,
		Expression:  req.Expression,
		For:         req.For,
		Interval:    req.Interval,
		Severity:    req.Severity,
		Labels:      req.Labels,
		Annotations: req.Annotations,
		Receivers:   req.Receivers,
		Enabled:     enabled,
	}
}

func (h *Handler) listRules(w http.ResponseWriter, r *http.Request) {
	rs, err := h.alerting.ListRules(tenantOf(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rs == nil {
		rs = []*rules.Rule{}
	}
	writeJSON(w, http.StatusOK, rs)
}

func (h *Handler) getRule(w http.ResponseWriter, r *http.Request) {
	rule, err := h.alerting.GetRule(tenantOf(r), r.PathValue("id"))
	if err != nil {
		writeAlertingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

func (h *Handler) createRule(w http.ResponseWriter, r *http.Request) {
	var req ruleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	rule, err := h.alerting.PutRule(tenantOf(r), req.toRule())
	if err != nil {
		writeAlertingError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rule)
}

func (h *Handler) updateRule(w http.ResponseWriter, r *http.Request) {
	var req ruleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// The path is authoritative: a body that names a different rule must not
	// silently rewrite it.
	req.ID = r.PathValue("id")

	rule, err := h.alerting.PutRule(tenantOf(r), req.toRule())
	if err != nil {
		writeAlertingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

func (h *Handler) deleteRule(w http.ResponseWriter, r *http.Request) {
	if err := h.alerting.DeleteRule(tenantOf(r), r.PathValue("id")); err != nil {
		writeAlertingError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// previewRequest backtests an expression over historical data.
type previewRequest struct {
	Expression string         `json:"expression"`
	From       time.Time      `json:"from,omitempty"`
	To         time.Time      `json:"to,omitempty"`
	Step       rules.Duration `json:"step,omitempty"`
}

// previewRule evaluates an expression without saving it, so a user can see what
// a rule would have fired on before committing to it.
func (h *Handler) previewRule(w http.ResponseWriter, r *http.Request) {
	var req previewRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	results, err := h.alerting.Preview(r.Context(), tenantOf(r), req.Expression, req.From, req.To, req.Step.D())
	if err != nil {
		writeAlertingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "count": len(results)})
}

func (h *Handler) listAlerts(w http.ResponseWriter, r *http.Request) {
	active := h.alerting.ActiveAlerts(tenantOf(r))
	if active == nil {
		active = []*rules.AlertState{}
	}
	writeJSON(w, http.StatusOK, active)
}

// silenceRequest creates a silence. Duration is a convenience alternative to
// EndsAt ("mute this for 2h from now").
type silenceRequest struct {
	ID        string            `json:"id,omitempty"`
	Matchers  []silence.Matcher `json:"matchers"`
	StartsAt  time.Time         `json:"starts_at,omitempty"`
	EndsAt    time.Time         `json:"ends_at,omitempty"`
	Duration  rules.Duration    `json:"duration,omitempty"`
	CreatedBy string            `json:"created_by,omitempty"`
	Comment   string            `json:"comment,omitempty"`
}

func (h *Handler) listSilences(w http.ResponseWriter, r *http.Request) {
	sils := h.alerting.ListSilences(tenantOf(r))
	if sils == nil {
		sils = []*silence.Silence{}
	}
	writeJSON(w, http.StatusOK, sils)
}

func (h *Handler) createSilence(w http.ResponseWriter, r *http.Request) {
	var req silenceRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	starts := req.StartsAt
	if starts.IsZero() {
		starts = time.Now().UTC()
	}
	ends := req.EndsAt
	if ends.IsZero() && req.Duration > 0 {
		ends = starts.Add(req.Duration.D())
	}

	sil, err := h.alerting.PutSilence(tenantOf(r), &silence.Silence{
		ID:        req.ID,
		Matchers:  req.Matchers,
		StartsAt:  starts,
		EndsAt:    ends,
		CreatedBy: req.CreatedBy,
		Comment:   req.Comment,
	})
	if err != nil {
		writeAlertingError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sil)
}

func (h *Handler) deleteSilence(w http.ResponseWriter, r *http.Request) {
	if err := h.alerting.DeleteSilence(tenantOf(r), r.PathValue("id")); err != nil {
		writeAlertingError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodeJSON reads a capped, strict JSON body. It writes the error response and
// reports false when the body is unusable.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	defer func() { _ = r.Body.Close() }()
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid json")
		return false
	}
	return true
}

// writeAlertingError maps service errors to status codes. Anything that is not
// a known sentinel is a bad request: rule compilation and silence validation
// return plain errors describing what the caller got wrong.
func writeAlertingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, alerting.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, alerting.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden")
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}
