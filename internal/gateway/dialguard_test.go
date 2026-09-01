package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// TestDialGuardBlocksIPv4LinkLocalMetadata drives the REAL package-level
// dialGuardTransport (the exact object wired into connectUpstream's
// httpClient in broker.go), not blockMetadataDial in isolation, so a
// mutant that stops wiring the Control hook into the dialer at all — not
// just one that guts the predicate — also goes red here.
//
// Red-proof: make blockMetadataDial always return nil (the block removed).
// The dial then proceeds to the OS instead of failing inside Control, and
// errors.Is(err, errMetadataBlocked) is false.
func TestDialGuardBlocksIPv4LinkLocalMetadata(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	_, err := dialGuardTransport.DialContext(ctx, "tcp", "169.254.169.254:80")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("dial to 169.254.169.254:80 succeeded; want refused by the dial guard")
	}
	if !errors.Is(err, errMetadataBlocked) {
		t.Fatalf("dial to 169.254.169.254:80 failed for the wrong reason: %v (want errMetadataBlocked)", err)
	}
	// Control runs before any packet is sent, so a real block is near
	// instant. A blown context deadline (2s) would mean the guard let the
	// dial reach the OS instead of refusing it in Control.
	if elapsed >= 2*time.Second {
		t.Fatalf("dial took %s, at the context deadline; the guard did not intervene in Control", elapsed)
	}
}

// TestDialGuardBlocksAWSIPv6MetadataAddress pins fd00:ec2::254 specifically
// (not just "IPv6 link-local"): this address is Unique Local Address space,
// which IsLinkLocalUnicast does NOT cover, so it needs its own check in
// blockMetadataDial.
//
// Red-proof: delete the ip.Equal(awsIPv6MetadataAddr) arm (the blocklist
// omits fd00:ec2::254). The dial then proceeds instead of being refused in
// Control.
func TestDialGuardBlocksAWSIPv6MetadataAddress(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := dialGuardTransport.DialContext(ctx, "tcp", "[fd00:ec2::254]:80")
	if err == nil {
		t.Fatal("dial to [fd00:ec2::254]:80 succeeded; want refused by the dial guard")
	}
	if !errors.Is(err, errMetadataBlocked) {
		t.Fatalf("dial to [fd00:ec2::254]:80 failed for the wrong reason: %v (want errMetadataBlocked)", err)
	}
}

// TestDialGuardBlocksAlibabaMetadataAddress pins 100.100.100.200
// specifically (fable-audit B38(d)): it lives in RFC 6598 Shared Address
// Space (100.64.0.0/10), which IsLinkLocalUnicast does NOT cover (it is not
// 169.254.0.0/16), so -- exactly like fd00:ec2::254 above -- it needs its own
// explicit check in blockMetadataDial.
//
// Red-proof: delete the ip.Equal(alibabaMetadataAddr) arm. The dial then
// proceeds instead of being refused in Control.
func TestDialGuardBlocksAlibabaMetadataAddress(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	_, err := dialGuardTransport.DialContext(ctx, "tcp", "100.100.100.200:80")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("dial to 100.100.100.200:80 succeeded; want refused by the dial guard")
	}
	if !errors.Is(err, errMetadataBlocked) {
		t.Fatalf("dial to 100.100.100.200:80 failed for the wrong reason: %v (want errMetadataBlocked)", err)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("dial took %s, at the context deadline; the guard did not intervene in Control", elapsed)
	}
}

// TestDialGuardAllowsPrivateRFC1918Address pins the design's deliberate
// exclusion: internal MCP servers on a private network are orbeat's PRIMARY
// deployment shape, not an attacker's, so 10.0.0.0/8 must stay dialable.
//
// The Control hook must let the address through; what happens to the actual
// TCP connect afterward is irrelevant and environment-dependent (i/o
// timeout, connection refused, network unreachable, or a sandbox denying
// outbound network entirely) — none of those are errMetadataBlocked, which
// is exactly the property under test. A short deadline keeps this bounded
// regardless of environment.
//
// Red-proof: widen blockMetadataDial to also block 10.0.0.0/8 (someone
// "hardening" this into a general private-range denylist). The dial then
// fails with errMetadataBlocked instead of some other, non-guard reason.
func TestDialGuardAllowsPrivateRFC1918Address(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err := dialGuardTransport.DialContext(ctx, "tcp", "10.255.255.1:80")
	if err == nil {
		// A real host answered on this address in this environment; that is
		// still "not blocked by the guard", which is what this test asserts.
		return
	}
	if errors.Is(err, errMetadataBlocked) {
		t.Fatalf("dial to private address 10.255.255.1:80 was refused by the dial guard: %v (private ranges must stay allowed)", err)
	}
}

// TestBlockMetadataDialFailsClosedOnNonIPAddress documents blockMetadataDial's
// defensive branch: Control always receives a resolved literal-IP address in
// practice, but a malformed one must refuse, not silently allow.
func TestBlockMetadataDialFailsClosedOnNonIPAddress(t *testing.T) {
	t.Parallel()
	if err := blockMetadataDial("tcp", "not-an-address", nil); err == nil {
		t.Fatal("blockMetadataDial(\"not-an-address\") returned nil; want a fail-closed error")
	}
}

// TestConnectUpstreamRefusesMetadataEndpoint pins the WIRING, which the tests
// above deliberately do not cover: they drive dialGuardTransport directly, so
// every one of them still passes when broker.go builds its httpClient on
// http.DefaultTransport instead. Verified: un-wiring the guard leaves the whole
// internal/gateway package green.
//
// That gap is not hypothetical here. a0312bc committed the guard with its
// wiring hunk left uncommitted (a concurrent edit tangled broker.go), so the
// control existed, passed its own tests, and did nothing. The wiring later
// landed inside fccb608, an unrelated observability commit, which means a
// revert of that commit would silently disarm this control. This test is what
// makes that revert loud.
//
// It asserts the SPECIFIC error, never merely that the dial failed: without the
// guard the dial still errors, just by timing out against an unreachable
// address, so an `err != nil` assertion would pass in both directions and pin
// nothing.
func TestConnectUpstreamRefusesMetadataEndpoint(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := connectUpstream(ctx, store.MCPServer{
		ID:                "srv-metadata",
		Name:              "metadata probe",
		Transport:         "http",
		EndpointOrCommand: "http://169.254.169.254/mcp",
	}, "", "", 0)

	if err == nil {
		t.Fatal("connectUpstream to a metadata endpoint succeeded; want refused")
	}
	if !errors.Is(err, errMetadataBlocked) {
		t.Fatalf("connectUpstream failed for the wrong reason: %v\nwant errMetadataBlocked; a timeout here means broker.go is not using dialGuardTransport", err)
	}
}

// TestConnectUpstreamRefusesMetadataEndpointWithCA is
// TestConnectUpstreamRefusesMetadataEndpoint's CA-configured sibling, and the
// wiring gap none of the existing tests close: TestUpstreamTransportKeepsTheDialGuard
// (tlsconfig_test.go) drives upstreamTransport directly, not through
// connectUpstream's caPEM branch in broker.go; TestConnectUpstreamRefusesMetadataEndpoint
// passes an EMPTY caPEM, so it only ever exercises the shared dialGuardTransport;
// and TestConnectUpstreamOwnsItsTransportWithCA (broker_test.go) passes a real
// CA but asserts only that conn.transport is non-nil, never that the guard on
// that owned transport actually fires. Verified: stripping the guard inside
// broker.go's `caPEM != ""` branch (replacing its DialContext with a bare
// net.Dialer's) leaves the whole internal/gateway package green without this
// test. As with TestConnectUpstreamRefusesMetadataEndpoint, it asserts the
// SPECIFIC sentinel error, never merely that the dial failed: without the
// guard the dial still errors, just by timing out against an unreachable
// address.
func TestConnectUpstreamRefusesMetadataEndpointWithCA(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := connectUpstream(ctx, store.MCPServer{
		ID:                "srv-metadata-ca",
		Name:              "metadata probe ca",
		Transport:         "http",
		EndpointOrCommand: "http://169.254.169.254/mcp",
	}, "", testCAPEM, 0)

	if err == nil {
		t.Fatal("connectUpstream (CA-configured) to a metadata endpoint succeeded; want refused")
	}
	if !errors.Is(err, errMetadataBlocked) {
		t.Fatalf("connectUpstream (CA-configured) failed for the wrong reason: %v\nwant errMetadataBlocked; a timeout here means the CA-pinned transport is not carrying the dial guard", err)
	}
}

// TestGuardedTransportsCarryNoProxy is the gate for audit A16: every test above
// dials with no proxy environment set, so all of them stayed green while a
// single HTTP_PROXY variable turned the guard off completely.
//
// The property is that no proxy function can intervene at all, which is why the
// assertion is on Proxy being nil rather than on any particular value: a mutant
// that re-attached http.ProxyFromEnvironment and one that attached a hand-rolled
// proxy func are the same defect, and both fail here.
//
// The first assertion is what stops this test decaying into one that cannot
// fail. If a future Go release ever ships an http.DefaultTransport with no
// proxy function, `t.Proxy = nil` in newDialGuardTransport would be removing
// nothing, the three assertions below would pass on a transport built any way
// at all, and this test would report a control that no longer exists.
func TestGuardedTransportsCarryNoProxy(t *testing.T) {
	t.Parallel()

	def, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("http.DefaultTransport is %T, not *http.Transport; this file's clone-and-harden "+
			"construction no longer describes what runs", http.DefaultTransport)
	}
	if def.Proxy == nil {
		t.Fatal("http.DefaultTransport carries no Proxy function, so clearing it proves nothing " +
			"and the assertions below cannot fail for the reason this test exists")
	}

	if dialGuardTransport.Proxy != nil {
		t.Error("the shared dialGuardTransport carries a Proxy function: with a proxy in the path " +
			"the dialer's Control hook only ever sees the proxy's address, so the metadata guard " +
			"never sees 169.254.169.254 and is inert")
	}
	if tr := newDialGuardTransport(); tr.Proxy != nil {
		t.Error("newDialGuardTransport returned a transport carrying a Proxy function")
	}
	// The CA-pinned per-upstream transport is a clone of the shared one
	// (tlsconfig.go), so it inherits this; asserted rather than assumed, because
	// the whole reason upstreamTransport clones is that a hand-built transport
	// silently drops exactly these properties.
	pinned, err := upstreamTransport(testCAPEM)
	if err != nil {
		t.Fatalf("upstreamTransport: %v", err)
	}
	defer pinned.CloseIdleConnections()
	if pinned.Proxy != nil {
		t.Error("the CA-pinned upstream transport carries a Proxy function")
	}
}

// TestProxyPutsTheMetadataAddressOutOfTheGuardsView is the evidence behind
// TestGuardedTransportsCarryNoProxy, and it is the half a reader needs to
// believe the fix: it shows the bypass happening rather than asserting that it
// would.
//
// One transport, built exactly as production builds it, is driven twice against
// the same metadata URL. Without a proxy the Control hook refuses the dial.
// With a proxy attached, the identical transport connects to the proxy, hands
// it the metadata host in the request line, and the guard never runs on that
// address at all: the dial it inspected was to 127.0.0.1, which it correctly
// allows.
//
// It is a canary as much as a demonstration. If a future net/http gave Control
// visibility of the origin address through a proxy, this test goes red and the
// trade recorded in newDialGuardTransport's comment (an operator's egress proxy
// given up to keep the guard) would need revisiting.
func TestProxyPutsTheMetadataAddressOutOfTheGuardsView(t *testing.T) {
	t.Parallel()

	proxied := make(chan string, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A proxied absolute-form request line carries the real destination
		// here; the proxy, not this process, would do the DNS lookup and the
		// onward connection.
		select {
		case proxied <- r.Host:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	const metadataURL = "http://169.254.169.254/mcp"

	tr := newDialGuardTransport()
	defer tr.CloseIdleConnections()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if _, err := tr.RoundTrip(req); !errors.Is(err, errMetadataBlocked) {
		t.Fatalf("without a proxy the guard must refuse %s, got: %v", metadataURL, err)
	}

	// Same transport, same URL, one field changed.
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	tr.Proxy = func(*http.Request) (*url.URL, error) { return proxyURL, nil }

	req2, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := tr.RoundTrip(req2)
	if errors.Is(err, errMetadataBlocked) {
		t.Fatalf("the dial guard refused a PROXIED request to %s. That is better than the "+
			"documented behaviour, and it means newDialGuardTransport's stated reason for "+
			"clearing Proxy is now wrong: %v", metadataURL, err)
	}
	if err != nil {
		t.Fatalf("proxied request failed for an unrelated reason: %v", err)
	}
	defer resp.Body.Close()

	select {
	case host := <-proxied:
		if host != "169.254.169.254" {
			t.Fatalf("the proxy was reached but for host %q, not the metadata address; this test "+
				"is no longer demonstrating what it claims", host)
		}
	default:
		t.Fatal("the proxied request returned without the proxy handler ever running")
	}
}
