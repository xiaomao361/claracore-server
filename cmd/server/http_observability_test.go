package main

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestObserveHTTPEmitsBoundedCompletionRecord(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	times := []time.Time{time.Unix(100, 0), time.Unix(100, 37*int64(time.Millisecond))}
	now := func() time.Time {
		value := times[0]
		times = times[1:]
		return value
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "ok")
	})
	handler := observeHTTP(mux, logger, func() string { return "request-safe-1" }, now, nil)

	req := httptest.NewRequest(http.MethodPost, "/tokens?token=query-secret&password=hidden", strings.NewReader("body-secret"))
	req.Header.Set("Authorization", "Bearer header-secret")
	req.Header.Set("Cookie", "ccs_session=cookie-secret")
	req.Header.Set(requestIDHeader, "attacker-controlled-id")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated || rec.Header().Get(requestIDHeader) != "request-safe-1" {
		t.Fatalf("response = status %d request-id %q", rec.Code, rec.Header().Get(requestIDHeader))
	}
	line := logs.String()
	for _, want := range []string{
		"request_id=request-safe-1", "method=POST", `route="POST /tokens"`,
		"status=201", "response_bytes=2", "duration_ms=37",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("completion log missing %q: %s", want, line)
		}
	}
	for _, secret := range []string{"query-secret", "hidden", "body-secret", "header-secret", "cookie-secret", "attacker-controlled-id"} {
		if strings.Contains(line, secret) {
			t.Errorf("completion log leaked %q: %s", secret, line)
		}
	}
}

func TestObserveHTTPRecoversUncommittedPanicWithoutLeakingValue(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	handler := observeHTTP(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("panic-secret-value")
	}), logger, func() string { return "request-panic-1" }, time.Now, nil)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/unexpected", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "panic") || strings.Contains(body, "secret") {
		t.Fatalf("panic response leaked details: %q", body)
	}
	line := logs.String()
	for _, want := range []string{"request_id=request-panic-1", "status=500", "panic=true", "stack="} {
		if !strings.Contains(line, want) {
			t.Errorf("panic log missing %q: %s", want, line)
		}
	}
	if strings.Contains(line, "panic-secret-value") {
		t.Fatalf("panic value leaked into log: %s", line)
	}
}

func TestObserveHTTPAbortsConnectionAfterCommittedPanic(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := observeHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		panic("must-not-be-reported")
	}), logger, func() string { return "request-panic-2" }, time.Now, nil)

	defer func() {
		if recovered := recover(); !errors.Is(asError(recovered), http.ErrAbortHandler) {
			t.Fatalf("panic = %v, want http.ErrAbortHandler", recovered)
		}
	}()
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/committed", nil))
}

func TestObserveHTTPPreservesFlushing(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := observeHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("observed writer no longer implements http.Flusher")
		}
		flusher.Flush()
	}), logger, func() string { return "request-stream-1" }, time.Now, nil)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if !rec.Flushed || rec.Code != http.StatusOK {
		t.Fatalf("flushed=%v status=%d", rec.Flushed, rec.Code)
	}
}

func asError(value any) error {
	err, _ := value.(error)
	return err
}
