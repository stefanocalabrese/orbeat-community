package ratelimit

import "github.com/stefanocalabrese/orbeat-community/internal/auth"

// KeyFor derives a bucket key from a principal. It is exported because tests
// must be able to name a key to pre-drain a bucket (see the API integration
// test), and because both adapters must derive keys identically.
//
// Falls back to subject-only when the token carries no azp. Never returns ""
// for a valid principal: an empty key would merge every caller into one
// bucket, a global limit wearing a per-principal costume.
func KeyFor(p auth.Principal) string {
	if p.ClientID == "" {
		return p.Subject
	}
	return p.Subject + "|" + p.ClientID
}
