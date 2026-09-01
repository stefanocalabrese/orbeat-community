package authz

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// TestNewResolverWiresSeatLimitFromTheEditionExtensionPoint pins the one
// statement about Resolver.seatLimit that is true in BOTH editions:
// NewResolver reads the extension point (seatlimit.ee.go /
// seatlimit.community.go) rather than deciding for itself. The Enterprise
// VALUE assertions, communitySeatLimit() == 0 and NewResolver's default being
// 0, moved to seatlimit.ee_test.go, which communitygen drops, because in a
// generated Community tree both are false by design (seatlimit.community.go
// returns 10).
//
// Stated honestly, the same caveat as internal/api's
// TestNewWiresLimitsFromTheEditionExtensionPoint: in THIS build both sides
// are 0, so a NewResolver that hard-coded 0 would still pass here and only
// seatlimit.ee_test.go's value pin would catch it. In a generated Community
// tree it is the decisive wiring check, and the only one that can live in a
// file surviving generation.
func TestNewResolverWiresSeatLimitFromTheEditionExtensionPoint(t *testing.T) {
	r := NewResolver(nil, "x")
	if r.seatLimit != communitySeatLimit() {
		t.Fatalf("NewResolver's seatLimit = %d, want communitySeatLimit()'s %d, NewResolver must read "+
			"the edition extension point, not decide for itself", r.seatLimit, communitySeatLimit())
	}
}

// TestCheckSeatCapBothBoundaries is the decisive gate for the seat cap
// (docs/plans/orbeat-community-caps-2026-08-19.md Task 4): it must fire at
// N+1 and NOT at N. A small limit (2) is poked directly onto the Resolver
// rather than relying on the real Community number (10, unreachable from
// this repo's own Enterprise build). The boundary arithmetic under test
// does not depend on which number is configured.
//
// Red-proven by hand (not committed): changing `n >= r.seatLimit` in
// checkSeatCap (seatcap.go) to `n > r.seatLimit` makes the third Resolve
// below wrongly succeed; changing it to `n >= r.seatLimit-1` makes the
// second Resolve wrongly fail. Both mutations were applied, observed to
// fail exactly the assertion they should, and reverted.
func TestCheckSeatCapBothBoundaries(t *testing.T) {
	ctx := context.Background()
	s, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(s.Close)

	r := NewResolver(s, "seatcap-boundaries-"+t.Name())
	r.seatLimit = 2

	// n=0 < limit=2: the first brand-new subject is admitted.
	if _, err := r.Resolve(ctx, auth.Principal{Subject: "seat-1", Email: "s1@x.io"}); err != nil {
		t.Fatalf("first subject (n=0) should be admitted, got %v", err)
	}

	// n=1 < limit=2: the second (the "at N" boundary, filling the LAST seat
	// must never itself be blocked) is also admitted.
	if _, err := r.Resolve(ctx, auth.Principal{Subject: "seat-2", Email: "s2@x.io"}); err != nil {
		t.Fatalf("second subject (n=1 < limit=2) should be admitted, got %v", err)
	}

	// n=2 == limit=2: a third brand-new subject is rejected (the "N+1" boundary).
	_, err = r.Resolve(ctx, auth.Principal{Subject: "seat-3", Email: "s3@x.io"})
	var seatErr SeatLimitError
	if !errors.As(err, &seatErr) {
		t.Fatalf("third subject (n=2 >= limit=2) should be rejected with SeatLimitError, got %v", err)
	}
	if seatErr.Max != 2 || seatErr.Current != 2 {
		t.Fatalf("SeatLimitError = %+v, want Max=2 Current=2", seatErr)
	}
}

// TestCheckSeatCapNeverBlocksExistingUser is the direct verification that an
// existing user is never blocked by the cap. No brief states that
// requirement: an earlier version of this comment quoted one, and
// checkSeatCap's own doc comment (seatcap.go) records where the quotation
// came from and what actually justifies the rule. The verification itself is
// unaffected: a subject already known to the tenant is re-resolved
// repeatedly WHILE the tenant is at its seat cap, and must never be
// rejected, while a genuinely brand-new subject, at the same moment, IS
// rejected. This is what proves checkSeatCap's existence-check-first
// ordering (seatcap.go) actually holds at the Resolve level, not just by
// reading the code.
//
// Red-proven by hand (not committed): removing checkSeatCap's early return
// on a known subject (the `if err == nil { return nil }` branch), so every
// call falls through to the count check regardless of whether the subject
// is already seated, makes this test fail on its very first re-resolve
// attempt, with the actual SeatLimitError a naive "count first"
// implementation would produce. Applied, observed to fail, reverted.
func TestCheckSeatCapNeverBlocksExistingUser(t *testing.T) {
	ctx := context.Background()
	s, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(s.Close)

	r := NewResolver(s, "seatcap-existing-"+t.Name())
	r.seatLimit = 1

	p := auth.Principal{Subject: "seat-existing", Email: "e@x.io"}
	rc1, err := r.Resolve(ctx, p)
	if err != nil {
		t.Fatalf("seed the one seat: %v", err)
	}

	// Now at cap (n=1, limit=1). Re-resolving the SAME subject must never be
	// blocked, however many times it happens.
	for i := 0; i < 3; i++ {
		rc, err := r.Resolve(ctx, p)
		if err != nil {
			t.Fatalf("re-resolving an already-seated user at cap must never be blocked (attempt %d): %v", i, err)
		}
		if rc.UserID != rc1.UserID {
			t.Fatalf("attempt %d: user id drifted, %s vs %s", i, rc.UserID, rc1.UserID)
		}
	}

	// A brand-new subject, at the same cap, IS rejected, proving the test
	// above isn't vacuously passing because the cap never fires at all.
	other := auth.Principal{Subject: "seat-new", Email: "n@x.io"}
	_, err = r.Resolve(ctx, other)
	var seatErr SeatLimitError
	if !errors.As(err, &seatErr) {
		t.Fatalf("a brand-new subject at cap should be rejected, got %v", err)
	}
}

// TestCheckSeatCapIgnoresStaleExistingUser proves the active-window staleness
// (spec §3.2) does not turn a returning, previously-known subject into a
// "new signup" for cap purposes: checkSeatCap's existence check
// (GetUserBySubject) finds the row regardless of how old its last_seen_at
// is, and returns before CountActiveUsers is even consulted.
func TestCheckSeatCapIgnoresStaleExistingUser(t *testing.T) {
	ctx := context.Background()
	s, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(s.Close)

	tenantName := "seatcap-stale-" + t.Name()
	tn, err := s.GetOrCreateTenantByName(ctx, tenantName)
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	// Seed a user whose last_seen_at is long outside the 7-day active window,
	// directly via the store (bypassing Resolve, which would refresh it).
	if _, err := s.UpsertUser(ctx, store.User{TenantID: tn.ID, Subject: "seat-stale", Email: "stale@x.io"}); err != nil {
		t.Fatalf("seed stale user: %v", err)
	}
	raw, err := sql.Open("pgx", testDSN)
	if err != nil {
		t.Fatalf("raw db: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.ExecContext(ctx, `UPDATE users SET last_seen_at = $1 WHERE tenant_id = $2 AND subject = $3`,
		time.Now().Add(-30*24*time.Hour), tn.ID, "seat-stale"); err != nil {
		t.Fatalf("backdate last_seen_at: %v", err)
	}

	r := NewResolver(s, tenantName)
	r.seatLimit = 1
	// The tenant currently has ZERO active users (the seeded one is stale),
	// so this would also succeed via the count path. The point of this test
	// is that it succeeds via the EXISTENCE path, never even reaching
	// CountActiveUsers, which TestCheckSeatCapNeverBlocksExistingUser's
	// "at cap" framing cannot show on its own.
	if _, err := r.Resolve(ctx, auth.Principal{Subject: "seat-stale", Email: "stale@x.io"}); err != nil {
		t.Fatalf("a stale-but-known subject must not be treated as a new signup: %v", err)
	}
}

// TestCheckSeatCapIgnoresSCIMProvisionedNeverAuthenticatedUsers is audit B9's
// end-to-end headline scenario, driven through the real checkSeatCap ->
// CountActiveUsers path: "an IdP provisioning 10 users exhausts the free
// tier with zero logins." seatLimit rows are provisioned via
// store.UpsertProvisionedUser directly, standing in for a SCIM
// implementation's own POST /scim/v2/Users calls (internal/api's
// TestSCIMCreatedUserDoesNotConsumeASeatUntilFirstLogin proves that handler
// calls the same store method), and NONE of them ever call Resolve. A
// genuinely new human subject arriving after the tenant is "full" of
// provisioned-only rows must still be ADMITTED.
func TestCheckSeatCapIgnoresSCIMProvisionedNeverAuthenticatedUsers(t *testing.T) {
	ctx := context.Background()
	s, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(s.Close)

	tenantName := "seatcap-scim-provisioned-" + t.Name()
	tn, err := s.GetOrCreateTenantByName(ctx, tenantName)
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}

	const seatLimit = 3
	for i := 0; i < seatLimit; i++ {
		if _, err := s.UpsertProvisionedUser(ctx, store.User{
			TenantID: tn.ID, Subject: fmt.Sprintf("scim-provisioned-%d", i), DisplayName: fmt.Sprintf("Provisioned %d", i),
		}); err != nil {
			t.Fatalf("provision seat %d: %v", i, err)
		}
	}

	r := NewResolver(s, tenantName)
	r.seatLimit = seatLimit

	// The decisive assertion: a brand-new HUMAN subject, arriving into a
	// tenant that already holds seatLimit provisioned-but-never-authenticated
	// rows, must be admitted -- a bug reproducing B9 would reject this with
	// SeatLimitError, exactly the "exhausts the free tier with zero logins"
	// failure the finding describes.
	rc, err := r.Resolve(ctx, auth.Principal{Subject: "first-real-human-login", Email: "human@x.io"})
	if err != nil {
		t.Fatalf("a real human subject was rejected even though every existing seat is a SCIM-provisioned row "+
			"that never authenticated: %v", err)
	}
	if rc.UserID == "" {
		t.Fatal("Resolve returned an empty UserID on success")
	}
}

// TestMiddlewareRespondsWithSeatLimitBody proves Middleware turns a
// SeatLimitError into a 402 with the same envelope shape internal/api's
// writeLimitReached uses (writeSeatLimitReached, seatcap.go), and that the
// wrapped handler never runs.
func TestMiddlewareRespondsWithSeatLimitBody(t *testing.T) {
	ctx := context.Background()
	s, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(s.Close)

	r := NewResolver(s, "seatcap-mw-"+t.Name())
	r.seatLimit = 1
	if _, err := r.Resolve(ctx, auth.Principal{Subject: "seat-mw-1", Email: "m1@x.io"}); err != nil {
		t.Fatalf("seed the one seat: %v", err)
	}

	called := false
	h := r.Middleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { called = true }))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{Subject: "seat-mw-2", Email: "m2@x.io"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if called {
		t.Fatal("wrapped handler must not run when the seat cap rejects the request")
	}
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402, body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Limit struct {
			Resource string `json:"resource"`
			Max      int    `json:"max"`
			Current  int    `json:"current"`
			Contact  string `json:"contact"`
		} `json:"limit"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Limit.Resource != "seats" || body.Limit.Max != 1 || body.Limit.Current != 1 {
		t.Fatalf("limit body = %+v", body.Limit)
	}
	if body.Limit.Contact != DefaultContactEmail {
		t.Fatalf("contact = %q, want %q", body.Limit.Contact, DefaultContactEmail)
	}
	if body.Error.Message == "" {
		t.Fatal("error.message must be non-empty")
	}
}

// TestNewResolverContactEmailDefaultAndOverride pins NewResolver's default
// contactEmail (DefaultContactEmail) and SetContactEmail's empty-ignore
// contract (mirrors internal/api's TestSetContactEmailIgnoresEmpty).
// Edition-agnostic: the contact address is the same in both builds.
//
// It was TestNewResolverDefaultsToUnlimited, which also asserted seatLimit ==
// 0. That half is edition-specific (a generated Community tree defaults to
// 10) and moved to seatlimit.ee_test.go; the wiring statement that survives
// generation is TestNewResolverWiresSeatLimitFromTheEditionExtensionPoint
// above.
func TestNewResolverContactEmailDefaultAndOverride(t *testing.T) {
	r := NewResolver(nil, "x")
	if r.contactEmail != DefaultContactEmail {
		t.Fatalf("default contactEmail = %q, want %q", r.contactEmail, DefaultContactEmail)
	}
	r.SetContactEmail("")
	if r.contactEmail != DefaultContactEmail {
		t.Fatal("SetContactEmail(\"\") must not blank the default")
	}
	r.SetContactEmail("ops@example.com")
	if r.contactEmail != "ops@example.com" {
		t.Fatalf("SetContactEmail did not override: %q", r.contactEmail)
	}
}
