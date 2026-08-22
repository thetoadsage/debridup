package main

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSendNtfy(t *testing.T) {
	var gotTitle, gotTags, gotEventID, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTitle = r.Header.Get("Title")
		gotTags = r.Header.Get("Tags")
		gotEventID = r.Header.Get("X-Event-ID")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	a := &app{client: server.Client()}
	err := a.sendNtfy(server.URL+"/topic/", "DebridUp test", "It works", "white_check_mark", "test-1")
	if err != nil {
		t.Fatalf("sendNtfy returned an error: %v", err)
	}
	if gotTitle != "DebridUp test" || gotTags != "white_check_mark" || gotEventID != "test-1" || gotBody != "It works" {
		t.Fatalf("unexpected ntfy request: title=%q tags=%q event=%q body=%q", gotTitle, gotTags, gotEventID, gotBody)
	}
}

func TestHealthz(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()
	(&app{}).routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "{\"ok\":true}\n" {
		t.Fatalf("health check returned %d: %q", response.Code, response.Body.String())
	}
}

func TestDashboardHTMLContainsAccessibleLandmarks(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, required := range []string{
		`<nav aria-label="Primary">`, `id="range-controls"`, `id="summary"`,
		`id="provider-pulse"`, `id="provider-table-body"`, `id="latency-chart"`,
		`id="incidents"`, `id="provider-drawer"`, `id="dashboard-status"`,
		`aria-live="polite"`,
	} {
		if !strings.Contains(html, required) {
			t.Errorf("missing %s", required)
		}
	}
}

func TestDashboardAssetsContainResponsiveMonitorDialog(t *testing.T) {
	htmlBytes, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(htmlBytes), `<dialog id="monitor-dialog">`) {
		t.Fatal("monitor management dialog is missing")
	}

	cssBytes, err := webFS.ReadFile("web/app.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cssBytes), "dialog .form { min-width: 0; width: 100%; }") {
		t.Fatal("monitor dialog form must shrink to the dialog content width")
	}
}

func TestProviderDrawerLabelsTheLatestCheckAccurately(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	if !strings.Contains(html, `<dt>Last check</dt>`) {
		t.Fatal("provider drawer must label the API's lastCheck value as Last check")
	}
	if strings.Contains(html, `<dt>Last successful check</dt>`) {
		t.Fatal("provider drawer must not describe lastCheck as successful")
	}
}

func TestPulseStylesBoundMaximumHistoryWithoutColorOnlyMeaning(t *testing.T) {
	b, err := webFS.ReadFile("web/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	if !strings.Contains(css, `.pulse-track { display: grid; min-width: 0; max-width: 100%; overflow: hidden; grid-template-columns: repeat(var(--pulse-bucket-count, 1), minmax(0, 1fr));`) {
		t.Fatal("pulse track must scale every server bucket inside its available width")
	}
	if strings.Count(css, "repeating-linear-gradient") < 4 {
		t.Fatal("pulse states must use distinct visible patterns in addition to color")
	}
}

func TestStatsResetsRefreshMonitorSettingsAndDashboard(t *testing.T) {
	b, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	app := string(b)
	perProviderStart := strings.Index(app, "$('#reset-monitor-stats').addEventListener")
	globalStart := strings.Index(app, "$('#reset-all-stats').addEventListener")
	notificationStart := strings.Index(app, "$('#ntfy-form').addEventListener")
	if perProviderStart < 0 || globalStart <= perProviderStart || notificationStart <= globalStart {
		t.Fatal("could not locate reset handlers")
	}
	for name, block := range map[string]string{
		"per-provider reset": app[perProviderStart:globalStart],
		"global reset":       app[globalStart:notificationStart],
	} {
		if !strings.Contains(block, "loadMonitorSettings()") || !strings.Contains(block, "dashboard.refresh({supersede: true})") {
			t.Errorf("%s must refresh both monitor settings and dashboard data", name)
		}
	}
}

func TestSendNtfyReportsHTTPFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	a := &app{client: server.Client()}
	err := a.sendNtfy(server.URL+"/topic", "Test", "Test", "warning", "test-2")
	if err == nil || err.Error() != "ntfy returned HTTP 401" {
		t.Fatalf("expected safe HTTP status error, got %v", err)
	}
}

func TestNormalizeNtfyURL(t *testing.T) {
	got, err := normalizeNtfyURL(" https://ntfy.sh/private-topic/ ")
	if err != nil {
		t.Fatalf("normalizeNtfyURL returned an error: %v", err)
	}
	if got != "https://ntfy.sh/private-topic" {
		t.Fatalf("expected trailing slash to be removed, got %q", got)
	}
	if _, err = normalizeNtfyURL("https://ntfy.sh/"); err == nil {
		t.Fatal("expected a topic-less ntfy URL to be rejected")
	}
}

func TestIncidentSummary(t *testing.T) {
	tests := []struct {
		name   string
		result checkResult
		want   string
	}{
		{"authentication", checkResult{State: stateAuthFailed}, "The provider rejected the configured credential. Verify or replace it."},
		{"timeout", checkResult{State: stateConnection, ErrorCode: "timeout"}, "The authenticated API request timed out before the provider responded."},
		{"server status", checkResult{State: stateAPI, ErrorCode: "server_error", HTTPStatus: 503}, "The provider API returned HTTP 503, indicating a server-side failure."},
		{"invalid response", checkResult{State: stateAPI, ErrorCode: "invalid_response"}, "The provider API was reachable, but its response was invalid or could not be understood."},
		{"rate limited", checkResult{State: stateAPI, ErrorCode: "rate_limited"}, "The provider API rate-limited the authenticated health check."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := incidentSummary(test.result); got != test.want {
				t.Fatalf("incidentSummary() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestConfirmedAvailability(t *testing.T) {
	tests := []struct {
		name    string
		start   int64
		now     int64
		periods []incidentPeriod
		want    *float64
	}{
		{name: "no monitoring history", start: 0, now: 1100},
		{name: "isolated failures do not count", start: 100, now: 1100, want: floatPointer(100)},
		{name: "confirmed resolved incident", start: 100, now: 1100, periods: []incidentPeriod{{OpenedAt: 200, ResolvedAt: 300}}, want: floatPointer(90)},
		{name: "incident is clipped to window", start: 100, now: 1100, periods: []incidentPeriod{{OpenedAt: 50, ResolvedAt: 200}}, want: floatPointer(90)},
		{name: "open incident runs to now", start: 100, now: 1100, periods: []incidentPeriod{{OpenedAt: 900}}, want: floatPointer(80)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := confirmedAvailability(test.start, test.now, test.periods)
			if test.want == nil {
				if got != nil {
					t.Fatalf("confirmedAvailability() = %.2f, want nil", *got)
				}
				return
			}
			if got == nil || *got != *test.want {
				t.Fatalf("confirmedAvailability() = %v, want %.2f", got, *test.want)
			}
		})
	}
}

func floatPointer(value float64) *float64 { return &value }

func TestProviderDefinitions(t *testing.T) {
	want := []string{"torbox", "premiumize", "alldebrid", "realdebrid", "torrin", "pikpak", "offcloud", "debridlink", "easydebrid", "debrider", "deepbrid"}
	for _, id := range want {
		provider, ok := providerDefinitions[id]
		if !ok {
			t.Fatalf("provider %q is missing", id)
		}
		if !strings.HasPrefix(provider.Endpoint, "https://") || !strings.HasPrefix(provider.PublicEndpoint, "https://") {
			t.Fatalf("provider %q must use HTTPS endpoints: %#v", id, provider)
		}
	}
	if got := providerDefinitions["torrin"]; got.Endpoint != "https://torrin.app/api/stats" || got.PublicEndpoint != "https://torrin.app/api/stats/public" {
		t.Fatalf("Torrin must use its authenticated and public stats endpoints: %#v", got)
	}
}

func TestClassifyProviderPayload(t *testing.T) {
	tests := []struct {
		name, provider string
		payload        any
		state, code    string
	}{
		{"torbox success", "torbox", map[string]any{"success": true}, "", ""},
		{"torbox invalid token", "torbox", map[string]any{"success": false, "detail": "Invalid token"}, stateAuthFailed, "authentication_rejected"},
		{"premiumize API failure", "premiumize", map[string]any{"status": "error", "message": "backend unavailable"}, stateAPI, "api_error"},
		{"alldebrid invalid key", "alldebrid", map[string]any{"status": "error", "error": map[string]any{"code": "AUTH_BAD_APIKEY"}}, stateAuthFailed, "authentication_rejected"},
		{"offcloud invalid key", "offcloud", map[string]any{"error": "Invalid API key"}, stateAuthFailed, "authentication_rejected"},
		{"deepbrid application failure", "deepbrid", map[string]any{"error": float64(1), "message": "Internal failure"}, stateAPI, "api_error"},
		{"generic account response", "realdebrid", map[string]any{"id": float64(42)}, "", ""},
		{"torrin stats response", "torrin", map[string]any{"stats": map[string]any{}, "plan": map[string]any{}}, "", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, code := classifyProviderPayload(test.provider, test.payload)
			if state != test.state || code != test.code {
				t.Fatalf("classifyProviderPayload() = (%q, %q), want (%q, %q)", state, code, test.state, test.code)
			}
		})
	}
}

func TestAuthCheckHandlesBearerAndRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bearer":
			if r.Header.Get("Authorization") != "Bearer correct-credential" {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":1}`))
		default:
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("login"))
		}
	}))
	defer server.Close()
	providerDefinitions["test-bearer"] = providerDefinition{Endpoint: server.URL + "/bearer", Method: http.MethodGet}
	defer delete(providerDefinitions, "test-bearer")
	a := &app{client: server.Client()}

	if result := a.authCheck(monitor{Provider: "test-bearer", TimeoutSeconds: 3}, "correct-credential"); result.State != stateHealthy {
		t.Fatalf("bearer check returned %q (%q)", result.State, result.ErrorCode)
	}
	if result := a.authCheck(monitor{Provider: "test-bearer", TimeoutSeconds: 3}, "wrong-credential"); result.State != stateAuthFailed || result.ErrorCode != "authentication_redirect" {
		t.Fatalf("redirected auth check returned %q (%q)", result.State, result.ErrorCode)
	}
}

func TestMigrateExpandsProviderConstraint(t *testing.T) {
	db, err := sql.Open("sqlite", "file:provider-migration?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE monitors (
 id INTEGER PRIMARY KEY, provider TEXT NOT NULL CHECK(provider IN ('torbox','premiumize')), name TEXT NOT NULL,
 enabled INTEGER NOT NULL DEFAULT 1, interval_seconds INTEGER NOT NULL DEFAULT 60, timeout_seconds INTEGER NOT NULL DEFAULT 15,
 failure_threshold INTEGER NOT NULL DEFAULT 3, recovery_threshold INTEGER NOT NULL DEFAULT 2, public_check INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
); INSERT INTO monitors(provider,name,created_at,updated_at) VALUES('torbox','Existing TorBox',1,1);`)
	if err != nil {
		t.Fatal(err)
	}
	if err = migrateDatabase(context.Background(), db); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}
	if _, err = db.Exec(`INSERT INTO monitors(provider,name,created_at,updated_at) VALUES('realdebrid','Real-Debrid',2,2)`); err != nil {
		t.Fatalf("new provider rejected after migration: %v", err)
	}
	var count int
	if err = db.QueryRow(`SELECT COUNT(*) FROM monitors`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("monitor data was not preserved: count=%d err=%v", count, err)
	}
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("provider constraint migration left a foreign key violation")
	}
}

func TestResetHistoryScopesAndPreservesConfiguration(t *testing.T) {
	db, err := sql.Open("sqlite", "file:reset-history?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	a := &app{db: db}
	if err = migrateDatabase(context.Background(), db); err != nil {
		t.Fatal(err)
	}

	for id, provider := range []string{"torbox", "torrin"} {
		monitorID := id + 1
		if _, err = db.Exec(`INSERT INTO monitors(id,provider,name,created_at,updated_at) VALUES(?,?,?,?,?)`, monitorID, provider, provider, 1, 1); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO monitor_secrets(monitor_id,nonce,ciphertext,updated_at) VALUES(?,?,?,?)`, monitorID, []byte{1}, []byte{2}, 1); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO monitor_states(monitor_id,current_state,state_since,last_raw_state,last_check_at) VALUES(?,?,?,?,?)`, monitorID, stateAPI, 1, stateAPI, 1); err != nil {
			t.Fatal(err)
		}
		check, err := db.Exec(`INSERT INTO check_results(monitor_id,source,state,duration_ms,checked_at) VALUES(?,?,?,?,?)`, monitorID, "authenticated", stateAPI, 10, 1)
		if err != nil {
			t.Fatal(err)
		}
		checkID, _ := check.LastInsertId()
		incident, err := db.Exec(`INSERT INTO incidents(monitor_id,opened_at,detected_at,initial_state,latest_state,summary) VALUES(?,?,?,?,?,?)`, monitorID, 1, 1, stateAPI, stateAPI, "test")
		if err != nil {
			t.Fatal(err)
		}
		incidentID, _ := incident.LastInsertId()
		if _, err = db.Exec(`INSERT INTO incident_events(incident_id,type,new_state,created_at,check_id) VALUES(?,?,?,?,?)`, incidentID, "opened", stateAPI, 1, checkID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec(`INSERT INTO notification_channels(id,kind,updated_at) VALUES(1,'ntfy',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO notification_outbox(channel_id,incident_id,event_type,payload,next_attempt_at) SELECT 1,id,'opened','{}',1 FROM incidents`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO notification_outbox(channel_id,event_type,payload,next_attempt_at) VALUES(1,'test','{}',1)`); err != nil {
		t.Fatal(err)
	}

	firstID := int64(1)
	if err = a.resetHistory(&firstID); err != nil {
		t.Fatalf("reset provider history: %v", err)
	}
	assertCount := func(query string, want int) {
		t.Helper()
		var got int
		if err := db.QueryRow(query).Scan(&got); err != nil || got != want {
			t.Fatalf("%s: count=%d err=%v, want %d", query, got, err, want)
		}
	}
	assertCount(`SELECT COUNT(*) FROM check_results WHERE monitor_id=1`, 0)
	assertCount(`SELECT COUNT(*) FROM incidents WHERE monitor_id=1`, 0)
	assertCount(`SELECT COUNT(*) FROM monitor_states WHERE monitor_id=1`, 0)
	assertCount(`SELECT COUNT(*) FROM check_results WHERE monitor_id=2`, 1)
	assertCount(`SELECT COUNT(*) FROM monitors`, 2)
	assertCount(`SELECT COUNT(*) FROM monitor_secrets`, 2)

	if err = a.resetHistory(nil); err != nil {
		t.Fatalf("reset all history: %v", err)
	}
	assertCount(`SELECT COUNT(*) FROM check_results`, 0)
	assertCount(`SELECT COUNT(*) FROM incidents`, 0)
	assertCount(`SELECT COUNT(*) FROM incident_events`, 0)
	assertCount(`SELECT COUNT(*) FROM monitor_states`, 0)
	assertCount(`SELECT COUNT(*) FROM notification_outbox WHERE incident_id IS NOT NULL`, 0)
	assertCount(`SELECT COUNT(*) FROM notification_outbox WHERE event_type='test'`, 1)
	assertCount(`SELECT COUNT(*) FROM monitors`, 2)
	assertCount(`SELECT COUNT(*) FROM monitor_secrets`, 2)
}

func TestUpdateMonitorRetriesImmediateCheckAfterRejection(t *testing.T) {
	for _, rejection := range []string{"overlap", "capacity"} {
		t.Run(rejection, func(t *testing.T) {
			db := migratedTestDB(t)
			monitorID := insertSyntheticMonitor(t, db, "torbox")
			a := &app{db: db, runs: newRunCoordinator(1)}
			now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
			if got := a.runs.Claim(monitorID, now, time.Hour); got != claimAccepted {
				t.Fatalf("initial monitor claim = %v, want accepted", got)
			}
			blockerID := monitorID
			if rejection == "capacity" {
				a.runs.Release(monitorID)
				blockerID = 99
				if got := a.runs.Claim(blockerID, now, time.Hour); got != claimAccepted {
					t.Fatalf("capacity holder claim = %v, want accepted", got)
				}
			}

			body := strings.NewReader(`{"name":"Updated","apiKey":"","enabled":true,"intervalSeconds":3600,"timeoutSeconds":15,"failureThreshold":3,"recoveryThreshold":2,"publicCheck":false}`)
			request := httptest.NewRequest(http.MethodPut, "/api/monitors/1", body)
			request.Header.Set("Content-Type", "application/json")
			request.SetPathValue("id", "1")
			response := httptest.NewRecorder()
			a.updateMonitor(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("update status = %d, body = %q", response.Code, response.Body.String())
			}

			a.runs.Release(blockerID)
			if got := a.runs.Claim(monitorID, now.Add(time.Minute), time.Hour); got != claimAccepted {
				t.Fatalf("post-update retry = %v, want accepted", got)
			}
		})
	}
}

func TestManualMonitorCheckDistinguishesOverlapFromCapacity(t *testing.T) {
	db := migratedTestDB(t)
	monitorID := insertSyntheticMonitor(t, db, "torbox")
	key := bytes.Repeat([]byte{1}, 32)
	a := &app{db: db, key: key, runs: newRunCoordinator(1)}
	nonce, ciphertext, err := a.encrypt([]byte("test-credential"), "monitor:1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO monitor_secrets(monitor_id,nonce,ciphertext,updated_at) VALUES(?,?,?,?)`, monitorID, nonce, ciphertext, 1); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if got := a.runs.Claim(monitorID, now, time.Hour); got != claimAccepted {
		t.Fatalf("monitor claim = %v, want accepted", got)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/monitors/1/test", nil)
	request.SetPathValue("id", "1")
	overlap := httptest.NewRecorder()
	a.testMonitor(overlap, request)
	if overlap.Code != http.StatusConflict {
		t.Fatalf("overlap status = %d, want %d", overlap.Code, http.StatusConflict)
	}
	a.runs.Release(monitorID)

	if got := a.runs.Claim(99, now, time.Hour); got != claimAccepted {
		t.Fatalf("capacity holder claim = %v, want accepted", got)
	}
	capacity := httptest.NewRecorder()
	a.testMonitor(capacity, request)
	if capacity.Code != http.StatusServiceUnavailable {
		t.Fatalf("capacity status = %d, want %d", capacity.Code, http.StatusServiceUnavailable)
	}
}
