package server

import (
	"log/slog"
	"net/http"
	"strings"

	"metrics-system/internal/auth"
)

// actionForRequest maps a request to the RBAC action it requires. The second
// return is false for public endpoints that need no authentication.
func actionForRequest(r *http.Request) (auth.Action, bool) {
	switch {
	case r.URL.Path == "/healthz":
		return "", false // liveness is public
	case r.URL.Path == "/" || r.URL.Path == "/ws" || strings.HasPrefix(r.URL.Path, "/static/"):
		// Dashboard shell, static assets and the WS handshake are public at the
		// middleware layer; the WS handler authenticates and tenant-scopes itself
		// (browsers can't set headers on a WebSocket handshake).
		return "", false
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/metrics":
		return auth.ActionIngest, true
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/query":
		return auth.ActionQuery, true
	default:
		// /debug/stats, /debug/pprof/* and anything else are privileged.
		return auth.ActionAdmin, true
	}
}

// Authenticate authenticates every non-public request, enforces the action's
// RBAC requirement, and stashes the principal in the context for the handlers
// (which use it to scope data to the caller's tenant). It is only added to the
// chain when auth is enabled, so the default build is unaffected.
func Authenticate(authenticator auth.Authenticator, logger *slog.Logger) Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			action, required := actionForRequest(r)
			if !required {
				next.ServeHTTP(w, r)
				return
			}

			principal, err := authenticator.Authenticate(r.Context(), credentialsFromRequest(r))
			if err != nil {
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeError(w, http.StatusUnauthorized, "unauthenticated")
				return
			}
			if !principal.Can(action) {
				logger.Debug("authorization denied",
					"subject", principal.Subject, "tenant", principal.Tenant, "action", action)
				writeError(w, http.StatusForbidden, "forbidden")
				return
			}
			next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
		})
	}
}

// credentialsFromRequest pulls an API key and/or bearer token from the request.
func credentialsFromRequest(r *http.Request) auth.Credentials {
	c := auth.Credentials{APIKey: r.Header.Get("X-API-Key")}
	if token, ok := auth.BearerToken(r.Header.Get("Authorization")); ok {
		c.Bearer = token
	}
	return c
}
