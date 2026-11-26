package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/nitishkdas/blobfish/internal/auth"
	"github.com/nitishkdas/blobfish/internal/engine"
	"github.com/nitishkdas/blobfish/internal/store"
)

type fakeHealth struct{ rep store.HealthReport }

func (f fakeHealth) Health(context.Context) store.HealthReport { return f.rep }

func TestHealthz(t *testing.T) {
	secret := errors.New("open /var/lib/blobfish/meta.db: input/output error")
	tests := []struct {
		name       string
		rep        store.HealthReport
		wantCode   int
		wantStatus string
		wantChecks map[string]string
	}{
		{"ok", store.HealthReport{FreeBytes: 1 << 30}, 200, "ok",
			map[string]string{"metadata": "ok", "disk": "ok", "space": "ok"}},
		{"low space is degraded, still 200", store.HealthReport{FreeBytes: 10, LowSpace: true}, 200, "degraded",
			map[string]string{"metadata": "ok", "disk": "ok", "space": "low"}},
		{"space unknown", store.HealthReport{FreeBytes: -1}, 200, "ok",
			map[string]string{"metadata": "ok", "disk": "ok", "space": "unknown"}},
		{"disk failing", store.HealthReport{Disk: secret, FreeBytes: 1}, 503, "unavailable",
			map[string]string{"metadata": "ok", "disk": "failing", "space": "ok"}},
		{"metadata failing", store.HealthReport{Metadata: secret, FreeBytes: 1}, 503, "unavailable",
			map[string]string{"metadata": "failing", "disk": "ok", "space": "ok"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := &lockedBuffer{}
			a := &testAPI{t: t, h: New(Options{Logger: slog.New(slog.NewTextHandler(logs, nil)), Health: fakeHealth{tt.rep}})}
			rr := a.must(tt.wantCode, "GET", "/healthz", "", nil)
			var body struct {
				Status string
				Checks map[string]string
			}
			json.Unmarshal(rr.Body.Bytes(), &body)
			if body.Status != tt.wantStatus || len(body.Checks) != 3 {
				t.Errorf("body = %s", rr.Body)
			}
			for k, v := range tt.wantChecks {
				if body.Checks[k] != v {
					t.Errorf("check %s = %q, want %q", k, body.Checks[k], v)
				}
			}
			if strings.Contains(rr.Body.String(), "meta.db") {
				t.Error("unauthenticated health output reveals internal details")
			}
			if tt.wantCode == 503 && !strings.Contains(logs.String(), "input/output error") {
				t.Error("failure details were not logged for the operator")
			}
		})
	}
}

// apiWithLogs is a full API over a real engine whose logs can be read.
func apiWithLogs(t *testing.T) (*testAPI, *lockedBuffer) {
	t.Helper()
	eng, err := engine.Open(t.TempDir(), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	svc := auth.NewService(eng, eng.PresignSecret())
	admin, _ := svc.EnsureAdminKey(context.Background())
	logs := &lockedBuffer{}
	a := &testAPI{t: t, auth: svc, eng: eng, admin: admin, h: New(Options{
		Logger: slog.New(slog.NewTextHandler(logs, nil)), MaxObjectSize: 1 << 20,
		Store: eng, Buckets: eng, Auth: svc, Links: eng, Health: eng, Version: "v-test", DataFormat: engine.FormatVersion,
		PublicURL: "https://files.example.com",
	})}
	return a, logs
}

func TestStatusEndpoint(t *testing.T) {
	a, _ := apiWithLogs(t)
	a.must(http.StatusCreated, "PUT", "/v1/buckets/st-b", a.admin, nil)
	a.must(http.StatusOK, "PUT", "/v1/b/st-b/k", a.admin, strings.NewReader(strings.Repeat("x", 1000)))
	a.must(http.StatusNotFound, "GET", "/v1/b/st-b/missing", a.admin, nil)
	a.must(http.StatusUnauthorized, "GET", "/v1/status", "", nil)
	a.must(http.StatusForbidden, "GET", "/v1/status", a.key("st-b:admin"), nil)

	rr := a.must(http.StatusOK, "GET", "/v1/status", a.admin, nil)
	var st statusJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Version != "v-test" || st.DataFormat != engine.FormatVersion || st.Disk.FreeBytes <= 0 {
		t.Errorf("status = %s", rr.Body)
	}
	// So far: create(201), put(200), 404, 401, 403. The status request itself
	// is counted when it finishes, so it isn't included yet.
	r := st.Requests
	if r.Total != 5 || r.S2xx != 2 || r.S4xx != 3 || st.Bytes.In < 1000 {
		t.Errorf("counters = %+v, bytes = %+v", r, st.Bytes)
	}
}

func TestAccessLogNamesTheActor(t *testing.T) {
	a, logs := apiWithLogs(t)
	keys, _ := a.auth.ListKeys(context.Background())
	adminID := keys[0].ID
	a.must(http.StatusCreated, "PUT", "/v1/buckets/who-b", a.admin, nil)
	a.must(http.StatusOK, "PUT", "/v1/b/who-b/f.txt", a.admin, strings.NewReader("x"))

	lastLine := func(path string) string {
		t.Helper()
		lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if strings.Contains(lines[i], "msg=request") && strings.Contains(lines[i], "path="+path) {
				return lines[i]
			}
		}
		t.Fatalf("no access log line for %s", path)
		return ""
	}
	keyField := regexp.MustCompile(` key=(\S+)`)
	actor := func(path string) string {
		if m := keyField.FindStringSubmatch(lastLine(path)); m != nil {
			return m[1]
		}
		return ""
	}

	if got := actor("/v1/b/who-b/f.txt"); got != "bootstrap-admin/"+adminID {
		t.Errorf("API key request logged as %q", got)
	}

	body, _ := json.Marshal(map[string]any{"bucket": "who-b", "key": "f.txt", "method": "GET"})
	rr := a.must(http.StatusOK, "POST", "/v1/presign", a.admin, strings.NewReader(string(body)))
	var p presignResponse
	json.Unmarshal(rr.Body.Bytes(), &p)
	u, _ := url.Parse(p.URL)
	a.must(http.StatusOK, "GET", u.RequestURI(), "", nil)
	if got := actor("/v1/b/who-b/f.txt"); got != "presigned" {
		t.Errorf("presigned request logged as %q", got)
	}

	l := createLinkJSON(t, a, a.admin, "who-b", "f.txt", 0, http.StatusCreated)
	a.must(http.StatusOK, "GET", linkPath(t, l), "", nil)
	if got := actor("/s/"); got != "link:"+l.ID { // log path is /s/<token>/<name>
		t.Errorf("share link request logged as %q", got)
	}

	a.must(http.StatusOK, "PATCH", "/v1/buckets/who-b", a.admin, strings.NewReader(`{"public":true}`))
	a.must(http.StatusOK, "GET", "/v1/b/who-b/f.txt", "", nil)
	if got := actor("/v1/b/who-b/f.txt"); got != "public" {
		t.Errorf("public read logged as %q", got)
	}

	a.must(http.StatusUnauthorized, "GET", "/v1/buckets", "", nil)
	if got := actor("/v1/buckets"); got != "" {
		t.Errorf("unauthenticated request logged with key %q", got)
	}
	if strings.Contains(logs.String(), a.admin) {
		t.Fatal("an API key secret appeared in the log")
	}
}
