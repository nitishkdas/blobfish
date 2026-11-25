// Package server holds the BlobFish HTTP router, middleware, the native /v1
// API and the shared JSON error format. Handlers reach data only through
// the store interfaces and authenticate only through auth, never bbolt or
// files directly.
package server

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/nitishkdas/blobfish/internal/auth"
	"github.com/nitishkdas/blobfish/internal/store"
	"github.com/nitishkdas/blobfish/internal/webui"
)

// Options are the dependencies and settings the HTTP layer needs.
type Options struct {
	Logger        *slog.Logger
	MaxObjectSize int64
	Store         store.Store
	Buckets       store.Buckets
	Auth          *auth.Service
	// PublicURL is the base for presigned URLs; empty means "use the Host
	// header of the presign request".
	PublicURL string
}

// api holds the /v1 handlers' dependencies.
type api struct {
	log       *slog.Logger
	store     store.Store
	buckets   store.Buckets
	auth      *auth.Service
	publicURL string
	maxObject int64
}

// New returns the root handler with all routes and middleware applied.
func New(opts Options) http.Handler {
	a := &api{
		log:       opts.Logger,
		store:     opts.Store,
		buckets:   opts.Buckets,
		auth:      opts.Auth,
		publicURL: strings.TrimSuffix(opts.PublicURL, "/"),
		maxObject: opts.MaxObjectSize,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /v1/whoami", a.whoami)
	mux.HandleFunc("GET /v1/buckets", a.listBuckets)
	mux.HandleFunc("PUT /v1/buckets/{bucket}", a.createBucket)
	mux.HandleFunc("DELETE /v1/buckets/{bucket}", a.deleteBucket)
	mux.HandleFunc("POST /v1/presign", a.presign)
	mux.HandleFunc("GET /v1/keys", a.listKeys)
	mux.HandleFunc("POST /v1/keys", a.createKey)
	mux.HandleFunc("DELETE /v1/keys/{id}", a.deleteKey)
	mux.Handle("GET "+webui.Prefix, webui.Handler())
	mux.Handle("GET /favicon.ico", webui.Favicon())
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, webui.Prefix, http.StatusFound)
	})
	mux.HandleFunc("/", handleNotFound)

	// Object routes bypass ServeMux on purpose: it cleans paths and
	// redirects "a//b" or "x/../y", but those are valid object keys.
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, objectsPrefix) {
			a.objects(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})

	// Outermost first: every request gets an ID, then a log line, then panic
	// protection, then the body limit.
	return chain(root,
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

func methodNotAllowed(w http.ResponseWriter, r *http.Request, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, r, http.StatusMethodNotAllowed, CodeMethodNotAllowed, r.Method+" is not allowed here")
}

// fail writes err as an API error, logging server-side failures.
func (a *api) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, code, msg := classify(err)
	if status >= 500 {
		a.log.ErrorContext(r.Context(), "request failed",
			"request_id", RequestID(r.Context()), "code", code, "err", err)
	}
	writeError(w, r, status, code, msg)
}

// authorize authenticates the request and checks it holds perm on t.
// On failure it writes the error response and returns false.
func (a *api) authorize(w http.ResponseWriter, r *http.Request, t auth.Target, perm auth.Perm) (auth.Principal, bool) {
	p, err := a.auth.Authenticate(r, t)
	if err != nil {
		a.fail(w, r, err)
		return auth.Principal{}, false
	}
	if !p.Can(perm, t.Bucket, t.Key) {
		a.fail(w, r, errAccessDenied)
		return auth.Principal{}, false
	}
	return p, true
}
