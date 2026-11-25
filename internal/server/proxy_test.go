package server

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientIPMiddleware(t *testing.T) {
	trusted, err := ParseTrustedProxies([]string{"127.0.0.1", "10.0.0.0/8", "::1"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		remote    string
		xff       []string
		proto     string
		wantIP    string
		wantProxy string
		wantHTTPS bool
	}{
		{"direct client, no headers", "203.0.113.9:5000", nil, "", "203.0.113.9", "", false},
		{"untrusted peer can't spoof", "203.0.113.9:5000", []string{"1.1.1.1"}, "https", "203.0.113.9", "", false},
		{"trusted proxy", "127.0.0.1:5000", []string{"198.51.100.7"}, "https", "198.51.100.7", "127.0.0.1", true},
		{"forged left entry ignored", "127.0.0.1:5000", []string{"6.6.6.6, 198.51.100.7"}, "https", "198.51.100.7", "127.0.0.1", true},
		{"proxy chain", "127.0.0.1:5000", []string{"198.51.100.7, 10.1.2.3"}, "", "198.51.100.7", "127.0.0.1", false},
		{"multiple headers", "127.0.0.1:5000", []string{"198.51.100.7", "10.1.2.3"}, "", "198.51.100.7", "127.0.0.1", false},
		{"ipv6 proxy", "[::1]:5000", []string{"2001:db8::5"}, "https", "2001:db8::5", "::1", true},
		{"garbage stops the walk", "127.0.0.1:5000", []string{"198.51.100.7, not-an-ip"}, "", "127.0.0.1", "127.0.0.1", false},
		{"bogus proto ignored", "127.0.0.1:5000", nil, "gopher", "127.0.0.1", "127.0.0.1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got clientInfo
			h := clientIPMiddleware(trusted)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = clientFrom(r) }))
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tt.remote
			for _, v := range tt.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if tt.proto != "" {
				r.Header.Set("X-Forwarded-Proto", tt.proto)
			}
			h.ServeHTTP(httptest.NewRecorder(), r)
			if got.ip != tt.wantIP || got.proxy != tt.wantProxy || got.https != tt.wantHTTPS {
				t.Errorf("got %+v, want ip=%s proxy=%s https=%v", got, tt.wantIP, tt.wantProxy, tt.wantHTTPS)
			}
		})
	}
	if _, err := ParseTrustedProxies([]string{"caddy"}); err == nil {
		t.Error("accepted a host name as a trusted proxy")
	}
}

// Presigned URLs use https only when a trusted proxy says the client did.
func TestPresignSchemeBehindProxy(t *testing.T) {
	base := newTestAPI(t) // only for its engine and keys
	base.must(http.StatusCreated, "PUT", "/v1/buckets/px-b", base.admin, nil)
	base.must(http.StatusOK, "PUT", "/v1/b/px-b/k", base.admin, nil)
	trusted, _ := ParseTrustedProxies([]string{"192.0.2.1"}) // httptest's RemoteAddr
	for _, tt := range []struct {
		trusted bool
		want    string
	}{{false, `"url":"http://files.example.com/`}, {true, `"url":"https://files.example.com/`}} {
		opts := Options{Logger: slog.New(slog.DiscardHandler), MaxObjectSize: 1 << 20,
			Store: base.eng, Buckets: base.eng, Auth: base.auth, Links: base.eng}
		if tt.trusted {
			opts.TrustedProxies = trusted
		}
		r := httptest.NewRequest("POST", "/v1/presign", strings.NewReader(`{"bucket":"px-b","key":"k","method":"GET"}`))
		r.Host = "files.example.com"
		r.Header.Set("Authorization", "Bearer "+base.admin)
		r.Header.Set("X-Forwarded-Proto", "https")
		rr := httptest.NewRecorder()
		New(opts).ServeHTTP(rr, r)
		if !strings.Contains(rr.Body.String(), tt.want) {
			t.Errorf("trusted=%v: %s", tt.trusted, rr.Body)
		}
	}
}
