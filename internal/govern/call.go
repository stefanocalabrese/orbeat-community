package govern

import (
	"context"
	"fmt"
)

// Direction values for CallPayload, naming which side of a gateway tools/call
// round trip the content came from.
const (
	DirectionArguments = "arguments"
	DirectionResult    = "result"
)

// CallPayload is the scannable surface of one side of a runtime-intercepted
// MCP tool call: the arguments an agent sends (Direction ==
// DirectionArguments) or the content an upstream tool returns
// (Direction == DirectionResult). Tool is the namespaced tool name (e.g.
// "srv__read_file"), carried for the finding message and for callers that
// want it for logging/audit; ScanCall itself never inspects it beyond that.
type CallPayload struct {
	Tool      string
	Direction string
	Content   string
}

// ScanCall runs an EXISTING govern.Scanner against a tool call payload. It
// does not carry its own detection rules: CallPayload.Content is funneled
// through ArtifactPayload.Content, the exact field scanner.go's
// scanSecrets/scanReserved/scanRemoteExec/size-check already inspect, so a
// call is scanned by literally the same rule bodies an artifact is -- not a
// second copy of them. See docs/specs/2026-08-25-orbeat-runtime-
// interception-design.md §2 for why this reuse, rather than a parallel rule
// set, is the point of this subsystem.
//
// The same rule BODIES, never the same scanner INSTANCE artifact submission
// gets. The only production callers are interceptArguments and
// interceptResult in internal/gateway/intercept.go, and cmd/gateway builds
// what they receive as interceptorFor(cfg, govern.NewDefaultScanner()), which
// yields that plain rule scanner or nil; cmd/gateway reads none of the
// ORBEAT_SCAN_LLM_* settings. The CompositeScanner that adds the optional
// advisory LLM layer is assembled only by cmd/api's buildScanner, in a
// different process, so an intercepted call is NEVER scanned by it however
// that process is configured. The parameter stays an interface and this
// function assumes nothing beyond it, which is what lets call_test.go drive
// it with a fake.
//
// DECIDED (the spec named only the secret rules explicitly): EVERY other rule
// family defaultScanner runs fires on call content too, unchanged, as a direct
// consequence of reusing Scan(ArtifactPayload) rather than the lower-level
// scanSecrets alone. With scanSecrets that is all four families in
// defaultScanner.Scan, and this list has to move whenever that function does.
// Only the content halves fire: ScanCall leaves ArtifactPayload.MemorySeed
// empty, so each rule's "seed" arm is unreachable from here.
//   - scanReserved: KEPT. A tool call carrying a literal
//     "<!-- ORBEAT-SEED:BEGIN -->"/"<!-- ORBEAT-RULES:BEGIN -->" sentinel is
//     exactly the kind of input that could plant an orphan managed-block
//     marker into a file an upstream write-capable tool touches -- the same
//     malformed-marker splice class internal/syncclient was hardened against
//     for artifact content (v1.14.1, v1.17.0). Blocking it at the call
//     boundary is defense in depth, not a false trigger.
//   - scanRemoteExec: KEPT, as the WARN it already is for artifacts (it never
//     blocks anywhere). A "curl ... | bash" sitting in tool arguments is an
//     agent one hop from running a script nobody reviewed, which is the same
//     threat the rule was written for, closer to execution. Its accepted
//     false positives are cheaper here than on the submit path: a warn on a
//     call costs one gateway.call.flagged audit row, not an approver's
//     attention.
//   - size: KEPT, as a WARN only (never blocks -- HasBlocking is unaffected).
//     The threshold is WarnContentBytes (49152, three quarters of
//     MaxContentBytes) and the message reads "content is approaching the
//     64KiB limit". Both are artifact vocabulary: an artifact really is
//     nearing a 64KiB ceiling validateArtifact enforces with a 400, while a
//     tool call has no ceiling at all, so on a call the sentence names a
//     limit that does not exist and the threshold is arbitrary. Kept anyway
//     because it is harmless, audited-not-blocking operator signal about an
//     unusually large call, and splitting Scan to withhold it would require
//     forking the rule bodies this file exists to avoid duplicating.
//
// All three are pinned by call_test.go rather than left as unexercised
// assumptions.
//
// Every returned Finding's Message is suffixed with the call's tool and
// direction (e.g. "(tool srv__read_file, arguments)") regardless of which
// scanner produced it -- the underlying rule/LLM message text may say
// "content" or "seed" (ArtifactPayload's own vocabulary) or nothing
// call-shaped at all (an LLM judge's free-text finding), so appending is the
// only way to GUARANTEE an operator can tell which side of the call leaked
// without parsing scanner-internal wording. This is call.go's only piece of
// logic; it does not alter Rule or Severity.
func ScanCall(ctx context.Context, s Scanner, p CallPayload) ([]Finding, error) {
	findings, err := s.Scan(ctx, ArtifactPayload{Content: p.Content})
	if err != nil {
		return nil, err
	}
	out := make([]Finding, len(findings))
	for i, f := range findings {
		out[i] = Finding{
			Rule:     f.Rule,
			Severity: f.Severity,
			Message:  fmt.Sprintf("%s (tool %s, %s)", f.Message, p.Tool, p.Direction),
		}
	}
	return out, nil
}
