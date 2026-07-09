package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"
)

// Sinks keep the optimizer from eliding the verify/authorize calls under test.
var (
	sinkClaims *Claims
	sinkPrinc  *Principal
	sinkErr    error
	sinkBool   bool
)

// BenchmarkJWTVerifyHS256 measures the symmetric path: one SHA-256 HMAC plus
// JSON claim decoding. Compare against RS256 below — the RSA verify dominates.
func BenchmarkJWTVerifyHS256(b *testing.B) {
	secret := []byte("bench-secret")
	now := time.Now()
	v, err := NewHS256Verifier(secret, withClock(fixedClock(now)))
	if err != nil {
		b.Fatalf("verifier: %v", err)
	}
	tok := mintHS256(secret, map[string]any{
		"sub": "u", "tenant": "acme", "roles": []string{"reader"},
		"exp": now.Add(time.Hour).Unix(),
	})

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sinkClaims, sinkErr = v.Verify(tok)
	}
}

// BenchmarkJWTVerifyRS256 measures the asymmetric path. An RSA-2048 PKCS1v15
// verify is roughly two orders of magnitude slower than an HMAC, which is the
// whole reason a service caches or prefers symmetric tokens on the hot path.
func BenchmarkJWTVerifyRS256(b *testing.B) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		b.Fatalf("keygen: %v", err)
	}
	now := time.Now()
	v, err := NewRS256Verifier(StaticRSAKey{Public: &key.PublicKey}, withClock(fixedClock(now)))
	if err != nil {
		b.Fatalf("verifier: %v", err)
	}
	tok := mintRS256(b, key, "kid-1", map[string]any{
		"sub": "u", "tenant": "acme", "scope": "reader",
		"exp": now.Add(time.Hour).Unix(),
	})

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sinkClaims, sinkErr = v.Verify(tok)
	}
}

// BenchmarkAPIKeyAuthenticate contrasts a hit (key present) with a miss (key
// absent). Both hash the input and probe the map; a large gap between them would
// hint the lookup short-circuits on the secret — it must not.
func BenchmarkAPIKeyAuthenticate(b *testing.B) {
	a, err := NewAPIKeyAuthenticator(APIKeyConfig{Keys: []APIKeyEntry{
		{Key: "writer-key", Subject: "agent-1", Tenant: "acme", Roles: []string{"writer"}},
		{Key: "reader-key", Subject: "dash", Tenant: "acme", Roles: []string{"reader"}},
	}})
	if err != nil {
		b.Fatalf("build: %v", err)
	}
	ctx := context.Background()

	b.Run("hit", func(b *testing.B) {
		creds := Credentials{APIKey: "writer-key"}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			sinkPrinc, sinkErr = a.Authenticate(ctx, creds)
		}
	})
	b.Run("miss", func(b *testing.B) {
		creds := Credentials{APIKey: "no-such-key"}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			sinkPrinc, sinkErr = a.Authenticate(ctx, creds)
		}
	})
}

// BenchmarkRBACAllows measures Principal.Can, run once per authorized request.
// The "denied" case walks the whole role list before failing — the worst case.
func BenchmarkRBACAllows(b *testing.B) {
	writer := &Principal{Subject: "s", Tenant: "t", Roles: []Role{RoleWriter}}
	admin := &Principal{Subject: "s", Tenant: "t", Roles: []Role{RoleReader, RoleWriter, RoleAdmin}}

	b.Run("allow", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			sinkBool = writer.Can(ActionIngest)
		}
	})
	b.Run("deny", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			sinkBool = writer.Can(ActionAdmin)
		}
	})
	b.Run("admin-allow", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			sinkBool = admin.Can(ActionAdmin)
		}
	})
}
