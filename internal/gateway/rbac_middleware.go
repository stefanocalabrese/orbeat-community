package gateway

import (
	"context"
	"sync"
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

// toolCallAllowed maps a namespaced tool id to its catalog server and checks
// the session's entitlements. Unknown slug or malformed name → denied
// (fail-closed).
//
// keyID distinguishes a human session (empty -- behaviour identical to
// before virtual keys existed: the role's live grants alone, via
// rbac.ToolAllowed) from a virtual-key session (non-empty), which is instead
// decided by rbac.KeyToolAllowed -- the role's grants INTERSECTED with the
// key's narrowing (docs/specs/2026-08-25-orbeat-virtual-keys-design.md §6).
// keyNarrow is sess.keyNarrow verbatim: the key's RAW, NAMESPACED
// allowed_tools column, spanning every server the role can see. It is
// filtered down to slug's bare tool names by filterKeyNarrowForServer below
// BEFORE it ever reaches KeyToolAllowed, which stays namespacing-free by
// design and must never see it unfiltered -- spec §6 "the second trap":
// passing the flat list straight through would let a narrowing meant for
// one server also apply to any other server whose role grant happens to
// share the same bare tool name (measured: narrow=["read"] unfiltered
// allowed BOTH srv1/read and srv2/read on a role granting bare "read" on
// both).
func toolCallAllowed(namespaced string, slugToServer map[string]string, ents []store.Entitlement, keyID string, keyNarrow []string) bool {
	_, _, _, ok := toolCallAuthorization(namespaced, slugToServer, ents, keyID, keyNarrow)
	return ok
}

// toolCallAuthorization is toolCallAllowed's fuller sibling, alongside it
// rather than replacing it (toolCallAllowed now delegates here, so the two
// can never disagree): for an ALLOWED call it also reports the server's
// store id, the bare tool name, and the id of the role whose entitlement
// authorized it -- the three facts usage metering (this task's Count call,
// below) and quota enforcement need beyond the bare bool. roleID comes from
// rbac.AuthorizingEntitlement, the SAME predicate rbac.ToolAllowed is
// defined in terms of (Task 1's correction), so a call this function
// authorizes always has a real authorizing entitlement to report -- the
// "not found" branch below is unreachable today and exists as fail-closed
// defense, not a live path.
func toolCallAuthorization(namespaced string, slugToServer map[string]string, ents []store.Entitlement, keyID string, keyNarrow []string) (serverID, tool, roleID string, ok bool) {
	slug, toolName, split := Split(namespaced)
	if !split {
		return "", "", "", false
	}
	srvID, known := slugToServer[slug]
	if !known {
		return "", "", "", false
	}
	var allowed bool
	if keyID == "" {
		allowed = rbac.ToolAllowed(ents, srvID, toolName)
	} else {
		allowed = rbac.KeyToolAllowed(ents, filterKeyNarrowForServer(keyNarrow, slug), srvID, toolName)
	}
	if !allowed {
		return "", "", "", false
	}
	ent, found := rbac.AuthorizingEntitlement(ents, srvID, toolName)
	if !found {
		// Unreachable given allowed == true above: both branches reduce to
		// rbac.ToolAllowed holding for (srvID, toolName), and
		// AuthorizingEntitlement is the exact predicate ToolAllowed is
		// defined in terms of. Fail closed rather than attribute to a
		// zero-value role if that invariant is ever violated.
		return "", "", "", false
	}
	return srvID, toolName, ent.RoleID, true
}

// filterKeyNarrowForServer reduces a virtual key's raw, namespaced
// allowed_tools list to the bare tool names that belong to slug -- the
// per-server split spec §6 requires before rbac.KeyToolAllowed ever sees a
// narrowing. Malformed entries (no "__" separator; should never occur since
// the admin surface only ever writes Namespace(slug, tool) output, but this
// is untrusted-shape defense, not user input) are skipped rather than
// trusted.
//
// nil narrow passes through as nil unchanged: it means "the key applies no
// narrowing at all", and KeyToolAllowed's own nil case already means
// "everything the role allows" -- nothing to filter. A non-nil narrow ALWAYS
// produces a non-nil result, via make, even when nothing matches slug: an
// empty result correctly means "this key says nothing about THIS server",
// which must deny every tool on it, not fall back to nil's "everything the
// role allows" meaning. Silently promoting a no-match result to nil here
// would reopen exactly the cross-server leak this function exists to close
// -- a key narrowed only to srv1 would stop denying srv2 outright.
func filterKeyNarrowForServer(narrow []string, slug string) []string {
	if narrow == nil {
		return nil
	}
	out := make([]string, 0, len(narrow))
	for _, n := range narrow {
		s, tool, ok := Split(n)
		if ok && s == slug {
			out = append(out, tool)
		}
	}
	return out
}

// revocationCache remembers which virtual keys (by client_id) are known
// revoked, so a dead credential that keeps calling doesn't force a store
// round trip every time (docs/specs/2026-08-25-orbeat-virtual-keys-design.md
// §8). It ONLY ever caches true: revocation is monotonic -- a key never
// un-revokes (store.RevokeVirtualKey's doc comment) -- so that is the one
// answer that can never go stale. "Not revoked" is deliberately never
// cached anywhere, by this type or its caller: Server.keyRevoked
// (virtualkey.ee.go) always re-checks a not-yet-known-revoked key against
// the store, which is what makes a revocation felt on the very next call
// after it happens rather than after some TTL expires. Safe for concurrent
// use; entries are never removed (there is nothing to invalidate -- a true
// entry is permanently correct for that client_id's lifetime).
type revocationCache struct {
	mu sync.Mutex
	m  map[string]bool
}

func newRevocationCache() *revocationCache {
	return &revocationCache{m: make(map[string]bool)}
}

// isKnownRevoked reports whether clientID has already been proven revoked.
// false does NOT mean "not revoked" -- it means "not yet proven revoked by
// this cache", so a false answer must always be followed by a fresh store
// check (Server.keyRevoked does exactly this).
func (c *revocationCache) isKnownRevoked(clientID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[clientID]
}

// markRevoked records clientID as permanently revoked.
func (c *revocationCache) markRevoked(clientID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[clientID] = true
}

// toolDeniedError surfaces a per-call RBAC denial (no entitlement) to the
// client as a JSON-RPC error. interceptDeniedError (intercept.go) is the
// distinct sibling used for a content-governance block, deliberately never
// this type -- fable-audit B38(c): before that split both denials carried
// the exact same "forbidden: tool not entitled" message, so an agent facing
// a blocking scan rule had no way to tell "you lack a grant" apart from
// "this call's own arguments tripped a governance rule" and would routinely
// retry the untouched call.
type toolDeniedError struct{ tool string }

func (e *toolDeniedError) Error() string { return "forbidden: tool not entitled: " + e.tool }

// toolsListDeniedError surfaces a denied tools/list to the client
// (rbacToolsList, below) as a JSON-RPC error. Deliberately not
// toolDeniedError: a tools/list denial is not about any one tool, so
// echoing a tool name the way that type does would be misleading.
type toolsListDeniedError struct{}

func (e *toolsListDeniedError) Error() string { return "forbidden: virtual key revoked" }

// listAuditTarget is the audit Target for a denied tools/list -- there is no
// single tool name to attach, unlike a tools/call denial, so a fixed
// bracketed sentinel is used instead (mirrors "<unparseable>" above).
const listAuditTarget = "<tools/list>"

// rbacMiddleware enforces per-call RBAC on tools/call, audits the decision,
// and (rbacToolsList) closes tools/list against an already-revoked virtual
// key. Every other method passes through untouched.
//
// ctx is patched with the CURRENT call's own request id (fable-audit B13,
// REPRODUCED) before anything below writes an audit row: the ctx this
// function is invoked with is the jsonrpc2 connection's own context, frozen
// at whichever HTTP POST established the transport session -- see
// request_id.go's package doc comment (the identical fact sessionKeyFn's own
// doc comment states for req.GetExtra().TokenInfo). Every audit call
// reachable from here, including interceptResult one layer further down in
// broker.go's proxy closure (which receives this same, now-patched ctx via
// next), picks up the override.
func (s *Server) rbacMiddleware(sess *session) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			ctx = withCallRequestID(ctx, perCallRequestID(req))
			switch method {
			case "tools/call":
				return s.rbacToolsCall(ctx, sess, next, req)
			case "tools/list":
				return s.rbacToolsList(ctx, sess, next, method, req)
			default:
				return next(ctx, method, req)
			}
		}
	}
}

// rbacToolsCall is rbacMiddleware's tools/call branch: per-call RBAC
// (entitlement + virtual-key revocation), runtime interception, quota
// enforcement and usage metering, in that order.
//
// For a virtual-key session (sess.keyID != ""), the revocation check runs
// BEFORE the entitlement check, on EVERY call -- never once at session build,
// and never read off anything the session cached (docs/specs/2026-08-25-
// orbeat-virtual-keys-design.md §8). Sessions cache entitlements for up to
// sessionMaxAge; a revoked key's very next tool call must still fail no
// matter how fresh that cache is, which is exactly what a session-build-time
// check could not guarantee. A human session (keyID == "") takes neither
// this check nor the narrowing branch in toolCallAllowed below -- its
// behaviour is byte-for-byte what it was before virtual keys existed.
func (s *Server) rbacToolsCall(ctx context.Context, sess *session, next mcp.MethodHandler, req mcp.Request) (mcp.Result, error) {
	params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
	if !ok {
		// Fail closed (audit G8): server-side tools/call always carries
		// *CallToolParamsRaw in go-sdk v1.6.1, so this is unreachable via
		// SDK routing today -- but this is a security chokepoint, and an SDK
		// change here must become a deny, never a silent RBAC bypass.
		s.auditDecision(ctx, sess, "<unparseable>", "deny")
		return nil, &toolDeniedError{tool: "<unparseable>"}
	}
	target := truncateAuditTarget(params.Name)
	if sess.keyID != "" {
		revoked, err := s.keyRevoked(ctx, sess.tenantID, sess.keyClientID)
		if err != nil {
			// Fail closed: a revocation check the store could not
			// answer must never be treated as "not revoked".
			s.logger.Warn("virtual key revocation check", "key_id", sess.keyID, "err", err.Error())
			s.auditDecision(ctx, sess, target, "deny")
			return nil, &toolDeniedError{tool: params.Name}
		}
		if revoked {
			s.auditDecision(ctx, sess, target, "deny")
			return nil, &toolDeniedError{tool: params.Name}
		}
	}
	serverID, tool, roleID, ok := toolCallAuthorization(params.Name, sess.slugToServer, sess.entitlements, sess.keyID, sess.keyNarrow)
	if !ok {
		s.auditDecision(ctx, sess, target, "deny")
		return nil, &toolDeniedError{tool: params.Name}
	}
	s.auditDecision(ctx, sess, target, "allow")
	// Runtime call interception (intercept.go, Task 2 of docs/plans/
	// orbeat-runtime-interception-2026-08-25.md): a no-op unless
	// s.interceptor is configured. Deliberately AFTER the RBAC decision
	// above (so RBAC's own allow/deny audit trail is unaffected by
	// governance) and BEFORE next (so a blocking finding never reaches
	// the upstream tool).
	if err := s.interceptArguments(ctx, sess, target, params.Arguments); err != nil {
		return nil, err
	}
	// Quota enforcement (docs/specs/2026-08-25-orbeat-usage-metering-
	// design.md section 2): a no-op unless s.quota is configured.
	// Checked AFTER the interceptor above (a call the interceptor
	// already blocked never reaches here) and BEFORE both usage
	// counting and next, so a call denied for being over quota is
	// neither counted (spec section 1: only allowed, forwarded calls
	// count) nor proxied to the upstream. quota.Check's error is
	// already a *jsonrpc.Error (quota.ee.go's quotaExceededError,
	// mirroring internal/ratelimit's shape) -- returned directly, never
	// wrapped, so the retry hint in its Data survives onto the wire.
	if s.quota != nil {
		if err := s.quota.Check(roleID); err != nil {
			return nil, err
		}
	}
	// Usage metering (spec section 1, section 3): a no-op unless
	// s.usage is configured. Deliberately AFTER both the interceptor
	// and the quota check above, and BEFORE next, so ONLY a call that
	// is actually allowed, actually cleared governance, actually under
	// quota, and is actually about to be forwarded is ever counted --
	// a call denied by any of the checks above did no work and must
	// never increment (it would let a hostile client exhaust another
	// role's quota with calls it was never permitted to make).
	if s.usage != nil {
		s.usage.Count(sess.subject, serverID, tool, roleID)
	}
	return next(ctx, "tools/call", req)
}

// rbacToolsList is rbacMiddleware's tools/list branch (fable-audit B11,
// second half: "a revoked virtual key can still enumerate"). Per-tool
// AllowedTools filtering is deliberately NOT re-decided here: it is enforced
// once, at session-build time, by registerProxies (broker.go) only ever
// registering a tool this session's entitlements actually grant, so the
// SDK's own built-in tools/list handler (next, below) can only ever
// enumerate what a real tools/call could also reach -- one predicate
// (toolCallAllowed) decides both surfaces, so they cannot disagree by
// construction, which is the actual fix for the first half of B11 ("session
// build filters by server visibility; per-tool AllowedTools is applied only
// at call time").
//
// What session-build-time filtering cannot catch is revocation that happens
// AFTER the session was already cached: rbacToolsCall's own per-call
// revocation check already refuses tools/call for a revoked virtual key on
// its very next call, but a cached session survives up to sessionMaxAge, and
// before this branch existed tools/list was reachable through it with no
// revocation check at all -- handing a caller whose credential is already
// dead the full list of entitled tool names and schemas. Human sessions
// (sess.keyID == "") have no per-call revocation concept (their access
// changes through role/entitlement edits, felt at the next session rebuild
// like every other human-path behaviour) and take neither this check nor
// what it implies, matching rbacToolsCall's own human/key fork.
//
// A denial here is audited but deliberately does NOT go through
// auditDecision: that helper is the ONLY place the RBACDecision metric
// increments (audit G12), and that counter's whole point is to reflect
// per-TOOL-CALL allow/deny decisions -- folding a session-level enumeration
// gate into it would skew the exact dashboard G12 exists to keep honest.
func (s *Server) rbacToolsList(ctx context.Context, sess *session, next mcp.MethodHandler, method string, req mcp.Request) (mcp.Result, error) {
	if sess.keyID == "" {
		return next(ctx, method, req)
	}
	revoked, err := s.keyRevoked(ctx, sess.tenantID, sess.keyClientID)
	if err != nil {
		// Fail closed, the same rule rbacToolsCall's own revocation check
		// follows: a decision the store could not answer must never be
		// treated as "not revoked".
		s.logger.Warn("virtual key revocation check", "key_id", sess.keyID, "err", err.Error())
		s.audit(ctx, sess, "gateway.tools.list", listAuditTarget, "deny")
		return nil, &toolsListDeniedError{}
	}
	if revoked {
		s.audit(ctx, sess, "gateway.tools.list", listAuditTarget, "deny")
		return nil, &toolsListDeniedError{}
	}
	return next(ctx, method, req)
}

// auditDecision records a per-call RBAC decision: it is the ONLY place that
// increments the RBACDecision metric (audit G12 -- the counter used to
// increment on every s.audit call, including gateway.upstream.connect events
// during session build, skewing deny-rate dashboards), then writes the usual
// audit event.
func (s *Server) auditDecision(ctx context.Context, sess *session, target, decision string) {
	s.metrics.RBACDecision.Add(ctx, 1, metric.WithAttributes(attribute.String("decision", decision)))
	s.audit(ctx, sess, "gateway.tool.call", target, decision)
}
