package gateway

import (
	"context"
	"fmt"
	"net/http"
	"time"

	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
)

// principalCtxKey is the Extra key under which we stash the validated Principal,
// so getServer and the per-call middleware can recover it from the SDK's TokenInfo.
const principalCtxKey = "orbeat.principal"

// tokenInfoLifetime is the sentinel expiration stamped on the SDK TokenInfo.
// The SDK's RequireBearerToken rejects a TokenInfo whose Expiration is zero
// ("token missing expiration") or already past, as a belt-and-suspenders guard.
// auth.Principal carries no expiry, but auth.Validator.Validate has already
// fully enforced the underlying token's exp/nbf before we ever build a
// Principal, so we stamp a short bounded future value purely to satisfy the
// SDK's check. It is not an independent authorization lifetime: every request
// is re-validated against Keycloak's keys, so a revoked/expired upstream token
// fails at the verifier regardless of this sentinel.
const tokenInfoLifetime = 5 * time.Minute

// principalToTokenInfo packs a validated Principal into the SDK's TokenInfo.
// The whole Principal is carried in Extra (same process, our own producer/consumer).
func principalToTokenInfo(p auth.Principal) *mcpauth.TokenInfo {
	return &mcpauth.TokenInfo{
		UserID:     p.Subject,
		Expiration: time.Now().Add(tokenInfoLifetime),
		Extra:      map[string]any{principalCtxKey: p},
	}
}

// principalFromTokenInfo recovers the Principal stashed by principalToTokenInfo.
func principalFromTokenInfo(ti *mcpauth.TokenInfo) (auth.Principal, bool) {
	if ti == nil || ti.Extra == nil {
		return auth.Principal{}, false
	}
	p, ok := ti.Extra[principalCtxKey].(auth.Principal)
	return p, ok
}

// NewTokenVerifier returns an SDK TokenVerifier backed by the orbeat validator.
func NewTokenVerifier(v *auth.Validator) mcpauth.TokenVerifier { return newTokenVerifier(v) }

// newTokenVerifier adapts our OAuth 2.1 Validator to the SDK's TokenVerifier.
// On failure it returns an error unwrapping to mcpauth.ErrInvalidToken so the
// SDK responds 401 with the RFC 9728 WWW-Authenticate challenge.
func newTokenVerifier(v *auth.Validator) mcpauth.TokenVerifier {
	return func(ctx context.Context, token string, _ *http.Request) (*mcpauth.TokenInfo, error) {
		p, err := v.Validate(ctx, token)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", mcpauth.ErrInvalidToken, err)
		}
		return principalToTokenInfo(p), nil
	}
}
