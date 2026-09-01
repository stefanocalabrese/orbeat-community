// Package auth validates OAuth 2.1 access tokens and enforces authentication.
package auth

import (
	"context"
	"errors"
	"strings"
)

// Principal is the authenticated identity extracted from a validated token.
type Principal struct {
	Subject string
	Email   string
	Roles   []string
	// ClientID is the azp claim: the OAuth client the token was issued to,
	// never the person or robot behind it. See principalFromClaims for why
	// azp and not client_id.
	ClientID string
	// PreferredUsername is the preferred_username claim, and it is carried
	// for exactly one reason: IsServiceAccount below. Nothing authorizes on
	// it, nothing displays it (the portal reads Email), and nothing stores
	// it -- users.display_name is set from Email by authz.Resolver.Resolve.
	// Treat it as an input to that one predicate rather than as an identity.
	PreferredUsername string
}

// serviceAccountUsernamePrefix is the fixed prefix Keycloak gives the user it
// creates behind a client with service accounts enabled: the username is
// literally "service-account-" + the client id, lowercased. Measured against
// a real Keycloak 26.2 rather than taken from documentation, by registering a
// client through the same OIDC dynamic-registration endpoint a virtual key
// uses and reading the client_credentials token it minted:
//
//	azp                = "61062fd7-55aa-460e-8437-1d90941979fb"
//	preferred_username = "service-account-61062fd7-55aa-460e-8437-1d90941979fb"
//
// while the same realm's human token carried azp "orbeat-cli" and
// preferred_username "alice". The gate that keeps this measurement from
// rotting into an assumption is TestServiceAccountTokenIsDistinguishable
// (internal/keycloak/dcr.ee_test.go), which re-derives both halves against a
// live Keycloak.
const serviceAccountUsernamePrefix = "service-account-"

// IsServiceAccount reports whether this token was issued to a CLIENT acting
// on its own behalf (an OAuth client_credentials grant) rather than to a
// person. It exists so internal/gateway can refuse a robot whose virtual_key
// row was destroyed instead of quietly resolving it as a human, which would
// give it a users row and a Community seat (see resolveVirtualKey,
// internal/gateway/virtualkey.ee.go).
//
// THE TEST IS AN EXACT EQUALITY, NOT A PREFIX, and that choice is the whole
// safety argument. A bare prefix check refuses any person whose IdP username
// happens to start with "service-account-"; requiring the username to equal
// the prefix plus THIS TOKEN'S OWN azp means a false positive needs a human
// account named after the exact client id it authenticated with, which is not
// something an operator reaches by accident. The failure modes are wildly
// asymmetric -- refusing a human breaks every login on the deployment, while
// failing to recognise a robot merely leaves today's behaviour in place -- so
// the predicate is deliberately built to be wrong in the harmless direction.
//
// EqualFold rather than ==: Keycloak lowercases usernames, so a client id
// carrying uppercase letters would produce a username that does not match
// byte for byte. Every client id orbeat mints is a lowercase uuid from
// Keycloak's own DCR endpoint, so this is defense against a client id created
// some other way, not against anything observed.
//
// IT IS A KEYCLOAK CONVENTION AND NOTHING MORE, which bounds what may be
// built on it. Against a non-Keycloak OIDC provider this returns false for a
// genuine service account, so any caller must treat a false as "not known to
// be a robot" and never as "proven to be a person". That is why the caller
// uses it to ADD a refusal rather than to grant anything.
func (p Principal) IsServiceAccount() bool {
	if p.ClientID == "" || p.PreferredUsername == "" {
		return false
	}
	return strings.EqualFold(p.PreferredUsername, serviceAccountUsernamePrefix+p.ClientID)
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
	// Only IsServiceAccount reads this. It arrives here only if
	// tokenToClaims (validator.go) copied it off the token, which is an
	// allow-list rather than a passthrough: adding the field here without
	// adding it there leaves the predicate permanently false with every test
	// in this package still green, which is why the end-to-end gate runs
	// against a real Keycloak token instead of a claims map.
	if pu, ok := claims["preferred_username"].(string); ok {
		p.PreferredUsername = pu
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
