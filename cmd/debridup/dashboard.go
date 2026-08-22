package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"time"
)

type dashboardRange struct {
	Window    time.Duration `json:"window"`
	Bucket    time.Duration `json:"bucket"`
	MaxPoints int           `json:"maxPoints"`
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
	Transient    bool   `json:"transient,omitempty"`
}

type apiError struct {
	Code  string `json:"code"`
	Error string `json:"error"`
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

// dashboardWindow returns the bucket-aligned window for a range. Buckets are
// anchored to the epoch so their contents are stable between refreshes, which
// is what makes them storable. The window is exactly MaxPoints buckets ending
// with the one currently being filled.
func dashboardWindow(spec dashboardRange, now time.Time) (firstBucket, lastBucket, width int64) {
	width = int64(spec.Bucket / time.Second)
	if width <= 0 || spec.MaxPoints <= 0 {
		return 0, 0, width
	}
	// now-1 rather than now: on an exact bucket boundary the bucket starting at
	// now holds no checks yet, so the window would waste a slot and emit a
	// bucket start equal to the snapshot time.
	lastBucket = bucketStartFor(now.Unix()-1, width)
	firstBucket = lastBucket - int64(spec.MaxPoints-1)*width
	return firstBucket, lastBucket, width
}

// dashboardDisplayState shows a provider whose latest raw check failed as
// degraded, even before the failure threshold confirms a state change.
func dashboardDisplayState(current, lastRaw string) string {
	if current == stateHealthy && lastRaw != "" && lastRaw != stateHealthy {
		return stateDegraded
	}
	return current
}

func transientDegradationSummary(durationMS int64, timeoutSeconds int) string {
	duration := fmt.Sprintf("%.1fs", float64(durationMS)/1000)
	if timeoutSeconds > 0 {
		return fmt.Sprintf("Authenticated check took %s and exceeded the %ds timeout. Possible degraded service; no notification was sent because the failure threshold has not been reached.", duration, timeoutSeconds)
	}
	return fmt.Sprintf("Authenticated check timed out after %s. Possible degraded service; no notification was sent because the failure threshold has not been reached.", duration)
}

// rollupPoint is one stored bucket.
type rollupPoint struct {
	BucketStart int64
	Total       int64
	Healthy     int64
	SlowestMS   int64
	P50MS       *int64
	P95MS       *int64
}

func (a *app) dashboardSnapshot(ctx context.Context, spec dashboardRange, now time.Time) (dashboardResponse, error) {
	now = now.UTC()
	firstBucket, lastBucket, width := dashboardWindow(spec, now)
	windowStart := time.Unix(firstBucket, 0).UTC()
	response := dashboardResponse{
		GeneratedAt: now.Unix(),
		Range:       dashboardRangeLabel(spec),
		Providers:   make([]dashboardProvider, 0),
		Incidents:   make([]dashboardIncident, 0),
		Summary: dashboardSummary{
			OverallState: stateHealthy,
		},
	}

	type providerState struct {
		provider       dashboardProvider
		enabled        bool
		lastRawState   string
		failureStarted *int64
		timeoutSeconds int
		// The most recent authenticated check in the window, used to surface a
		// transient timeout that has not yet crossed the failure threshold.
		// The rollups summarise buckets, so the individual latest check is
		// fetched alongside the provider row rather than from a sample slice.
		latestErrorCode  string
		latestDurationMS int64
		latestCheckedAt  int64
	}
	providers := make([]providerState, 0)
	rollupsByMonitor := make(map[int64][]rollupPoint)
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	err := a.withReadOnlyDashboardTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT m.id,m.name,m.provider,m.enabled,m.timeout_seconds,
		s.current_state,s.state_since,s.last_raw_state,s.failure_started_at,s.last_check_at,
		COALESCE(l.error_code,''),COALESCE(l.duration_ms,0),COALESCE(l.checked_at,0)
		FROM monitors m
		LEFT JOIN monitor_states s ON s.monitor_id=m.id
		LEFT JOIN check_results l ON l.id = (
		  SELECT id FROM check_results
		  WHERE monitor_id=m.id AND source='authenticated' AND checked_at>=? AND checked_at<?
		  ORDER BY checked_at DESC, id DESC LIMIT 1)
		ORDER BY m.id`, firstBucket, now.Unix())
		if err != nil {
			return fmt.Errorf("load dashboard providers: %w", err)
		}
		for rows.Next() {
			var currentState, lastRawState sql.NullString
			var stateSince, failureStarted, lastCheck sql.NullInt64
			var provider providerState
			if err := rows.Scan(&provider.provider.ID, &provider.provider.Name, &provider.provider.Provider, &provider.enabled, &provider.timeoutSeconds,
				&currentState, &stateSince, &lastRawState, &failureStarted, &lastCheck,
				&provider.latestErrorCode, &provider.latestDurationMS, &provider.latestCheckedAt); err != nil {
				rows.Close()
				return fmt.Errorf("scan dashboard provider: %w", err)
			}
			provider.provider.State = "unknown"
			provider.provider.Series = make([]dashboardPoint, 0)
			if currentState.Valid && lastCheck.Valid {
				provider.provider.State = currentState.String
				provider.provider.LastCheck = int64Pointer(lastCheck.Int64)
				if stateSince.Valid {
					provider.provider.StateSince = int64Pointer(stateSince.Int64)
				}
				if lastRawState.Valid {
					provider.lastRawState = lastRawState.String
				}
				if failureStarted.Valid {
					provider.failureStarted = int64Pointer(failureStarted.Int64)
				}
			}
			providers = append(providers, provider)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("iterate dashboard providers: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close dashboard providers: %w", err)
		}

		// Pre-aggregated buckets: at most MaxPoints rows per provider, rather
		// than every raw check in the window.
		rows, err = tx.QueryContext(ctx, `SELECT monitor_id,bucket_start,total,healthy,slowest_ms,p50_ms,p95_ms
		FROM check_rollups WHERE bucket_width=? AND bucket_start>=? AND bucket_start<=?
		ORDER BY monitor_id,bucket_start`, width, firstBucket, lastBucket)
		if err != nil {
			return fmt.Errorf("load dashboard rollups: %w", err)
		}
		for rows.Next() {
			var monitorID int64
			var point rollupPoint
			if err := rows.Scan(&monitorID, &point.BucketStart, &point.Total, &point.Healthy, &point.SlowestMS, &point.P50MS, &point.P95MS); err != nil {
				rows.Close()
				return fmt.Errorf("scan dashboard rollup: %w", err)
			}
			rollupsByMonitor[monitorID] = append(rollupsByMonitor[monitorID], point)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("iterate dashboard rollups: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close dashboard rollups: %w", err)
		}

		rows, err = tx.QueryContext(ctx, `SELECT i.id,i.monitor_id,m.name,m.provider,i.opened_at,i.detected_at,i.resolved_at,
		i.initial_state,i.latest_state,COALESCE(i.summary,'')
		FROM incidents i JOIN monitors m ON m.id=i.monitor_id
		WHERE i.opened_at<? AND (i.resolved_at IS NULL OR i.resolved_at>?)
		ORDER BY i.opened_at DESC,i.id DESC`, now.Unix(), windowStart.Unix())
		if err != nil {
			return fmt.Errorf("load dashboard incidents: %w", err)
		}
		for rows.Next() {
			var incident dashboardIncident
			var resolvedAt sql.NullInt64
			if err := rows.Scan(&incident.ID, &incident.MonitorID, &incident.Name, &incident.Provider, &incident.OpenedAt, &incident.DetectedAt, &resolvedAt, &incident.InitialState, &incident.LatestState, &incident.Summary); err != nil {
				rows.Close()
				return fmt.Errorf("scan dashboard incident: %w", err)
			}
			if resolvedAt.Valid {
				incident.ResolvedAt = int64Pointer(resolvedAt.Int64)
			} else {
				response.Summary.ActiveIncidents++
			}
			response.Incidents = append(response.Incidents, incident)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("iterate dashboard incidents: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close dashboard incidents: %w", err)
		}
		return nil
	})
	if err != nil {
		return dashboardResponse{}, err
	}

	for _, provider := range providers {
		provider.provider.State = dashboardDisplayState(provider.provider.State, provider.lastRawState)
		if provider.provider.State == stateDegraded && provider.failureStarted != nil {
			provider.provider.StateSince = provider.failureStarted
		}
		points := rollupsByMonitor[provider.provider.ID]
		provider.provider.Series = seriesFromRollups(points, firstBucket, lastBucket, width, spec.MaxPoints)

		var total, healthy, slowest int64
		for _, point := range points {
			total += point.Total
			healthy += point.Healthy
			if point.SlowestMS > slowest {
				slowest = point.SlowestMS
			}
			// Midnight is a boundary for every bucket width in use, so buckets
			// at or after it tile today exactly.
			if point.BucketStart >= midnight.Unix() {
				response.Summary.ChecksToday += int(point.Total)
			}
		}
		if total > 0 {
			availability := float64(healthy) / float64(total) * 100
			provider.provider.Availability = &availability
			provider.provider.SlowestMS = int64Pointer(slowest)
			provider.provider.P50MS = weightedPercentile(points, .50)
			provider.provider.P95MS = weightedPercentile(points, .95)
		}
		if provider.provider.State == stateDegraded && provider.latestErrorCode == "timeout" {
			response.Incidents = append(response.Incidents, dashboardIncident{
				ID:          -provider.provider.ID,
				MonitorID:   provider.provider.ID,
				Name:        provider.provider.Name,
				Provider:    provider.provider.Provider,
				OpenedAt:    provider.latestCheckedAt,
				DetectedAt:  provider.latestCheckedAt,
				LatestState: stateDegraded,
				Summary:     transientDegradationSummary(provider.latestDurationMS, provider.timeoutSeconds),
				Transient:   true,
			})
		}

		if provider.enabled {
			switch {
			case provider.provider.State == stateDegraded && response.Summary.OverallState == stateHealthy:
				response.Summary.OverallState = stateDegraded
			case provider.provider.State != stateHealthy && provider.provider.State != "unknown":
				response.Summary.OverallState = "outage"
			case provider.provider.State == "unknown" && response.Summary.OverallState == stateHealthy:
				response.Summary.OverallState = "degraded"
			}
		}
		if provider.enabled && provider.provider.State == stateHealthy {
			response.Summary.ProvidersOnline++
		}
		response.Providers = append(response.Providers, provider.provider)
	}
	sort.SliceStable(response.Incidents, func(i, j int) bool {
		if response.Incidents[i].OpenedAt == response.Incidents[j].OpenedAt {
			return response.Incidents[i].ID > response.Incidents[j].ID
		}
		return response.Incidents[i].OpenedAt > response.Incidents[j].OpenedAt
	})
	return response, nil
}

// seriesFromRollups expands stored buckets into a dense series, filling gaps
// with unknown points so a provider with no checks in a bucket still occupies
// its slot on the timeline.
func seriesFromRollups(points []rollupPoint, firstBucket, lastBucket, width int64, maxPoints int) []dashboardPoint {
	if width <= 0 || maxPoints <= 0 || lastBucket < firstBucket {
		return []dashboardPoint{}
	}
	stored := make(map[int64]rollupPoint, len(points))
	for _, point := range points {
		stored[point.BucketStart] = point
	}
	series := make([]dashboardPoint, 0, maxPoints)
	for bucket := firstBucket; bucket <= lastBucket; bucket += width {
		point := dashboardPoint{BucketStart: bucket, State: "unknown"}
		if summary, present := stored[bucket]; present && summary.Total > 0 {
			switch {
			case summary.Healthy == summary.Total:
				point.State = "healthy"
			case summary.Healthy == 0:
				point.State = "outage"
			default:
				point.State = "degraded"
			}
			availability := float64(summary.Healthy) / float64(summary.Total) * 100
			point.Availability = &availability
			point.P50MS = summary.P50MS
			point.P95MS = summary.P95MS
		}
		series = append(series, point)
	}
	return series
}

// weightedPercentile estimates a percentile across the window from the stored
// per-bucket percentiles, weighted by how many checks each bucket holds.
//
// The exact value would need every raw latency in the window, which is the work
// the rollups exist to avoid. Buckets are small relative to the window, so this
// tracks the true nearest-rank value closely; it is a summary figure, and the
// per-bucket percentiles it is built from remain exact.
func weightedPercentile(points []rollupPoint, percentile float64) *int64 {
	type weighted struct {
		value  int64
		weight int64
	}
	samples := make([]weighted, 0, len(points))
	var totalWeight int64
	for _, point := range points {
		value := point.P95MS
		if percentile <= .5 {
			value = point.P50MS
		}
		if value == nil || point.Total <= 0 {
			continue
		}
		samples = append(samples, weighted{value: *value, weight: point.Total})
		totalWeight += point.Total
	}
	if totalWeight == 0 {
		return nil
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].value < samples[j].value })
	target := int64(math.Ceil(percentile * float64(totalWeight)))
	var seen int64
	for _, sample := range samples {
		seen += sample.weight
		if seen >= target {
			value := sample.value
			return &value
		}
	}
	value := samples[len(samples)-1].value
	return &value
}

func (a *app) withReadOnlyDashboardTransaction(ctx context.Context, load func(*sql.Tx) error) (err error) {
	connection, err := a.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire dashboard connection: %w", err)
	}
	defer connection.Close()

	if _, err := connection.ExecContext(ctx, `PRAGMA query_only = ON`); err != nil {
		return fmt.Errorf("enable dashboard query-only mode: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, resetErr := connection.ExecContext(cleanupCtx, `PRAGMA query_only = OFF`); resetErr != nil {
			_ = connection.Raw(func(any) error { return driver.ErrBadConn })
			err = errors.Join(err, fmt.Errorf("disable dashboard query-only mode: %w", resetErr))
		}
	}()

	tx, err := connection.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin dashboard snapshot: %w", err)
	}
	defer tx.Rollback()
	loadErr := load(tx)
	rollbackErr := tx.Rollback()
	if loadErr != nil {
		return loadErr
	}
	if rollbackErr != nil {
		return fmt.Errorf("finish dashboard snapshot: %w", rollbackErr)
	}
	return nil
}

func dashboardRangeLabel(spec dashboardRange) string {
	switch spec.Window {
	case 24 * time.Hour:
		return "24h"
	case 7 * 24 * time.Hour:
		return "7d"
	case 30 * 24 * time.Hour:
		return "30d"
	default:
		return ""
	}
}

func int64Pointer(value int64) *int64 {
	return &value
}

func (a *app) dashboard(w http.ResponseWriter, r *http.Request) {
	rawRange := r.URL.Query().Get("range")
	spec, err := parseDashboardRange(rawRange)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Code: "invalid_range", Error: "range must be one of 24h, 7d, or 30d"})
		return
	}
	response, err := a.dashboardSnapshot(r.Context(), spec, time.Now().UTC())
	if err != nil {
		logger := a.logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Error("dashboard snapshot failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, apiError{Code: "dashboard_unavailable", Error: "could not load dashboard"})
		return
	}
	writeJSON(w, http.StatusOK, response)
}
