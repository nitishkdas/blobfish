// Package server holds the BlobFish HTTP router, middleware and the shared
// JSON error format. Frontends registered here must reach data only through
// the Store and Authenticator interfaces, never bbolt or files directly.
package server

import (
	"log/slog"
	"net/http"
)

// Options are the dependencies and settings the HTTP layer needs.
type Options struct {
	Logger        *slog.Logger
	MaxObjectSize int64
}

// New returns the root handler with all routes and middleware applied.
func New(opts Options) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("/", handleNotFound)

	// Outermost first: every request gets an ID, then a log line, then panic
	// protection, then the body limit.
	return chain(mux,
		requestIDMiddleware,
		logMiddleware(opts.Logger),
		recoverMiddleware(opts.Logger),
		maxBodyMiddleware(opts.MaxObjectSize),
	)
}

// chain wraps h so that mws[0] is the outermost middleware.
func chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusNotFound, CodeNotFound, "no route for "+r.Method+" "+r.URL.Path)
}
