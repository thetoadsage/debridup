package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"
)

func TestDashboardEndpointReturnsConsolidatedSnapshot(t *testing.T) {
	a := testApp(t)
	seedDashboardFixture(t, a.db, time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC))
	req := authenticatedRequest(t, a, http.MethodGet, "/api/dashboard?range=24h")
	rr := httptest.NewRecorder()
	a.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got dashboardResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Range != "24h" || len(got.Providers) != 3 {
		t.Fatalf("snapshot=%#v", got)
	}
}

func TestCurrentStatusMarksSlowWithoutChangingIncidentState(t *testing.T) {
	a := testApp(t)
	if err := migrateDatabase(context.Background(), a.db); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Unix()
	result, err := a.db.Exec(`INSERT INTO monitors(provider,name,timeout_seconds,created_at,updated_at) VALUES(?,?,?,?,?)`, "torbox", "TorBox", 10, now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	if _, err := a.db.Exec(`INSERT INTO monitor_states(monitor_id,current_state,state_since,last_raw_state,last_check_at) VALUES(?,?,?,?,?)`, id, stateHealthy, now, stateHealthy, now); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO check_results(monitor_id,source,state,duration_ms,checked_at) VALUES(?,?,?,?,?)`, id, "authenticated", stateHealthy, 8000, now); err != nil {
		t.Fatal(err)
	}
	req := authenticatedRequest(t, a, http.MethodGet, "/api/dashboard")
	rr := httptest.NewRecorder()
	a.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got currentStatusResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Providers) != 1 || got.Providers[0].State != "slow" || got.Summary.ProvidersOnline != 1 || got.Summary.ActiveIncidents != 0 {
		t.Fatalf("status=%#v", got)
	}
}

func TestCurrentStatusShowsDisabledMonitorAsPaused(t *testing.T) {
	a := testApp(t)
	if err := migrateDatabase(context.Background(), a.db); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Unix()
	result, err := a.db.Exec(`INSERT INTO monitors(provider,name,enabled,created_at,updated_at) VALUES(?,?,?,?,?)`, "torbox", "Paused", 0, now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	if _, err := a.db.Exec(`INSERT INTO monitor_states(monitor_id,current_state,state_since,last_raw_state,last_check_at) VALUES(?,?,?,?,?)`, id, stateHealthy, now, stateHealthy, now); err != nil {
		t.Fatal(err)
	}
	got, err := a.currentStatus(context.Background(), time.Unix(now, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Providers) != 1 || got.Providers[0].State != "paused" || got.Providers[0].Enabled || got.Summary.ProvidersOnline != 0 || got.Summary.OverallState != "unknown" {
		t.Fatalf("status=%#v", got)
	}
}

func TestCheckLogCursorUsesTimestampAndIDTuple(t *testing.T) {
	a := testApp(t)
	if err := migrateDatabase(context.Background(), a.db); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	res, err := a.db.Exec(`INSERT INTO monitors(provider,name,created_at,updated_at) VALUES(?,?,?,?)`, "torbox", "TorBox", now, now)
	if err != nil {
		t.Fatal(err)
	}
	monitorID, _ := res.LastInsertId()
	// IDs increase in insertion order while timestamps intentionally do not.
	for _, checked := range []int64{100, 300, 200} {
		if _, err := a.db.Exec(`INSERT INTO check_results(monitor_id,source,state,duration_ms,checked_at) VALUES(?,?,?,?,?)`, monitorID, "authenticated", stateHealthy, 1, checked); err != nil {
			t.Fatal(err)
		}
	}
	get := func(path string) struct {
		Checks     []checkLogRow `json:"checks"`
		NextBefore *string       `json:"nextBefore"`
	} {
		rr := httptest.NewRecorder()
		a.routes().ServeHTTP(rr, authenticatedRequest(t, a, http.MethodGet, path))
		if rr.Code != 200 {
			t.Fatalf("%d %s", rr.Code, rr.Body.String())
		}
		var got struct {
			Checks     []checkLogRow `json:"checks"`
			NextBefore *string       `json:"nextBefore"`
		}
		if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	first := get("/api/checks?limit=2")
	if len(first.Checks) != 2 || first.NextBefore == nil {
		t.Fatalf("first=%#v", first)
	}
	second := get("/api/checks?limit=2&before=" + *first.NextBefore)
	if len(second.Checks) != 1 {
		t.Fatalf("second=%#v", second)
	}
	seen := map[int64]bool{}
	for _, c := range append(first.Checks, second.Checks...) {
		if seen[c.ID] {
			t.Fatalf("duplicate %d", c.ID)
		}
		seen[c.ID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("seen=%v", seen)
	}
}

func TestCheckLogAssociatesChecksInsideIncidentWindow(t *testing.T) {
	a := testApp(t)
	if err := migrateDatabase(context.Background(), a.db); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	res, err := a.db.Exec(`INSERT INTO monitors(provider,name,created_at,updated_at) VALUES(?,?,?,?)`, "torbox", "TorBox", now, now)
	if err != nil {
		t.Fatal(err)
	}
	monitorID, _ := res.LastInsertId()
	incident, err := a.db.Exec(`INSERT INTO incidents(monitor_id,opened_at,detected_at,resolved_at,initial_state,latest_state,summary) VALUES(?,?,?,?,?,?,?)`, monitorID, 100, 110, 200, stateAPI, stateHealthy, "Recovered")
	if err != nil {
		t.Fatal(err)
	}
	incidentID, _ := incident.LastInsertId()
	if _, err := a.db.Exec(`INSERT INTO check_results(monitor_id,source,state,duration_ms,error_code,checked_at) VALUES(?,?,?,?,?,?)`, monitorID, "authenticated", stateAPI, 250, "server_error", 150); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	a.routes().ServeHTTP(rr, authenticatedRequest(t, a, http.MethodGet, "/api/checks?limit=10"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Checks []checkLogRow `json:"checks"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Checks) != 1 || got.Checks[0].IncidentID == nil || *got.Checks[0].IncidentID != incidentID || got.Checks[0].ErrorCode == nil || *got.Checks[0].ErrorCode != "server_error" {
		t.Fatalf("checks=%#v", got.Checks)
	}
}

func TestReportRanges(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	for _, value := range []string{"1d", "7d", "30d", "90d", "all"} {
		t.Run(value, func(t *testing.T) {
			start, label, err := reportRange(value, now)
			if err != nil || label == "" || start.After(now) {
				t.Fatalf("start=%v label=%q err=%v", start, label, err)
			}
		})
	}
	if _, _, err := reportRange("24h", now); err == nil {
		t.Fatal("unsupported report range was accepted")
	}
}

func TestReportIsSafeAndUsesAuthenticatedStatistics(t *testing.T) {
	a := testApp(t)
	if err := migrateDatabase(context.Background(), a.db); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Unix()
	res, err := a.db.Exec(`INSERT INTO monitors(provider,name,created_at,updated_at) VALUES(?,?,?,?)`, "torbox", `<Unsafe & Service>`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	monitorID, _ := res.LastInsertId()
	checks := []struct {
		source, state, code string
		duration            int64
	}{
		{"authenticated", stateHealthy, "", 100},
		{"authenticated", stateAPI, `<server_error>`, 300},
		{"public", stateHealthy, "", 1000},
	}
	for index, check := range checks {
		if _, err := a.db.Exec(`INSERT INTO check_results(monitor_id,source,state,duration_ms,error_code,checked_at) VALUES(?,?,?,?,?,?)`, monitorID, check.source, check.state, check.duration, nullString(check.code), now-int64(index)); err != nil {
			t.Fatal(err)
		}
	}
	rr := httptest.NewRecorder()
	a.routes().ServeHTTP(rr, authenticatedRequest(t, a, http.MethodGet, "/api/report?range=all"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Disposition"); !strings.Contains(got, "attachment") || !strings.Contains(got, ".html") {
		t.Fatalf("content-disposition=%q", got)
	}
	if got := rr.Header().Get("Content-Security-Policy"); !strings.Contains(got, "style-src 'unsafe-inline'") || !strings.Contains(got, "default-src 'none'") {
		t.Fatalf("csp=%q", got)
	}
	body := rr.Body.String()
	for _, expected := range []string{"&lt;Unsafe &amp; Service&gt;", "Authenticated availability: 50.00% (2 checks)", "Average latency: 200 ms", "maximum latency: 300 ms", "&lt;server_error&gt;"} {
		if !strings.Contains(body, expected) {
			t.Errorf("report missing %q", expected)
		}
	}
	if strings.Contains(body, `<Unsafe & Service>`) || strings.Contains(body, `<server_error>`) {
		t.Fatal("report contains unescaped stored content")
	}
}

func TestDashboardEndpointRejectsInvalidRange(t *testing.T) {
	a := testApp(t)
	req := authenticatedRequest(t, a, http.MethodGet, "/api/dashboard?range=1y")
	rr := httptest.NewRecorder()
	a.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"code":"invalid_range"`) {
		t.Fatalf("body=%s", rr.Body.String())
	}
}

func TestDashboardEndpointRequiresAuthentication(t *testing.T) {
	a := testApp(t)
	req := httptest.NewRequest(http.MethodGet, "/api/dashboard?range=24h", nil)
	rr := httptest.NewRecorder()
	a.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestDashboardEndpointReturnsStableSnapshotError(t *testing.T) {
	a := testApp(t)
	a.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	req := authenticatedRequest(t, a, http.MethodGet, "/api/dashboard?range=24h")
	rr := httptest.NewRecorder()
	a.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got apiError
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Code != "dashboard_unavailable" || got.Error != "could not load dashboard" {
		t.Fatalf("error=%#v", got)
	}
}

func TestDashboardSnapshotReturnsSummaryAndProviderMetrics(t *testing.T) {
	a := testApp(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	seedDashboardFixture(t, a.db, now)
	spec, _ := parseDashboardRange("24h")
	got, err := a.dashboardSnapshot(context.Background(), spec, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.GeneratedAt != now.Unix() || got.Summary.OverallState != "outage" || got.Summary.ProvidersOnline != 1 || got.Summary.ActiveIncidents != 1 || got.Summary.ChecksToday != 3 {
		t.Fatalf("snapshot summary=%#v generatedAt=%d", got.Summary, got.GeneratedAt)
	}
	if len(got.Providers) != 3 || len(got.Incidents) != 2 {
		t.Fatalf("providers=%d incidents=%d", len(got.Providers), len(got.Incidents))
	}
	first := got.Providers[0]
	if first.Name != "Provider Alpha" || first.State != stateHealthy || first.Availability == nil || *first.Availability != 50 || first.P50MS == nil || *first.P50MS != 100 || first.P95MS == nil || *first.P95MS != 300 || first.SlowestMS == nil || *first.SlowestMS != 300 {
		t.Fatalf("first provider=%#v", first)
	}
	third := got.Providers[2]
	if third.State != "unknown" || third.StateSince != nil || third.LastCheck != nil || third.Availability != nil || third.P50MS != nil || third.P95MS != nil || third.SlowestMS != nil {
		t.Fatalf("provider without current data=%#v", third)
	}
}

func TestDashboardSnapshotSupportsRangesWithBoundedChronologicalSeries(t *testing.T) {
	a := testApp(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	seedDashboardFixture(t, a.db, now)
	for _, raw := range []string{"24h", "7d", "30d"} {
		t.Run(raw, func(t *testing.T) {
			spec, _ := parseDashboardRange(raw)
			got, err := a.dashboardSnapshot(context.Background(), spec, now)
			if err != nil {
				t.Fatal(err)
			}
			if got.Providers == nil || got.Incidents == nil {
				t.Fatalf("nil arrays: %#v", got)
			}
			for _, provider := range got.Providers {
				if provider.Series == nil || len(provider.Series) > spec.MaxPoints {
					t.Fatalf("provider=%d series=%d", provider.ID, len(provider.Series))
				}
				for i, point := range provider.Series {
					if point.BucketStart < now.Add(-spec.Window).Unix() || point.BucketStart >= now.Unix() {
						t.Fatalf("provider=%d point outside range: %#v", provider.ID, point)
					}
					if i > 0 && point.BucketStart <= provider.Series[i-1].BucketStart {
						t.Fatalf("provider=%d series not chronological", provider.ID)
					}
				}
			}
		})
	}
}

func TestDashboardSnapshotReturnsNonNilArraysForEmptyDatabase(t *testing.T) {
	a := testApp(t)
	if err := migrateDatabase(context.Background(), a.db); err != nil {
		t.Fatal(err)
	}
	spec, _ := parseDashboardRange("24h")
	got, err := a.dashboardSnapshot(context.Background(), spec, time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if got.Providers == nil || len(got.Providers) != 0 || got.Incidents == nil || len(got.Incidents) != 0 {
		t.Fatalf("snapshot=%#v", got)
	}
}

func TestDashboardSnapshotExcludesDisabledMonitorsFromOverallHealth(t *testing.T) {
	a := testApp(t)
	if err := migrateDatabase(context.Background(), a.db); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	for _, monitor := range []struct {
		provider string
		enabled  int
		state    string
	}{
		{provider: "torbox", enabled: 1, state: stateHealthy},
		{provider: "premiumize", enabled: 0, state: stateAPI},
	} {
		result, err := a.db.Exec(`INSERT INTO monitors(provider,name,enabled,created_at,updated_at) VALUES(?,?,?,?,?)`, monitor.provider, monitor.provider, monitor.enabled, now.Unix(), now.Unix())
		if err != nil {
			t.Fatal(err)
		}
		monitorID, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.db.Exec(`INSERT INTO monitor_states(monitor_id,current_state,state_since,last_raw_state,last_check_at) VALUES(?,?,?,?,?)`, monitorID, monitor.state, now.Unix(), monitor.state, now.Unix()); err != nil {
			t.Fatal(err)
		}
	}

	spec, _ := parseDashboardRange("24h")
	got, err := a.dashboardSnapshot(context.Background(), spec, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary.OverallState != stateHealthy || got.Summary.ProvidersOnline != 1 {
		t.Fatalf("summary=%#v", got.Summary)
	}
}

func TestDashboardSnapshotShowsUnconfirmedLatestFailureAsDegraded(t *testing.T) {
	a := testApp(t)
	if err := migrateDatabase(context.Background(), a.db); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	result, err := a.db.Exec(`INSERT INTO monitors(provider,name,enabled,created_at,updated_at) VALUES(?,?,?,?,?)`, "torbox", "TorBox", 1, now.Unix(), now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	monitorID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	failureStarted := now.Add(-time.Minute).Unix()
	if _, err := a.db.Exec(`INSERT INTO monitor_states(monitor_id,current_state,state_since,last_raw_state,failure_started_at,last_check_at) VALUES(?,?,?,?,?,?)`, monitorID, stateHealthy, now.Add(-time.Hour).Unix(), stateConnection, failureStarted, failureStarted); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO check_results(monitor_id,source,state,duration_ms,checked_at) VALUES(?,?,?,?,?)`, monitorID, "authenticated", stateConnection, 15000, failureStarted); err != nil {
		t.Fatal(err)
	}

	spec, _ := parseDashboardRange("24h")
	got, err := a.dashboardSnapshot(context.Background(), spec, now)
	if err != nil {
		t.Fatal(err)
	}
	provider := got.Providers[0]
	if provider.State != stateDegraded || provider.StateSince == nil || *provider.StateSince != failureStarted {
		t.Fatalf("provider=%#v", provider)
	}
	if got.Summary.OverallState != stateDegraded || got.Summary.ProvidersOnline != 0 || got.Summary.ActiveIncidents != 0 {
		t.Fatalf("summary=%#v", got.Summary)
	}
}

func TestDashboardSnapshotAddsTransientTimeoutDegradationWithoutIncident(t *testing.T) {
	a := testApp(t)
	if err := migrateDatabase(context.Background(), a.db); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	result, err := a.db.Exec(`INSERT INTO monitors(provider,name,enabled,timeout_seconds,created_at,updated_at) VALUES(?,?,?,?,?,?)`, "torbox", "TorBox", 1, 15, now.Unix(), now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	monitorID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	checkedAt := now.Add(-time.Minute).Unix()
	if _, err := a.db.Exec(`INSERT INTO monitor_states(monitor_id,current_state,state_since,last_raw_state,failure_started_at,last_check_at) VALUES(?,?,?,?,?,?)`, monitorID, stateHealthy, now.Add(-time.Hour).Unix(), stateConnection, checkedAt, checkedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO check_results(monitor_id,source,state,duration_ms,error_code,checked_at) VALUES(?,?,?,?,?,?)`, monitorID, "authenticated", stateConnection, 15000, "timeout", checkedAt); err != nil {
		t.Fatal(err)
	}

	spec, _ := parseDashboardRange("24h")
	got, err := a.dashboardSnapshot(context.Background(), spec, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Incidents) != 1 {
		t.Fatalf("incidents=%#v", got.Incidents)
	}
	incident := got.Incidents[0]
	if !incident.Transient || incident.LatestState != stateDegraded || !strings.Contains(incident.Summary, "15.0s") || !strings.Contains(incident.Summary, "no notification was sent") {
		t.Fatalf("incident=%#v", incident)
	}
	if got.Summary.ActiveIncidents != 0 {
		t.Fatalf("summary=%#v", got.Summary)
	}
}

func TestDashboardSnapshotUsesOneReadOnlyTransactionAndThreeBulkQueries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dashboard.db")
	seedDB, err := openDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	seedDashboardFixture(t, seedDB, time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC))
	if err := seedDB.Close(); err != nil {
		t.Fatal(err)
	}

	observer := &dashboardQueryObserver{driver: &sqlite.Driver{}}
	driverName := fmt.Sprintf("dashboard-observer-%d", dashboardDriverSequence.Add(1))
	sql.Register(driverName, observer)
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	spec, _ := parseDashboardRange("24h")
	if _, err := (&app{db: db}).dashboardSnapshot(context.Background(), spec, time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if observer.transactions.Load() != 1 || !observer.readOnly.Load() || observer.queries.Load() != 3 {
		t.Fatalf("transactions=%d readOnly=%t queries=%d", observer.transactions.Load(), observer.readOnly.Load(), observer.queries.Load())
	}
}

func TestDashboardReadOnlyTransactionRejectsWritesAndRestoresConnection(t *testing.T) {
	a := testApp(t)
	if err := migrateDatabase(context.Background(), a.db); err != nil {
		t.Fatal(err)
	}
	a.db.SetMaxOpenConns(1)

	var writeErr error
	err := a.withReadOnlyDashboardTransaction(context.Background(), func(tx *sql.Tx) error {
		_, writeErr = tx.ExecContext(context.Background(), `INSERT INTO monitors(provider,name,created_at,updated_at) VALUES('torbox','Rejected Write',1,1)`)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if writeErr == nil {
		t.Fatal("write succeeded inside the dashboard snapshot transaction")
	}
	if _, err := a.db.Exec(`INSERT INTO monitors(provider,name,created_at,updated_at) VALUES('torbox','Normal Write',2,2)`); err != nil {
		t.Fatalf("normal write failed after dashboard snapshot cleanup: %v", err)
	}
}

// authenticatedRequest issues a real session, so tests exercise the same
// signing and lookup path as a signed-in browser.
func authenticatedRequest(t *testing.T, a *app, method, target string) *http.Request {
	t.Helper()
	if a.sessions == nil {
		a.sessions = newSessionStore(a.db)
	}
	// Registered directly in the store: these tests cover handler behaviour,
	// and some deliberately run without a migrated or open database. Session
	// issuing, revocation, and persistence have their own tests.
	id := "test-session"
	a.sessions.mu.Lock()
	a.sessions.active[id] = time.Now().Add(time.Hour).Unix()
	a.sessions.mu.Unlock()
	data := []byte(id)
	mac := hmac.New(sha256.New, a.cookieKey)
	_, _ = mac.Write(data)
	value := base64.RawURLEncoding.EncodeToString(data) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	req := httptest.NewRequest(method, target, nil)
	req.AddCookie(&http.Cookie{Name: "debridup_session", Value: value})
	return req
}

func seedDashboardFixture(t *testing.T, db *sql.DB, now time.Time) {
	t.Helper()
	if err := migrateDatabase(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	providers := []struct {
		provider, name string
		enabled        int
	}{
		{"torbox", "Provider Alpha", 1},
		{"premiumize", "Provider Beta", 1},
		{"alldebrid", "Provider Gamma", 0},
	}
	ids := make([]int64, 0, len(providers))
	for _, provider := range providers {
		result, err := db.Exec(`INSERT INTO monitors(provider,name,enabled,created_at,updated_at) VALUES(?,?,?,?,?)`, provider.provider, provider.name, provider.enabled, now.Add(-48*time.Hour).Unix(), now.Add(-48*time.Hour).Unix())
		if err != nil {
			t.Fatal(err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if _, err := db.Exec(`INSERT INTO monitor_states(monitor_id,current_state,state_since,last_raw_state,last_check_at) VALUES(?,?,?,?,?)`, ids[0], stateHealthy, now.Add(-time.Hour).Unix(), stateHealthy, now.Add(-20*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO monitor_states(monitor_id,current_state,state_since,last_raw_state,last_check_at) VALUES(?,?,?,?,?)`, ids[1], stateAPI, now.Add(-2*time.Hour).Unix(), stateAPI, now.Add(-10*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		monitorID, duration int64
		state               string
		checkedAt           time.Time
	}{
		{ids[0], 100, stateHealthy, now.Add(-40 * time.Minute)},
		{ids[0], 300, stateAPI, now.Add(-20 * time.Minute)},
		{ids[1], 400, stateAPI, now.Add(-10 * time.Minute)},
		{ids[1], 50, stateHealthy, now.Add(-25 * time.Hour)},
	}
	for _, check := range checks {
		if _, err := db.Exec(`INSERT INTO check_results(monitor_id,source,state,duration_ms,checked_at) VALUES(?,?,?,?,?)`, check.monitorID, "authenticated", check.state, check.duration, check.checkedAt.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO check_results(monitor_id,source,state,duration_ms,checked_at) VALUES(?,?,?,?,?)`, ids[0], "public", stateHealthy, 1, now.Add(-5*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO incidents(monitor_id,opened_at,detected_at,resolved_at,initial_state,latest_state,summary) VALUES(?,?,?,?,?,?,?)`, ids[0], now.Add(-2*time.Hour).Unix(), now.Add(-2*time.Hour).Unix(), now.Add(-time.Hour).Unix(), stateAPI, stateHealthy, "Recovered after a transient failure."); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO incidents(monitor_id,opened_at,detected_at,initial_state,latest_state,summary) VALUES(?,?,?,?,?,?)`, ids[1], now.Add(-90*time.Minute).Unix(), now.Add(-90*time.Minute).Unix(), stateAPI, stateAPI, "Authenticated checks are failing."); err != nil {
		t.Fatal(err)
	}
	// Raw checks were inserted in bulk rather than through recordResult, so the
	// rollups are built here the same way the backfill migration builds them.
	if err := rebuildRollups(context.Background(), db, nil); err != nil {
		t.Fatal(err)
	}
}

var dashboardDriverSequence atomic.Uint64

type dashboardQueryObserver struct {
	driver       driver.Driver
	transactions atomic.Int64
	queries      atomic.Int64
	readOnly     atomic.Bool
}

func (d *dashboardQueryObserver) Open(name string) (driver.Conn, error) {
	connection, err := d.driver.Open(name)
	if err != nil {
		return nil, err
	}
	return &dashboardObservedConnection{Conn: connection, observer: d}, nil
}

type dashboardObservedConnection struct {
	driver.Conn
	observer *dashboardQueryObserver
}

func (c *dashboardObservedConnection) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.observer.transactions.Add(1)
	c.observer.readOnly.Store(opts.ReadOnly)
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func (c *dashboardObservedConnection) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.observer.queries.Add(1)
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

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

func TestSeriesFromRollupsClassifiesBucketState(t *testing.T) {
	width := int64(900)
	first := int64(1787313600)
	p := func(v int64) *int64 { return &v }
	points := []rollupPoint{
		{BucketStart: first, Total: 2, Healthy: 2, SlowestMS: 120, P50MS: p(100), P95MS: p(120)},
		{BucketStart: first + width, Total: 2, Healthy: 1, SlowestMS: 300, P50MS: p(100), P95MS: p(300)},
		{BucketStart: first + 2*width, Total: 2, Healthy: 0, SlowestMS: 400, P50MS: p(350), P95MS: p(400)},
		// first+3*width intentionally absent: a bucket with no checks.
	}
	series := seriesFromRollups(points, first, first+4*width, width, 5)
	if len(series) != 5 {
		t.Fatalf("series length = %d, want 5", len(series))
	}
	want := []string{"healthy", "degraded", "outage", "unknown", "unknown"}
	for i, state := range want {
		if series[i].State != state {
			t.Fatalf("bucket %d state = %q, want %q", i, series[i].State, state)
		}
		if series[i].BucketStart != first+int64(i)*width {
			t.Fatalf("bucket %d start = %d, want %d", i, series[i].BucketStart, first+int64(i)*width)
		}
	}
	if series[1].Availability == nil || *series[1].Availability != 50 {
		t.Fatalf("degraded availability = %v, want 50", series[1].Availability)
	}
	// A bucket with no checks carries no metrics and is not counted as downtime.
	if series[3].Availability != nil || series[3].P50MS != nil || series[3].P95MS != nil {
		t.Fatalf("unknown bucket must have no metrics: %#v", series[3])
	}
	if series[0].P50MS == nil || *series[0].P50MS != 100 || series[0].P95MS == nil || *series[0].P95MS != 120 {
		t.Fatalf("stored percentiles not surfaced: %#v", series[0])
	}
}

func TestSeriesFromRollupsRejectsDegenerateInput(t *testing.T) {
	if got := seriesFromRollups(nil, 100, 0, 900, 5); len(got) != 0 {
		t.Fatalf("reversed window should produce no points, got %d", len(got))
	}
	if got := seriesFromRollups(nil, 0, 900, 0, 5); len(got) != 0 {
		t.Fatalf("zero width should produce no points, got %d", len(got))
	}
	if got := seriesFromRollups(nil, 0, 900, 900, 0); len(got) != 0 {
		t.Fatalf("zero maxPoints should produce no points, got %d", len(got))
	}
}

func TestBucketStartForSnapsDownToBoundary(t *testing.T) {
	width := int64(900)
	for _, test := range []struct{ at, want int64 }{
		{1787313600, 1787313600},
		{1787313601, 1787313600},
		{1787314499, 1787313600},
		{1787314500, 1787314500},
		{-1, -900},
	} {
		if got := bucketStartFor(test.at, width); got != test.want {
			t.Fatalf("bucketStartFor(%d) = %d, want %d", test.at, got, test.want)
		}
	}
}

// Midnight must be a bucket boundary for every width, because the
// checks-completed-today figure is summed from buckets at or after it.
func TestMidnightIsABucketBoundaryForEveryWidth(t *testing.T) {
	midnight := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC).Unix()
	for _, width := range rollupBucketWidths {
		if bucketStartFor(midnight, width) != midnight {
			t.Fatalf("width %d does not align to midnight", width)
		}
	}
}

// The aligned window must cover exactly the documented range: MaxPoints
// buckets of Bucket width equals Window for every supported range.
func TestDashboardWindowCoversTheDocumentedRange(t *testing.T) {
	for _, raw := range []string{"24h", "7d", "30d"} {
		spec, err := parseDashboardRange(raw)
		if err != nil {
			t.Fatal(err)
		}
		if int64(spec.MaxPoints)*int64(spec.Bucket/time.Second) != int64(spec.Window/time.Second) {
			t.Fatalf("%s: %d buckets of %s do not span %s", raw, spec.MaxPoints, spec.Bucket, spec.Window)
		}
		for _, now := range []time.Time{
			time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),  // exactly on a boundary
			time.Date(2026, 8, 21, 12, 7, 30, 0, time.UTC), // mid-bucket
			time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC),   // midnight
		} {
			first, last, width := dashboardWindow(spec, now)
			if last >= now.Unix() {
				t.Fatalf("%s at %s: last bucket %d is not before now", raw, now, last)
			}
			if first < now.Add(-spec.Window).Unix() {
				t.Fatalf("%s at %s: first bucket %d precedes the window", raw, now, first)
			}
			if (last-first)/width+1 != int64(spec.MaxPoints) {
				t.Fatalf("%s at %s: window holds %d buckets, want %d", raw, now, (last-first)/width+1, spec.MaxPoints)
			}
		}
	}
}
