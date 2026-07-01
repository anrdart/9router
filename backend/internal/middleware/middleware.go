// Package middleware contains HTTP middleware used by the Go backend.
package middleware

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// Key type for request-scoped values.
type ctxKey string

// RequestIDKey is the context key carrying the per-request id.
const RequestIDKey ctxKey = "request_id"

// Header names that must NEVER be logged because they carry secrets. Lookup is
// case-insensitive (keys stored lowercase) so it is robust to whatever casing a
// client sends — including the non-obvious canonicalization of names like
// "X-9R-Cli-Token" (net/http maps that to "X-9r-Cli-Token").
var redactedHeaders = map[string]struct{}{
	"authorization":       {},
	"x-api-key":           {},
	"x-9r-api-key":        {},
	"x-9r-cli-token":      {},
	"proxy-authorization": {},
	"cookie":              {},
}

// IsRedacted reports whether a header name is sensitive and must never be logged.
func IsRedacted(name string) bool {
	_, ok := redactedHeaders[strings.ToLower(name)]
	return ok
}

// newRequestID returns a short random hex id.
func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// RequestID injects a unique id into the request context and the X-Request-ID
// response header. If the client supplied X-Request-ID it is preserved.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), RequestIDKey, id)))
	})
}

// statusRecorder captures the status code written by downstream handlers.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wroteHeader {
		return
	}
	s.status = code
	s.wroteHeader = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.status = http.StatusOK
		s.wroteHeader = true
	}
	return s.ResponseWriter.Write(b)
}

// Hijack delegates to the underlying ResponseWriter so protocol upgrades
// (WebSocket, HTTP/2 prior-knowledge, etc.) work through the logging wrapper.
// Without this, httputil.ReverseProxy fails WebSocket handshakes with
// "can't switch protocols using non-Hijacker ResponseWriter".
func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("inner ResponseWriter of type %T is not a Hijacker", s.ResponseWriter)
	}
	return h.Hijack()
}

// Flush delegates to the underlying Flusher so chunked/SSE responses flush
// immediately rather than being buffered by the recorder.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying ResponseWriter so middleware/handlers that need
// to assert a concrete type (e.g. http.Hijacker) can reach it. Go 1.20+ uses
// this for ResponseUnwrapError-based interface resolution.
func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

// Logging records one structured log line per request: method, path, status,
// duration, and request id. Sensitive headers are never logged.
func Logging(logger *slog.Logger, includePathPrefix string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: 0}
			next.ServeHTTP(rec, r)
			id, _ := r.Context().Value(RequestIDKey).(string)

			logger.LogAttrs(r.Context(), slog.LevelInfo, "request",
				slog.String("id", id),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Duration("duration", time.Since(start)),
				slog.String("remote", r.RemoteAddr),
			)
		})
	}
}

// Recover catches panics from downstream handlers, logs a stack trace, and
// returns 500 instead of crashing the process.
func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.Error("panic recovered",
						slog.Any("panic", rec),
						slog.String("stack", string(debug.Stack())),
						slog.String("path", r.URL.Path),
					)
					if !headersWritten(w) {
						http.Error(w, "internal server error", http.StatusInternalServerError)
					}
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// headersWritten is a best-effort check; in practice our statusRecorder tracks it,
// but for handlers that bypass the recorder we treat the writer as not-yet-written.
func headersWritten(_ http.ResponseWriter) bool { return false }

// LimitBody wraps a handler to reject request bodies larger than maxBytes BEFORE
// the downstream handler reads them.
func LimitBody(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Chain composes middleware left-to-right (first listed runs outermost).
func Chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// ErrServerClosed is returned by http.Server.Serve after Shutdown; treat as benign.
var ErrServerClosed = errors.New("http: Server closed")
