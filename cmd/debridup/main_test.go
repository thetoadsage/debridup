package main

import (
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	a := &app{db: db}
	if err = a.migrate(); err != nil {
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
