package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProviderHealth(t *testing.T) {
	for _, tc := range []struct {
		name, state, raw, path, want string
		enabled, age, code           int
	}{
		{"healthy", stateHealthy, stateHealthy, "torbox", `{"ok":true,"state":"healthy"}` + "\n", 1, 0, 200},
		{"pending failure", stateHealthy, stateAPI, "torbox", `{"ok":true,"state":"healthy"}` + "\n", 1, 0, 200},
		{"pending recovery", stateAPI, stateHealthy, "torbox", `{"ok":false,"state":"unhealthy"}` + "\n", 1, 0, 503},
		{"auth failure", stateAuthFailed, stateAuthFailed, "torbox", `{"ok":false,"state":"unhealthy"}` + "\n", 1, 0, 503},
		{"stale", stateHealthy, stateHealthy, "torbox", `{"ok":false,"state":"unhealthy"}` + "\n", 1, 136, 503},
		{"future", stateHealthy, stateHealthy, "torbox", `{"ok":false,"state":"unhealthy"}` + "\n", 1, -60, 503},
		{"paused", stateHealthy, stateHealthy, "torbox", `{"ok":false,"state":"paused"}` + "\n", 0, 0, 503},
		{"unconfigured", stateHealthy, stateHealthy, "premiumize", `{"ok":false,"state":"not_found"}` + "\n", 1, 0, 404},
		{"unsupported", stateHealthy, stateHealthy, "invalid", `{"ok":false,"state":"not_found"}` + "\n", 1, 0, 404},
		{"never checked", stateHealthy, stateHealthy, "torbox", `{"ok":false,"state":"unhealthy"}` + "\n", 1, -1, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(t)
			if err := migrateDatabase(context.Background(), a.db); err != nil {
				t.Fatal(err)
			}
			now := time.Now().Unix()
			if _, err := a.db.Exec(`INSERT INTO monitors(id,provider,name,enabled,created_at,updated_at) VALUES(1,'torbox','private account name',?,?,?)`, tc.enabled, now, now); err != nil {
				t.Fatal(err)
			}
			checked := now - int64(tc.age)
			if tc.age == -1 {
				checked = 0
			}
			if _, err := a.db.Exec(`INSERT INTO monitor_states(monitor_id,current_state,state_since,last_raw_state,last_check_at) VALUES(1,?,?,?,?)`, tc.state, now, tc.raw, checked); err != nil {
				t.Fatal(err)
			}
			rr := httptest.NewRecorder()
			a.routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/integrations/health/"+tc.path, nil))
			if rr.Code != tc.code || rr.Body.String() != tc.want {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			if rr.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("health response must not be cached by proxies")
			}
		})
	}
}

func TestProviderHealthMultipleMonitorsAndUnavailableDatabase(t *testing.T) {
	a := testApp(t)
	if err := migrateDatabase(context.Background(), a.db); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for id := 1; id <= 2; id++ {
		if _, err := a.db.Exec(`INSERT INTO monitors(id,provider,name,created_at,updated_at) VALUES(?,'torbox','private',?,?)`, id, now, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.db.Exec(`INSERT INTO monitor_states(monitor_id,current_state,state_since,last_check_at) VALUES(1,'healthy',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	check := func(want int) {
		t.Helper()
		rr := httptest.NewRecorder()
		a.routes().ServeHTTP(rr, httptest.NewRequest("GET", "/integrations/health/torbox", nil))
		if rr.Code != want {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
	}
	check(503) // An enabled monitor with no state must not be hidden by a healthy one.
	if _, err := a.db.Exec(`UPDATE monitors SET enabled=0 WHERE id=2`); err != nil {
		t.Fatal(err)
	}
	check(200)
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	check(503)
}
