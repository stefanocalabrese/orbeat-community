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

// findingsDigestMismatchError is a well-formed acknowledgment whose digest
// does not match the artifact's CURRENT scan_findings_digest
// (handleAcknowledgeFindings, admin_artifact_review.ee.go). It plays the role
// versionMismatchError plays for a stale If-Match, a value that is
// well-formed but no longer matches the row, for a value that travels in
// the request body instead of a header, since that endpoint deliberately
// uses the digest as its precondition rather than If-Match (see that
// handler's own comment for why). Maps to 412 in fail() below: the request
// itself is fine, but what it names has moved on.
//
// Declared HERE, in a SHARED (non-.ee.go) file, even though its only
// constructor lives in an Enterprise-only one: internal/communitygen's
// Enterprise-symbol boundary scan flags any shared file spelling an
// identifier declared ONLY inside an .ee.go file (see dcrRegisterFunc's doc
// comment, api.go, for the fullest account of this class), and fail() below,
// itself shared since every handler in both editions calls it, has to
// name this type to map it to a status code. Declaring it here instead of
// alongside handleAcknowledgeFindings is the fix, not a workaround: nothing
// about the TYPE is Enterprise-specific, only the ONE HANDLER that ever
// constructs a value of it is, and a generated Community tree compiling an
// unconstructed error type is no different from any other dead code path.
type findingsDigestMismatchError struct{ msg string }

func (e findingsDigestMismatchError) Error() string { return e.msg }

// authorFindingsAckRequiredCode and approverFindingsAckRequiredCode are the
// machine-readable body field handleApproveArtifact's findingsAckRequiredError
// carries (respond.go's writeFindingsAckRequired), mirroring
// idpAssertionRequiredCode's role for the role-rename slice (admin_roles.go):
// both refusals map to the SAME 409, so the status alone cannot tell a
// client which UI to render: a banner asking the author to go acknowledge,
// versus the approver's own unticked checkbox. Also reused as the deny
// audit event's "reason" (admin_artifact_review.ee.go), so the code a client
// reads and the reason an operator queries are the same string.
const (
	authorFindingsAckRequiredCode   = "author_findings_ack_required"
	approverFindingsAckRequiredCode = "approver_findings_ack_required"
)

// findingsAckRequiredError is a refusal from handleApproveArtifact when the
// artifact carries scan findings that capo's 2026-08-27 decision (mandatory
// acknowledgment of scan findings) requires acknowledged before approval may
// proceed: either the AUTHOR has not acknowledged the artifact's CURRENT
// scan_findings_digest, or this approve request itself does not carry the
// APPROVER's own acknowledgment of it. Code distinguishes which.
//
// Maps to 409 in fail() below, not 412: unlike findingsDigestMismatchError
// (a well-formed value that has moved on since it was read) this is a gate
// that was never satisfied in the first place: the artifact's workflow
// state genuinely does not permit approval yet, the same category as the
// plain "artifact is not pending review" conflictError elsewhere in this
// file, just with a machine-readable code attached because two DIFFERENT
// conditions share that one status.
//
// Declared HERE, in a SHARED (non-.ee.go) file, even though its only
// constructor lives in admin_artifact_review.ee.go (Enterprise-only), for
// the identical reason findingsDigestMismatchError above is: fail() is
// shared and must name this type to map it to a status code, and
// internal/communitygen's boundary scan flags any shared file spelling an
// identifier declared ONLY inside an .ee.go file.
type findingsAckRequiredError struct {
	code string
	msg  string
}

func (e findingsAckRequiredError) Error() string { return e.msg }

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
