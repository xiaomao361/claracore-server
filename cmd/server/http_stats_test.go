package main

import (
	"net/http"
	"testing"
	"time"
)

func TestHTTPWindowStatsAggregatesRecentBusinessRequests(t *testing.T) {
	stats := newHTTPWindowStats()
	base := time.Date(2026, 8, 29, 8, 10, 0, 0, time.UTC)

	stats.Begin()
	stats.Finish(base, "GET /livez", http.StatusOK, false, 2*time.Millisecond)
	stats.Begin()
	stats.Finish(base.Add(-5*time.Minute), "GET /browse", http.StatusInternalServerError, false, time.Second)

	for _, sample := range []struct {
		at       time.Time
		status   int
		panicked bool
		latency  time.Duration
	}{
		{base.Add(-4 * time.Minute), http.StatusOK, false, 8 * time.Millisecond},
		{base.Add(-2 * time.Minute), http.StatusNotFound, false, 42 * time.Millisecond},
		{base, http.StatusBadGateway, false, 220 * time.Millisecond},
		{base, http.StatusInternalServerError, true, 2 * time.Second},
	} {
		stats.Begin()
		stats.Finish(sample.at, "GET /browse", sample.status, sample.panicked, sample.latency)
	}
	stats.Begin()

	snapshot := stats.Snapshot(base)
	if snapshot.Window != 5*time.Minute || snapshot.Requests != 4 || snapshot.ClientErrors != 1 ||
		snapshot.ServerErrors != 2 || snapshot.Panics != 1 || snapshot.InFlight != 1 {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	if snapshot.AverageLatency != (8*time.Millisecond+42*time.Millisecond+220*time.Millisecond+2*time.Second)/4 {
		t.Fatalf("average latency = %s", snapshot.AverageLatency)
	}
	if snapshot.P95Latency != 2500*time.Millisecond || snapshot.P95Overflow {
		t.Fatalf("p95 = %s overflow=%v", snapshot.P95Latency, snapshot.P95Overflow)
	}
}

func TestHTTPWindowStatsRotatesBucketsAndBoundsLatency(t *testing.T) {
	stats := newHTTPWindowStats()
	base := time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)
	stats.Begin()
	stats.Finish(base, "POST /mcp", http.StatusOK, false, 45*time.Minute)

	initial := stats.Snapshot(base)
	if initial.Requests != 1 || initial.AverageLatency != maxTrackedHTTPLatency || !initial.P95Overflow {
		t.Fatalf("bounded initial snapshot: %+v", initial)
	}
	rotated := stats.Snapshot(base.Add(5 * time.Minute))
	if rotated.Requests != 0 || rotated.ServerErrors != 0 || rotated.Panics != 0 {
		t.Fatalf("expired bucket remained visible: %+v", rotated)
	}
}
