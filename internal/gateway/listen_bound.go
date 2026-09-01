package gateway

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// listenMethod is the one MCP method that blocks rather than dispatching.
const listenMethod = "subscriptions/listen"

// listenMaxHold bounds how long a single subscriptions/listen POST may hold
// its goroutine, and it is sessionMaxAge deliberately rather than a number
// chosen for feel.
//
// Past sessionMaxAge the gateway session this POST belongs to has been evicted
// and rebuilt, so from that moment the only thing the blocked goroutine can
// still be holding is the FROZEN object graph: the old *mcp.Server, its
// rbacMiddleware closure over a pre-revocation entitlement snapshot, and its
// []*upstreamConn. There is nothing left for it to be legitimately waiting
// for. Tying the two together means a future change to sessionMaxAge cannot
// silently reopen the gap by leaving this constant behind.
var listenMaxHold = sessionMaxAge

// boundListen caps subscriptions/listen and nothing else.
//
// THE PROBLEM. The SDK's subscriptions/listen blocks on <-ctx.Done() whenever
// a subscription is agreed, which it is for any server with tools. Nothing
// bounded that: cmd/gateway/main.go sets ReadHeaderTimeout alone, and the
// SDK's startPOST stops the transport idle timer for the POST's whole
// duration, resetting it only in endPOST, which is deferred until the response
// completes. So one client holding a POST open pinned a whole gateway session
// for as long as it liked. withSession's doc comment carried this as an open
// item from the A1 fix; it is resource retention rather than a governance
// bypass, because the held POST forwards no call and reaches no upstream.
//
// WHY THIS METHOD AND NOT A SERVER-WIDE TIMEOUT. The obvious cap is a
// WriteTimeout on the gateway's own http.Server, and it is the wrong one: it
// would bound every legitimate long-running tools/call too. v1.17.0 already
// proved that shape breaks slow tools, when a 10s ResponseHeaderTimeout capped
// every http-transport tool call because the SDK withholds tool-call response
// headers until the result event. That was removed on purpose, pinned by a 2s
// slow-tool test. Capping one blocking method leaves tools/call untouched.
//
// WHY A CAP IS SAFE HERE SPECIFICALLY. This gateway registers no resources and
// no SubscribeHandler, so subscriptions/listen has no work to do against it at
// all: it can only block. A client that wants to keep listening reissues the
// request, which is the ordinary MCP long-poll shape.
func boundListen(method string, maxHold time.Duration) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, m string, req mcp.Request) (mcp.Result, error) {
			if m != method {
				return next(ctx, m, req)
			}
			// The deadline is the whole mechanism: the SDK's handler returns
			// when this ctx is done, which releases the goroutine and with it
			// every reference it held into the session.
			ctx, cancel := context.WithTimeout(ctx, maxHold)
			defer cancel()
			return next(ctx, m, req)
		}
	}
}
