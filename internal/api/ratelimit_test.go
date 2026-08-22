package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwt"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/ratelimit"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// tokenWithAzp mints a token like mwOrderTestIdP.token (middleware_order_test.go),
// but additionally sets azp — needed to exercise KeyFor's per-(subject,client)
// keying (spec §6.1), which mwOrderTestIdP.token never carries. Ground truth:
// azp is where Keycloak puts the client id; client_id is absent.
func tokenWithAzp(t *testing.T, idp *mwOrderTestIdP, subject, azp string, roles []string) string {
	t.Helper()
	roleVals := make([]any, len(roles))
	for i, r := range roles {
		roleVals[i] = r
	}
	tok, err := jwt.NewBuilder().
		Issuer(idp.srv.URL).
		Subject(subject).
		Audience([]string{"orbeat-api"}).
		Expiration(time.Now().Add(5*time.Minute)).
		IssuedAt(time.Now()).
		Claim("email", subject+"@example.com").
		Claim("azp", azp).
		Claim("realm_access", map[string]any{"roles": roleVals}).
		Build()
	if err != nil {
		t.Fatalf("build token: %v", err)
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256(), idp.signKey))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return string(signed)
}

// newRateLimitedServer builds a Server wired with a real auth.Validator
// (mirrors newPagingServer's shape) plus a caller-chosen *ratelimit.Limiter,
// and returns the idp (so callers can mint additional tokens, e.g. via
// tokenWithAzp, beyond the one admin token newPagingServer itself exposes)
// and the tenant name the resolver will lazily create rows under — needed by
// tests that assert "no user row exists" AFTER driving a request, the same
// way TestAllAdminRoutesRejectNonAdminBeforeAnyDBWrite does.
func newRateLimitedServer(t *testing.T, tenantPrefix string, l *ratelimit.Limiter) (srv *Server, st *store.Store, idp *mwOrderTestIdP, tenantName string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)
	tenantName = fmt.Sprintf("%s-%d", tenantPrefix, time.Now().UnixNano())
	idp = newMWOrderTestIdP(t)
	v, err := auth.NewValidator(ctx, auth.Config{Issuer: idp.srv.URL, Audience: "orbeat-api"})
	if err != nil {
		t.Fatalf("validator: %v", err)
	}
	srv = New(st, authz.NewResolver(st, tenantName), v, nil, nil)
	t.Cleanup(func() { _ = l.Close() })
	srv.SetRateLimiter(l)
	return srv, st, idp, tenantName
}

// TestRateLimitRejectsAndSkipsResolver is spec §8 items 3+4+5, through the
// REAL router: a request against a pre-drained bucket returns 429 with a
// correctly-computed Retry-After, and — the ordering assertion — the resolver
// must never have run for the rejected subject.
//
// The bucket is pre-drained IN-PROCESS via the exported ratelimit.KeyFor,
// never by sending burst allowed requests first (plan correction C1's rule):
// driving traffic would run resolver.Middleware -> UpsertUser on every
// allowed request, so the "resolver never ran" assertion would fail on
// CORRECT code. This is why KeyFor is exported at all.
//
// rps is deliberately 0.2, not the 50 default: at 50 rps the true wait is
// 20ms and any rounding-up implementation emits "1", so a hardcoded
// `Retry-After: 1` would pass undetected. At rps=0.2 the correct value is
// unambiguously "5" (ceil(1/0.2)), so only a real TokensAt computation
// produces it.
//
// This is also the test the Task 4 ordering red-proof runs against: overlay
// api.go's authed closure to `s.resolver.Middleware(s.rateLimited(h))` (wrong
// order) and only the trailing "resolver never ran" check should fail — the
// 429 and Retry-After assertions above it stay green, because the request is
// still ultimately rejected, just after the resolver already ran first.
func TestRateLimitRejectsAndSkipsResolver(t *testing.T) {
	limiter := ratelimit.New(0.2, 1, time.Minute, 100)
	srv, st, idp, tenantName := newRateLimitedServer(t, "ratelimit", limiter)

	const subject = "kc-ratelimit-drain"
	tok := idp.token(t, subject, []string{"orbeat-user"})

	// The token carries no azp, so KeyFor falls back to subject-only — mirror
	// that exactly when pre-draining, or the drained key won't match what
	// ratelimit.HTTP derives from this same token.
	key := ratelimit.KeyFor(auth.Principal{Subject: subject})
	if ok, _ := limiter.Allow(key); !ok {
		t.Fatal("setup: could not drain the bucket with the first call")
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/catalog", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("GET /v1/catalog with a drained bucket = %d, want 429, body=%s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q, want %q (ceil(1/0.2 rps))", got, "5")
	}

	// The resolver must never have run: no tenant/user row for this subject.
	// GetOrCreateTenantByName here is exactly what resolver.Middleware itself
	// calls, so this fetches-or-creates the SAME row the resolver would have
	// (mirrors TestAllAdminRoutesRejectNonAdminBeforeAnyDBWrite's pattern).
	ctx := context.Background()
	tn, err := st.GetOrCreateTenantByName(ctx, tenantName)
	if err != nil {
		t.Fatalf("tenant lookup: %v", err)
	}
	if _, err := st.GetUserBySubject(ctx, tn.ID, subject); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected no user row for the rate-limited subject (resolver must not run before the limiter), got err=%v", err)
	}
}

// TestRateLimitKeysByClientNotJustSubject pins KeyFor's per-(subject,azp)
// keying through the real router (spec §6.1, §8 item 6): draining one azp's
// bucket must not affect a DIFFERENT azp for the SAME subject.
func TestRateLimitKeysByClientNotJustSubject(t *testing.T) {
	limiter := ratelimit.New(0.01, 1, time.Minute, 100)
	srv, _, idp, _ := newRateLimitedServer(t, "ratelimit-azp", limiter)

	const subject = "kc-ratelimit-azp"
	tokA := tokenWithAzp(t, idp, subject, "claude", []string{"orbeat-user"})
	tokB := tokenWithAzp(t, idp, subject, "codex", []string{"orbeat-user"})

	// Drain client A's bucket only.
	keyA := ratelimit.KeyFor(auth.Principal{Subject: subject, ClientID: "claude"})
	if ok, _ := limiter.Allow(keyA); !ok {
		t.Fatal("setup: could not drain client A's bucket")
	}

	reqA := httptest.NewRequest(http.MethodGet, "/v1/catalog", nil)
	reqA.Header.Set("Authorization", "Bearer "+tokA)
	recA := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recA, reqA)
	if recA.Code != http.StatusTooManyRequests {
		t.Fatalf("client A (drained) = %d, want 429, body=%s", recA.Code, recA.Body)
	}

	reqB := httptest.NewRequest(http.MethodGet, "/v1/catalog", nil)
	reqB.Header.Set("Authorization", "Bearer "+tokB)
	recB := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recB, reqB)
	if recB.Code == http.StatusTooManyRequests {
		t.Fatalf("client B (same subject %q, different azp, undrained bucket) = 429, want NOT 429 — buckets must be independent per client", subject)
	}
}

// TestRateLimitDisabledAtRPSZero pins New's rps<=0 sentinel (spec §6, "0
// disables limiting") end to end: even 10x the configured burst must all
// succeed with the limiter attached and active on the Server.
func TestRateLimitDisabledAtRPSZero(t *testing.T) {
	const burst = 5
	limiter := ratelimit.New(0, burst, time.Minute, 100)
	srv, _, idp, _ := newRateLimitedServer(t, "ratelimit-off", limiter)

	const subject = "kc-ratelimit-off"
	tok := idp.token(t, subject, []string{"orbeat-user"})

	for i := 0; i < 10*burst; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/catalog", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d with rps=0 (disabled) was rate limited, want never", i)
		}
	}
}

// unauthenticatedRoutes are the only two mux.Handle routes in api.go that
// skip RequireAuth entirely — registered directly on the mux rather than
// through authed(...), admin(...), or an explicit RequireAuth wrap (api.go's
// GET /healthz and GET /openapi.yaml). Every other route requires a bearer
// token and therefore MUST be rate limited (spec §4.1).
var unauthenticatedRoutes = map[string]bool{
	"GET /healthz":      true,
	"GET /openapi.yaml": true,
}

// derivedLimitedRoutes returns every {method, path} api.go registers via
// mux.Handle EXCEPT the two genuinely unauthenticated routes above — derived
// from source (codeRoutes, openapi_test.go), not a hand-maintained list, so a
// newly added authenticated route is automatically covered here without
// anyone remembering to add a case (same discipline as derivedAdminRoutes,
// admin_gate_test.go:39). This is also what makes GET /v1/me appear: it sits
// outside both the authed(...) and admin(...) closures (api.go:210-212), so
// unless it is wired explicitly it would otherwise ship unlimited with
// nothing else here to catch it.
func derivedLimitedRoutes(t *testing.T) map[string]bool {
	t.Helper()
	all := codeRoutes(t)
	out := make(map[string]bool, len(all))
	for r := range all {
		if !unauthenticatedRoutes[r] {
			out[r] = true
		}
	}
	if len(out) == 0 {
		t.Fatal("derivedLimitedRoutes: empty result — codeRoutes extraction is stale")
	}
	return out
}

// TestAllAuthenticatedRoutesAreRateLimited is spec §8 item 7: the limiter
// must be wired into EVERY authenticated route, not just authed(...) or just
// admin(...), and GET /v1/me (outside both closures) must not be forgotten.
//
// The bucket is pre-drained ONCE for the probing principal via the exported
// KeyFor, mirroring TestRateLimitRejectsAndSkipsResolver: rate limiting is
// per-PRINCIPAL, not per-route, so one drained bucket rejects every
// subsequent request from that principal regardless of which route it hits —
// which is what lets a single loop assert every derived route without
// re-draining between iterations. An admin-role token satisfies both
// authed(...) (no role check) and admin(...) (requires orbeat-admin), so one
// token covers every derived route too.
func TestAllAuthenticatedRoutesAreRateLimited(t *testing.T) {
	limiter := ratelimit.New(0.01, 1, time.Minute, 100)
	srv, _, idp, _ := newRateLimitedServer(t, "ratelimit-coverage", limiter)

	const subject = "kc-ratelimit-coverage"
	tok := idp.token(t, subject, []string{"orbeat-admin"})

	key := ratelimit.KeyFor(auth.Principal{Subject: subject})
	if ok, _ := limiter.Allow(key); !ok {
		t.Fatal("setup: could not drain the bucket")
	}

	routes := derivedLimitedRoutes(t)
	var keys []string
	for r := range routes {
		keys = append(keys, r)
	}
	sort.Strings(keys)

	for _, route := range keys {
		route := route
		t.Run(route, func(t *testing.T) {
			parts := strings.SplitN(route, " ", 2)
			method, path := parts[0], parts[1]
			target := strings.ReplaceAll(path, "{id}", "00000000-0000-0000-0000-000000000000")

			req := httptest.NewRequest(method, target, nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("%s with a drained bucket = %d, want 429, body=%s", route, rec.Code, rec.Body)
			}
		})
	}
}
