package gateway

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/secrets"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// TestEntitlementNudgeInvalidatesSessions drives the whole path against a real
// Postgres: a session is built and cached, the tenant's entitlements change,
// the NOTIFY travels, and the cached session is gone.
//
// It waits on the OUTCOME with a liveness ceiling rather than sleeping for a
// fixed interval, because the thing under test is "does this happen at all",
// not "how fast". A sleep-then-assert would pass or fail on machine load.
func TestEntitlementNudgeInvalidatesSessions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	st, err := store.New(ctx, gwDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	up := newUpstreamFixtureWithTools(t, "echo")
	tn, _ := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("nudge-%d", time.Now().UnixNano()))
	role, _ := st.CreateRole(ctx, tn.ID, "orbeat-user")
	srv, _ := st.CreateMCPServer(ctx, store.MCPServer{TenantID: tn.ID, Name: "nudge-up", Transport: "http", EndpointOrCommand: up.URL, Status: "active"})
	if _, err := st.CreateEntitlement(ctx, store.Entitlement{TenantID: tn.ID, RoleID: role.ID, MCPServerID: srv.ID}); err != nil {
		t.Fatalf("entitle: %v", err)
	}

	gw := New(st, authz.NewResolver(st, tn.Name), nil, secrets.NewResolver(), "http://gw.test", "http://kc.test/realms/orbeat")
	t.Cleanup(gw.Close)

	nudgeCtx, stopNudge := context.WithCancel(ctx)
	defer stopNudge()
	go gw.StartEntitlementNudge(nudgeCtx)

	p := auth.Principal{Subject: "kc-nudge", Roles: []string{"orbeat-user"}}
	sess, err := gw.sessions.getOrBuild(p.Subject, time.Now(), func() (*session, error) { return gw.buildSession(ctx, p) })
	if err != nil {
		t.Fatalf("buildSession: %v", err)
	}
	_ = sess
	if gw.sessions.size() != 1 {
		t.Fatalf("cache size = %d, want the session we just built", gw.sessions.size())
	}

	// The listener may still be connecting; retry the notify until it lands or
	// the ceiling expires. A single notify racing the LISTEN would make this
	// test flaky for a reason that has nothing to do with the behaviour.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := st.NotifyEntitlementChange(ctx, tn.ID); err != nil {
			t.Fatalf("notify: %v", err)
		}
		if gw.sessions.size() == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the cached session survived an entitlement-change notification")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestEntitlementNudgeIgnoresOtherTenants pins that the payload is used rather
// than treated as "something changed somewhere". Dropping every tenant's
// sessions on any tenant's change would be a self-inflicted thundering herd on
// a multi-tenant deployment, and it would look identical in the logs.
func TestEntitlementNudgeIgnoresOtherTenants(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	st, err := store.New(ctx, gwDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	up := newUpstreamFixtureWithTools(t, "echo")
	tn, _ := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("nudgeself-%d", time.Now().UnixNano()))
	other, _ := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("nudgeother-%d", time.Now().UnixNano()))
	role, _ := st.CreateRole(ctx, tn.ID, "orbeat-user")
	srv, _ := st.CreateMCPServer(ctx, store.MCPServer{TenantID: tn.ID, Name: "nudge-up2", Transport: "http", EndpointOrCommand: up.URL, Status: "active"})
	if _, err := st.CreateEntitlement(ctx, store.Entitlement{TenantID: tn.ID, RoleID: role.ID, MCPServerID: srv.ID}); err != nil {
		t.Fatalf("entitle: %v", err)
	}

	gw := New(st, authz.NewResolver(st, tn.Name), nil, secrets.NewResolver(), "http://gw.test", "http://kc.test/realms/orbeat")
	t.Cleanup(gw.Close)
	nudgeCtx, stopNudge := context.WithCancel(ctx)
	defer stopNudge()
	go gw.StartEntitlementNudge(nudgeCtx)

	p := auth.Principal{Subject: "kc-nudge2", Roles: []string{"orbeat-user"}}
	if _, err := gw.sessions.getOrBuild(p.Subject, time.Now(), func() (*session, error) { return gw.buildSession(ctx, p) }); err != nil {
		t.Fatalf("buildSession: %v", err)
	}

	// Notify for the OTHER tenant repeatedly, then for ours. When ours lands
	// the listener is demonstrably alive, so the survival above was a decision
	// rather than a message that never arrived: this is what stops the test
	// passing on a dead listener.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := st.NotifyEntitlementChange(ctx, other.ID); err != nil {
			t.Fatalf("notify other: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
		if gw.sessions.size() != 1 {
			t.Fatal("another tenant's entitlement change dropped this tenant's session")
		}
		if err := st.NotifyEntitlementChange(ctx, tn.ID); err != nil {
			t.Fatalf("notify self: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
		if gw.sessions.size() == 0 {
			return // our own notification landed: the listener was alive all along
		}
		if time.Now().After(deadline) {
			t.Fatal("neither tenant's notification ever landed; the listener was not running")
		}
	}
}
