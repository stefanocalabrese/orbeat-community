package gateway

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
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

// alibabaMetadataAddr is Alibaba Cloud's instance-metadata address (fable-
// audit B38(d)) -- a single ordinary IPv4 address, not link-local, so
// net.IP.IsLinkLocalUnicast cannot cover it the way it covers AWS/GCP/Azure/
// DigitalOcean's shared 169.254.169.254. Blocked the same way
// awsIPv6MetadataAddr is: a specific address, not a wider block, for the
// same reason that one is (see its own note in blockMetadataDial's doc
// comment) -- it is ordinary address space Alibaba happens to route its
// metadata service on, not a class this guard has any other reason to deny.
var alibabaMetadataAddr = net.ParseIP("100.100.100.200")

// dialGuardTransport is a clone of http.DefaultTransport (the global is never
// mutated — see connectUpstream's use of it in broker.go) whose outbound
// connections additionally refuse cloud-provider instance-metadata endpoints
// and never go through a proxy. It is package-level and shared across every
// upstream dial, same as the DefaultTransport it wraps.
var dialGuardTransport *http.Transport = newDialGuardTransport()

// proxyEnvVars are the variables http.ProxyFromEnvironment consults, and so the
// ones whose presence used to disable this guard. Names taken from
// golang.org/x/net/http/httpproxy's Config, which net/http delegates to:
// HTTP_PROXY and HTTPS_PROXY, each also honoured lowercase. NO_PROXY is not
// listed because it only ever removes proxying.
var proxyEnvVars = []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"}

// newDialGuardTransport clones http.DefaultTransport and swaps in a dialer
// whose Control hook blocks metadata destinations. See blockMetadataDial for
// what is blocked, and why private ranges deliberately are not.
//
// It also CLEARS Proxy, and that line is a security control, not tidiness
// (audit A16). http.DefaultTransport carries Proxy: http.ProxyFromEnvironment,
// and Clone copies it. blockMetadataDial is a net.Dialer Control hook, so it
// only ever sees the address this process dials: with a proxy configured that
// address is the PROXY's, the proxy performs the DNS lookup and the connection
// to the real destination, and 169.254.169.254 never passes under the hook at
// all. Setting a single environment variable therefore turned the whole guard
// off, with nothing in the logs and every one of its tests still green (they
// dial with no proxy env set, so no mutant could fail them).
//
// Validating post-proxy was the alternative and was rejected as unbuildable
// rather than as merely worse. Once a hostname is handed to a proxy, this
// process never resolves it and never sees the address chosen; net/http exposes
// no hook on the proxy's own onward connection. Any check we could still run
// would be on the URL STRING before the request leaves, which is exactly the
// write-time validation blockMetadataDial's doc comment explains is defeated by
// DNS and by rebinding. A guard that cannot observe the address being connected
// to is not a guard.
//
// The cost is real and is accepted: an operator who needs an egress proxy to
// reach upstream MCP servers cannot get one from the environment any more.
// That is a supported deployment shape, so it is announced rather than silently
// dropped, and an explicit orbeat-owned proxy setting (with the documented
// consequence that the SSRF guard cannot cover it) is the way to give it back
// if anyone asks for it.
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
	t.Proxy = nil
	// Announced, never silent: a proxy env var that used to take effect now
	// does nothing, so an operator debugging "the gateway cannot reach my
	// upstream" is told why here instead of inferring it. Emitted at package
	// init, before cmd/gateway configures slog, so this line arrives in the
	// default text format rather than the JSON the rest of the process emits.
	// That is the trade for a warning that cannot be left un-wired: there is no
	// call site to forget.
	if set := proxyEnvSet(); len(set) > 0 {
		slog.Warn("gateway: proxy environment ignored for upstream dials; the SSRF dial guard "+
			"cannot inspect a destination it never resolves (audit A16)",
			"vars", set)
	}
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

// proxyEnvSet returns the names of the proxy environment variables that are
// set and non-empty. Names only: the values routinely carry credentials.
func proxyEnvSet() []string {
	var set []string
	for _, name := range proxyEnvVars {
		if os.Getenv(name) != "" {
			set = append(set, name)
		}
	}
	return set
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
//   - Alibaba Cloud's metadata address, 100.100.100.200 — also a single
//     address rather than a wider block, for the identical reason: it lives
//     in RFC 6598 Shared Address Space (100.64.0.0/10), which is ordinary
//     non-globally-routable IPv4 addressing an admin's own network could
//     otherwise be using
//
// net.IP.IsLinkLocalUnicast covers both link-local ranges in one call (it is
// the same predicate the standard library itself defines for "169.254/16 or
// fe80::/10"), so only the AWS IPv6 address needs an explicit check.
//
// The hook sees only the address THIS process dials. Anything that puts a
// middlebox between the process and the destination therefore takes the
// destination out of its view entirely, which is why newDialGuardTransport
// clears Proxy, and why that line belongs to this control rather than to
// transport configuration (audit A16).
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
	if ip.IsLinkLocalUnicast() || ip.Equal(awsIPv6MetadataAddr) || ip.Equal(alibabaMetadataAddr) {
		return fmt.Errorf("%w: %s", errMetadataBlocked, ip)
	}
	return nil
}
