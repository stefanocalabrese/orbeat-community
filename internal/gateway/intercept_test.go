package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/govern"
	"github.com/stefanocalabrese/orbeat-community/internal/secrets"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// newInterceptTestFixture builds a real-store-backed Server and a session
// entitled to every tool on "alpha" -- mirrors
// TestRBACMiddlewareFailsClosedOnUnexpectedParamsType's fixture
// (rbac_middleware_test.go): interceptArguments's audit path
// (auditCallFinding, intercept.go) writes through the real store exactly
// like auditDecision's does, so a stub-only Server cannot exercise it.
func newInterceptTestFixture(t *testing.T, ctx context.Context, namePrefix string) (*Server, *session) {
	t.Helper()
	st, err := store.New(ctx, gwDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	tn, err := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("%s-%d", namePrefix, time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}

	gw := New(st, authz.NewResolver(st, tn.Name), nil, secrets.NewResolver(), "http://gw.test", "http://kc.test/realms/orbeat")
	t.Cleanup(gw.Close)

	sess := &session{
		subject: "kc-intercept", tenantID: tn.ID, actor: "kc-intercept",
		entitlements: []store.Entitlement{{MCPServerID: "srv-1", AllowedTools: nil}},
		slugToServer: map[string]string{"alpha": "srv-1"},
	}
	return gw, sess
}

// callToolRequest builds the *mcp.ServerRequest[*mcp.CallToolParamsRaw]
// shape rbacMiddleware type-asserts req.GetParams() into -- the same
// construction TestRBACMiddlewareFailsClosedOnUnexpectedParamsType uses.
func callToolRequest(name, args string) mcp.Request {
	return &mcp.ServerRequest[*mcp.CallToolParamsRaw]{Params: &mcp.CallToolParamsRaw{Name: name, Arguments: []byte(args)}}
}

// TestRBACMiddlewareInterceptBlocksSecretInArgumentsBeforeUpstream is Task
// 2's gate (a)+(c) fixture in one test, on purpose: a blocking finding in
// the arguments must (1) actually deny the call via toolDeniedError --
// proving the hook's verdict is not ignored (mutant a) -- AND (2) never let
// the fake upstream ("next") run -- proving the scan happens BEFORE the
// call is proxied, not after with the result merely discarded (mutant c).
// A test that only checks the response is denied cannot tell those two
// apart; asserting upstreamCalled as a SEPARATE fact is what makes them
// distinguishable failures.
func TestRBACMiddlewareInterceptBlocksSecretInArgumentsBeforeUpstream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	gw, sess := newInterceptTestFixture(t, ctx, "intercept-block")
	gw.SetInterceptor(govern.NewDefaultScanner())

	upstreamCalled := false
	next := func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		upstreamCalled = true
		return nil, nil
	}
	handler := gw.rbacMiddleware(sess)(next)

	_, err := handler(ctx, "tools/call", callToolRequest("alpha__anything", `{"token":"AKIAIOSFODNN7EXAMPLE"}`))

	// fable-audit B38(c): a content-governance block must be distinguishable
	// from an RBAC "not entitled" denial (toolDeniedError) -- this asserts
	// the SPECIFIC type, not just "any error", so a regression back to the
	// shared toolDeniedError would fail here even though the call is still
	// denied either way.
	var denied *interceptDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("want an interceptDeniedError for arguments carrying a secret, got %v", err)
	}
	if upstreamCalled {
		t.Fatal("upstream must not be reached when the arguments are blocked -- the scan must run BEFORE the call is proxied, not after with the result discarded")
	}

	evs, err := gw.store.ListAuditEventsByTenant(ctx, sess.tenantID, 10)
	if err != nil {
		t.Fatalf("list audit events: %v", err)
	}
	found := false
	for _, e := range evs {
		if e.Action == "gateway.call.blocked" && e.Decision == "deny" && e.Metadata["rule"] == "secret" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want a gateway.call.blocked audit row with rule=secret, got %+v", evs)
	}
}

// TestRBACMiddlewareInterceptWarnPassesThroughAndAudits is Task 2's gate
// (b): an oversized argument payload trips govern's non-blocking "size"
// rule (reused unchanged from ArtifactPayload scanning via ScanCall,
// internal/govern/call.go). A warn finding must NOT deny the call.
func TestRBACMiddlewareInterceptWarnPassesThroughAndAudits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	gw, sess := newInterceptTestFixture(t, ctx, "intercept-warn")
	gw.SetInterceptor(govern.NewDefaultScanner())

	upstreamCalled := false
	next := func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		upstreamCalled = true
		return nil, nil
	}
	handler := gw.rbacMiddleware(sess)(next)

	bigArgs := strings.Repeat("x", 64*1024+1)
	_, err := handler(ctx, "tools/call", callToolRequest("alpha__anything", bigArgs))
	if err != nil {
		t.Fatalf("a warn finding must not deny the call, got %v", err)
	}
	if !upstreamCalled {
		t.Fatal("a warn finding must still let the call reach the upstream")
	}

	evs, err := gw.store.ListAuditEventsByTenant(ctx, sess.tenantID, 10)
	if err != nil {
		t.Fatalf("list audit events: %v", err)
	}
	found := false
	for _, e := range evs {
		if e.Action == "gateway.call.flagged" && e.Decision == "allow" && e.Metadata["rule"] == "size" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want a gateway.call.flagged audit row with rule=size, got %+v", evs)
	}
}

// TestRBACMiddlewareInterceptDisabledIsUnchanged proves the off path: with
// no interceptor configured (New's default -- s.interceptor stays nil), a
// call carrying a secret in its arguments proceeds exactly as it did before
// this slice -- no scan, no denial, no behaviour change.
func TestRBACMiddlewareInterceptDisabledIsUnchanged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	gw, sess := newInterceptTestFixture(t, ctx, "intercept-off")
	// gw.interceptor is left at its nil zero value -- interceptArguments's
	// first line must return nil without scanning anything.

	upstreamCalled := false
	next := func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		upstreamCalled = true
		return nil, nil
	}
	handler := gw.rbacMiddleware(sess)(next)

	_, err := handler(ctx, "tools/call", callToolRequest("alpha__anything", `{"token":"AKIAIOSFODNN7EXAMPLE"}`))
	if err != nil {
		t.Fatalf("interception disabled: call must proceed unchanged, got %v", err)
	}
	if !upstreamCalled {
		t.Fatal("interception disabled: upstream must be reached")
	}
}

// newInterceptResultFixture wires newInterceptTestFixture's real-store-backed
// Server + session to a live upstream (newUpstreamFixture's "echo" tool) via
// connectUpstream + registerProxies, then connects an in-memory MCP client to
// the resulting gw.mcpServer -- mirrors TestRegisterProxiesForwardsCall's
// setup, extended to thread s/sess through registerProxies so
// interceptResult (intercept.go) is exercised for real, exactly as
// buildSession wires it in production (server.go). Returns a client session
// callers use to invoke "alpha__echo" and inspect what comes back.
func newInterceptResultFixture(t *testing.T, ctx context.Context, namePrefix string) (*Server, *session, *mcp.ClientSession) {
	t.Helper()
	return newInterceptResultFixtureWithUpstream(t, ctx, namePrefix, newUpstreamFixture(t))
}

// newInterceptResultFixtureWithUpstream is newInterceptResultFixture against a
// caller-supplied upstream, so a test can choose a fixture whose results carry
// more than Content alone (newStructuredUpstreamFixture).
func newInterceptResultFixtureWithUpstream(t *testing.T, ctx context.Context, namePrefix string, up *upstreamFixture) (*Server, *session, *mcp.ClientSession) {
	t.Helper()
	gw, sess := newInterceptTestFixture(t, ctx, namePrefix)
	gw.SetInterceptor(govern.NewDefaultScanner())

	srv := store.MCPServer{ID: "srv-1", Name: "alpha", Transport: "http", EndpointOrCommand: up.URL, Status: "active"}
	conn, err := connectUpstream(ctx, srv, "", "", 0)
	if err != nil {
		t.Fatalf("connectUpstream: %v", err)
	}
	t.Cleanup(func() { _ = conn.session.Close() })

	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "orbeat-gateway", Version: "test"}, nil)
	if _, err := registerProxies(ctx, mcpServer, conn, func() {}, nil, gw, sess); err != nil {
		t.Fatalf("registerProxies: %v", err)
	}

	st, ct := mcp.NewInMemoryTransports()
	if _, err := mcpServer.Connect(ctx, st, nil); err != nil {
		t.Fatalf("mcpServer.Connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	return gw, sess, cs
}

// TestRegisterProxiesResultInterceptBlocksSecretAndAudits is Task 3's
// blocking gate: a secret in the UPSTREAM'S RESULT -- registerProxies never
// sees or scans arguments itself, that is interceptArguments's job one layer
// up in rbacMiddleware, so the "alpha__echo" upstream here simply echoes the
// secret straight back as its result -- must be replaced with a refusal
// before the client ever sees it (mutant: the result passes through
// unmodified when it should be replaced), and the audit row recording the
// block must NEVER carry the secret itself (mutant: the audit row carries
// the scanned content). The content the scanner exists to find is precisely
// what must never be copied into the audit table -- proven by scanning EVERY
// audit metadata value for the literal secret string, not by trusting the
// code not to have put it there.
func TestRegisterProxiesResultInterceptBlocksSecretAndAudits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	gw, sess, cs := newInterceptResultFixture(t, ctx, "intercept-result-block")

	const secret = "AKIAIOSFODNN7EXAMPLE"
	argsJSON, err := json.Marshal(map[string]string{"text": secret})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	out, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "alpha__echo", Arguments: json.RawMessage(argsJSON)})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	// Gate (a): the result must NOT pass through unmodified.
	for _, c := range out.Content {
		if tc, ok := c.(*mcp.TextContent); ok && strings.Contains(tc.Text, secret) {
			t.Fatalf("result content still carries the secret verbatim: %+v -- a blocking finding must replace it", out.Content)
		}
	}
	if len(out.Content) == 0 {
		t.Fatal("a blocked result must still carry a refusal, got empty content")
	}

	evs, err := gw.store.ListAuditEventsByTenant(ctx, sess.tenantID, 10)
	if err != nil {
		t.Fatalf("list audit events: %v", err)
	}
	found := false
	for _, e := range evs {
		if e.Action == "gateway.call.blocked" && e.Decision == "deny" && e.Metadata["rule"] == "secret" {
			found = true
		}
		// Gate (b): no audit metadata value may carry the scanned secret.
		for k, v := range e.Metadata {
			if sv, ok := v.(string); ok && strings.Contains(sv, secret) {
				t.Fatalf("audit metadata key %q leaked the scanned secret: %+v", k, e.Metadata)
			}
		}
	}
	if !found {
		t.Fatalf("want a gateway.call.blocked audit row with rule=secret, got %+v", evs)
	}
}

// TestRegisterProxiesResultInterceptWarnPassesThroughAndAudits is Task 3's
// gate (c): a warn-severity finding on the RESULT -- the same non-blocking
// "size" rule Task 2's argument-side warn test trips, reused unchanged via
// ScanCall -- must NOT replace the content. Only "block" severity does.
func TestRegisterProxiesResultInterceptWarnPassesThroughAndAudits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	gw, sess, cs := newInterceptResultFixture(t, ctx, "intercept-result-warn")

	bigText := strings.Repeat("x", 64*1024+1)
	argsJSON, err := json.Marshal(map[string]string{"text": bigText})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	out, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "alpha__echo", Arguments: json.RawMessage(argsJSON)})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	passed := false
	for _, c := range out.Content {
		if tc, ok := c.(*mcp.TextContent); ok && tc.Text == bigText {
			passed = true
		}
	}
	if !passed {
		t.Fatal("a warn finding must not replace the result content")
	}

	evs, err := gw.store.ListAuditEventsByTenant(ctx, sess.tenantID, 10)
	if err != nil {
		t.Fatalf("list audit events: %v", err)
	}
	found := false
	for _, e := range evs {
		if e.Action == "gateway.call.flagged" && e.Decision == "allow" && e.Metadata["rule"] == "size" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want a gateway.call.flagged audit row with rule=size, got %+v", evs)
	}
}

// TestRegisterProxiesResultInterceptWithholdsWholeResult is the gate for the
// comment-sweep Bug 1 fix (docs/plans/orbeat-comment-sweep-fixes-2026-08-28.md):
// a blocking finding must withhold the WHOLE mcp.CallToolResult, not just its
// Content field. The upstream here returns the same secret three times over,
// in the three places a CallToolResult can carry one -- Content,
// StructuredContent and _meta -- because a fixture returning Content alone
// cannot distinguish an interceptor that governs the result from one that
// governs res.Content and lets the rest ride through in the same response.
//
// The decisive assertion is on the MARSHALLED result: asserting only that
// Content was replaced passes on the bug, since Content was always replaced.
// IsError is asserted separately: a refusal reported with isError false tells
// the agent the call succeeded and the refusal text is the tool's real output.
func TestRegisterProxiesResultInterceptWithholdsWholeResult(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	up := newStructuredUpstreamFixture(t)
	gw, sess, cs := newInterceptResultFixtureWithUpstream(t, ctx, "intercept-result-whole", up)

	const secret = "AKIAIOSFODNN7EXAMPLE"
	argsJSON, err := json.Marshal(structuredEchoArgs{Text: secret, Payload: secret, MetaValue: secret})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	out, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "alpha__structured_echo", Arguments: json.RawMessage(argsJSON)})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	// The whole result, exactly as it reached the client. Nothing in it may
	// carry the secret: not content, not structuredContent, not _meta.
	wire, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if strings.Contains(string(wire), secret) {
		t.Fatalf("the blocked result still carries the secret somewhere on the wire: %s", wire)
	}
	if out.StructuredContent != nil {
		t.Fatalf("structuredContent must be cleared on a blocked result, got %#v", out.StructuredContent)
	}
	// Only the UPSTREAM's _meta is the interceptor's to clear: the gateway's
	// own SDK server re-annotates every outgoing result with
	// mcp.MetaKeyServerInfo (go-sdk mcp/server.go annotateServerInfo), which
	// runs after the proxy handler returns and carries orbeat's own bounded
	// Implementation, never upstream bytes. Asserting an empty _meta would
	// therefore be asserting something the interceptor cannot deliver.
	if _, ok := out.Meta["upstreamNote"]; ok {
		t.Fatalf("upstream-supplied _meta must be cleared on a blocked result, got %#v", out.Meta)
	}
	if !out.IsError {
		t.Fatal("a withheld result must set isError -- leaving it false tells the agent the call succeeded and the refusal text is the tool's real output")
	}
	if len(out.Content) == 0 {
		t.Fatal("a blocked result must still carry a refusal, got empty content")
	}

	evs, err := gw.store.ListAuditEventsByTenant(ctx, sess.tenantID, 10)
	if err != nil {
		t.Fatalf("list audit events: %v", err)
	}
	found := false
	for _, e := range evs {
		if e.Action == "gateway.call.blocked" && e.Decision == "deny" && e.Metadata["rule"] == "secret" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want a gateway.call.blocked audit row with rule=secret, got %+v", evs)
	}
}

// TestRegisterProxiesResultInterceptScansStructuredContent is the other half
// of the Bug 1 decision: structuredContent is SCANNED, not merely cleared once
// something else in the result trips a rule. The upstream's Content is
// innocuous here and the secret lives only in the structured payload, so a
// scan that reads Content alone finds nothing, blocks nothing, and hands the
// agent the secret. Clearing-without-scanning would pass the sibling test
// above and fail this one, which is why the two are separate tests.
func TestRegisterProxiesResultInterceptScansStructuredContent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	up := newStructuredUpstreamFixture(t)
	gw, sess, cs := newInterceptResultFixtureWithUpstream(t, ctx, "intercept-result-structured", up)

	const secret = "AKIAIOSFODNN7EXAMPLE"
	const innocuous = "nothing to see in the unstructured content"
	argsJSON, err := json.Marshal(structuredEchoArgs{Text: innocuous, Payload: secret})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	out, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "alpha__structured_echo", Arguments: json.RawMessage(argsJSON)})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	wire, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if strings.Contains(string(wire), secret) {
		t.Fatalf("a secret carried ONLY in structuredContent reached the client: %s", wire)
	}
	for _, c := range out.Content {
		if tc, ok := c.(*mcp.TextContent); ok && tc.Text == innocuous {
			t.Fatal("the result passed through unblocked -- structuredContent was never scanned, only Content was")
		}
	}
	if !out.IsError {
		t.Fatal("a withheld result must set isError")
	}

	evs, err := gw.store.ListAuditEventsByTenant(ctx, sess.tenantID, 10)
	if err != nil {
		t.Fatalf("list audit events: %v", err)
	}
	found := false
	for _, e := range evs {
		if e.Action == "gateway.call.blocked" && e.Decision == "deny" && e.Metadata["rule"] == "secret" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want a gateway.call.blocked audit row with rule=secret from the structured payload, got %+v", evs)
	}
}
