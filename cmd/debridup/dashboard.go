package main

import (
	"fmt"
	"math"
	"sort"
	"time"
)

type dashboardRange struct {
	Window    time.Duration `json:"window"`
	Bucket    time.Duration `json:"bucket"`
	MaxPoints int           `json:"maxPoints"`
}

type dashboardSample struct {
	Source     string    `json:"source"`
	State      string    `json:"state"`
	DurationMS int64     `json:"durationMs"`
	CheckedAt  time.Time `json:"checkedAt"`
}

type dashboardResponse struct {
	GeneratedAt int64               `json:"generatedAt"`
	Range       string              `json:"range"`
	Summary     dashboardSummary    `json:"summary"`
	Providers   []dashboardProvider `json:"providers"`
	Incidents   []dashboardIncident `json:"incidents"`
}

type dashboardSummary struct {
	OverallState    string `json:"overallState"`
	ProvidersOnline int    `json:"providersOnline"`
	ActiveIncidents int    `json:"activeIncidents"`
	ChecksToday     int    `json:"checksToday"`
}

type dashboardProvider struct {
	ID           int64            `json:"id"`
	Name         string           `json:"name"`
	Provider     string           `json:"provider"`
	State        string           `json:"state"`
	StateSince   *int64           `json:"stateSince"`
	LastCheck    *int64           `json:"lastCheck"`
	Availability *float64         `json:"availability"`
	P50MS        *int64           `json:"p50Ms"`
	P95MS        *int64           `json:"p95Ms"`
	SlowestMS    *int64           `json:"slowestMs"`
	Series       []dashboardPoint `json:"series"`
}

type dashboardPoint struct {
	BucketStart  int64    `json:"bucketStart"`
	State        string   `json:"state"`
	Availability *float64 `json:"availability"`
	P50MS        *int64   `json:"p50Ms"`
	P95MS        *int64   `json:"p95Ms"`
}

type dashboardIncident struct {
	ID           int64  `json:"id"`
	MonitorID    int64  `json:"monitorId"`
	Name         string `json:"name"`
	Provider     string `json:"provider"`
	OpenedAt     int64  `json:"openedAt"`
	DetectedAt   int64  `json:"detectedAt"`
	ResolvedAt   *int64 `json:"resolvedAt"`
	InitialState string `json:"initialState"`
	LatestState  string `json:"latestState"`
	Summary      string `json:"summary"`
}

func parseDashboardRange(raw string) (dashboardRange, error) {
	switch raw {
	case "24h":
		return dashboardRange{Window: 24 * time.Hour, Bucket: 15 * time.Minute, MaxPoints: 96}, nil
	case "7d":
		return dashboardRange{Window: 7 * 24 * time.Hour, Bucket: 2 * time.Hour, MaxPoints: 84}, nil
	case "30d":
		return dashboardRange{Window: 30 * 24 * time.Hour, Bucket: 8 * time.Hour, MaxPoints: 90}, nil
	default:
		return dashboardRange{}, fmt.Errorf("unsupported dashboard range %q", raw)
	}
}

func nearestRank(values []int64, percentile float64) *int64 {
	if len(values) == 0 || percentile <= 0 || percentile > 1 {
		return nil
	}
	ordered := append([]int64(nil), values...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	index := int(math.Ceil(percentile*float64(len(ordered)))) - 1
	return &ordered[index]
}

func pulseState(samples []dashboardSample) string {
	if len(samples) == 0 {
		return "unknown"
	}
	healthy := 0
	for _, sample := range samples {
		if sample.State == stateHealthy {
			healthy++
		}
	}
	switch {
	case healthy == len(samples):
		return "healthy"
	case healthy == 0:
		return "outage"
	default:
		return "degraded"
	}
}

func aggregateSeries(samples []dashboardSample, spec dashboardRange, start, end time.Time) []dashboardPoint {
	if spec.Bucket <= 0 || spec.MaxPoints <= 0 || !end.After(start) {
		return []dashboardPoint{}
	}
	bucketCount := int((end.Sub(start) + spec.Bucket - 1) / spec.Bucket)
	if bucketCount > spec.MaxPoints {
		bucketCount = spec.MaxPoints
	}
	buckets := make([][]dashboardSample, bucketCount)
	for _, sample := range samples {
		if sample.Source != "authenticated" || sample.CheckedAt.Before(start) || !sample.CheckedAt.Before(end) {
			continue
		}
		index := int(sample.CheckedAt.Sub(start) / spec.Bucket)
		if index >= 0 && index < bucketCount {
			buckets[index] = append(buckets[index], sample)
		}
	}

	points := make([]dashboardPoint, 0, bucketCount)
	for index, bucket := range buckets {
		point := dashboardPoint{
			BucketStart: start.Add(time.Duration(index) * spec.Bucket).Unix(),
			State:       pulseState(bucket),
		}
		if len(bucket) == 0 {
			points = append(points, point)
			continue
		}

		healthy := 0
		latencies := make([]int64, 0, len(bucket))
		for _, sample := range bucket {
			if sample.State == stateHealthy {
				healthy++
			}
			latencies = append(latencies, sample.DurationMS)
		}
		availability := float64(healthy) / float64(len(bucket)) * 100
		point.Availability = &availability
		point.P50MS = nearestRank(latencies, .50)
		point.P95MS = nearestRank(latencies, .95)
		points = append(points, point)
	}
	return points
}
