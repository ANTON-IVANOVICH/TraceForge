package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func testAPIKeys(t *testing.T) *APIKeyAuthenticator {
	t.Helper()
	a, err := NewAPIKeyAuthenticator(APIKeyConfig{Keys: []APIKeyEntry{
		{Key: "writer-key", Subject: "agent-1", Tenant: "acme", Roles: []string{"writer"}},
		{Key: "reader-key", Subject: "dash", Tenant: "acme", Roles: []string{"reader"}},
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return a
}

func TestAPIKeyAuthenticate(t *testing.T) {
	t.Parallel()
	a := testAPIKeys(t)

	p, err := a.Authenticate(context.Background(), Credentials{APIKey: "writer-key"})
	if err != nil {
		t.Fatalf("valid key: %v", err)
	}
	if p.Tenant != "acme" || !p.Can(ActionIngest) || p.Can(ActionQuery) {
		t.Fatalf("unexpected principal: %+v", p)
	}

	if _, err := a.Authenticate(context.Background(), Credentials{APIKey: "nope"}); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("bad key: got %v, want ErrUnauthenticated", err)
	}
	if _, err := a.Authenticate(context.Background(), Credentials{}); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("no key: got %v, want ErrNoCredentials", err)
	}
}

func TestAPIKeyConfigValidation(t *testing.T) {
	t.Parallel()
	if _, err := NewAPIKeyAuthenticator(APIKeyConfig{}); err == nil {
		t.Error("empty config accepted")
	}
	if _, err := NewAPIKeyAuthenticator(APIKeyConfig{Keys: []APIKeyEntry{
		{Key: "k", Subject: "s", Tenant: "t", Roles: []string{"superuser"}},
	}}); err == nil {
		t.Error("unknown role accepted")
	}
	if _, err := NewAPIKeyAuthenticator(APIKeyConfig{Keys: []APIKeyEntry{
		{Key: "", Subject: "s", Roles: []string{"reader"}},
	}}); err == nil {
		t.Error("empty key accepted")
	}
}

func TestLoadAPIKeyConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	content := `{"keys":[{"key":"k1","subject":"a","tenant":"t1","roles":["writer"]}]}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadAPIKeyConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Keys) != 1 || cfg.Keys[0].Tenant != "t1" {
		t.Fatalf("parsed wrong: %+v", cfg)
	}
}
