package server

import (
	"net/http"
	"slices"
)

// cors applies one bucket's browser-access list (BucketInfo.CORSOrigins)
// to a request, so a web page on an allowed site can call the bucket's
// objects, typically uploading straight to a presigned PUT URL.
//
// Browsers ask first with a preflight (OPTIONS + Access-Control-Request-*).
// cors answers it and returns true when the request is fully handled.
// Origins not on the list get 403 on preflight, and no CORS headers on
// other requests, so the browser blocks them. An unknown bucket looks the
// same as a bucket that doesn't list the origin. BlobFish uses keys and
// signed URLs, never cookies, so no credentialed mode is needed and "*"
// is acceptable.
func (a *api) cors(w http.ResponseWriter, r *http.Request, bucket string) (handled bool) {
	origin := r.Header.Get("Origin")
	preflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
	if origin == "" {
		if r.Method == http.MethodOptions {
			methodNotAllowed(w, r, "GET, HEAD, PUT, DELETE")
			return true
		}
		return false
	}
	h := w.Header()
	h.Add("Vary", "Origin")

	var allowed []string
	if b, err := a.buckets.GetBucket(r.Context(), bucket); err == nil {
		allowed = b.CORSOrigins
	}
	anyOrigin := slices.Contains(allowed, "*")
	if !anyOrigin && !slices.Contains(allowed, origin) {
		if preflight || r.Method == http.MethodOptions {
			writeError(w, r, http.StatusForbidden, CodeCORSNotAllowed,
				"this bucket does not allow browser access from "+origin)
			return true
		}
		return false // no CORS headers: the browser hides the response
	}
	if anyOrigin {
		h.Set("Access-Control-Allow-Origin", "*")
	} else {
		h.Set("Access-Control-Allow-Origin", origin)
	}
	if preflight {
		h.Set("Access-Control-Allow-Methods", "GET, HEAD, PUT, DELETE")
		// Echo the requested headers (Authorization, Content-Type, Range,
		// X-Meta-*...): the origin is trusted and X-Meta-* names vary.
		if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
			h.Set("Access-Control-Allow-Headers", req)
		}
		h.Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	// Let pages read what they need: ETag (SHA-256) after an upload,
	// Content-Range for media, X-Request-Id for support.
	h.Set("Access-Control-Expose-Headers",
		"ETag, Content-Range, Content-Length, Accept-Ranges, Last-Modified, Content-Disposition, X-Request-Id, *")
	return false
}
