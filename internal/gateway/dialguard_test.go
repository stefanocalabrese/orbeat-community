package gateway

import (
	"context"
	"errors"
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
