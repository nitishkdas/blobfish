package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
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
				writeError(w, r, http.StatusRequestEntityTooLarge, CodeEntityTooLarge, tooLargeMessage(limit))
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
					writeError(w, r, http.StatusInternalServerError, CodeInternalError, internalErrorMessage)
				}
			}()
			next.ServeHTTP(rec, r)
		})
	}
}

// logFields carries values from deep in a handler back to the access log.
type logFields struct {
	errCode string // API error code, set by writeError
	actor   string // who made the request, set by authentication
}

// setActor records who made the request for the access log: an API key's
// name and ID, "presigned", "link:<id>" or "public". Never a secret.
func setActor(r *http.Request, actor string) {
	if f := logFieldsFrom(r); f != nil {
		f.actor = actor
	}
}

func logFieldsFrom(r *http.Request) *logFields {
	f, _ := r.Context().Value(logFieldsKey).(*logFields)
	return f
}

// logMiddleware writes one access-log line per request. Failed requests
// also log their API error code, so "status=400" says which 400.
func logMiddleware(log *slog.Logger, stats *serverStats) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			fields := &logFields{}
			body := &countingBody{ReadCloser: r.Body}
			r.Body = body
			next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), logFieldsKey, fields)))
			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			attrs := []any{
				"request_id", RequestID(r.Context()),
				"method", r.Method,
				"path", logPath(r.URL.Path),
				"status", status,
			}
			if fields.errCode != "" {
				attrs = append(attrs, "code", fields.errCode)
			}
			if fields.actor != "" {
				attrs = append(attrs, "key", fields.actor)
			}
			client := clientFrom(r)
			attrs = append(attrs,
				"bytes", rec.bytes,
				"duration", time.Since(start),
				"remote", client.ip)
			if client.proxy != "" {
				attrs = append(attrs, "proxy", client.proxy)
			}
			log.InfoContext(r.Context(), "request", attrs...)
			if stats != nil {
				stats.record(status, body.n, rec.bytes)
			}
		})
	}
}

// logPath is the request path as the access log shows it. Share and upload
// link paths carry their token, which is the credential itself: it is
// replaced (the "key" field names the link by its ID instead).
func logPath(p string) string {
	for _, prefix := range []string{"/s/", "/u/"} {
		rest, ok := strings.CutPrefix(p, prefix)
		if !ok || rest == "" {
			continue
		}
		_, tail, more := strings.Cut(rest, "/")
		if more {
			return prefix + "<token>/" + tail
		}
		return prefix + "<token>"
	}
	return p
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
