package main

import (
	"math"
	"slices"
	"testing"
	"time"
)

func TestParseDashboardRange(t *testing.T) {
	cases := []struct {
		raw            string
		window, bucket time.Duration
		points         int
	}{
		{"24h", 24 * time.Hour, 15 * time.Minute, 96},
		{"7d", 7 * 24 * time.Hour, 2 * time.Hour, 84},
		{"30d", 30 * 24 * time.Hour, 8 * time.Hour, 90},
	}
	for _, tc := range cases {
		got, err := parseDashboardRange(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		if got.Window != tc.window || got.Bucket != tc.bucket || got.MaxPoints != tc.points {
			t.Fatalf("%s: %#v", tc.raw, got)
		}
	}
	if _, err := parseDashboardRange("1y"); err == nil {
		t.Fatal("invalid range accepted")
	}
}

func TestNearestRankDoesNotMutateInput(t *testing.T) {
	input := []int64{400, 100, 300, 200}
	got := nearestRank(input, .95)
	if got == nil || *got != 400 {
		t.Fatalf("p95=%v", got)
	}
	if !slices.Equal(input, []int64{400, 100, 300, 200}) {
		t.Fatalf("input mutated: %v", input)
	}
}

func TestPulseState(t *testing.T) {
	if got := pulseState(nil); got != "unknown" {
		t.Fatalf("empty=%s", got)
	}
	if got := pulseState([]dashboardSample{{State: stateHealthy}}); got != "healthy" {
		t.Fatalf("healthy=%s", got)
	}
	if got := pulseState([]dashboardSample{{State: stateHealthy}, {State: stateAPI}}); got != "degraded" {
		t.Fatalf("mixed=%s", got)
	}
	if got := pulseState([]dashboardSample{{State: stateAPI}, {State: stateConnection}}); got != "outage" {
		t.Fatalf("failed=%s", got)
	}
}

func TestAggregateSeriesProducesBoundedChronologicalBuckets(t *testing.T) {
	end := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	spec, _ := parseDashboardRange("24h")
	points := aggregateSeries(syntheticDaySamples(end), spec, end.Add(-spec.Window), end)
	if len(points) > spec.MaxPoints {
		t.Fatalf("points=%d", len(points))
	}
	for i := 1; i < len(points); i++ {
		if points[i].BucketStart <= points[i-1].BucketStart {
			t.Fatal("points not chronological")
		}
	}
}

func TestAggregateSeriesIgnoresPublicSamplesAndSummarizesCompletedChecks(t *testing.T) {
	start := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	end := start.Add(30 * time.Minute)
	spec := dashboardRange{Window: 30 * time.Minute, Bucket: 15 * time.Minute, MaxPoints: 2}
	points := aggregateSeries([]dashboardSample{
		{Source: "public", State: stateHealthy, DurationMS: 1, CheckedAt: start.Add(time.Minute)},
		{Source: "authenticated", State: stateHealthy, DurationMS: 100, CheckedAt: start.Add(2 * time.Minute)},
		{Source: "authenticated", State: stateHealthy, DurationMS: 200, CheckedAt: start.Add(4 * time.Minute)},
		{Source: "authenticated", State: stateAPI, DurationMS: 300, CheckedAt: start.Add(6 * time.Minute)},
		{Source: "authenticated", State: stateAPI, DurationMS: 400, CheckedAt: start.Add(16 * time.Minute)},
		{Source: "authenticated", State: stateConnection, DurationMS: 500, CheckedAt: start.Add(17 * time.Minute)},
	}, spec, start, end)
	if len(points) != 2 {
		t.Fatalf("points=%d", len(points))
	}
	first := points[0]
	if first.State != "degraded" || first.Availability == nil || math.Abs(*first.Availability-200.0/3.0) > .000001 {
		t.Fatalf("first=%#v", first)
	}
	if first.P50MS == nil || *first.P50MS != 200 || first.P95MS == nil || *first.P95MS != 300 {
		t.Fatalf("first latency=%#v", first)
	}
	second := points[1]
	if second.State != "outage" || second.Availability == nil || *second.Availability != 0 {
		t.Fatalf("second=%#v", second)
	}
	if second.P50MS == nil || *second.P50MS != 400 || second.P95MS == nil || *second.P95MS != 500 {
		t.Fatalf("second latency=%#v", second)
	}
}

func TestAggregateSeriesMarksEmptyBucketsUnknownWithoutAvailability(t *testing.T) {
	start := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	spec := dashboardRange{Window: 30 * time.Minute, Bucket: 15 * time.Minute, MaxPoints: 2}
	points := aggregateSeries(nil, spec, start, start.Add(30*time.Minute))
	if len(points) != 2 {
		t.Fatalf("points=%d", len(points))
	}
	for _, point := range points {
		if point.State != "unknown" || point.Availability != nil || point.P50MS != nil || point.P95MS != nil {
			t.Fatalf("point=%#v", point)
		}
	}
}

func syntheticDaySamples(end time.Time) []dashboardSample {
	start := end.Add(-24 * time.Hour)
	return []dashboardSample{
		{Source: "authenticated", State: stateHealthy, DurationMS: 100, CheckedAt: start.Add(time.Minute)},
		{Source: "authenticated", State: stateAPI, DurationMS: 200, CheckedAt: start.Add(2 * time.Hour)},
		{Source: "authenticated", State: stateHealthy, DurationMS: 300, CheckedAt: end.Add(-time.Minute)},
	}
}
