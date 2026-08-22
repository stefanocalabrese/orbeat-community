package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

const (
	// Lower than the audit endpoint's 100/1000: an admin row (especially an
	// artifact) is far larger than an audit event.
	defaultListLimit = 100
	// maxListLimit: internal/store cannot import internal/api (this package
	// imports store, so the reverse would cycle), so four store tests —
	// TestListEntitlementsPageUnboundedReturnsEverything,
	// TestListArtifactEntitlementsPageUnboundedReturnsEverything,
	// TestListMCPServersByTenantUnboundedReturnsEverything, and
	// TestListArtifactRevisionsStillReturnsEverything, all in
	// internal/store/paging_test.go — hard-code n=501 as "one past this
	// constant" instead of referencing it. Raising this requires also raising
	// n in all four.
	maxListLimit = 500
)

// cursorKind is the Postgres type a cursor key is cast to in the keyset
// predicate. Validating it HERE is what keeps a malformed cursor a 400: an
// unvalidated key reaches the cast and throws SQLSTATE 22P02, which fail()'s
// default case surfaces as a 500 (the same trap the audit cursor's uuid check
// closed).
type cursorKind int

const (
	cursorText cursorKind = iota
	cursorUUID
	cursorInt
)

// cursorShape is a list's cursor key kinds, in sort order (excluding the
// trailing id, which is always a uuid).
type cursorShape []cursorKind

// encodeListCursor serializes a keyset position as
// base64url(JSON ["<key1>",…,"<keyN>","<id>"]).
//
// Deliberately NOT the audit endpoint's base64url("<a>:<b>"). That encoding is
// ambiguous the moment a sort key can contain a colon, and these sort keys are
// user-controlled strings: role names are validated only as non-empty, so a role
// named "a:b" yields a cursor that decodes to the wrong position. A JSON array
// is immune to whatever bytes the key contains.
func encodeListCursor(c store.ListCursor) string {
	parts := make([]string, 0, len(c.Keys)+1)
	parts = append(parts, c.Keys...)
	parts = append(parts, c.ID)
	// A []string always marshals; the error is structurally unreachable.
	b, _ := json.Marshal(parts)
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeListCursor parses an opaque cursor, returning a validationError (→ 400)
// for any malformed input.
func decodeListCursor(s string, shape cursorShape) (*store.ListCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, validationError{"invalid cursor"}
	}
	var parts []string
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, validationError{"invalid cursor"}
	}
	if len(parts) != len(shape)+1 {
		return nil, validationError{"invalid cursor"}
	}
	keys := parts[:len(parts)-1]
	id := parts[len(parts)-1]
	if !uuidRe.MatchString(id) {
		return nil, validationError{"invalid cursor"}
	}
	for i, kind := range shape {
		switch kind {
		case cursorUUID:
			if !uuidRe.MatchString(keys[i]) {
				return nil, validationError{"invalid cursor"}
			}
		case cursorInt:
			// ParseInt sized to 32 bits, NOT strconv.Atoi: Atoi validates
			// against Go's int (64-bit on every release target), but every
			// int cursor key targets a Postgres `int` (int4) column —
			// '3000000000'::int raises SQLSTATE 22003
			// (numeric_value_out_of_range), which is in neither
			// idCastNotFound (22P02 only) nor fail()'s switch, so it would
			// surface as a 500. Verified on PG 16. Same trap as the NUL byte
			// below, one cursorKind over.
			if _, err := strconv.ParseInt(keys[i], 10, 32); err != nil {
				return nil, validationError{"invalid cursor"}
			}
		case cursorText:
			// NOT "any byte sequence": a NUL byte survives encoding/json and
			// reaches the $n::text placeholder, where Postgres raises SQLSTATE
			// 22021 ("invalid byte sequence for encoding UTF8"). 22021 is in
			// neither idCastNotFound (22P02 only) nor fail()'s switch, so it
			// would surface as a 500. Rejecting here is what keeps it a 400.
			// (Raw invalid UTF-8 is not reachable — encoding/json coerces it
			// to U+FFFD, which Postgres accepts; verified live.)
			if strings.ContainsRune(keys[i], 0) {
				return nil, validationError{"invalid cursor"}
			}
		default:
			// Fails CLOSED, not open: a cursorKind added to the const block
			// above and forgotten here must not silently skip validation —
			// that is exactly the class of defect this switch exists to
			// prevent. Structurally unreachable today (every defined kind has
			// its own case), which is exactly why this arm is the one that
			// keeps it that way for the next kind added.
			return nil, validationError{"invalid cursor"}
		}
	}
	return &store.ListCursor{Keys: keys, ID: id}, nil
}

// pageParams parses ?limit and ?cursor for a paginated list endpoint. An absent
// or empty limit uses def; a limit above max is CLAMPED (not an error); a
// non-integer or non-positive limit is a validationError. An absent cursor
// yields nil (first page).
func pageParams(r *http.Request, def, max int, shape cursorShape) (int, *store.ListCursor, error) {
	limit := def
	if q := r.URL.Query().Get("limit"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n <= 0 {
			return 0, nil, validationError{"limit must be a positive integer"}
		}
		limit = n
	}
	if limit > max {
		limit = max
	}
	var cursor *store.ListCursor
	if c := r.URL.Query().Get("cursor"); c != "" {
		parsed, err := decodeListCursor(c, shape)
		if err != nil {
			return 0, nil, err
		}
		cursor = parsed
	}
	return limit, cursor, nil
}
