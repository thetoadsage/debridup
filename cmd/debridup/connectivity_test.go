package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestConnectivityControls(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer bad.Close()
	a := &app{connectivityURLs: []string{bad.URL, ok.URL}}
	if !a.connectivityAvailable() {
		t.Fatal("one reachable control should establish connectivity")
	}
	a.connectivityURLs = []string{bad.URL, bad.URL}
	if a.connectivityAvailable() {
		t.Fatal("two failed controls should leave connectivity unknown")
	}
	result := checkResult{State: stateConnection, ErrorCode: "connection_error"}
	a.connectivityIPURL = ok.URL
	classified := a.classifyConnectivity(result)
	if classified.State != "unknown" || classified.ErrorCode != "dns_issue_possible" {
		t.Fatalf("IP reachable after hostname failures: %+v", classified)
	}
	a.connectivityIPURL = bad.URL
	classified = a.classifyConnectivity(result)
	if classified.State != "unknown" || classified.ErrorCode != "local_connectivity_unknown" {
		t.Fatalf("all controls failed: %+v", classified)
	}
	a.connectivityURLs = []string{bad.URL, ok.URL}
	classified = a.classifyConnectivity(result)
	if classified != result {
		t.Fatalf("named control reachable: %+v", classified)
	}
}

func TestUnknownCheckBreaksIncidentStreakAndCoverage(t *testing.T) {
	a := testApp(t)
	if err := migrateDatabase(context.Background(), a.db); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 26, 12, 1, 0, 0, time.UTC)
	if _, err := a.db.Exec(`INSERT INTO monitors(id,provider,name,created_at,updated_at) VALUES(1,'torbox','TorBox',?,?)`, now.Unix(), now.Unix()); err != nil {
		t.Fatal(err)
	}
	m := monitor{ID: 1, Provider: "torbox", Name: "TorBox", FailureThreshold: 2, RecoveryThreshold: 2}
	for i, state := range []string{stateConnection, "unknown", stateConnection, stateConnection} {
		if _, err := a.recordResult(m, "authenticated", checkResult{State: state, DurationMS: 10, CheckedAt: now.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
		var incidents int
		if err := a.db.QueryRow(`SELECT COUNT(*) FROM incidents`).Scan(&incidents); err != nil {
			t.Fatal(err)
		}
		if want := []int{0, 0, 0, 1}[i]; incidents != want {
			t.Fatalf("after check %d: incidents=%d, want %d", i, incidents, want)
		}
	}
	var total int
	if err := a.db.QueryRow(`SELECT total FROM check_rollups WHERE monitor_id=1 AND bucket_width=900 AND bucket_start=?`, bucketStartFor(now.Unix(), 900)).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Fatalf("coverage total=%d, want three provider checks", total)
	}
}
