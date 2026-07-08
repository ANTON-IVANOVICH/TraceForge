package auth

import "context"

// JWTAuthenticator turns a verified bearer token into a Principal.
type JWTAuthenticator struct {
	verifier *Verifier
}

// NewJWTAuthenticator wraps a Verifier as an Authenticator.
func NewJWTAuthenticator(v *Verifier) *JWTAuthenticator {
	return &JWTAuthenticator{verifier: v}
}

// Authenticate verifies creds.Bearer and maps its claims to a Principal. Unknown
// scope/role strings are ignored rather than rejected: a valid token that lacks
// any recognised role authenticates fine but will fail authorization (403) when
// it tries to act.
func (a *JWTAuthenticator) Authenticate(_ context.Context, creds Credentials) (*Principal, error) {
	if creds.Bearer == "" {
		return nil, ErrNoCredentials
	}
	claims, err := a.verifier.Verify(creds.Bearer)
	if err != nil {
		return nil, err
	}
	return &Principal{
		Subject: claims.Subject,
		Tenant:  claims.Tenant,
		Roles:   knownRoles(claims.Roles),
	}, nil
}

// knownRoles keeps only the role strings that map to a defined Role.
func knownRoles(ss []string) []Role {
	var roles []Role
	for _, s := range ss {
		if r, err := ParseRole(s); err == nil {
			roles = append(roles, r)
		}
	}
	return roles
}
