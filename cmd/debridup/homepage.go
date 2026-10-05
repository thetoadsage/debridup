package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// This credential only authorizes the read-only integration, never admin APIs.
type homepageConfig struct{ token, origin string }

func parseHomepageConfig(token, origin string) (homepageConfig, error) {
	c := homepageConfig{token: token, origin: origin}
	if token != "" && len(token) < 32 {
		return c, fmt.Errorf("DEBRIDUP_HOMEPAGE_TOKEN must contain at least 32 characters")
	}
	if origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(origin, ";,* \t\r\n") {
			return c, fmt.Errorf("DEBRIDUP_HOMEPAGE_ORIGIN must be an http(s) origin without a trailing slash")
		}
	}
	return c, nil
}

func (a *app) homepageAuthorized(w http.ResponseWriter, r *http.Request, embed bool) bool {
	if a.homepage.token == "" || (embed && a.homepage.origin == "") {
		http.NotFound(w, r)
		return false
	}
	supplied := ""
	if header := r.Header.Get("Authorization"); strings.HasPrefix(header, "Bearer ") {
		supplied = strings.TrimPrefix(header, "Bearer ")
	}
	if embed && supplied == "" {
		supplied = r.URL.Query().Get("token")
	}
	expectedHash, suppliedHash := sha256.Sum256([]byte(a.homepage.token)), sha256.Sum256([]byte(supplied))
	if subtle.ConstantTimeCompare(expectedHash[:], suppliedHash[:]) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

type homepageResponse struct {
	GeneratedAt int64               `json:"generatedAt"`
	Range       string              `json:"range"`
	Summary     dashboardSummary    `json:"summary"`
	Providers   []dashboardProvider `json:"providers"`
}

func (a *app) homepageData(w http.ResponseWriter, r *http.Request) {
	a.serveHomepage(w, r, false)
}
func (a *app) homepageEmbed(w http.ResponseWriter, r *http.Request) {
	a.serveHomepage(w, r, true)
}
func (a *app) serveHomepage(w http.ResponseWriter, r *http.Request, embed bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.homepageAuthorized(w, r, embed) {
		return
	}
	rangeName := r.URL.Query().Get("range")
	if rangeName == "" {
		rangeName = "24h"
	}
	spec, err := parseDashboardRange(rangeName)
	if err != nil {
		http.Error(w, "range must be 24h, 7d or 30d", 400)
		return
	}
	metric := r.URL.Query().Get("metric")
	if metric == "" {
		metric = "p95"
	}
	if embed && metric != "p95" && metric != "availability" {
		http.Error(w, "metric must be p95 or availability", 400)
		return
	}
	snapshot, err := a.dashboardSnapshot(r.Context(), spec, time.Now())
	if err != nil {
		http.Error(w, "history unavailable", 503)
		return
	}
	// Incident descriptions and account configuration are intentionally excluded.
	payload := homepageResponse{snapshot.GeneratedAt, snapshot.Range, snapshot.Summary, snapshot.Providers}
	if !embed {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
		return
	}
	w.Header().Del("X-Frame-Options")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors "+a.homepage.origin)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = homepageTemplate.Execute(w, makeHomepageChart(payload, metric))
}

type homepageLine struct{ Name, Color, Path string }
type homepageChart struct {
	Title, Range, Maximum, Updated string
	Lines                          []homepageLine
	HasData                        bool
}

// Use one shared axis and preserve missing buckets as gaps in every series.
func makeHomepageChart(data homepageResponse, metric string) homepageChart {
	c := homepageChart{Title: "P95 latency (ms)", Range: data.Range, Updated: time.Unix(data.GeneratedAt, 0).UTC().Format("15:04 UTC"), Lines: []homepageLine{}}
	maxValue := 1.0
	value := func(p dashboardPoint) *float64 {
		if metric == "availability" {
			return p.Availability
		}
		if p.P95MS == nil {
			return nil
		}
		v := float64(*p.P95MS)
		return &v
	}
	if metric == "availability" {
		c.Title = "Availability (%)"
		maxValue = 100
	}
	for _, provider := range data.Providers {
		for _, p := range provider.Series {
			if v := value(p); v != nil {
				c.HasData = true
				if *v > maxValue {
					maxValue = *v
				}
			}
		}
	}
	palette := []string{"#60a5fa", "#fb923c", "#4ade80", "#c084fc", "#f472b6", "#22d3ee", "#facc15", "#f87171", "#a3e635", "#818cf8", "#2dd4bf"}
	for _, provider := range data.Providers {
		providers := []string{"torbox", "premiumize", "alldebrid", "realdebrid", "torrin", "pikpak", "offcloud", "debridlink", "easydebrid", "debrider", "deepbrid"}
		colorIndex := int(provider.ID % int64(len(palette)))
		if colorIndex < 0 {
			colorIndex = -colorIndex
		}
		for index, name := range providers {
			if name == provider.Provider {
				colorIndex = index
				break
			}
		}
		var path strings.Builder
		connected := false
		for j, p := range provider.Series {
			v := value(p)
			if v == nil {
				connected = false
				continue
			}
			x := 48.0
			if len(provider.Series) > 1 {
				x += float64(j) * 704 / float64(len(provider.Series)-1)
			}
			y := 184 - *v/maxValue*160
			command := "L"
			if !connected {
				command = "M"
			}
			fmt.Fprintf(&path, "%s%.2f %.2f ", command, x, y)
			// A tiny segment also makes isolated observations visible.
			if !connected {
				fmt.Fprintf(&path, "L%.2f %.2f ", x+0.1, y)
			}
			connected = true
		}
		c.Lines = append(c.Lines, homepageLine{provider.Name, palette[colorIndex], path.String()})
	}
	c.Maximum = fmt.Sprintf("%.0f", maxValue)
	return c
}

var homepageTemplate = template.Must(template.New("homepage").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>DebridUp · {{.Title}}</title><style>
:root{color-scheme:dark}*{box-sizing:border-box}body{margin:0;background:#101827;color:#e5e7eb;font:14px system-ui,sans-serif;padding:16px}header{display:flex;justify-content:space-between;gap:12px}strong{font-size:15px}small{color:#94a3b8}svg{display:block;width:100%;height:180px}text{fill:#94a3b8;font-size:12px}.legend{display:flex;flex-wrap:wrap;gap:8px 16px;font-size:12px}.legend span{display:inline-flex;align-items:center;gap:6px}.dot{width:8px;height:8px;border-radius:50%}footer{margin-top:10px;color:#94a3b8;font-size:11px}
</style></head><body><header><strong>{{.Title}}</strong><small>{{.Range}}</small></header>
{{if .HasData}}<svg viewBox="0 0 768 210" role="img" aria-label="{{.Title}} over {{.Range}}"><text x="0" y="29">{{.Maximum}}</text><text x="26" y="189">0</text><path d="M48 24H752 M48 104H752 M48 184H752" stroke="#273449" fill="none"/>
{{range .Lines}}<path d="{{.Path}}" stroke="{{.Color}}" fill="none" stroke-width="2.5" stroke-linecap="round"><title>{{.Name}}</title></path>{{end}}<text x="48" y="207">Earlier</text><text x="725" y="207">Now</text></svg>{{else}}<p>No observations in this range.</p>{{end}}
<div class="legend">{{range .Lines}}<span><i class="dot" style="background:{{.Color}}"></i>{{.Name}}</span>{{end}}</div><footer>DebridUp · Updated {{.Updated}} · Gaps indicate missing observations</footer></body></html>`))
