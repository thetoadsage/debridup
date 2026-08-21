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
	err := a.sendNtfy(server.URL, "DebridUp test", "It works", "white_check_mark", "test-1")
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
	err := a.sendNtfy(server.URL, "Test", "Test", "warning", "test-2")
	if err == nil || err.Error() != "ntfy returned HTTP 401" {
		t.Fatalf("expected safe HTTP status error, got %v", err)
	}
}
