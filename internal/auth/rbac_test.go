package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRolePolicy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		roles  []Role
		action Action
		want   bool
	}{
		{[]Role{RoleWriter}, ActionIngest, true},
		{[]Role{RoleWriter}, ActionQuery, false},
		{[]Role{RoleWriter}, ActionAdmin, false},
		{[]Role{RoleReader}, ActionQuery, true},
		{[]Role{RoleReader}, ActionIngest, false},
		{[]Role{RoleAdmin}, ActionIngest, true},
		{[]Role{RoleAdmin}, ActionQuery, true},
		{[]Role{RoleAdmin}, ActionAdmin, true},
		{[]Role{RoleReader, RoleWriter}, ActionIngest, true},
	}
	for _, c := range cases {
		p := &Principal{Roles: c.roles}
		if got := p.Can(c.action); got != c.want {
			t.Errorf("roles %v action %s: got %v want %v", c.roles, c.action, got, c.want)
		}
	}
	// A nil principal can do nothing.
	if (*Principal)(nil).Can(ActionQuery) {
		t.Error("nil principal granted access")
	}
}

// stubAuth returns a fixed principal for a specific credential, else NoCredentials.
type stubAuth struct {
	wantKey string
	p       *Principal
}

func (s stubAuth) Authenticate(_ context.Context, c Credentials) (*Principal, error) {
	if c.APIKey == s.wantKey && s.wantKey != "" {
		return s.p, nil
	}
	return nil, ErrNoCredentials
}

func TestChainFallthrough(t *testing.T) {
	t.Parallel()
	apiP := &Principal{Subject: "api", Tenant: "t", Roles: []Role{RoleWriter}}
	chain := Chain{
		stubAuth{wantKey: "key", p: apiP},
		testJWTChainAuth(t), // resolves a bearer token
	}

	// API key wins.
	if p, err := chain.Authenticate(context.Background(), Credentials{APIKey: "key"}); err != nil || p.Subject != "api" {
		t.Fatalf("api path: p=%+v err=%v", p, err)
	}
	// No key -> falls through to JWT.
	if p, err := chain.Authenticate(context.Background(), Credentials{Bearer: chainToken}); err != nil || p.Tenant != "jwt-tenant" {
		t.Fatalf("jwt path: p=%+v err=%v", p, err)
	}
	// Nothing -> ErrNoCredentials.
	if _, err := chain.Authenticate(context.Background(), Credentials{}); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("empty: got %v", err)
	}
}

var chainToken string

func testJWTChainAuth(t *testing.T) Authenticator {
	t.Helper()
	secret := []byte("chain-secret")
	now := time.Now()
	chainToken = mintHS256(secret, map[string]any{
		"sub": "u", "tenant": "jwt-tenant", "roles": []string{"reader"},
		"exp": now.Add(time.Hour).Unix(),
	})
	v, _ := NewHS256Verifier(secret)
	return NewJWTAuthenticator(v)
}
