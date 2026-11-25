package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestPerBucketCORS: each bucket lists the sites whose pages may call it.
// app1's site may reach bucket1 from a browser but not bucket2.
func TestPerBucketCORS(t *testing.T) {
	a := newTestAPI(t)
	const app1 = "https://app1.example.com"
	for _, b := range []string{"bucket1", "bucket2", "open-b"} {
		a.must(http.StatusCreated, "PUT", "/v1/buckets/"+b, a.admin, nil)
		a.must(http.StatusOK, "PUT", "/v1/b/"+b+"/f.txt", a.admin, strings.NewReader("hi"))
	}
	rr := a.must(http.StatusOK, "PATCH", "/v1/buckets/bucket1", a.admin,
		strings.NewReader(`{"corsOrigins": ["https://APP1.example.com", "http://localhost:3000"]}`))
	var b bucketJSON
	json.Unmarshal(rr.Body.Bytes(), &b)
	if strings.Join(b.CORSOrigins, ",") != "http://localhost:3000,https://app1.example.com" {
		t.Fatalf("stored origins = %v (want normalized and sorted)", b.CORSOrigins)
	}
	a.must(http.StatusOK, "PATCH", "/v1/buckets/open-b", a.admin, strings.NewReader(`{"corsOrigins": ["*"]}`))

	preflight := func(path, origin string) []string {
		return []string{"Origin", origin, "Access-Control-Request-Method", "PUT", "Access-Control-Request-Headers", "content-type, x-meta-owner"}
	}
	tests := []struct {
		name     string
		method   string
		path     string
		hdr      []string
		want     int
		wantACAO string
	}{
		{"preflight, listed site", "OPTIONS", "/v1/b/bucket1/up.pdf", preflight("", app1), 204, app1},
		{"preflight, other bucket", "OPTIONS", "/v1/b/bucket2/up.pdf", preflight("", app1), 403, ""},
		{"preflight, unlisted site", "OPTIONS", "/v1/b/bucket1/up.pdf", preflight("", "https://evil.example"), 403, ""},
		{"preflight, missing bucket looks the same", "OPTIONS", "/v1/b/no-such-b/up.pdf", preflight("", app1), 403, ""},
		{"preflight, wildcard bucket", "OPTIONS", "/v1/b/open-b/up.pdf", preflight("", "https://anyone.example"), 204, "*"},
		{"real read, listed site", "GET", "/v1/b/bucket1/f.txt", []string{"Origin", app1}, 401, app1}, // no key: error still readable
		{"real read, unlisted site gets no headers", "GET", "/v1/b/bucket2/f.txt", []string{"Origin", app1}, 401, ""},
		{"listing a bucket", "OPTIONS", "/v1/b/bucket1", preflight("", app1), 204, app1},
		{"no Origin: plain OPTIONS is not a method we serve", "OPTIONS", "/v1/b/bucket1/up.pdf", nil, 405, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := a.must(tt.want, tt.method, tt.path, "", nil, tt.hdr...)
			h := rr.Header()
			if got := h.Get("Access-Control-Allow-Origin"); got != tt.wantACAO {
				t.Errorf("ACAO = %q, want %q", got, tt.wantACAO)
			}
			if tt.want == 204 {
				if !strings.Contains(h.Get("Access-Control-Allow-Methods"), "PUT") ||
					h.Get("Access-Control-Allow-Headers") != "content-type, x-meta-owner" {
					t.Errorf("preflight headers %v", h)
				}
			}
			if tt.want == 403 {
				if c := errCode(t, rr); c != CodeCORSNotAllowed {
					t.Errorf("code = %s", c)
				}
			}
			if len(tt.hdr) > 0 && !strings.Contains(h.Get("Vary"), "Origin") {
				t.Error("missing Vary: Origin")
			}
		})
	}

	// With a key, an allowed page can read the ETag after uploading.
	rr = a.must(http.StatusOK, "PUT", "/v1/b/bucket1/new.txt", a.admin, strings.NewReader("x"), "Origin", app1)
	if !strings.Contains(rr.Header().Get("Access-Control-Expose-Headers"), "ETag") || rr.Header().Get("Access-Control-Allow-Origin") != app1 {
		t.Errorf("upload response headers %v", rr.Header())
	}

	// Share links follow their file's bucket.
	l1 := createLinkJSON(t, a, a.admin, "bucket1", "f.txt", 0, http.StatusCreated)
	l2 := createLinkJSON(t, a, a.admin, "bucket2", "f.txt", 0, http.StatusCreated)
	if got := a.must(http.StatusOK, "GET", linkPath(t, l1), "", nil, "Origin", app1).Header().Get("Access-Control-Allow-Origin"); got != app1 {
		t.Errorf("share link in bucket1: ACAO %q", got)
	}
	if got := a.must(http.StatusOK, "GET", linkPath(t, l2), "", nil, "Origin", app1).Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("share link in bucket2: ACAO %q", got)
	}
	a.must(http.StatusNoContent, "OPTIONS", linkPath(t, l1), "", nil, "Origin", app1, "Access-Control-Request-Method", "GET")

	// Changing the list applies at once; clearing it closes access.
	a.must(http.StatusOK, "PATCH", "/v1/buckets/bucket1", a.admin, strings.NewReader(`{"corsOrigins": []}`))
	a.must(http.StatusForbidden, "OPTIONS", "/v1/b/bucket1/up.pdf", "", nil, preflight("", app1)...)
}

func TestBucketCORSSettingsAccess(t *testing.T) {
	a := newTestAPI(t)
	a.must(http.StatusCreated, "PUT", "/v1/buckets/set-b", a.admin, nil)
	body := `{"corsOrigins": ["https://app.example.com"]}`
	a.must(http.StatusForbidden, "PATCH", "/v1/buckets/set-b", a.key("set-b:rw"), strings.NewReader(body))
	a.must(http.StatusOK, "PATCH", "/v1/buckets/set-b", a.key("set-b:admin"), strings.NewReader(body))
	for _, bad := range []string{`{"corsOrigins": ["app.example.com"]}`, `{"corsOrigins": ["https://a.com/path"]}`} {
		if c := errCode(t, a.must(http.StatusBadRequest, "PATCH", "/v1/buckets/set-b", a.admin, strings.NewReader(bad))); c != CodeInvalidOrigin {
			t.Errorf("%s: code %s", bad, c)
		}
	}
	// Both settings in one request, and neither clobbers the other.
	rr := a.must(http.StatusOK, "PATCH", "/v1/buckets/set-b", a.admin, strings.NewReader(`{"public": true}`))
	var b bucketJSON
	json.Unmarshal(rr.Body.Bytes(), &b)
	if !b.Public || len(b.CORSOrigins) != 1 {
		t.Errorf("after setting public: %+v", b)
	}
	rr = a.must(http.StatusOK, "GET", "/v1/buckets", a.admin, nil)
	if !strings.Contains(rr.Body.String(), `"corsOrigins":["https://app.example.com"]`) {
		t.Errorf("bucket list: %s", rr.Body)
	}
}
