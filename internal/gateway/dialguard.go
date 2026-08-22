package gateway

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// errMetadataBlocked is the sentinel wrapped into every error blockMetadataDial
// returns, so callers (and tests) can distinguish "the dial guard refused
// this" from an ordinary network failure via errors.Is.
var errMetadataBlocked = errors.New("gateway: dial guard: refusing to dial cloud metadata address")

// awsIPv6MetadataAddr is AWS's IPv6 instance-metadata address. Parsed once at
// init instead of on every dial attempt.
var awsIPv6MetadataAddr = net.ParseIP("fd00:ec2::254")

// dialGuardTransport is a clone of http.DefaultTransport (the global is never
// mutated — see connectUpstream's use of it in broker.go) whose outbound
// connections additionally refuse cloud-provider instance-metadata
// endpoints. It is package-level and shared across every upstream dial, same
// as the DefaultTransport it wraps.
var dialGuardTransport *http.Transport = newDialGuardTransport()

// newDialGuardTransport clones http.DefaultTransport and swaps in a dialer
// whose Control hook blocks metadata destinations. See blockMetadataDial for
// what is blocked, and why private ranges deliberately are not.
func newDialGuardTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// http.DefaultTransport has been documented as *http.Transport since
		// Go 1.0; this branch is unreachable in practice. Fail closed with a
		// bare Transport carrying only the guard rather than panicking the
		// process over a defensive type assertion.
		base = &http.Transport{}
	}
	t := base.Clone()
	// Timeout/KeepAlive match the net.Dialer http.DefaultTransport itself
	// builds internally (both 30s) — the guard changes what is dialed, not
	// how long dialling is allowed to take.
	t.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   blockMetadataDial,
	}).DialContext
	return t
}

// blockMetadataDial is a net.Dialer Control hook. The standard library
// invokes it AFTER DNS resolution, once per connection attempt, with the
// concrete address about to be dialed — that is the security boundary this
// file exists for.
//
// Write-time endpoint validation (internal/api/admin_servers.go
// validEndpoint) only ever sees the configured URL STRING, before any DNS
// lookup: it cannot know that evil.example.com resolves to
// 169.254.169.254. Any literal-IP check there would be trivially bypassed by
// DNS, including DNS rebinding (the address changing between validation time
// and dial time) or a multi-A/AAAA-record response picking a different
// address on a later attempt. Running the check here, on every dial attempt
// post-resolution, closes both.
//
// Blocks:
//   - IPv4 link-local, 169.254.0.0/16 — where 169.254.169.254 lives, used by
//     AWS, GCP, Azure and DigitalOcean's instance-metadata services
//   - IPv6 link-local, fe80::/10
//   - AWS's IPv6 metadata address, fd00:ec2::254 — a single address, not a
//     wider block: it lives in Unique Local Address space, and ULA is
//     ordinary private IPv6 addressing (see the no-private-block note below)
//
// net.IP.IsLinkLocalUnicast covers both link-local ranges in one call (it is
// the same predicate the standard library itself defines for "169.254/16 or
// fe80::/10"), so only the AWS IPv6 address needs an explicit check.
//
// Deliberately does NOT block RFC 1918 private ranges (10.0.0.0/8,
// 172.16.0.0/12, 192.168.0.0/16) or loopback. orbeat is a self-hosted
// orchestrator: its PRIMARY deployment shape is internal MCP servers running
// on a private network the admin controls, not an attacker's — that is the
// common case, not an edge case. Blocking private ranges would break normal
// operation for essentially every enterprise install. The threat this guard
// closes is a malicious or compromised upstream endpoint reaching the CLOUD
// PROVIDER's own control plane (instance-credential theft), not the admin's
// own internal network. Do NOT widen this into a general private-range
// denylist "for defense in depth" — it would not add security here, only
// break the primary deployment shape.
func blockMetadataDial(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		// Control only ever runs with an address net.Dialer itself built
		// (host:port, post-resolution). A SplitHostPort failure here would
		// mean the standard library changed its contract; fail closed rather
		// than dial an address we couldn't parse.
		return fmt.Errorf("gateway: dial guard: unparseable address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Control runs post-resolution: host is always a literal IP by this
		// point. Fail closed on anything else.
		return fmt.Errorf("gateway: dial guard: non-IP host %q", host)
	}
	if ip.IsLinkLocalUnicast() || ip.Equal(awsIPv6MetadataAddr) {
		return fmt.Errorf("%w: %s", errMetadataBlocked, ip)
	}
	return nil
}
