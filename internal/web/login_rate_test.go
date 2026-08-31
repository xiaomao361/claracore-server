package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"claracore-server/internal/ports"
)

type scriptedRateLimiter struct {
	results []ports.RateLimitResult
	err     error
	keys    []string
	limits  []int
}

func (s *scriptedRateLimiter) Allow(_ context.Context, key string, limit int, _ time.Duration) (ports.RateLimitResult, error) {
	s.keys = append(s.keys, key)
	s.limits = append(s.limits, limit)
	if s.err != nil {
		return ports.RateLimitResult{}, s.err
	}
	result := ports.RateLimitResult{Allowed: true}
	if len(s.results) >= len(s.keys) {
		result = s.results[len(s.keys)-1]
	}
	return result, nil
}

func TestLoginRateLimitBlocksBeforePasswordHashWithoutLeakingIdentity(t *testing.T) {
	gov := newFakeGov("admin", false)
	limiter := &scriptedRateLimiter{results: []ports.RateLimitResult{
		{Allowed: true},
		{Allowed: false, RetryAfter: 75 * time.Second},
	}}
	host := NewHost(gov, &fakeKnow{}, newFakeContinuity(), &fakeActivity{}, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
		Version: "test", LoginRateLimiter: limiter, LoginRateLimit: 10, LoginRateWindow: 15 * time.Minute,
	})
	form := url.Values{"username": {"maozai"}, "password": {"wrong-password"}}
	req := httptest.NewRequest(http.MethodPost, "http://ccs.example.test/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Real-IP", "198.51.100.77") // direct peer: this spoof must be ignored
	req.RemoteAddr = "203.0.113.9:54321"
	rec := httptest.NewRecorder()
	host.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "75" {
		t.Fatalf("blocked login should return 429 + Retry-After: status=%d headers=%v", rec.Code, rec.Header())
	}
	if gov.calls != 0 {
		t.Fatalf("blocked attempt must not execute password hashing, Login calls=%d", gov.calls)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "登录尝试过多，请稍后再试。") || strings.Contains(body, "maozai") || strings.Contains(body, "203.0.113.9") {
		t.Fatalf("429 copy should guide without leaking account/address: %s", body)
	}
	if len(limiter.keys) != 2 || limiter.limits[0] != 10 || limiter.limits[1] != 60 {
		t.Fatalf("must count pair and IP dimensions: keys=%v limits=%v", limiter.keys, limiter.limits)
	}
	for _, key := range limiter.keys {
		if strings.Contains(key, "maozai") || strings.Contains(key, "203.0.113.9") || strings.Contains(key, "198.51.100.77") {
			t.Fatalf("persistent rate key must be privacy-safe: %s", key)
		}
	}
}

func TestLoginClientIPTrustsForwardedAddressOnlyFromLoopbackProxy(t *testing.T) {
	proxied := httptest.NewRequest(http.MethodPost, "http://ccs.test/login", nil)
	proxied.RemoteAddr = "127.0.0.1:52100"
	proxied.Header.Set("X-Real-IP", "198.51.100.10")
	if got := loginClientIP(proxied); got != "198.51.100.10" {
		t.Fatalf("loopback nginx should supply client address, got %q", got)
	}

	direct := httptest.NewRequest(http.MethodPost, "http://ccs.test/login", nil)
	direct.RemoteAddr = "203.0.113.11:52100"
	direct.Header.Set("X-Real-IP", "198.51.100.10")
	if got := loginClientIP(direct); got != "203.0.113.11" {
		t.Fatalf("direct peer must not spoof X-Real-IP, got %q", got)
	}
}

func TestLoginLimiterFailurePreservesAvailability(t *testing.T) {
	gov := newFakeGov("admin", false)
	limiter := &scriptedRateLimiter{err: errors.New("all limiter layers unavailable")}
	host := NewHost(gov, &fakeKnow{}, newFakeContinuity(), &fakeActivity{}, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
		Version: "test", LoginRateLimiter: limiter, LoginRateLimit: 10, LoginRateWindow: 15 * time.Minute,
	})
	form := url.Values{"username": {"maozai"}, "password": {"correct-password"}}
	req := httptest.NewRequest(http.MethodPost, "http://ccs.test/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	host.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || gov.calls != 1 {
		t.Fatalf("limiter infrastructure failure should fail open after logging: status=%d loginCalls=%d", rec.Code, gov.calls)
	}
}

var _ ports.RateLimiter = (*scriptedRateLimiter)(nil)
