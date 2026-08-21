package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestOpenDatabaseAppliesConnectionPragmas(t *testing.T) {
	db, err := openDatabase(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	db.SetMaxOpenConns(2)
	for i := 0; i < 2; i++ {
		conn, err := db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var foreignKeys, busyTimeout int
		if err := conn.QueryRowContext(context.Background(), "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
			t.Fatal(err)
		}
		conn.Close()
		if foreignKeys != 1 || busyTimeout != 5000 {
			t.Fatalf("foreign_keys=%d busy_timeout=%d", foreignKeys, busyTimeout)
		}
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

func testApp(t *testing.T) *app {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &app{db: db}
}
