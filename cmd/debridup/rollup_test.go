package main

import (
	"context"
	"math"
	"math/rand"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// seedChecks writes raw authenticated checks without going through
// recordResult, then rebuilds rollups the way the backfill migration does.
func seedChecks(t *testing.T, a *app, monitors int, samples int, spacing time.Duration, now time.Time) {
	t.Helper()
	random := rand.New(rand.NewSource(99))
	tx, err := a.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for id := 1; id <= monitors; id++ {
		if _, err := tx.Exec(`INSERT INTO monitors(id,provider,name,created_at,updated_at) VALUES(?,'torbox',?,?,?)`,
			id, "monitor", now.Unix(), now.Unix()); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO monitor_states(monitor_id,current_state,state_since,last_raw_state,last_check_at) VALUES(?,?,?,?,?)`,
			id, stateHealthy, now.Unix(), stateHealthy, now.Unix()); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < samples; i++ {
			state := stateHealthy
			switch {
			case i%41 == 0:
				state = stateAPI
			case i%67 == 0:
				state = stateAuthFailed
			}
			checkedAt := now.Add(-time.Duration(i) * spacing).Unix()
			if _, err := tx.Exec(`INSERT INTO check_results(monitor_id,source,state,duration_ms,checked_at) VALUES(?,'authenticated',?,?,?)`,
				id, state, int64(random.Intn(800)+20), checkedAt); err != nil {
				t.Fatal(err)
			}
		}
		// Public checks must never reach the dashboard aggregates.
		if _, err := tx.Exec(`INSERT INTO check_results(monitor_id,source,state,duration_ms,checked_at) VALUES(?,'public','healthy',9999,?)`, id, now.Add(-time.Minute).Unix()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := rebuildRollups(context.Background(), a.db, nil); err != nil {
		t.Fatal(err)
	}
}

func rollupTestApp(t *testing.T) *app {
	t.Helper()
	a := testApp(t)
	if err := migrateDatabase(context.Background(), a.db); err != nil {
		t.Fatal(err)
	}
	return a
}

// TestRollupBucketsMatchDirectComputation is the correctness guarantee for the
// pre-aggregation: every stored bucket must equal what a direct calculation
// over its raw checks produces.
func TestRollupBucketsMatchDirectComputation(t *testing.T) {
	a := rollupTestApp(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	seedChecks(t, a, 3, 600, 5*time.Minute, now)

	rows, err := a.db.Query(`SELECT monitor_id,bucket_width,bucket_start,total,healthy,slowest_ms,p50_ms,p95_ms FROM check_rollups`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	checked := 0
	for rows.Next() {
		var monitorID, width, bucketStart, total, healthy, slowest int64
		var p50, p95 *int64
		if err := rows.Scan(&monitorID, &width, &bucketStart, &total, &healthy, &slowest, &p50, &p95); err != nil {
			t.Fatal(err)
		}

		rawRows, err := a.db.Query(`SELECT state,duration_ms FROM check_results
		WHERE monitor_id=? AND source='authenticated' AND checked_at>=? AND checked_at<?`, monitorID, bucketStart, bucketStart+width)
		if err != nil {
			t.Fatal(err)
		}
		var latencies []int64
		var wantTotal, wantHealthy, wantSlowest int64
		for rawRows.Next() {
			var state string
			var duration int64
			if err := rawRows.Scan(&state, &duration); err != nil {
				t.Fatal(err)
			}
			wantTotal++
			if state == stateHealthy {
				wantHealthy++
			}
			if duration > wantSlowest {
				wantSlowest = duration
			}
			latencies = append(latencies, duration)
		}
		rawRows.Close()

		if total != wantTotal || healthy != wantHealthy || slowest != wantSlowest {
			t.Fatalf("monitor %d width %d bucket %d: got total=%d healthy=%d slowest=%d, want %d/%d/%d",
				monitorID, width, bucketStart, total, healthy, slowest, wantTotal, wantHealthy, wantSlowest)
		}
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		wantP50 := nearestRank(latencies, .50)
		wantP95 := nearestRank(latencies, .95)
		if (p50 == nil) != (wantP50 == nil) || (p50 != nil && *p50 != *wantP50) {
			t.Fatalf("monitor %d bucket %d p50 = %v, want %v", monitorID, bucketStart, p50, wantP50)
		}
		if (p95 == nil) != (wantP95 == nil) || (p95 != nil && *p95 != *wantP95) {
			t.Fatalf("monitor %d bucket %d p95 = %v, want %v", monitorID, bucketStart, p95, wantP95)
		}
		checked++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no rollup buckets were produced")
	}
	t.Logf("verified %d rollup buckets against direct computation", checked)
}

// TestSnapshotAvailabilityAndSlowestAreExact pins the aggregates that the
// rollups must reproduce exactly, rather than approximate.
func TestSnapshotAvailabilityAndSlowestAreExact(t *testing.T) {
	a := rollupTestApp(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	seedChecks(t, a, 2, 400, 3*time.Minute, now)

	for _, raw := range []string{"24h", "7d", "30d"} {
		spec, _ := parseDashboardRange(raw)
		first, last, width := dashboardWindow(spec, now)
		got, err := a.dashboardSnapshot(context.Background(), spec, now)
		if err != nil {
			t.Fatal(err)
		}
		for _, provider := range got.Providers {
			var total, healthy, slowest int64
			rawRows, err := a.db.Query(`SELECT state,duration_ms FROM check_results
			WHERE monitor_id=? AND source='authenticated' AND checked_at>=? AND checked_at<?`,
				provider.ID, first, last+width)
			if err != nil {
				t.Fatal(err)
			}
			for rawRows.Next() {
				var state string
				var duration int64
				if err := rawRows.Scan(&state, &duration); err != nil {
					t.Fatal(err)
				}
				total++
				if state == stateHealthy {
					healthy++
				}
				if duration > slowest {
					slowest = duration
				}
			}
			rawRows.Close()
			if total == 0 {
				continue
			}
			wantAvailability := float64(healthy) / float64(total) * 100
			if provider.Availability == nil || *provider.Availability != wantAvailability {
				t.Fatalf("%s provider %d availability = %v, want %v", raw, provider.ID, provider.Availability, wantAvailability)
			}
			if provider.SlowestMS == nil || *provider.SlowestMS != slowest {
				t.Fatalf("%s provider %d slowest = %v, want %d", raw, provider.ID, provider.SlowestMS, slowest)
			}
		}
	}
}

// TestRecordResultKeepsRollupsCurrent covers the live path: a check written
// through recordResult must update its buckets in the same transaction.
func TestRecordResultKeepsRollupsCurrent(t *testing.T) {
	a := rollupTestApp(t)
	if _, err := a.db.Exec(`INSERT INTO monitors(id,provider,name,created_at,updated_at) VALUES(1,'torbox','TorBox',1,1)`); err != nil {
		t.Fatal(err)
	}
	m := monitor{ID: 1, Provider: "torbox", Name: "TorBox", FailureThreshold: 3, RecoveryThreshold: 2}
	now := time.Date(2026, 8, 21, 12, 3, 0, 0, time.UTC)

	for i, result := range []checkResult{
		{State: stateHealthy, DurationMS: 100, CheckedAt: now},
		{State: stateHealthy, DurationMS: 200, CheckedAt: now.Add(time.Minute)},
		{State: stateAPI, DurationMS: 500, CheckedAt: now.Add(2 * time.Minute)},
	} {
		if _, err := a.recordResult(m, "authenticated", result); err != nil {
			t.Fatalf("check %d: %v", i, err)
		}
	}
	// A public check must not appear in any bucket.
	if _, err := a.recordResult(m, "public", checkResult{State: stateHealthy, DurationMS: 7000, CheckedAt: now}); err != nil {
		t.Fatal(err)
	}

	width := int64(900)
	bucket := bucketStartFor(now.Unix(), width)
	var total, healthy, slowest int64
	var p50, p95 *int64
	if err := a.db.QueryRow(`SELECT total,healthy,slowest_ms,p50_ms,p95_ms FROM check_rollups WHERE monitor_id=1 AND bucket_width=? AND bucket_start=?`, width, bucket).
		Scan(&total, &healthy, &slowest, &p50, &p95); err != nil {
		t.Fatalf("rollup not maintained by recordResult: %v", err)
	}
	if total != 3 || healthy != 2 || slowest != 500 {
		t.Fatalf("total=%d healthy=%d slowest=%d, want 3/2/500", total, healthy, slowest)
	}
	if p50 == nil || *p50 != 200 || p95 == nil || *p95 != 500 {
		t.Fatalf("p50=%v p95=%v, want 200/500", p50, p95)
	}

	// Every configured width must be maintained, not just the smallest.
	for _, w := range rollupBucketWidths {
		var count int
		if err := a.db.QueryRow(`SELECT COUNT(*) FROM check_rollups WHERE monitor_id=1 AND bucket_width=?`, w).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("width %d has %d buckets, want 1", w, count)
		}
	}
}

// TestPruneHistoryKeepsRollupsConsistent covers retention: buckets wholly
// outside retention go, and a bucket straddling the cutoff is recomputed.
func TestPruneHistoryKeepsRollupsConsistent(t *testing.T) {
	a := rollupTestApp(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	seedChecks(t, a, 1, 500, time.Hour, now)

	cutoff := now.Add(-72 * time.Hour)
	if _, err := pruneHistory(context.Background(), a.db, cutoff); err != nil {
		t.Fatal(err)
	}

	rows, err := a.db.Query(`SELECT monitor_id,bucket_width,bucket_start,total,healthy FROM check_rollups`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var monitorID, width, bucketStart, total, healthy int64
		if err := rows.Scan(&monitorID, &width, &bucketStart, &total, &healthy); err != nil {
			t.Fatal(err)
		}
		var wantTotal, wantHealthy int64
		if err := a.db.QueryRow(`SELECT COUNT(*),SUM(CASE WHEN state='healthy' THEN 1 ELSE 0 END) FROM check_results
		WHERE monitor_id=? AND source='authenticated' AND checked_at>=? AND checked_at<?`,
			monitorID, bucketStart, bucketStart+width).Scan(&wantTotal, &wantHealthy); err != nil {
			t.Fatal(err)
		}
		if total != wantTotal || healthy != wantHealthy {
			t.Fatalf("bucket %d (width %d) has total=%d healthy=%d but raw rows give %d/%d",
				bucketStart, width, total, healthy, wantTotal, wantHealthy)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	// No bucket may survive entirely below the retention cutoff.
	for _, width := range rollupBucketWidths {
		var orphaned int
		if err := a.db.QueryRow(`SELECT COUNT(*) FROM check_rollups WHERE bucket_width=? AND bucket_start<?`,
			width, bucketStartFor(cutoff.Unix(), width)).Scan(&orphaned); err != nil {
			t.Fatal(err)
		}
		if orphaned != 0 {
			t.Fatalf("width %d kept %d buckets below the retention cutoff", width, orphaned)
		}
	}
}

func TestResetHistoryClearsRollups(t *testing.T) {
	a := rollupTestApp(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	seedChecks(t, a, 2, 100, 10*time.Minute, now)

	id := int64(1)
	if err := a.resetHistory(&id); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM check_rollups WHERE monitor_id=1`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("monitor 1 kept %d rollups after reset", remaining)
	}
	// The other monitor is untouched.
	var other int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM check_rollups WHERE monitor_id=2`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if other == 0 {
		t.Fatal("resetting one monitor cleared another monitor's rollups")
	}

	if err := a.resetHistory(nil); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM check_rollups`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("full reset left %d rollups", remaining)
	}
}

// TestWeightedPercentileTracksTheExactValue bounds the one figure the rollups
// approximate. Computing the exact window percentile needs every raw latency
// ordered, which measured slower than the pre-aggregation replaced; the
// per-bucket percentiles remain exact, and this pins how far the window-level
// summary may drift from a direct nearest-rank calculation.
func TestWeightedPercentileTracksTheExactValue(t *testing.T) {
	a := rollupTestApp(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	seedChecks(t, a, 3, 2000, time.Minute, now)

	worst := 0.0
	for _, raw := range []string{"24h", "7d", "30d"} {
		spec, _ := parseDashboardRange(raw)
		first, last, width := dashboardWindow(spec, now)
		got, err := a.dashboardSnapshot(context.Background(), spec, now)
		if err != nil {
			t.Fatal(err)
		}
		for _, provider := range got.Providers {
			rawRows, err := a.db.Query(`SELECT duration_ms FROM check_results
			WHERE monitor_id=? AND source='authenticated' AND checked_at>=? AND checked_at<?`,
				provider.ID, first, last+width)
			if err != nil {
				t.Fatal(err)
			}
			var latencies []int64
			for rawRows.Next() {
				var duration int64
				if err := rawRows.Scan(&duration); err != nil {
					t.Fatal(err)
				}
				latencies = append(latencies, duration)
			}
			rawRows.Close()
			if len(latencies) == 0 {
				continue
			}
			for _, probe := range []struct {
				name  string
				got   *int64
				exact *int64
			}{
				{"p50", provider.P50MS, nearestRank(latencies, .50)},
				{"p95", provider.P95MS, nearestRank(latencies, .95)},
			} {
				if probe.got == nil || probe.exact == nil {
					t.Fatalf("%s %s missing: got=%v exact=%v", raw, probe.name, probe.got, probe.exact)
				}
				deviation := math.Abs(float64(*probe.got-*probe.exact)) / float64(*probe.exact) * 100
				if deviation > worst {
					worst = deviation
				}
				if deviation > 15 {
					t.Fatalf("%s provider %d %s = %d, exact %d (%.1f%% off)",
						raw, provider.ID, probe.name, *probe.got, *probe.exact, deviation)
				}
			}
		}
	}
	t.Logf("worst window-percentile deviation from exact nearest-rank: %.2f%%", worst)
}

// TestMigrationFromEarlierSchemaBackfillsRollups covers the upgrade path: a
// database created before rollups existed must gain correct buckets, including
// the eligible column added by a later migration.
func TestMigrationFromEarlierSchemaBackfillsRollups(t *testing.T) {
	db, err := openDatabase(filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// Build the schema as it stood before rollups, and stop there.
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for version := 1; version <= 4; version++ {
		if err := applyMigration(ctx, tx, version); err != nil {
			t.Fatalf("migration %d: %v", version, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES(?,0)`, version); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Migration 1 creates the current schema, which already includes rollups;
	// drop them so this really starts from a pre-rollup database.
	if _, err := conn.ExecContext(ctx, `DROP TABLE IF EXISTS check_rollups`); err != nil {
		t.Fatal(err)
	}
	conn.Close()

	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`INSERT INTO monitors(id,provider,name,created_at,updated_at) VALUES(1,'torbox','TorBox',1,1)`); err != nil {
		t.Fatal(err)
	}
	for i, state := range []string{stateHealthy, stateHealthy, stateAuthFailed, stateAPI} {
		if _, err := db.Exec(`INSERT INTO check_results(monitor_id,source,state,duration_ms,checked_at) VALUES(1,'authenticated',?,?,?)`,
			state, 100+i*50, now.Add(-time.Duration(i)*time.Minute).Unix()); err != nil {
			t.Fatal(err)
		}
	}

	// Now upgrade the rest of the way.
	if err := migrateDatabase(ctx, db); err != nil {
		t.Fatalf("upgrade failed: %v", err)
	}

	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != currentSchemaVersion {
		t.Fatalf("schema version = %d, want %d", version, currentSchemaVersion)
	}

	var total, healthy, eligible int64
	if err := db.QueryRow(`SELECT SUM(total),SUM(healthy),SUM(eligible) FROM check_rollups WHERE bucket_width=900 AND monitor_id=1`).
		Scan(&total, &healthy, &eligible); err != nil {
		t.Fatalf("rollups were not backfilled: %v", err)
	}
	if total != 4 || healthy != 2 || eligible != 3 {
		t.Fatalf("backfilled total=%d healthy=%d eligible=%d, want 4/2/3", total, healthy, eligible)
	}
}
