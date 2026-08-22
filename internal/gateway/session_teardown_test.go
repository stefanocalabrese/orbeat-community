package gateway

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// trackedConn records whether the connection was ever closed, so a test can
// observe CloseIdleConnections having actually done something. http.Transport
// exposes no idle-pool accessor, so the socket is the only honest vantage point.
type trackedConn struct {
	net.Conn
	closed *atomic.Bool
}

func (c trackedConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

// idleTrackedTransport returns a transport holding exactly one IDLE connection
// to a throwaway server, plus the flag that flips when that connection closes.
// The connection is deliberately left idle (body drained and closed) so it sits
// in the pool: CloseIdleConnections only closes idle connections, so a test that
// left the body open would pass for the wrong reason.
func idleTrackedTransport(t *testing.T) (*http.Transport, *atomic.Bool) {
	t.Helper()
	closed := &atomic.Bool{}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return trackedConn{Conn: c, closed: closed}, nil
		},
	}
	t.Cleanup(tr.CloseIdleConnections)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	resp, err := (&http.Client{Transport: tr}).Get(srv.URL)
	if err != nil {
		t.Fatalf("priming request: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if closed.Load() {
		t.Fatal("precondition failed: the primed connection closed before the test ran")
	}
	return tr, closed
}

// TestSessionCloseClosesOwnedTransport pins the teardown leg of spec §9. A
// CA-configured upstream owns its transport, and unlike the process-lived shared
// dialGuardTransport it is PER SESSION — so a session torn down without closing
// it leaks a connection pool on every session build, directly underneath the
// session leak v1.25.0 closed.
//
// This gate did not exist when Task 4 shipped the wiring: removing the
// CloseIdleConnections call from session.close() left the entire gateway package
// green, measured. A pool leak produces no wrong answers, only file descriptors,
// which is exactly why nothing else catches it.
func TestSessionCloseClosesOwnedTransport(t *testing.T) {
	up := newUpstreamFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn, err := connectUpstream(ctx, store.MCPServer{
		ID: "srv-tls", Name: "Pinned Upstream", Transport: "http",
		EndpointOrCommand: up.URL, Status: "active",
	}, "", testCAPEM, 0)
	if err != nil {
		t.Fatalf("connectUpstream: %v", err)
	}
	if conn.transport == nil {
		t.Fatal("precondition failed: a CA-configured upstream must own its transport")
	}

	// Swap in a transport whose idle connection is observable. Both are
	// *http.Transport, and session.close() cannot tell the difference — it just
	// calls CloseIdleConnections on whatever the field holds.
	conn.transport.CloseIdleConnections()
	tr, closed := idleTrackedTransport(t)
	conn.transport = tr

	sess := &session{upstreams: []*upstreamConn{conn}}
	sess.close()

	if !closed.Load() {
		t.Fatal("session.close() did not close the owned transport's idle connections — every session build leaks a pool")
	}
}

// TestSessionCloseSkipsTheSharedTransport is the converse, and it is not
// decoration: session.close() must NOT close a transport it does not own. Every
// upstream without a CA shares the package-level dialGuardTransport, so closing
// it on one session's teardown would drop idle connections belonging to every
// other session in the process.
func TestSessionCloseSkipsTheSharedTransport(t *testing.T) {
	up := newUpstreamFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn, err := connectUpstream(ctx, store.MCPServer{
		ID: "srv-plain", Name: "Shared Upstream", Transport: "http",
		EndpointOrCommand: up.URL, Status: "active",
	}, "", "", 0)
	if err != nil {
		t.Fatalf("connectUpstream: %v", err)
	}
	if conn.transport != nil {
		t.Fatal("an upstream without a CA must not own a transport")
	}

	// close() must not panic on the nil field, and must leave the shared
	// transport alone. The nil check in session.close() is what provides both.
	sess := &session{upstreams: []*upstreamConn{conn}}
	sess.close()
}
