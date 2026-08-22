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
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
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

func authenticatedRequest(t *testing.T, a *app, method, target string) *http.Request {
	t.Helper()
	data := []byte(strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
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
