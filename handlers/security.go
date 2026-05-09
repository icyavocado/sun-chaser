package handlers

import "net/http"

// SecurityHeaders is a middleware that sets standard HTTP security headers on
// every response. HSTS is intentionally omitted — Traefik handles TLS
// termination and is responsible for the Strict-Transport-Security header.
//
// CSP allows:
//   - unpkg.com  — HTMX
//   - cdn.jsdelivr.net — Pico.css and Chart.js
//   - 'unsafe-inline' — inline <script> blocks in html/template partials
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self' 'unsafe-inline' https://unpkg.com https://cdn.jsdelivr.net; "+
				"style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; "+
				"img-src 'self' data:; "+
				"font-src 'self' https://cdn.jsdelivr.net; "+
				"connect-src 'self'",
		)
		next.ServeHTTP(w, r)
	})
}
