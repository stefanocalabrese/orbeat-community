package authz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// activeSeatWindow is the Community seat cap's active-user window (spec
// §3.2): a subject counts toward the cap only if it authenticated within the
// last 7 days, so a seat self-heals when someone stops using orbeat.
const activeSeatWindow = 7 * 24 * time.Hour

// SeatLimitError is returned by Resolve when admitting a brand-new subject
// would exceed the edition's active-seat cap (spec §4). Exported so a
// caller, Middleware below and any other consumer of Resolve including
// cmd/gateway's session build, can distinguish "no seats left" from every
// other resolve failure without string-matching. It cannot be
// internal/api's limitError (respond.go): internal/authz cannot import
// internal/api.
type SeatLimitError struct {
	Max     int
	Current int
	Contact string
}

func (e SeatLimitError) Error() string {
	return fmt.Sprintf("community edition seat limit reached: %d of %d used", e.Current, e.Max)
}

// checkSeatCap enforces the Community seat cap (spec §3.2, §4) before
// UpsertUser (called immediately after, in Resolve) can insert a NEW user
// row. Skipped entirely when seatLimit is 0 (Enterprise, and NewResolver's
// default in this repo's own build): zero extra reads on the hot path.
//
// It is a hard requirement that an ALREADY-KNOWN subject is never rejected
// here, however stale its last_seen_at: this function checks existence
// FIRST (GetUserBySubject) and returns immediately the moment the subject is
// found, before CountActiveUsers even runs. The cap only ever gates the
// FIRST authentication of a subject this tenant has never seen: the one
// moment UpsertUser is actually about to insert a new row rather than
// refresh an existing one. Checking the count first and rejecting whenever
// it is at/over the cap, with no existence check, would reject every one of
// a tenant's already-seated users on their very next request too, once the
// tenant reached its cap. That is the exact failure mode docs/plans/orbeat-
// community-caps-2026-08-19.md Task 4 names directly: "an existing user
// inside the window must never be blocked by the cap, or the eleventh
// signup locks out the ten people already working."
func (r *Resolver) checkSeatCap(ctx context.Context, tenantID, subject string) error {
	if r.seatLimit <= 0 {
		return nil
	}
	_, err := r.store.GetUserBySubject(ctx, tenantID, subject)
	if err == nil {
		return nil // known subject; the upsert that follows refreshes it, never creates
	}
	if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("authz: check existing user: %w", err)
	}
	since := time.Now().Add(-activeSeatWindow)
	n, err := r.store.CountActiveUsers(ctx, tenantID, since)
	if err != nil {
		return fmt.Errorf("authz: count active users: %w", err)
	}
	if n >= r.seatLimit {
		return SeatLimitError{Max: r.seatLimit, Current: n, Contact: r.contactEmail}
	}
	return nil
}

// writeSeatLimitReached writes the same 402 envelope shape as internal/api's
// writeLimitReached (respond.go). Duplicated rather than shared, since
// internal/authz cannot import internal/api. Keep both in sync if either
// changes; TestMiddlewareRespondsWithSeatLimitBody pins the field names.
func writeSeatLimitReached(w http.ResponseWriter, e SeatLimitError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"message": e.Error()},
		"limit": map[string]any{
			"resource": "seats",
			"max":      e.Max,
			"current":  e.Current,
			"contact":  e.Contact,
		},
	})
}
