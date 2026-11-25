package server

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nitishkdas/blobfish/internal/auth"
	"github.com/nitishkdas/blobfish/internal/engine"
	"github.com/nitishkdas/blobfish/internal/store"
)

// testAPI is a full server over a real engine in a temp dir.
type testAPI struct {
	t     *testing.T
	h     http.Handler
	auth  *auth.Service
	eng   *engine.Engine
	admin string
}

func newTestAPI(t *testing.T) *testAPI {
	t.Helper()
	eng, err := engine.Open(t.TempDir(), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	svc := auth.NewService(eng, eng.PresignSecret())
	admin, err := svc.EnsureAdminKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h := New(Options{
		Logger:        slog.New(slog.DiscardHandler),
		MaxObjectSize: 1 << 20,
		Store:         eng,
		Buckets:       eng,
		Auth:          svc,
		PublicURL:     "https://files.example.com",
	})
	return &testAPI{t: t, h: h, auth: svc, eng: eng, admin: admin}
}

// do sends a request. token "" sends no Authorization header.
func (a *testAPI) do(method, target, token string, body io.Reader, hdr ...string) *httptest.ResponseRecorder {
	a.t.Helper()
	r := httptest.NewRequest(method, target, body)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	rr := httptest.NewRecorder()
	a.h.ServeHTTP(rr, r)
	return rr
}

// must sends a request and fails the test unless the status matches.
func (a *testAPI) must(want int, method, target, token string, body io.Reader, hdr ...string) *httptest.ResponseRecorder {
	a.t.Helper()
	rr := a.do(method, target, token, body, hdr...)
	if rr.Code != want {
		a.t.Fatalf("%s %s = %d, want %d; body: %s", method, target, rr.Code, want, rr.Body)
	}
	return rr
}

func (a *testAPI) key(scopes ...string) string {
	a.t.Helper()
	tok, _, err := a.auth.CreateKey(context.Background(), "test", scopes)
	if err != nil {
		a.t.Fatal(err)
	}
	return tok
}

func errCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var b errorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil {
		t.Fatalf("not an error body: %s", rr.Body)
	}
	if b.Error.RequestID == "" {
		t.Errorf("error body has no requestId: %s", rr.Body)
	}
	return b.Error.Code
}

func TestObjectLifecycle(t *testing.T) {
	a := newTestAPI(t)
	a.must(http.StatusCreated, "PUT", "/v1/buckets/docs", a.admin, nil)

	rr := a.must(http.StatusOK, "PUT", "/v1/b/docs/2026/inv.pdf", a.admin, strings.NewReader("%PDF-hello"),
		"Content-Type", "application/pdf", "X-Meta-Customer", "acme")
	var put objectJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &put); err != nil {
		t.Fatal(err)
	}
	if put.Size != 10 || put.ContentType != "application/pdf" || put.Meta["customer"] != "acme" ||
		rr.Header().Get("ETag") != `"`+put.ETag+`"` {
		t.Fatalf("put response = %+v, headers %v", put, rr.Header())
	}

	rr = a.must(http.StatusOK, "GET", "/v1/b/docs/2026/inv.pdf", a.admin, nil)
	h := rr.Header()
	if rr.Body.String() != "%PDF-hello" || h.Get("Content-Type") != "application/pdf" ||
		h.Get("X-Meta-Customer") != "acme" || h.Get("Content-Length") != "10" ||
		h.Get("Accept-Ranges") != "bytes" || h.Get("Last-Modified") == "" {
		t.Fatalf("GET = %q, headers %v", rr.Body, h)
	}

	rr = a.must(http.StatusOK, "HEAD", "/v1/b/docs/2026/inv.pdf", a.admin, nil)
	if rr.Body.Len() != 0 || rr.Header().Get("Content-Length") != "10" {
		t.Errorf("HEAD body %q, length %q", rr.Body, rr.Header().Get("Content-Length"))
	}

	a.must(http.StatusNotModified, "GET", "/v1/b/docs/2026/inv.pdf", a.admin, nil, "If-None-Match", `W/"x", "`+put.ETag+`"`)
	a.must(http.StatusOK, "GET", "/v1/b/docs/2026/inv.pdf", a.admin, nil, "If-None-Match", `"other"`)

	a.must(http.StatusNoContent, "DELETE", "/v1/b/docs/2026/inv.pdf", a.admin, nil)
	a.must(http.StatusNoContent, "DELETE", "/v1/b/docs/2026/inv.pdf", a.admin, nil) // idempotent
	rr = a.must(http.StatusNotFound, "GET", "/v1/b/docs/2026/inv.pdf", a.admin, nil)
	if c := errCode(t, rr); c != CodeNoSuchKey {
		t.Errorf("code = %s", c)
	}
	a.must(http.StatusNoContent, "DELETE", "/v1/buckets/docs", a.admin, nil)
}

// TestOddKeys checks keys that ServeMux would clean or redirect.
func TestOddKeys(t *testing.T) {
	a := newTestAPI(t)
	a.must(http.StatusCreated, "PUT", "/v1/buckets/odd", a.admin, nil)
	keys := []string{"a//b", "x/../y", "./dot", "trailing/", "sp ace", "q?mark", "%percent", "ünï/🐟"}
	for _, k := range keys {
		t.Run(k, func(t *testing.T) {
			path := (&url.URL{Path: "/v1/b/odd/" + k}).EscapedPath()
			a.must(http.StatusOK, "PUT", path, a.admin, strings.NewReader("v:"+k))
			if rr := a.must(http.StatusOK, "GET", path, a.admin, nil); rr.Body.String() != "v:"+k {
				t.Errorf("got %q", rr.Body)
			}
		})
	}
	rr := a.must(http.StatusOK, "GET", "/v1/b/odd", a.admin, nil)
	var list listJSON
	json.Unmarshal(rr.Body.Bytes(), &list)
	if len(list.Objects) != len(keys) {
		t.Errorf("listed %d objects, want %d", len(list.Objects), len(keys))
	}
}

func TestRangeRequests(t *testing.T) {
	a := newTestAPI(t)
	a.must(http.StatusCreated, "PUT", "/v1/buckets/r-b", a.admin, nil)
	a.must(http.StatusOK, "PUT", "/v1/b/r-b/k", a.admin, strings.NewReader("0123456789"))
	tests := []struct {
		rng         string
		wantStatus  int
		wantBody    string
		wantContRng string
	}{
		{"bytes=2-4", 206, "234", "bytes 2-4/10"},
		{"bytes=7-", 206, "789", "bytes 7-9/10"},
		{"bytes=-3", 206, "789", "bytes 7-9/10"},
		{"bytes=8-100", 206, "89", "bytes 8-9/10"},
		{"bytes=10-", 416, "", "bytes */10"},
		{"bytes=-0", 416, "", "bytes */10"},
		{"bytes=0-1,4-5", 200, "0123456789", ""}, // multi-range: ignored
		{"bytes=5-2", 200, "0123456789", ""},     // invalid: ignored
		{"items=0-1", 200, "0123456789", ""},
	}
	for _, tt := range tests {
		t.Run(tt.rng, func(t *testing.T) {
			rr := a.must(tt.wantStatus, "GET", "/v1/b/r-b/k", a.admin, nil, "Range", tt.rng)
			if got := rr.Header().Get("Content-Range"); got != tt.wantContRng {
				t.Errorf("Content-Range = %q, want %q", got, tt.wantContRng)
			}
			if tt.wantStatus == 416 {
				if c := errCode(t, rr); c != CodeInvalidRange {
					t.Errorf("code = %s", c)
				}
				return
			}
			if rr.Body.String() != tt.wantBody {
				t.Errorf("body = %q, want %q", rr.Body, tt.wantBody)
			}
		})
	}
}

func TestAuthorization(t *testing.T) {
	a := newTestAPI(t)
	for _, b := range []string{"inv", "photos"} {
		a.must(http.StatusCreated, "PUT", "/v1/buckets/"+b, a.admin, nil)
		a.must(http.StatusOK, "PUT", "/v1/b/"+b+"/k", a.admin, strings.NewReader("x"))
	}
	reader := a.key("inv:r")
	writer := a.key("inv:w")
	bucketAdmin := a.key("inv:admin")

	tests := []struct {
		name     string
		method   string
		path     string
		token    string
		want     int
		wantCode string
	}{
		{"no key", "GET", "/v1/b/inv/k", "", 401, CodeUnauthorized},
		{"bad key", "GET", "/v1/b/inv/k", "bf_bogus", 401, CodeUnauthorized},
		{"reader reads", "GET", "/v1/b/inv/k", reader, 200, ""},
		{"reader lists", "GET", "/v1/b/inv", reader, 200, ""},
		{"reader cannot write", "PUT", "/v1/b/inv/k", reader, 403, CodeAccessDenied},
		{"reader cannot delete", "DELETE", "/v1/b/inv/k", reader, 403, CodeAccessDenied},
		{"reader other bucket", "GET", "/v1/b/photos/k", reader, 403, CodeAccessDenied},
		{"writer writes", "PUT", "/v1/b/inv/k2", writer, 200, ""},
		{"writer cannot read", "GET", "/v1/b/inv/k", writer, 403, CodeAccessDenied},
		{"denied before existence leaks", "GET", "/v1/b/secret-bucket/k", reader, 403, CodeAccessDenied},
		{"reader cannot create bucket", "PUT", "/v1/buckets/new-one", reader, 403, CodeAccessDenied},
		{"bucket admin cannot create bucket", "PUT", "/v1/buckets/new-one", bucketAdmin, 403, CodeAccessDenied},
		{"reader cannot list keys", "GET", "/v1/keys", reader, 403, CodeAccessDenied},
		{"bucket admin cannot list keys", "GET", "/v1/keys", bucketAdmin, 403, CodeAccessDenied},
		{"bucket admin cannot delete other bucket", "DELETE", "/v1/buckets/photos", bucketAdmin, 403, CodeAccessDenied},
		{"bucket admin deletes own (non-empty)", "DELETE", "/v1/buckets/inv", bucketAdmin, 409, CodeBucketNotEmpty},
		{"healthz needs no key", "GET", "/healthz", "", 200, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := a.must(tt.want, tt.method, tt.path, tt.token, strings.NewReader("y"))
			if tt.wantCode != "" {
				if c := errCode(t, rr); c != tt.wantCode {
					t.Errorf("code = %s, want %s", c, tt.wantCode)
				}
			}
			if tt.want == 401 && rr.Header().Get("WWW-Authenticate") == "" {
				t.Error("401 without WWW-Authenticate")
			}
		})
	}

	// Bucket listing is filtered to what the key can read.
	rr := a.must(http.StatusOK, "GET", "/v1/buckets", reader, nil)
	if !strings.Contains(rr.Body.String(), `"inv"`) || strings.Contains(rr.Body.String(), "photos") {
		t.Errorf("reader sees buckets %s", rr.Body)
	}
}

func TestPresignFlow(t *testing.T) {
	a := newTestAPI(t)
	a.must(http.StatusCreated, "PUT", "/v1/buckets/share", a.admin, nil)
	presign := func(token, method, key string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"bucket": "share", "key": key, "method": method, "expiresIn": 60})
		return a.do("POST", "/v1/presign", token, strings.NewReader(string(body)))
	}
	pathOf := func(rr *httptest.ResponseRecorder) string {
		t.Helper()
		if rr.Code != http.StatusOK {
			t.Fatalf("presign = %d %s", rr.Code, rr.Body)
		}
		var resp presignResponse
		json.Unmarshal(rr.Body.Bytes(), &resp)
		u, err := url.Parse(resp.URL)
		if err != nil || u.Host != "files.example.com" || u.Scheme != "https" {
			t.Fatalf("presigned URL %q", resp.URL)
		}
		return u.RequestURI()
	}

	key := "reports/q1 final.pdf"
	putURL := pathOf(presign(a.admin, "PUT", key))
	a.must(http.StatusOK, "PUT", putURL, "", strings.NewReader("quarterly"))
	getURL := pathOf(presign(a.admin, "GET", key))
	if rr := a.must(http.StatusOK, "GET", getURL, "", nil); rr.Body.String() != "quarterly" {
		t.Errorf("presigned GET body %q", rr.Body)
	}
	a.must(http.StatusOK, "HEAD", getURL, "", nil)

	// A GET URL cannot write, delete, list, or reach another key.
	if c := errCode(t, a.must(403, "PUT", getURL, "", strings.NewReader("evil"))); c != CodeSignatureInvalid {
		t.Errorf("PUT with GET url: %s", c)
	}
	a.must(403, "DELETE", getURL, "", nil)
	other := strings.Replace(getURL, "final", "draft", 1)
	a.must(403, "GET", other, "", nil)
	a.must(401, "GET", "/v1/b/share?"+strings.SplitN(getURL, "?", 2)[1], "", nil)

	// You cannot presign what you cannot do yourself.
	reader := a.key("share:r")
	if rr := presign(reader, "PUT", key); rr.Code != 403 {
		t.Errorf("reader presigning PUT = %d", rr.Code)
	}
	if rr := presign(reader, "GET", key); rr.Code != 200 {
		t.Errorf("reader presigning GET = %d", rr.Code)
	}
	if rr := presign(a.admin, "DELETE", key); rr.Code != 400 {
		t.Errorf("presigning DELETE = %d", rr.Code)
	}
}

func TestKeysAPI(t *testing.T) {
	a := newTestAPI(t)
	rr := a.must(http.StatusCreated, "POST", "/v1/keys", a.admin,
		strings.NewReader(`{"name":"backup job","scopes":["inv:rw"]}`))
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Error("key creation response is cacheable")
	}
	var created keyJSON
	json.Unmarshal(rr.Body.Bytes(), &created)
	if !strings.HasPrefix(created.Key, "bf_") || created.ID == "" {
		t.Fatalf("created = %+v", created)
	}

	// The new key works.
	a.must(http.StatusCreated, "PUT", "/v1/buckets/inv", a.admin, nil)
	a.must(http.StatusOK, "PUT", "/v1/b/inv/x", created.Key, strings.NewReader("1"))

	rr = a.must(http.StatusOK, "GET", "/v1/keys", a.admin, nil)
	if strings.Contains(rr.Body.String(), created.Key) || strings.Contains(rr.Body.String(), "hash") {
		t.Fatalf("key listing leaks secrets: %s", rr.Body)
	}
	if !strings.Contains(rr.Body.String(), created.ID) {
		t.Fatalf("listing misses new key: %s", rr.Body)
	}

	tests := []struct {
		name     string
		body     string
		wantCode string
	}{
		{"bad scope", `{"name":"x","scopes":["inv:everything"]}`, CodeInvalidScope},
		{"no scopes", `{"name":"x","scopes":[]}`, CodeInvalidScope},
		{"no name", `{"scopes":["inv:r"]}`, CodeInvalidRequest},
		{"unknown field", `{"name":"x","scopes":["inv:r"],"admin":true}`, CodeInvalidRequest},
		{"not json", `name=x`, CodeInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := a.must(http.StatusBadRequest, "POST", "/v1/keys", a.admin, strings.NewReader(tt.body))
			if c := errCode(t, rr); c != tt.wantCode {
				t.Errorf("code = %s, want %s", c, tt.wantCode)
			}
		})
	}

	a.must(http.StatusNoContent, "DELETE", "/v1/keys/"+created.ID, a.admin, nil)
	a.must(http.StatusUnauthorized, "GET", "/v1/b/inv/x", created.Key, nil)
	a.must(http.StatusNotFound, "DELETE", "/v1/keys/"+created.ID, a.admin, nil)

	keys, _ := a.auth.ListKeys(context.Background())
	if c := errCode(t, a.must(http.StatusConflict, "DELETE", "/v1/keys/"+keys[0].ID, a.admin, nil)); c != CodeLastAdminKey {
		t.Errorf("deleting last admin: %s", c)
	}
}

func TestListObjectsAPI(t *testing.T) {
	a := newTestAPI(t)
	a.must(http.StatusCreated, "PUT", "/v1/buckets/lst", a.admin, nil)
	for _, k := range []string{"a", "d/1", "d/2", "z"} {
		a.must(http.StatusOK, "PUT", "/v1/b/lst/"+k, a.admin, strings.NewReader(k))
	}
	rr := a.must(http.StatusOK, "GET", "/v1/b/lst?delimiter=/&limit=2", a.admin, nil)
	var page listJSON
	json.Unmarshal(rr.Body.Bytes(), &page)
	if len(page.Objects) != 1 || page.Objects[0].Key != "a" || len(page.Prefixes) != 1 || page.Prefixes[0] != "d/" || page.NextCursor == "" {
		t.Fatalf("page 1 = %s", rr.Body)
	}
	rr = a.must(http.StatusOK, "GET", "/v1/b/lst?delimiter=/&limit=2&cursor="+page.NextCursor, a.admin, nil)
	page = listJSON{} // nextCursor is omitted when empty; don't keep page 1's
	json.Unmarshal(rr.Body.Bytes(), &page)
	if len(page.Objects) != 1 || page.Objects[0].Key != "z" || page.NextCursor != "" {
		t.Fatalf("page 2 = %s", rr.Body)
	}
	if c := errCode(t, a.must(400, "GET", "/v1/b/lst?limit=zero", a.admin, nil)); c != CodeInvalidRequest {
		t.Errorf("bad limit: %s", c)
	}
	if c := errCode(t, a.must(400, "GET", "/v1/b/lst?cursor=%21%21", a.admin, nil)); c != CodeInvalidCursor {
		t.Errorf("bad cursor: %s", c)
	}
	// Empty lists are [] not null.
	a.must(http.StatusCreated, "PUT", "/v1/buckets/empty", a.admin, nil)
	if rr := a.must(200, "GET", "/v1/b/empty", a.admin, nil); !strings.Contains(rr.Body.String(), `"objects":[]`) {
		t.Errorf("empty list = %s", rr.Body)
	}
}

func TestErrorMapping(t *testing.T) {
	a := newTestAPI(t)
	a.must(http.StatusCreated, "PUT", "/v1/buckets/m-b", a.admin, nil)
	big := strings.Repeat("x", 1<<20+1)
	tests := []struct {
		name     string
		method   string
		path     string
		body     string
		want     int
		wantCode string
	}{
		{"missing bucket", "PUT", "/v1/b/nope-bucket/k", "x", 404, CodeNoSuchBucket},
		{"bad bucket name", "PUT", "/v1/buckets/Bad_Name", "", 400, CodeInvalidBucketName},
		{"duplicate bucket", "PUT", "/v1/buckets/m-b", "", 409, CodeBucketExists},
		{"empty key", "PUT", "/v1/b/m-b/", "x", 400, CodeInvalidKey},
		{"too large", "PUT", "/v1/b/m-b/big", big, 413, CodeEntityTooLarge},
		{"wrong method on object", "POST", "/v1/b/m-b/k", "", 405, CodeMethodNotAllowed},
		{"wrong method on bucket", "DELETE", "/v1/b/m-b", "", 405, CodeMethodNotAllowed},
		{"unknown route", "GET", "/v2/whatever", "", 404, CodeNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := a.must(tt.want, tt.method, tt.path, a.admin, strings.NewReader(tt.body))
			if c := errCode(t, rr); c != tt.wantCode {
				t.Errorf("code = %s, want %s", c, tt.wantCode)
			}
		})
	}
	// Big metadata.
	rr := a.must(400, "PUT", "/v1/b/m-b/k", a.admin, strings.NewReader("x"), "X-Meta-Big", strings.Repeat("v", 3000))
	if c := errCode(t, rr); c != CodeMetadataTooLarge {
		t.Errorf("code = %s", c)
	}
}

// TestCorruptDownloadAborts checks that bit rot discovered mid-stream cuts
// the connection instead of ending the response cleanly.
func TestCorruptDownloadAborts(t *testing.T) {
	dir := t.TempDir()
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	svc := auth.NewService(eng, eng.PresignSecret())
	admin, _ := svc.EnsureAdminKey(context.Background())
	srv := httptest.NewServer(New(Options{Logger: slog.New(slog.DiscardHandler), MaxObjectSize: 1 << 20,
		Store: eng, Buckets: eng, Auth: svc}))
	defer srv.Close()

	ctx := context.Background()
	eng.CreateBucket(ctx, "c-b")
	eng.Put(ctx, "c-b", "k", strings.NewReader(strings.Repeat("a", 100_000)), storeMeta())
	// Flip one byte in the only blob file.
	var blob string
	walk(t, dir+"/objects", func(p string) { blob = p })
	b, _ := os.ReadFile(blob)
	b[50_000] ^= 1
	os.WriteFile(blob, b, 0o600)

	req, _ := http.NewRequest("GET", srv.URL+"/v1/b/c-b/k", nil)
	req.Header.Set("Authorization", "Bearer "+admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err == nil {
		t.Fatal("corrupt download completed cleanly; want a broken connection")
	}
}

func storeMeta() store.Meta { return store.Meta{} }

// walk calls fn for every regular file under dir.
func walk(t *testing.T, dir string, fn func(path string)) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			fn(p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSandboxScriptableContent(t *testing.T) {
	a := newTestAPI(t)
	a.must(http.StatusCreated, "PUT", "/v1/buckets/sbx", a.admin, nil)
	tests := []struct {
		contentType string
		sandboxed   bool
	}{
		{"text/html", true},
		{"text/html; charset=utf-8", true},
		{"TEXT/HTML", true},
		{"image/svg+xml", true},
		{"application/xhtml+xml", true},
		{"application/atom+xml", true},
		{"text/xml", true},
		{"not a / valid type;;", true},
		{"application/pdf", false},
		{"image/png", false},
		{"text/plain", false},
		{"application/octet-stream", false},
	}
	for _, tt := range tests {
		t.Run(tt.contentType, func(t *testing.T) {
			a.must(http.StatusOK, "PUT", "/v1/b/sbx/f", a.admin, strings.NewReader("<script>alert(1)</script>"),
				"Content-Type", tt.contentType)
			for _, method := range []string{"GET", "HEAD"} {
				rr := a.must(http.StatusOK, method, "/v1/b/sbx/f", a.admin, nil)
				got := rr.Header().Get("Content-Security-Policy") == "sandbox"
				if got != tt.sandboxed {
					t.Errorf("%s: sandboxed = %v, want %v", method, got, tt.sandboxed)
				}
				if rr.Header().Get("X-Content-Type-Options") != "nosniff" {
					t.Errorf("%s: missing nosniff", method)
				}
			}
		})
	}
}

func TestRootRedirectsToUI(t *testing.T) {
	a := newTestAPI(t)
	rr := a.must(http.StatusFound, "GET", "/", "", nil)
	if loc := rr.Header().Get("Location"); loc != "/ui/" {
		t.Errorf("Location = %q", loc)
	}
	a.must(http.StatusOK, "GET", "/ui/", "", nil)
}
