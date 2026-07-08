package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// APIKeyEntry is one configured key and the identity it maps to. The plaintext
// key lives only in the config source; at load time it is hashed and discarded.
type APIKeyEntry struct {
	Key     string   `json:"key"`
	Subject string   `json:"subject"`
	Tenant  string   `json:"tenant"`
	Roles   []string `json:"roles"`
}

// APIKeyConfig is the on-disk / inline shape for API-key definitions.
type APIKeyConfig struct {
	Keys []APIKeyEntry `json:"keys"`
}

// APIKeyAuthenticator resolves opaque keys to principals. Keys are indexed by
// their SHA-256 hash: because the map key is a cryptographic hash of the secret,
// a single changed byte scrambles the whole lookup key, so map-probe timing
// cannot be walked to recover the secret.
type APIKeyAuthenticator struct {
	byHash map[string]*Principal
}

// NewAPIKeyAuthenticator builds an authenticator from a config, hashing keys.
func NewAPIKeyAuthenticator(cfg APIKeyConfig) (*APIKeyAuthenticator, error) {
	if len(cfg.Keys) == 0 {
		return nil, fmt.Errorf("no api keys configured")
	}
	byHash := make(map[string]*Principal, len(cfg.Keys))
	for i, e := range cfg.Keys {
		if strings.TrimSpace(e.Key) == "" {
			return nil, fmt.Errorf("api key entry %d: empty key", i)
		}
		roles, err := ParseRoles(e.Roles)
		if err != nil {
			return nil, fmt.Errorf("api key entry %d (%s): %w", i, e.Subject, err)
		}
		sum := sha256.Sum256([]byte(e.Key))
		byHash[hex.EncodeToString(sum[:])] = &Principal{
			Subject: e.Subject,
			Tenant:  e.Tenant,
			Roles:   roles,
		}
	}
	return &APIKeyAuthenticator{byHash: byHash}, nil
}

// LoadAPIKeyConfig reads and parses an API-key config file (strict JSON).
func LoadAPIKeyConfig(path string) (APIKeyConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return APIKeyConfig{}, err
	}
	defer func() { _ = f.Close() }()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var cfg APIKeyConfig
	if err := dec.Decode(&cfg); err != nil {
		return APIKeyConfig{}, fmt.Errorf("parse api keys: %w", err)
	}
	return cfg, nil
}

// Authenticate resolves creds.APIKey. It returns ErrNoCredentials when no key
// was supplied so a Chain can try the next scheme.
func (a *APIKeyAuthenticator) Authenticate(_ context.Context, creds Credentials) (*Principal, error) {
	if creds.APIKey == "" {
		return nil, ErrNoCredentials
	}
	sum := sha256.Sum256([]byte(creds.APIKey))
	if p, ok := a.byHash[hex.EncodeToString(sum[:])]; ok {
		return p, nil
	}
	return nil, ErrUnauthenticated
}
