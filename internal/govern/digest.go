package govern

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

// Digest returns a stable fingerprint of a set of scan findings. It exists to
// bind an acknowledgment to the EXACT findings a person read
// (docs/plans/orbeat-scan-acknowledgment-2026-08-27.md): an author or an
// approver acknowledges a digest, and handleApproveArtifact
// (internal/api/admin_artifact_review.ee.go) refuses to approve unless the
// acknowledged digest still equals the artifact's current one.
//
// IT HASHES THE FINDINGS, NOT THE CONTENT, and a re-scan of DIFFERENT content
// routinely produces the SAME digest. Every deterministic finding message in
// scanner.go interpolates only the word "content" or "seed", never the bytes
// that matched, so two materially different payloads that trip the same rule
// are byte-identical AS FINDINGS SETS and digest identically. Measured: a
// 68-byte artifact carrying one AWS key and a 386-byte artifact carrying a
// different AWS key both scan to the single finding {secret, "possible AWS
// access key ID in content", block}, and both digest to
// d272a215cbdb4d9baf682b13c58388eada34207d54b6d2fe542c4d28ccbf9c9f. An edit
// that fixes the problem outright digests to "", not to a fresh hash. The
// optional advisory LLM layer does write content-derived free text into its
// messages, but it is off by default, Enterprise-only and built solely in
// cmd/api, so nothing may depend on it moving the digest.
//
// SO A RE-SCAN DOES NOT INVALIDATE A STALE ACKNOWLEDGMENT. THE CLEAR DOES.
// store.SetArtifactSubmitted (internal/store/artifact.go) NULLs
// findings_ack_digest, findings_ack_by and findings_ack_at in the same UPDATE
// that writes the new digest, so every submission starts from "nobody has
// acknowledged anything". That clear is LOAD-BEARING, not redundant tidying.
// Delete it and an author who acknowledged one AWS key, withdrew, swapped in
// a different one and resubmitted would find the old acknowledgment still
// equal to the new digest and still satisfying approval's precondition: an
// acknowledgment of findings on content nobody re-read, which is exactly the
// separation-of-duties hole the acknowledgment gate exists to close. Nothing
// this function does would catch it.
//
// NOT a security boundary. This is a change-detection fingerprint, not an
// authentication or integrity control: nothing stops a client from computing
// the correct digest for a findings set it never actually displayed to a
// human being, and comparing digests only proves "this matches the
// artifact's CURRENT findings", never "a human actually read them". Do not
// describe this function, or code that compares its output, as tamper-proof
// or as resisting an adversary: its job is to detect an ordinary re-scan,
// not to defend against one.
//
// Order does NOT affect the result. The LLM half of CompositeScanner
// (internal/govern/llm_scanner.ee.go) gives no guarantee that repeated scans
// of identical content return findings in the same order, so an
// order-sensitive digest would spuriously invalidate an acknowledgment on a
// re-scan that reproduced the exact same set. Treating two orderings of the
// same set as the same acknowledgment is also the more defensible reading of
// what "read" means here: a reviewer who saw the same N findings in a
// different sequence saw the same thing.
//
// Duplicates DO affect the result: this is a multiset fingerprint, not a
// set fingerprint. The LLM layer can genuinely emit the identical
// {rule,message,severity} triple more than once in a single scan
// (parseLLMFindings in llm_scanner.ee.go does not dedupe its output, and
// CompositeScanner just concatenates every scanner's findings), so a set of
// two identical findings must digest differently from a set of one; silently
// collapsing them would let a re-scan that dropped a repeated finding still
// pass as "the same set the reader acknowledged".
//
// The empty set is a defined special case: Digest(nil) and Digest of a
// zero-length slice both return "", never a hash. A real digest is always
// exactly 64 lowercase hex characters (a sha256 sum), so "" can never
// collide with one, which is what lets a caller treat "" as an unambiguous
// "no findings, no acknowledgment needed" rather than as some particular
// empty state's hash.
//
// The result is lowercase hex: case-stable (a hex digit carries the same
// value whether written upper or lower case, unlike e.g. base64 where case
// changes the decoded value) and URL-safe, matching how it is used: stored
// in Postgres and echoed back verbatim by a client in a request body.
func Digest(findings []Finding) string {
	if len(findings) == 0 {
		return ""
	}

	// Each finding is rendered to its own canonical JSON encoding before
	// combining. json.Marshal on Finding's three plain string fields is
	// deterministic (struct fields marshal in declaration order, always
	// Rule/Message/Severity) and self-delimiting: any characters a
	// rule/message/severity string contains, including a literal quote,
	// backslash, or newline, are escaped INSIDE the JSON string by the
	// encoder. That is what rules out the kind of ambiguity a naive
	// delimiter-joined encoding would risk, e.g. {Rule:"a", Message:"b|c"}
	// colliding with {Rule:"a|b", Message:"c"} under a "|"-joined scheme.
	canon := make([]string, len(findings))
	for i, f := range findings {
		b, _ := json.Marshal(f) // three string fields: cannot fail
		canon[i] = string(b)
	}

	// Sorting makes the combination order-insensitive while preserving
	// duplicates: two identical findings produce two identical canonical
	// strings, both of which survive the sort and both of which are hashed,
	// so the multiset's cardinality still affects the result.
	sort.Strings(canon)

	// "\n" is a safe separator: json.Marshal escapes any raw newline inside a
	// string field as the two-byte sequence \n, so no canon[i] can contain a
	// literal newline byte, and the join can never be mis-segmented.
	sum := sha256.Sum256([]byte(strings.Join(canon, "\n")))
	return hex.EncodeToString(sum[:])
}
