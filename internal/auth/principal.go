// Package auth provides authentication (API keys and JWTs) and authorization
// (role-based access control) for the server's HTTP and gRPC transports, plus a
// tenant identity carried through context.Context for multi-tenant isolation.
package auth

import (
	"context"
	"fmt"
	"strings"
)

// Action is a coarse permission checked per request.
type Action string

const (
	ActionIngest Action = "ingest" // write metrics
	ActionQuery  Action = "query"  // read metrics
	ActionAdmin  Action = "admin"  // stats, profiling, other privileged endpoints
)

// Role bundles a set of actions. The mapping is fixed policy (not data): it is
// small, auditable, and cannot be widened by a crafted token.
type Role string

const (
	RoleWriter Role = "writer" // ingest only
	RoleReader Role = "reader" // query only
	RoleAdmin  Role = "admin"  // everything
)

// rolePolicy maps each role to the actions it grants.
var rolePolicy = map[Role]map[Action]bool{
	RoleWriter: {ActionIngest: true},
	RoleReader: {ActionQuery: true},
	RoleAdmin:  {ActionIngest: true, ActionQuery: true, ActionAdmin: true},
}

// Principal is an authenticated identity. Tenant scopes the data it may read or
// write; Roles decide which actions it may perform.
type Principal struct {
	Subject string // who: agent id, user id, service name
	Tenant  string // data isolation boundary
	Roles   []Role
}

// Can reports whether the principal may perform action, per the role policy.
func (p *Principal) Can(action Action) bool {
	if p == nil {
		return false
	}
	for _, r := range p.Roles {
		if rolePolicy[r][action] {
			return true
		}
	}
	return false
}

type ctxKey int

const principalKey ctxKey = iota

// WithPrincipal returns a context carrying the authenticated principal.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

// FromContext extracts the principal placed by the auth layer, if any.
func FromContext(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalKey).(*Principal)
	return p, ok && p != nil
}

// ParseRole validates and normalises a role string.
func ParseRole(s string) (Role, error) {
	switch Role(strings.ToLower(strings.TrimSpace(s))) {
	case RoleWriter:
		return RoleWriter, nil
	case RoleReader:
		return RoleReader, nil
	case RoleAdmin:
		return RoleAdmin, nil
	default:
		return "", fmt.Errorf("unknown role %q", s)
	}
}

// ParseRoles validates a list of role strings. An empty list is rejected: an
// identity with no role can do nothing and is almost certainly a config error.
func ParseRoles(ss []string) ([]Role, error) {
	if len(ss) == 0 {
		return nil, fmt.Errorf("at least one role is required")
	}
	roles := make([]Role, 0, len(ss))
	for _, s := range ss {
		r, err := ParseRole(s)
		if err != nil {
			return nil, err
		}
		roles = append(roles, r)
	}
	return roles, nil
}
