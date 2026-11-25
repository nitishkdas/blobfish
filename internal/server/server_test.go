package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

var ulidRE = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

func newTestHandler(t *testing.T, maxBody int64, extra map[string]http.HandlerFunc) (http.Handler, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	h := New(Options{Logger: log, MaxObjectSize: maxBody})
	if len(extra) == 0 {
		return h, &logs
	}
	// Route extra test-only handlers through the same middleware chain.
	mux := http.NewServeMux()
	for pattern, fn := range extra {
		mux.HandleFunc(pattern, fn)
	}
	return chain(mux,
		requestIDMiddleware,
		logMiddleware(log),
		recoverMiddleware(log),
		maxBodyMiddleware(maxBody),
	), &logs
}

func decodeError(t *testing.T, body io.Reader) errorDetail {
	t.Helper()
	var b errorBody
	if err := json.NewDecoder(body).Decode(&b); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return b.Error
}

func TestRoutes(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantCode   string // API error code; empty for success
	}{
		{"healthz", http.MethodGet, "/healthz", http.StatusOK, ""},
		{"healthz head", http.MethodHead, "/healthz", http.StatusOK, ""},
		{"unknown path", http.MethodGet, "/nope", http.StatusNotFound, CodeNotFound},
		{"healthz wrong method", http.MethodPost, "/healthz", http.StatusNotFound, CodeNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _ := newTestHandler(t, 1024, nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(tt.method, tt.path, nil))

			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rr.Code, tt.wantStatus)
			}
			if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			reqID := rr.Header().Get("X-Request-Id")
			if !ulidRE.MatchString(reqID) {
				t.Errorf("X-Request-Id = %q, want a ULID", reqID)
			}
			if tt.method == http.MethodHead {
				return
			}
			if tt.wantCode == "" {
				if got := strings.TrimSpace(rr.Body.String()); got != `{"status":"ok"}` {
					t.Errorf("body = %s", got)
				}
				return
			}
			e := decodeError(t, rr.Body)
			if e.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", e.Code, tt.wantCode)
			}
			if e.RequestID != reqID {
				t.Errorf("body requestId = %q, header = %q", e.RequestID, reqID)
			}
		})
	}
}

func TestMaxBody(t *testing.T) {
	const limit = 10
	// The handler reads the whole body (small, test-only) and reports whether
	// the limit was hit, the way real upload handlers will.
	echo := func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, r, http.StatusRequestEntityTooLarge, CodeEntityTooLarge, "too large")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}

	tests := []struct {
		name       string
		body       string
		hideLength bool // send as chunked so only the reader limit applies
		wantStatus int
	}{
		{"under limit", "0123456789", false, http.StatusNoContent},
		{"over limit declared", "0123456789X", false, http.StatusRequestEntityTooLarge},
		{"over limit streamed", "0123456789X", true, http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _ := newTestHandler(t, limit, map[string]http.HandlerFunc{"PUT /up": echo})
			req := httptest.NewRequest(http.MethodPut, "/up", strings.NewReader(tt.body))
			if tt.hideLength {
				req.ContentLength = -1
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rr.Code, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusRequestEntityTooLarge {
				if e := decodeError(t, rr.Body); e.Code != CodeEntityTooLarge {
					t.Errorf("code = %q, want %q", e.Code, CodeEntityTooLarge)
				}
			}
		})
	}
}

func TestRecover(t *testing.T) {
	h, logs := newTestHandler(t, 1024, map[string]http.HandlerFunc{
		"GET /boom": func(http.ResponseWriter, *http.Request) { panic("kaboom") },
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if e := decodeError(t, rr.Body); e.Code != CodeInternalError {
		t.Errorf("code = %q, want %q", e.Code, CodeInternalError)
	}
	if !strings.Contains(logs.String(), "kaboom") {
		t.Errorf("panic not logged; logs:\n%s", logs)
	}
	// The access log must still record the request with its final status.
	if !strings.Contains(logs.String(), `"status":500`) {
		t.Errorf("access log missing status 500; logs:\n%s", logs)
	}
}

func TestRecoverReRaisesAbortHandler(t *testing.T) {
	h, _ := newTestHandler(t, 1024, map[string]http.HandlerFunc{
		"GET /abort": func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) },
	})
	defer func() {
		if v := recover(); v != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler", v)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/abort", nil))
}

func TestNewRequestID(t *testing.T) {
	t0 := time.UnixMilli(1_700_000_000_000)
	a, b := newRequestID(t0), newRequestID(t0.Add(time.Millisecond))
	for _, id := range []string{a, b} {
		if !ulidRE.MatchString(id) {
			t.Fatalf("id %q is not a ULID", id)
		}
	}
	if a >= b {
		t.Errorf("ids not time-ordered: %s >= %s", a, b)
	}
	if a[:10] == "0000000000" {
		t.Errorf("timestamp part is empty: %s", a)
	}
	if c, d := newRequestID(t0), newRequestID(t0); c == d {
		t.Error("two ids in the same millisecond collided")
	}
}
