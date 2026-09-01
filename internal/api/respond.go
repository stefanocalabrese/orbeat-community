// Package api implements orbeat's HTTP API surface.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": msg}})
}

// writeBlocked writes a 422 with the standard error envelope plus the scanner
// findings that caused the block, keeping one source of truth for the envelope.
func writeBlocked(w http.ResponseWriter, msg string, findings any) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"error":    map[string]string{"message": msg},
		"findings": findings,
	})
}

// writeLimitReached writes the structured 402 body (spec §5): the error
// envelope every other handler already uses, plus a "limit" object naming
// the resource, the max, the current count and the contact, everything a
// client needs without parsing e.Error()'s prose.
func writeLimitReached(w http.ResponseWriter, e limitError) {
	writeJSON(w, http.StatusPaymentRequired, map[string]any{
		"error": map[string]string{"message": e.Error()},
		"limit": map[string]any{
			"resource": e.Resource,
			"max":      e.Max,
			"current":  e.Current,
			"contact":  e.Contact,
		},
	})
}

// idpAssertionRequiredCode is the machine-readable body field the portal
// checks to switch into the "confirm you renamed this in the identity
// provider" checkbox flow (admin_roles.go's idpAssertionRequiredError,
// docs/plans/orbeat-role-rename-2026-08-27.md's decision: "the portal
// learns the mode from a 400, not a capability endpoint").
const idpAssertionRequiredCode = "idp_rename_assertion_required"

// writeIdpAssertionRequired writes idpAssertionRequiredError's 400 body: the
// standard error envelope plus the "code" field above, the same shape
// writeLimitReached/writeBlocked use for THEIR extra data.
func writeIdpAssertionRequired(w http.ResponseWriter, e idpAssertionRequiredError) {
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"error": map[string]string{"message": e.Error()},
		"code":  idpAssertionRequiredCode,
	})
}

// writeFindingsAckRequired writes a findingsAckRequiredError's 409 body: the
// standard error envelope plus the "code" field naming WHICH acknowledgment
// (author's or approver's) is missing or stale, the same shape
// writeIdpAssertionRequired uses for the role-rename slice's own 400.
func writeFindingsAckRequired(w http.ResponseWriter, e findingsAckRequiredError) {
	writeJSON(w, http.StatusConflict, map[string]any{
		"error": map[string]string{"message": e.Error()},
		"code":  e.code,
	})
}

// validationError is a client-input error that maps to HTTP 400. Handlers (or
// in-transaction closures) return it to reject a request with a clear message
// without leaking internals.
type validationError struct{ msg string }

func (e validationError) Error() string { return e.msg }

// forbiddenError maps to HTTP 403 (e.g. separation-of-duties: self-approval).
type forbiddenError struct{ msg string }

func (e forbiddenError) Error() string { return e.msg }

// conflictError maps to HTTP 409 (e.g. a transition from the wrong state).
type conflictError struct{ msg string }

func (e conflictError) Error() string { return e.msg }

// limitError maps to HTTP 402 (Community edition write-time cap reached,
// docs/specs/2026-08-19-orbeat-community-caps-design.md §5). Resource names
// which cap fired ("servers" | "roles"); Max/Current/Contact are exactly the
// fields the portal's 402 modal needs to render without parsing prose. 402
// is rare enough that some client libraries handle it poorly, so the
// response BODY carries everything a client needs, not just the status.
type limitError struct {
	Resource string
	Max      int
	Current  int
	Contact  string
}

func (e limitError) Error() string {
	return fmt.Sprintf("community edition limit reached: %s (%d of %d used)", e.Resource, e.Current, e.Max)
}

// fail maps a domain or database error to an HTTP response:
//   - preconditionRequiredError        → 428
//   - versionMismatchError             → 412
//   - findingsDigestMismatchError      → 412 (the acknowledge-findings endpoint's own precondition)
//   - findingsAckRequiredError         → 409, with the machine-readable author/approverFindingsAckRequiredCode
//   - idpAssertionRequiredError        → 400, with the machine-readable idpAssertionRequiredCode
//   - idpUnavailableError              → 502
//   - store.ErrNotFound                → 404
//   - store.ErrVersionMismatch         → 412
//   - store.ErrNameTaken               → 409
//   - store.ErrCursorSortMismatch      → 400 (a well-formed cursor minted under a different sort)
//   - unique_violation (pg 23505)      → 409
//   - foreign_key_violation (pg 23503) → 400
//   - everything else                  → 500 (internal details not leaked)
//
// The 23505 arm is split in two. A duplicate against
// store.ApprovedIdentityUniqueIndex is not the duplicate an admin can see:
// the row holding the contested identity is called something else in the
// admin list, so "already exists" sends them looking for a name that is not
// there. This arm is the FLOOR under that, not the whole answer. A handler
// that knows which pair collided returns a conflictError naming it, and
// conflictError is matched earlier in the switch, so the specific sentence
// wins wherever one exists and every other caller still gets a true one.
func fail(w http.ResponseWriter, err error) {
	var pgErr *pgconn.PgError
	var vErr validationError
	var fErr forbiddenError
	var cErr conflictError
	var lErr limitError
	var pcErr preconditionRequiredError
	var vmErr versionMismatchError
	var fdmErr findingsDigestMismatchError
	var farErr findingsAckRequiredError
	var iarErr idpAssertionRequiredError
	var iuErr idpUnavailableError
	switch {
	case errors.As(err, &vErr):
		writeError(w, http.StatusBadRequest, vErr.msg)
	case errors.As(err, &fErr):
		writeError(w, http.StatusForbidden, fErr.msg)
	case errors.As(err, &lErr):
		writeLimitReached(w, lErr)
	case errors.As(err, &pcErr):
		writeError(w, http.StatusPreconditionRequired, pcErr.msg)
	case errors.As(err, &vmErr):
		writeError(w, http.StatusPreconditionFailed, vmErr.msg)
	case errors.As(err, &fdmErr):
		writeError(w, http.StatusPreconditionFailed, fdmErr.msg)
	case errors.As(err, &farErr):
		writeFindingsAckRequired(w, farErr)
	case errors.As(err, &iarErr):
		writeIdpAssertionRequired(w, iarErr)
	case errors.As(err, &iuErr):
		writeError(w, http.StatusBadGateway, iuErr.msg)
	case errors.As(err, &cErr):
		writeError(w, http.StatusConflict, cErr.msg)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrVersionMismatch):
		writeError(w, http.StatusPreconditionFailed,
			"the resource changed since you loaded it; reload and reapply your change")
	case errors.Is(err, store.ErrNameTaken):
		writeError(w, http.StatusConflict, "a role in this tenant already has this name")
	case errors.Is(err, store.ErrCursorSortMismatch):
		writeError(w, http.StatusBadRequest, "cursor does not match the requested sort")
	case errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == store.ApprovedIdentityUniqueIndex:
		writeError(w, http.StatusConflict, approvedIdentityTaken)
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		writeError(w, http.StatusConflict, "already exists")
	case errors.As(err, &pgErr) && pgErr.Code == "23503":
		writeError(w, http.StatusBadRequest, "references a nonexistent resource")
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}
