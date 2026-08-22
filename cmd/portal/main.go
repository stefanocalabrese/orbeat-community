// Command portal serves the built orbeat portal SPA (embedded dist) with an
// index.html fallback for client-side routes, security + cache headers, and
// gzip compression.
package main

import (
	"compress/gzip"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/logging"
)

//go:embed all:dist
var distFS embed.FS

// buildCSP assembles the Content-Security-Policy. connectSrc lists the
// cross-origin endpoints the SPA talks to (api, OIDC issuer, gateway) —
// sourced from ORBEAT_PORTAL_CONNECT_SRC. With no extra origins the policy is
// still emitted ('self' only), which blocks the SPA's cross-origin API/OIDC
// calls: deployments MUST set the env var (compose does).
//
// script-src 'self' works because index.html has no inline scripts (the theme
// bootstrap lives in /theme-init.js). style-src 'self' works because the
// Tailwind build emits a single external stylesheet and the app uses no inline
// <style> or style attributes.
// parseConnectSrc splits ORBEAT_PORTAL_CONNECT_SRC into origin tokens. Tokens
// must be bare origins (scheme://host[:port]); a ';' would terminate the
// connect-src directive and inject an operator-typo'd CSP directive, so it is
// rejected outright.
func parseConnectSrc(env string) ([]string, error) {
	tokens := strings.Fields(env)
	for _, tok := range tokens {
		if strings.Contains(tok, ";") {
			return nil, fmt.Errorf("token %q contains ';' — tokens must be bare origins like http://localhost:8080", tok)
		}
	}
	return tokens, nil
}

func buildCSP(connectSrc []string) string {
	connect := "'self'"
	if len(connectSrc) > 0 {
		connect += " " + strings.Join(connectSrc, " ")
	}
	return "default-src 'self'; script-src 'self'; style-src 'self'; connect-src " + connect +
		"; frame-ancestors 'none'; base-uri 'self'; object-src 'none'"
}

func securityHeaders(csp string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// compressibleType reports whether a Content-Type benefits from gzip.
// Already-compressed formats (png, jpeg, woff2, …) are excluded by not being
// listed.
func compressibleType(ct string) bool {
	for _, p := range []string{
		"text/", // html, css, plain, javascript (Go's FileServer uses text/javascript)
		"application/javascript",
		"application/json",
		"image/svg+xml",
	} {
		if strings.HasPrefix(ct, p) {
			return true
		}
	}
	return false
}

// gzipMinSize is the smallest Content-Length worth compressing: below ~one
// MTU the gzip framing overhead can exceed the savings (a tiny response gets
// LARGER), and the transfer fits one packet either way.
const gzipMinSize = 1400

// gzipResponseWriter compresses the body iff the response turns out to be a
// 200 with a compressible Content-Type, no prior Content-Encoding, and a body
// not known to be tiny. The decision is deferred to WriteHeader time, when the
// FileServer has set the final headers (including Content-Length for files).
type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
}

// tooSmallToGzip reports whether a declared Content-Length is below the
// compression floor. An absent/unparseable Content-Length compresses (size
// unknown → assume it's worth it).
func tooSmallToGzip(cl string) bool {
	if cl == "" {
		return false
	}
	n, err := strconv.Atoi(cl)
	return err == nil && n < gzipMinSize
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	if g.wroteHeader {
		return
	}
	g.wroteHeader = true
	h := g.Header()
	if code == http.StatusOK && h.Get("Content-Encoding") == "" && compressibleType(h.Get("Content-Type")) &&
		!tooSmallToGzip(h.Get("Content-Length")) {
		h.Del("Content-Length") // the compressed length differs; let chunked encoding handle it
		h.Set("Content-Encoding", "gzip")
		g.gz = gzip.NewWriter(g.ResponseWriter)
	}
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	if g.gz != nil {
		return g.gz.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

func (g *gzipResponseWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

func (g *gzipResponseWriter) close() error {
	if g.gz != nil {
		return g.gz.Close()
	}
	return nil
}

func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		// GET only: a HEAD body is never written, so wrapping it would emit an
		// empty gzip frame whose length leaks into Content-Length.
		if r.Method != http.MethodGet || !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer func() {
			if err := gw.close(); err != nil {
				slog.Warn("gzip flush", "err", err)
			}
		}()
		next.ServeHTTP(gw, r)
	})
}

// portalConfig is the runtime configuration the SPA fetches at boot from
// /config.json, so a single built portal image runs at any domain (the API/
// OIDC/gateway URLs are no longer baked in at Vite build time).
type portalConfig struct {
	APIBase           string `json:"apiBase"`
	GatewayURL        string `json:"gatewayUrl"`
	OIDCAuthority     string `json:"oidcAuthority"`
	OIDCClientID      string `json:"oidcClientId"`
	MarketplaceSource string `json:"marketplaceSource"`
}

func newHandler(sub fs.FS, connectSrc []string, cfg portalConfig) http.Handler {
	files := http.FileServerFS(sub)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		const body = `{"status":"ok"}`
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		// Declared so the gzip layer can see it's below the compression floor.
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write([]byte(body))
	})
	cfgJSON, _ := json.Marshal(cfg) // struct with string fields never fails to marshal
	mux.HandleFunc("GET /config.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Length", strconv.Itoa(len(cfgJSON)))
		_, _ = w.Write(cfgJSON)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p != "" {
			if _, err := fs.Stat(sub, p); err != nil {
				// Asset-ish paths 404; route-ish paths fall back to the SPA shell.
				if strings.Contains(p, ".") {
					http.NotFound(w, r)
					return
				}
				r.URL.Path = "/" // SPA fallback → index.html
				p = ""
			}
		}
		// Vite content-hashes everything under assets/ → cache forever.
		// Everything else (index.html shell, favicon, theme-init.js) must
		// revalidate so a redeploy is picked up immediately.
		if strings.HasPrefix(p, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
	// Chain order: logging outermost (wrapped in main — times the full request
	// incl. the gzip flush), then securityHeaders, then gzip innermost so its
	// WriteHeader-time decision sees the handler's final Content-Type/Length.
	return securityHeaders(buildCSP(connectSrc), gzipMiddleware(mux))
}

// healthProbe performs a GET on this service's own /healthz and returns 0 on a
// 200, 1 otherwise. It reads the listen address from ORBEAT_HTTP_ADDR (the same
// source the server uses), defaulting to defAddr; an empty/wildcard host is
// dialed as 127.0.0.1.
func healthProbe(defAddr string) int {
	addr := os.Getenv("ORBEAT_HTTP_ADDR")
	if addr == "" {
		addr = defAddr
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: bad ORBEAT_HTTP_ADDR:", err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: /healthz returned", resp.StatusCode)
		return 1
	}
	return 0
}

func main() {
	// `-healthcheck` is the container self-probe: GET our own /healthz and exit
	// 0/1. The distroless image has no shell or curl, so the compose healthcheck
	// invokes the binary itself. Handle it before any other startup work.
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(healthProbe(":8081"))
	}

	logger := logging.New(os.Stdout, os.Getenv("ORBEAT_LOG_FORMAT"), os.Getenv("ORBEAT_LOG_LEVEL")).With("service", "orbeat-portal")
	slog.SetDefault(logger)

	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		slog.Error("embed", "err", err)
		os.Exit(1)
	}

	// Space-separated origins the SPA is allowed to call (CSP connect-src):
	// the api, the OIDC issuer, and the gateway. Unset → 'self' only, which
	// breaks the SPA's cross-origin calls; warn loudly.
	connectSrc, err := parseConnectSrc(os.Getenv("ORBEAT_PORTAL_CONNECT_SRC"))
	if err != nil {
		slog.Error("ORBEAT_PORTAL_CONNECT_SRC", "err", err)
		os.Exit(1)
	}
	if len(connectSrc) == 0 {
		slog.Warn("ORBEAT_PORTAL_CONNECT_SRC is unset; CSP connect-src is 'self' only — browser will block the SPA's api/OIDC/gateway calls")
	}

	// Runtime config the SPA fetches from /config.json at boot — lets one
	// built image run at any domain instead of baking URLs at Vite build time.
	cfg := portalConfig{
		APIBase:           os.Getenv("ORBEAT_PORTAL_API_BASE"),
		GatewayURL:        os.Getenv("ORBEAT_PORTAL_GATEWAY_URL"),
		OIDCAuthority:     os.Getenv("ORBEAT_PORTAL_OIDC_AUTHORITY"),
		OIDCClientID:      os.Getenv("ORBEAT_PORTAL_OIDC_CLIENT_ID"),
		MarketplaceSource: os.Getenv("ORBEAT_PORTAL_MARKETPLACE_SOURCE"),
	}

	addr := os.Getenv("ORBEAT_HTTP_ADDR")
	if addr == "" {
		addr = ":8081"
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           logging.Requests(logger, nil)(newHandler(sub, connectSrc, cfg)),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	slog.Info("listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("server", "err", err)
		os.Exit(1)
	}
}
