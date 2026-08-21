package main

import (
	"io"
	"net/http"
	"net/http/httptest"
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
		{"authentication", checkResult{State: stateAuthFailed}, "The provider rejected the configured API key. Verify or replace the credential."},
		{"timeout", checkResult{State: stateConnection, ErrorCode: "timeout"}, "The authenticated API request timed out before the provider responded."},
		{"server status", checkResult{State: stateAPI, ErrorCode: "server_error", HTTPStatus: 503}, "The provider API returned HTTP 503, indicating a server-side failure."},
		{"invalid response", checkResult{State: stateAPI, ErrorCode: "invalid_response"}, "The provider API was reachable, but its response was invalid or could not be understood."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := incidentSummary(test.result); got != test.want {
				t.Fatalf("incidentSummary() = %q, want %q", got, test.want)
			}
		})
	}
}
