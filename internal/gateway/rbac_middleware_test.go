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
		// keyID="" (human path): identical to toolCallAllowed's behaviour
		// before virtual keys existed -- keyNarrow is never consulted.
		got := toolCallAllowed(c.name, slugToServer, ents, "", nil)
		if got != c.allowed {
			t.Fatalf("toolCallAllowed(%q) = %v, want %v", c.name, got, c.allowed)
		}
	}
}

// TestKeyToolCallDecisionNarrowingFiltersPerServer is Task 4's gates 2 and 3
// in one fixture, matching spec §6's own measured example: two servers
// (alpha/srv-1, beta/srv-2) whose role grants EVERY tool (AllowedTools: nil)
// on both, sharing the bare tool name "read". The key narrows ONLY
// alpha__read.
//
// Gate 2 mutant (call rbac.ToolAllowed instead of KeyToolAllowed, dropping
// the narrowing): alpha__write would become allowed (the role grants it and
// nothing would remove it) -- the "alpha__write denied" case below fails.
//
// Gate 3 mutant (pass keyNarrow unfiltered into KeyToolAllowed, stripping
// the per-server slug filter): two different wrong implementations are both
// caught here. A mutant that skips filtering ENTIRELY compares the raw
// namespaced narrow ("alpha__read") against the bare tool name ("read"),
// which is never equal -- alpha__read would become falsely DENIED, catching
// it on the "alpha__read allowed" case. A subtler mutant that splits each
// narrow entry but drops the slug match (returning every bare name
// regardless of server) makes beta__read TRUE, because it shares "read"'s
// bare name with alpha -- caught on the "beta__read denied" case. This is
// deliberately the exact defect spec §6 measured: narrow=["read"] applied
// unfiltered allowed BOTH srv1/read and srv2/read.
func TestKeyToolCallDecisionNarrowingFiltersPerServer(t *testing.T) {
	ents := []store.Entitlement{
		{MCPServerID: "srv-1", AllowedTools: nil}, // alpha grants every tool
		{MCPServerID: "srv-2", AllowedTools: nil}, // beta grants every tool too, INCLUDING "read"
	}
	slugToServer := map[string]string{"alpha": "srv-1", "beta": "srv-2"}
	narrow := []string{"alpha__read"} // narrows ONLY alpha's "read"

	cases := []struct {
		name    string
		allowed bool
	}{
		{"alpha__read", true},   // role grants it AND the key narrows exactly this
		{"alpha__write", false}, // role grants it, but the narrowing removes it
		{"beta__read", false},   // role grants it, but the narrowing says nothing about beta -- must NOT fall back to "everything"
	}
	for _, c := range cases {
		got := toolCallAllowed(c.name, slugToServer, ents, "key-1", narrow)
		if got != c.allowed {
			t.Errorf("toolCallAllowed(%q, keyID=key-1, narrow=%v) = %v, want %v", c.name, narrow, got, c.allowed)
		}
	}
}

// TestKeyToolCallDecisionNilNarrowGrantsEverythingTheRoleAllows proves the
// keyed path's nil case matches the human path exactly when a key applies no
// narrowing at all (store.VirtualKey.AllowedTools nil) -- nothing to filter,
// so rbac.KeyToolAllowed's own nil branch decides.
func TestKeyToolCallDecisionNilNarrowGrantsEverythingTheRoleAllows(t *testing.T) {
	ents := []store.Entitlement{{MCPServerID: "srv-1", AllowedTools: []string{"read"}}}
	slugToServer := map[string]string{"alpha": "srv-1"}

	if !toolCallAllowed("alpha__read", slugToServer, ents, "key-1", nil) {
		t.Error("alpha__read should be allowed: role grants it and the key applies no narrowing")
	}
	if toolCallAllowed("alpha__write", slugToServer, ents, "key-1", nil) {
		t.Error("alpha__write should be denied: the ROLE never granted it, and nil narrowing cannot grant what the role lacks")
	}
}

// TestRBACMiddlewareFailsClosedOnUnexpectedParamsType pins audit G8: a
// tools/call whose params are NOT *mcp.CallToolParamsRaw must be DENIED, not
// forwarded unchecked. The go-sdk's server-side routing always delivers
// *CallToolParamsRaw today, so this crafts a ServerRequest with a different
// (still valid) params type and invokes the middleware function directly --
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
	// based on entitlements it would allow -- only the fail-closed guard denies.
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
		t.Fatal("tools/call with unexpected params type was FORWARDED -- RBAC bypassed (must fail closed)")
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

// TestHumanSessionTakesNoVirtualKeyPath is Task 4's gate 4: a human session
// (sess.keyID == "") must take neither the per-call revocation lookup nor
// the narrowing branch in toolCallAllowed -- behaviour byte-for-byte what it
// was before virtual keys existed.
//
// keyClientID is deliberately set to a client_id that matches NO virtual_key
// row: if the revocation lookup ran anyway, Server.keyRevoked
// (virtualkey.ee.go) would call store.GetVirtualKeyByClientID, get
// store.ErrNotFound, and treat that exactly like revoked (its own doc
// comment: "there is no policy left ... to be authorized against") -- denying
// a call that a correctly-gated human session must allow.
//
// Mutant: apply the key path when keyID == "" (e.g. gate the revocation
// check on sess.keyClientID != "" instead of sess.keyID != ""). The
// otherwise-entitled call below would be wrongly denied.
func TestHumanSessionTakesNoVirtualKeyPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.New(ctx, gwDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	tn, err := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("rbac-human-unaffected-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}

	gw := New(st, authz.NewResolver(st, tn.Name), nil, secrets.NewResolver(), "http://gw.test", "http://kc.test/realms/orbeat")
	t.Cleanup(gw.Close)

	sess := &session{
		subject: "kc-human-unaffected", tenantID: tn.ID, actor: "kc-human-unaffected",
		entitlements: []store.Entitlement{{MCPServerID: "srv-1", AllowedTools: nil}},
		slugToServer: map[string]string{"alpha": "srv-1"},
		// keyID intentionally left empty: this is a human session.
		keyClientID: "client-id-matching-no-virtual-key-row",
	}

	forwarded := false
	next := func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		forwarded = true
		return nil, nil
	}
	handler := gw.rbacMiddleware(sess)(next)

	req := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{Params: &mcp.CallToolParamsRaw{Name: "alpha__anything"}}
	if _, err := handler(ctx, "tools/call", req); err != nil {
		t.Fatalf("handler returned an error for an entitled human call: %v", err)
	}
	if !forwarded {
		t.Fatal("an entitled human call was denied -- the virtual-key revocation path ran for a session with no key")
	}
}
