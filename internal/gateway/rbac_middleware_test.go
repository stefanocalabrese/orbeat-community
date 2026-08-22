package gateway

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/secrets"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

func TestToolCallDecision(t *testing.T) {
	ents := []store.Entitlement{
		{MCPServerID: "srv-1", AllowedTools: nil},              // all tools on srv-1
		{MCPServerID: "srv-2", AllowedTools: []string{"safe"}}, // only "safe" on srv-2
	}
	slugToServer := map[string]string{"alpha": "srv-1", "beta": "srv-2"}

	cases := []struct {
		name    string
		allowed bool
	}{
		{"alpha__anything", true},  // srv-1 allows all
		{"beta__safe", true},       // srv-2 allows "safe"
		{"beta__danger", false},    // srv-2 denies others
		{"unknown__x", false},      // unknown slug
		{"no-separator", false},    // malformed (no __)
		{"alpha__sub__tool", true}, // tool name with __ (srv-1 allows all)
	}
	for _, c := range cases {
		got := toolCallAllowed(c.name, slugToServer, ents)
		if got != c.allowed {
			t.Fatalf("toolCallAllowed(%q) = %v, want %v", c.name, got, c.allowed)
		}
	}
}

// TestRBACMiddlewareFailsClosedOnUnexpectedParamsType pins audit G8: a
// tools/call whose params are NOT *mcp.CallToolParamsRaw must be DENIED, not
// forwarded unchecked. The go-sdk's server-side routing always delivers
// *CallToolParamsRaw today, so this crafts a ServerRequest with a different
// (still valid) params type and invokes the middleware function directly —
// exactly the shape an SDK change could start delivering, which previously
// bypassed RBAC silently.
func TestRBACMiddlewareFailsClosedOnUnexpectedParamsType(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.New(ctx, gwDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	tn, err := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("rbac-failclosed-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}

	gw := New(st, authz.NewResolver(st, tn.Name), nil, secrets.NewResolver(), "http://gw.test", "http://kc.test/realms/orbeat")
	t.Cleanup(gw.Close)

	// A session entitled to EVERYTHING on srv-1: if the middleware forwarded
	// based on entitlements it would allow — only the fail-closed guard denies.
	sess := &session{
		subject: "kc-failclosed", tenantID: tn.ID, actor: "kc-failclosed",
		entitlements: []store.Entitlement{{MCPServerID: "srv-1", AllowedTools: nil}},
		slugToServer: map[string]string{"alpha": "srv-1"},
	}

	forwarded := false
	next := func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		forwarded = true
		return nil, nil
	}
	handler := gw.rbacMiddleware(sess)(next)

	// *mcp.CallToolParams satisfies mcp.Params but is not *mcp.CallToolParamsRaw.
	req := &mcp.ServerRequest[*mcp.CallToolParams]{Params: &mcp.CallToolParams{Name: "alpha__anything"}}
	_, err = handler(ctx, "tools/call", req)

	if forwarded {
		t.Fatal("tools/call with unexpected params type was FORWARDED — RBAC bypassed (must fail closed)")
	}
	var denied *toolDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("err = %v, want *toolDeniedError (fail closed)", err)
	}

	// The deny must be audited, target flagged as unparseable.
	evs, err := st.ListAuditEventsByTenant(ctx, tn.ID, 50)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	sawDeny := false
	for _, e := range evs {
		if e.Action == "gateway.tool.call" && e.Decision == "deny" && e.Target == "<unparseable>" {
			sawDeny = true
		}
	}
	if !sawDeny {
		t.Fatalf("no deny audit for the unparseable tools/call; events=%+v", evs)
	}
}
