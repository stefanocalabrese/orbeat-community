package main

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// testDist mimics a built Vite dist: SPA shell + content-hashed assets. The
// JS bundle is padded past gzipMinSize so it is eligible for compression; the
// other files are realistically tiny (below the floor → served identity).
func testDist() fs.FS {
	bundle := "console.log('orbeat portal bundle');" + strings.Repeat("/* pad */", 400)
	return fstest.MapFS{
		"index.html":             {Data: []byte("<!doctype html><div id=root></div>")},
		"favicon.svg":            {Data: []byte("<svg></svg>")},
		"assets/app-abc123.js":   {Data: []byte(bundle)},
		"assets/app-abc123.css":  {Data: []byte(".x{color:red}")},
		"assets/logo-def456.png": {Data: []byte("not-really-a-png")},
		"theme-init.js":          {Data: []byte("(function(){})();")},
	}
}

func get(t *testing.T, h http.Handler, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSPAServer(t *testing.T) {
	h := newHandler(testDist(), nil, portalConfig{})
	cases := []struct {
		path     string
		wantCode int
		wantBody string // substring (case-insensitive); "" = don't check
	}{
		{"/healthz", http.StatusOK, `"status":"ok"`},
		{"/", http.StatusOK, "<!doctype html>"},
		{"/catalog", http.StatusOK, "<!doctype html>"},       // SPA fallback
		{"/admin/servers", http.StatusOK, "<!doctype html>"}, // deep link fallback
		{"/assets/nope.js", http.StatusNotFound, ""},         // missing asset is a real 404
		{"/assets/app-abc123.js", http.StatusOK, "bundle"},
	}
	for _, c := range cases {
		rec := get(t, h, c.path, nil)
		if rec.Code != c.wantCode {
			t.Fatalf("%s = %d, want %d", c.path, rec.Code, c.wantCode)
		}
		if c.wantBody != "" && !containsFold(rec.Body.String(), c.wantBody) {
			t.Fatalf("%s body missing %q", c.path, c.wantBody)
		}
	}
}

// The real embedded dist (placeholder or built) must still serve.
func TestEmbeddedDistServes(t *testing.T) {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		t.Fatal(err)
	}
	h := newHandler(sub, nil, portalConfig{})
	if rec := get(t, h, "/", nil); rec.Code != http.StatusOK {
		t.Fatalf("/ = %d, want 200", rec.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newHandler(testDist(), []string{"http://localhost:8080", "http://localhost:8088", "http://localhost:8090"}, portalConfig{})
	for _, path := range []string{"/", "/catalog", "/assets/app-abc123.js", "/healthz"} {
		rec := get(t, h, path, nil)
		wantCSP := "default-src 'self'; script-src 'self'; style-src 'self'; " +
			"connect-src 'self' http://localhost:8080 http://localhost:8088 http://localhost:8090; " +
			"frame-ancestors 'none'; base-uri 'self'; object-src 'none'"
		if got := rec.Header().Get("Content-Security-Policy"); got != wantCSP {
			t.Fatalf("%s CSP = %q, want %q", path, got, wantCSP)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("%s X-Content-Type-Options = %q", path, got)
		}
		if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
			t.Fatalf("%s Referrer-Policy = %q", path, got)
		}
		if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Fatalf("%s X-Frame-Options = %q", path, got)
		}
	}
}

func TestSecurityHeadersWithoutConnectSrc(t *testing.T) {
	h := newHandler(testDist(), nil, portalConfig{})
	rec := get(t, h, "/", nil)
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "connect-src 'self';") {
		t.Fatalf("CSP without env origins should be connect-src 'self' only, got %q", csp)
	}
}

func TestCacheHeaders(t *testing.T) {
	h := newHandler(testDist(), nil, portalConfig{})
	cases := []struct {
		path string
		want string
	}{
		{"/assets/app-abc123.js", "public, max-age=31536000, immutable"},
		{"/assets/app-abc123.css", "public, max-age=31536000, immutable"},
		{"/", "no-cache"},
		{"/catalog", "no-cache"}, // SPA fallback serves index → must revalidate
		{"/favicon.svg", "no-cache"},
		{"/healthz", "no-store"},
	}
	for _, c := range cases {
		rec := get(t, h, c.path, nil)
		if got := rec.Header().Get("Cache-Control"); got != c.want {
			t.Fatalf("%s Cache-Control = %q, want %q", c.path, got, c.want)
		}
	}
	// A missing asset 404 must never be marked immutable.
	rec := get(t, h, "/assets/nope.js", nil)
	if got := rec.Header().Get("Cache-Control"); strings.Contains(got, "immutable") {
		t.Fatalf("404 Cache-Control = %q, must not be immutable", got)
	}
}

func TestGzipNegotiation(t *testing.T) {
	h := newHandler(testDist(), nil, portalConfig{})

	// With Accept-Encoding: gzip → compressed, no Content-Length lie, Vary set.
	rec := get(t, h, "/assets/app-abc123.js", map[string]string{"Accept-Encoding": "gzip, deflate, br"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if !strings.Contains(rec.Header().Get("Vary"), "Accept-Encoding") {
		t.Fatalf("Vary = %q, want Accept-Encoding", rec.Header().Get("Vary"))
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("body is not valid gzip: %v", err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	if !strings.Contains(string(body), "bundle") {
		t.Fatalf("gunzipped body = %q", body)
	}

	// Without Accept-Encoding → identity.
	rec = get(t, h, "/assets/app-abc123.js", nil)
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("identity Content-Encoding = %q, want empty", got)
	}
	if !strings.Contains(rec.Body.String(), "bundle") {
		t.Fatalf("identity body = %q", rec.Body.String())
	}

	// Already-compressed content type (png) → never re-compressed.
	rec = get(t, h, "/assets/logo-def456.png", map[string]string{"Accept-Encoding": "gzip"})
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("png Content-Encoding = %q, want empty", got)
	}

	// Tiny responses (below gzipMinSize: the small index shell, healthz JSON)
	// stay identity — gzip framing would make them larger.
	for _, path := range []string{"/", "/healthz"} {
		rec = get(t, h, path, map[string]string{"Accept-Encoding": "gzip"})
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("%s Content-Encoding = %q, want empty (below min size)", path, got)
		}
	}

	// 404 (non-200) responses are not compressed.
	rec = get(t, h, "/assets/nope.js", map[string]string{"Accept-Encoding": "gzip"})
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("404 Content-Encoding = %q, want empty", got)
	}

	// HEAD is never wrapped (no body → an empty gzip frame would leak into
	// Content-Length).
	req := httptest.NewRequest(http.MethodHead, "/assets/app-abc123.js", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	hrec := httptest.NewRecorder()
	h.ServeHTTP(hrec, req)
	if got := hrec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("HEAD Content-Encoding = %q, want empty", got)
	}
}

func TestParseConnectSrc(t *testing.T) {
	got, err := parseConnectSrc("  http://localhost:8080   http://localhost:8088 ")
	if err != nil || len(got) != 2 || got[0] != "http://localhost:8080" || got[1] != "http://localhost:8088" {
		t.Fatalf("parseConnectSrc = %v, %v", got, err)
	}
	if got, err := parseConnectSrc(""); err != nil || len(got) != 0 {
		t.Fatalf("empty env = %v, %v; want empty, nil", got, err)
	}
	// A ';' would terminate connect-src and inject a new CSP directive.
	if _, err := parseConnectSrc("http://localhost:8080; script-src *"); err == nil {
		t.Fatal("token with ';' must be rejected")
	}
}

func TestBuildCSP(t *testing.T) {
	got := buildCSP([]string{"http://a", "http://b"})
	want := "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self' http://a http://b; frame-ancestors 'none'; base-uri 'self'; object-src 'none'"
	if got != want {
		t.Fatalf("buildCSP = %q, want %q", got, want)
	}
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func TestConfigJSONHandler(t *testing.T) {
	cfg := portalConfig{
		APIBase:           "https://orbeat.example.com",
		GatewayURL:        "https://orbeat.example.com/mcp",
		OIDCAuthority:     "https://auth.orbeat.example.com/realms/orbeat",
		OIDCClientID:      "orbeat-portal",
		MarketplaceSource: "./marketplace",
	}
	// Empty dist FS is fine; we only hit /config.json.
	h := newHandler(fstest.MapFS{}, nil, cfg)

	req := httptest.NewRequest(http.MethodGet, "/config.json", nil)
	req.Header.Set("Accept-Encoding", "identity") // avoid gzip for the assertion
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	var got portalConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, rec.Body.String())
	}
	if got != cfg {
		t.Errorf("config = %+v, want %+v", got, cfg)
	}
}
