package api

import "net/http"

// corsMiddleware allows browser cross-origin calls from an exact-match origin
// allow-list. Empty list = no CORS headers at all (fail closed: browsers block
// cross-origin; same-origin and non-browser clients are unaffected). The
// matched origin is echoed back (never "*": requests carry Authorization).
func corsMiddleware(origins []string) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(origins))
	for _, o := range origins {
		if o != "" {
			allowed[o] = struct{}{}
		}
	}
	return func(next http.Handler) http.Handler {
		if len(allowed) == 0 {
			return next // CORS disabled: true pass-through.
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Vary on Origin for every response we can influence, so a shared cache
			// never serves one origin's CORS response (or lack of one) to another.
			w.Header().Add("Vary", "Origin")
			origin := r.Header.Get("Origin")
			if _, ok := allowed[origin]; ok {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				if r.Method == http.MethodOptions {
					// Any OPTIONS from an allowed origin is answered as a preflight;
					// we don't gate on Access-Control-Request-Method (no OPTIONS
					// routes exist, and the allow-list already constrains callers).
					h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
					h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-Id, If-Match")
					h.Set("Access-Control-Max-Age", "600")
					w.WriteHeader(http.StatusNoContent)
					return
				}
				// Non-preflight response: neither of these is in the CORS
				// response-header safelist (Cache-Control, Content-Language,
				// Content-Length, Content-Type, Expires, Last-Modified, Pragma), so
				// without exposing them a cross-origin res.headers.get(...) is null
				// in JS for both. ETag: the optimistic-concurrency client
				// (admin_servers.go, admin_artifacts.go) can never read the version
				// to send back as If-Match. X-Orbeat-Export-Truncated: the audit
				// export truncation warning (admin_audit.go, read by
				// AuditPage.tsx) silently never fires — it did not fire in dev or
				// CI from v1.8.0 until this line was added, because dev/CI are
				// cross-origin (see corsMiddleware's doc) and this was missing.
				// Grep before adding a third: any other custom (non-safelisted)
				// response header a client reads across the origin boundary
				// belongs in this list too; one that's set but never read (e.g.
				// Content-Disposition on the same export) does not.
				h.Set("Access-Control-Expose-Headers", "ETag, X-Orbeat-Export-Truncated")
			}
			next.ServeHTTP(w, r)
		})
	}
}
