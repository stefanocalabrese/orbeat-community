package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/logging"
	"github.com/stefanocalabrese/orbeat-community/internal/secrets"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// requestIDStampingRoundTripper wraps bearerRoundTripper and DELIBERATELY
// never sets its own X-Request-Id -- a real MCP client (Claude Code, etc.)
// has no notion of this orbeat-specific header, and that is the realistic,
// common case this test means to exercise: the GATEWAY itself must generate
// one (logging.Requests' newRequestID, echoed back via the response header
// logging.Requests sets) and it is THAT generated value which must end up on
// the per-call audit log line -- not a client-supplied one, which would only
// prove withSession's header is capable of forwarding a value already
// sitting in r.Header untouched.
//
// It records the SERVER's own X-Request-Id response header for every
// outgoing "tools/call" JSON-RPC request, IN SEND ORDER, so a test driving
// sequential (blocking) cs.CallTool invocations can correlate "the Nth
// tools/call I made" with "the id the GATEWAY assigned it" -- independent of
// whatever rbacMiddleware's audit call does with it afterward, which is
// exactly the fact fable-audit B13 is about.
type requestIDStampingRoundTripper struct {
	base bearerRoundTripper

	mu          sync.Mutex
	toolCallIDs []string // one entry per outgoing "tools/call" request, in send order
}

func (rt *requestIDStampingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	isToolCall := false
	if r.Body != nil {
		body, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err == nil {
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			var probe struct {
				Method string `json:"method"`
			}
			// Best-effort: a batch or non-JSON-object body just fails this
			// unmarshal and is silently not recorded, which only means this
			// helper under-counts, never over-counts or mis-attributes.
			isToolCall = json.Unmarshal(body, &probe) == nil && probe.Method == "tools/call"
		}
	}
	resp, err := rt.base.RoundTrip(r)
	if isToolCall && err == nil && resp != nil {
		rt.mu.Lock()
		rt.toolCallIDs = append(rt.toolCallIDs, resp.Header.Get("X-Request-Id"))
		rt.mu.Unlock()
	}
	return resp, err
}

// sentToolCallIDs returns a snapshot of the SERVER-assigned ids observed on
// outgoing tools/call requests' responses, in send order.
func (rt *requestIDStampingRoundTripper) sentToolCallIDs() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]string(nil), rt.toolCallIDs...)
}

// connectThroughGatewayStampingRequestIDs is connectThroughGateway
// (gateway_integration_test.go) with the client's transport wrapped in
// requestIDStampingRoundTripper, so the test can read back the SERVER's own
// X-Request-Id for each outgoing tools/call POST.
func connectThroughGatewayStampingRequestIDs(t *testing.T, ctx context.Context, gatewayURL, token string) (*mcp.ClientSession, *requestIDStampingRoundTripper) {
	t.Helper()
	parsed, err := neturl.Parse(gatewayURL)
	if err != nil {
		t.Fatalf("parse gateway url %q: %v", gatewayURL, err)
	}
	rt := &requestIDStampingRoundTripper{
		base: bearerRoundTripper{token: token, allowedHost: parsed.Host, base: http.DefaultTransport},
	}
	httpClient := &http.Client{Transport: rt}
	transport := &mcp.StreamableClientTransport{Endpoint: gatewayURL, HTTPClient: httpClient}
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect through gateway: %v", err)
	}
	return cs, rt
}

// auditLogLine is the subset of an "audit" log line's fields this test reads.
type auditLogLine struct {
	Action    string `json:"action"`
	Target    string `json:"target"`
	RequestID string `json:"request_id"`
}

// parseAuditLines decodes every JSON log line in s whose msg is "audit" and
// whose action is wantAction, in the order they were written.
func parseAuditLines(t *testing.T, s, wantAction string) []auditLogLine {
	t.Helper()
	var out []auditLogLine
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if line == "" {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatalf("log line not valid JSON: %v\nline: %s", err, line)
		}
		if fields["msg"] != "audit" {
			continue
		}
		var al auditLogLine
		if err := json.Unmarshal([]byte(line), &al); err != nil {
			t.Fatalf("audit log line not decodable: %v\nline: %s", err, line)
		}
		if al.Action != wantAction {
			continue
		}
		out = append(out, al)
	}
	return out
}

// TestToolCallAuditRequestIDMatchesTheCallItCameFrom is fable-audit B13's
// gate, REPRODUCED: rbacMiddleware's ctx is the jsonrpc2 connection's own
// context, frozen at whichever HTTP POST established the transport session
// (sessionKeyFn's doc comment states the identical fact for TokenInfo) --
// before request_id.go existed, every gateway.tool.call audit LOG LINE
// emitted from calls sharing one MCP transport session carried the SAME,
// WRONG request_id: the one belonging to whichever POST happened to build
// the session, matching none of the actual per-call requests.
//
// This drives THREE tools/call requests over ONE mcp.ClientSession (one
// transport session, one frozen jsonrpc2 ctx) -- the client sends no
// X-Request-Id of its own (the realistic case: real MCP clients don't know
// this header exists), so every id in play is one the GATEWAY itself
// generated per POST. It then asserts each gateway.tool.call audit log
// line's request_id equals the id the gateway assigned THAT SPECIFIC call's
// own response -- not merely that the three ids differ from each other
// (which a wrong-but-still-varying value could also satisfy), and not
// merely that they are non-empty.
func TestToolCallAuditRequestIDMatchesTheCallItCameFrom(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.New(ctx, gwDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	up := newUpstreamFixture(t)
	tn, err := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("b13-reqid-%d", time.Now().UnixNano()))
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
	if _, err := st.CreateEntitlement(ctx, store.Entitlement{
		TenantID: tn.ID, RoleID: role.ID, MCPServerID: srv.ID, AllowedTools: []string{"echo"},
	}); err != nil {
		t.Fatalf("entitlement: %v", err)
	}

	verifier := stubVerifier(map[string]auth.Principal{
		"reqid-tok": {Subject: "kc-reqid", Email: "reqid@x.io", Roles: []string{"orbeat-user"}},
	})
	buf := &syncBuffer{}
	gw := New(st, authz.NewResolver(st, tn.Name), verifier, secrets.NewResolver(), "http://gw.test", "http://kc.test/realms/orbeat")
	gw.logger = logging.New(buf, "json", "info") // capture, in place of New()'s slog.Default()
	t.Cleanup(gw.Close)
	httpSrv := httptest.NewServer(gw.Handler())
	t.Cleanup(httpSrv.Close)

	cs, rt := connectThroughGatewayStampingRequestIDs(t, ctx, httpSrv.URL+"/mcp", "reqid-tok")
	t.Cleanup(func() { _ = cs.Close() })

	const n = 3
	for i := 0; i < n; i++ {
		if _, err := cs.CallTool(ctx, &mcp.CallToolParams{
			Name: "fixture__echo", Arguments: json.RawMessage(fmt.Sprintf(`{"text":"call-%d"}`, i)),
		}); err != nil {
			t.Fatalf("CallTool #%d: %v", i, err)
		}
	}

	sent := rt.sentToolCallIDs()
	if len(sent) != n {
		t.Fatalf("client sent %d tools/call requests, want %d; ids=%v", len(sent), n, sent)
	}

	audited := parseAuditLines(t, buf.String(), "gateway.tool.call")
	// Filter to allow decisions only: this fixture's role is entitled, so all
	// three should be "allow" -- but filtering explicitly (rather than
	// assuming) keeps the test correct even if the gateway ever logs an
	// incidental extra gateway.tool.call row for something else.
	var allowed []auditLogLine
	for _, a := range audited {
		if a.Target == "fixture__echo" {
			allowed = append(allowed, a)
		}
	}
	if len(allowed) != n {
		t.Fatalf("got %d gateway.tool.call audit lines for fixture__echo, want %d; lines=%+v\nfull log:\n%s", len(allowed), n, allowed, buf.String())
	}

	// THE ASSERTION THAT CANNOT PASS ON THE BUG: each audited call's
	// request_id must equal the id THAT SPECIFIC call's own HTTP POST
	// carried -- not merely be present, not merely differ from its
	// neighbours. Under the frozen-ctx bug every entry here would read the
	// same wrong value (whichever POST built the session), matching none of
	// sent[i] for the tools/call POSTs made after the first.
	for i := 0; i < n; i++ {
		if allowed[i].RequestID != sent[i] {
			t.Fatalf("call #%d: audit log request_id = %q, want %q (the id THIS call's own POST carried); sent=%v audited=%+v",
				i, allowed[i].RequestID, sent[i], sent, allowed)
		}
	}
	// And the negative control restated explicitly: they must not all
	// collapse to one repeated value (the exact symptom fable-audit B13
	// measured -- "three tool calls produced one repeated id, and it
	// matched none of the three requests").
	if allowed[0].RequestID == allowed[1].RequestID && allowed[1].RequestID == allowed[2].RequestID {
		t.Fatalf("all three audit request_ids are identical (%q); want one per call", allowed[0].RequestID)
	}
}
