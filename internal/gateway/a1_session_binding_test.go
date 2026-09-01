package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/logging"
	"github.com/stefanocalabrese/orbeat-community/internal/secrets"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// ============================================================================
// A1 — the gateway session and the SDK transport session ARE now bound to
// each other, and these two tests are the gate for that binding. Unit 1 wrote
// them asserting the BROKEN behaviour, guarded behind an env var, as a
// red-proof; Unit 2 inverted THREE of the four load-bearing assertions and
// removed the guard, so they now run in the default matrix and state the
// REQUIREMENT. The fourth, (A1) below, kept its polarity deliberately -- see
// HOW THESE WERE PROVEN.
//
// THE DEFECT THEY PIN. orbeat-gateway kept two independent session lifetimes:
//
//  1. The gateway session (session.go), keyed by subject, holding the
//     resolved entitlements, slugToServer, the *mcp.Server and every live
//     upstream ClientSession. It is evicted when dirty, when idle past
//     sessionTTL, when builtAt is older than sessionMaxAge (absolute,
//     regardless of activity) and by invalidateTenant on the
//     entitlement-change nudge. Eviction calls session.close(), which closes
//     every upstream connection the session holds.
//
//  2. The SDK transport session inside mcp.StreamableHTTPHandler, addressed
//     by the Mcp-Session-Id header and reclaimed only by its own IDLE
//     SessionTimeout (Handler(), server.go).
//
// In go-sdk v1.7.0 (mcp/streamable.go:633-653) serveStatefulPOST calls
// h.getServer(req) ONLY for a request that carries no Mcp-Session-Id. Every
// later POST/GET/DELETE goes through lookupSession straight to
// sessInfo.transport.ServeHTTP. So after initialize, Server.getServer is
// never consulted again: the *mcp.Server the client talks to -- with
// s.rbacMiddleware(sess) closed over a snapshot (server.go) and the proxy
// closures registered against that snapshot's upstream connections
// (registerProxies, broker.go) -- is frozen for the life of the transport
// session, no matter what happens to the gateway session behind it.
//
// Note sessionTTL = 2 * sessionMaxAge (server.go), so in production only the
// max-age branch can ever fire, and max-age is absolute while SessionTimeout
// is idle: an ACTIVE client always hits gateway-session eviction first, and
// therefore always lands in exactly the state these tests set up.
//
// THE FIX THESE NOW GATE (Unit 2). sessionCache carries an Mcp-Session-Id ->
// *session index; withSession binds the id the SDK mints on initialize, and
// answers any later request whose binding no longer points at the subject's
// CURRENT session with 404 + X-Orbeat-Session-Rebuilt, before the request can
// reach the frozen transport. The SDK client maps 404 to mcp.ErrSessionMissing
// (streamable.go:2533-2539, quoting spec §2.5.3 inline), so the client is told
// to start a new session instead of being stranded on a dead one.
//
// WHY THIS SURVIVED. The closest existing test,
// TestGatewayMaxAgeRebuildBoundsRevocation (gateway_integration_test.go),
// asserts the right property but drives gw.sessions.getOrBuild DIRECTLY and
// then talks to the REBUILT session over a fresh in-memory transport. It
// never asks what the already-connected client sees. These two tests go
// through a live MCP client over real HTTP, which is the only vantage point
// from which the defect is visible at all.
// ============================================================================

// HOW THESE WERE PROVEN, AND WHERE THE PROOF STOPS. Unit 1 wrote the
// load-bearing assertions the other way round -- stating the defect -- and
// Unit 2 inverted three of them: (A2) the identity of the post-rebuild error,
// (b.1) the tool listing, and (b.2) the audit delta. Each of those three
// discriminates a fixed gateway from a broken one, and each was red-proved on
// 2026-08-29 by inverting it under `go test -overlay` and watching that ONE
// test fail, separately rather than as a batch, because a single proof passes
// when only one of several assertions is discriminating. The
// broken-behaviour client error was: calling "tools/call": connection closed:
// calling "tools/call": client is closing.
//
// (A1) IS THE EXCEPTION, and this block used to say all four were inverted
// and each independently red-proved. It was neither. (A1) kept its polarity
// through Unit 2, and it cannot discriminate at all, for the reason its own
// comment already concedes: the call fails on BOTH sides of the fix, only the
// error's identity changes. It is a guard against a silent SUCCESS -- a
// gateway serving a request from a session it had already reclaimed -- and
// not evidence that the binding works. (A2), immediately below it, is what
// carries that.
//
// Unit 2 re-proved the pair the other way round as well: with the 404 branch
// removed from withSession, both tests must fail. That output is in Unit 2's
// report.

// a1Fixture builds the shared state both A1 tests need: a private tenant, an
// orbeat-user role, one reachable HTTP upstream exposing "echo", and an
// entitlement granting it. It deliberately does NOT reuse ratelimitTestFixture
// (ratelimit_test.go), which returns neither the entitlement id nor the server
// id -- Test B has to DELETE the entitlement by id, and both tests need the
// principal and the fixture handle to reason about upstream liveness.
//
// The returned Server is NOT yet serving: the caller wires httptest and the
// t.Cleanup ordering itself, because that ordering is load-bearing (gw.Close
// must run before the upstream fixture's httptest.Server.Close, or an active
// upstream connection blocks teardown -- see the note in
// TestGatewayVerticalEntitledAndDenied).
// a1SecondToken is the bearer token a1Fixture's verifier accepts for a SECOND
// principal in the same tenant and role, whose Principal is a1SecondPrincipal.
// It exists for the cross-subject replay gate, which needs two callers whose
// gateway sessions both build cleanly and differ only in subject. Registering
// it costs the other tests nothing: nothing enumerates the verifier's map, and
// no session is built for a token that is never presented.
const a1SecondToken = "a1-tok-2"

// a1SecondPrincipal is the Principal behind a1SecondToken, derived from the
// same fixture name so a test can name the subject it expects in a log line
// without a1Fixture having to return it.
func a1SecondPrincipal(name string) auth.Principal {
	return auth.Principal{Subject: "kc-" + name + "-2", Email: name + "-2@x.io", Roles: []string{"orbeat-user"}}
}

func a1Fixture(t *testing.T, ctx context.Context, name string) (gw *Server, st *store.Store, tenantID string, p auth.Principal, entID string) {
	t.Helper()

	st, err := store.New(ctx, gwDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	up := newUpstreamFixture(t)
	tn, err := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("%s-%d", name, time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	role, err := st.CreateRole(ctx, tn.ID, "orbeat-user")
	if err != nil {
		t.Fatalf("role: %v", err)
	}
	srv, err := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: "fixture", Transport: "http",
		EndpointOrCommand: up.URL, Status: "active",
	})
	if err != nil {
		t.Fatalf("mcp server: %v", err)
	}
	ent, err := st.CreateEntitlement(ctx, store.Entitlement{
		TenantID: tn.ID, RoleID: role.ID, MCPServerID: srv.ID, AllowedTools: []string{"echo"},
	})
	if err != nil {
		t.Fatalf("entitlement: %v", err)
	}

	p = auth.Principal{Subject: "kc-" + name, Email: name + "@x.io", Roles: []string{"orbeat-user"}}
	verifier := stubVerifier(map[string]auth.Principal{"a1-tok": p, a1SecondToken: a1SecondPrincipal(name)})
	gw = New(st, authz.NewResolver(st, tn.Name), verifier, secrets.NewResolver(), "http://gw.test", "http://kc.test/realms/orbeat")
	return gw, st, tn.ID, p, ent.ID
}

// a1ForceRebuild evicts the subject's cached gateway session and rebuilds it,
// exactly as sessionMaxAge does in production, and returns the new session.
//
// getOrBuild with a now past sessionMaxAge is used rather than
// invalidateTenant on purpose: get() closes the evicted session's upstreams
// SYNCHRONOUSLY on the calling goroutine, while invalidateTenant closes them
// in `go s.close()`. Either reproduces the defect, but only the synchronous
// one makes the test's next assertion race-free.
func a1ForceRebuild(t *testing.T, ctx context.Context, gw *Server, p auth.Principal) (before, after *session) {
	t.Helper()
	before, ok := gw.sessions.get(p.Subject, time.Now())
	if !ok {
		t.Fatal("expected a cached gateway session for the connected client")
	}
	build := func() (*session, error) { return gw.buildSession(ctx, p) }
	after, err := gw.sessions.getOrBuild(p.Subject, time.Now().Add(sessionMaxAge+time.Second), build)
	if err != nil {
		t.Fatalf("forced max-age rebuild: %v", err)
	}
	if after == before {
		t.Fatal("session past sessionMaxAge must be rebuilt, got the cached one back")
	}
	return before, after
}

// countGatewayAllows returns how many gateway.tool.call audit rows with
// decision "allow" the tenant currently has. The limit is generous relative
// to what these tests generate; a cap reached would understate the count, so
// each test asserts on a DELTA it produced rather than on an absolute.
func countGatewayAllows(t *testing.T, ctx context.Context, st *store.Store, tenantID string) int {
	t.Helper()
	evs, err := st.ListAuditEventsByTenant(ctx, tenantID, 200)
	if err != nil {
		t.Fatalf("list audit events: %v", err)
	}
	n := 0
	for _, e := range evs {
		if e.Action == "gateway.tool.call" && e.Decision == "allow" {
			n++
		}
	}
	return n
}

// TestA1StaleTransportSessionBreaksAnActiveClient is the AVAILABILITY half of
// A1 (consequence (a)).
//
// sessionMaxAge is ABSOLUTE from builtAt, while the SDK transport's
// SessionTimeout is IDLE -- so an active client always hits gateway-session
// eviction first. Eviction closes the upstream connections the still-live
// transport session's proxy closures hold. Before the fix the client's MCP
// session was never terminated, so it had no reason to reconnect and every
// subsequent tool call failed permanently against connections nobody would
// reopen. After it, the gateway answers the stale Mcp-Session-Id with the 404
// the spec reserves for a terminated session, which the SDK client surfaces as
// mcp.ErrSessionMissing -- an outcome a client can act on.
//
// The call still FAILS either way, which is why the error's identity, not its
// presence, is the assertion that matters here.
//
// The fresh-connection assertion at the end is what isolates the failure to
// the STALE TRANSPORT SESSION rather than to the gateway being broken: the
// rebuilt gateway session serves a brand-new client perfectly, at the same
// instant the old one is dead.
func TestA1StaleTransportSessionBreaksAnActiveClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	gw, _, _, p, _ := a1Fixture(t, ctx, "a1-availability")
	httpSrv := httptest.NewServer(gw.Handler())
	t.Cleanup(httpSrv.Close)
	t.Cleanup(gw.Close) // LIFO: before httpSrv.Close and before the upstream fixture's close.

	cs := connectThroughGateway(t, ctx, httpSrv.URL+"/mcp", "a1-tok")
	t.Cleanup(func() { _ = cs.Close() })

	out, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "fixture__echo", Arguments: json.RawMessage(`{"text":"before"}`)})
	if err != nil {
		t.Fatalf("first CallTool (the client is healthy at this point): %v", err)
	}
	if tc, ok := out.Content[0].(*mcp.TextContent); !ok || tc.Text != "before" {
		t.Fatalf("first echo result = %+v, want text %q", out.Content, "before")
	}

	before, _ := a1ForceRebuild(t, ctx, gw, p)

	// The evicted session's upstream really is closed -- this is the mechanism
	// the failure below rides on, asserted rather than assumed.
	if err := before.upstreams[0].session.Ping(ctx, nil); !errors.Is(err, mcp.ErrConnectionClosed) {
		t.Fatalf("evicted session's upstream Ping err = %v, want %v", err, mcp.ErrConnectionClosed)
	}

	// The client never learned anything happened: same transport session, same
	// Mcp-Session-Id, same frozen *mcp.Server behind it.
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "fixture__echo", Arguments: json.RawMessage(`{"text":"after"}`)})
	t.Logf("A1 availability: post-rebuild CallTool on the ORIGINAL session -> err=%v result=%+v", err, res)

	// (A1) The call must still fail -- the state it was built on is gone, and
	// silently succeeding would mean the gateway served a request from a
	// reclaimed session. res is nil on both sides of the fix; the SDK does not
	// degrade either failure to a CallToolResult with IsError set.
	if err == nil {
		t.Fatalf("post-rebuild CallTool unexpectedly succeeded (result=%+v); the stale transport session was served instead of rejected", res)
	}
	// (A2) And it must fail as a TERMINATED SESSION, not as a dead upstream
	// connection. This is the assertion the whole fix exists for: withSession
	// 404s the stale Mcp-Session-Id, the SDK client maps that to
	// mcp.ErrSessionMissing, and the client can reconnect. Before the fix the
	// error was `calling "tools/call": connection closed: ... client is
	// closing` -- a transport failure carrying no instruction, so nothing on
	// the wire ever told the client its session was finished.
	if !errors.Is(err, mcp.ErrSessionMissing) {
		t.Fatalf("post-rebuild CallTool err = %v, want an error wrapping mcp.ErrSessionMissing (the gateway must 404 a transport session whose gateway session was rebuilt)", err)
	}

	// Isolation: a client connecting NOW works immediately, because initialize
	// carries no Mcp-Session-Id and is therefore the one request that reaches
	// Server.getServer and picks up the rebuilt session. The gateway is fine;
	// only the already-connected client is stranded. This assertion is expected
	// to keep passing unchanged after the fix.
	fresh := connectThroughGateway(t, ctx, httpSrv.URL+"/mcp", "a1-tok")
	t.Cleanup(func() { _ = fresh.Close() })
	freshOut, err := fresh.CallTool(ctx, &mcp.CallToolParams{Name: "fixture__echo", Arguments: json.RawMessage(`{"text":"fresh"}`)})
	if err != nil {
		t.Fatalf("fresh connection after the rebuild must work (the gateway is healthy): %v", err)
	}
	if tc, ok := freshOut.Content[0].(*mcp.TextContent); !ok || tc.Text != "fresh" {
		t.Fatalf("fresh echo result = %+v, want text %q", freshOut.Content, "fresh")
	}
}

// TestA1StaleTransportSessionServesRevokedEntitlements is the GOVERNANCE half
// of A1 (consequence (b)).
//
// The caller's ONLY entitlement is deleted and the gateway session is rebuilt
// against that deletion -- the rebuilt session holds zero entitlements and
// zero upstreams. Before the fix, a client that stayed on its original
// transport session was still talking to the PRE-revocation *mcp.Server, so:
//
//   - tools/list still advertises fixture__echo. The proxies were registered
//     on that server's own tool registry at build time (registerProxies,
//     broker.go) and rbacMiddleware passes every non-tools/call method
//     through untouched, so the listing needs no upstream and no live grant.
//
//   - tools/call is still AUTHORIZED: rbacMiddleware is closed over the
//     pre-revocation snapshot, so toolCallAuthorization consults the deleted
//     entitlement and auditDecision writes decision="allow" (rbac_middleware.go)
//     BEFORE the call is forwarded. orbeat's per-call RBAC enforcement point
//     -- one of the three this product documents -- answers "allow" for a
//     grant that no longer exists.
//
// After the fix neither happens: the stale Mcp-Session-Id is 404-ed in
// withSession, so tools/list advertises nothing and tools/call never reaches
// rbacMiddleware, leaving the audit table untouched.
//
// SCOPE NOTE, deliberate and worth reading before changing these assertions:
// the call cannot also be asserted to RETURN THE UPSTREAM'S PAYLOAD, because
// consequence (a) gets there first. Any rebuild path -- max-age, idle, dirty,
// invalidateTenant -- runs session.close(), which closes the old session's
// upstream connections, so the forwarded half of this call dies on a dead
// connection whatever RBAC decided. The governance failure is the ALLOW
// DECISION and the tool listing, both of which happen before that and are
// what this test pins. A future test asserting a successful payload here
// would be asserting that (a) is unfixed, which is the wrong gate.
func TestA1StaleTransportSessionServesRevokedEntitlements(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	gw, st, tenantID, p, entID := a1Fixture(t, ctx, "a1-governance")
	httpSrv := httptest.NewServer(gw.Handler())
	t.Cleanup(httpSrv.Close)
	t.Cleanup(gw.Close)

	cs := connectThroughGateway(t, ctx, httpSrv.URL+"/mcp", "a1-tok")
	t.Cleanup(func() { _ = cs.Close() })

	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "fixture__echo", Arguments: json.RawMessage(`{"text":"entitled"}`)}); err != nil {
		t.Fatalf("first CallTool while genuinely entitled: %v", err)
	}

	// Revoke the caller's ONLY grant, then force the rebuild that is supposed
	// to make the revocation take effect.
	if err := st.DeleteEntitlement(ctx, tenantID, entID); err != nil {
		t.Fatalf("delete entitlement: %v", err)
	}
	_, after := a1ForceRebuild(t, ctx, gw, p)
	if len(after.entitlements) != 0 {
		t.Fatalf("rebuilt session entitlements = %d, want 0 (the grant was deleted)", len(after.entitlements))
	}
	if len(after.upstreams) != 0 {
		t.Fatalf("rebuilt session upstreams = %d, want 0 (the grant was deleted)", len(after.upstreams))
	}

	allowsBefore := countGatewayAllows(t, ctx, st, tenantID)

	// (b.1) The de-entitled caller must not be able to SEE the tool. The
	// listing needs neither a live upstream nor a live grant -- the proxies sit
	// on the frozen server's own tool registry and rbacMiddleware passes every
	// non-tools/call method through untouched -- so the ONLY thing that can
	// stop it is the transport session being rejected outright.
	tools, err := cs.ListTools(ctx, nil)
	var listed []string
	if tools != nil {
		for _, tl := range tools.Tools {
			listed = append(listed, tl.Name)
		}
	}
	t.Logf("A1 governance: post-revocation tools/list on the ORIGINAL session -> err=%v tools=%v", err, listed)
	if !errors.Is(err, mcp.ErrSessionMissing) {
		t.Fatalf("post-revocation ListTools err = %v (tools=%v), want an error wrapping mcp.ErrSessionMissing", err, listed)
	}
	if len(listed) != 0 {
		t.Fatalf("post-revocation tools/list advertised %v, want nothing", listed)
	}

	// (b.2) The de-entitled caller must not be AUTHORIZED either. Before the
	// fix rbacMiddleware consulted the pre-revocation snapshot and auditDecision
	// wrote decision="allow" for a grant that no longer existed -- orbeat's
	// per-call RBAC enforcement point answering "allow" for nothing.
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "fixture__echo", Arguments: json.RawMessage(`{"text":"revoked"}`)})
	t.Logf("A1 governance: post-revocation CallTool on the ORIGINAL session -> err=%v result=%+v", err, res)

	allowsAfter := countGatewayAllows(t, ctx, st, tenantID)
	// The call is refused at the transport-session check, so it never reaches
	// rbacMiddleware and the audit table does not move. Asserting on the DELTA
	// (not on zero) keeps the earlier legitimate call in this test out of the
	// way; asserting on the audit table rather than on the call's error is what
	// makes this a governance gate rather than a second copy of (A2).
	if allowsAfter != allowsBefore {
		t.Fatalf("gateway.tool.call allow rows went %d -> %d, want unchanged: a de-entitled caller was authorized on a stale transport session", allowsBefore, allowsAfter)
	}
}

// ============================================================================
// A1, ROUND TWO (2026-08-30) — the gates the code-quality review found missing
// or found gating the wrong thing.
// ============================================================================

// a1RawRequest replays an Mcp-Session-Id by hand against a live gateway. The
// SDK client is useless for this: it surfaces the error and never the response
// headers, and X-Orbeat-Session-Rebuilt is the only thing that tells the
// gateway's 404 apart from the SDK's own.
func a1RawRequest(t *testing.T, ctx context.Context, method, url, token, sid, body string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		t.Fatalf("build %s request: %v", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sid != "" {
		req.Header.Set(mcpSessionIDHeader, sid)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

// TestA1MintedSessionIDIsBoundBeforeTheClientCanUseIt gates the mint-time
// binding: buildSession installs a GetSessionID closure on the session's own
// *mcp.Server, and the SDK calls it inside serveStatefulPOST the statement
// after getServer -- before the transport is built and before the id is
// written into the response.
//
// WHY THIS GATE EXISTS AT ALL. The binding used to be taken by
// sessionIDCapturer, a response-writer wrapper that read the id back out of
// the headers at flush time. Review measured that removing that wrapper from
// the request path entirely left the whole internal/gateway package GREEN,
// because withSession's never-seen branch adopted the id on the client's very
// next request and nothing noticed. The only test that named the capturer
// asserted its Flusher plumbing, never that it bound anything.
//
// WHAT IT DISCRIMINATES, stated exactly rather than generously: it fails
// whenever the SDK mints an id that nothing binds -- the mutant being
// mcp.NewServer(gatewayImplementation(), nil), which silently falls back to
// the SDK's own rand.Text generator and leaves the index empty. It does NOT
// distinguish mint-time binding from a header-time capturer; nothing
// observable from outside the process can, since both are in place before the
// response headers reach the client. What makes the capturer non-viable is the
// OTHER half of this slice: withSession now refuses an id it holds no binding
// for, so a binding that depends on the client sending a second request is a
// binding that arrives one 404 too late.
func TestA1MintedSessionIDIsBoundBeforeTheClientCanUseIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	gw, _, _, p, _ := a1Fixture(t, ctx, "a1-mintbind")
	httpSrv := httptest.NewServer(gw.Handler())
	t.Cleanup(httpSrv.Close)
	t.Cleanup(gw.Close)

	// A raw initialize rather than connectThroughGateway: the id has to be read
	// off the RESPONSE, at the earliest instant a real client could have it.
	resp := a1RawRequest(t, ctx, http.MethodPost, httpSrv.URL+"/mcp", "a1-tok", "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"a1-raw","version":"0"}}}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize status = %d, want 200", resp.StatusCode)
	}
	sid := resp.Header.Get(mcpSessionIDHeader)
	if sid == "" {
		t.Fatal("initialize response carried no Mcp-Session-Id: the SDK was configured stateless or GetSessionID returned the empty string, and this gate cannot say anything")
	}

	sess, ok := gw.sessions.get(p.Subject, time.Now())
	if !ok {
		t.Fatal("no cached gateway session after initialize")
	}
	b := gw.sessions.lookupTransport(sid)
	if !b.known {
		t.Fatalf("Mcp-Session-Id %q reached the client while the gateway held NO binding for it; withSession refuses an unbound id, so this session is dead on its next request", sid)
	}
	if b.sess != sess {
		t.Fatalf("Mcp-Session-Id %q is bound to %p, want the subject's current session %p: the id must name the very session whose frozen *mcp.Server will serve it", sid, b.sess, sess)
	}
}

// TestA1UnknownMcpSessionIDIsRefusedNotAdopted gates the branch this slice
// deleted: withSession used to bind an inbound id it had never seen to the
// caller's CURRENT session and let the request through.
//
// THE STATED REASON FOR THAT BRANCH WAS FALSE. It claimed double-404-ing would
// "break every client after a gateway restart". Both 404s -- the gateway's and
// the SDK's own lookupSession -- reach the client as an error wrapping
// mcp.ErrSessionMissing and are indistinguishable client-side, so both make it
// re-initialize; and after a restart the SDK's session map is empty too, so
// the outcome is identical either way.
//
// Two assertions, failing for different reasons. The status+marker pair fails
// on the adopt-and-pass mutant because the SDK would then answer with its OWN
// unmarked 404. The index assertion fails on the same mutant one layer down
// and is the one that says WHY: a gateway that adopts a forged id has written
// an attacker-chosen key into its own binding table.
func TestA1UnknownMcpSessionIDIsRefusedNotAdopted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	gw, _, _, p, _ := a1Fixture(t, ctx, "a1-unknownid")
	httpSrv := httptest.NewServer(gw.Handler())
	t.Cleanup(httpSrv.Close)
	t.Cleanup(gw.Close)

	const forged = "forged-mcp-session-id-never-minted-here"
	resp := a1RawRequest(t, ctx, http.MethodPost, httpSrv.URL+"/mcp", "a1-tok", forged,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("forged Mcp-Session-Id got status %d (body %q), want 404", resp.StatusCode, body)
	}
	if marker := resp.Header.Get(sessionRebuiltHeader); marker != sessionUnbound {
		t.Fatalf("forged Mcp-Session-Id got 404 with %s=%q, want %q: an unmarked 404 is the SDK's, which means withSession adopted the id and passed the request on", sessionRebuiltHeader, marker, sessionUnbound)
	}
	if b := gw.sessions.lookupTransport(forged); b.known {
		t.Fatalf("the gateway BOUND a forged Mcp-Session-Id (sess=%p cause=%q); an id this process never minted must never enter the binding index", b.sess, b.cause)
	}
	// And the caller's own session is untouched: the refusal is per-request,
	// not a reason to tear down a healthy subject.
	if _, ok := gw.sessions.get(p.Subject, time.Now()); !ok {
		t.Fatal("refusing an unknown id evicted the caller's healthy gateway session")
	}
}

// TestA1ReplayAfterTombstoneExpiryCannotReachTheFrozenServer reproduces the
// exact reachable hole the review demonstrated, end to end, and is the
// strongest of these three: it is the only one whose mutant SUCCEEDS rather
// than merely 404-ing for the wrong reason.
//
// THE ATTACK, as the review laid it out. subscriptions/listen is in the SDK's
// default server method table, blocks on <-ctx.Done() whenever a subscription
// is agreed, and one is agreed whenever caps.Tools.ListChanged is set --
// which capabilities() sets for any server with tools. It passes untouched
// through rbacMiddleware, which gates tools/call alone, and startPOST stops the
// SDK's idle timer for the POST's whole duration while cmd/gateway sets only
// ReadHeaderTimeout. So a client could hold one POST open, outlive its own
// gateway session AND its tombstone, then replay the id on an ordinary POST
// and be adopted by the never-seen branch straight onto the frozen
// *mcp.Server.
//
// This test drives the CONSEQUENCE rather than the hanging POST: it evicts the
// gateway session, sweeps the tombstone past tombstoneHorizon, and replays the
// id. Holding a real POST open for 2*sessionMaxAge is not something a unit
// test can do, and it is not the mechanism under test -- the mechanism is what
// withSession does with an id it no longer has a binding for. The SDK's own
// transport session is deliberately left ALIVE (this Server's SessionTimeout
// is the production 5 minutes and the test runs in seconds), so the frozen
// *mcp.Server really is still there to be reached.
//
// The entitlement is revoked first so the mutant's success is unmistakable:
// with the never-seen branch restored, the replay is served by the
// pre-revocation server and the response carries fixture__echo, a tool the
// caller no longer has any grant for.
func TestA1ReplayAfterTombstoneExpiryCannotReachTheFrozenServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	gw, st, tenantID, p, entID := a1Fixture(t, ctx, "a1-sweptreplay")
	httpSrv := httptest.NewServer(gw.Handler())
	t.Cleanup(httpSrv.Close)
	t.Cleanup(gw.Close)

	cs := connectThroughGateway(t, ctx, httpSrv.URL+"/mcp", "a1-tok")
	t.Cleanup(func() { _ = cs.Close() })
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "fixture__echo", Arguments: json.RawMessage(`{"text":"entitled"}`)}); err != nil {
		t.Fatalf("first CallTool while genuinely entitled: %v", err)
	}
	sid := cs.ID()
	if sid == "" {
		t.Fatal("client session reported no Mcp-Session-Id")
	}

	if err := st.DeleteEntitlement(ctx, tenantID, entID); err != nil {
		t.Fatalf("delete entitlement: %v", err)
	}

	// Evict at a known instant so the sweep below can be placed relative to it
	// rather than relative to whatever a1ForceRebuild happened to choose.
	evictAt := time.Now().Add(sessionMaxAge + time.Second)
	after, err := gw.sessions.getOrBuild(p.Subject, evictAt, func() (*session, error) { return gw.buildSession(ctx, p) })
	if err != nil {
		t.Fatalf("forced max-age rebuild: %v", err)
	}
	if len(after.entitlements) != 0 {
		t.Fatalf("rebuilt session entitlements = %d, want 0", len(after.entitlements))
	}
	if b := gw.sessions.lookupTransport(sid); !b.known || b.cause != reclaimMaxAge {
		t.Fatalf("after eviction the id is known=%v cause=%q, want a max_age tombstone; the sweep below would otherwise be sweeping nothing", b.known, b.cause)
	}

	// Sweep it away. One reap past staleAt+tombstoneHorizon, with 2*maxAge
	// spelled out from the constant rather than read back from
	// tombstoneHorizon(), so shrinking the horizon cannot move this sweep with
	// it and keep the test green.
	gw.sessions.reap(evictAt.Add(2*sessionMaxAge + time.Second))
	if b := gw.sessions.lookupTransport(sid); b.known {
		t.Fatalf("the tombstone survived a sweep past 2*sessionMaxAge (known=%v cause=%q); this test is meant to exercise the FORGOTTEN-id path and is currently exercising the tombstone path instead", b.known, b.cause)
	}

	resp := a1RawRequest(t, ctx, http.MethodPost, httpSrv.URL+"/mcp", "a1-tok", sid,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("replay of a swept id got status %d, want 404. Body: %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "fixture__echo") {
		t.Fatalf("replay of a swept id was served by the FROZEN *mcp.Server: the response advertises fixture__echo, a tool whose entitlement was deleted. Body: %s", body)
	}
	if marker := resp.Header.Get(sessionRebuiltHeader); marker != sessionUnbound {
		t.Fatalf("replay of a swept id got %s=%q, want %q", sessionRebuiltHeader, marker, sessionUnbound)
	}
}

// TestA1StaleIDDeleteIsLetThroughAndTombstoneSurvives gates the one method
// exempted from the 404.
//
// serveStatefulDELETE is the ONLY mechanism that promptly reclaims an SDK
// transport session. While withSession 404-ed a stale-id DELETE, an evicted
// session's frozen *mcp.Server, its rbacMiddleware closure over the revoked
// entitlement snapshot and its []*upstreamConn stayed resident for the full
// SessionTimeout even when the client politely said goodbye -- the gateway
// refusing the one request that would have cleaned up after it.
//
// Two halves, and the second is what keeps the exemption from becoming a
// bypass: the DELETE must reach the SDK (204, no gateway marker), and the
// tombstone must SURVIVE it, so a POST replaying the same id afterwards is
// still refused with the original cause. A DELETE that consumed the tombstone
// would hand an attacker a two-request downgrade from "max_age" to "unbound"
// and, before this slice, from "refused" to "served".
func TestA1StaleIDDeleteIsLetThroughAndTombstoneSurvives(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	gw, _, _, p, _ := a1Fixture(t, ctx, "a1-staledelete")
	httpSrv := httptest.NewServer(gw.Handler())
	t.Cleanup(httpSrv.Close)
	t.Cleanup(gw.Close)

	cs := connectThroughGateway(t, ctx, httpSrv.URL+"/mcp", "a1-tok")
	t.Cleanup(func() { _ = cs.Close() })
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "fixture__echo", Arguments: json.RawMessage(`{"text":"before"}`)}); err != nil {
		t.Fatalf("first CallTool: %v", err)
	}
	sid := cs.ID()
	if sid == "" {
		t.Fatal("client session reported no Mcp-Session-Id")
	}

	a1ForceRebuild(t, ctx, gw, p)
	if b := gw.sessions.lookupTransport(sid); !b.known || b.cause != reclaimMaxAge {
		t.Fatalf("after the rebuild the id is known=%v cause=%q, want a max_age tombstone", b.known, b.cause)
	}

	del := a1RawRequest(t, ctx, http.MethodDelete, httpSrv.URL+"/mcp", "a1-tok", sid, "")
	defer del.Body.Close()
	delBody, _ := io.ReadAll(del.Body)
	if marker := del.Header.Get(sessionRebuiltHeader); marker != "" {
		t.Fatalf("stale-id DELETE was answered by withSession (%s=%q): it never reached serveStatefulDELETE, so the evicted session's frozen *mcp.Server, its rbacMiddleware closure over the revoked snapshot and its []*upstreamConn stay resident for the full SessionTimeout", sessionRebuiltHeader, marker)
	}
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("stale-id DELETE status = %d (body %q), want 204 from the SDK's serveStatefulDELETE", del.StatusCode, delBody)
	}

	// The tombstone is not consumed by the DELETE: a POST replaying the id is
	// still refused, and still with the reason the eviction recorded.
	replay := a1RawRequest(t, ctx, http.MethodPost, httpSrv.URL+"/mcp", "a1-tok", sid,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	defer replay.Body.Close()
	replayBody, _ := io.ReadAll(replay.Body)
	if replay.StatusCode != http.StatusNotFound {
		t.Fatalf("POST replay after the DELETE got status %d (body %q), want 404", replay.StatusCode, replayBody)
	}
	if marker := replay.Header.Get(sessionRebuiltHeader); marker != reclaimMaxAge {
		t.Fatalf("POST replay after the DELETE got %s=%q, want %q: letting DELETE through must not consume the tombstone", sessionRebuiltHeader, marker, reclaimMaxAge)
	}
}

// a1RejectionLog returns the fields of the ONE "mcp transport session
// rejected" line naming sid, and fails if there is not exactly one.
//
// Exactly one, rather than "the first one that matches": a gate scanning for
// any line carrying the expected cause would go green on a withSession that
// wrote the old line and a new one, which is the shape a careless fix to this
// very branch would take.
func a1RejectionLog(t *testing.T, buf *syncBuffer, sid string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatalf("log line not valid JSON: %v\nline: %s", err, line)
		}
		msg, _ := fields["msg"].(string)
		if !strings.HasPrefix(msg, "mcp transport session rejected") || fields["mcp_session_id"] != sid {
			continue
		}
		found = append(found, fields)
	}
	if len(found) != 1 {
		t.Fatalf("found %d rejection log lines for Mcp-Session-Id %q, want exactly 1; log:\n%s", len(found), sid, buf.String())
	}
	return found[0]
}

// TestA1CrossSubjectReplayIsLoggedAsAReplayNotAsARebuild gates the one shape in
// withSession's `case b.known:` branch where NOTHING was rebuilt: bob presents
// an Mcp-Session-Id minted for alice while alice's gateway session is alive and
// current.
//
// WHAT WAS WRONG. The branch collapsed all three of its shapes onto one cause
// and one message, so a review's two-principal probe produced
//
//	mcp transport session rejected: gateway session rebuilt subject=kc-bob cause=unknown
//
// while alice's session sat untouched in the cache. Nothing had been rebuilt and
// nothing was unknown to the gateway, and a replay of another subject's LIVE id
// read in the log exactly like the internal bind-after-eviction race
// reclaimUnknown names, on the one surface an operator has for telling them
// apart.
//
// THE THREE ASSERTIONS FAIL FOR THREE DIFFERENT REASONS, which is why they are
// separate:
//
//   - the 404 plus "unknown" header pair fails if the wire answer ever starts
//     carrying the distinction, which would hand any authenticated caller a
//     liveness oracle over other people's session ids;
//   - the cause fails on the mutant that deletes the cross-subject branch, and
//     also on the one that deletes the empty-cause fallback, which leaves it as
//     the empty string;
//   - the message fails on a branch that sets a distinct cause and goes on
//     telling the operator a session was rebuilt.
func TestA1CrossSubjectReplayIsLoggedAsAReplayNotAsARebuild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	gw, _, _, alice, _ := a1Fixture(t, ctx, "a1-crosssubject")
	buf := &syncBuffer{}
	gw.logger = logging.New(buf, "json", "info") // capture, in place of New()'s slog.Default()
	httpSrv := httptest.NewServer(gw.Handler())
	t.Cleanup(httpSrv.Close)
	t.Cleanup(gw.Close)

	// Nothing is evicted anywhere in this test. Alice connects and stays
	// healthy for its whole duration, which is what separates this case from
	// every other one in the branch.
	cs := connectThroughGateway(t, ctx, httpSrv.URL+"/mcp", "a1-tok")
	t.Cleanup(func() { _ = cs.Close() })
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "fixture__echo", Arguments: json.RawMessage(`{"text":"alice"}`)}); err != nil {
		t.Fatalf("alice's first CallTool: %v", err)
	}
	sid := cs.ID()
	if sid == "" {
		t.Fatal("alice's client session reported no Mcp-Session-Id")
	}

	bob := a1SecondPrincipal("a1-crosssubject")
	if bob.Subject == alice.Subject {
		t.Fatal("the two fixture principals share a subject; this gate would be asserting nothing")
	}
	resp := a1RawRequest(t, ctx, http.MethodPost, httpSrv.URL+"/mcp", a1SecondToken, sid,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("bob replaying alice's Mcp-Session-Id got status %d (body %q), want 404", resp.StatusCode, body)
	}
	// The WIRE answer is deliberately the same one an unrecorded-cause binding
	// gets. A replayer must not be able to tell "this id names a live session
	// that is not yours" from "nobody recorded why this id went stale".
	if marker := resp.Header.Get(sessionRebuiltHeader); marker != reclaimUnknown {
		t.Fatalf("cross-subject replay got %s=%q, want %q: the header must not distinguish this case, or it becomes a liveness oracle over other subjects' session ids", sessionRebuiltHeader, marker, reclaimUnknown)
	}
	// Alice is untouched: the refusal is per-request, and her binding still
	// points at a live session of hers rather than at a tombstone.
	b := gw.sessions.lookupTransport(sid)
	if !b.known || b.sess == nil || b.sess.subject != alice.Subject {
		t.Fatalf("after bob's replay alice's binding is known=%v sess=%p cause=%q; her session must still be live and hers, or this test is exercising the tombstone path instead", b.known, b.sess, b.cause)
	}

	fields := a1RejectionLog(t, buf, sid)
	if fields["subject"] != bob.Subject {
		t.Fatalf("rejection log line subject = %v, want %q (the replayer, not the id's owner)", fields["subject"], bob.Subject)
	}
	if fields["cause"] != sessionCrossSubject {
		t.Fatalf("rejection log line cause = %v, want %q: a replay of another subject's LIVE session is not the reclaimUnknown race and must not be logged as it", fields["cause"], sessionCrossSubject)
	}
	if msg, _ := fields["msg"].(string); strings.Contains(msg, "rebuilt") {
		t.Fatalf("rejection log message = %q: alice's session was never rebuilt, and telling an operator it was is the false rationale this gate exists for", msg)
	}
}

// TestA1BindingWithNoRecordedCauseIsStillGivenOne gates the empty-cause
// fallback in withSession's `case b.known:` branch.
//
// Review measured that deleting it failed ZERO tests. Every 404 on that branch
// would then ship an empty X-Orbeat-Session-Rebuilt and log cause="", which is
// precisely the operator dead end tombstone retention exists to prevent: the
// client is told to start a new session, and nobody on either side is told why.
//
// It reproduces the race reclaimUnknown documents WITHOUT a race harness, by
// assembling the state that race leaves behind: a session is evicted while it
// holds no bindings at all, so no eviction path records a cause anywhere; a
// fresh session takes its place; and only then is an id bound to the dead one,
// which is exactly what bindTransport does when it lands just after an
// eviction.
//
// It also pins the cross-subject predicate from the other side. b.sess != sess
// is true here too, so a cross-subject check written on session identity
// instead of on subject would report this internal race as a replay by another
// subject, a false accusation in the one place an operator goes looking for a
// real one.
func TestA1BindingWithNoRecordedCauseIsStillGivenOne(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	gw, _, _, p, _ := a1Fixture(t, ctx, "a1-racecause")
	buf := &syncBuffer{}
	gw.logger = logging.New(buf, "json", "info")
	httpSrv := httptest.NewServer(gw.Handler())
	t.Cleanup(httpSrv.Close)
	t.Cleanup(gw.Close)

	build := func() (*session, error) { return gw.buildSession(ctx, p) }
	doomed, err := gw.sessions.getOrBuild(p.Subject, time.Now(), build)
	if err != nil {
		t.Fatalf("build the session that will be evicted: %v", err)
	}
	// Evicted while it holds NO bindings, so staleTransportsLocked tombstones
	// nothing and no cause is recorded for anything.
	if _, ok := gw.sessions.get(p.Subject, time.Now().Add(sessionMaxAge+time.Second)); ok {
		t.Fatal("a session past sessionMaxAge was not evicted; the state this gate needs was never reached")
	}
	current, err := gw.sessions.getOrBuild(p.Subject, time.Now(), build)
	if err != nil {
		t.Fatalf("build the replacement session: %v", err)
	}
	if current == doomed {
		t.Fatal("got the evicted session back as the current one")
	}

	// bindTransport landing after the eviction, which is the whole race.
	const raceID = "a1-id-bound-just-after-its-session-was-evicted"
	gw.sessions.bindTransport(raceID, doomed)
	if b := gw.sessions.lookupTransport(raceID); !b.known || b.sess != doomed || b.cause != "" {
		t.Fatalf("setup: binding is known=%v sess=%p cause=%q, want a live-looking binding to the EVICTED session with no cause; this gate is meaningless unless it starts from that state", b.known, b.sess, b.cause)
	}

	resp := a1RawRequest(t, ctx, http.MethodPost, httpSrv.URL+"/mcp", "a1-tok", raceID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("replaying a binding with no recorded cause got status %d (body %q), want 404", resp.StatusCode, body)
	}
	if marker := resp.Header.Get(sessionRebuiltHeader); marker != reclaimUnknown {
		t.Fatalf("404 carried %s=%q, want %q: an empty marker tells the client to reconnect and tells the operator nothing at all", sessionRebuiltHeader, marker, reclaimUnknown)
	}

	fields := a1RejectionLog(t, buf, raceID)
	if fields["cause"] != reclaimUnknown {
		t.Fatalf("rejection log line cause = %v, want %q: %q here would mean the cross-subject check compares session identity rather than subject, and is accusing this subject of replaying its own id", fields["cause"], reclaimUnknown, sessionCrossSubject)
	}
}
