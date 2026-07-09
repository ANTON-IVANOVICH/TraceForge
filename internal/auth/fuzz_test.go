package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// fuzzSecret and fuzzClock pin the HS256 verifier for the fuzzer so a run is
// reproducible: the clock is fixed well before any seed's expiry, so acceptance
// turns purely on the signature — which is the property under attack.
var (
	fuzzSecret = []byte("fuzz-secret-key")
	fuzzClock  = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
)

// FuzzJWTVerify hammers Verify with arbitrary token strings. Two invariants:
//
//  1. Verify never panics, whatever the segment count, encoding or JSON shape.
//  2. Unforgeability: err == nil implies the token has exactly three segments
//     and its signature segment equals HMAC-SHA256(secret, header"."payload).
//     Because the fuzzer never sees fuzzSecret, it cannot construct that HMAC
//     for a header/payload of its choosing — so a nil error can only come from a
//     token whose signature genuinely verifies, never from a forgery.
func FuzzJWTVerify(f *testing.F) {
	v, err := NewHS256Verifier(fuzzSecret, withClock(fixedClock(fuzzClock)))
	if err != nil {
		f.Fatalf("build verifier: %v", err)
	}

	valid := mintHS256(fuzzSecret, map[string]any{
		"sub": "u", "tenant": "acme", "roles": []string{"reader"},
		"exp": fuzzClock.Add(time.Hour).Unix(),
	})
	emptyHeader := "." + b64(map[string]any{"exp": fuzzClock.Add(time.Hour).Unix()}) + ".sig"
	noneAlg := b64(map[string]any{"alg": "none", "typ": "JWT"}) + "." +
		b64(map[string]any{"exp": fuzzClock.Add(time.Hour).Unix()}) + "."
	hugeExp := mintHS256(fuzzSecret, map[string]any{"exp": int64(1) << 62})

	for _, s := range []string{valid, "", "not.a.token", "a.b.c", emptyHeader, noneAlg, hugeExp} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, token string) {
		claims, err := v.Verify(token)
		if err != nil {
			return
		}
		if claims == nil {
			t.Fatalf("Verify returned (nil, nil) for %q", token)
		}
		parts := strings.Split(token, ".")
		if len(parts) != 3 {
			t.Fatalf("accepted a token with %d segments: %q", len(parts), token)
		}
		gotSig, decErr := base64.RawURLEncoding.DecodeString(parts[2])
		if decErr != nil {
			t.Fatalf("accepted a token with an undecodable signature: %q", token)
		}
		mac := hmac.New(sha256.New, fuzzSecret)
		mac.Write([]byte(parts[0] + "." + parts[1]))
		if !hmac.Equal(mac.Sum(nil), gotSig) {
			t.Fatalf("FORGERY: Verify accepted a token whose signature is not HMAC(secret, input): %q", token)
		}
	})
}

// FuzzParseClaims drives the payload decoder and every claim extractor with
// arbitrary bytes. parseClaims tolerates non-object and malformed JSON by
// erroring, and parseAudience/parseNumericDate/parseRolesClaim must survive any
// decoded `any` shape (string, number, bool, array, object, null) without
// panicking — they are reached with attacker-controlled JSON in production.
func FuzzParseClaims(f *testing.F) {
	for _, s := range []string{
		`{"iss":"x","sub":"y","aud":"a","exp":1,"nbf":2,"iat":3,"roles":["reader"],"scope":"a b"}`,
		`{"aud":["a","b",1,null],"exp":9999999999,"roles":[1,2,"admin"]}`,
		`{"exp":1e309,"nbf":-1e309}`, // overflow-prone numeric dates
		`{"aud":{},"roles":{},"scope":42}`,
		`[]`, `"string"`, `12345`, `true`, `null`, ``, `{`,
	} {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// parseClaims must never panic; a decode failure is a legitimate outcome.
		if c, err := parseClaims(data); err == nil && c == nil {
			t.Fatal("parseClaims returned (nil, nil)")
		}

		// Independently exercise the extractors on the raw decoded shape, which may
		// be any JSON value, not just an object.
		var decoded any
		dec := json.NewDecoder(strings.NewReader(string(data)))
		dec.UseNumber()
		if dec.Decode(&decoded) != nil {
			return
		}
		_ = parseAudience(decoded)
		_ = parseNumericDate(decoded)
		if m, ok := decoded.(map[string]any); ok {
			_ = parseAudience(m["aud"])
			_ = parseNumericDate(m["exp"])
			_ = parseRolesClaim(m)
		}
	})
}

// FuzzAPIKeyAuthenticate feeds arbitrary key strings at a fixed key set. It must
// never panic, and — since keys are indexed by SHA-256 digest — a nil error can
// only occur for one of the exact configured plaintext keys (a preimage the
// fuzzer cannot otherwise produce), never for an unknown key.
func FuzzAPIKeyAuthenticate(f *testing.F) {
	known := map[string]bool{"writer-key": true, "reader-key": true}
	a, err := NewAPIKeyAuthenticator(APIKeyConfig{Keys: []APIKeyEntry{
		{Key: "writer-key", Subject: "agent-1", Tenant: "acme", Roles: []string{"writer"}},
		{Key: "reader-key", Subject: "dash", Tenant: "acme", Roles: []string{"reader"}},
	}})
	if err != nil {
		f.Fatalf("build authenticator: %v", err)
	}

	for _, s := range []string{"writer-key", "reader-key", "", "nope", "WRITER-KEY", " writer-key "} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, key string) {
		p, err := a.Authenticate(context.Background(), Credentials{APIKey: key})
		switch {
		case err == nil:
			if p == nil {
				t.Fatalf("nil error but nil principal for key %q", key)
			}
			if !known[key] {
				t.Fatalf("authenticated an unknown key %q", key)
			}
		case key == "":
			if !errors.Is(err, ErrNoCredentials) {
				t.Fatalf("empty key: got %v, want ErrNoCredentials", err)
			}
		default:
			if p != nil {
				t.Fatalf("non-nil principal alongside error %v for key %q", err, key)
			}
		}
	})
}
