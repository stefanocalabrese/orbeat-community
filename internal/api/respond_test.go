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
