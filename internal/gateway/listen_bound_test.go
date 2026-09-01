package gateway

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// blockingHandler stands in for the SDK's subscriptions/listen, whose whole
// behaviour is to block on <-ctx.Done() rather than dispatch. Using a stub
// rather than the real SDK method is deliberate: the property under test is
// that a DEADLINE reaches the handler, and driving the real method would need
// a live transport and a subscription while proving the same one thing.
func blockingHandler(observed *context.Context) mcp.MethodHandler {
	return func(ctx context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
		if observed != nil {
			*observed = ctx
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
}

// TestBoundListenReleasesABlockedListen is the gate for the retention the A1
// fix left open: a subscriptions/listen POST admitted before an eviction held
// the frozen *mcp.Server, its rbacMiddleware closure and its []*upstreamConn
// reachable from one goroutine for as long as the client kept the POST open,
// with nothing bounding it.
//
// The cap is asserted by the handler RETURNING, which is what releases those
// references. A test that only checked the ctx carried a deadline would pass
// on a middleware that set one and never let it reach the handler.
func TestBoundListenReleasesABlockedListen(t *testing.T) {
	mw := boundListen(listenMethod, 40*time.Millisecond)
	h := mw(blockingHandler(nil))

	done := make(chan error, 1)
	go func() {
		_, err := h(context.Background(), listenMethod, &mcp.ServerRequest[*mcp.CallToolParamsRaw]{})
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("blocked listen returned %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal(`subscriptions/listen was still blocked 10s after a 40ms cap.

Nothing bounds this method without the cap: the http.Server sets ReadHeaderTimeout
alone, and the SDK stops the transport idle timer for the POST's whole duration. One
client holding a POST open pins a whole gateway session's object graph.`)
	}
}

// TestBoundListenDoesNotCapOtherMethods is the half that keeps the fix from
// being worse than the defect. The reason this is a per-method cap rather than
// a WriteTimeout on the http.Server is that a server-wide bound would also cap
// every legitimate long-running tools/call: v1.17.0 proved exactly that when a
// 10s ResponseHeaderTimeout capped every http-transport tool call, and removed
// it. A middleware that capped everything would reintroduce it here.
func TestBoundListenDoesNotCapOtherMethods(t *testing.T) {
	var seen context.Context
	mw := boundListen(listenMethod, time.Nanosecond)
	h := mw(func(ctx context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
		seen = ctx
		return nil, nil
	})

	if _, err := h(context.Background(), "tools/call", &mcp.ServerRequest[*mcp.CallToolParamsRaw]{}); err != nil {
		t.Fatalf("tools/call returned %v, want nil", err)
	}
	if seen == nil {
		t.Fatal("handler never ran, so this proved nothing")
	}
	if _, ok := seen.Deadline(); ok {
		t.Error("tools/call was given a deadline by the listen cap. A one-nanosecond cap " +
			"leaking onto tools/call is v1.17.0's ResponseHeaderTimeout defect returning: " +
			"a slow tool would be cut off mid-call.")
	}
}

// TestListenMaxHoldTracksSessionMaxAge pins the constant to its reason rather
// than to its value. Past sessionMaxAge the session this POST belongs to has
// been evicted and rebuilt, so the only thing a still-blocked listen can hold
// is the frozen graph. A future change to sessionMaxAge that left this behind
// would silently widen the window it exists to close.
func TestListenMaxHoldTracksSessionMaxAge(t *testing.T) {
	if listenMaxHold != sessionMaxAge {
		t.Errorf("listenMaxHold = %v, sessionMaxAge = %v. They are tied on purpose: past "+
			"sessionMaxAge a blocked listen can only be holding the evicted session's frozen "+
			"object graph, so a cap longer than it leaves exactly the retention this closes.",
			listenMaxHold, sessionMaxAge)
	}
}

// TestBoundListenIsWiredIntoTheMiddlewareChain closes the gap the other tests
// in this file cannot: they drive boundListen directly, so every one of them
// passes with the middleware absent from buildSession's
// AddReceivingMiddleware call. That is this repo's inert-feature class, and it
// has shipped twice: v1.25.0's SSRF guard was wired to nothing while its own
// tests were green, and virtual keys shipped nine tasks of tested code that
// cmd/api never installed.
//
// Derived from buildSession's own source rather than from a list, so adding a
// middleware does not require touching this test and REMOVING this one fails
// it. Red-proven by deleting the line from the chain.
func TestBoundListenIsWiredIntoTheMiddlewareChain(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "server.go", nil, 0)
	if err != nil {
		t.Fatalf("parse server.go: %v", err)
	}

	var chain []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "AddReceivingMiddleware" {
			return true
		}
		for _, arg := range call.Args {
			inner, ok := arg.(*ast.CallExpr)
			if !ok {
				continue
			}
			switch fn := inner.Fun.(type) {
			case *ast.Ident:
				chain = append(chain, fn.Name)
			case *ast.SelectorExpr:
				chain = append(chain, fn.Sel.Name)
			}
		}
		return true
	})

	if len(chain) == 0 {
		t.Fatal("found no AddReceivingMiddleware call in server.go, so this gate proved nothing. " +
			"Either the chain moved or the SDK method was renamed; fix this gate, do not delete it.")
	}
	for _, name := range chain {
		if name == "boundListen" {
			return
		}
	}
	t.Errorf(`boundListen is not in buildSession's middleware chain. Found: %v

Every other test in this file drives boundListen directly and passes with it absent
from the chain, so without this gate the cap ships inert: a subscriptions/listen POST
goes on pinning the whole session object graph with nothing bounding it, and the suite
stays green.`, chain)
}
