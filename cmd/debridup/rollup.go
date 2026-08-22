package main

import (
	"context"
	"database/sql"
	"time"
)

// rollupBucketWidths are the bucket sizes the dashboard ranges use, in seconds.
// Keeping a rollup per width means every range reads pre-summarised rows
// instead of re-reducing raw checks on each request.
var rollupBucketWidths = []int64{
	int64(15 * time.Minute / time.Second),
	int64(2 * time.Hour / time.Second),
	int64(8 * time.Hour / time.Second),
}

// bucketStartFor snaps a timestamp down to its bucket boundary. Buckets are
// anchored to the epoch rather than to "now minus the window", so a bucket's
// contents are stable between refreshes and can be stored.
func bucketStartFor(checkedAt, width int64) int64 {
	if width <= 0 {
		return checkedAt
	}
	return checkedAt - modFloor(checkedAt, width)
}

// modFloor is a modulo that floors toward negative infinity, so pre-epoch
// timestamps bucket consistently instead of snapping the wrong way.
func modFloor(value, width int64) int64 {
	remainder := value % width
	if remainder < 0 {
		remainder += width
	}
	return remainder
}

type rollupExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// refreshRollupBucket recomputes one bucket from the raw authenticated checks it
// covers. Recomputing keeps the stored percentiles exactly equal to what a
// direct calculation would produce, which an incremental update cannot promise.
func refreshRollupBucket(ctx context.Context, db rollupExecutor, monitorID, width, bucketStart int64) error {
	rows, err := db.QueryContext(ctx, `SELECT state,duration_ms FROM check_results
	WHERE monitor_id=? AND source='authenticated' AND checked_at>=? AND checked_at<?`,
		monitorID, bucketStart, bucketStart+width)
	if err != nil {
		return err
	}
	var (
		total     int64
		healthy   int64
		eligible  int64
		slowest   int64
		latencies []int64
	)
	for rows.Next() {
		var state string
		var duration int64
		if err := rows.Scan(&state, &duration); err != nil {
			rows.Close()
			return err
		}
		total++
		if state == stateHealthy {
			healthy++
		}
		if state != stateAuthFailed {
			eligible++
		}
		if duration > slowest {
			slowest = duration
		}
		latencies = append(latencies, duration)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	if total == 0 {
		_, err = db.ExecContext(ctx, `DELETE FROM check_rollups WHERE monitor_id=? AND bucket_width=? AND bucket_start=?`,
			monitorID, width, bucketStart)
		return err
	}

	// Percentiles span every completed check in the bucket, matching the
	// documented definition, not just the healthy ones.
	p50 := nearestRank(latencies, .50)
	p95 := nearestRank(latencies, .95)
	_, err = db.ExecContext(ctx, `INSERT INTO check_rollups(monitor_id,bucket_width,bucket_start,total,healthy,eligible,slowest_ms,p50_ms,p95_ms)
	VALUES(?,?,?,?,?,?,?,?,?)
	ON CONFLICT(monitor_id,bucket_width,bucket_start) DO UPDATE SET
	 total=excluded.total,healthy=excluded.healthy,eligible=excluded.eligible,slowest_ms=excluded.slowest_ms,p50_ms=excluded.p50_ms,p95_ms=excluded.p95_ms`,
		monitorID, width, bucketStart, total, healthy, eligible, slowest, p50, p95)
	return err
}

// refreshRollupsFor updates every bucket width covering one check.
func refreshRollupsFor(ctx context.Context, db rollupExecutor, monitorID, checkedAt int64) error {
	for _, width := range rollupBucketWidths {
		if err := refreshRollupBucket(ctx, db, monitorID, width, bucketStartFor(checkedAt, width)); err != nil {
			return err
		}
	}
	return nil
}

// rebuildRollups recomputes every bucket from the raw checks currently stored.
// Used by the backfill migration and after any bulk deletion of raw rows.
func rebuildRollups(ctx context.Context, db rollupExecutor, monitorID *int64) error {
	if monitorID == nil {
		if _, err := db.ExecContext(ctx, `DELETE FROM check_rollups`); err != nil {
			return err
		}
	} else if _, err := db.ExecContext(ctx, `DELETE FROM check_rollups WHERE monitor_id=?`, *monitorID); err != nil {
		return err
	}

	for _, width := range rollupBucketWidths {
		// Counters and the slowest sample aggregate directly in SQL. Percentiles
		// need ordering, so they are filled in per bucket below.
		query := `INSERT INTO check_rollups(monitor_id,bucket_width,bucket_start,total,healthy,eligible,slowest_ms,p50_ms,p95_ms)
		SELECT monitor_id, ?, checked_at - ((checked_at % ? + ?) % ?),
		       COUNT(*), SUM(CASE WHEN state='healthy' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN state!='auth_failed' THEN 1 ELSE 0 END), MAX(duration_ms), NULL, NULL
		FROM check_results
		WHERE source='authenticated'`
		args := []any{width, width, width, width}
		if monitorID != nil {
			query += ` AND monitor_id=?`
			args = append(args, *monitorID)
		}
		query += ` GROUP BY monitor_id, checked_at - ((checked_at % ? + ?) % ?)`
		args = append(args, width, width, width)
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			return err
		}
	}

	return fillRollupPercentiles(ctx, db, monitorID)
}

// fillRollupPercentiles computes exact percentiles for every bucket that does
// not have them yet.
func fillRollupPercentiles(ctx context.Context, db rollupExecutor, monitorID *int64) error {
	query := `SELECT monitor_id,bucket_width,bucket_start FROM check_rollups WHERE p95_ms IS NULL`
	var args []any
	if monitorID != nil {
		query += ` AND monitor_id=?`
		args = append(args, *monitorID)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	type target struct{ monitorID, width, bucketStart int64 }
	var targets []target
	for rows.Next() {
		var item target
		if err := rows.Scan(&item.monitorID, &item.width, &item.bucketStart); err != nil {
			rows.Close()
			return err
		}
		targets = append(targets, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, item := range targets {
		if err := refreshRollupBucket(ctx, db, item.monitorID, item.width, item.bucketStart); err != nil {
			return err
		}
	}
	return nil
}
