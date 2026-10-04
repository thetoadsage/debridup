package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHomepageConfig(t *testing.T) {
	for _, origin := range []string{"https://homepage.example", "http://localhost:3000", ""} {
		if _, err := parseHomepageConfig(strings.Repeat("x", 32), origin); err != nil {
			t.Fatal(err)
		}
	}
	for _, origin := range []string{"*", "https://example.com/", "https://example.com; https://evil.example", "https://user" + string(rune(64)) + "example.com", "javascript:alert(1)", "https://example.com?x=1"} {
		if _, err := parseHomepageConfig(strings.Repeat("x", 32), origin); err == nil {
			t.Fatalf("accepted %s", origin)
		}
	}
	if _, err := parseHomepageConfig("short", ""); err == nil {
		t.Fatal("accepted short credential")
	}
}

func TestHomepageAccessAndPayload(t *testing.T) {
	a := testApp(t)
	if err := migrateDatabase(context.Background(), a.db); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := a.db.Exec(`INSERT INTO monitors(id,provider,name,created_at,updated_at) VALUES(1,'torbox','<script>account</script>',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	credential := strings.Repeat("x", 32)
	request := func(path, auth, method string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		if auth != "" {
			r.Header.Set("Authorization", "Bearer "+auth)
		}
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		return w
	}
	if w := request("/integrations/homepage", "", "GET"); w.Code != 404 {
		t.Fatal(w.Code)
	}
	a.homepage = homepageConfig{credential, "https://homepage.example"}
	if w := request("/api/dashboard", credential, "GET"); w.Code != 303 || w.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal("integration credential authorized admin dashboard")
	}
	for _, tc := range []struct {
		path, auth, method string
		code               int
	}{
		{"/integrations/homepage", "", "GET", 401},
		{"/integrations/homepage?token=" + credential, "", "GET", 401},
		{"/integrations/homepage", "wrong", "GET", 401},
		{"/integrations/homepage?range=forever", credential, "GET", 400},
		{"/integrations/homepage/chart?metric=bad", credential, "GET", 400},
		{"/integrations/homepage", credential, "POST", 405},
	} {
		if w := request(tc.path, tc.auth, tc.method); w.Code != tc.code {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	for _, period := range []string{"24h", "7d", "30d"} {
		w := request("/integrations/homepage?range="+period, credential, "GET")
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		var result homepageResponse
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Range != period || len(result.Providers) != 1 || result.Providers[0].Availability != nil {
			t.Fatalf("unexpected payload: %+v", result)
		}
		for _, private := range []string{"incidents\"", "apiKey", credential} {
			if strings.Contains(w.Body.String(), private) {
				t.Fatalf("exposed %s", private)
			}
		}
		if w.Header().Get("X-Frame-Options") != "DENY" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("missing headers")
		}
	}
	w := request("/integrations/homepage/chart?token="+credential, "", "GET")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "No observations") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "<script>account") {
		t.Fatal("unescaped monitor name")
	}
	if w.Header().Get("X-Frame-Options") != "" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors https://homepage.example") {
		t.Fatal("incorrect frame policy")
	}
	a.homepage.origin = ""
	if w := request("/integrations/homepage/chart", credential, "GET"); w.Code != 404 {
		t.Fatal(w.Code)
	}
	a.db.Close()
	if w := request("/integrations/homepage", credential, "GET"); w.Code != 503 {
		t.Fatal(w.Code)
	}
}

func TestHomepageChartGapsAndStableColors(t *testing.T) {
	ms := int64(50)
	availability := 100.0
	provider := dashboardProvider{ID: 1, Name: "TorBox", Provider: "torbox", Series: []dashboardPoint{{P95MS: &ms, Availability: &availability}, {}, {P95MS: &ms, Availability: &availability}}}
	data := homepageResponse{Range: "24h", Providers: []dashboardProvider{provider, {ID: 2, Provider: "premiumize"}}}
	chart := makeHomepageChart(data, "p95")
	var rendered strings.Builder
	if err := homepageTemplate.Execute(&rendered, chart); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered.String(), "</html>") || strings.Contains(rendered.String(), "ZgotmplZ") {
		t.Fatal("invalid template output")
	}
	if !chart.HasData || chart.Maximum != "50" || strings.Count(chart.Lines[0].Path, "M") != 2 {
		t.Fatalf("lost gaps: %+v", chart)
	}
	if chart.Lines[0].Color == chart.Lines[1].Color {
		t.Fatal("provider colors collide")
	}
	data.Providers[0], data.Providers[1] = data.Providers[1], data.Providers[0]
	reordered := makeHomepageChart(data, "availability")
	if reordered.Maximum != "100" || reordered.Lines[1].Color != chart.Lines[0].Color {
		t.Fatal("unstable scale or color")
	}
}
