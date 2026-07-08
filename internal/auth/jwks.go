package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// minRSABits rejects undersized (weak) RSA keys served by a JWKS endpoint.
const minRSABits = 2048

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
	Alg string `json:"alg"`
	Use string `json:"use"`
}

type jwksDoc struct {
	Keys []jwk `json:"keys"`
}

// KeySet is a JWKS-backed RSAKeyResolver: it fetches a JSON Web Key Set over
// HTTP and resolves RSA public keys by kid. It supports key rotation two ways —
// a background periodic refresh and an on-demand (rate-limited) refetch when an
// unknown kid appears.
type KeySet struct {
	url        string
	httpClient *http.Client
	refresh    time.Duration // periodic refresh interval
	minRefetch time.Duration // floor between on-demand refetches
	clock      func() time.Time

	mu          sync.RWMutex
	keys        map[string]*rsa.PublicKey
	lastAttempt time.Time // time of the last fetch ATTEMPT (success or failure)

	fetchMu sync.Mutex // serialises network refetches
}

// NewKeySet builds a KeySet for the given JWKS URL. refresh <= 0 disables the
// periodic background refresh (on-demand refetch still works).
func NewKeySet(url string, refresh time.Duration) *KeySet {
	return &KeySet{
		url:        url,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		refresh:    refresh,
		minRefetch: 30 * time.Second,
		clock:      time.Now,
		keys:       map[string]*rsa.PublicKey{},
	}
}

// Key resolves the RSA public key for kid, refetching once (rate-limited) if the
// kid is unknown — the common case right after the issuer rotates its keys.
func (k *KeySet) Key(kid string) (*rsa.PublicKey, error) {
	k.mu.RLock()
	key, ok := k.keys[kid]
	last := k.lastAttempt
	k.mu.RUnlock()
	if ok {
		return key, nil
	}

	if !last.IsZero() && k.clock().Sub(last) < k.minRefetch {
		return nil, fmt.Errorf("unknown kid %q", kid)
	}
	if err := k.Fetch(); err != nil {
		return nil, err
	}

	k.mu.RLock()
	key, ok = k.keys[kid]
	k.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown kid %q", kid)
	}
	return key, nil
}

// Fetch pulls the JWKS and atomically swaps in the parsed key map. It records
// the attempt time up front and coalesces behind the minRefetch floor, so even
// FAILING fetches are throttled — otherwise a burst of tokens bearing unknown
// (attacker-chosen) kids during a JWKS outage would each launch a blocking
// fetch and pile up goroutines.
func (k *KeySet) Fetch() error {
	k.fetchMu.Lock()
	defer k.fetchMu.Unlock()

	k.mu.RLock()
	recent := !k.lastAttempt.IsZero() && k.clock().Sub(k.lastAttempt) < k.minRefetch
	k.mu.RUnlock()
	if recent {
		return nil // a fetch was just attempted; don't hammer the endpoint
	}
	k.mu.Lock()
	k.lastAttempt = k.clock()
	k.mu.Unlock()

	req, err := http.NewRequest(http.MethodGet, k.url, nil)
	if err != nil {
		return fmt.Errorf("jwks request: %w", err)
	}
	resp, err := k.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("jwks fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks fetch: status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("jwks read: %w", err)
	}
	keys, err := parseJWKS(body)
	if err != nil {
		return err
	}

	k.mu.Lock()
	k.keys = keys
	k.mu.Unlock()
	return nil
}

// Refresh runs the periodic background refresh until ctx is cancelled.
func (k *KeySet) Refresh(ctx context.Context, logger *slog.Logger) {
	if k.refresh <= 0 {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	t := time.NewTicker(k.refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := k.Fetch(); err != nil {
				logger.Warn("jwks refresh failed", "url", k.url, "error", err)
			}
		}
	}
}

func parseJWKS(body []byte) (map[string]*rsa.PublicKey, error) {
	var doc jwksDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("jwks parse: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey)
	for _, j := range doc.Keys {
		if !strings.EqualFold(j.Kty, "RSA") {
			continue // ignore non-RSA keys; we only verify RS256
		}
		if j.Use != "" && !strings.EqualFold(j.Use, "sig") {
			continue // ignore encryption keys
		}
		pub, err := parseRSAJWK(j)
		if err != nil {
			return nil, err
		}
		if j.Kid == "" {
			return nil, fmt.Errorf("jwks: RSA key without kid")
		}
		keys[j.Kid] = pub
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("jwks: no usable RSA keys")
	}
	return keys, nil
}

func parseRSAJWK(j jwk) (*rsa.PublicKey, error) {
	nb, err := base64.RawURLEncoding.DecodeString(j.N)
	if err != nil {
		return nil, fmt.Errorf("jwks kid %q: bad modulus: %w", j.Kid, err)
	}
	eb, err := base64.RawURLEncoding.DecodeString(j.E)
	if err != nil {
		return nil, fmt.Errorf("jwks kid %q: bad exponent: %w", j.Kid, err)
	}
	n := new(big.Int).SetBytes(nb)
	e := new(big.Int).SetBytes(eb)
	if n.Sign() == 0 || e.Sign() == 0 {
		return nil, fmt.Errorf("jwks kid %q: zero modulus or exponent", j.Kid)
	}
	if n.BitLen() < minRSABits {
		return nil, fmt.Errorf("jwks kid %q: RSA key too small (%d bits)", j.Kid, n.BitLen())
	}
	if !e.IsInt64() || e.Int64() > 1<<31-1 {
		return nil, fmt.Errorf("jwks kid %q: exponent out of range", j.Kid)
	}
	return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
}
