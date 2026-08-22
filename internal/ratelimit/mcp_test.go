package ratelimit

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fixedKeyFn returns a KeyFunc that always yields key, ok.
func fixedKeyFn(key string, ok bool) KeyFunc {
	return func(context.Context, mcp.Request) (string, bool) { return key, ok }
}

func nopHandler(context.Context, string, mcp.Request) (mcp.Result, error) { return nil, nil }

func TestMCPNilLimiterNeverThrottles(t *testing.T) {
	handler := MCP(nil, "tools/call", fixedKeyFn("k", true), Observability{})(nopHandler)
	for i := 0; i < 50; i++ {
		if _, err := handler(context.Background(), "tools/call", &mcp.ServerRequest[*mcp.CallToolParamsRaw]{}); err != nil {
			t.Fatalf("call %d: nil limiter must never throttle, got %v", i, err)
		}
	}
}

func TestMCPOnlyMetersConfiguredMethod(t *testing.T) {
	// A real, tiny budget: a mismatched method must pass through even though
	// the bucket is drained, proving the method filter — not incidental
	// allowance — is why it succeeds.
	l := New(0.001, 1, time.Minute, 100)
	defer l.Close()

	handler := MCP(l, "tools/call", fixedKeyFn("k", true), Observability{})(nopHandler)

	// Drain the sole token via a matching call.
	if _, err := handler(context.Background(), "tools/call", &mcp.ServerRequest[*mcp.CallToolParamsRaw]{}); err != nil {
		t.Fatalf("first tools/call (within burst) should succeed: %v", err)
	}
	// A DIFFERENT method must pass straight through even though the bucket is drained.
	for i := 0; i < 10; i++ {
		if _, err := handler(context.Background(), "tools/list", &mcp.ServerRequest[*mcp.CallToolParamsRaw]{}); err != nil {
			t.Fatalf("tools/list call %d: unmetered method must never be throttled, got %v", i, err)
		}
	}
	// The metered method is now throttled.
	if _, err := handler(context.Background(), "tools/call", &mcp.ServerRequest[*mcp.CallToolParamsRaw]{}); err == nil {
		t.Fatal("second tools/call should have been throttled")
	}
}

func TestMCPKeyFnNotOKPassesThrough(t *testing.T) {
	l := New(0.001, 1, time.Minute, 100)
	defer l.Close()
	handler := MCP(l, "tools/call", fixedKeyFn("", false), Observability{})(nopHandler)
	for i := 0; i < 20; i++ {
		if _, err := handler(context.Background(), "tools/call", &mcp.ServerRequest[*mcp.CallToolParamsRaw]{}); err != nil {
			t.Fatalf("call %d: a request with no derivable key must never be throttled, got %v", i, err)
		}
	}
}

func TestMCPThrottledCallIsAJSONRPCErrorWithRetryData(t *testing.T) {
	l := New(0.001, 1, time.Minute, 100)
	defer l.Close()
	handler := MCP(l, "tools/call", fixedKeyFn("k", true), Observability{})(nopHandler)

	if _, err := handler(context.Background(), "tools/call", &mcp.ServerRequest[*mcp.CallToolParamsRaw]{}); err != nil {
		t.Fatalf("first call (within burst) should succeed: %v", err)
	}
	_, err := handler(context.Background(), "tools/call", &mcp.ServerRequest[*mcp.CallToolParamsRaw]{})
	if err == nil {
		t.Fatal("second call should have been throttled")
	}
	// A DIRECT type assertion, not errors.As, and the distinction is the whole
	// point of this assertion. go-sdk's toWireError does `err.(*WireError)` for
	// the "already a wire error, just use it" path and only falls back to
	// errors.As — for Code ALONE, dropping Data — when that assertion fails.
	// errors.As walks the Unwrap chain, so it succeeds on a wrapped error and
	// hands back the original object with Data intact: a test built on it alone
	// passes identically whether or not the error is wrapped.
	//
	// Measured, not reasoned: wrapping rateLimitedError in fmt.Errorf left this
	// test green before this line existed, while the retry hint would have been
	// dropped on the wire. Found while adding the sibling concurrency test.
	if _, direct := err.(*jsonrpc.Error); !direct {
		t.Fatalf("error must be a *jsonrpc.Error DIRECTLY, not wrapped (%T) — toWireError drops Data for a wrapper", err)
	}
	var jerr *jsonrpc.Error
	if !errors.As(err, &jerr) {
		t.Fatalf("expected a *jsonrpc.Error, got %T: %v", err, err)
	}
	if jerr.Code != codeRateLimited {
		t.Errorf("Code = %d, want %d", jerr.Code, codeRateLimited)
	}
	if jerr.Code == jsonrpc.CodeMethodNotFound || jerr.Code == jsonrpc.CodeInvalidParams {
		t.Errorf("Code %d collides with an SDK code that maps to an HTTP 4xx (spec §5.2)", jerr.Code)
	}
	if len(jerr.Data) == 0 {
		t.Fatal("Data must carry the retry hint — an mcp.Middleware cannot set a Retry-After header")
	}
	var payload struct {
		RetryAfterSeconds int64 `json:"retryAfterSeconds"`
	}
	if err := json.Unmarshal(jerr.Data, &payload); err != nil {
		t.Fatalf("Data is not valid JSON: %v", err)
	}
	if payload.RetryAfterSeconds < 1 {
		t.Errorf("retryAfterSeconds = %d, want >= 1 (floor of 1, never 0)", payload.RetryAfterSeconds)
	}
}

// TestMCPReportsRejectionCounter is the gateway-side half of Task 7's
// "incremented from both adapters" requirement: TestHTTPCounterIncrementsOnlyOnRejection
// (http_test.go) pins the API adapter; this pins that MCP's own rejection
// path increments the SAME orbeat.ratelimit.rejected counter, not on the
// allowed call that precedes it.
func TestMCPReportsRejectionCounter(t *testing.T) {
	obs, rdr, _ := newTestObservability()

	l := New(0.001, 1, time.Minute, 100)
	defer l.Close()
	handler := MCP(l, "tools/call", fixedKeyFn("k", true), obs)(nopHandler)

	if _, err := handler(context.Background(), "tools/call", &mcp.ServerRequest[*mcp.CallToolParamsRaw]{}); err != nil {
		t.Fatalf("first call (within burst) should succeed: %v", err)
	}
	if got := rejectedCounterValue(t, rdr); got != 0 {
		t.Fatalf("counter after one ALLOWED call = %d, want 0", got)
	}

	if _, err := handler(context.Background(), "tools/call", &mcp.ServerRequest[*mcp.CallToolParamsRaw]{}); err == nil {
		t.Fatal("second call should have been throttled")
	}
	if got := rejectedCounterValue(t, rdr); got != 1 {
		t.Fatalf("counter after one REJECTED call = %d, want 1", got)
	}
}

// TestMCPConcurrencyRejectsAboveTheCap drives the middleware itself rather
// than the limiter, so it covers the wiring: method matching, key
// derivation, and the defer'd release.
func TestMCPConcurrencyRejectsAboveTheCap(t *testing.T) {
	c := NewConcurrency(1, time.Minute)
	defer c.Close()
	mw := MCPConcurrency(c, "tools/call", fixedKeyFn("alice", true), Observability{})

	// A handler that blocks until released, so a call is genuinely in flight.
	block := make(chan struct{})
	held := make(chan struct{})
	slow := mw(func(ctx context.Context, m string, req mcp.Request) (mcp.Result, error) {
		close(held)
		<-block
		return nil, nil
	})
	go func() { _, _ = slow(context.Background(), "tools/call", nil) }()
	<-held

	// Second concurrent call must be rejected while the first is in flight.
	reject := mw(nopHandler)
	if _, err := reject(context.Background(), "tools/call", nil); err == nil {
		t.Fatal("second concurrent call must be rejected at a cap of 1")
	}
	close(block)
}

// TestMCPConcurrencyReleasesOnPanic proves the slot frees when the handler
// panics. A happy-path-only test passes on the implementation that leaks, and a
// leaked slot is a PERMANENT lockout for that principal — nothing else
// decrements the count.
func TestMCPConcurrencyReleasesOnPanic(t *testing.T) {
	c := NewConcurrency(1, time.Minute)
	defer c.Close()
	mw := MCPConcurrency(c, "tools/call", fixedKeyFn("alice", true), Observability{})

	boom := mw(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		panic("boom")
	})
	func() {
		defer func() { _ = recover() }()
		_, _ = boom(context.Background(), "tools/call", nil)
	}()

	if n := c.InFlight("alice"); n != 0 {
		t.Fatalf("in-flight = %d after a panicking handler, want 0 — the slot leaked and alice is locked out forever", n)
	}
}

// TestMCPConcurrencyReleasesOnCancel proves the same for a cancelled call.
func TestMCPConcurrencyReleasesOnCancel(t *testing.T) {
	c := NewConcurrency(1, time.Minute)
	defer c.Close()
	mw := MCPConcurrency(c, "tools/call", fixedKeyFn("alice", true), Observability{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := mw(func(ctx context.Context, m string, req mcp.Request) (mcp.Result, error) {
		return nil, ctx.Err()
	})
	_, _ = h(ctx, "tools/call", nil)

	if n := c.InFlight("alice"); n != 0 {
		t.Fatalf("in-flight = %d after a cancelled call, want 0 — the slot leaked", n)
	}
}

// TestMCPConcurrencyPassesThroughWhenNotConfigured mirrors MCP's contract.
func TestMCPConcurrencyPassesThroughWhenNotConfigured(t *testing.T) {
	// nil limiter
	if _, err := MCPConcurrency(nil, "tools/call", fixedKeyFn("alice", true), Observability{})(nopHandler)(
		context.Background(), "tools/call", nil); err != nil {
		t.Fatalf("nil limiter must pass through, got %v", err)
	}
	// non-matching method: a cap of 0 would reject if it metered this method
	c := NewConcurrency(1, time.Minute)
	defer c.Close()
	_, _ = c.Acquire("alice") // alice is already at her cap
	if _, err := MCPConcurrency(c, "tools/call", fixedKeyFn("alice", true), Observability{})(nopHandler)(
		context.Background(), "initialize", nil); err != nil {
		t.Fatalf("non-matching method must pass through, got %v", err)
	}
	// underivable key
	if _, err := MCPConcurrency(c, "tools/call", fixedKeyFn("", false), Observability{})(nopHandler)(
		context.Background(), "tools/call", nil); err != nil {
		t.Fatalf("underivable key must pass through, got %v", err)
	}
}

// TestMCPConcurrencyErrorCarriesDataOnTheWire mirrors
// TestMCPThrottledCallIsAJSONRPCErrorWithRetryData's idiom: errors.As to the
// concrete *jsonrpc.Error (never wrapped, or Data is dropped on the wire per
// mcp.go's toWireError doc comment) and decode Data. It must assert the
// decoded payload carries the limit and the in-flight count, since the
// wire-shape defect (a wrapped error) is invisible to any assertion that only
// checks the code or "an error happened".
//
// It ALSO asserts a raw type assertion (err.(*jsonrpc.Error)), not just
// errors.As, and this is load-bearing, not redundant: errors.As walks the
// whole Unwrap() chain, so errors.As(fmt.Errorf("%w", concurrencyLimitedError(...)), &jerr)
// still finds the original *jsonrpc.Error with Data fully intact — verified
// against go-sdk v1.7.0's actual toWireError (internal/jsonrpc2/messages.go),
// which does NOT use errors.As for this check. It does a raw type assertion
// (err.(*WireError), "already a wire error, just use it") and only falls
// back to errors.As — explicitly dropping Data — when THAT assertion fails.
// A test using only errors.As therefore cannot catch a wrap: it would pass on
// both the correct code and the buggy one, exactly the "invisible to any
// assertion that checks only the code" trap this test exists to close.
func TestMCPConcurrencyErrorCarriesDataOnTheWire(t *testing.T) {
	c := NewConcurrency(1, time.Minute)
	defer c.Close()
	mw := MCPConcurrency(c, "tools/call", fixedKeyFn("alice", true), Observability{})

	release, ok := c.Acquire("alice")
	if !ok {
		t.Fatal("setup: first acquire must succeed")
	}
	defer release()

	_, err := mw(nopHandler)(context.Background(), "tools/call", nil)
	if err == nil {
		t.Fatal("call above the cap should have been rejected")
	}
	if _, direct := err.(*jsonrpc.Error); !direct {
		t.Fatalf("error must be a *jsonrpc.Error DIRECTLY (got %T) — go-sdk's toWireError type-asserts rather than errors.As, so a wrap drops Data on the wire even though errors.As still finds it at the Go level", err)
	}
	var jerr *jsonrpc.Error
	if !errors.As(err, &jerr) {
		t.Fatalf("expected a *jsonrpc.Error, got %T: %v", err, err)
	}
	if jerr.Code != codeConcurrencyLimited {
		t.Errorf("Code = %d, want %d", jerr.Code, codeConcurrencyLimited)
	}
	if jerr.Code == codeRateLimited {
		t.Error("concurrency-cap rejections must NOT share the rate-limiter's code — the Data payload shapes differ and a client needs the code to know which one to decode")
	}
	if jerr.Code == jsonrpc.CodeMethodNotFound || jerr.Code == jsonrpc.CodeInvalidParams {
		t.Errorf("Code %d collides with an SDK code that maps to an HTTP 4xx (spec §5.2)", jerr.Code)
	}
	if len(jerr.Data) == 0 {
		t.Fatal("Data must carry {limit, inFlight} — an mcp.Middleware cannot set a Retry-After header, and a concurrency cap cannot compute one anyway")
	}
	var payload struct {
		Limit    int `json:"limit"`
		InFlight int `json:"inFlight"`
	}
	if err := json.Unmarshal(jerr.Data, &payload); err != nil {
		t.Fatalf("Data is not valid JSON: %v", err)
	}
	if payload.Limit != 1 {
		t.Errorf("Limit = %d, want 1", payload.Limit)
	}
	if payload.InFlight != 1 {
		t.Errorf("InFlight = %d, want 1", payload.InFlight)
	}
}
