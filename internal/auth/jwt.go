package auth

import (
	"crypto"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Alg is a JWS signing algorithm. Only these two are supported; "none" and any
// other value are always rejected.
type Alg string

const (
	AlgHS256 Alg = "HS256"
	AlgRS256 Alg = "RS256"
)

// Claims holds the registered claims we care about plus the tenant/roles
// extensions. Raw keeps the full decoded payload for callers that need more.
type Claims struct {
	Issuer    string
	Subject   string
	Audience  []string
	Expiry    time.Time
	NotBefore time.Time
	IssuedAt  time.Time
	Tenant    string
	Roles     []string
	Raw       map[string]any
}

// RSAKeyResolver returns the RSA public key for a given JWK key id (kid). A
// JWKS-backed KeySet and a single static key both satisfy it.
type RSAKeyResolver interface {
	Key(kid string) (*rsa.PublicKey, error)
}

// StaticRSAKey adapts one fixed public key to the RSAKeyResolver interface.
type StaticRSAKey struct{ Public *rsa.PublicKey }

func (s StaticRSAKey) Key(string) (*rsa.PublicKey, error) { return s.Public, nil }

// Verifier validates a compact JWS and its claims. It is pinned to exactly one
// algorithm: a verifier configured for RS256 will never HMAC-verify a token,
// which is what closes the classic RS256->HS256 "algorithm confusion" hole.
type Verifier struct {
	alg      Alg
	secret   []byte         // HS256
	rsaKeys  RSAKeyResolver // RS256
	issuer   string         // required iss when set
	audience string         // required aud when set
	leeway   time.Duration
	clock    func() time.Time
}

// VerifierOption tunes claim validation.
type VerifierOption func(*Verifier)

func WithIssuer(iss string) VerifierOption        { return func(v *Verifier) { v.issuer = iss } }
func WithAudience(aud string) VerifierOption      { return func(v *Verifier) { v.audience = aud } }
func WithLeeway(d time.Duration) VerifierOption   { return func(v *Verifier) { v.leeway = d } }
func withClock(f func() time.Time) VerifierOption { return func(v *Verifier) { v.clock = f } }

// NewHS256Verifier verifies HMAC-SHA256 tokens with a shared secret.
func NewHS256Verifier(secret []byte, opts ...VerifierOption) (*Verifier, error) {
	if len(secret) == 0 {
		return nil, errors.New("hs256: empty secret")
	}
	return newVerifier(AlgHS256, secret, nil, opts...), nil
}

// NewRS256Verifier verifies RSA-SHA256 tokens against keys from the resolver
// (typically a JWKS KeySet), selecting the key by the token's kid header.
func NewRS256Verifier(keys RSAKeyResolver, opts ...VerifierOption) (*Verifier, error) {
	if keys == nil {
		return nil, errors.New("rs256: nil key resolver")
	}
	return newVerifier(AlgRS256, nil, keys, opts...), nil
}

func newVerifier(alg Alg, secret []byte, keys RSAKeyResolver, opts ...VerifierOption) *Verifier {
	v := &Verifier{alg: alg, secret: secret, rsaKeys: keys, leeway: time.Minute, clock: time.Now}
	for _, o := range opts {
		o(v)
	}
	return v
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

// Verify checks the signature and claims of a compact JWS, returning the parsed
// claims on success.
func (v *Verifier) Verify(token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: malformed token", ErrUnauthenticated)
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: bad header encoding", ErrUnauthenticated)
	}
	var hdr jwtHeader
	if err := json.Unmarshal(headerBytes, &hdr); err != nil {
		return nil, fmt.Errorf("%w: bad header json", ErrUnauthenticated)
	}
	// Pin the algorithm: reject anything but the one this verifier was built for
	// (this rejects "none" and blocks alg-confusion attacks).
	if Alg(hdr.Alg) != v.alg {
		return nil, fmt.Errorf("%w: unexpected alg %q", ErrUnauthenticated, hdr.Alg)
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: bad signature encoding", ErrUnauthenticated)
	}
	signingInput := []byte(parts[0] + "." + parts[1])
	if err := v.verifySignature(hdr, signingInput, sig); err != nil {
		return nil, err
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: bad payload encoding", ErrUnauthenticated)
	}
	claims, err := parseClaims(payloadBytes)
	if err != nil {
		return nil, err
	}
	if err := v.validateClaims(claims); err != nil {
		return nil, err
	}
	return claims, nil
}

func (v *Verifier) verifySignature(hdr jwtHeader, signingInput, sig []byte) error {
	switch v.alg {
	case AlgHS256:
		mac := hmac.New(sha256.New, v.secret)
		mac.Write(signingInput)
		if !hmac.Equal(mac.Sum(nil), sig) {
			return fmt.Errorf("%w: bad signature", ErrUnauthenticated)
		}
		return nil
	case AlgRS256:
		if hdr.Kid == "" {
			return fmt.Errorf("%w: missing kid", ErrUnauthenticated)
		}
		pub, err := v.rsaKeys.Key(hdr.Kid)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrUnauthenticated, err)
		}
		sum := sha256.Sum256(signingInput)
		if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
			return fmt.Errorf("%w: bad signature", ErrUnauthenticated)
		}
		return nil
	default:
		return fmt.Errorf("%w: unsupported alg", ErrUnauthenticated)
	}
}

func (v *Verifier) validateClaims(c *Claims) error {
	now := v.clock()
	// exp is mandatory: a token that never expires is a footgun.
	if c.Expiry.IsZero() {
		return fmt.Errorf("%w: missing exp", ErrUnauthenticated)
	}
	if now.After(c.Expiry.Add(v.leeway)) {
		return fmt.Errorf("%w: token expired", ErrUnauthenticated)
	}
	if !c.NotBefore.IsZero() && now.Add(v.leeway).Before(c.NotBefore) {
		return fmt.Errorf("%w: token not yet valid", ErrUnauthenticated)
	}
	if v.issuer != "" && c.Issuer != v.issuer {
		return fmt.Errorf("%w: wrong issuer", ErrUnauthenticated)
	}
	if v.audience != "" && !containsString(c.Audience, v.audience) {
		return fmt.Errorf("%w: wrong audience", ErrUnauthenticated)
	}
	return nil
}

// parseClaims decodes the payload, tolerating the string-or-array shape of aud
// and the numeric-date shape of exp/nbf/iat.
func parseClaims(payload []byte) (*Claims, error) {
	var raw map[string]any
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%w: bad payload json", ErrUnauthenticated)
	}
	c := &Claims{Raw: raw}
	c.Issuer, _ = raw["iss"].(string)
	c.Subject, _ = raw["sub"].(string)
	c.Tenant, _ = raw["tenant"].(string)
	c.Audience = parseAudience(raw["aud"])
	c.Expiry = parseNumericDate(raw["exp"])
	c.NotBefore = parseNumericDate(raw["nbf"])
	c.IssuedAt = parseNumericDate(raw["iat"])
	c.Roles = parseRolesClaim(raw)
	return c, nil
}

func parseAudience(v any) []string {
	switch a := v.(type) {
	case string:
		if a == "" {
			return nil
		}
		return []string{a}
	case []any:
		out := make([]string, 0, len(a))
		for _, e := range a {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func parseNumericDate(v any) time.Time {
	n, ok := v.(json.Number)
	if !ok {
		return time.Time{}
	}
	f, err := n.Float64()
	if err != nil {
		return time.Time{}
	}
	sec := int64(f)
	nsec := int64((f - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC()
}

// parseRolesClaim accepts either a "roles" array or a space-delimited "scope"
// string (OIDC style), so tokens minted by different issuers both work.
func parseRolesClaim(raw map[string]any) []string {
	if arr, ok := raw["roles"].([]any); ok {
		out := make([]string, 0, len(arr))
		for _, e := range arr {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	if scope, ok := raw["scope"].(string); ok && scope != "" {
		return strings.Fields(scope)
	}
	return nil
}

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
