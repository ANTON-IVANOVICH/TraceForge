package auth

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func b64(v any) string {
	data, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(data)
}

func mintHS256(secret []byte, claims map[string]any) string {
	signing := b64(map[string]any{"alg": "HS256", "typ": "JWT"}) + "." + b64(claims)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signing))
	return signing + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func mintRS256(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	signing := b64(map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}) + "." + b64(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func fixedClock(ts time.Time) func() time.Time { return func() time.Time { return ts } }

func TestHS256Valid(t *testing.T) {
	t.Parallel()
	secret := []byte("super-secret")
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	tok := mintHS256(secret, map[string]any{
		"sub":    "user-1",
		"tenant": "acme",
		"roles":  []string{"reader"},
		"exp":    now.Add(time.Hour).Unix(),
	})
	v, _ := NewHS256Verifier(secret, withClock(fixedClock(now)))
	c, err := v.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if c.Subject != "user-1" || c.Tenant != "acme" {
		t.Fatalf("claims: %+v", c)
	}
}

func TestHS256Rejects(t *testing.T) {
	t.Parallel()
	secret := []byte("super-secret")
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	v, _ := NewHS256Verifier(secret, withClock(fixedClock(now)))

	cases := map[string]string{
		"expired":       mintHS256(secret, map[string]any{"exp": now.Add(-2 * time.Hour).Unix()}),
		"not yet valid": mintHS256(secret, map[string]any{"exp": now.Add(time.Hour).Unix(), "nbf": now.Add(time.Hour).Unix()}),
		"missing exp":   mintHS256(secret, map[string]any{"sub": "x"}),
		"wrong secret":  mintHS256([]byte("other-secret"), map[string]any{"exp": now.Add(time.Hour).Unix()}),
	}
	for name, tok := range cases {
		if _, err := v.Verify(tok); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestVerifierRejectsAlgConfusionAndNone(t *testing.T) {
	t.Parallel()
	secret := []byte("super-secret")
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	hs, _ := NewHS256Verifier(secret, withClock(fixedClock(now)))

	// alg=none must never verify.
	noneTok := b64(map[string]any{"alg": "none", "typ": "JWT"}) + "." +
		b64(map[string]any{"exp": now.Add(time.Hour).Unix()}) + "."
	if _, err := hs.Verify(noneTok); err == nil {
		t.Error("alg=none accepted")
	}

	// An RS256-pinned verifier must refuse an HS256 token even if its bytes
	// verify as an HMAC (the classic algorithm-confusion attack).
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	rs, _ := NewRS256Verifier(StaticRSAKey{Public: &key.PublicKey}, withClock(fixedClock(now)))
	hsTok := mintHS256(secret, map[string]any{"exp": now.Add(time.Hour).Unix()})
	if _, err := rs.Verify(hsTok); err == nil {
		t.Error("RS256 verifier accepted an HS256 token")
	}
}

func TestRS256Valid(t *testing.T) {
	t.Parallel()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	tok := mintRS256(t, key, "kid-1", map[string]any{
		"sub":    "svc",
		"tenant": "beta",
		"scope":  "reader writer",
		"exp":    now.Add(time.Hour).Unix(),
	})
	v, _ := NewRS256Verifier(StaticRSAKey{Public: &key.PublicKey}, withClock(fixedClock(now)))
	c, err := v.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if c.Tenant != "beta" || len(c.Roles) != 2 {
		t.Fatalf("claims: %+v", c)
	}
}

func TestRS256RejectsTamperedAndWrongKey(t *testing.T) {
	t.Parallel()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	tok := mintRS256(t, key, "kid-1", map[string]any{"exp": now.Add(time.Hour).Unix()})

	// Wrong public key.
	wrong, _ := NewRS256Verifier(StaticRSAKey{Public: &other.PublicKey}, withClock(fixedClock(now)))
	if _, err := wrong.Verify(tok); err == nil {
		t.Error("verified with wrong key")
	}

	// Tampered payload.
	good, _ := NewRS256Verifier(StaticRSAKey{Public: &key.PublicKey}, withClock(fixedClock(now)))
	if _, err := good.Verify(tok[:len(tok)-4] + "AAAA"); err == nil {
		t.Error("verified tampered signature")
	}
}

func TestIssuerAudienceEnforced(t *testing.T) {
	t.Parallel()
	secret := []byte("s")
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	v, _ := NewHS256Verifier(secret,
		withClock(fixedClock(now)), WithIssuer("trace"), WithAudience("api"))

	ok := mintHS256(secret, map[string]any{"exp": now.Add(time.Hour).Unix(), "iss": "trace", "aud": []string{"api", "x"}})
	if _, err := v.Verify(ok); err != nil {
		t.Fatalf("valid iss/aud rejected: %v", err)
	}
	badIss := mintHS256(secret, map[string]any{"exp": now.Add(time.Hour).Unix(), "iss": "evil", "aud": "api"})
	if _, err := v.Verify(badIss); err == nil {
		t.Error("wrong issuer accepted")
	}
	badAud := mintHS256(secret, map[string]any{"exp": now.Add(time.Hour).Unix(), "iss": "trace", "aud": "other"})
	if _, err := v.Verify(badAud); err == nil {
		t.Error("wrong audience accepted")
	}
}
