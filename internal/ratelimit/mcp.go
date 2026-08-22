package ratelimit

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// KeyFunc derives a rate-limit key from an inbound MCP request. ctx is
// PASSED, not omitted, precisely so that "read from ctx instead of
// req.GetExtra().TokenInfo" is a real, easy-to-write mistake a KeyFunc
// implementation must deliberately avoid — not one made structurally
// impossible by the signature. ok is false when no key can be derived (e.g. a
// request built without RequestExtra, as direct-call tests do) — such a call
// is NOT limited: rate limiting is an availability backstop here, not the
// gateway's security gate (bearer-token verification and rbacMiddleware own
// that).
//
// Implementations MUST read the PER-CALL principal from req.GetExtra().
// TokenInfo, never from ctx. Verified against the pinned go-sdk v1.7.0:
// RequestExtra.TokenInfo is stamped fresh on every incoming message —
// mcp/streamable.go's (*streamableServerConn).servePOST re-derives
// auth.TokenInfoFromContext(req.Context()) into a new RequestExtra for every
// HTTP POST, whether it is the session's first (initialize) or a later one.
// ctx, by contrast, is the jsonrpc2 CONNECTION's own context: for a stateful
// session it is bound exactly once, to the FIRST request that created that
// connection (mcp/streamable.go:700-703 — "Pass req.Context() here... The
// context is detached in the jsonrpc2 library when handling the long-running
// stream" — reached only on the no-existing-session-ID branch), and every
// later POST reuses the existing connection without rebinding it. A ctx-based
// lookup would therefore silently key every later call on a session by
// whichever token happened to establish it, not by the token the CURRENT
// call actually carries.
type KeyFunc func(ctx context.Context, req mcp.Request) (key string, ok bool)

// codeRateLimited is a JSON-RPC error code in the -32000..-32099 range the
// JSON-RPC 2.0 spec reserves for implementation-defined server errors.
// Deliberately NOT one of the four codes go-sdk's extractErrorStatus
// (mcp/streamable.go) maps to an HTTP 4xx, and only for protocol
// >= 2026-07-28: jsonrpc.CodeMethodNotFound -> 404; jsonrpc.CodeInvalidParams,
// mcp.CodeUnsupportedProtocolVersion, mcp.CodeMissingRequiredClientCapabilities
// -> 400. Every other code, on every protocol version, stays HTTP 200 (design
// spec §5.2 — an mcp.Middleware cannot set an HTTP status at all). The
// gateway documents that clients like Claude Code treat an HTTP 400 as
// permanent and stop retrying (internal/gateway/server.go); this code can
// never produce one.
const codeRateLimited int64 = -32029

// MCP returns an mcp.Middleware enforcing l against calls to method only —
// every other method passes straight through unmetered (this is how
// "initialize has its own budget, tools/list is unlimited" is expressed: two
// MCP middlewares configured for different methods, and tools/list is in
// neither's method set).
//
// l may be nil (limiting disabled), mirroring the HTTP adapter's nil-safe
// Limiter: a Server that never configures a limiter — including every
// existing test that constructs one directly — behaves exactly as before
// this slice.
//
// The tools/call middleware, the initialize middleware, and rbacMiddleware
// MUST be composed in ONE AddReceivingMiddleware call, limiter(s) before
// rbacMiddleware in the argument list (see gateway.Server.buildSession).
// AddReceivingMiddleware(m1, m2) composes m1(m2(handler)) — the first
// argument runs outermost — but a SEPARATE later call wraps whatever handler
// is already installed, so last-registered runs first. Splitting the
// registration in two therefore reverses the effective order even though
// each call individually "looks" first-argument-outermost, which is exactly
// why this is pinned by a test (internal/gateway/ratelimit_test.go), not a
// comment.
// obs (Task 7, spec §9) reports every rejection to the ratelimit.rejected
// counter and, at most once per key per streak, a sampled log breadcrumb.
// reason is method itself: tools/call and initialize are metered on
// separate limiters (spec §4.3), so the method name is exactly "which budget
// was exceeded". Its zero value is safe, so callers that do not care about
// this telemetry (most direct-construction tests) can pass Observability{}.
func MCP(l *Limiter, method string, keyFn KeyFunc, obs Observability) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, m string, req mcp.Request) (mcp.Result, error) {
			if l == nil || m != method {
				return next(ctx, m, req)
			}
			key, ok := keyFn(ctx, req)
			if !ok {
				return next(ctx, m, req)
			}
			allowed, retryAfter, logRejection := l.AllowSampled(key)
			if !allowed {
				reportRejected(ctx, obs, "gateway", method, key, logRejection)
				return nil, rateLimitedError(retryAfter)
			}
			return next(ctx, m, req)
		}
	}
}

// rateLimitedError builds the JSON-RPC error a throttled call returns (design
// spec §5.2: an mcp.Middleware returns (mcp.Result, error), serialized inside
// an HTTP 200, so this is the only way to carry the retry hint).
//
// It MUST be returned as a *jsonrpc.Error directly, not wrapped in another
// error type. go-sdk's toWireError (internal/jsonrpc2/messages.go) only
// preserves a custom Code and Data when the returned error already IS a
// *jsonrpc.Error ("already a wire error, just use it"); a wrapping type is
// unwound via errors.As for its Code only — Data is dropped entirely — so
// wrapping this the way toolDeniedError wraps a plain denial would silently
// lose the retry hint on the wire.
//
// retryAfter is rendered as integer delta-seconds, rounded up with a floor of
// 1 (RFC 9110 semantics, mirroring the HTTP adapter's Retry-After header,
// which this transport — no response headers available to a method-level
// middleware — has no equivalent of).
func rateLimitedError(retryAfter time.Duration) *jsonrpc.Error {
	secs := int64(math.Ceil(retryAfter.Seconds()))
	if secs < 1 {
		secs = 1
	}
	data, _ := json.Marshal(struct {
		RetryAfterSeconds int64 `json:"retryAfterSeconds"`
	}{secs})
	return &jsonrpc.Error{
		Code:    codeRateLimited,
		Message: "rate limited: retry later",
		Data:    data,
	}
}

// codeConcurrencyLimited is a JSON-RPC error code in the same
// implementation-defined server-error range as codeRateLimited, and
// deliberately NOT equal to it: the two rejections carry different Data
// shapes (retryAfterSeconds vs {limit, inFlight}), and a client dispatches on
// Code to know which one to decode before it can safely unmarshal Data. It is
// also, like codeRateLimited, deliberately none of the four codes go-sdk's
// extractErrorStatus maps to an HTTP 4xx, for the same reason: this rejection
// must stay HTTP 200 on every protocol version so a client that treats 400 as
// permanent doesn't stop retrying.
const codeConcurrencyLimited int64 = -32030

// MCPConcurrency caps how many calls to method a single principal may have IN
// FLIGHT at once. It mirrors MCP's contract exactly — nil limiter, non-matching
// method, or an underivable key all pass through untouched — so a Server that
// never configures one behaves exactly as before.
//
// The release is deferred immediately after a successful acquire, which covers
// normal return, error return, panic and context cancellation, because the
// handler returns in all four. That matters more than usual here: a leaked slot
// is not a degraded cap but a PERMANENT lockout for that principal, since
// nothing else decrements the count.
func MCPConcurrency(c *ConcurrencyLimiter, method string, keyFn KeyFunc, obs Observability) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, m string, req mcp.Request) (mcp.Result, error) {
			if c == nil || m != method {
				return next(ctx, m, req)
			}
			key, ok := keyFn(ctx, req)
			if !ok {
				return next(ctx, m, req)
			}
			release, admitted := c.Acquire(key)
			if !admitted {
				reportRejected(ctx, obs, "gateway", method+" concurrency", key, true)
				return nil, concurrencyLimitedError(c.Max(), c.InFlight(key))
			}
			defer release()
			return next(ctx, m, req)
		}
	}
}

// concurrencyLimitedError builds the JSON-RPC error a capped call returns.
//
// It carries limit and inFlight rather than a retryAfter: a token bucket
// knows when the next token arrives, but a concurrency slot frees when some
// OTHER call finishes, which is not knowable from here. Reporting a
// fabricated duration would put a number in the payload that means nothing —
// {limit, inFlight} is the strictly weaker but honest alternative: it tells
// the caller exactly what it hit without pretending to predict when a slot
// will free.
//
// Like rateLimitedError it MUST be returned as a *jsonrpc.Error directly, not
// wrapped in another error type: go-sdk's toWireError (internal/jsonrpc2/
// messages.go) only preserves a custom Code and Data when the returned error
// already IS a *jsonrpc.Error; a wrapping type is unwound via errors.As for
// its Code only — Data is dropped entirely.
func concurrencyLimitedError(limit, inFlight int) *jsonrpc.Error {
	data, _ := json.Marshal(struct {
		Limit    int `json:"limit"`
		InFlight int `json:"inFlight"`
	}{limit, inFlight})
	return &jsonrpc.Error{
		Code:    codeConcurrencyLimited,
		Message: "too many concurrent calls: retry shortly",
		Data:    data,
	}
}
