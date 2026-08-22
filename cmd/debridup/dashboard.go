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

func (a *app) dashboardSnapshot(ctx context.Context, spec dashboardRange, now time.Time) (dashboardResponse, error) {
	now = now.UTC()
	start := now.Add(-spec.Window)
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
		provider dashboardProvider
		enabled  bool
	}
	providers := make([]providerState, 0)
	samplesByMonitor := make(map[int64][]dashboardSample)
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	err := a.withReadOnlyDashboardTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT m.id,m.name,m.provider,m.enabled,s.current_state,s.state_since,s.last_check_at
		FROM monitors m LEFT JOIN monitor_states s ON s.monitor_id=m.id ORDER BY m.id`)
		if err != nil {
			return fmt.Errorf("load dashboard providers: %w", err)
		}
		for rows.Next() {
			var currentState sql.NullString
			var stateSince, lastCheck sql.NullInt64
			var provider providerState
			if err := rows.Scan(&provider.provider.ID, &provider.provider.Name, &provider.provider.Provider, &provider.enabled, &currentState, &stateSince, &lastCheck); err != nil {
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

		rows, err = tx.QueryContext(ctx, `SELECT monitor_id,source,state,duration_ms,checked_at
		FROM check_results WHERE source='authenticated' AND checked_at>=? AND checked_at<?
		ORDER BY monitor_id,checked_at,id`, start.Unix(), now.Unix())
		if err != nil {
			return fmt.Errorf("load dashboard checks: %w", err)
		}
		for rows.Next() {
			var monitorID, checkedAt int64
			var sample dashboardSample
			if err := rows.Scan(&monitorID, &sample.Source, &sample.State, &sample.DurationMS, &checkedAt); err != nil {
				rows.Close()
				return fmt.Errorf("scan dashboard check: %w", err)
			}
			sample.CheckedAt = time.Unix(checkedAt, 0).UTC()
			samplesByMonitor[monitorID] = append(samplesByMonitor[monitorID], sample)
			if !sample.CheckedAt.Before(midnight) {
				response.Summary.ChecksToday++
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("iterate dashboard checks: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close dashboard checks: %w", err)
		}

		rows, err = tx.QueryContext(ctx, `SELECT i.id,i.monitor_id,m.name,m.provider,i.opened_at,i.detected_at,i.resolved_at,
		i.initial_state,i.latest_state,COALESCE(i.summary,'')
		FROM incidents i JOIN monitors m ON m.id=i.monitor_id
		WHERE i.opened_at<? AND (i.resolved_at IS NULL OR i.resolved_at>?)
		ORDER BY i.opened_at DESC,i.id DESC`, now.Unix(), start.Unix())
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
		samples := samplesByMonitor[provider.provider.ID]
		provider.provider.Series = aggregateSeries(samples, spec, start, now)
		if len(samples) > 0 {
			latencies := make([]int64, 0, len(samples))
			healthy := 0
			var slowest int64
			for _, sample := range samples {
				if sample.State == stateHealthy {
					healthy++
				}
				latencies = append(latencies, sample.DurationMS)
				if sample.DurationMS > slowest {
					slowest = sample.DurationMS
				}
			}
			availability := float64(healthy) / float64(len(samples)) * 100
			provider.provider.Availability = &availability
			provider.provider.P50MS = nearestRank(latencies, .50)
			provider.provider.P95MS = nearestRank(latencies, .95)
			provider.provider.SlowestMS = int64Pointer(slowest)
		}

		if provider.enabled {
			switch {
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
	return response, nil
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
