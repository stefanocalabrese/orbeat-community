package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// TestNudgesGatewaySessions pins WHICH audit actions are worth dropping a live
// MCP session for.
//
// Matched by prefix rather than by an exact list because an exact list is a
// second place to remember, and forgetting it fails silently: a new
// `entitlement.update` handler would simply stop nudging and nobody would
// notice for five minutes at a time.
//
// Artifacts are excluded on purpose and the test says so: they are Channel-1
// and Channel-2 content that no gateway session reads, so nudging on an
// artifact change would tear down live MCP sessions to no effect.
func TestNudgesGatewaySessions(t *testing.T) {
	for _, tc := range []struct {
		action string
		want   bool
	}{
		{"entitlement.create", true},
		{"entitlement.delete", true},
		{"entitlement.update", true}, // does not exist yet; the prefix covers it in advance
		{"role.create", true},
		{"role.delete", true},
		{"server.create", true},
		{"server.update", true},
		{"server.delete", true},
		{"artifact.create", false},
		{"artifact.approve", false},
		{"artifact.rollback", false},
		{"marketplace.publish", false},
		{"", false},
	} {
		if got := nudgesGatewaySessions(tc.action); got != tc.want {
			t.Errorf("nudgesGatewaySessions(%q) = %v, want %v", tc.action, got, tc.want)
		}
	}
}

// TestEntitlementWriteEmitsTheNudge is the wiring gate, and it exists because a
// mutant proved nothing else covered it: the API could stop notifying entirely
// and every gateway-side test would stay green, since those call
// NotifyEntitlementChange directly.
//
// It listens on the real channel, drives the REAL handler, and waits for the
// payload. The payload is asserted to be the tenant id rather than merely
// non-empty: a nudge naming the wrong tenant invalidates nobody, which looks
// identical to a nudge that worked.
func TestEntitlementWriteEmitsTheNudge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	srv, st, tn := newAdminServer(t)
	role, err := st.CreateRole(ctx, tn.ID, fmt.Sprintf("nudge-role-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("role: %v", err)
	}
	mcpSrv, err := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: fmt.Sprintf("nudge-srv-%d", time.Now().UnixNano()),
		Transport: "http", EndpointOrCommand: "https://example.invalid/mcp", Status: "active",
	})
	if err != nil {
		t.Fatalf("server: %v", err)
	}

	// A SECOND store handle: Listen holds a dedicated connection for as long as
	// it runs, and taking it from the pool the handler is also using would be a
	// self-inflicted starvation in a test that is not about pooling.
	listener, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("listener store: %v", err)
	}
	t.Cleanup(listener.Close)

	payloads := make(chan string, 8)
	listenCtx, stopListen := context.WithCancel(ctx)
	defer stopListen()
	go func() {
		_ = listener.Listen(listenCtx, store.EntitlementChannel, func(p string) {
			select {
			case payloads <- p:
			default:
			}
		})
	}()

	// The listener may still be connecting, so retry the write until a payload
	// arrives or the ceiling expires. Each attempt is a real entitlement create
	// through the real handler.
	deadline := time.Now().Add(30 * time.Second)
	for attempt := 0; ; attempt++ {
		body := map[string]any{"roleId": role.ID, "mcpServerId": mcpSrv.ID}
		rec := httptest.NewRecorder()
		req := adminReq(ctx, http.MethodPost, "/v1/admin/entitlements", body, tn)
		srv.handleCreateEntitlement(rec, req)
		if rec.Code != http.StatusCreated && rec.Code != http.StatusConflict {
			t.Fatalf("create entitlement: status %d body %s", rec.Code, rec.Body)
		}
		if rec.Code == http.StatusCreated {
			// Delete it again so the next attempt can recreate it.
			var created struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &created)
			if created.ID != "" {
				delRec := httptest.NewRecorder()
				delReq := adminReq(ctx, http.MethodDelete, "/v1/admin/entitlements/"+created.ID, nil, tn)
				delReq.SetPathValue("id", created.ID)
				srv.handleDeleteEntitlement(delRec, delReq)
			}
		}

		select {
		case got := <-payloads:
			if got != tn.ID {
				t.Fatalf("nudge payload = %q, want the tenant id %q (a nudge naming the wrong tenant invalidates nobody)", got, tn.ID)
			}
			return
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("an entitlement write emitted no entitlement-change notification")
		}
	}
}
