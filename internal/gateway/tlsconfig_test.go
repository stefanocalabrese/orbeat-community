package gateway

import (
	"crypto/x509"
	"errors"
	"testing"
)

// A self-signed CA generated solely for these tests; never dialled.
//
//	openssl req -x509 -newkey rsa:2048 -nodes -keyout /dev/null \
//	  -out ca.pem -days 36500 -subj "/CN=orbeat-test-ca"
const testCAPEM = `-----BEGIN CERTIFICATE-----
MIICsDCCAZgCCQDwjrRPGGhT4TANBgkqhkiG9w0BAQsFADAZMRcwFQYDVQQDDA5v
cmJlYXQtdGVzdC1jYTAgFw0yNjA4MTcxMTQxMTVaGA8yMTI2MDcyNDExNDExNVow
GTEXMBUGA1UEAwwOb3JiZWF0LXRlc3QtY2EwggEiMA0GCSqGSIb3DQEBAQUAA4IB
DwAwggEKAoIBAQDFmz6BTVh2ogZsiDRh7arl8ytvaBMW+kxIvcOUB/RgPYZl+u9H
flJiJXNYdnLdaWJOMMBD3MSKYbaC8fRVlo4aWgNz8OgNzJIe6sEKjFXJ5MMDnXDU
dJblIC6Pg4z1g0pWZ6jMJ5eEo2HNRQLwGXsXWZkMZjMtRYLIrKZUjZ6UT4lgNkt2
QcyVc3mG+/WZyDlnYwz/ES5sZa9HntTPxBt83sx3e+Sbtmxoo9KbVMAoTbBJJgig
0eoaSelnNieKCtm2p6p4ZfvSuocdigbqSFDd6V/Gx0fwNWcxGAJowATRk0yWYsrg
dmMFMXuAdUk04kvy2zFwK1c/b8FATSaaqKBpAgMBAAEwDQYJKoZIhvcNAQELBQAD
ggEBAEyUAzMo+JVfoPdDT6RgFML4pjMDGIxaFX7IzPrjcHpfZl+e+GbMDSZEJfY0
mKCl/cAzDz6fCOJwRSPaXayJOZoz330EvAm4hg2DTP6qaxh2Xsv/fLnMDv/XKcNi
upCo4bVwxYrQhhNtJLGZM88Yo/0PhPHYOfLLQzoQauLnzCWQZ3yNSMXHHSFD3zcq
l+W4Uzmut1/LC9aTF7i0pm+nbCi4iQYanwM/8vkt4iqyqxcUr2pyqXZITfFKj6VS
wXQHQNf9rAOy79xXSS41e3Ux0Vdpu5eR7YSQtxv6/urG4awtP27k2QgJQoI8dlDJ
47yKRr3MEQK3SAVRR1YghItBHME=
-----END CERTIFICATE-----
`

// TestUpstreamTransportKeepsTheDialGuard is the assertion that matters. A
// CA-configured upstream must STILL refuse a cloud metadata address. Asserting
// only that the CA was applied would pass on a transport built from scratch,
// which is exactly how this ships broken (spec §6) — and how the guard itself
// shipped wired to nothing in v1.25.0 with the whole package green.
func TestUpstreamTransportKeepsTheDialGuard(t *testing.T) {
	tr, err := upstreamTransport(testCAPEM)
	if err != nil {
		t.Fatalf("upstreamTransport: %v", err)
	}
	_, dialErr := tr.DialContext(t.Context(), "tcp", "169.254.169.254:80")
	if !errors.Is(dialErr, errMetadataBlocked) {
		t.Fatalf("CA-configured transport dialled a metadata address: err = %v, want errMetadataBlocked", dialErr)
	}
}

// TestUpstreamTransportReplacesTheSystemPool pins the trust model: RootCAs holds
// ONLY the configured CA, so a publicly-trusted certificate for the same host is
// refused (spec §7). This is pinning, not widening.
func TestUpstreamTransportReplacesTheSystemPool(t *testing.T) {
	tr, err := upstreamTransport(testCAPEM)
	if err != nil {
		t.Fatal(err)
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.RootCAs == nil {
		t.Fatal("RootCAs not set")
	}
	if tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("InsecureSkipVerify must never be set")
	}

	// CertPool.Subjects() is deprecated AND, on this platform (darwin),
	// x509.SystemCertPool().Subjects() measurably returns zero entries
	// (verified empirically: the system pool is a lazy, non-enumerable
	// object on macOS) — so a count comparison against it is not a
	// real assertion here; it would pass on both correct and mutant code.
	//
	// CertPool.Equal (Go 1.19+) compares two pools by their actual DER
	// contents (and an internal systemPool flag), so it discriminates a
	// pool holding ONLY the configured CA from one holding the system
	// roots PLUS the configured CA — exactly the mutation this test must
	// catch (spec §7's "replaces, not extends"). Verified against both
	// shapes before relying on it here.
	wantOnly := x509.NewCertPool()
	if !wantOnly.AppendCertsFromPEM([]byte(testCAPEM)) {
		t.Fatal("test setup: failed to build the reference single-CA pool")
	}
	if !tr.TLSClientConfig.RootCAs.Equal(wantOnly) {
		t.Error("RootCAs is not exactly {testCAPEM}; it looks like it EXTENDS a broader pool (e.g. the system pool) rather than REPLACING it")
	}
}

// TestUpstreamTransportRejectsUnparseablePEM proves AppendCertsFromPEM's bool is
// checked. It returns false when NO certificate parsed; ignoring it yields an
// empty pool that trusts nothing and surfaces as a baffling handshake error at
// dial time instead of a clean, nameable skip (spec §8).
func TestUpstreamTransportRejectsUnparseablePEM(t *testing.T) {
	for _, bad := range []string{
		"",
		"not a pem at all",
		"-----BEGIN CERTIFICATE-----\nnope\n-----END CERTIFICATE-----\n",
	} {
		if _, err := upstreamTransport(bad); err == nil {
			t.Errorf("upstreamTransport(%q) returned nil error, want a failure", bad)
		}
	}
}
