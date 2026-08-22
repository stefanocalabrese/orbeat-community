package api

import (
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestIfMatch pins ifMatch's table exactly as designed in
// docs/specs/2026-08-11-orbeat-optimistic-concurrency-design.md §5. Every case
// must land in exactly one of three buckets: preconditionRequiredError (428),
// validationError (400), or a parsed version with no error.
func TestIfMatch(t *testing.T) {
	cases := []struct {
		name         string
		setHeader    bool
		header       string
		wantVersion  int64
		precondition bool
		validation   bool
	}{
		{name: "absent entirely", setHeader: false, precondition: true},
		{name: "present, empty", setHeader: true, header: "", precondition: true},
		{name: "wildcard", setHeader: true, header: "*", precondition: true},
		{name: `quoted "7"`, setHeader: true, header: `"7"`, wantVersion: 7},
		{name: "quoted large (beyond float64 precision)", setHeader: true, header: `"9007199254740993"`, wantVersion: 9007199254740993},
		{name: "unquoted 7", setHeader: true, header: `7`, validation: true},
		{name: "weak validator", setHeader: true, header: `W/"7"`, validation: true},
		{name: "non-numeric", setHeader: true, header: `"abc"`, validation: true},
		{name: "negative", setHeader: true, header: `"-1"`, validation: true},
		{name: "zero", setHeader: true, header: `"0"`, validation: true},
		{name: "multiple entity-tags", setHeader: true, header: `"1", "2"`, validation: true},
		{name: "overflows int64", setHeader: true, header: `"99999999999999999999"`, validation: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPut, "/v1/admin/servers/x", nil)
			if c.setHeader {
				r.Header.Set("If-Match", c.header)
			}

			got, err := ifMatch(r)

			var pcErr preconditionRequiredError
			var vErr validationError

			switch {
			case c.precondition:
				if !errors.As(err, &pcErr) {
					t.Fatalf("ifMatch() error = %v (%T), want preconditionRequiredError", err, err)
				}
			case c.validation:
				if !errors.As(err, &vErr) {
					t.Fatalf("ifMatch() error = %v (%T), want validationError", err, err)
				}
			default:
				if err != nil {
					t.Fatalf("ifMatch() unexpected error: %v", err)
				}
				if got != c.wantVersion {
					t.Fatalf("ifMatch() = %d, want %d", got, c.wantVersion)
				}
			}
		})
	}
}

// TestETagRoundTrip pins etag() as ifMatch()'s exact inverse — the single
// invariant the whole optimistic-concurrency feature rests on: whatever
// etag() renders into an ETag response header, ifMatch() must parse back
// into the identical row_version once a client echoes it verbatim as
// If-Match. Covers the full int64 domain that matters: small values, the
// int32 boundary, beyond float64's 2^53 integer-precision limit (a client
// that round-tripped through JSON-as-number would corrupt this), and the
// int64 boundary itself.
func TestETagRoundTrip(t *testing.T) {
	for _, v := range []int64{1, 2, 9, 10, 99, 100, 1<<31 - 1, 1 << 31, 1<<53 + 1, math.MaxInt64 - 1, math.MaxInt64} {
		r := httptest.NewRequest(http.MethodPut, "/v1/admin/servers/x", nil)
		r.Header.Set("If-Match", etag(v))
		got, err := ifMatch(r)
		if err != nil {
			t.Fatalf("round-trip(%d): unexpected error: %v", v, err)
		}
		if got != v {
			t.Fatalf("round-trip(%d): ifMatch(etag(%d)) = %d", v, v, got)
		}
	}
}
