package gateway

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
)

// errBadCAPEM is returned when a resolved CA reference does not parse as at
// least one certificate. Distinguished from a resolve failure so the skip log
// can name which of the two happened.
var errBadCAPEM = errors.New("gateway: tls ca ref did not parse as a PEM certificate")

// upstreamTransport builds a transport that verifies an upstream against ONLY
// the supplied CA, replacing the system pool (spec §7 — pinning, not widening).
//
// It CLONES dialGuardTransport rather than constructing a transport, and that is
// load-bearing: TLS settings live on the transport, and the SSRF dial guard
// lives on the same object. Building from http.DefaultTransport or a bare
// &http.Transport{} would silently drop the guard for exactly the upstreams that
// opted into a custom CA. TestUpstreamTransportKeepsTheDialGuard exists to fail
// if anyone does that.
//
// Callers MUST CloseIdleConnections() the result when the owning session is torn
// down: unlike the shared dialGuardTransport this is per-session, so an unclosed
// one leaks a connection pool per session build.
func upstreamTransport(caPEM string) (*http.Transport, error) {
	pool := x509.NewCertPool()
	// AppendCertsFromPEM reports false when NO certificate parsed. Ignoring it
	// yields an empty pool, which trusts nothing and fails at handshake time
	// with an opaque error instead of here with a nameable one.
	if !pool.AppendCertsFromPEM([]byte(caPEM)) {
		return nil, errBadCAPEM
	}
	t := dialGuardTransport.Clone()
	t.TLSClientConfig = &tls.Config{RootCAs: pool}
	return t, nil
}
