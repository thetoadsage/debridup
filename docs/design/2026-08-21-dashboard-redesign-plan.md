# Dashboard Redesign and Runtime Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deliver a range-aware operations dashboard, safer runtime behavior, hardened secret handling, and automated release-safety checks without adding a frontend framework.

**Architecture:** Keep the existing single-binary Go application and embedded static frontend. Extract focused files within `cmd/debridup`, aggregate dashboard data in one authenticated endpoint, render it with browser-native modules, and preserve all existing management endpoints.

**Tech Stack:** Go 1.24, `net/http`, `database/sql`, `modernc.org/sqlite`, embedded HTML/CSS/JavaScript, Node's built-in test runner, Docker, GitHub Actions.

**Spec:** `docs/design/2026-08-21-dashboard-redesign.md`

## Global Constraints

- Preserve the current Go plus dependency-free browser architecture.
- Keep `GET /api/overview`, monitor, check, incident, notification, login, logout, and reset routes compatible.
- Support exactly `24h`, `7d`, and `30d` dashboard ranges.
- Cap every provider chart series at 96, 84, and 90 points respectively.
- Use UTC for stored timestamps and the checks-today boundary.
- Retain raw checks for 90 days by default.
- Permit only one active check per monitor and bound total check concurrency.
- Never embed encryption material in Compose, tracked configuration, documentation examples, logs, fixtures, or proposed change text.
- Keep private addresses, user-home paths, credentials, personal contact details, and prohibited attribution out of every tracked artifact and commit.
- Use generic providers and deterministic timestamps in tests.
- Commit after every independently testable task.

---

## File Map

- `cmd/debridup/main.go`: application wiring, existing domain behavior, route registration, and server lifecycle.
- `cmd/debridup/database.go`: SQLite DSN construction, connection verification, schema migrations, readiness, and retention pruning.
- `cmd/debridup/database_test.go`: connection, migration, readiness, and retention tests.
- `cmd/debridup/scheduler.go`: run claiming, per-monitor exclusion, bounded workers, and scheduler loop.
- `cmd/debridup/scheduler_test.go`: overlap and concurrency tests.
- `cmd/debridup/dashboard.go`: range definitions, response models, percentile and pulse aggregation, and dashboard HTTP handler.
- `cmd/debridup/dashboard_test.go`: range, aggregation, authorization, and response tests.
- `cmd/debridup/web/index.html`: dashboard landmarks, summary regions, tables, charts, drawer, and existing management dialogs.
- `cmd/debridup/web/app.css`: existing tokens plus responsive dashboard and drawer styling.
- `cmd/debridup/web/app.js`: application bootstrap and existing monitor/notification actions.
- `cmd/debridup/web/dashboard.mjs`: dashboard request state, rendering, refresh deduplication, visibility behavior, and range selection.
- `cmd/debridup/web/dashboard-model.mjs`: pure formatting and view-model transformations.
- `cmd/debridup/web/chart.mjs`: accessible pulse and latency chart rendering.
- `cmd/debridup/web/drawer.mjs`: provider-detail drawer focus and interaction behavior.
- `cmd/debridup/web/dashboard-model.test.mjs`: pure browser-module tests with synthetic fixtures.
- `docker-entrypoint.sh`: inherited descriptor handling for root-readable secrets.
- `docker-compose.yml`: capability and privilege hardening.
- `Dockerfile`: readiness health check and supported runtime tooling.
- `unraid/debridup.xml`: secret-only host path and hardened runtime arguments.
- `unraid/README.md`: safe key creation, ownership, backup, and migration guidance.
- `scripts/check-release-safety.sh`: tracked-content, metadata, and proposed-change scanner.
- `.github/workflows/test.yml`: formatting, tests, static analysis, vulnerability, container, and release-safety jobs.
- `.github/workflows/container.yml`: publish job dependency on verified checks.
- `README.md`: dashboard, range, retention, readiness, and deployment documentation.

---

### Task 1: SQLite Lifecycle, Migrations, and Readiness

**Files:**
- Create: `cmd/debridup/database.go`
- Create: `cmd/debridup/database_test.go`
- Modify: `cmd/debridup/main.go:108-247`
- Modify: `cmd/debridup/main.go:303-329`

**Interfaces:**
- Produces: `openDatabase(path string) (*sql.DB, error)`
- Produces: `migrateDatabase(ctx context.Context, db *sql.DB) error`
- Produces: `databaseReady(ctx context.Context, db *sql.DB) error`
- Produces: `app.readiness(http.ResponseWriter, *http.Request)`
- Consumes: existing schema and `app.db`

- [ ] **Step 1: Write failing connection and readiness tests**

```go
func TestOpenDatabaseAppliesConnectionPragmas(t *testing.T) {
	db, err := openDatabase(filepath.Join(t.TempDir(), "test.db"))
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { db.Close() })

	db.SetMaxOpenConns(2)
	for i := 0; i < 2; i++ {
		conn, err := db.Conn(context.Background())
		if err != nil { t.Fatal(err) }
		var foreignKeys, busyTimeout int
		if err := conn.QueryRowContext(context.Background(), "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil { t.Fatal(err) }
		if err := conn.QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil { t.Fatal(err) }
		conn.Close()
		if foreignKeys != 1 || busyTimeout != 5000 { t.Fatalf("foreign_keys=%d busy_timeout=%d", foreignKeys, busyTimeout) }
	}
}

func TestReadyzReturnsServiceUnavailableWhenDatabaseIsClosed(t *testing.T) {
	a := testApp(t)
	a.db.Close()
	rr := httptest.NewRecorder()
	a.readiness(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusServiceUnavailable { t.Fatalf("status=%d", rr.Code) }
}
```

- [ ] **Step 2: Run the focused tests and verify failure**

Run: `go test ./cmd/debridup -run 'TestOpenDatabaseAppliesConnectionPragmas|TestReadyzReturnsServiceUnavailableWhenDatabaseIsClosed' -count=1`

Expected: FAIL because `openDatabase` and `readiness` do not exist.

- [ ] **Step 3: Add connection-safe database setup**

Implement `openDatabase` with a URI DSN whose query includes:

```go
values := url.Values{}
values.Add("_pragma", "foreign_keys(1)")
values.Add("_pragma", "busy_timeout(5000)")
values.Add("_pragma", "journal_mode(WAL)")
dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: values.Encode()}).String()
db, err := sql.Open("sqlite", dsn)
```

Move schema creation and the provider-constraint migration into `database.go`. Add a `schema_migrations(version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)` table and execute each missing migration inside a transaction. Make the current schema version `1`; migration `1` creates the current schema idempotently.

Add:

```go
func databaseReady(ctx context.Context, db *sql.DB) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var one int
	return db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}
```

Register `GET /readyz` without authentication and return `200 {"ok":true}` or `503 {"code":"database_unavailable","error":"database is not ready"}`.

- [ ] **Step 4: Wire startup through the new functions**

Replace `sql.Open` and `a.migrate()` in `main` with `openDatabase` and `migrateDatabase`. Remove the migrated database methods from `main.go`. Keep `/healthz` as liveness.

- [ ] **Step 5: Run tests and static analysis**

Run: `gofmt -w cmd/debridup/database.go cmd/debridup/database_test.go cmd/debridup/main.go`

Run: `go test ./cmd/debridup -count=1`

Run: `go vet ./...`

Expected: all commands pass.

- [ ] **Step 6: Commit the database foundation**

```bash
git add cmd/debridup/database.go cmd/debridup/database_test.go cmd/debridup/main.go
git commit -m "refactor: make database setup connection safe"
```

---

### Task 2: Retention Pruning

**Files:**
- Modify: `cmd/debridup/database.go`
- Modify: `cmd/debridup/database_test.go`
- Modify: `cmd/debridup/main.go:108-139`
- Modify: `README.md`

**Interfaces:**
- Produces: `parseRetention(raw string) (time.Duration, error)`
- Produces: `pruneHistory(ctx context.Context, db *sql.DB, cutoff time.Time) (int64, error)`
- Produces: `app.retentionWorker(ctx context.Context, retention time.Duration)`
- Consumes: `app.db` and `app.logger`

- [ ] **Step 1: Write failing retention tests**

```go
func TestParseRetentionDefaultsToNinetyDays(t *testing.T) {
	got, err := parseRetention("")
	if err != nil { t.Fatal(err) }
	if got != 90*24*time.Hour { t.Fatalf("retention=%s", got) }
}

func TestPruneHistoryRemovesOnlyExpiredChecks(t *testing.T) {
	db := migratedTestDB(t)
	monitorID := insertSyntheticMonitor(t, db, "Provider A")
	insertSyntheticCheck(t, db, monitorID, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	insertSyntheticCheck(t, db, monitorID, time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC))
	deleted, err := pruneHistory(context.Background(), db, time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC))
	if err != nil { t.Fatal(err) }
	if deleted != 1 { t.Fatalf("deleted=%d", deleted) }
}
```

- [ ] **Step 2: Run tests and verify failure**

Run: `go test ./cmd/debridup -run 'TestParseRetention|TestPruneHistory' -count=1`

Expected: FAIL because the retention functions do not exist.

- [ ] **Step 3: Implement bounded retention**

Accept `DEBRIDUP_HISTORY_RETENTION` as a Go duration with a minimum of `24h`; default to `2160h`. Delete expired `check_results` in a transaction. Do not delete incidents or incident events in this task because those records remain useful after raw samples expire.

Run pruning once at startup and then after each UTC date change. A prune failure logs `history prune failed` and does not stop monitoring.

- [ ] **Step 4: Document the setting and verify**

Add the exact default, minimum, and behavior to the environment table in `README.md`.

Run: `gofmt -w cmd/debridup/database.go cmd/debridup/database_test.go cmd/debridup/main.go`

Run: `go test ./cmd/debridup -count=1`

Expected: PASS.

- [ ] **Step 5: Commit retention**

```bash
git add cmd/debridup/database.go cmd/debridup/database_test.go cmd/debridup/main.go README.md
git commit -m "feat: bound raw check history"
```

---

### Task 3: Scheduler Exclusion and Concurrency Bound

**Files:**
- Create: `cmd/debridup/scheduler.go`
- Create: `cmd/debridup/scheduler_test.go`
- Modify: `cmd/debridup/main.go:43-51`
- Modify: `cmd/debridup/main.go:423-470`

**Interfaces:**
- Produces: `newRunCoordinator(limit int) *runCoordinator`
- Produces: `(*runCoordinator).Claim(id int64, now time.Time, interval time.Duration) bool`
- Produces: `(*runCoordinator).Release(id int64)`
- Produces: `app.runDueMonitorsAt(now time.Time)`
- Consumes: `app.monitors()` and `app.runMonitor(monitor)`

- [ ] **Step 1: Write failing coordinator tests**

```go
func TestRunCoordinatorRejectsOverlap(t *testing.T) {
	c := newRunCoordinator(2)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if !c.Claim(1, now, 15*time.Second) { t.Fatal("first claim rejected") }
	if c.Claim(1, now.Add(20*time.Second), 15*time.Second) { t.Fatal("overlap accepted") }
	c.Release(1)
	if !c.Claim(1, now.Add(20*time.Second), 15*time.Second) { t.Fatal("claim after release rejected") }
}

func TestRunCoordinatorBoundsGlobalConcurrency(t *testing.T) {
	c := newRunCoordinator(1)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if !c.Claim(1, now, time.Second) { t.Fatal("first claim rejected") }
	if c.Claim(2, now, time.Second) { t.Fatal("global limit exceeded") }
}
```

- [ ] **Step 2: Run tests and verify failure**

Run: `go test ./cmd/debridup -run 'TestRunCoordinator' -count=1`

Expected: FAIL because `runCoordinator` does not exist.

- [ ] **Step 3: Implement the coordinator**

Use one mutex-protected structure:

```go
type runCoordinator struct {
	mu       sync.Mutex
	lastRuns map[int64]time.Time
	inFlight map[int64]struct{}
	active   int
	limit    int
}
```

`Claim` returns false when the monitor is not due, is already in flight, or the global limit is reached. It sets `lastRuns`, records the monitor in `inFlight`, and increments `active` atomically. `Release` removes the monitor and decrements `active` exactly once.

Default `DEBRIDUP_MAX_CONCURRENT_CHECKS` to `4`, accept integers from `1` through `32`, and fail startup on invalid values.

- [ ] **Step 4: Wire scheduler execution**

Move `scheduler`, `runDueMonitors`, and `runMonitor` into `scheduler.go`. Wrap every launched run with:

```go
go func(m monitor) {
	defer a.runs.Release(m.ID)
	a.runMonitor(m)
}(m)
```

- [ ] **Step 5: Run race-enabled tests**

Run: `gofmt -w cmd/debridup/scheduler.go cmd/debridup/scheduler_test.go cmd/debridup/main.go`

Run: `go test -race ./cmd/debridup -run 'TestRunCoordinator|TestProviderDefinitions' -count=1`

Expected: PASS without race reports.

- [ ] **Step 6: Commit scheduler protection**

```bash
git add cmd/debridup/scheduler.go cmd/debridup/scheduler_test.go cmd/debridup/main.go
git commit -m "fix: prevent overlapping provider checks"
```

---

### Task 4: Dashboard Ranges and Aggregation Primitives

**Files:**
- Create: `cmd/debridup/dashboard.go`
- Create: `cmd/debridup/dashboard_test.go`

**Interfaces:**
- Produces: `parseDashboardRange(raw string) (dashboardRange, error)`
- Produces: `nearestRank(values []int64, percentile float64) *int64`
- Produces: `pulseState(samples []dashboardSample) string`
- Produces: `aggregateSeries(samples []dashboardSample, spec dashboardRange, start, end time.Time) []dashboardPoint`
- Produces: input type `dashboardSample` and response types `dashboardResponse`, `dashboardSummary`, `dashboardProvider`, `dashboardPoint`, `dashboardIncident`

- [ ] **Step 1: Write failing range and percentile tests**

```go
func TestParseDashboardRange(t *testing.T) {
	cases := []struct { raw string; window, bucket time.Duration; points int }{
		{"24h", 24*time.Hour, 15*time.Minute, 96},
		{"7d", 7*24*time.Hour, 2*time.Hour, 84},
		{"30d", 30*24*time.Hour, 8*time.Hour, 90},
	}
	for _, tc := range cases {
		got, err := parseDashboardRange(tc.raw)
		if err != nil { t.Fatal(err) }
		if got.Window != tc.window || got.Bucket != tc.bucket || got.MaxPoints != tc.points { t.Fatalf("%s: %#v", tc.raw, got) }
	}
	if _, err := parseDashboardRange("1y"); err == nil { t.Fatal("invalid range accepted") }
}

func TestNearestRankDoesNotMutateInput(t *testing.T) {
	input := []int64{400, 100, 300, 200}
	got := nearestRank(input, .95)
	if got == nil || *got != 400 { t.Fatalf("p95=%v", got) }
	if !slices.Equal(input, []int64{400, 100, 300, 200}) { t.Fatalf("input mutated: %v", input) }
}
```

- [ ] **Step 2: Write failing pulse and series tests**

```go
func TestPulseState(t *testing.T) {
	if got := pulseState(nil); got != "unknown" { t.Fatalf("empty=%s", got) }
	if got := pulseState([]dashboardSample{{State: stateHealthy}}); got != "healthy" { t.Fatalf("healthy=%s", got) }
	if got := pulseState([]dashboardSample{{State: stateHealthy}, {State: stateAPI}}); got != "degraded" { t.Fatalf("mixed=%s", got) }
	if got := pulseState([]dashboardSample{{State: stateAPI}, {State: stateConnection}}); got != "outage" { t.Fatalf("failed=%s", got) }
}

func TestAggregateSeriesProducesBoundedChronologicalBuckets(t *testing.T) {
	end := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	spec, _ := parseDashboardRange("24h")
	points := aggregateSeries(syntheticDaySamples(end), spec, end.Add(-spec.Window), end)
	if len(points) > spec.MaxPoints { t.Fatalf("points=%d", len(points)) }
	for i := 1; i < len(points); i++ {
		if points[i].BucketStart <= points[i-1].BucketStart { t.Fatal("points not chronological") }
	}
}
```

- [ ] **Step 3: Implement the range and response model**

Use explicit JSON tags in camel case. The top-level response is:

```go
type dashboardResponse struct {
	GeneratedAt int64               `json:"generatedAt"`
	Range       string              `json:"range"`
	Summary     dashboardSummary    `json:"summary"`
	Providers   []dashboardProvider `json:"providers"`
	Incidents   []dashboardIncident `json:"incidents"`
}
```

Each `dashboardProvider` includes `ID`, `Name`, `Provider`, `State`, `StateSince`, `LastCheck`, `Availability`, `P50MS`, `P95MS`, `SlowestMS`, and `Series`. Each `dashboardPoint` includes `BucketStart`, `State`, `Availability`, `P50MS`, and `P95MS`.

- [ ] **Step 4: Implement pure aggregation**

Copy latency input before sorting. Use `ceil(percentile * len(values)) - 1` for nearest rank. Ignore samples whose source is not `authenticated`. Treat empty buckets as `unknown`, all-healthy buckets as `healthy`, all-failed buckets as `outage`, and mixed buckets as `degraded`. Do not count unknown buckets as downtime.

- [ ] **Step 5: Run focused and full tests**

Run: `gofmt -w cmd/debridup/dashboard.go cmd/debridup/dashboard_test.go`

Run: `go test ./cmd/debridup -run 'TestParseDashboardRange|TestNearestRank|TestPulseState|TestAggregateSeries' -count=1`

Run: `go test ./cmd/debridup -count=1`

Expected: PASS.

- [ ] **Step 6: Commit aggregation primitives**

```bash
git add cmd/debridup/dashboard.go cmd/debridup/dashboard_test.go
git commit -m "feat: add dashboard aggregation primitives"
```

---

### Task 5: Consolidated Dashboard Query and Endpoint

**Files:**
- Modify: `cmd/debridup/dashboard.go`
- Modify: `cmd/debridup/dashboard_test.go`
- Modify: `cmd/debridup/main.go:303-329`

**Interfaces:**
- Produces: `app.dashboardSnapshot(ctx context.Context, spec dashboardRange, now time.Time) (dashboardResponse, error)`
- Produces: `app.dashboard(http.ResponseWriter, *http.Request)`
- Consumes: Task 4 response types and aggregation functions

- [ ] **Step 1: Write a failing authenticated endpoint test**

```go
func TestDashboardEndpointReturnsConsolidatedSnapshot(t *testing.T) {
	a := testApp(t)
	seedDashboardFixture(t, a.db)
	req := authenticatedRequest(t, a, http.MethodGet, "/api/dashboard?range=24h")
	rr := httptest.NewRecorder()
	a.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK { t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String()) }
	var got dashboardResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil { t.Fatal(err) }
	if got.Range != "24h" || len(got.Providers) != 3 { t.Fatalf("snapshot=%#v", got) }
}

func TestDashboardEndpointRejectsInvalidRange(t *testing.T) {
	a := testApp(t)
	req := authenticatedRequest(t, a, http.MethodGet, "/api/dashboard?range=1y")
	rr := httptest.NewRecorder()
	a.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest { t.Fatalf("status=%d", rr.Code) }
	if !strings.Contains(rr.Body.String(), `"code":"invalid_range"`) { t.Fatalf("body=%s", rr.Body.String()) }
}
```

- [ ] **Step 2: Run tests and verify failure**

Run: `go test ./cmd/debridup -run 'TestDashboardEndpoint' -count=1`

Expected: FAIL because the route is not registered.

- [ ] **Step 3: Implement one-snapshot loading**

Begin one read-only transaction. Load monitors and states in one query, authenticated checks since the selected cutoff in one query, and intersecting incidents in one query. Group rows by monitor in Go and call Task 4 aggregation functions. Do not issue a query inside a provider loop.

Calculate:

- `overallState`: `outage` when any provider is in a confirmed failure state, `degraded` when any provider lacks current data, otherwise `healthy`.
- `providersOnline`: enabled providers whose current state is `healthy`.
- `activeIncidents`: unresolved incident count.
- `checksToday`: authenticated checks at or after the current UTC midnight.

Return non-nil empty arrays when no providers or incidents exist.

- [ ] **Step 4: Add stable API errors and the route**

Add:

```go
type apiError struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}
```

Register `GET /api/dashboard` through `a.auth`. Use `invalid_range` for a bad range and `dashboard_unavailable` for snapshot errors. Log the internal error without query text or secret-bearing values.

- [ ] **Step 5: Verify authorization, empty data, and bounded payloads**

Add tests for unauthenticated `401`, an empty database, each supported range, non-nil arrays, chronological series, and maximum point counts.

Run: `gofmt -w cmd/debridup/dashboard.go cmd/debridup/dashboard_test.go cmd/debridup/main.go`

Run: `go test ./cmd/debridup -run 'TestDashboard' -count=1`

Run: `go test -race ./cmd/debridup -count=1`

Expected: PASS.

- [ ] **Step 6: Commit the endpoint**

```bash
git add cmd/debridup/dashboard.go cmd/debridup/dashboard_test.go cmd/debridup/main.go
git commit -m "feat: serve consolidated dashboard data"
```

---

### Task 6: Dashboard Structure and Responsive Visual System

**Files:**
- Modify: `cmd/debridup/web/index.html`
- Modify: `cmd/debridup/web/app.css`
- Modify: `cmd/debridup/main_test.go`

**Interfaces:**
- Produces: stable element IDs `range-controls`, `summary`, `provider-pulse`, `provider-table-body`, `latency-chart`, `incidents`, `provider-drawer`, `dashboard-status`
- Consumes: existing monitor and notification dialogs

- [ ] **Step 1: Write a failing embedded-asset structure test**

```go
func TestDashboardHTMLContainsAccessibleLandmarks(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil { t.Fatal(err) }
	html := string(b)
	for _, required := range []string{
		`<nav aria-label="Primary">`, `id="range-controls"`, `id="summary"`,
		`id="provider-pulse"`, `id="provider-table-body"`, `id="latency-chart"`,
		`id="provider-drawer"`, `aria-live="polite"`,
	} {
		if !strings.Contains(html, required) { t.Errorf("missing %s", required) }
	}
}
```

- [ ] **Step 2: Run the test and verify failure**

Run: `go test ./cmd/debridup -run TestDashboardHTMLContainsAccessibleLandmarks -count=1`

Expected: FAIL because the new landmarks do not exist.

- [ ] **Step 3: Replace the dashboard shell while preserving management dialogs**

Create a full labeled sidebar and these ordered regions:

1. Page heading, last-updated status, refresh, and range controls.
2. Four summary cards.
3. Provider pulse timeline.
4. Provider table.
5. Latency comparison chart and recent incidents.
6. Existing provider and notification settings panels.
7. Provider detail drawer with an overlay, close button, heading, metrics, and incident list.

Use semantic `nav`, `main`, `section`, `table`, `button`, and `dialog`-equivalent drawer attributes. Every status includes visible text.

- [ ] **Step 4: Extend existing CSS tokens and responsive rules**

Keep the current color values as the source of truth. Add layout variables for sidebar width, content maximum, panel gap, and drawer width. Implement:

- Desktop: labeled sidebar, four-column summary, table, chart plus incident split.
- Tablet: two-column summary and full-width chart/incident stack.
- Mobile: top navigation, single-column summary, horizontally scrollable provider table, and full-screen drawer sheet.
- `:focus-visible` outlines and `prefers-reduced-motion` rules.
- Skeleton blocks that preserve final region dimensions.

- [ ] **Step 5: Verify the embedded page and existing tests**

Run: `go test ./cmd/debridup -run 'TestDashboardHTML|TestHealthz' -count=1`

Run: `go test ./cmd/debridup -count=1`

Expected: PASS.

- [ ] **Step 6: Commit the visual structure**

```bash
git add cmd/debridup/web/index.html cmd/debridup/web/app.css cmd/debridup/main_test.go
git commit -m "feat: add responsive operations dashboard shell"
```

---

### Task 7: Dashboard State, Charts, and Provider Drawer

**Files:**
- Create: `cmd/debridup/web/dashboard.mjs`
- Create: `cmd/debridup/web/dashboard-model.mjs`
- Create: `cmd/debridup/web/chart.mjs`
- Create: `cmd/debridup/web/drawer.mjs`
- Create: `cmd/debridup/web/dashboard-model.test.mjs`
- Modify: `cmd/debridup/web/app.js`
- Modify: `cmd/debridup/web/index.html`

**Interfaces:**
- Produces: `createDashboardModel(payload, now)`
- Produces: `renderPulse(providers)` and `renderLatencyChart(providers)`
- Produces: `createProviderDrawer(root)` with `open(provider, trigger)` and `close()`
- Produces: `startDashboard({api, document, window})`
- Consumes: `GET /api/dashboard?range=<range>` and Task 6 element IDs

- [ ] **Step 1: Write failing pure module tests**

```js
import test from 'node:test';
import assert from 'node:assert/strict';
import { createDashboardModel, formatLatency } from './dashboard-model.mjs';

test('creates a stable empty model', () => {
  const model = createDashboardModel({generatedAt: 1787320800, range: '24h', summary: {}, providers: [], incidents: []}, 1787320830000);
  assert.deepEqual(model.providers, []);
  assert.equal(model.stale, false);
});

test('formats missing and measured latency', () => {
  assert.equal(formatLatency(null), '—');
  assert.equal(formatLatency(138), '138 ms');
});
```

- [ ] **Step 2: Run the module tests and verify failure**

Run: `node --test cmd/debridup/web/dashboard-model.test.mjs`

Expected: FAIL because the modules do not exist.

- [ ] **Step 3: Implement deterministic view-model helpers**

Normalize absent arrays to empty arrays, format percentages to two decimals, format latency in milliseconds, calculate response age from injected `now`, and mark responses stale after 90 seconds. Keep all HTML escaping centralized and test it with `<`, `>`, `&`, single quote, and double quote inputs.

- [ ] **Step 4: Implement accessible pulse and latency charts**

Render the pulse as labeled provider rows with one button per bucket and text in each bucket's accessible name. Render the latency chart as SVG paths generated from server points, with visible axes, units, legends, and a neighboring text summary table. Return explicit empty-chart markup when fewer than two measured points exist.

- [ ] **Step 5: Implement refresh and failure behavior**

`startDashboard` performs one dashboard request per refresh. Use an `AbortController` to cancel an obsolete refresh, disable duplicate manual refreshes, pause the timer while `document.hidden` is true, and refresh immediately on visibility restoration. Preserve the last successful model on later failure and show its age with a retry button.

Range buttons update `range`, set `aria-pressed`, and refresh the complete dashboard. The default range is `24h`.

- [ ] **Step 6: Implement provider drawer focus behavior**

On open, capture the trigger, populate provider metrics and incidents, reveal the overlay, set `aria-hidden="false"`, and focus the close button. Trap Tab and Shift+Tab within the drawer. Escape and overlay click close it. Closing restores focus to the captured trigger.

- [ ] **Step 7: Integrate without breaking settings**

Change the page script to `type="module"`. Keep existing monitor and notification form behavior in `app.js`, import `startDashboard`, and replace the old multi-request `refresh` implementation. Monitor mutations call the single dashboard refresh after success.

- [ ] **Step 8: Run module and Go regression tests**

Run: `node --test cmd/debridup/web/dashboard-model.test.mjs`

Run: `go test ./cmd/debridup -count=1`

Expected: PASS.

- [ ] **Step 9: Commit interactive dashboard behavior**

```bash
git add cmd/debridup/web/app.js cmd/debridup/web/index.html cmd/debridup/web/dashboard.mjs cmd/debridup/web/dashboard-model.mjs cmd/debridup/web/chart.mjs cmd/debridup/web/drawer.mjs cmd/debridup/web/dashboard-model.test.mjs
git commit -m "feat: render range-aware provider dashboard"
```

---

### Task 8: Secret Descriptor and Container Hardening

**Files:**
- Modify: `cmd/debridup/main.go:147-162`
- Modify: `cmd/debridup/main_test.go`
- Modify: `docker-entrypoint.sh`
- Modify: `docker-compose.yml`
- Modify: `Dockerfile`
- Modify: `unraid/debridup.xml`
- Modify: `unraid/README.md`

**Interfaces:**
- Produces: `DEBRIDUP_ENCRYPTION_KEY_FD` runtime input
- Consumes: existing file-based key input and PUID/PGID privilege drop

- [ ] **Step 1: Write a failing descriptor-key test**

```go
func TestLoadKeyReadsInheritedDescriptor(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { r.Close() })
	encoded := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, chacha20poly1305.KeySize))
	if _, err := io.WriteString(w, encoded); err != nil { t.Fatal(err) }
	w.Close()
	t.Setenv("DEBRIDUP_ENCRYPTION_KEY", "")
	t.Setenv("DEBRIDUP_ENCRYPTION_KEY_FILE", "")
	t.Setenv("DEBRIDUP_ENCRYPTION_KEY_FD", strconv.Itoa(int(r.Fd())))
	got, err := loadKey()
	if err != nil { t.Fatal(err) }
	if !bytes.Equal(got, bytes.Repeat([]byte{7}, chacha20poly1305.KeySize)) { t.Fatal("wrong key") }
}
```

- [ ] **Step 2: Run the test and verify failure**

Run: `go test ./cmd/debridup -run TestLoadKeyReadsInheritedDescriptor -count=1`

Expected: FAIL because descriptor input is not supported.

- [ ] **Step 3: Add descriptor input with strict precedence**

`loadKey` checks the descriptor first, then the file path, then the direct development variable. Parse the descriptor as a non-negative integer, read at most 4 KiB, close the descriptor after reading, and return a generic invalid-key error that contains no input value.

Change the entrypoint from exporting key content to:

```sh
exec 3<"$DEBRIDUP_ENCRYPTION_KEY_FILE"
export DEBRIDUP_ENCRYPTION_KEY_FD=3
unset DEBRIDUP_ENCRYPTION_KEY_FILE
```

Keep the root-readable file open while `su-exec` drops privileges and starts the application.

- [ ] **Step 4: Harden Compose and the container health check**

Add:

```yaml
cap_drop:
  - ALL
security_opt:
  - no-new-privileges:true
```

Point the image health check at `/readyz`. Keep the root filesystem read-only and `/tmp` as tmpfs.

- [ ] **Step 5: Separate the Unraid secret path**

Change the default key host path to `/mnt/cache/appdata/debridup-secrets/encryption_key`. Update the guide to create the data directory as `99:100`, create the secret-only directory as `root:root` mode `0700`, and create the key as `root:root` mode `0400`. Add capability drop and no-new-privileges flags to `ExtraParams`.

- [ ] **Step 6: Verify tests and a local image**

Run: `gofmt -w cmd/debridup/main.go cmd/debridup/main_test.go`

Run: `go test ./cmd/debridup -count=1`

Run: `docker build -t debridup:test .`

Run: `docker inspect debridup:test --format '{{json .Config.Healthcheck.Test}}'`

Expected: tests pass, build succeeds, and the health command contains `/readyz`.

- [ ] **Step 7: Commit deployment hardening**

```bash
git add cmd/debridup/main.go cmd/debridup/main_test.go docker-entrypoint.sh docker-compose.yml Dockerfile unraid/debridup.xml unraid/README.md
git commit -m "security: isolate encryption material"
```

---

### Task 9: Continuous Integration and Release-Safety Gate

**Files:**
- Create: `scripts/check-release-safety.sh`
- Create: `.github/workflows/test.yml`
- Modify: `.github/workflows/container.yml`

**Interfaces:**
- Produces: `scripts/check-release-safety.sh [base-ref]`
- Consumes: optional `PRIVATE_PATTERNS` and `CHANGE_TEXT` environment variables

- [ ] **Step 1: Write the release scanner with self-tests**

The script must use `set -eu`, enumerate working-tree files with `git ls-files -z`, scan staged content with `git grep --cached`, and reject:

- RFC1918 addresses.
- Windows, macOS, and Linux user-home paths.
- common credential assignments and private-key headers.
- ordinary personal email addresses while allowing repository-hosted no-reply addresses.
- runtime-constructed prohibited markers so their plain text is not stored in the script.
- newline-delimited values supplied through `PRIVATE_PATTERNS`.

Construct the built-in markers without storing their plain text:

```sh
marker_one="$(printf '\143\157\144\145\170')"
marker_two="$(printf '\146\151\147\155\141')"
```

Scan `git log --format='%s%n%b' "${base_ref}..HEAD"` when a base ref is supplied, plus `CHANGE_TEXT`. If image files are tracked, run `exiftool -json` and scan its textual output with the same patterns.

Add a `--self-test` mode that initializes a temporary repository, proves a neutral file passes, then proves one synthetic private address, home path, credential assignment, email, and prohibited marker each fail.

- [ ] **Step 2: Run scanner self-tests**

Run: `sh scripts/check-release-safety.sh --self-test`

Expected: PASS with `release-safety self-test passed` and no sample values printed.

- [ ] **Step 3: Add the pull-request workflow**

Create `.github/workflows/test.yml` for pull requests and pushes to `main`. Grant only `contents: read`. Use Go `1.24.x` and Node `22`. Run:

```text
gofmt verification
go test ./...
go test -race ./...
go vet ./...
node --test cmd/debridup/web/dashboard-model.test.mjs
go install golang.org/x/vuln/cmd/govulncheck@v1.7.0
govulncheck ./...
docker build -t debridup:test .
sh scripts/check-release-safety.sh "$BASE_SHA"
```

Install `libimage-exiftool-perl` before the release scan. Supply the proposed title and description through `CHANGE_TEXT`. Supply any repository-specific deny list through the encrypted `PRIVATE_PATTERNS` value.

- [ ] **Step 4: Gate image publication on verification**

Make `container.yml` call the reusable verification workflow or duplicate only the required test job before `publish`. The publish job uses `needs: verify` and cannot run when verification fails.

- [ ] **Step 5: Run all local checks**

Run: `gofmt -l .`

Run: `go test ./...`

Run: `go vet ./...`

Run: `node --test cmd/debridup/web/dashboard-model.test.mjs`

Run: `sh scripts/check-release-safety.sh HEAD~1`

Expected: no formatting output and all checks pass.

- [ ] **Step 6: Commit automation**

```bash
git add scripts/check-release-safety.sh .github/workflows/test.yml .github/workflows/container.yml
git commit -m "ci: verify tests security and release safety"
```

---

### Task 10: Documentation, Integrated Verification, and Publication Preview

**Files:**
- Modify: `README.md`
- Modify: `unraid/README.md`
- Modify: `docs/design/2026-08-21-dashboard-redesign.md` only if implementation changes an approved interface

**Interfaces:**
- Consumes: every prior task's finished behavior
- Produces: reviewed branch, neutral commit history, and sanitized proposed title and description

- [ ] **Step 1: Update user-facing documentation**

Document the dashboard ranges, p50/p95 definitions, pulse-state meanings, 90-day retention default, concurrency setting, `/healthz` versus `/readyz`, file-descriptor secret handoff, Compose hardening, and safe upgrade steps for moving an existing key to the secret-only directory.

- [ ] **Step 2: Run the complete automated suite**

Run: `go test ./...`

Run: `go test -race ./...`

Run: `go vet ./...`

Run: `node --test cmd/debridup/web/dashboard-model.test.mjs`

Run: `docker build -t debridup:test .`

Run: `sh scripts/check-release-safety.sh main`

Expected: every command passes.

- [ ] **Step 3: Verify the main user journey in a disposable container**

Start the image with a generated one-time key, temporary data volume, synthetic administrator password, port assigned by the container runtime, dropped capabilities, and no-new-privileges. Verify:

1. `/healthz` and `/readyz` return `200`.
2. Login succeeds with the synthetic password.
3. Empty dashboard, provider creation, manual test, populated charts, range changes, provider drawer, settings, and logout work.
4. Keyboard focus order, Escape close, reduced motion, tablet, and mobile layouts work.
5. A stopped database or forced request error shows a safe stale/error state without exposing internals.

Destroy the temporary container, key, and volume after verification.

- [ ] **Step 4: Review the branch diff and commit metadata**

Run: `git diff --check main...HEAD`

Run: `git log --format='%h %s' main..HEAD`

Run: `sh scripts/check-release-safety.sh main`

Expected: clean diff, neutral commit subjects, and a passing safety scan.

- [ ] **Step 5: Commit documentation**

```bash
git add README.md unraid/README.md docs/design/2026-08-21-dashboard-redesign.md
git commit -m "docs: explain dashboard and secure deployment"
```

- [ ] **Step 6: Prepare a sanitized publication preview**

Use this neutral title:

```text
Redesign monitoring dashboard and harden runtime behavior
```

The proposed description summarizes dashboard ranges, consolidated API, scheduling safeguards, secret isolation, and automated verification. It contains no live service values, screenshots, private paths, credentials, personal information, or tool attribution. Run the release scanner against the exact title and description before requesting publication approval.
