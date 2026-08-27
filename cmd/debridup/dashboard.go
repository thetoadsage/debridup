package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
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
	// No range is the deliberately small current-health API used by the UI.
	// Keep the range form below for existing API consumers during the transition.
	if rawRange == "" {
		response, err := a.currentStatus(r.Context(), time.Now().UTC())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, apiError{Code: "dashboard_unavailable", Error: "could not load dashboard"})
			return
		}
		writeJSON(w, http.StatusOK, response)
		return
	}
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

type currentStatusResponse struct {
	GeneratedAt int64                   `json:"generatedAt"`
	Summary     dashboardSummary        `json:"summary"`
	Providers   []currentStatusProvider `json:"providers"`
}

type currentStatusProvider struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	Provider       string `json:"provider"`
	State          string `json:"state"`
	LatencyMS      *int64 `json:"latencyMs"`
	LastCheck      *int64 `json:"lastCheck"`
	ActiveIncident bool   `json:"activeIncident"`
	Enabled        bool   `json:"enabled"`
}

// A successful authenticated request is Slow when it consumes at least 80% of
// its configured timeout. This is informational only: monitoring state and
// notifications continue to use the existing failure thresholds.
func currentDisplayState(state string, durationMS int64, timeoutSeconds int) string {
	if state == stateHealthy && timeoutSeconds > 0 && durationMS >= int64(timeoutSeconds)*800 {
		return "slow"
	}
	if state == "" {
		return "unknown"
	}
	return state
}

func (a *app) currentStatus(ctx context.Context, now time.Time) (currentStatusResponse, error) {
	response := currentStatusResponse{GeneratedAt: now.Unix(), Providers: make([]currentStatusProvider, 0), Summary: dashboardSummary{OverallState: stateHealthy}}
	enabledProviders := 0
	err := a.withReadOnlyDashboardTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT m.id,m.name,m.provider,m.enabled,m.timeout_seconds,
		 COALESCE(s.current_state,''), COALESCE(s.last_check_at,0),
		 COALESCE(c.state,''),COALESCE(c.duration_ms,0),COALESCE(c.checked_at,0),
		 EXISTS(SELECT 1 FROM incidents i WHERE i.monitor_id=m.id AND i.resolved_at IS NULL)
		 FROM monitors m LEFT JOIN monitor_states s ON s.monitor_id=m.id
		 LEFT JOIN check_results c ON c.id=(SELECT id FROM check_results WHERE monitor_id=m.id AND source='authenticated' ORDER BY checked_at DESC,id DESC LIMIT 1)
		 ORDER BY m.id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p currentStatusProvider
			var enabled bool
			var timeout int
			var monitorState, rawState string
			var lastCheck, rawChecked, duration int64
			if err := rows.Scan(&p.ID, &p.Name, &p.Provider, &enabled, &timeout, &monitorState, &lastCheck, &rawState, &duration, &rawChecked, &p.ActiveIncident); err != nil {
				return err
			}
			// raw checked time is authoritative where a result exists.
			if rawChecked > 0 {
				lastCheck = rawChecked
				p.LatencyMS = int64Pointer(duration)
			}
			if lastCheck > 0 {
				p.LastCheck = int64Pointer(lastCheck)
			}
			if rawState == "" {
				rawState = monitorState
			}
			p.State = currentDisplayState(rawState, duration, timeout)
			if p.ActiveIncident {
				response.Summary.ActiveIncidents++
			}
			p.Enabled = enabled
			if !enabled {
				p.State = "paused"
			} else {
				enabledProviders++
			}
			if enabled && (p.State == stateHealthy || p.State == "slow") {
				response.Summary.ProvidersOnline++
			}
			if enabled && p.State != stateHealthy {
				if p.State == "slow" || p.State == stateDegraded || p.State == "unknown" {
					if response.Summary.OverallState == stateHealthy {
						response.Summary.OverallState = stateDegraded
					}
				} else {
					response.Summary.OverallState = "outage"
				}
			}
			response.Providers = append(response.Providers, p)
		}
		return rows.Err()
	})
	if err == nil && enabledProviders == 0 {
		response.Summary.OverallState = "unknown"
	}
	return response, err
}

type checkLogRow struct {
	ID          int64   `json:"id"`
	MonitorID   int64   `json:"monitorId"`
	Name        string  `json:"name"`
	Source      string  `json:"source"`
	State       string  `json:"state"`
	DurationMS  int64   `json:"durationMs"`
	HTTPStatus  *int    `json:"httpStatus"`
	ErrorCode   *string `json:"errorCode"`
	ErrorDetail *string `json:"errorDetail"`
	CheckedAt   int64   `json:"checkedAt"`
	IncidentID  *int64  `json:"incidentId"`
}

func (a *app) listAllChecks(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 200 {
			limit = n
		} else {
			writeJSON(w, 400, apiError{Code: "invalid_limit", Error: "limit must be 1 to 200"})
			return
		}
	}
	beforeTime, beforeID := int64(0), int64(0)
	if raw := r.URL.Query().Get("before"); raw != "" {
		parts := strings.Split(raw, ".")
		if len(parts) != 2 {
			writeJSON(w, 400, apiError{Code: "invalid_cursor", Error: "before must be a check cursor"})
			return
		}
		var err error
		beforeTime, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			writeJSON(w, 400, apiError{Code: "invalid_cursor", Error: "before must be a check cursor"})
			return
		}
		beforeID, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || beforeID < 1 {
			writeJSON(w, 400, apiError{Code: "invalid_cursor", Error: "before must be a check cursor"})
			return
		}
	}
	query := `SELECT c.id,c.monitor_id,m.name,c.source,c.state,c.duration_ms,c.http_status,c.error_code,c.error_detail,c.checked_at,(SELECT i.id FROM incidents i WHERE i.monitor_id=c.monitor_id AND i.opened_at<=c.checked_at AND (i.resolved_at IS NULL OR i.resolved_at>=c.checked_at) ORDER BY i.opened_at DESC,i.id DESC LIMIT 1) FROM check_results c JOIN monitors m ON m.id=c.monitor_id WHERE (?=0 OR c.checked_at<? OR (c.checked_at=? AND c.id<?)) ORDER BY c.checked_at DESC,c.id DESC LIMIT ?`
	rows, err := a.db.QueryContext(r.Context(), query, beforeID, beforeTime, beforeTime, beforeID, limit+1)
	if err != nil {
		writeJSON(w, 500, apiError{Code: "checks_unavailable", Error: "could not load checks"})
		return
	}
	defer rows.Close()
	out := make([]checkLogRow, 0, limit)
	var next *string
	for rows.Next() {
		var x checkLogRow
		var hs sql.NullInt64
		var code sql.NullString
		var detail sql.NullString
		var incident sql.NullInt64
		if err := rows.Scan(&x.ID, &x.MonitorID, &x.Name, &x.Source, &x.State, &x.DurationMS, &hs, &code, &detail, &x.CheckedAt, &incident); err != nil {
			writeJSON(w, 500, apiError{Code: "checks_unavailable", Error: "could not load checks"})
			return
		}
		if len(out) == limit {
			// The next request is exclusive, so use the final row we returned;
			// using this look-ahead row would skip it.
			cursor := strconv.FormatInt(out[len(out)-1].CheckedAt, 10) + "." + strconv.FormatInt(out[len(out)-1].ID, 10)
			next = &cursor
			break
		}
		if hs.Valid {
			v := int(hs.Int64)
			x.HTTPStatus = &v
		}
		if detail.Valid {
			v := detail.String
			x.ErrorDetail = &v
		}
		if code.Valid {
			v := code.String
			x.ErrorCode = &v
		}
		if incident.Valid {
			v := incident.Int64
			x.IncidentID = &v
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, 500, apiError{Code: "checks_unavailable", Error: "could not load checks"})
		return
	}
	writeJSON(w, 200, map[string]any{"checks": out, "nextBefore": next})
}

func reportRange(raw string, now time.Time) (time.Time, string, error) {
	switch raw {
	case "1d":
		return now.Add(-24 * time.Hour), "1 day", nil
	case "7d":
		return now.Add(-7 * 24 * time.Hour), "7 days", nil
	case "30d":
		return now.Add(-30 * 24 * time.Hour), "30 days", nil
	case "90d":
		return now.Add(-90 * 24 * time.Hour), "90 days", nil
	case "all":
		return time.Unix(0, 0), "all retained history", nil
	default:
		return time.Time{}, "", errors.New("invalid report range")
	}
}
func (a *app) report(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	start, label, err := reportRange(r.URL.Query().Get("range"), now)
	if err != nil {
		writeJSON(w, 400, apiError{Code: "invalid_range", Error: "range must be 1d, 7d, 30d, 90d, or all"})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="debridup-report.html"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Reports are self-contained downloads with no scripts or external assets.
	// Allow their embedded stylesheet while retaining a restrictive document CSP.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	var firstCheck, lastCheck sql.NullInt64
	if err := a.db.QueryRowContext(r.Context(), `SELECT MIN(checked_at),MAX(checked_at) FROM check_results WHERE checked_at>=?`, start.Unix()).Scan(&firstCheck, &lastCheck); err != nil {
		http.Error(w, "Could not generate report", 500)
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT m.name,c.source,c.state,c.duration_ms,c.http_status,c.error_code,c.error_detail,c.checked_at,(SELECT i.id FROM incidents i WHERE i.monitor_id=c.monitor_id AND i.opened_at<=c.checked_at AND (i.resolved_at IS NULL OR i.resolved_at>=c.checked_at) ORDER BY i.opened_at DESC,i.id DESC LIMIT 1) FROM check_results c JOIN monitors m ON m.id=c.monitor_id WHERE c.checked_at>=? ORDER BY c.checked_at DESC,c.id DESC`, start.Unix())
	if err != nil {
		http.Error(w, "Could not generate report", 500)
		return
	}
	defer rows.Close()
	type serviceSummary struct {
		name                                            string
		total, authenticated, healthy, average, maximum int64
	}
	summaryRows, err := a.db.QueryContext(r.Context(), `SELECT m.name,COUNT(*),SUM(CASE WHEN c.source='authenticated' THEN 1 ELSE 0 END),SUM(CASE WHEN c.source='authenticated' AND c.state='healthy' THEN 1 ELSE 0 END),COALESCE(AVG(CASE WHEN c.source='authenticated' THEN c.duration_ms END),0),COALESCE(MAX(CASE WHEN c.source='authenticated' THEN c.duration_ms END),0) FROM check_results c JOIN monitors m ON m.id=c.monitor_id WHERE c.checked_at>=? GROUP BY m.id,m.name ORDER BY m.name`, start.Unix())
	if err != nil {
		http.Error(w, "Could not generate report", 500)
		return
	}
	summaries := make([]serviceSummary, 0)
	for summaryRows.Next() {
		var s serviceSummary
		if err := summaryRows.Scan(&s.name, &s.total, &s.authenticated, &s.healthy, &s.average, &s.maximum); err != nil {
			summaryRows.Close()
			http.Error(w, "Could not generate report", 500)
			return
		}
		summaries = append(summaries, s)
	}
	if err := summaryRows.Err(); err != nil {
		summaryRows.Close()
		http.Error(w, "Could not generate report", 500)
		return
	}
	summaryRows.Close()
	incidentRows, err := a.db.QueryContext(r.Context(), `SELECT m.name,i.opened_at,i.resolved_at,i.latest_state,COALESCE(i.summary,'') FROM incidents i JOIN monitors m ON m.id=i.monitor_id WHERE i.opened_at>=? OR (i.resolved_at IS NULL OR i.resolved_at>=?) ORDER BY i.opened_at DESC`, start.Unix(), start.Unix())
	if err != nil {
		http.Error(w, "Could not generate report", 500)
		return
	}
	defer incidentRows.Close()
	coverage := "No retained checks in this range"
	if firstCheck.Valid {
		coverage = time.Unix(firstCheck.Int64, 0).UTC().Format(time.RFC3339) + " to " + time.Unix(lastCheck.Int64, 0).UTC().Format(time.RFC3339)
	}
	totalChecks, authenticatedChecks, healthyChecks, weightedLatency, maximumLatency := int64(0), int64(0), int64(0), int64(0), int64(0)
	for _, s := range summaries {
		totalChecks += s.total
		authenticatedChecks += s.authenticated
		healthyChecks += s.healthy
		weightedLatency += s.average * s.authenticated
		if s.maximum > maximumLatency {
			maximumLatency = s.maximum
		}
	}
	overallAvailability, overallAverage := 0.0, int64(0)
	if authenticatedChecks > 0 {
		overallAvailability = float64(healthyChecks) * 100 / float64(authenticatedChecks)
		overallAverage = weightedLatency / authenticatedChecks
	}
	fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><title>DebridUp report</title><style>body{font:15px system-ui;margin:2rem;color:#18212b}table{border-collapse:collapse;width:100%%;margin-bottom:2rem}th,td{padding:.45rem;border-bottom:1px solid #ccd;text-align:left}th{background:#eef}code{white-space:pre-wrap}</style><h1>DebridUp incident report</h1><p>Range: <strong>%s</strong>. Data coverage: %s. Generated: %s. Raw checks are retained for a configured period (90 days by default); incidents may outlive these checks.</p><h2>Overall summary</h2><p>%d retained responses across %d services. Authenticated availability: %.2f%% (%d checks). Average latency: %d ms; maximum latency: %d ms. Public checks remain in the response history but do not affect availability or latency statistics.</p><h2>Service summary</h2><table><thead><tr><th>Service</th><th>Responses</th><th>Authenticated availability</th><th>Average latency</th><th>Maximum latency</th></tr></thead><tbody>", template.HTMLEscapeString(label), template.HTMLEscapeString(coverage), now.Format(time.RFC3339), totalChecks, len(summaries), overallAvailability, authenticatedChecks, overallAverage, maximumLatency)
	for _, s := range summaries {
		availability := 0.0
		if s.authenticated > 0 {
			availability = float64(s.healthy) * 100 / float64(s.authenticated)
		}
		fmt.Fprintf(w, "<tr><td>%s</td><td>%d</td><td>%.2f%% (%d checks)</td><td>%d ms</td><td>%d ms</td></tr>", template.HTMLEscapeString(s.name), s.total, availability, s.authenticated, s.average, s.maximum)
	}
	fmt.Fprint(w, "</tbody></table><h2>Incident and recovery timeline</h2><table><thead><tr><th>Service</th><th>Opened</th><th>Resolved</th><th>State</th><th>Summary</th></tr></thead><tbody>")
	for incidentRows.Next() {
		var name, state, summary string
		var opened int64
		var resolved sql.NullInt64
		if err := incidentRows.Scan(&name, &opened, &resolved, &state, &summary); err != nil {
			return
		}
		resolvedText := "Ongoing"
		if resolved.Valid {
			resolvedText = time.Unix(resolved.Int64, 0).UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(w, "<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>", template.HTMLEscapeString(name), time.Unix(opened, 0).UTC().Format(time.RFC3339), template.HTMLEscapeString(resolvedText), template.HTMLEscapeString(state), template.HTMLEscapeString(summary))
	}
	if err := incidentRows.Err(); err != nil {
		a.log().Error("stream report incidents", "error", err)
		fmt.Fprint(w, `<tr><td colspan="5">The incident timeline ended early because stored data could not be read.</td></tr>`)
	}
	fmt.Fprint(w, "</tbody></table><h2>Complete response history</h2><table><thead><tr><th>Service</th><th>Source</th><th>Result</th><th>Latency</th><th>HTTP/error</th><th>Checked</th><th>Incident</th></tr></thead><tbody>")
	for rows.Next() {
		var name, source, state string
		var duration, checked int64
		var hs sql.NullInt64
		var code sql.NullString
		var detail sql.NullString
		var incident sql.NullInt64
		if err := rows.Scan(&name, &source, &state, &duration, &hs, &code, &detail, &checked, &incident); err != nil {
			return
		}
		result := state
		if code.Valid {
			result += ": " + code.String
		}
		if detail.Valid {
			result += ": " + detail.String
		}
		httpValue := "—"
		if hs.Valid {
			httpValue = strconv.FormatInt(hs.Int64, 10)
		}
		incidentValue := "—"
		if incident.Valid {
			incidentValue = "#" + strconv.FormatInt(incident.Int64, 10)
		}
		fmt.Fprintf(w, "<tr><td>%s</td><td>%s</td><td>%s</td><td>%d ms</td><td>%s</td><td>%s</td><td>%s</td></tr>", template.HTMLEscapeString(name), template.HTMLEscapeString(source), template.HTMLEscapeString(result), duration, template.HTMLEscapeString(httpValue), time.Unix(checked, 0).UTC().Format(time.RFC3339), template.HTMLEscapeString(incidentValue))
	}
	if err := rows.Err(); err != nil {
		a.log().Error("stream report responses", "error", err)
		fmt.Fprint(w, `<tr><td colspan="7">The response history ended early because stored data could not be read.</td></tr>`)
	}
	fmt.Fprint(w, "</tbody></table>")
}
