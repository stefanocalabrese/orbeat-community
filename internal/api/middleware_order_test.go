package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jwt"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// mwOrderTestIdP mints RS256 tokens and serves OIDC discovery + JWKS — enough
// to drive Server.Handler() through the REAL auth.Validator end-to-end. This
// is deliberately NOT internal/auth's authTestServer (unexported there): the
// point of this test is to exercise the actual middleware composition wired
// in api.go's `admin` closure (RequireAuth -> RequireRole -> resolver.Middleware
// -> handler), which a call to authz.RequireRole in isolation (as the existing
// TestAdminRouteRequiresAdminRole / TestArtifactRBACNonAdminIs403 do) cannot
// catch a regression in — those tests would pass unchanged even if api.go put
// resolver.Middleware back before RequireRole.
type mwOrderTestIdP struct {
	srv     *httptest.Server
	signKey jwk.Key
}

func newMWOrderTestIdP(t *testing.T) *mwOrderTestIdP {
	t.Helper()
	raw, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	priv, err := jwk.Import(raw)
	if err != nil {
		t.Fatalf("import priv: %v", err)
	}
	_ = priv.Set(jwk.KeyIDKey, "test-key-1")
	_ = priv.Set(jwk.AlgorithmKey, jwa.RS256())
	pub, err := priv.PublicKey()
	if err != nil {
		t.Fatalf("pubkey: %v", err)
	}
	set := jwk.NewSet()
	_ = set.AddKey(pub)

	idp := &mwOrderTestIdP{signKey: priv}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":   idp.srv.URL,
			"jwks_uri": idp.srv.URL + "/protocol/openid-connect/certs",
		})
	})
	mux.HandleFunc("/protocol/openid-connect/certs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		buf, _ := json.Marshal(set)
		_, _ = w.Write(buf)
	})
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	return idp
}

func (idp *mwOrderTestIdP) token(t *testing.T, subject string, roles []string) string {
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

// TestAdminRouteDeniesNonAdminBeforeAnyDBWrite pins the audit-B4 middleware
// reorder (internal/api/api.go's `admin` closure) end-to-end, through the
// real Server.Handler() mux: a non-admin bearer token hitting an admin route
// must be rejected by RequireRole BEFORE resolver.Middleware ever runs, so it
// must 403 AND must leave no tenant/user row behind for that subject.
//
// Pre-fix (RequireAuth(resolver.Middleware(RequireRole(...)))), this test's
// second assertion fails: resolver.Middleware runs first and upserts the
// tenant+user row, and only then does RequireRole deny the request — so a
// flood of unauthorized admin-route probes would still pay for (and leave
// behind) a full resolve on every single request.
func TestAdminRouteDeniesNonAdminBeforeAnyDBWrite(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	tenantName := fmt.Sprintf("mwo-%d", time.Now().UnixNano())
	idp := newMWOrderTestIdP(t)
	v, err := auth.NewValidator(ctx, auth.Config{Issuer: idp.srv.URL, Audience: "orbeat-api"})
	if err != nil {
		t.Fatalf("validator: %v", err)
	}

	srv := New(st, authz.NewResolver(st, tenantName), v, nil, nil)

	const subject = "kc-nonadmin-1"
	tok := idp.token(t, subject, []string{"orbeat-user"})

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/servers", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin request to an admin route = %d, want 403, body=%s", rec.Code, rec.Body)
	}

	// Look the tenant up (creating it if this is the very first thing to touch
	// the DB in this test — irrelevant to what we're pinning) purely to scope
	// the user lookup; what matters is that THIS request's resolve never ran.
	tn, err := st.GetOrCreateTenantByName(ctx, tenantName)
	if err != nil {
		t.Fatalf("tenant lookup: %v", err)
	}
	if _, err := st.GetUserBySubject(ctx, tn.ID, subject); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected no user row for the denied non-admin subject, got err=%v", err)
	}
}

// TestAdminRouteAllowsAdminAndResolves is the positive counterpart: an admin
// token still gets through the reordered chain and still reaches a fully
// resolved context (proving resolver.Middleware still runs, just after the
// role check, not before it).
func TestAdminRouteAllowsAdminAndResolves(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	tenantName := fmt.Sprintf("mwo-admin-%d", time.Now().UnixNano())
	idp := newMWOrderTestIdP(t)
	v, err := auth.NewValidator(ctx, auth.Config{Issuer: idp.srv.URL, Audience: "orbeat-api"})
	if err != nil {
		t.Fatalf("validator: %v", err)
	}

	srv := New(st, authz.NewResolver(st, tenantName), v, nil, nil)

	const subject = "kc-admin-1"
	tok := idp.token(t, subject, []string{"orbeat-admin"})

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/servers", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("admin request = %d, want 200, body=%s", rec.Code, rec.Body)
	}

	tn, err := st.GetOrCreateTenantByName(ctx, tenantName)
	if err != nil {
		t.Fatalf("tenant lookup: %v", err)
	}
	if _, err := st.GetUserBySubject(ctx, tn.ID, subject); err != nil {
		t.Fatalf("expected the admin's request to have resolved a user row, got err=%v", err)
	}
}
