package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestValidateFilename(t *testing.T) {
	tests := []struct {
		name string
		ok   bool
	}{
		{"report.pdf", true},
		{"résumé 2026.pdf", true},
		{"no-extension", true},
		{strings.Repeat("a", maxFilenameLen), true},
		{"", false},
		{strings.Repeat("a", maxFilenameLen+1), false},
		{".", false},
		{"..", false},
		{"a/b.pdf", false},
		{`a\b.pdf`, false},
		{" lead.pdf", false},
		{"trail.pdf ", false},
		{"tab\there.pdf", false},
		{"new\nline.pdf", false},
		{"del\x7f.pdf", false},
		{"bad\xffutf8", false},
	}
	for _, tt := range tests {
		if err := validateFilename(tt.name); (err == nil) != tt.ok {
			t.Errorf("validateFilename(%q) = %v, want ok=%v", tt.name, err, tt.ok)
		}
	}
}

func TestDownloadNames(t *testing.T) {
	a := newTestAPI(t)
	a.must(http.StatusCreated, "PUT", "/v1/buckets/docs", a.admin, nil)
	a.must(http.StatusOK, "PUT", "/v1/b/docs/2026/report%20q1.pdf", a.admin, strings.NewReader("pdf"),
		"Content-Type", "application/pdf")
	reader := a.key("docs:r")
	obj := "/v1/b/docs/2026/report%20q1.pdf"

	t.Run("object GET", func(t *testing.T) {
		tests := []struct {
			query, want string
			status      int
		}{
			{"", "", http.StatusOK}, // unchanged: no header unless asked
			{"?download", `attachment; filename="report q1.pdf"`, http.StatusOK},
			{"?download=", `attachment; filename="report q1.pdf"`, http.StatusOK},
			{"?download=invoice-7.pdf", "attachment; filename=invoice-7.pdf", http.StatusOK},
			{"?download=" + url.QueryEscape("résumé.pdf"), "attachment; filename*=utf-8''r%C3%A9sum%C3%A9.pdf", http.StatusOK},
			{"?download=a%2Fb.pdf", "", http.StatusBadRequest},
			{"?download=..", "", http.StatusBadRequest},
		}
		for _, tt := range tests {
			for _, method := range []string{"GET", "HEAD"} {
				rr := a.must(tt.status, method, obj+tt.query, reader, nil)
				if got := rr.Header().Get("Content-Disposition"); tt.status == http.StatusOK && got != tt.want {
					t.Errorf("%s %s: Content-Disposition = %q, want %q", method, tt.query, got, tt.want)
				}
			}
		}
	})

	t.Run("auth comes first", func(t *testing.T) {
		// A bad name must not turn a 401/403 into a 400.
		a.must(http.StatusUnauthorized, "GET", obj+"?download=a%2Fb", "", nil)
		a.must(http.StatusForbidden, "GET", obj+"?download=a%2Fb", a.key("other:r"), nil)
	})

	t.Run("presigned URL", func(t *testing.T) {
		// ?download is not part of the signature, so it can be added to
		// any presigned GET.
		body := `{"bucket":"docs","key":"2026/report q1.pdf","method":"GET","expiresIn":60}`
		var resp presignResponse
		json.Unmarshal(a.must(http.StatusOK, "POST", "/v1/presign", reader, strings.NewReader(body)).Body.Bytes(), &resp)
		u, _ := url.Parse(resp.URL)
		rr := a.must(http.StatusOK, "GET", u.RequestURI()+"&download=q1.pdf", "", nil)
		if got := rr.Header().Get("Content-Disposition"); got != "attachment; filename=q1.pdf" || rr.Body.String() != "pdf" {
			t.Errorf("Content-Disposition = %q, body %q", got, rr.Body)
		}
	})

	t.Run("share link filename", func(t *testing.T) {
		create := func(filename string, want int) linkJSON {
			t.Helper()
			body, _ := json.Marshal(map[string]any{"bucket": "docs", "key": "2026/report q1.pdf", "filename": filename})
			var l linkJSON
			json.Unmarshal(a.must(want, "POST", "/v1/links", reader, strings.NewReader(string(body))).Body.Bytes(), &l)
			return l
		}
		create("a/b.pdf", http.StatusBadRequest)
		create(" x.pdf", http.StatusBadRequest)

		plain := create("", http.StatusCreated)
		named := create("Q1 Report – ACME.pdf", http.StatusCreated)
		if plain.Filename != "report q1.pdf" || named.Filename != "Q1 Report – ACME.pdf" {
			t.Errorf("filenames %q, %q", plain.Filename, named.Filename)
		}
		if !strings.HasSuffix(named.URL, "/"+url.PathEscape("Q1 Report – ACME.pdf")) {
			t.Errorf("named URL %q", named.URL)
		}

		tests := []struct {
			link        linkJSON
			query, want string
		}{
			{plain, "", `inline; filename="report q1.pdf"`},
			{plain, "?download", `attachment; filename="report q1.pdf"`},
			{named, "", `inline; filename*=utf-8''Q1%20Report%20%E2%80%93%20ACME.pdf`},
			{named, "?download", `attachment; filename*=utf-8''Q1%20Report%20%E2%80%93%20ACME.pdf`},
			{named, "?download=other.pdf", "attachment; filename=other.pdf"},
		}
		for _, tt := range tests {
			rr := a.must(http.StatusOK, "GET", linkPath(t, tt.link)+tt.query, "", nil)
			if got := rr.Header().Get("Content-Disposition"); got != tt.want {
				t.Errorf("%s%s: Content-Disposition = %q, want %q", tt.link.Filename, tt.query, got, tt.want)
			}
		}
		a.must(http.StatusBadRequest, "GET", linkPath(t, named)+"?download=%00", "", nil)

		// The listing shows the chosen name too.
		var list struct{ Links []linkJSON }
		json.Unmarshal(a.must(http.StatusOK, "GET", "/v1/links?bucket=docs", reader, nil).Body.Bytes(), &list)
		found := false
		for _, l := range list.Links {
			found = found || (l.ID == named.ID && l.Filename == named.Filename)
		}
		if !found {
			t.Errorf("listing lacks the named link: %+v", list.Links)
		}
	})
}
