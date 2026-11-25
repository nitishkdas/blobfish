// Package webui serves the embedded browser UI. It is static files only:
// the page talks to the /v1 API with the API key the user pastes in, so it
// has no server-side access to data at all.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
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

// Handler serves the UI under Prefix.
func Handler() http.Handler {
	static, err := fs.Sub(files, "static")
	if err != nil {
		panic(err) // the embed pattern above guarantees the directory exists
	}
	files := http.StripPrefix(Prefix[:len(Prefix)-1], http.FileServerFS(static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		// Presigned URLs carry their signature in the query string; never
		// leak them to other sites through the Referer header.
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}
