package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func createUploadLinkJSON(t *testing.T, a *testAPI, token string, req map[string]any, want int) uploadLinkJSON {
	t.Helper()
	body, _ := json.Marshal(req)
	var l uploadLinkJSON
	json.Unmarshal(a.must(want, "POST", "/v1/uploadlinks", token, strings.NewReader(string(body))).Body.Bytes(), &l)
	return l
}

func uploadPath(t *testing.T, l uploadLinkJSON) string {
	t.Helper()
	u, err := url.Parse(l.URL)
	if err != nil || !strings.HasPrefix(u.Path, "/u/") {
		t.Fatalf("upload link url %q", l.URL)
	}
	return u.Path
}

func TestUploadLinkAPI(t *testing.T) {
	a := newTestAPI(t)
	a.must(http.StatusCreated, "PUT", "/v1/buckets/inbox", a.admin, nil)
	writer := a.key("inbox:rw")
	reader := a.key("inbox:r")

	// Only writers may hand out upload access.
	createUploadLinkJSON(t, a, reader, map[string]any{"bucket": "inbox"}, http.StatusForbidden)
	createUploadLinkJSON(t, a, "", map[string]any{"bucket": "inbox"}, http.StatusUnauthorized)
	createUploadLinkJSON(t, a, writer, map[string]any{"bucket": "inbox", "maxFiles": -1}, http.StatusBadRequest)
	createUploadLinkJSON(t, a, writer, map[string]any{"bucket": "inbox", "maxFileSize": 2 << 20}, http.StatusBadRequest) // > server max (1 MiB in tests)
	createUploadLinkJSON(t, a, writer, map[string]any{"bucket": "inbox", "folder": "a//b"}, http.StatusBadRequest)
	createUploadLinkJSON(t, a, writer, map[string]any{"bucket": "inbox", "label": "bad\nlabel"}, http.StatusBadRequest)

	l := createUploadLinkJSON(t, a, writer, map[string]any{
		"bucket": "inbox", "folder": "/from-acme", "label": "Receipts for ACME", "maxFiles": 3, "maxFileSize": 10, "expiresIn": 3600,
	}, http.StatusCreated)
	if l.Prefix != "from-acme/" || l.MaxFiles != 3 || l.Expires == nil || l.Uploads != 0 {
		t.Fatalf("link = %+v", l)
	}
	p := uploadPath(t, l)

	t.Run("page and info reveal nothing about the bucket", func(t *testing.T) {
		rr := a.must(http.StatusOK, "GET", p, "", nil)
		if !strings.Contains(rr.Body.String(), "upload-page.js") || rr.Header().Get("Referrer-Policy") != "no-referrer" ||
			!strings.Contains(rr.Header().Get("Content-Security-Policy"), "script-src 'self'") {
			t.Errorf("page headers %v", rr.Header())
		}
		body := a.must(http.StatusOK, "GET", p+"/info", "", nil).Body.String()
		if !strings.Contains(body, `"label":"Receipts for ACME"`) || !strings.Contains(body, `"filesLeft":3`) ||
			!strings.Contains(body, `"maxFileSize":10`) || strings.Contains(body, "inbox") || strings.Contains(body, "acme/") {
			t.Errorf("info = %s", body)
		}
		a.must(http.StatusNotFound, "GET", "/u/nope/info", "", nil)
	})

	t.Run("uploads add, never replace", func(t *testing.T) {
		a.must(http.StatusOK, "PUT", "/v1/b/inbox/from-acme/r.pdf", writer, strings.NewReader("mine"))
		rr := a.must(http.StatusCreated, "PUT", p+"/r.pdf", "", strings.NewReader("theirs"), "Content-Type", "application/pdf")
		if rr.Body.String() != `{"size":6}`+"\n" {
			t.Errorf("upload result %s (must not reveal the stored name)", rr.Body)
		}
		if got := a.must(http.StatusOK, "GET", "/v1/b/inbox/from-acme/r.pdf", writer, nil).Body.String(); got != "mine" {
			t.Errorf("existing file replaced: %q", got)
		}
		rr = a.must(http.StatusOK, "HEAD", "/v1/b/inbox/from-acme/r%20(2).pdf", writer, nil)
		if rr.Header().Get("Content-Type") != "application/pdf" {
			t.Errorf("content type %q", rr.Header().Get("Content-Type"))
		}
	})

	t.Run("limits", func(t *testing.T) {
		if c := errCode(t, a.must(http.StatusRequestEntityTooLarge, "PUT", p+"/big.bin", "", strings.NewReader("12345678901"))); c != CodeEntityTooLarge {
			t.Errorf("too big: %s", c)
		}
		a.must(http.StatusBadRequest, "PUT", p+"/bell%07.txt", "", strings.NewReader("x"))
		a.must(http.StatusTemporaryRedirect, "PUT", p+"/..", "", strings.NewReader("x")) // ServeMux cleans it away; never reaches a handler
		a.must(http.StatusBadRequest, "PUT", p+"/%20lead", "", strings.NewReader("x"))
		// Failed uploads gave their slots back: 1 used, 2 left.
		a.must(http.StatusCreated, "PUT", p+"/two.txt", "", strings.NewReader("2"))
		a.must(http.StatusCreated, "PUT", p+"/three.txt", "", strings.NewReader("3"))
		if c := errCode(t, a.must(http.StatusForbidden, "PUT", p+"/four.txt", "", strings.NewReader("4"))); c != CodeUploadLimit {
			t.Errorf("past maxFiles: %s", c)
		}
		a.must(http.StatusNotFound, "HEAD", "/v1/b/inbox/from-acme/four.txt", writer, nil)
	})

	t.Run("list, expire, revoke", func(t *testing.T) {
		var list struct{ UploadLinks []uploadLinkJSON }
		json.Unmarshal(a.must(http.StatusOK, "GET", "/v1/uploadlinks?bucket=inbox", writer, nil).Body.Bytes(), &list)
		if len(list.UploadLinks) != 1 || list.UploadLinks[0].Uploads != 3 {
			t.Fatalf("list = %+v", list.UploadLinks)
		}
		a.must(http.StatusForbidden, "GET", "/v1/uploadlinks?bucket=inbox", reader, nil)

		old := createUploadLinkJSON(t, a, writer, map[string]any{"bucket": "inbox", "expiresIn": 60}, http.StatusCreated)
		rec, _ := a.eng.UploadLinkByID(t.Context(), old.ID)
		a.eng.DeleteUploadLink(t.Context(), old.ID)
		rec.Expires = time.Now().Add(-time.Minute)
		a.eng.CreateUploadLink(t.Context(), rec)
		if c := errCode(t, a.must(http.StatusGone, "PUT", uploadPath(t, old)+"/x", "", strings.NewReader("x"))); c != CodeLinkExpired {
			t.Errorf("expired: %s", c)
		}
		a.must(http.StatusGone, "GET", uploadPath(t, old)+"/info", "", nil)

		a.must(http.StatusForbidden, "DELETE", "/v1/uploadlinks/"+l.ID, reader, nil)
		a.must(http.StatusNoContent, "DELETE", "/v1/uploadlinks/"+l.ID, writer, nil)
		a.must(http.StatusNotFound, "PUT", p+"/late.txt", "", strings.NewReader("x"))
	})

	t.Run("quota applies", func(t *testing.T) {
		a.must(http.StatusOK, "PATCH", "/v1/buckets/inbox", a.admin, strings.NewReader(`{"quotaBytes": 20}`))
		q := createUploadLinkJSON(t, a, writer, map[string]any{"bucket": "inbox"}, http.StatusCreated)
		if c := errCode(t, a.must(http.StatusInsufficientStorage, "PUT", uploadPath(t, q)+"/fill.bin", "", strings.NewReader(strings.Repeat("x", 100)))); c != CodeQuotaExceeded {
			t.Errorf("over quota: %s", c)
		}
	})
}

func TestUploadLinkFreeNames(t *testing.T) {
	a := newTestAPI(t)
	a.must(http.StatusCreated, "PUT", "/v1/buckets/names", a.admin, nil)
	for _, k := range []string{"a.tar.gz", "README", ".env", "x.pdf", "x (2).pdf"} {
		a.must(http.StatusOK, "PUT", "/v1/b/names/"+url.PathEscape(k), a.admin, strings.NewReader("1"))
	}
	p := uploadPath(t, createUploadLinkJSON(t, a, a.admin, map[string]any{"bucket": "names"}, http.StatusCreated))
	tests := []struct{ name, want string }{
		{"a.tar.gz", "a.tar (2).gz"},
		{"README", "README (2)"},
		{".env", ".env (2)"},
		{"x.pdf", "x (3).pdf"},
		{"new.txt", "new.txt"},
		{"new.txt", "new (2).txt"},
	}
	for _, tt := range tests {
		a.must(http.StatusCreated, "PUT", p+"/"+url.PathEscape(tt.name), "", strings.NewReader("2"))
		if got := a.must(http.StatusOK, "GET", "/v1/b/names/"+url.PathEscape(tt.want), a.admin, nil).Body.String(); got != "2" {
			t.Errorf("%q not saved as %q (got %q)", tt.name, tt.want, got)
		}
	}
	// Every numbered name taken: still stored, under a random suffix.
	for n := 2; n <= maxNameTries; n++ {
		a.must(http.StatusOK, "PUT", "/v1/b/names/"+url.PathEscape(fmt.Sprintf("full (%d).txt", n)), a.admin, strings.NewReader("1"))
	}
	a.must(http.StatusOK, "PUT", "/v1/b/names/full.txt", a.admin, strings.NewReader("1"))
	a.must(http.StatusCreated, "PUT", p+"/full.txt", "", strings.NewReader("new"))
	var list struct{ Objects []struct{ Key string } }
	json.Unmarshal(a.must(http.StatusOK, "GET", "/v1/b/names?prefix=full%20(&limit=1000", a.admin, nil).Body.Bytes(), &list)
	if len(list.Objects) != maxNameTries {
		t.Errorf("%d objects named full (…), want %d", len(list.Objects), maxNameTries)
	}
}

func TestPutIfNoneMatch(t *testing.T) {
	a := newTestAPI(t)
	a.must(http.StatusCreated, "PUT", "/v1/buckets/inm-b", a.admin, nil)
	a.must(http.StatusOK, "PUT", "/v1/b/inm-b/k", a.admin, strings.NewReader("v1"), "If-None-Match", "*")
	if c := errCode(t, a.must(http.StatusPreconditionFailed, "PUT", "/v1/b/inm-b/k", a.admin, strings.NewReader("v2"), "If-None-Match", "*")); c != CodePreconditionFailed {
		t.Errorf("code %s", c)
	}
	if got := a.must(http.StatusOK, "GET", "/v1/b/inm-b/k", a.admin, nil).Body.String(); got != "v1" {
		t.Errorf("replaced: %q", got)
	}
	a.must(http.StatusOK, "PUT", "/v1/b/inm-b/k", a.admin, strings.NewReader("v3")) // without the header: replaces
}

func TestUploadLinkShowsOperator(t *testing.T) {
	a := newTestAPI(t)
	a.must(http.StatusCreated, "PUT", "/v1/buckets/inbox", a.admin, nil)
	p := uploadPath(t, createUploadLinkJSON(t, a, a.admin, map[string]any{"bucket": "inbox"}, http.StatusCreated))
	if body := a.must(http.StatusOK, "GET", p+"/info", "", nil).Body.String(); strings.Contains(body, "operator") {
		t.Errorf("operator shown though not configured: %s", body)
	}
	a.h = New(Options{
		Logger: slog.New(slog.DiscardHandler), MaxObjectSize: 1 << 20,
		Store: a.eng, Buckets: a.eng, Auth: a.auth, UploadLinks: a.eng,
		OperatorName: "Acme Ltd", PrivacyURL: "https://acme.example/privacy",
	})
	body := a.must(http.StatusOK, "GET", p+"/info", "", nil).Body.String()
	if !strings.Contains(body, `"operator":{"name":"Acme Ltd","privacyUrl":"https://acme.example/privacy"}`) {
		t.Errorf("info = %s", body)
	}
}
