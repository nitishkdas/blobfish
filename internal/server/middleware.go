package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// requestIDMiddleware gives every request a fresh ID, stores it in the
// context and echoes it in the X-Request-Id response header. Client-supplied
// IDs are ignored so log lines can always be trusted.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID(time.Now())
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(withRequestID(r.Context(), id)))
	})
}

// maxBodyMiddleware caps request bodies at limit bytes. Reads past the limit
// fail with *http.MaxBytesError, which handlers map to EntityTooLarge.
// Requests that declare a larger Content-Length are rejected up front.
func maxBodyMiddleware(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > limit {
				writeError(w, r, http.StatusRequestEntityTooLarge, CodeEntityTooLarge,
					fmt.Sprintf("request body exceeds the %d byte limit", limit))
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

// recoverMiddleware turns a handler panic into a logged 500 instead of a
// dropped connection. http.ErrAbortHandler is re-raised, as net/http expects.
func recoverMiddleware(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &statusRecorder{ResponseWriter: w}
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(v)
				}
				log.ErrorContext(r.Context(), "panic in handler",
					"request_id", RequestID(r.Context()),
					"panic", fmt.Sprint(v),
					"stack", string(debug.Stack()))
				if !rec.wroteHeader {
					writeError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
				}
			}()
			next.ServeHTTP(rec, r)
		})
	}
}

// logMiddleware writes one access-log line per request.
func logMiddleware(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			log.InfoContext(r.Context(), "request",
				"request_id", RequestID(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"bytes", rec.bytes,
				"duration", time.Since(start),
				"remote", r.RemoteAddr)
		})
	}
}

// statusRecorder captures the status code and body size a handler writes.
// Unwrap lets http.ResponseController reach the underlying writer (Flush,
// deadlines), which large object downloads will need.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	if !s.wroteHeader {
		s.WriteHeader(http.StatusOK)
	}
	n, err := s.ResponseWriter.Write(p)
	s.bytes += int64(n)
	return n, err
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
