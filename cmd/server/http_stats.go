package main

import (
	"sync"
	"sync/atomic"
	"time"

	"claracore-server/internal/web"
)

const (
	httpStatsWindowMinutes = 5
	maxTrackedHTTPLatency  = 30 * time.Minute
)

var httpLatencyBounds = [...]time.Duration{
	10 * time.Millisecond,
	50 * time.Millisecond,
	100 * time.Millisecond,
	250 * time.Millisecond,
	500 * time.Millisecond,
	time.Second,
	2500 * time.Millisecond,
	5 * time.Second,
	30 * time.Second,
}

type httpStatsBucket struct {
	minute       int64
	requests     uint64
	clientErrors uint64
	serverErrors uint64
	panics       uint64
	duration     time.Duration
	latencies    [len(httpLatencyBounds) + 1]uint64
}

// httpWindowStats keeps a constant-memory, privacy-safe view of recent HTTP
// health. It stores no routes, request IDs, identities, or content.
type httpWindowStats struct {
	mu       sync.Mutex
	buckets  [httpStatsWindowMinutes]httpStatsBucket
	inFlight atomic.Int64
}

func newHTTPWindowStats() *httpWindowStats { return &httpWindowStats{} }

func (s *httpWindowStats) Begin() {
	if s != nil {
		s.inFlight.Add(1)
	}
}

func (s *httpWindowStats) Finish(at time.Time, route string, status int, panicked bool, duration time.Duration) {
	if s == nil {
		return
	}
	s.inFlight.Add(-1)
	if isProbeRoute(route) {
		return
	}
	if duration < 0 {
		duration = 0
	} else if duration > maxTrackedHTTPLatency {
		duration = maxTrackedHTTPLatency
	}
	minute := at.Unix() / 60
	index := minute % httpStatsWindowMinutes
	if index < 0 {
		index += httpStatsWindowMinutes
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	bucket := &s.buckets[index]
	if bucket.minute != minute {
		*bucket = httpStatsBucket{minute: minute}
	}
	bucket.requests++
	bucket.duration += duration
	if status >= 400 && status < 500 {
		bucket.clientErrors++
	}
	if status >= 500 {
		bucket.serverErrors++
	}
	if panicked {
		bucket.panics++
	}
	bucket.latencies[latencyBucket(duration)]++
}

func (s *httpWindowStats) Snapshot(at time.Time) web.HTTPRequestStats {
	out := web.HTTPRequestStats{
		Available: true,
		Window:    httpStatsWindowMinutes * time.Minute,
		InFlight:  s.inFlight.Load(),
	}
	minute := at.Unix() / 60
	var duration time.Duration
	var latencies [len(httpLatencyBounds) + 1]uint64

	s.mu.Lock()
	for i := range s.buckets {
		bucket := s.buckets[i]
		age := minute - bucket.minute
		if age < 0 || age >= httpStatsWindowMinutes {
			continue
		}
		out.Requests += bucket.requests
		out.ClientErrors += bucket.clientErrors
		out.ServerErrors += bucket.serverErrors
		out.Panics += bucket.panics
		duration += bucket.duration
		for j, count := range bucket.latencies {
			latencies[j] += count
		}
	}
	s.mu.Unlock()

	if out.Requests == 0 {
		return out
	}
	out.AverageLatency = duration / time.Duration(out.Requests)
	rank := (out.Requests*95 + 99) / 100
	var cumulative uint64
	for index, count := range latencies {
		cumulative += count
		if cumulative < rank {
			continue
		}
		if index == len(httpLatencyBounds) {
			out.P95Latency = httpLatencyBounds[len(httpLatencyBounds)-1]
			out.P95Overflow = true
		} else {
			out.P95Latency = httpLatencyBounds[index]
		}
		break
	}
	return out
}

func latencyBucket(duration time.Duration) int {
	for index, bound := range httpLatencyBounds {
		if duration <= bound {
			return index
		}
	}
	return len(httpLatencyBounds)
}

func isProbeRoute(route string) bool {
	switch route {
	case "GET /livez", "GET /readyz", "GET /healthz":
		return true
	default:
		return false
	}
}
