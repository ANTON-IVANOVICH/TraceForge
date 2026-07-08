package auth

import (
	"context"
	"errors"
	"strings"
)

var (
	// ErrNoCredentials means the request carried nothing to authenticate with.
	ErrNoCredentials = errors.New("no credentials")
	// ErrUnauthenticated means credentials were present but invalid.
	ErrUnauthenticated = errors.New("invalid credentials")
)

// Credentials is the raw material extracted from a request by the transport
// layer, before any scheme has been chosen.
type Credentials struct {
	APIKey string // opaque key (X-API-Key header / x-api-key metadata)
	Bearer string // JWT from an "Authorization: Bearer <token>" header
}

// Empty reports whether no credential was supplied at all.
func (c Credentials) Empty() bool { return c.APIKey == "" && c.Bearer == "" }

// BearerToken extracts the token from an Authorization header value. The
// auth-scheme is case-insensitive per RFC 7235, so "Bearer", "bearer" and
// "BEARER" are all accepted.
func BearerToken(header string) (string, bool) {
	const scheme = "bearer "
	if len(header) < len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", false
	}
	return strings.TrimSpace(header[len(scheme):]), true
}

// Authenticator turns credentials into a Principal. An authenticator that does
// not recognise the supplied credential kind returns ErrNoCredentials so a
// Chain can fall through to the next one.
type Authenticator interface {
	Authenticate(ctx context.Context, creds Credentials) (*Principal, error)
}

// Chain tries each authenticator in order. It returns the first success; if a
// credential was present but rejected it returns that hard error; if nothing
// matched it returns ErrNoCredentials.
type Chain []Authenticator

func (c Chain) Authenticate(ctx context.Context, creds Credentials) (*Principal, error) {
	var hardErr error
	for _, a := range c {
		p, err := a.Authenticate(ctx, creds)
		if err == nil {
			return p, nil
		}
		if !errors.Is(err, ErrNoCredentials) {
			hardErr = err
		}
	}
	if hardErr != nil {
		return nil, hardErr
	}
	return nil, ErrNoCredentials
}
