// Package auth validates OAuth 2.1 access tokens and enforces authentication.
package auth

import (
	"context"
	"errors"
)

// Principal is the authenticated identity extracted from a validated token.
type Principal struct {
	Subject  string
	Email    string
	Roles    []string
	ClientID string
}

type principalCtxKey struct{}

// WithPrincipal returns a copy of ctx carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// PrincipalFrom returns the Principal stored in ctx, if any.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalCtxKey{}).(Principal)
	return p, ok
}

// principalFromClaims extracts a Principal from a decoded claims map.
// sub is required; realm_access.roles is optional.
func principalFromClaims(claims map[string]any) (Principal, error) {
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return Principal{}, errors.New("token missing sub claim")
	}
	p := Principal{Subject: sub}
	if email, ok := claims["email"].(string); ok {
		p.Email = email
	}
	// azp ("authorized party") is where Keycloak puts the client id. Verified
	// against a real token: azp = "orbeat-cli" and there is NO client_id claim,
	// so reading client_id would silently always yield "".
	if azp, ok := claims["azp"].(string); ok {
		p.ClientID = azp
	}
	if ra, ok := claims["realm_access"].(map[string]any); ok {
		if roles, ok := ra["roles"].([]any); ok {
			for _, r := range roles {
				if s, ok := r.(string); ok {
					p.Roles = append(p.Roles, s)
				}
			}
		}
	}
	return p, nil
}
