package main

import (
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

const requestIDHeader = "X-Request-ID"

// observedResponseWriter records only response metadata. Unwrap and Flush keep
// the MCP SDK's streaming behavior available through the middleware.
type observedResponseWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (w *observedResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *observedResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *observedResponseWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(body)
	w.bytes += int64(n)
	return n, err
}

func (w *observedResponseWriter) ReadFrom(src io.Reader) (int64, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := io.Copy(w.ResponseWriter, src)
	w.bytes += n
	return n, err
}

func (w *observedResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *observedResponseWriter) finalStatus() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// observeHTTP gives every request an internal correlation ID, emits one
// structured completion record, and contains panics before response headers
// are committed. It deliberately never logs URL queries, headers, cookies,
// request bodies, principals, or panic values: all can contain credentials or
// team knowledge.
func observeHTTP(next http.Handler, logger *slog.Logger, newRequestID func() string, now func() time.Time, stats *httpWindowStats) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := newRequestID()
		w.Header().Set(requestIDHeader, requestID)
		observed := &observedResponseWriter{ResponseWriter: w}
		startedAt := now()
		stats.Begin()

		defer func() {
			recovered := recover()
			panicked := recovered != nil
			committed := observed.wroteHeader
			if panicked && !committed && recovered != http.ErrAbortHandler {
				observed.Header().Set("Cache-Control", "no-store")
				http.Error(observed, "服务暂时不可用，请稍后重试", http.StatusInternalServerError)
			}

			finishedAt := now()
			elapsed := finishedAt.Sub(startedAt)
			if elapsed < 0 {
				elapsed = 0
			}
			route := r.Pattern
			if route == "" {
				route = "unmatched_or_pre_routing"
			}
			status := observed.finalStatus()
			stats.Finish(finishedAt, route, status, panicked, elapsed)
			level := requestLogLevel(route, status, panicked)
			attrs := []slog.Attr{
				slog.String("request_id", requestID),
				slog.String("method", r.Method),
				slog.String("route", route),
				slog.Int("status", status),
				slog.Int64("response_bytes", observed.bytes),
				slog.Int64("duration_ms", elapsed.Milliseconds()),
			}
			if panicked {
				attrs = append(attrs, slog.Bool("panic", true), slog.String("stack", string(debug.Stack())))
			}
			logger.LogAttrs(r.Context(), level, "HTTP 请求完成", attrs...)

			if panicked && (committed || recovered == http.ErrAbortHandler) {
				// Ask net/http to close the connection without its default panic
				// message, which would include an arbitrary panic value.
				panic(http.ErrAbortHandler)
			}
		}()

		next.ServeHTTP(observed, r)
	})
}

func requestLogLevel(route string, status int, panicked bool) slog.Level {
	if panicked || status >= http.StatusInternalServerError {
		return slog.LevelError
	}
	if status == http.StatusTooManyRequests {
		return slog.LevelWarn
	}
	if route == "GET /livez" || route == "GET /readyz" || route == "GET /healthz" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}
