package api

import (
	"net/http"
	"strconv"
	"strings"
)

// preconditionRequiredError is a missing (or deliberately refused) If-Match.
// Distinct from validationError because it maps to 428, not 400: RFC 6585 §3's
// stated motivation is verbatim this lost-update case, and unlike a bare 400 a
// 428 body is expected to explain how to resubmit.
type preconditionRequiredError struct{ msg string }

func (e preconditionRequiredError) Error() string { return e.msg }

// versionMismatchError is a well-formed If-Match that does not match the row.
type versionMismatchError struct{ msg string }

func (e versionMismatchError) Error() string { return e.msg }

// ifMatch parses a REQUIRED If-Match header into the row_version the caller
// expects.
//
// `*` is refused with 428 rather than honoured. RFC 9110 §13.1.1 defines it as
// "any current representation" — an explicit unconditional overwrite, which is
// precisely what this guard removes. Refusing it with 400 would be a
// conformance bug (it is well-formed); 428 is the honest status, because what
// we require is a SPECIFIC precondition. Policy, not a parser limitation.
//
// Only a strong, single entity-tag is accepted. A weak validator (W/"7") never
// matches under the strong comparison RFC 9110 §13.1.1 mandates for If-Match,
// so accepting one would guarantee a 412 forever — better to reject it as
// malformed than to fail mysteriously later.
func ifMatch(r *http.Request) (int64, error) {
	raw := strings.TrimSpace(r.Header.Get("If-Match"))
	if raw == "" {
		return 0, preconditionRequiredError{
			"If-Match is required on this request; GET the resource and resubmit with its ETag"}
	}
	if raw == "*" {
		return 0, preconditionRequiredError{
			`If-Match: * is not accepted; supply the resource's specific ETag`}
	}
	if len(raw) < 3 || raw[0] != '"' || raw[len(raw)-1] != '"' ||
		strings.ContainsAny(raw[1:len(raw)-1], `",`) {
		return 0, validationError{"If-Match must be a single strong entity-tag"}
	}
	n, err := strconv.ParseInt(raw[1:len(raw)-1], 10, 64)
	if err != nil || n < 1 {
		return 0, validationError{"If-Match is not a valid entity-tag for this resource"}
	}
	return n, nil
}

// etag renders a row_version as a strong entity-tag.
func etag(rowVersion int64) string { return `"` + strconv.FormatInt(rowVersion, 10) + `"` }
