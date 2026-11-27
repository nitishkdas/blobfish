package server

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nitishkdas/blobfish/internal/auth"
	"github.com/nitishkdas/blobfish/internal/store"
)

const (
	objectsPrefix = "/v1/b/"
	metaHeader    = "X-Meta-"
)

type objectJSON struct {
	Bucket      string            `json:"bucket"`
	Key         string            `json:"key"`
	Size        int64             `json:"size"`
	ETag        string            `json:"etag"`
	ContentType string            `json:"contentType"`
	Modified    time.Time         `json:"modified"`
	Meta        map[string]string `json:"meta,omitempty"`
}

func toObjectJSON(i store.ObjectInfo) objectJSON {
	return objectJSON{
		Bucket: i.Bucket, Key: i.Key, Size: i.Size, ETag: i.ETag,
		ContentType: i.ContentType, Modified: i.Modified, Meta: i.User,
	}
}

// objects routes /v1/b/{bucket} and /v1/b/{bucket}/{key...}. The key is the
// decoded path after the bucket, taken verbatim (no path cleaning).
func (a *api) objects(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, objectsPrefix)
	bucket, key, isObject := strings.Cut(rest, "/")
	// Browser access is per bucket; settle it before auth so even error
	// replies are readable by an allowed page.
	if a.cors(w, r, bucket) {
		return
	}
	if !isObject {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			a.listObjects(w, r, bucket)
		default:
			methodNotAllowed(w, r, "GET, HEAD")
		}
		return
	}
	t := auth.Target{Bucket: bucket, Key: key}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		a.getObject(w, r, t)
	case http.MethodPut:
		a.putObject(w, r, t)
	case http.MethodDelete:
		a.deleteObject(w, r, t)
	default:
		methodNotAllowed(w, r, "GET, HEAD, PUT, DELETE")
	}
}

func (a *api) putObject(w http.ResponseWriter, r *http.Request, t auth.Target) {
	if _, ok := a.authorize(w, r, t, auth.PermWrite); !ok {
		return
	}
	meta := store.Meta{
		ContentType: r.Header.Get("Content-Type"), SizeHint: r.ContentLength,
		// "If-None-Match: *": create only, never replace (RFC 9110 13.1.2).
		NoOverwrite: strings.TrimSpace(r.Header.Get("If-None-Match")) == "*",
	}
	for name, vals := range r.Header {
		if k, ok := strings.CutPrefix(name, metaHeader); ok && k != "" {
			if meta.User == nil {
				meta.User = make(map[string]string)
			}
			meta.User[strings.ToLower(k)] = strings.Join(vals, ",")
		}
	}
	info, err := a.store.Put(r.Context(), t.Bucket, t.Key, r.Body, meta)
	if err != nil {
		// The body may be partly unread (refused for quota or space before
		// or while streaming). Don't let net/http drain gigabytes of it to
		// keep the connection alive.
		w.Header().Set("Connection", "close")
		if meta.NoOverwrite && errors.Is(err, store.ErrKeyExists) {
			writeError(w, r, http.StatusPreconditionFailed, CodePreconditionFailed, "An object with this key already exists, and If-None-Match: * forbids replacing it.")
			return
		}
		a.fail(w, r, err)
		return
	}
	w.Header().Set("ETag", quoteETag(info.ETag))
	writeJSON(w, http.StatusOK, toObjectJSON(info))
}

func (a *api) getObject(w http.ResponseWriter, r *http.Request, t auth.Target) {
	ok, public := a.authorizeRead(w, r, t)
	if !ok {
		return
	}
	// Plain object reads send no Content-Disposition unless ?download asks
	// the browser to save the file.
	disposition := ""
	if r.URL.Query().Has("download") {
		var err error
		if disposition, err = contentDisposition(r, defaultFilename(t.Key)); err != nil {
			a.fail(w, r, err)
			return
		}
	}
	extra := func(h http.Header) {
		if public {
			// Shared caches may keep public files, but must revalidate, so
			// making the bucket private takes effect at once.
			h.Set("Cache-Control", "public, no-cache")
		}
		if disposition != "" {
			h.Set("Content-Disposition", disposition)
		}
	}
	a.serveObject(w, r, t, extra)
}

// authorizeRead allows a read with a key or presigned URL as usual, or with
// no credentials at all when the bucket is public. A request without
// credentials on a private or missing bucket gets the usual 401, so a
// bucket's existence is never revealed.
//
// public reports that access was granted only because the bucket is public.
func (a *api) authorizeRead(w http.ResponseWriter, r *http.Request, t auth.Target) (ok, public bool) {
	anonymous := r.Header.Get("Authorization") == "" && !r.URL.Query().Has("sig")
	if anonymous && t.Key != "" {
		if b, err := a.buckets.GetBucket(r.Context(), t.Bucket); err == nil && b.Public {
			setActor(r, "public")
			return true, true
		}
	}
	_, ok = a.authorize(w, r, t, auth.PermRead)
	return ok, false
}

// serveObject answers GET or HEAD for an object the caller may read:
// Range, If-None-Match, checksum-verified full reads. extra, if set, adds
// headers (for example Content-Disposition on share links).
func (a *api) serveObject(w http.ResponseWriter, r *http.Request, t auth.Target, extra func(http.Header)) {
	setHeaders := func(info store.ObjectInfo) {
		setObjectHeaders(w, info)
		if extra != nil {
			extra(w.Header())
		}
	}

	if r.Method == http.MethodHead {
		info, err := a.store.Head(r.Context(), t.Bucket, t.Key)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		setHeaders(info)
		if etagMatches(r.Header.Get("If-None-Match"), info.ETag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
		w.WriteHeader(http.StatusOK)
		return
	}

	rng, unsatisfiable := parseRange(r.Header.Get("Range"))
	if unsatisfiable {
		// Still need the size for the Content-Range header.
		info, err := a.store.Head(r.Context(), t.Bucket, t.Key)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		a.rangeNotSatisfiable(w, r, info.Size)
		return
	}

	rc, info, err := a.store.Get(r.Context(), t.Bucket, t.Key, rng)
	if errors.Is(err, store.ErrInvalidRange) {
		a.rangeNotSatisfiable(w, r, info.Size)
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	defer rc.Close()

	setHeaders(info)
	if etagMatches(r.Header.Get("If-None-Match"), info.ETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	status, length := http.StatusOK, info.Size
	if rng != nil {
		off, n, _ := rng.Resolve(info.Size) // Get already validated it
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, off+n-1, info.Size))
		status, length = http.StatusPartialContent, n
	}
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(status)

	if _, err := io.Copy(w, rc); err != nil {
		if errors.Is(err, store.ErrDataCorrupt) {
			// Headers are gone already; the only honest signal left is to
			// cut the connection so the client sees a failed download, not
			// a clean end of bad data.
			a.log.ErrorContext(r.Context(), "corrupt object detected while streaming",
				"request_id", RequestID(r.Context()), "bucket", t.Bucket, "key", t.Key, "err", err)
			panic(http.ErrAbortHandler)
		}
		a.log.DebugContext(r.Context(), "download interrupted",
			"request_id", RequestID(r.Context()), "err", err)
	}
}

func (a *api) rangeNotSatisfiable(w http.ResponseWriter, r *http.Request, size int64) {
	w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
	a.fail(w, r, fmt.Errorf("%w: object is %d bytes", store.ErrInvalidRange, size))
}

func (a *api) deleteObject(w http.ResponseWriter, r *http.Request, t auth.Target) {
	if _, ok := a.authorize(w, r, t, auth.PermWrite); !ok {
		return
	}
	if err := a.store.Delete(r.Context(), t.Bucket, t.Key); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type listJSON struct {
	Objects    []objectJSON `json:"objects"`
	Prefixes   []string     `json:"prefixes"`
	NextCursor string       `json:"nextCursor,omitempty"`
}

func (a *api) listObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	if _, ok := a.authorize(w, r, auth.Target{Bucket: bucket}, auth.PermRead); !ok {
		return
	}
	q := r.URL.Query()
	opts := store.ListOptions{
		Prefix:    q.Get("prefix"),
		Delimiter: q.Get("delimiter"),
		Cursor:    q.Get("cursor"),
	}
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			a.fail(w, r, badRequest(`"limit" must be a positive integer`))
			return
		}
		opts.Limit = n
	}
	res, err := a.store.List(r.Context(), bucket, opts)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := listJSON{
		Objects:    make([]objectJSON, 0, len(res.Objects)),
		Prefixes:   res.CommonPrefixes,
		NextCursor: res.NextCursor,
	}
	if out.Prefixes == nil {
		out.Prefixes = []string{} // JSON [] rather than null
	}
	for _, o := range res.Objects {
		out.Objects = append(out.Objects, toObjectJSON(o))
	}
	writeJSON(w, http.StatusOK, out)
}

func setObjectHeaders(w http.ResponseWriter, info store.ObjectInfo) {
	h := w.Header()
	h.Set("Content-Type", info.ContentType)
	h.Set("ETag", quoteETag(info.ETag))
	h.Set("Last-Modified", info.Modified.UTC().Format(http.TimeFormat))
	h.Set("Accept-Ranges", "bytes")
	h.Set("X-Content-Type-Options", "nosniff")
	// Caches must revalidate every time (a cheap 304 thanks to the ETag),
	// so revoking a key or link, or making a bucket private, takes effect
	// immediately. "private": shared caches never store these.
	h.Set("Cache-Control", "private, no-cache")
	if canRunScript(info.ContentType) {
		// An uploaded HTML/SVG/XML file opened from this origin could run
		// script next to the web UI and read its stored API key. The sandbox
		// gives it an opaque origin with scripts disabled.
		h.Set("Content-Security-Policy", "sandbox")
	}
	for k, v := range info.User {
		h.Set(metaHeader+k, v)
	}
}

// canRunScript reports whether a browser may execute script in a document
// of this type. Unparseable types count as unsafe. Other types (PDF, images,
// text/plain) are left alone: with nosniff they cannot turn into HTML, and
// Chrome refuses to show PDFs under a sandbox CSP.
func canRunScript(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return true
	}
	switch mt {
	case "text/html", "application/xhtml+xml", "image/svg+xml", "text/xml", "application/xml",
		"text/xsl", "application/xslt+xml", "multipart/x-mixed-replace":
		return true
	}
	return strings.HasSuffix(mt, "+xml")
}

func quoteETag(etag string) string { return `"` + etag + `"` }

// etagMatches implements If-None-Match's weak comparison against a list
// like `"abc", W/"def"` or `*`.
func etagMatches(header, etag string) bool {
	if header == "" {
		return false
	}
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "*" || strings.TrimPrefix(part, "W/") == quoteETag(etag) {
			return true
		}
	}
	return false
}

// parseRange parses a single-range "bytes=" header. It returns nil when the
// header is absent, malformed or asks for several ranges; per RFC 9110 the
// server then ignores it and sends the whole object. unsatisfiable is true
// only for a well-formed range that can never match ("bytes=-0").
func parseRange(h string) (rng *store.Range, unsatisfiable bool) {
	spec, ok := strings.CutPrefix(h, "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return nil, false
	}
	first, last, ok := strings.Cut(strings.TrimSpace(spec), "-")
	if !ok {
		return nil, false
	}
	if first == "" { // suffix: last N bytes
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n < 0 {
			return nil, false
		}
		if n == 0 {
			return nil, true
		}
		return &store.Range{Start: -n}, false
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 {
		return nil, false
	}
	end := int64(-1)
	if last != "" {
		end, err = strconv.ParseInt(last, 10, 64)
		if err != nil || end < start {
			return nil, false
		}
	}
	return &store.Range{Start: start, End: end}, false
}
