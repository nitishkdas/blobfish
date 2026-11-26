// Package webui serves the embedded browser UI. It is static files only:
// the page talks to the /v1 API with the API key the user pastes in, so it
// has no server-side access to data at all.
package webui

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
)

//go:embed static
var files embed.FS

// Prefix is the URL path the UI is mounted at.
const Prefix = "/ui/"

// csp allows only same-origin scripts, styles and API calls. With no inline
// script allowed, an injection bug cannot run code that reads the stored key.
const csp = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Favicon serves the BlobFish icon at /favicon.ico. Browsers ask for that
// path on their own when a file is opened straight from the server (for
// example a share link), which otherwise fills the log with 404s.
func Favicon() http.Handler {
	icon, err := files.ReadFile("static/favicon.svg")
	if err != nil {
		panic(err) // embedded at build time
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "image/svg+xml")
		h.Set("Cache-Control", "public, max-age=86400")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "default-src 'none'")
		w.Write(icon)
	})
}

// UploadPage serves the page behind an upload link (/u/<token>): a file
// picker that sends files to PUT /u/<token>/<name>. The page itself knows
// nothing; it asks /u/<token>/info what the link allows.
func UploadPage() http.Handler {
	page, err := files.ReadFile("static/upload.html")
	if err != nil {
		panic(err) // embedded at build time
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		// The token is in the URL: keep it out of Referer headers and
		// search engines.
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Robots-Tag", "noindex")
		h.Set("Cache-Control", "no-store")
		w.Write(page)
	})
}

// asset is one embedded file, ready to serve.
type asset struct {
	body, gz []byte // gz is nil when compressing doesn't help
	ctype    string
	etag     string
}

// loadAssets reads every embedded file once, at start: its type, a strong
// ETag from its content (so browsers revalidate with a cheap 304), and a
// gzip copy for text files (the UI is ~150 KB, ~40 KB compressed).
func loadAssets() map[string]*asset {
	out := map[string]*asset{}
	err := fs.WalkDir(files, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := files.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		a := &asset{
			body:  body,
			ctype: cmp.Or(mime.TypeByExtension(path.Ext(p)), "application/octet-stream"),
			etag:  `"` + hex.EncodeToString(sum[:16]) + `"`,
		}
		if len(body) > 1024 && compressible(a.ctype) {
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			zw.Write(body)
			zw.Close()
			if buf.Len() < len(body) {
				a.gz = buf.Bytes()
			}
		}
		out[strings.TrimPrefix(p, "static/")] = a
		return nil
	})
	if err != nil {
		panic(err) // embedded at build time
	}
	return out
}

func compressible(ctype string) bool {
	return strings.HasPrefix(ctype, "text/") || strings.Contains(ctype, "javascript") ||
		strings.Contains(ctype, "json") || strings.HasPrefix(ctype, "image/svg")
}

// Handler serves the UI under Prefix: exactly the embedded files, no
// directory listings. "/ui/" is index.html.
func Handler() http.Handler {
	assets := loadAssets()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		// Presigned URLs carry their signature in the query string; never
		// leak them to other sites through the Referer header.
		h.Set("Referrer-Policy", "no-referrer")
		// An admin console has no business in search results.
		h.Set("X-Robots-Tag", "noindex, nofollow")

		name := strings.TrimPrefix(r.URL.Path, Prefix)
		if name == "" {
			name = "index.html"
		}
		a := assets[name]
		if a == nil {
			http.NotFound(w, r)
			return
		}
		// Revalidate on every load (cheap: 304 via ETag), so an upgrade
		// never leaves a browser running half old, half new modules.
		h.Set("Cache-Control", "no-cache")
		h.Set("ETag", a.etag)
		h.Set("Vary", "Accept-Encoding")
		if etagMatch(r.Header.Get("If-None-Match"), a.etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		body := a.body
		if a.gz != nil && acceptsGzip(r) {
			body = a.gz
			h.Set("Content-Encoding", "gzip")
		}
		h.Set("Content-Type", a.ctype)
		h.Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method != http.MethodHead {
			w.Write(body)
		}
	})
}

// etagMatch reports whether an If-None-Match header lists etag (weak
// comparison, as RFC 9110 asks for If-None-Match).
func etagMatch(header, etag string) bool {
	for t := range strings.SplitSeq(header, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == etag || t == "*" {
			return true
		}
	}
	return false
}

// acceptsGzip reports whether the client accepts gzip (q=0 means no).
func acceptsGzip(r *http.Request) bool {
	for part := range strings.SplitSeq(r.Header.Get("Accept-Encoding"), ",") {
		coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(coding), "gzip") {
			q := strings.ReplaceAll(strings.TrimSpace(params), " ", "")
			return q != "q=0" && q != "q=0.0" && q != "q=0.00" && q != "q=0.000"
		}
	}
	return false
}
