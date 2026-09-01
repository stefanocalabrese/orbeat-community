package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
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
// base64url(JSON ["<sort>","<key1>",…,"<keyN>","<id>"]).
//
// Deliberately NOT the audit endpoint's base64url("<a>:<b>"). That encoding is
// ambiguous the moment a sort key can contain a colon, and these sort keys are
// user-controlled strings: role names are validated only as non-empty, so a role
// named "a:b" yields a cursor that decodes to the wrong position. A JSON array
// is immune to whatever bytes the key contains.
//
// The leading "<sort>" element is store.ListCursor.Sort (see its doc comment
// in internal/store/paging.go for why it exists). Prepending it, rather than
// appending, means a cursor minted BEFORE this element existed decodes to a
// JSON array one element SHORTER than any current shape expects, decodeListCursor's
// existing length check rejects it outright rather than misreading its last
// element as a sort identity or its first key as something else.
func encodeListCursor(c store.ListCursor) string {
	parts := make([]string, 0, len(c.Keys)+2)
	parts = append(parts, c.Sort)
	parts = append(parts, c.Keys...)
	parts = append(parts, c.ID)
	// A []string always marshals; the error is structurally unreachable.
	b, _ := json.Marshal(parts)
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeListCursor parses an opaque cursor, returning a validationError (→ 400)
// for any malformed input.
//
// A cursor encoded before the sort-identity element existed is one element
// shorter than len(shape)+2 and is rejected right here, by the length check
// below, before its Sort ever reaches a comparison, it can never be
// mistaken for a same-length cursor that just happens to carry an empty
// Sort string (store.ErrCursorSortMismatch, checked downstream in
// keysetTail, is what refuses THAT case instead).
func decodeListCursor(s string, shape cursorShape) (*store.ListCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, validationError{"invalid cursor"}
	}
	var parts []string
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, validationError{"invalid cursor"}
	}
	if len(parts) != len(shape)+2 {
		return nil, validationError{"invalid cursor"}
	}
	sortID := parts[0]
	keys := parts[1 : len(parts)-1]
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
	return &store.ListCursor{Keys: keys, ID: id, Sort: sortID}, nil
}

// The sort allowlist (docs/plans/orbeat-admin-search-sort-2026-08-27.md Task 2):
// one query-string value per admin list, defined here in ONE place rather
// than scattered per handler, so a future addition (or removal) has a single
// site to change and nowhere else to forget.
//
// Every value below is chosen because it is ALREADY that list's production
// default order (internal/store's roleKeys/mcpServerKeys/entitlementKeys/
// artifactEntitlementKeys/artifactKeys/virtualKeyKeys) and is confirmed
// index-backed (internal/store/explain_test.go's TestPaginatedListsUseTheirIndexes,
// plus its virtual_key sibling this same task adds, see migration 00029).
// This is deliberately NOT "the first of several planned columns": every
// other column examined for these six tables (artifact by name alone,
// mcp_server/role by status, entitlement/artifact_entitlement by a joined
// server or artifact name) needs an index that does not exist today, and
// Task 2's rule is "servable by an existing index, or refused", so adding
// one is coupled to shipping that index first, in its own reviewed migration,
// not a client-side request. What ?sort DOES add today, safely, is
// ?order=desc: every list's comparison direction was previously hardcoded
// ascending in the SQL and is now client-controlled.
const (
	roleSortName                = "name"
	mcpServerSortName           = "name"
	entitlementSortName         = "role_id"
	artifactEntitlementSortName = "role_id"
	artifactSortName            = "type"
	virtualKeySortName          = "name"
)

// sortOrderParams parses ?sort and ?order for a list endpoint that offers
// exactly one sort column today (want, see the allowlist above). An absent
// ?sort defaults to want; any other value is a validationError (→ 400), never
// a silent fallback: showing the user a table sorted one way while its column
// header (or a ?sort the client just sent) claims another is worse than an
// error. ?order accepts "asc" (default, absent) or "desc"; anything else is
// also a validationError.
//
// This is intentionally a plain string-equality check, not a lookup into a
// map of many options: with exactly one allowed value per list, a map would
// have one entry and still forbid everything else, which equality already
// does, and does more simply. It generalizes without restructuring the
// moment a second value is added, see the allowlist comment above.
func sortOrderParams(r *http.Request, want string) (desc bool, err error) {
	if q := r.URL.Query().Get("sort"); q != "" && q != want {
		return false, validationError{fmt.Sprintf("sort must be %q", want)}
	}
	switch o := r.URL.Query().Get("order"); o {
	case "", "asc":
		return false, nil
	case "desc":
		return true, nil
	default:
		return false, validationError{`order must be "asc" or "desc"`}
	}
}

// searchParam returns the ?q= substring search term for a list endpoint that
// supports search (docs/plans/orbeat-admin-search-sort-2026-08-27.md Task 4):
// "" for both an absent and an explicitly-empty ?q=, which is deliberate:
// they mean the same thing to a user (a cleared search box shows everything)
// and to store.likeSearchArg (paging.go, internal/store), which treats ""
// as "no filter" for exactly that reason. The raw, un-escaped term is
// returned; wildcard escaping happens once, at the SQL-building boundary
// (store.escapeLikeSpecials), not here: this function's only job is
// reading the query string.
func searchParam(r *http.Request) string {
	return r.URL.Query().Get("q")
}

// refuseSearch rejects ?q= with 400 for a list with no natural text column to
// search: entitlement and artifact_entitlement (Decision 1,
// docs/plans/orbeat-admin-search-sort-2026-08-27.md Task 4) sort, and are
// keyed, for cursor purposes, on role_id, a uuid, with no name or other free
// text of their own a substring match could compare against. The alternative
// was joining to role.name so search would have something to match, but that
// drags a JOIN into a keyset query that today has none on either list (see
// entitlementKeys' doc comment, internal/store/rbac.go, for the full
// reasoning); refusing is louder and smaller.
//
// r.URL.Query().Has("q"), not Get("q") != "": Has reports the PARAMETER'S
// PRESENCE regardless of its value, so a bare "?q=" (empty value) is refused
// exactly like "?q=foo": a client that thinks it is filtering, even by an
// empty string, must be told this list cannot, rather than silently getting
// the unfiltered page back and concluding search "worked" and just happened
// to return everything. That silent success is precisely the failure mode
// the plan calls out: "a search box that appears to work and filters nothing
// is worse than one that says it cannot."
func refuseSearch(r *http.Request) error {
	if r.URL.Query().Has("q") {
		return validationError{"q is not supported on this list: role_id has no natural text column to search"}
	}
	return nil
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
