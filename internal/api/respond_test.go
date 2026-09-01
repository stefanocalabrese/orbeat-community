package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

func TestFailMapsConstraintViolations(t *testing.T) {
	cases := []struct {
		code string
		want int
	}{
		{"23505", http.StatusConflict},            // unique_violation
		{"23503", http.StatusBadRequest},          // foreign_key_violation
		{"23502", http.StatusInternalServerError}, // not-null: unmapped → 500
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		fail(rec, &pgconn.PgError{Code: c.code})
		if rec.Code != c.want {
			t.Fatalf("code %s => %d, want %d", c.code, rec.Code, c.want)
		}
	}
}

// TestFailSeparatesTheApprovedIdentityDuplicate pins fail()'s floor arm for a
// 23505 against store.ApprovedIdentityUniqueIndex, and the control that keeps
// it a SPLIT rather than a rename of the generic one.
//
// Both cases are 409, so the status can prove nothing here; the sentences are
// the whole assertion. And the two must stay different: "already exists" is
// true of a live-namespace duplicate and false of this one, where the name IS
// free everywhere the admin can see it.
//
// This lives in an ordinary _test.go, unlike the handler gates, because the
// arm itself is shared code. respond.go compiles into a generated Community
// tree, where a Community admin can reach SetArtifactApproved through
// auto-approve with no handler arm in front of it.
func TestFailSeparatesTheApprovedIdentityDuplicate(t *testing.T) {
	body := func(err error) (int, string) {
		rec := httptest.NewRecorder()
		fail(rec, err)
		var b struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if e := json.Unmarshal(rec.Body.Bytes(), &b); e != nil {
			t.Fatalf("decode %q: %v", rec.Body.String(), e)
		}
		return rec.Code, b.Error.Message
	}

	code, msg := body(&pgconn.PgError{Code: "23505", ConstraintName: store.ApprovedIdentityUniqueIndex})
	if code != http.StatusConflict {
		t.Fatalf("approved-identity duplicate = %d, want 409", code)
	}
	if msg != approvedIdentityTaken {
		t.Fatalf("approved-identity duplicate says %q, want %q", msg, approvedIdentityTaken)
	}

	// The control. A duplicate on any other constraint (the live
	// UNIQUE (tenant_id, type, name) from 00003, a role name, a server name)
	// must keep the generic sentence.
	if code, msg = body(&pgconn.PgError{Code: "23505", ConstraintName: "artifact_tenant_id_type_name_key"}); code != http.StatusConflict || msg != "already exists" {
		t.Fatalf("a duplicate on another constraint = %d %q, want 409 \"already exists\"", code, msg)
	}

	// A handler that knows which pair collided wins over the floor: fail()
	// matches conflictError before it ever reaches the pg arms, which is what
	// lets denyApprovedIdentityConflict name the holder.
	if code, msg = body(conflictError{"artifact \"bar\" already distributes as skill/foo"}); code != http.StatusConflict || msg != "artifact \"bar\" already distributes as skill/foo" {
		t.Fatalf("conflictError = %d %q, want the handler's own sentence", code, msg)
	}
}

// TestFailMapsPreconditionErrors pins that fail()'s two new arms
// (preconditionRequiredError, versionMismatchError) — plus the pre-existing
// store.ErrVersionMismatch sentinel — land on the right status, and that
// their addition did not disturb conflictError/validationError (controls).
// Ordering in fail()'s switch matters (see the -overlay red-proof recorded
// in the Task 4 report): this test is what would catch a regression there.
func TestFailMapsPreconditionErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"preconditionRequiredError", preconditionRequiredError{"missing If-Match"}, http.StatusPreconditionRequired},
		{"versionMismatchError", versionMismatchError{"stale version"}, http.StatusPreconditionFailed},
		{"store.ErrVersionMismatch", store.ErrVersionMismatch, http.StatusPreconditionFailed},
		{"conflictError (control)", conflictError{"wrong state"}, http.StatusConflict},
		{"validationError (control)", validationError{"bad input"}, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			fail(rec, c.err)
			if rec.Code != c.want {
				t.Fatalf("fail(%v) => %d, want %d", c.err, rec.Code, c.want)
			}
		})
	}
}

// TestFailMapsLimitError pins fail()'s dispatch of limitError (docs/specs/
// 2026-08-19-orbeat-community-caps-design.md §5) to a 402 with the
// structured body writeLimitReached builds.
func TestFailMapsLimitError(t *testing.T) {
	rec := httptest.NewRecorder()
	fail(rec, limitError{Resource: "roles", Max: 1, Current: 1, Contact: "ops@example.com"})
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", rec.Code)
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
	if body.Limit.Resource != "roles" || body.Limit.Max != 1 || body.Limit.Current != 1 || body.Limit.Contact != "ops@example.com" {
		t.Fatalf("body = %+v", body)
	}
	if body.Error.Message == "" {
		t.Fatal("error.message must be non-empty")
	}
}
