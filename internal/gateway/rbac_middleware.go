package gateway

import (
	"context"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/stefanocalabrese/orbeat-community/internal/rbac"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// maxAuditTargetRunes caps a client-supplied audit target (an MCP tool call
// name) before it is persisted/logged (audit G12): the tool name arrives
// verbatim from the client, so without a bound it is unbounded attacker-
// controlled data landing in the audit table and the log stream.
const maxAuditTargetRunes = 256

// truncateAuditTarget bounds s to at most maxAuditTargetRunes runes, cutting
// on a rune boundary so a multi-byte UTF-8 character is never split (which
// would otherwise produce invalid UTF-8 that Postgres' text column rejects).
func truncateAuditTarget(s string) string {
	if utf8.RuneCountInString(s) <= maxAuditTargetRunes {
		return s
	}
	r := []rune(s)
	return string(r[:maxAuditTargetRunes])
}

// toolCallAllowed maps a namespaced tool id to its catalog server and checks the
// session's entitlements. Unknown slug or malformed name → denied (fail-closed).
func toolCallAllowed(namespaced string, slugToServer map[string]string, ents []store.Entitlement) bool {
	slug, tool, ok := Split(namespaced)
	if !ok {
		return false
	}
	serverID, ok := slugToServer[slug]
	if !ok {
		return false
	}
	return rbac.ToolAllowed(ents, serverID, tool)
}

// toolDeniedError surfaces a per-call RBAC denial to the client as a JSON-RPC error.
type toolDeniedError struct{ tool string }

func (e *toolDeniedError) Error() string { return "forbidden: tool not entitled: " + e.tool }

// rbacMiddleware enforces per-call RBAC on tools/call and audits the decision.
// Non-tools/call methods pass through. A denied call short-circuits with an
// error (surfaced as a JSON-RPC error) and is NOT forwarded.
func (s *Server) rbacMiddleware(sess *session) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
			if !ok {
				// Fail closed (audit G8): server-side tools/call always carries
				// *CallToolParamsRaw in go-sdk v1.6.1, so this is unreachable via
				// SDK routing today — but this is a security chokepoint, and an SDK
				// change here must become a deny, never a silent RBAC bypass.
				s.auditDecision(ctx, sess, "<unparseable>", "deny")
				return nil, &toolDeniedError{tool: "<unparseable>"}
			}
			target := truncateAuditTarget(params.Name)
			if !toolCallAllowed(params.Name, sess.slugToServer, sess.entitlements) {
				s.auditDecision(ctx, sess, target, "deny")
				return nil, &toolDeniedError{tool: params.Name}
			}
			s.auditDecision(ctx, sess, target, "allow")
			return next(ctx, method, req)
		}
	}
}

// auditDecision records a per-call RBAC decision: it is the ONLY place that
// increments the RBACDecision metric (audit G12 — the counter used to
// increment on every s.audit call, including gateway.upstream.connect events
// during session build, skewing deny-rate dashboards), then writes the usual
// audit event.
func (s *Server) auditDecision(ctx context.Context, sess *session, target, decision string) {
	s.metrics.RBACDecision.Add(ctx, 1, metric.WithAttributes(attribute.String("decision", decision)))
	s.audit(ctx, sess, "gateway.tool.call", target, decision)
}
