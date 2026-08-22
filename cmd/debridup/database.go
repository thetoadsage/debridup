package main

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

const currentSchemaVersion = 6

// maxOpenConnections bounds the SQLite pool. Readers run concurrently in WAL
// mode; writers serialise, so a handful of connections is the useful range.
const maxOpenConnections = 4

const (
	defaultHistoryRetention = 90 * 24 * time.Hour
	minimumHistoryRetention = 24 * time.Hour
)

const currentSchema = `
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS monitors (
 id INTEGER PRIMARY KEY, provider TEXT NOT NULL CHECK(provider IN ('torbox','premiumize','alldebrid','realdebrid','torrin','pikpak','offcloud','debridlink','easydebrid','debrider','deepbrid')), name TEXT NOT NULL,
 enabled INTEGER NOT NULL DEFAULT 1, interval_seconds INTEGER NOT NULL DEFAULT 60, timeout_seconds INTEGER NOT NULL DEFAULT 15,
 failure_threshold INTEGER NOT NULL DEFAULT 3, recovery_threshold INTEGER NOT NULL DEFAULT 2, public_check INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS monitor_secrets (monitor_id INTEGER PRIMARY KEY REFERENCES monitors(id) ON DELETE CASCADE, nonce BLOB NOT NULL, ciphertext BLOB NOT NULL, key_version INTEGER NOT NULL DEFAULT 1, updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS monitor_states (
 monitor_id INTEGER PRIMARY KEY REFERENCES monitors(id) ON DELETE CASCADE, current_state TEXT NOT NULL DEFAULT 'healthy', state_since INTEGER NOT NULL,
 last_raw_state TEXT NOT NULL DEFAULT 'healthy', failure_streak INTEGER NOT NULL DEFAULT 0, recovery_streak INTEGER NOT NULL DEFAULT 0,
 failure_started_at INTEGER NOT NULL DEFAULT 0, last_check_at INTEGER
);
CREATE TABLE IF NOT EXISTS check_results (
 id INTEGER PRIMARY KEY, monitor_id INTEGER NOT NULL REFERENCES monitors(id) ON DELETE CASCADE, source TEXT NOT NULL CHECK(source IN ('authenticated','public')),
 state TEXT NOT NULL, duration_ms INTEGER NOT NULL, http_status INTEGER, error_code TEXT, error_detail TEXT, checked_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS check_results_monitor_time ON check_results(monitor_id, checked_at DESC);
CREATE TABLE IF NOT EXISTS incidents (
 id INTEGER PRIMARY KEY, monitor_id INTEGER NOT NULL REFERENCES monitors(id) ON DELETE CASCADE, opened_at INTEGER NOT NULL, detected_at INTEGER NOT NULL,
 resolved_at INTEGER, initial_state TEXT NOT NULL, latest_state TEXT NOT NULL, summary TEXT
);
CREATE INDEX IF NOT EXISTS incidents_monitor_time ON incidents(monitor_id, opened_at DESC);
CREATE TABLE IF NOT EXISTS incident_events (
 id INTEGER PRIMARY KEY, incident_id INTEGER NOT NULL REFERENCES incidents(id) ON DELETE CASCADE, type TEXT NOT NULL,
 previous_state TEXT, new_state TEXT NOT NULL, created_at INTEGER NOT NULL, check_id INTEGER REFERENCES check_results(id)
);
CREATE INDEX IF NOT EXISTS incident_events_incident ON incident_events(incident_id, created_at, id);
CREATE TABLE IF NOT EXISTS notification_channels (
 id INTEGER PRIMARY KEY, kind TEXT NOT NULL UNIQUE CHECK(kind='ntfy'), enabled INTEGER NOT NULL DEFAULT 0, nonce BLOB, ciphertext BLOB, updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS notification_outbox (
 id INTEGER PRIMARY KEY, channel_id INTEGER NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE, incident_id INTEGER REFERENCES incidents(id) ON DELETE CASCADE,
 event_type TEXT NOT NULL, payload TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending', attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at INTEGER NOT NULL, delivered_at INTEGER, last_error TEXT
);
CREATE INDEX IF NOT EXISTS outbox_pending ON notification_outbox(status, next_attempt_at);
CREATE TABLE IF NOT EXISTS sessions (id TEXT PRIMARY KEY, created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS sessions_expiry ON sessions(expires_at);
CREATE TABLE IF NOT EXISTS check_rollups (
 monitor_id INTEGER NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
 bucket_width INTEGER NOT NULL, bucket_start INTEGER NOT NULL,
 total INTEGER NOT NULL, healthy INTEGER NOT NULL, eligible INTEGER NOT NULL DEFAULT 0, slowest_ms INTEGER NOT NULL,
 p50_ms INTEGER, p95_ms INTEGER,
 PRIMARY KEY(monitor_id, bucket_width, bucket_start)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS check_rollups_window ON check_rollups(bucket_width, bucket_start);
`

func openDatabase(path string) (*sql.DB, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	path = absolutePath
	if filepath.VolumeName(path) != "" {
		path = "/" + path
	}
	values := url.Values{}
	values.Add("_pragma", "foreign_keys(1)")
	values.Add("_pragma", "busy_timeout(5000)")
	values.Add("_pragma", "journal_mode(WAL)")
	// WAL defaults to synchronous=FULL, which fsyncs on every commit. NORMAL is
	// the standard WAL pairing: durable across process crashes, and only at risk
	// from an OS-level crash. It matters a great deal on the SD and USB storage
	// a NAS deployment often uses.
	values.Add("_pragma", "synchronous(NORMAL)")
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: values.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite serialises writes regardless, so a large pool only converts lock
	// contention into SQLITE_BUSY. Bound it here rather than leaving production
	// unbounded while the tests pin it.
	db.SetMaxOpenConns(maxOpenConnections)
	db.SetMaxIdleConns(maxOpenConnections)
	db.SetConnMaxLifetime(time.Hour)
	return db, nil
}

func migrateDatabase(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err = conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), `PRAGMA foreign_keys = ON`)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	for version := 1; version <= currentSchemaVersion; version++ {
		var applied bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = ?)`, version).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		if err = applyMigration(ctx, tx, version); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`, version, time.Now().Unix()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func applyMigration(ctx context.Context, tx *sql.Tx, version int) error {
	switch version {
	case 1:
		if _, err := tx.ExecContext(ctx, currentSchema); err != nil {
			return err
		}
		return migrateProviderConstraint(ctx, tx)
	case 2:
		_, err := tx.ExecContext(ctx, `CREATE INDEX check_results_source_time_monitor ON check_results(source, checked_at, monitor_id)`)
		return err
	case 3:
		// incident_events had no index on incident_id, so loading one incident's
		// log scanned the whole table. check_results had no index leading with
		// monitor_id AND source, so per-monitor aggregates seeked on source and
		// scanned across every other monitor's history.
		_, err := tx.ExecContext(ctx, `
CREATE INDEX IF NOT EXISTS incident_events_incident ON incident_events(incident_id, created_at, id);
CREATE INDEX IF NOT EXISTS check_results_monitor_source_time ON check_results(monitor_id, source, checked_at);
`)
		return err
	case 4:
		// Sessions become server-side records so signing out revokes them,
		// instead of the cookie being a self-contained bearer of an expiry.
		_, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS sessions (id TEXT PRIMARY KEY, created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS sessions_expiry ON sessions(expires_at);
`)
		return err
	case 5:
		// Pre-aggregated dashboard buckets. Reading roughly ninety summarised
		// rows per provider replaces re-reducing every raw check in the window
		// on each refresh.
		if _, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS check_rollups (
 monitor_id INTEGER NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
 bucket_width INTEGER NOT NULL, bucket_start INTEGER NOT NULL,
 total INTEGER NOT NULL, healthy INTEGER NOT NULL, eligible INTEGER NOT NULL DEFAULT 0, slowest_ms INTEGER NOT NULL,
 p50_ms INTEGER, p95_ms INTEGER,
 PRIMARY KEY(monitor_id, bucket_width, bucket_start)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS check_rollups_window ON check_rollups(bucket_width, bucket_start);
`); err != nil {
			return err
		}
		return rebuildRollups(ctx, tx, nil)
	case 6:
		// Checks that are not authentication failures, so the overview's
		// coverage figure can be summed from buckets instead of reading the
		// state of every raw row in the window. A database created from the
		// current schema already has the column, so add it only when missing.
		present, err := columnExists(ctx, tx, "check_rollups", "eligible")
		if err != nil {
			return err
		}
		if !present {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE check_rollups ADD COLUMN eligible INTEGER NOT NULL DEFAULT 0`); err != nil {
				return err
			}
		}
		return rebuildRollups(ctx, tx, nil)
	default:
		return errors.New("unknown schema migration")
	}
}

func columnExists(ctx context.Context, tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func migrateProviderConstraint(ctx context.Context, tx *sql.Tx) error {
	var schema string
	if err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='monitors'`).Scan(&schema); err != nil {
		return err
	}
	if strings.Contains(schema, "'deepbrid'") {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
CREATE TABLE monitors_new (
 id INTEGER PRIMARY KEY, provider TEXT NOT NULL CHECK(provider IN ('torbox','premiumize','alldebrid','realdebrid','torrin','pikpak','offcloud','debridlink','easydebrid','debrider','deepbrid')), name TEXT NOT NULL,
 enabled INTEGER NOT NULL DEFAULT 1, interval_seconds INTEGER NOT NULL DEFAULT 60, timeout_seconds INTEGER NOT NULL DEFAULT 15,
 failure_threshold INTEGER NOT NULL DEFAULT 3, recovery_threshold INTEGER NOT NULL DEFAULT 2, public_check INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
INSERT INTO monitors_new SELECT * FROM monitors;
DROP TABLE monitors;
ALTER TABLE monitors_new RENAME TO monitors;
`)
	return err
}

func databaseReady(ctx context.Context, db *sql.DB) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var one int
	return db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

func parseRetention(raw string) (time.Duration, error) {
	if raw == "" {
		return defaultHistoryRetention, nil
	}
	retention, err := time.ParseDuration(raw)
	if err != nil {
		return 0, err
	}
	if retention < minimumHistoryRetention {
		return 0, errors.New("DEBRIDUP_HISTORY_RETENTION must be at least 24h")
	}
	return retention, nil
}

func pruneHistory(ctx context.Context, db *sql.DB, cutoff time.Time) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `UPDATE incident_events SET check_id = NULL WHERE check_id IN (SELECT id FROM check_results WHERE checked_at < ?)`, cutoff.Unix()); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM check_results WHERE checked_at < ?`, cutoff.Unix())
	if err != nil {
		return 0, err
	}
	// Rollup buckets summarise raw checks, so buckets that are now wholly
	// outside retention go with them. A bucket straddling the cutoff is
	// recomputed from whatever rows remain.
	for _, width := range rollupBucketWidths {
		boundary := bucketStartFor(cutoff.Unix(), width)
		if _, err := tx.ExecContext(ctx, `DELETE FROM check_rollups WHERE bucket_width=? AND bucket_start<?`, width, boundary); err != nil {
			return 0, err
		}
		straddling, err := tx.QueryContext(ctx, `SELECT monitor_id FROM check_rollups WHERE bucket_width=? AND bucket_start=?`, width, boundary)
		if err != nil {
			return 0, err
		}
		var monitors []int64
		for straddling.Next() {
			var monitorID int64
			if err := straddling.Scan(&monitorID); err != nil {
				straddling.Close()
				return 0, err
			}
			monitors = append(monitors, monitorID)
		}
		if err := straddling.Err(); err != nil {
			straddling.Close()
			return 0, err
		}
		straddling.Close()
		for _, monitorID := range monitors {
			if err := refreshRollupBucket(ctx, tx, monitorID, width, boundary); err != nil {
				return 0, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
