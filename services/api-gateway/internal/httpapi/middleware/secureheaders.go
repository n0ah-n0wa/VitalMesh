package middleware

import "net/http"

// SecureHeaders sets the response headers appropriate for a JSON API that
// never serves browser content. HSTS is not set here: it belongs to the TLS
// terminator, which knows whether the connection was actually encrypted.
func SecureHeaders() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			h.Set("Cache-Control", "no-store")
			next.ServeHTTP(w, r)
		})
	}
}
