package main

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// benchmarkLoad seeds a realistic installation: providers checked at the
// default 60s interval across the full default retention window.
func benchmarkLoad(tb testing.TB, providers, days, intervalSeconds int) *app {
	tb.Helper()
	db, err := openDatabase(filepath.Join(tb.TempDir(), "bench.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { db.Close() })
	if err := migrateDatabase(context.Background(), db); err != nil {
		tb.Fatal(err)
	}
	a := &app{db: db, logger: slog.New(slog.NewTextHandler(discardWriter{}, nil))}

	now := time.Now().UTC()
	names := []string{"torbox", "premiumize", "alldebrid", "realdebrid", "torrin", "pikpak", "offcloud", "debridlink", "easydebrid", "debrider", "deepbrid"}
	tx, err := db.Begin()
	if err != nil {
		tb.Fatal(err)
	}
	insertCheck, err := tx.Prepare(`INSERT INTO check_results(monitor_id,source,state,duration_ms,http_status,checked_at) VALUES(?,'authenticated',?,?,200,?)`)
	if err != nil {
		tb.Fatal(err)
	}
	total := 0
	for p := 1; p <= providers; p++ {
		name := names[(p-1)%len(names)]
		if _, err := tx.Exec(`INSERT INTO monitors(id,provider,name,enabled,interval_seconds,timeout_seconds,failure_threshold,recovery_threshold,public_check,created_at,updated_at) VALUES(?,?,?,1,60,15,3,2,0,?,?)`,
			p, name, name, now.Unix(), now.Unix()); err != nil {
			tb.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO monitor_states(monitor_id,current_state,state_since,last_raw_state,last_check_at) VALUES(?,'healthy',?,'healthy',?)`,
			p, now.Unix(), now.Unix()); err != nil {
			tb.Fatal(err)
		}
		samples := days * 24 * 3600 / intervalSeconds
		for i := 0; i < samples; i++ {
			at := now.Add(-time.Duration(i*intervalSeconds) * time.Second).Unix()
			state := stateHealthy
			if i%997 == 0 {
				state = stateAPI
			}
			if _, err := insertCheck.Exec(p, state, 120+int64(i%400), at); err != nil {
				tb.Fatal(err)
			}
			total++
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
	if err := rebuildRollups(context.Background(), db, nil); err != nil {
		tb.Fatal(err)
	}
	tb.Logf("seeded %d providers, %d raw check rows", providers, total)
	return a
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func benchmarkDashboard(b *testing.B, rangeName string) {
	a := benchmarkLoad(b, 11, 30, 60)
	spec, err := parseDashboardRange(rangeName)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		snapshot, err := a.dashboardSnapshot(context.Background(), spec, time.Now().UTC())
		if err != nil {
			b.Fatal(err)
		}
		if len(snapshot.Providers) == 0 {
			b.Fatal("empty snapshot")
		}
	}
}

func BenchmarkDashboard24h(b *testing.B) { benchmarkDashboard(b, "24h") }
func BenchmarkDashboard7d(b *testing.B)  { benchmarkDashboard(b, "7d") }
func BenchmarkDashboard30d(b *testing.B) { benchmarkDashboard(b, "30d") }

func BenchmarkOverview(b *testing.B) {
	a := benchmarkLoad(b, 11, 30, 60)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		response := httptest.NewRecorder()
		a.overview(response, httptest.NewRequest("GET", "/api/overview", nil))
		if response.Code != 200 {
			b.Fatalf("status %d", response.Code)
		}
	}
}

// BenchmarkRecordResult measures the cost the rollups add to the write path,
// which is the tradeoff the dashboard read speed is bought with.
func BenchmarkRecordResult(b *testing.B) {
	a := benchmarkLoad(b, 1, 1, 60)
	m := monitor{ID: 1, Provider: "torbox", Name: "torbox", FailureThreshold: 3, RecoveryThreshold: 2}
	now := time.Now().UTC()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := a.recordResult(m, "authenticated", checkResult{
			State: stateHealthy, DurationMS: int64(100 + i%300), CheckedAt: now.Add(time.Duration(i) * time.Second),
		}); err != nil {
			b.Fatal(err)
		}
	}
}
