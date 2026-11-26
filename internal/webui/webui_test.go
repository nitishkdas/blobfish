package webui

import (
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestServesAssetsWithSecurityHeaders(t *testing.T) {
	h := Handler()
	tests := []struct {
		path     string
		wantType string
		contains string
	}{
		{"/ui/", "text/html", `<div id="app"`},
		{"/ui/js/app.js", "text/javascript", "function renderLogin"},
		{"/ui/style.css", "text/css", "--accent"},
		{"/ui/favicon.svg", "image/svg+xml", "<svg"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("status %d", rr.Code)
			}
			if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, tt.wantType) {
				t.Errorf("Content-Type = %q, want %s", ct, tt.wantType)
			}
			if !strings.Contains(rr.Body.String(), tt.contains) {
				t.Errorf("body missing %q", tt.contains)
			}
			for _, hdr := range []string{"Content-Security-Policy", "X-Frame-Options", "Referrer-Policy", "X-Content-Type-Options"} {
				if rr.Header().Get(hdr) == "" {
					t.Errorf("missing %s", hdr)
				}
			}
			if csp := rr.Header().Get("Content-Security-Policy"); strings.Contains(csp, "unsafe") {
				t.Errorf("CSP allows unsafe sources: %s", csp)
			}
		})
	}
}

// TestNoUnsafeDOM guards the UI's XSS rules: object keys and bucket names
// are attacker-controlled strings, so the UI must never parse them as HTML,
// and the CSP forbids inline script/style, so none may appear.
func TestNoUnsafeDOM(t *testing.T) {
	// Native alert/confirm/prompt don't match the app; use confirmDialog.
	banned := regexp.MustCompile(`innerHTML|outerHTML|insertAdjacentHTML|document\.write|\beval\(|new Function|\b(alert|confirm|prompt)\(`)
	inline := regexp.MustCompile(`<script>|<script [^>]*>[^<]|<style|\sstyle="|\son[a-z]+="`)
	err := fs.WalkDir(files, "static", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(files, path)
		if err != nil {
			return err
		}
		src := string(b)
		if strings.HasSuffix(path, ".js") {
			if m := banned.FindString(src); m != "" {
				t.Errorf("%s uses %s", path, m)
			}
		}
		if strings.HasSuffix(path, ".html") {
			if m := inline.FindString(src); m != "" {
				t.Errorf("%s has inline code the CSP would block: %q", path, m)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFavicon(t *testing.T) {
	rr := httptest.NewRecorder()
	Favicon().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))
	if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != "image/svg+xml" ||
		!strings.Contains(rr.Body.String(), "<svg") {
		t.Fatalf("favicon: %d %q", rr.Code, rr.Header().Get("Content-Type"))
	}
}

func TestServesOnlyFiles(t *testing.T) {
	h := Handler()
	for _, p := range []string{"/ui/js/", "/ui/js", "/ui/nope.js", "/ui/../go.mod"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, p, nil))
		if rr.Code != http.StatusNotFound || strings.Contains(rr.Body.String(), "app.js") {
			t.Errorf("GET %s = %d %q, want 404 and no listing", p, rr.Code, rr.Body)
		}
	}
}

func TestCachingAndCompression(t *testing.T) {
	h := Handler()
	get := func(hdr ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/ui/js/files.js", nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr
	}
	plain := get()
	etag := plain.Header().Get("ETag")
	if plain.Code != 200 || etag == "" || plain.Header().Get("Content-Encoding") != "" ||
		!strings.Contains(plain.Header().Get("X-Robots-Tag"), "noindex") {
		t.Fatalf("plain: %d %v", plain.Code, plain.Header())
	}
	if rr := get("If-None-Match", etag); rr.Code != http.StatusNotModified || rr.Body.Len() != 0 {
		t.Errorf("revalidation = %d with %d bytes", rr.Code, rr.Body.Len())
	}
	if rr := get("If-None-Match", `W/`+etag); rr.Code != http.StatusNotModified {
		t.Errorf("weak revalidation = %d", rr.Code)
	}
	gz := get("Accept-Encoding", "br, gzip")
	if gz.Header().Get("Content-Encoding") != "gzip" || gz.Body.Len() >= plain.Body.Len() {
		t.Fatalf("gzip: %v, %d vs %d bytes", gz.Header(), gz.Body.Len(), plain.Body.Len())
	}
	zr, err := gzip.NewReader(gz.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(zr); string(got) != plain.Body.String() {
		t.Error("gzip body differs from the file")
	}
	if rr := get("Accept-Encoding", "gzip;q=0"); rr.Header().Get("Content-Encoding") != "" {
		t.Error("gzip sent although refused")
	}
}
