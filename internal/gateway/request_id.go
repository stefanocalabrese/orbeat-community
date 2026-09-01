package gateway

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stefanocalabrese/orbeat-community/internal/logging"
)

// callRequestIDKey carries a per-MCP-call request id override into audit
// calls made from inside the jsonrpc2 method-handler chain (rbacMiddleware,
// intercept.go), overriding logging.RequestID's ordinary ctx lookup.
//
// WHY THIS EXISTS (fable-audit B13, REPRODUCED): the ctx a receiving
// mcp.Middleware is invoked with is the jsonrpc2 CONNECTION's own context,
// frozen at whichever HTTP POST established the transport session --
// sessionKeyFn's doc comment (server.go) states the identical fact for
// req.GetExtra().TokenInfo and is the reason that function reads the
// per-call Extra instead of ctx. auditAs (server.go) used to call
// logging.RequestID(ctx) directly, so every gateway.tool.call /
// gateway.call.blocked / gateway.call.flagged audit LOG LINE (never the
// durable Postgres row -- store.AuditEvent carries no request_id column)
// emitted from inside one MCP transport session carried the SAME id: not
// "no id" but a WRONG, repeated one that matches none of the calls it was
// attached to, which is what makes this worse than an absent field for an
// incident responder correlating against the HTTP request log.
//
// THE FIX MIRRORS sessionKeyFn'S: withSession (server.go) restamps the
// resolved-by-logging.Requests request id onto r.Header before the SDK ever
// reads it, and the SDK's own servePOST (go-sdk@v1.7.0 mcp/streamable.go)
// repopulates RequestExtra.Header fresh from req.Header on EVERY POST -- so
// perCallRequestID below recovers the id of the CURRENT call's own HTTP
// request, never the one that opened the connection. rbacMiddleware installs
// it on ctx as early as possible so every audit call reachable from within
// that one dispatch (including interceptResult, one layer further down in
// broker.go's proxy closure, which receives the SAME ctx via next(ctx, ...))
// picks it up.
type callRequestIDKey struct{}

// withCallRequestID returns ctx carrying id as the per-call request id
// override, or ctx unchanged when id is empty (nothing to override with --
// leaves requestIDFor's ordinary ctx fallback in effect).
func withCallRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, callRequestIDKey{}, id)
}

// requestIDFor returns the request id an audit call should attach: the
// per-call override installed by withCallRequestID when present, else
// ctx's ordinary logging.RequestID -- which remains correct for every audit
// call site that runs directly inside an HTTP handler rather than inside the
// mcp middleware chain (buildSession's own s.audit calls, reached from
// withSession before getServer ever hands control to the SDK's dispatch).
func requestIDFor(ctx context.Context) string {
	if id, ok := ctx.Value(callRequestIDKey{}).(string); ok && id != "" {
		return id
	}
	return logging.RequestID(ctx)
}

// perCallRequestID recovers the CURRENT call's own request id from the
// per-call RequestExtra the SDK repopulates fresh on every POST (see this
// file's package doc comment above). "" when req carries no Extra/Header at
// all -- a unit test driving rbacMiddleware directly without going through
// Handler()/withSession, which is the same nil-safe shape sessionKeyFn
// itself already tolerates for req.GetExtra().TokenInfo.
func perCallRequestID(req mcp.Request) string {
	extra := req.GetExtra()
	if extra == nil {
		return ""
	}
	return extra.Header.Get(requestIDHeader)
}
