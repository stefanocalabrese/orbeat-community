package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/secrets"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// barrierUpstream is an MCP upstream that will not answer its FIRST request
// until `n` distinct upstreams have arrived. It is how these tests prove
// concurrency without measuring time: if the gateway dialled sequentially, the
// first upstream would wait for a second arrival that can never come, and the
// test would fail on its liveness ceiling rather than on a stopwatch reading.
// A wall-clock assertion ("the build took less than N times the timeout") would
// measure the machine, which this repo has been bitten by before.
type barrierUpstream struct {
	*httptest.Server
	arrived chan struct{}
}

func newBarrierUpstreams(t *testing.T, n int, toolPrefix string) []*barrierUpstream {
	t.Helper()
	arrivals := make(chan struct{}, n)
	release := make(chan struct{})
	var once sync.Once

	out := make([]*barrierUpstream, 0, n)
	for i := 0; i < n; i++ {
		srv := mcp.NewServer(&mcp.Implementation{Name: "barrier", Version: "0.0.1"}, nil)
		mcp.AddTool(srv, &mcp.Tool{Name: fmt.Sprintf("%s%d", toolPrefix, i), Description: "x"},
			func(ctx context.Context, _ *mcp.CallToolRequest, in echoArgs) (*mcp.CallToolResult, any, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.Text}}}, nil, nil
			})
		h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
		var gate sync.Once
		wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gate.Do(func() {
				arrivals <- struct{}{}
				if len(arrivals) >= n {
					once.Do(func() { close(release) })
				}
				select {
				case <-release:
				case <-time.After(20 * time.Second):
					// Liveness ceiling, not a measurement: reaching it means
					// the arrivals never overlapped.
				}
			})
			h.ServeHTTP(w, r)
		})
		ts := httptest.NewServer(wrapped)
		t.Cleanup(ts.Close)
		out = append(out, &barrierUpstream{Server: ts, arrived: arrivals})
	}
	return out
}

// TestBuildSessionDialsUpstreamsConcurrently is the proof that the parallel
// phase is real. Three upstreams each block their first request until all
// three have been reached; a sequential build deadlocks on the first one and
// this test fails on its ceiling.
func TestBuildSessionDialsUpstreamsConcurrently(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	st, err := store.New(ctx, gwDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	ups := newBarrierUpstreams(t, 3, "tool")
	tn, _ := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("pardial-%d", time.Now().UnixNano()))
	role, _ := st.CreateRole(ctx, tn.ID, "orbeat-user")
	for i, up := range ups {
		srv, err := st.CreateMCPServer(ctx, store.MCPServer{
			TenantID: tn.ID, Name: fmt.Sprintf("bar-%d", i), Transport: "http",
			EndpointOrCommand: up.URL, Status: "active",
		})
		if err != nil {
			t.Fatalf("create server %d: %v", i, err)
		}
		if _, err := st.CreateEntitlement(ctx, store.Entitlement{TenantID: tn.ID, RoleID: role.ID, MCPServerID: srv.ID}); err != nil {
			t.Fatalf("entitle %d: %v", i, err)
		}
	}

	gw := New(st, authz.NewResolver(st, tn.Name), nil, secrets.NewResolver(), "http://gw.test", "http://kc.test/realms/orbeat")
	t.Cleanup(gw.Close)

	done := make(chan *session, 1)
	go func() {
		sess, err := gw.buildSession(ctx, auth.Principal{Subject: "kc-par", Roles: []string{"orbeat-user"}})
		if err != nil {
			t.Errorf("buildSession: %v", err)
			done <- nil
			return
		}
		done <- sess
	}()

	select {
	case sess := <-done:
		if sess == nil {
			t.Fatal("session build failed")
		}
		t.Cleanup(sess.close)
		if len(sess.upstreams) != 3 {
			t.Fatalf("connected %d upstreams, want 3", len(sess.upstreams))
		}
	case <-ctx.Done():
		t.Fatal("session build never completed: the upstreams' first requests did not overlap, so the dials ran sequentially")
	}
}

// TestSlugCollisionWinnerIsDeterministicUnderAdversarialTiming is the reason
// phase 2 is sequential and ordered by name rather than by completion.
//
// The FIRST server by name is deliberately made the SLOW one. With a
// completion-ordered claim the fast collider would take the slug, and per-call
// RBAC would authorize the winner's tools against the collider's entitlements:
// the exact privilege escalation v1.17.0 closed, reintroduced by a scheduling
// detail rather than by a code change anyone would notice.
func TestSlugCollisionWinnerIsDeterministicUnderAdversarialTiming(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	st, err := store.New(ctx, gwDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	slow := newDelayedConnectUpstream(t, 700*time.Millisecond, "slowtool")
	fast := newUpstreamFixtureWithTools(t, "fasttool")

	tn, _ := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("slugrace-%d", time.Now().UnixNano()))
	role, _ := st.CreateRole(ctx, tn.ID, "orbeat-user")
	// "collide" sorts before "collide!", and is the slow one.
	srvSlow, _ := st.CreateMCPServer(ctx, store.MCPServer{TenantID: tn.ID, Name: "collide", Transport: "http", EndpointOrCommand: slow.URL, Status: "active"})
	srvFast, _ := st.CreateMCPServer(ctx, store.MCPServer{TenantID: tn.ID, Name: "collide!", Transport: "http", EndpointOrCommand: fast.URL, Status: "active"})
	for _, id := range []string{srvSlow.ID, srvFast.ID} {
		if _, err := st.CreateEntitlement(ctx, store.Entitlement{TenantID: tn.ID, RoleID: role.ID, MCPServerID: id}); err != nil {
			t.Fatalf("entitle: %v", err)
		}
	}

	gw := New(st, authz.NewResolver(st, tn.Name), nil, secrets.NewResolver(), "http://gw.test", "http://kc.test/realms/orbeat")
	t.Cleanup(gw.Close)

	sess, err := gw.buildSession(ctx, auth.Principal{Subject: "kc-slugrace", Roles: []string{"orbeat-user"}})
	if err != nil {
		t.Fatalf("buildSession: %v", err)
	}
	t.Cleanup(sess.close)

	if got := sess.slugToServer["collide"]; got != srvSlow.ID {
		t.Fatalf("slug 'collide' resolves to %q, want the FIRST server by name %q even though it answered last", got, srvSlow.ID)
	}
	if len(sess.upstreams) != 1 {
		t.Fatalf("connected upstreams = %d, want 1", len(sess.upstreams))
	}
}

// newDelayedConnectUpstream is an upstream whose HANDSHAKE is slow (every
// request delayed), not just its tool calls: the point is to lose the dial
// race, not to be slow afterwards.
func newDelayedConnectUpstream(t *testing.T, delay time.Duration, toolName string) *httptest.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "delayed", Version: "0.0.1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: toolName, Description: "x"},
		func(ctx context.Context, _ *mcp.CallToolRequest, in echoArgs) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.Text}}}, nil, nil
		})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}
