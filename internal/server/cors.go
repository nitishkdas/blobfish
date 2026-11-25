package server

import (
	"net/http"
	"slices"
)

// corsMiddleware lets web pages on allowed origins call BlobFish from the
// browser, typically to upload straight to a presigned PUT URL instead of
// through the app's own backend.
//
// Browsers ask first with a preflight (OPTIONS + Access-Control-Request-*).
// Allowed origins get permission; others get 403 and the browser blocks the
// real request. BlobFish authenticates with keys and signed URLs, never
// cookies, so no credentialed mode is needed and "*" is acceptable.
func corsMiddleware(allowed []string) func(http.Handler) http.Handler {
	anyOrigin := slices.Contains(allowed, "*")
	ok := func(origin string) bool { return anyOrigin || slices.Contains(allowed, origin) }
	return func(next http.Handler) http.Handler {
		if len(allowed) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			h := w.Header()
			h.Add("Vary", "Origin")
			preflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
			if !ok(origin) {
				if preflight {
					writeError(w, r, http.StatusForbidden, CodeCORSNotAllowed, "origin "+origin+" is not in cors_allowed_origins")
					return
				}
				next.ServeHTTP(w, r) // no CORS headers: the browser hides the response
				return
			}
			if anyOrigin {
				h.Set("Access-Control-Allow-Origin", "*")
			} else {
				h.Set("Access-Control-Allow-Origin", origin)
			}
			if preflight {
				h.Set("Access-Control-Allow-Methods", "GET, HEAD, PUT, POST, PATCH, DELETE")
				// Echo the requested headers (Authorization, Content-Type,
				// Range, X-Meta-*...): the origin is trusted, and X-Meta-*
				// names can't be listed in advance.
				if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
					h.Set("Access-Control-Allow-Headers", req)
				}
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			// Let pages read the headers they need (ETag = SHA-256 after an
			// upload, Content-Range for media, X-Request-Id for support).
			h.Set("Access-Control-Expose-Headers",
				"ETag, Content-Range, Content-Length, Accept-Ranges, Last-Modified, Content-Disposition, X-Request-Id, *")
			next.ServeHTTP(w, r)
		})
	}
}
