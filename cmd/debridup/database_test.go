package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenDatabaseAppliesConnectionPragmas(t *testing.T) {
	db, err := openDatabase(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// Exercise the pool that openDatabase actually configures, rather than
	// overriding it here and leaving the real setting untested.
	if got := db.Stats().MaxOpenConnections; got != maxOpenConnections {
		t.Fatalf("MaxOpenConnections = %d, want %d", got, maxOpenConnections)
	}

	connections := make([]*sql.Conn, 0, maxOpenConnections)
	for i := 0; i < maxOpenConnections; i++ {
		conn, err := db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, conn)

		var foreignKeys, busyTimeout, synchronous int
		var journalMode string
		if err := conn.QueryRowContext(context.Background(), "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(context.Background(), "PRAGMA synchronous").Scan(&synchronous); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&journalMode); err != nil {
			t.Fatal(err)
		}
		if foreignKeys != 1 || busyTimeout != 5000 {
			t.Fatalf("foreign_keys=%d busy_timeout=%d", foreignKeys, busyTimeout)
		}
		// 1 == NORMAL, the correct pairing with WAL.
		if synchronous != 1 {
			t.Fatalf("synchronous = %d, want 1 (NORMAL)", synchronous)
		}
		if !strings.EqualFold(journalMode, "wal") {
			t.Fatalf("journal_mode = %q, want wal", journalMode)
		}
	}
	// Held simultaneously, so each assertion above ran on a distinct connection.
	for _, conn := range connections {
		conn.Close()
	}
}

func TestOpenDatabaseAcceptsRelativePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relativePath, err := filepath.Rel(workingDirectory, path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := openDatabase(relativePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateDatabase(context.Background(), db); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("relative database path was not created at %q: %v", path, err)
	}
}

func TestDashboardCheckRangeMigrationAddsAndUsesSourceTimeIndex(t *testing.T) {
	db := migratedTestDB(t)
	rows, err := db.Query(`EXPLAIN QUERY PLAN
		SELECT monitor_id,source,state,duration_ms,checked_at
		FROM check_results
		WHERE source='authenticated' AND checked_at>=? AND checked_at<?
		ORDER BY monitor_id,checked_at,id`, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan, "\n"), "USING INDEX check_results_source_time_monitor") {
		t.Fatalf("query plan did not use dashboard range index: %v", plan)
	}
}

func TestReadyzReturnsServiceUnavailableWhenDatabaseIsClosed(t *testing.T) {
	a := testApp(t)
	a.db.Close()
	rr := httptest.NewRecorder()
	a.readiness(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestParseRetentionDefaultsToNinetyDays(t *testing.T) {
	got, err := parseRetention("")
	if err != nil {
		t.Fatal(err)
	}
	if got != 90*24*time.Hour {
		t.Fatalf("retention=%s", got)
	}
}

func TestParseRetentionRejectsDurationsBelowOneDay(t *testing.T) {
	if _, err := parseRetention("23h"); err == nil {
		t.Fatal("expected retention below 24h to be rejected")
	}
}

func TestPruneHistoryRemovesOnlyExpiredChecks(t *testing.T) {
	db := migratedTestDB(t)
	monitorID := insertSyntheticMonitor(t, db, "torbox")
	expiredID := insertSyntheticCheck(t, db, monitorID, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	insertSyntheticCheck(t, db, monitorID, time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC))

	incident, err := db.Exec(`INSERT INTO incidents(monitor_id,opened_at,detected_at,initial_state,latest_state,summary) VALUES(?,?,?,?,?,?)`, monitorID, 1, 1, stateAPI, stateAPI, "expired check incident")
	if err != nil {
		t.Fatal(err)
	}
	incidentID, err := incident.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO incident_events(incident_id,type,new_state,created_at,check_id) VALUES(?,?,?,?,?)`, incidentID, "opened", stateAPI, 1, expiredID); err != nil {
		t.Fatal(err)
	}

	deleted, err := pruneHistory(context.Background(), db, time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted=%d", deleted)
	}
	for table, want := range map[string]int{"check_results": 1, "incidents": 1, "incident_events": 1} {
		var got int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s=%d, want %d", table, got, want)
		}
	}
}

func migratedTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := openDatabase(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrateDatabase(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db
}

func insertSyntheticMonitor(t *testing.T, db *sql.DB, provider string) int64 {
	t.Helper()
	result, err := db.Exec(`INSERT INTO monitors(provider,name,created_at,updated_at) VALUES(?,?,?,?)`, provider, provider, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func insertSyntheticCheck(t *testing.T, db *sql.DB, monitorID int64, checkedAt time.Time) int64 {
	t.Helper()
	result, err := db.Exec(`INSERT INTO check_results(monitor_id,source,state,duration_ms,checked_at) VALUES(?,?,?,?,?)`, monitorID, "authenticated", stateHealthy, 10, checkedAt.Unix())
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func testApp(t *testing.T) *app {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &app{db: db}
}
