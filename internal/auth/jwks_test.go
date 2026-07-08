package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func rsaToJWK(kid string, pub *rsa.PublicKey) jwk {
	return jwk{
		Kty: "RSA",
		Kid: kid,
		Use: "sig",
		Alg: "RS256",
		N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

// jwksServer serves a JWKS document that can be swapped at runtime.
type jwksServer struct {
	mu  sync.Mutex
	doc jwksDoc
}

func (s *jwksServer) set(keys ...jwk) {
	s.mu.Lock()
	s.doc = jwksDoc{Keys: keys}
	s.mu.Unlock()
}

func (s *jwksServer) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	doc := s.doc
	s.mu.Unlock()
	_ = json.NewEncoder(w).Encode(doc)
}

func TestKeySetResolvesAndVerifies(t *testing.T) {
	t.Parallel()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	js := &jwksServer{}
	js.set(rsaToJWK("kid-1", &key.PublicKey))
	srv := httptest.NewServer(js)
	defer srv.Close()

	ks := NewKeySet(srv.URL, 0)
	got, err := ks.Key("kid-1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.N.Cmp(key.PublicKey.N) != 0 {
		t.Fatal("resolved wrong key")
	}

	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	v, _ := NewRS256Verifier(ks, withClock(fixedClock(now)))
	tok := mintRS256(t, key, "kid-1", map[string]any{"tenant": "t", "exp": now.Add(time.Hour).Unix()})
	if _, err := v.Verify(tok); err != nil {
		t.Fatalf("verify via jwks: %v", err)
	}
}

func TestKeySetRotation(t *testing.T) {
	t.Parallel()
	k1, _ := rsa.GenerateKey(rand.Reader, 2048)
	k2, _ := rsa.GenerateKey(rand.Reader, 2048)
	js := &jwksServer{}
	js.set(rsaToJWK("kid-1", &k1.PublicKey))
	srv := httptest.NewServer(js)
	defer srv.Close()

	ks := NewKeySet(srv.URL, 0)
	ks.minRefetch = 0 // allow immediate on-demand refetch in the test
	if _, err := ks.Key("kid-1"); err != nil {
		t.Fatalf("initial: %v", err)
	}

	// Issuer rotates to a new kid; an unknown kid triggers a refetch that finds it.
	js.set(rsaToJWK("kid-2", &k2.PublicKey))
	if _, err := ks.Key("kid-2"); err != nil {
		t.Fatalf("rotation not picked up: %v", err)
	}
}

func TestKeySetRejectsWeakKey(t *testing.T) {
	t.Parallel()
	weak, _ := rsa.GenerateKey(rand.Reader, 1024) // below minRSABits
	js := &jwksServer{}
	js.set(rsaToJWK("kid-weak", &weak.PublicKey))
	srv := httptest.NewServer(js)
	defer srv.Close()

	ks := NewKeySet(srv.URL, 0)
	if _, err := ks.Key("kid-weak"); err == nil {
		t.Fatal("accepted a 1024-bit RSA key")
	}
}
