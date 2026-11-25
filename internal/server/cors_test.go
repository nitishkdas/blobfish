package server

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCORS(t *testing.T) {
	const app = "https://app.example.com"
	tests := []struct {
		name        string
		allowed     []string
		method      string
		origin      string
		preflight   bool
		wantStatus  int
		wantACAO    string
		wantExposed bool
	}{
		{"same-origin request untouched", []string{app}, "GET", "", false, 200, "", false},
		{"allowed preflight", []string{app}, "OPTIONS", app, true, 204, app, false},
		{"allowed real request", []string{app}, "GET", app, false, 200, app, true},
		{"other origin preflight refused", []string{app}, "OPTIONS", "https://evil.example", true, 403, "", false},
		{"other origin real request gets no CORS headers", []string{app}, "GET", "https://evil.example", false, 200, "", false},
		{"port is part of the origin", []string{app}, "OPTIONS", app + ":8443", true, 403, "", false},
		{"wildcard", []string{"*"}, "OPTIONS", "https://anything.example", true, 204, "*", false},
		{"disabled by default", nil, "GET", app, false, 200, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := corsMiddleware(tt.allowed)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
			}))
			r := httptest.NewRequest(tt.method, "/v1/b/bucket/key", nil)
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}
			if tt.preflight {
				r.Header.Set("Access-Control-Request-Method", "PUT")
				r.Header.Set("Access-Control-Request-Headers", "content-type, x-meta-owner")
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)
			got := rr.Header()
			if rr.Code != tt.wantStatus || got.Get("Access-Control-Allow-Origin") != tt.wantACAO {
				t.Fatalf("status %d, ACAO %q; want %d, %q", rr.Code, got.Get("Access-Control-Allow-Origin"), tt.wantStatus, tt.wantACAO)
			}
			if tt.wantStatus == 204 {
				if !strings.Contains(got.Get("Access-Control-Allow-Methods"), "PUT") ||
					got.Get("Access-Control-Allow-Headers") != "content-type, x-meta-owner" {
					t.Errorf("preflight headers %v", got)
				}
			}
			if exposed := strings.Contains(got.Get("Access-Control-Expose-Headers"), "ETag"); exposed != tt.wantExposed {
				t.Errorf("ETag exposed = %v, want %v", exposed, tt.wantExposed)
			}
			if tt.origin != "" && len(tt.allowed) > 0 && !strings.Contains(got.Get("Vary"), "Origin") {
				t.Error("missing Vary: Origin (caches could serve one origin's answer to another)")
			}
		})
	}
}

// Through the full stack: a preflight for a presigned PUT is answered
// before authentication, and error replies stay readable by the page.
func TestCORSFullStack(t *testing.T) {
	base := newTestAPI(t)
	a := &testAPI{t: t, eng: base.eng, auth: base.auth, admin: base.admin, h: New(Options{
		Logger: slog.New(slog.DiscardHandler), MaxObjectSize: 1 << 20,
		Store: base.eng, Buckets: base.eng, Auth: base.auth, Links: base.eng,
		CORSAllowedOrigins: []string{"https://app.example.com"},
	})}
	rr := a.must(http.StatusNoContent, "OPTIONS", "/v1/b/some-bucket/upload.pdf", "", nil,
		"Origin", "https://app.example.com", "Access-Control-Request-Method", "PUT")
	if rr.Header().Get("Access-Control-Allow-Origin") != "https://app.example.com" {
		t.Errorf("preflight headers %v", rr.Header())
	}
	rr = a.must(http.StatusUnauthorized, "GET", "/v1/buckets", "", nil, "Origin", "https://app.example.com")
	if rr.Header().Get("Access-Control-Allow-Origin") == "" {
		t.Error("error reply not readable by the allowed page")
	}
	if c := errCode(t, a.must(http.StatusForbidden, "OPTIONS", "/v1/b/some-bucket/x", "", nil,
		"Origin", "https://evil.example", "Access-Control-Request-Method", "PUT")); c != CodeCORSNotAllowed {
		t.Errorf("code = %s", c)
	}
}
