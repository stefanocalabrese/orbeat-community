package gateway

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/ratelimit"
)

// TestSessionKeyFnReadsTheCallsOwnToken is the ONLY remaining gate on
// sessionKeyFn's central claim, and it exists because fixing fable-audit B14
// took the previous one away.
//
// ratelimit_test.go used to prove this end to end, by swapping the bearer
// token mid-connection over one Mcp-Session-Id and watching the rate-limit key
// follow the NEW token rather than the connection ctx's frozen one. B14
// (sessionCache keyed on "subject|azp") plus A1's mint-time binding make that
// swap 404 at the HTTP layer, so the scenario is unreachable in production and
// a test driving it would be asserting a security regression. Removing it was
// right. What removing it silently cost was the only thing that could fail if
// sessionKeyFn started reading ctx -- and sessionKeyFn's own doc comment says
// as much, that "reading ctx would not misattribute a REACHABLE call today",
// which is exactly the shape of a claim with no test behind it.
//
// WHAT THIS CAN AND CANNOT ASSERT, stated rather than implied. The ideal test
// gives ctx one principal and extra a DIFFERENT one and watches extra win.
// That cannot be built: the SDK stores TokenInfo under an unexported
// context key (go-sdk@v1.7.0 auth/auth.go, tokenInfoKey) with no exported
// setter, so no test outside that package can hand sessionKeyFn an
// authenticated ctx. The divergence is therefore asserted indirectly, and it
// still discriminates: ctx here carries NO token, so an implementation that
// read ctx would find nothing and return ok=false, failing this test. A
// mutant swapping extra.TokenInfo for mcpauth.TokenInfoFromContext(ctx) was
// run and does fail on exactly that.
func TestSessionKeyFnReadsTheCallsOwnToken(t *testing.T) {
	callPrincipal := auth.Principal{Subject: "sub-shared", ClientID: "codex"}

	req := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{
		Extra: &mcp.RequestExtra{TokenInfo: principalToTokenInfo(callPrincipal)},
	}

	got, ok := sessionKeyFn(context.Background(), req)
	if !ok {
		t.Fatal("sessionKeyFn returned ok=false for a request whose Extra carries a valid " +
			"principal, so it did not read req.GetExtra().TokenInfo")
	}
	if want := ratelimit.KeyFor(callPrincipal); got != want {
		t.Errorf("sessionKeyFn = %q, want %q (the CALL's own token)", got, want)
	}
}

// TestSessionKeyFnDistinguishesTwoClientsOfOneSubject pins the property B14
// is actually about: two OAuth clients belonging to the same human must not
// collapse onto one key. A key derived from Subject alone -- which is what
// sessionCache did before B14 -- makes these two equal, and whichever client
// connected first would decide what the other could do.
func TestSessionKeyFnDistinguishesTwoClientsOfOneSubject(t *testing.T) {
	keyFor := func(clientID string) string {
		p := auth.Principal{Subject: "sub-shared", ClientID: clientID}
		k, ok := sessionKeyFn(context.Background(), &mcp.ServerRequest[*mcp.CallToolParamsRaw]{
			Extra: &mcp.RequestExtra{TokenInfo: principalToTokenInfo(p)},
		})
		if !ok {
			t.Fatalf("sessionKeyFn refused a valid principal for client %q", clientID)
		}
		return k
	}
	claude, codex := keyFor("claude-code"), keyFor("codex")
	if claude == codex {
		t.Errorf("both clients of one subject key to %q, so they would share a cached gateway "+
			"session and the first to connect would decide the other's entitlements "+
			"(fable-audit B14)", claude)
	}
}

// TestSessionKeyFnRefusesARequestCarryingNoPrincipal pins the fail-closed
// half: returning a key here would let an unauthenticated dispatch share a
// bucket with whatever the empty principal hashes to.
func TestSessionKeyFnRefusesARequestCarryingNoPrincipal(t *testing.T) {
	for name, req := range map[string]mcp.Request{
		"nil Extra":     &mcp.ServerRequest[*mcp.CallToolParamsRaw]{},
		"nil TokenInfo": &mcp.ServerRequest[*mcp.CallToolParamsRaw]{Extra: &mcp.RequestExtra{}},
	} {
		t.Run(name, func(t *testing.T) {
			if got, ok := sessionKeyFn(context.Background(), req); ok {
				t.Errorf("sessionKeyFn = (%q, true), want ok=false for a request carrying "+
					"no principal", got)
			}
		})
	}
}
