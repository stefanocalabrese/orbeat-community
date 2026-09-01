package gateway

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stefanocalabrese/orbeat-community/internal/govern"
)

// Edition gating (docs/specs/2026-08-25-orbeat-runtime-interception-design.md
// carries no Community/Enterprise split of its own): this file is deliberately
// NOT ".ee.go". It names only govern.Scanner, govern.CallPayload, govern.ScanCall
// and govern.HasBlocking -- all declared in internal/govern's plain (non-".ee.go")
// files -- plus session/Server (this file's own interceptDeniedError, added for
// fable-audit B38(c), is a plain type declared right here). None of those are
// Enterprise-only, so a Community build has nothing missing to compile against;
// internal/communitygen's own
// TestNoSharedFileReferencesEnterpriseSymbol (which derives its Enterprise-only
// name set from every "*.ee.go"/"*.ee_test.go" file tree-wide, not just this
// package) confirms it finds no leak here -- run alongside this task's other
// gates. This also matches docs/specs/2026-08-17-orbeat-community-enterprise-
// editions-design.md §4.4, whose "Deterministic content scanner" row is ✅ in
// BOTH editions: this hook applies that exact same scanner to a different
// payload shape, so gating it Enterprise-only would contradict the row it
// reuses.

// interceptDeniedError surfaces a content-governance block on the ARGUMENT
// side (interceptArguments, below) to the client as a JSON-RPC error
// distinct from rbac_middleware.go's toolDeniedError (fable-audit B38(c)).
// Before this type existed both denials carried the exact same "forbidden:
// tool not entitled" message, so an agent facing a blocking scan rule had no
// way to tell "you lack a grant, ask an admin" apart from "this call's own
// arguments tripped a governance rule, don't resend them unchanged" -- and
// would routinely retry the untouched call, which is precisely what a
// blocking rule exists to stop.
//
// The message deliberately carries NEITHER the finding's rule id nor its
// severity nor whether this was an explicit block versus a fail-closed scan
// error (auditCallFinding's own doc comment states the identical constraint
// for its audit metadata): the finding's Rule/Message is what the scanner
// exists to find, and an advisory LLM finding's free-text Message in
// particular carries no safety guarantee for direct echo to the caller.
type interceptDeniedError struct{ tool string }

func (e *interceptDeniedError) Error() string {
	return "blocked: content governance intercepted this call's arguments: " + e.tool
}

// interceptArguments is the argument-direction half of runtime call
// interception (Task 2 of docs/plans/orbeat-runtime-interception-2026-08-25.md;
// the result-direction half is Task 3, in broker.go, and is explicitly not
// this function's job). rbacMiddleware calls it after the per-call RBAC
// decision and before proxying the call to the upstream (next), so a blocking
// finding stops the call from ever reaching the upstream tool.
//
// s.interceptor is nil unless ORBEAT_INTERCEPT is set and cmd/gateway wired a
// scanner via Server.SetInterceptor (Task 4) -- nil is a complete no-op, the
// first line returns before anything else runs, so a deployment with the
// feature off is byte-identical to before this slice existed.
//
// A "block" finding denies the call through interceptDeniedError, above --
// deliberately NOT rbacMiddleware's own toolDeniedError (fable-audit
// B38(c)): a missing entitlement and a governance block are different
// things an agent needs to react to differently, so they now leave this
// middleware as two distinguishable error types instead of one shared
// message. "warn"/"info" findings are audited (auditCallFinding) and never
// deny; HasBlocking is the sole authority for that distinction (design spec
// §4). A scanner error -- no built-in govern.Scanner returns one, per
// CompositeScanner's own doc comment, so this path is defensive rather than
// expected -- is treated fail-closed, the same rule the virtual-key
// revocation check above it in rbacMiddleware already follows: a decision
// this middleware could not compute must never be treated as "allow", and it
// is denied through the same interceptDeniedError as an explicit block --
// the caller is told "governance intercepted this call", never which of the
// two actually happened, so as not to hint at the scanner's own failure mode.
func (s *Server) interceptArguments(ctx context.Context, sess *session, tool string, argsJSON []byte) error {
	if s.interceptor == nil {
		return nil
	}
	findings, err := govern.ScanCall(ctx, s.interceptor, govern.CallPayload{
		Tool: tool, Direction: govern.DirectionArguments, Content: string(argsJSON),
	})
	if err != nil {
		s.logger.Warn("intercept scan", "direction", govern.DirectionArguments, "tool", tool, "err", err.Error())
		return &interceptDeniedError{tool: tool}
	}
	for _, f := range findings {
		s.auditCallFinding(ctx, sess, tool, f)
	}
	if govern.HasBlocking(findings) {
		return &interceptDeniedError{tool: tool}
	}
	return nil
}

// auditCallFinding records one interceptor finding (design spec §5):
// gateway.call.blocked for a block-severity finding, gateway.call.flagged for
// anything else (warn/info). Metadata carries only the finding's rule and
// severity -- NEVER its Message, which is the material the scanner exists to
// find (a rule-scanner message like "possible AWS access key ID in content"
// is itself safe, but a future advisory LLM finding's free-text Message
// carries no such guarantee, and this call site cannot tell them apart).
func (s *Server) auditCallFinding(ctx context.Context, sess *session, tool string, f govern.Finding) {
	action, decision := "gateway.call.flagged", "allow"
	if f.Severity == "block" {
		action, decision = "gateway.call.blocked", "deny"
	}
	s.audit(ctx, sess, action, tool, decision, map[string]any{"rule": f.Rule, "severity": f.Severity})
}

// interceptResult is the result-direction half of runtime call interception
// (Task 3 of docs/plans/orbeat-runtime-interception-2026-08-25.md; the
// argument-direction half is interceptArguments above). registerProxies's
// proxy closure (broker.go) calls it AFTER conn.session.CallTool has already
// returned successfully, handing it the WHOLE *mcp.CallToolResult the upstream
// sent back. It mutates that result in place; there is no second, unexamined
// copy of the upstream's answer left for the proxy closure to return.
//
// WHAT IT SCANS, and why it is the whole result rather than res.Content
// (docs/plans/orbeat-comment-sweep-fixes-2026-08-28.md, Bug 1): an
// mcp.CallToolResult carries the upstream's bytes in three places, not one --
// Content, StructuredContent (which the SDK auto-populates for every typed
// ToolHandlerFor upstream, so it is the WELL-BUILT upstreams that have it) and
// the _meta map. Until that sweep this function scanned and replaced Content
// alone, so a blocking finding handed the agent a refusal in "content" while
// the upstream's structured payload rode through untouched in the same
// response, never having been scanned either. DECIDED: scan AND clear, not
// clear alone. Scanning marshalResult's bytes costs one json.Marshal of a
// value that is about to be marshalled anyway, and clearing-without-scanning
// would leave structuredContent an unscanned exfiltration path on every call
// that does not happen to trip a rule in Content -- which is most of them.
//
// WHAT A BLOCK LEAVES BEHIND (withholdResult): refusal text in Content,
// StructuredContent and the upstream's _meta cleared, and IsError set. IsError
// is DECIDED true: a refusal reported with isError false tells the agent the
// call succeeded and that the refusal sentence is the tool's genuine output,
// which is both false and the reading most likely to get the sentence
// summarized to a user as fact. The SDK's own CallToolResult.IsError doc gives
// the same rule -- an error the tool surface produced belongs in Content with
// IsError true, so the model can see it and self-correct.
//
// WHAT THIS IS NOT (design spec §3): a "block" finding here does NOT prevent
// a leak. By the time this function runs, the upstream tool has already
// executed and, if its result carries a secret, that secret has already left
// whatever system produced it -- the round trip already happened. Denying an
// ARGUMENT (interceptArguments, above) prevents an action from occurring at
// all; denying a RESULT does not un-ring that bell. All this does is stop the
// AGENT from seeing what the upstream already returned. An operator who reads
// a gateway.call.blocked audit row on the result side as "orbeat stopped this
// leak" is wrong -- the honest claim is "orbeat stopped itself from showing
// the agent a leak that already happened".
//
// nil-safe on s (unlike interceptArguments, whose only caller, rbacMiddleware,
// is itself a method on a live *Server): registerProxies is exercised
// directly, by name, by several existing broker_test.go tests that build no
// Server at all, and those must see byte-identical behaviour to before this
// task -- the same "nil means total no-op" contract interceptArguments gives
// one layer up, extended one level further here because the RECEIVER itself
// can be nil, not just one of its fields. res == nil is tolerated for the same
// reason the caller already guards it: there is nothing to govern.
func (s *Server) interceptResult(ctx context.Context, sess *session, tool string, res *mcp.CallToolResult) {
	if s == nil || s.interceptor == nil || res == nil {
		return
	}
	findings, err := govern.ScanCall(ctx, s.interceptor, govern.CallPayload{
		Tool: tool, Direction: govern.DirectionResult, Content: marshalResult(res),
	})
	if err != nil {
		// Fail closed, the same rule interceptArguments follows for its own
		// scanner error: a decision this hook could not compute must never be
		// treated as "let the agent see it".
		s.logger.Warn("intercept scan", "direction", govern.DirectionResult, "tool", tool, "err", err.Error())
		withholdResult(res, tool, "scan-error")
		return
	}
	for _, f := range findings {
		s.auditCallFinding(ctx, sess, tool, f)
	}
	if govern.HasBlocking(findings) {
		withholdResult(res, tool, blockingRule(findings))
	}
}

// blockingRule returns the Rule of the first block-severity finding in fs for
// refusalContent's message, or "unknown" when fs carries no blocking finding
// (interceptResult, its only caller, has already checked HasBlocking).
//
// Echoing that rule back to the client is safe, but not for the reason this
// comment gave until 2026-08-28. There is no closed set of rule identifiers:
// the deterministic scanner's are "secret", "reserved-marker", "size" and
// "remote-exec" (never "reserved", which is not a rule id anywhere in
// govern), and an advisory LLM scanner emits "llm-" followed by whatever slug
// the model returned, which is model-supplied text.
//
// The guarantee is a pair of constraints instead, and it is what keeps
// model-supplied text out of the return value. govern's LLM scanner clamps
// every finding it produces to "warn" or "info", so an LLM finding can never
// carry "block"; and this function reads the Rule only of a finding whose
// Severity IS "block". What can leave here is therefore exactly the
// block-severity deterministic identifiers, today "secret" and
// "reserved-marker" ("size" and "remote-exec" are warns), plus the "unknown"
// fallback. That holds however s.interceptor is wired, which is the part that
// matters: SetInterceptor accepts any govern.Scanner, even though cmd/gateway
// installs only the plain rule scanner today.
func blockingRule(fs []govern.Finding) string {
	for _, f := range fs {
		if f.Severity == "block" {
			return f.Rule
		}
	}
	return "unknown"
}

// marshalResult renders the whole result as the string ScanCall's
// CallPayload.Content expects, mirroring how interceptArguments scans
// params.Arguments's raw JSON bytes. It marshals *mcp.CallToolResult itself,
// not res.Content, and that is the point: CallToolResult.MarshalJSON emits
// content, structuredContent, isError and _meta together, so the scanned
// bytes ARE the bytes the client would otherwise receive -- nothing
// summarized, nothing dropped. A secret is therefore found wherever the
// upstream put it: in any mcp.Content variant (not just TextContent), in a
// structuredContent object/array/primitive, or in a _meta value.
//
// The only fields it cannot reach are CallToolResult's unexported ones
// (resultType, err), which carry no upstream bytes.
func marshalResult(res *mcp.CallToolResult) string {
	b, err := json.Marshal(res)
	if err != nil {
		// Defensive only: CallToolResult.MarshalJSON fails only if a value the
		// upstream put in StructuredContent or _meta is unmarshalable, and
		// those arrived by being unmarshalled FROM json a moment earlier.
		// Matches CompositeScanner's own doc comment on why a Scan error is
		// treated as unexpected rather than routine. Returning "" here scans
		// an empty payload, which finds nothing and blocks nothing -- so the
		// caller's fail-closed branch is not reached; that is acceptable
		// precisely because the input is known-marshalable json.
		return ""
	}
	return string(b)
}

// withholdResult replaces every upstream-supplied part of res with the
// refusal. This is what "a blocked result" means: not "Content was swapped",
// which would leave the structured payload and the upstream's _meta riding
// through in the same response (the bug this function exists to close).
//
// res.Meta is the upstream's _meta only. The gateway's own SDK server
// re-annotates the outgoing result with mcp.MetaKeyServerInfo afterwards
// (go-sdk mcp/server.go, annotateServerInfo), so the client still sees a
// non-empty _meta carrying orbeat's own Implementation -- bounded, orbeat's
// own, and nothing to do with the upstream. The guarantee is "no
// upstream-supplied _meta", never "no _meta".
func withholdResult(res *mcp.CallToolResult, tool, rule string) {
	res.Content = refusalContent(tool, rule)
	res.StructuredContent = nil
	res.Meta = nil
	res.IsError = true
}

// refusalContent is the Content a withheld result carries in place of the
// upstream's own (design spec §3). It is not by itself the whole refusal:
// withholdResult, its only caller, also clears StructuredContent and the
// upstream's _meta and sets IsError, because a result carries upstream bytes
// in all of those and swapping Content alone leaves the rest in the response.
//
// tool and rule are both safe to echo: tool is the already-namespaced,
// already-bounded tool name the client itself sent; rule reaches here only
// from blockingRule, whose comment records why a block-severity finding's
// rule id can never be scanned content nor a model-supplied identifier.
func refusalContent(tool, rule string) []mcp.Content {
	return []mcp.Content{&mcp.TextContent{
		Text: fmt.Sprintf("orbeat: result withheld for tool %q -- its output matched a blocking content-governance rule (%s) and was not returned. This does not undo the call: the upstream tool already ran before this check happened.", tool, rule),
	}}
}
