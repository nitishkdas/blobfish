package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"github.com/nitishkdas/blobfish/internal/auth"
	"github.com/nitishkdas/blobfish/internal/store"
)

// maxJSONBody caps JSON request bodies, far below the object size limit.
const maxJSONBody = 64 << 10

// serverWide is the target for operations that need "*:admin".
var serverWide = auth.Target{Bucket: "*"}

// decodeJSON reads a small JSON request body into v, rejecting unknown
// fields so typos fail loudly.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest("invalid JSON body: %v", err)
	}
	if dec.More() {
		return badRequest("invalid JSON body: unexpected data after object")
	}
	return nil
}

type bucketJSON struct {
	Name        string    `json:"name"`
	Created     time.Time `json:"created"`
	Public      bool      `json:"public"`
	CORSOrigins []string  `json:"corsOrigins"` // sites whose pages may call this bucket
	QuotaBytes  int64     `json:"quotaBytes"`  // 0 = no quota
	UsedBytes   int64     `json:"usedBytes"`   // live objects; trash not counted
	Objects     int64     `json:"objects"`
}

func toBucketJSON(b store.BucketInfo) bucketJSON {
	origins := b.CORSOrigins
	if origins == nil {
		origins = []string{}
	}
	return bucketJSON{
		Name: b.Name, Created: b.Created, Public: b.Public, CORSOrigins: origins,
		QuotaBytes: b.QuotaBytes, UsedBytes: b.UsedBytes, Objects: b.Objects,
	}
}

func (a *api) createBucket(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authorize(w, r, serverWide, auth.PermAdmin); !ok {
		return
	}
	info, err := a.buckets.CreateBucket(r.Context(), r.PathValue("bucket"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toBucketJSON(info))
}

func (a *api) deleteBucket(w http.ResponseWriter, r *http.Request) {
	bucket := r.PathValue("bucket")
	if _, ok := a.authorize(w, r, auth.Target{Bucket: bucket}, auth.PermAdmin); !ok {
		return
	}
	if err := a.buckets.DeleteBucket(r.Context(), bucket); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listBuckets returns only the buckets the caller can read.
func (a *api) listBuckets(w http.ResponseWriter, r *http.Request) {
	p, err := a.authenticate(r, auth.Target{})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	all, err := a.buckets.ListBuckets(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := []bucketJSON{}
	for _, b := range all {
		if p.Can(auth.PermRead, b.Name, "") {
			out = append(out, toBucketJSON(b))
		}
	}
	writeJSON(w, http.StatusOK, map[string][]bucketJSON{"buckets": out})
}

type whoamiResponse struct {
	KeyID         string   `json:"keyId"`
	Name          string   `json:"name"`
	Scopes        []string `json:"scopes"`
	MaxObjectSize int64    `json:"maxObjectSize"`
	// TrashDays: deleted objects stay restorable this long; 0 = trash off.
	TrashDays int `json:"trashDays"`
}

// whoami tells a client which key it is using and what that key may do,
// so the web UI can hide actions that would only end in a 403. It reveals
// nothing the caller couldn't learn by trying, and never the token.
func (a *api) whoami(w http.ResponseWriter, r *http.Request) {
	p, err := a.authenticate(r, auth.Target{})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	scopes := make([]string, 0, len(p.Scopes))
	for _, s := range p.Scopes {
		scopes = append(scopes, s.String())
	}
	writeJSON(w, http.StatusOK, whoamiResponse{
		KeyID: p.KeyID, Name: p.KeyName, Scopes: scopes, MaxObjectSize: a.maxObject,
		TrashDays: a.trashDays(),
	})
}

type presignRequest struct {
	Bucket    string `json:"bucket"`
	Key       string `json:"key"`
	Method    string `json:"method"`
	ExpiresIn int64  `json:"expiresIn"` // seconds; 0 means the default
}

type presignResponse struct {
	URL     string    `json:"url"`
	Method  string    `json:"method"`
	Expires time.Time `json:"expires"`
}

// presign issues a URL that grants one method on one object until expiry.
// The caller must itself hold the permission it is handing out.
func (a *api) presign(w http.ResponseWriter, r *http.Request) {
	p, err := a.authenticate(r, auth.Target{})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req presignRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	perm := auth.PermRead
	if req.Method == http.MethodPut {
		perm = auth.PermWrite
	}
	if !p.Can(perm, req.Bucket, req.Key) {
		a.fail(w, r, errAccessDenied)
		return
	}
	ttl := auth.DefaultPresignTTL
	if req.ExpiresIn != 0 {
		ttl = time.Duration(req.ExpiresIn) * time.Second
	}
	query, exp, err := a.auth.Presign(req.Method, req.Bucket, req.Key, ttl)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	u := (&url.URL{Path: objectsPrefix + req.Bucket + "/" + req.Key}).EscapedPath() + "?" + query
	writeJSON(w, http.StatusOK, presignResponse{URL: a.baseURL(r) + u, Method: req.Method, Expires: exp})
}

// baseURL is the configured public URL, or one derived from the request.
func (a *api) baseURL(r *http.Request) string {
	if a.publicURL != "" {
		return a.publicURL
	}
	scheme := "http"
	if clientFrom(r).https { // X-Forwarded-Proto counts only from trusted proxies
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

type keyJSON struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Scopes  []string  `json:"scopes"`
	Created time.Time `json:"created"`
	// Key is the token, present only in the create response.
	Key string `json:"key,omitempty"`
}

func toKeyJSON(k store.KeyRecord) keyJSON { // never copies the hash
	return keyJSON{ID: k.ID, Name: k.Name, Scopes: k.Scopes, Created: k.Created}
}

func (a *api) createKey(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authorize(w, r, serverWide, auth.PermAdmin); !ok {
		return
	}
	var req struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	token, rec, err := a.auth.CreateKey(r.Context(), req.Name, req.Scopes)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := toKeyJSON(rec)
	out.Key = token
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, out)
}

func (a *api) listKeys(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authorize(w, r, serverWide, auth.PermAdmin); !ok {
		return
	}
	keys, err := a.auth.ListKeys(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]keyJSON, 0, len(keys))
	for _, k := range keys {
		out = append(out, toKeyJSON(k))
	}
	writeJSON(w, http.StatusOK, map[string][]keyJSON{"keys": out})
}

func (a *api) deleteKey(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authorize(w, r, serverWide, auth.PermAdmin); !ok {
		return
	}
	if err := a.auth.DeleteKey(r.Context(), r.PathValue("id")); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
