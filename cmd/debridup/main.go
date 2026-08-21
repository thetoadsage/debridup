package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
	_ "modernc.org/sqlite"
)

//go:embed web/*
var webFS embed.FS

const (
	stateHealthy    = "healthy"
	stateAuthFailed = "auth_failed"
	stateAPI        = "api_issue"
	stateConnection = "connection_issue"
)

type app struct {
	db        *sql.DB
	key       []byte
	client    *http.Client
	logger    *slog.Logger
	lastRuns  map[int64]time.Time
	runsMu    sync.Mutex
	cookieKey []byte
}

type monitor struct {
	ID                int64  `json:"id"`
	Provider          string `json:"provider"`
	Name              string `json:"name"`
	Enabled           bool   `json:"enabled"`
	IntervalSeconds   int    `json:"intervalSeconds"`
	TimeoutSeconds    int    `json:"timeoutSeconds"`
	FailureThreshold  int    `json:"failureThreshold"`
	RecoveryThreshold int    `json:"recoveryThreshold"`
	PublicCheck       bool   `json:"publicCheck"`
}

type checkResult struct {
	State       string
	DurationMS  int64
	HTTPStatus  int
	ErrorCode   string
	ErrorDetail string
	CheckedAt   time.Time
}

type providerDefinition struct {
	Endpoint       string
	PublicEndpoint string
	Method         string
}

var providerDefinitions = map[string]providerDefinition{
	"torbox":     {Endpoint: "https://api.torbox.app/v1/api/user/me", PublicEndpoint: "https://status.torbox.app/", Method: http.MethodGet},
	"premiumize": {Endpoint: "https://www.premiumize.me/api/account/info", PublicEndpoint: "https://premiumize.reamaze.com/status", Method: http.MethodGet},
	"alldebrid":  {Endpoint: "https://api.alldebrid.com/v4/user", PublicEndpoint: "https://api.alldebrid.com/v4/ping", Method: http.MethodGet},
	"realdebrid": {Endpoint: "https://api.real-debrid.com/rest/1.0/user", PublicEndpoint: "https://api.real-debrid.com/rest/1.0/time", Method: http.MethodGet},
	"torrin":     {Endpoint: "https://torrin.app/api/jobs", PublicEndpoint: "https://torrin.app/api/sites", Method: http.MethodGet},
	"pikpak":     {Endpoint: "https://user.mypikpak.com/v1/user/me", PublicEndpoint: "https://mypikpak.com/", Method: http.MethodGet},
	"offcloud":   {Endpoint: "https://offcloud.com/api/account/info", PublicEndpoint: "https://offcloud.com/", Method: http.MethodGet},
	"debridlink": {Endpoint: "https://debrid-link.com/api/v2/account/infos", PublicEndpoint: "https://www.debrid-link.com/webapp/status", Method: http.MethodGet},
	"easydebrid": {Endpoint: "https://easydebrid.com/api/v1/user/details", PublicEndpoint: "https://easydebrid.com/", Method: http.MethodGet},
	"debrider":   {Endpoint: "https://debrider.app/api/v1/tasks", PublicEndpoint: "https://stats.uptimerobot.com/shklobtEFJ/801337046", Method: http.MethodGet},
	"deepbrid":   {Endpoint: "https://www.deepbrid.com/api/v1/user", PublicEndpoint: "https://www.deepbrid.com/api/v1/hosts", Method: http.MethodGet},
}

type monitorState struct {
	Current          string
	StateSince       int64
	LastRaw          string
	FailureStreak    int
	RecoveryStreak   int
	FailureStartedAt int64
}

func main() {
	dataDir := env("DEBRIDUP_DATA_DIR", "./data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		panic(err)
	}
	key, err := loadKey()
	if err != nil {
		panic(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "debridup.db"))
	if err != nil {
		panic(err)
	}
	defer db.Close()
	cookieHash := sha256.Sum256(key)
	a := &app{db: db, key: key, cookieKey: cookieHash[:], client: &http.Client{Timeout: 65 * time.Second}, logger: slog.Default(), lastRuns: map[int64]time.Time{}}
	if err := a.migrate(); err != nil {
		panic(err)
	}
	if err := a.ensureAdmin(os.Getenv("DEBRIDUP_ADMIN_PASSWORD")); err != nil {
		panic(err)
	}
	go a.scheduler()
	go a.notificationWorker()
	addr := env("DEBRIDUP_ADDR", ":8080")
	a.logger.Info("DebridUp started", "addr", addr)
	server := &http.Server{Addr: addr, Handler: a.routes(), ReadHeaderTimeout: 10 * time.Second}
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		panic(err)
	}
}

func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func loadKey() ([]byte, error) {
	v := os.Getenv("DEBRIDUP_ENCRYPTION_KEY")
	if p := os.Getenv("DEBRIDUP_ENCRYPTION_KEY_FILE"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		v = string(b)
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
	if err != nil || len(b) != chacha20poly1305.KeySize {
		return nil, errors.New("DEBRIDUP_ENCRYPTION_KEY(_FILE) must be base64 for exactly 32 bytes")
	}
	return b, nil
}

func (a *app) migrate() error {
	_, err := a.db.Exec(`
PRAGMA foreign_keys = ON;
PRAGMA journal_mode = WAL;
PRAGMA busy_timeout = 5000;
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS monitors (
 id INTEGER PRIMARY KEY, provider TEXT NOT NULL CHECK(provider IN ('torbox','premiumize','alldebrid','realdebrid','torrin','pikpak','offcloud','debridlink','easydebrid','debrider','deepbrid')), name TEXT NOT NULL,
 enabled INTEGER NOT NULL DEFAULT 1, interval_seconds INTEGER NOT NULL DEFAULT 60, timeout_seconds INTEGER NOT NULL DEFAULT 15,
 failure_threshold INTEGER NOT NULL DEFAULT 3, recovery_threshold INTEGER NOT NULL DEFAULT 2, public_check INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS monitor_secrets (monitor_id INTEGER PRIMARY KEY REFERENCES monitors(id) ON DELETE CASCADE, nonce BLOB NOT NULL, ciphertext BLOB NOT NULL, key_version INTEGER NOT NULL DEFAULT 1, updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS monitor_states (
 monitor_id INTEGER PRIMARY KEY REFERENCES monitors(id) ON DELETE CASCADE, current_state TEXT NOT NULL DEFAULT 'healthy', state_since INTEGER NOT NULL,
 last_raw_state TEXT NOT NULL DEFAULT 'healthy', failure_streak INTEGER NOT NULL DEFAULT 0, recovery_streak INTEGER NOT NULL DEFAULT 0,
 failure_started_at INTEGER NOT NULL DEFAULT 0, last_check_at INTEGER
);
CREATE TABLE IF NOT EXISTS check_results (
 id INTEGER PRIMARY KEY, monitor_id INTEGER NOT NULL REFERENCES monitors(id) ON DELETE CASCADE, source TEXT NOT NULL CHECK(source IN ('authenticated','public')),
 state TEXT NOT NULL, duration_ms INTEGER NOT NULL, http_status INTEGER, error_code TEXT, error_detail TEXT, checked_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS check_results_monitor_time ON check_results(monitor_id, checked_at DESC);
CREATE TABLE IF NOT EXISTS incidents (
 id INTEGER PRIMARY KEY, monitor_id INTEGER NOT NULL REFERENCES monitors(id) ON DELETE CASCADE, opened_at INTEGER NOT NULL, detected_at INTEGER NOT NULL,
 resolved_at INTEGER, initial_state TEXT NOT NULL, latest_state TEXT NOT NULL, summary TEXT
);
CREATE INDEX IF NOT EXISTS incidents_monitor_time ON incidents(monitor_id, opened_at DESC);
CREATE TABLE IF NOT EXISTS incident_events (
 id INTEGER PRIMARY KEY, incident_id INTEGER NOT NULL REFERENCES incidents(id) ON DELETE CASCADE, type TEXT NOT NULL,
 previous_state TEXT, new_state TEXT NOT NULL, created_at INTEGER NOT NULL, check_id INTEGER REFERENCES check_results(id)
);
CREATE TABLE IF NOT EXISTS notification_channels (
 id INTEGER PRIMARY KEY, kind TEXT NOT NULL UNIQUE CHECK(kind='ntfy'), enabled INTEGER NOT NULL DEFAULT 0, nonce BLOB, ciphertext BLOB, updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS notification_outbox (
 id INTEGER PRIMARY KEY, channel_id INTEGER NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE, incident_id INTEGER REFERENCES incidents(id) ON DELETE CASCADE,
 event_type TEXT NOT NULL, payload TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending', attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at INTEGER NOT NULL, delivered_at INTEGER, last_error TEXT
);
CREATE INDEX IF NOT EXISTS outbox_pending ON notification_outbox(status, next_attempt_at);
`)
	if err != nil {
		return err
	}
	return a.migrateProviderConstraint()
}

func (a *app) migrateProviderConstraint() error {
	var schema string
	if err := a.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='monitors'`).Scan(&schema); err != nil {
		return err
	}
	if strings.Contains(schema, "'deepbrid'") {
		return nil
	}
	conn, err := a.db.Conn(context.Background())
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(context.Background(), `PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), `PRAGMA foreign_keys = ON`)
	tx, err := conn.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`
CREATE TABLE monitors_new (
 id INTEGER PRIMARY KEY, provider TEXT NOT NULL CHECK(provider IN ('torbox','premiumize','alldebrid','realdebrid','torrin','pikpak','offcloud','debridlink','easydebrid','debrider','deepbrid')), name TEXT NOT NULL,
 enabled INTEGER NOT NULL DEFAULT 1, interval_seconds INTEGER NOT NULL DEFAULT 60, timeout_seconds INTEGER NOT NULL DEFAULT 15,
 failure_threshold INTEGER NOT NULL DEFAULT 3, recovery_threshold INTEGER NOT NULL DEFAULT 2, public_check INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
INSERT INTO monitors_new SELECT * FROM monitors;
DROP TABLE monitors;
ALTER TABLE monitors_new RENAME TO monitors;
`); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *app) ensureAdmin(password string) error {
	var hash string
	err := a.db.QueryRow("SELECT value FROM settings WHERE key='admin_hash'").Scan(&hash)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if len(password) < 12 {
		return errors.New("DEBRIDUP_ADMIN_PASSWORD must be at least 12 characters on first startup")
	}
	_, err = a.db.Exec("INSERT INTO settings(key,value) VALUES('admin_hash',?)", hashPassword(password))
	return err
}

func hashPassword(password string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	hash := argon2.IDKey([]byte(password), salt, 3, 64*1024, 4, 32)
	return base64.RawStdEncoding.EncodeToString(salt) + ":" + base64.RawStdEncoding.EncodeToString(hash)
}
func verifyPassword(encoded, password string) bool {
	p := strings.Split(encoded, ":")
	if len(p) != 2 {
		return false
	}
	salt, e1 := base64.RawStdEncoding.DecodeString(p[0])
	expected, e2 := base64.RawStdEncoding.DecodeString(p[1])
	if e1 != nil || e2 != nil {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, 3, 64*1024, 4, uint32(len(expected)))
	return hmac.Equal(actual, expected)
}

func (a *app) encrypt(plain []byte, aad string) ([]byte, []byte, error) {
	c, err := chacha20poly1305.NewX(a.key)
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return nonce, c.Seal(nil, nonce, plain, []byte(aad)), nil
}
func (a *app) decrypt(nonce, ciphertext []byte, aad string) ([]byte, error) {
	c, err := chacha20poly1305.NewX(a.key)
	if err != nil {
		return nil, err
	}
	return c.Open(nil, nonce, ciphertext, []byte(aad))
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login", a.login)
	mux.HandleFunc("POST /logout", a.logout)
	mux.HandleFunc("GET /api/overview", a.auth(a.overview))
	mux.HandleFunc("GET /api/monitors", a.auth(a.listMonitors))
	mux.HandleFunc("POST /api/monitors", a.auth(a.createMonitor))
	mux.HandleFunc("PUT /api/monitors/{id}", a.auth(a.updateMonitor))
	mux.HandleFunc("DELETE /api/monitors/{id}", a.auth(a.deleteMonitor))
	mux.HandleFunc("POST /api/monitors/{id}/test", a.auth(a.testMonitor))
	mux.HandleFunc("GET /api/monitors/{id}/checks", a.auth(a.listChecks))
	mux.HandleFunc("GET /api/incidents", a.auth(a.listIncidents))
	mux.HandleFunc("GET /api/notifications/ntfy", a.auth(a.getNtfy))
	mux.HandleFunc("PUT /api/notifications/ntfy", a.auth(a.putNtfy))
	mux.HandleFunc("POST /api/notifications/ntfy/test", a.auth(a.testNtfy))
	static, _ := fs.Sub(webFS, "web")
	files := http.FileServer(http.FS(static))
	mux.Handle("GET /login.html", files)
	mux.Handle("GET /login.js", files)
	mux.Handle("GET /app.css", files)
	mux.Handle("/", a.auth(func(w http.ResponseWriter, r *http.Request) { files.ServeHTTP(w, r) }))
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (a *app) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.sessionOK(r) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
			} else {
				http.Redirect(w, r, "/login.html", http.StatusSeeOther)
			}
			return
		}
		next(w, r)
	}
}
func (a *app) sessionOK(r *http.Request) bool {
	c, err := r.Cookie("debridup_session")
	if err != nil {
		return false
	}
	p := strings.Split(c.Value, ".")
	if len(p) != 2 {
		return false
	}
	data, err := base64.RawURLEncoding.DecodeString(p[0])
	if err != nil {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(p[1])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, a.cookieKey)
	mac.Write(data)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return false
	}
	until, err := strconv.ParseInt(string(data), 10, 64)
	return err == nil && time.Now().Unix() < until
}
func (a *app) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	var hash string
	_ = a.db.QueryRow("SELECT value FROM settings WHERE key='admin_hash'").Scan(&hash)
	if !verifyPassword(hash, in.Password) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}
	data := []byte(strconv.FormatInt(time.Now().Add(24*time.Hour).Unix(), 10))
	mac := hmac.New(sha256.New, a.cookieKey)
	mac.Write(data)
	value := base64.RawURLEncoding.EncodeToString(data) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	http.SetCookie(w, &http.Cookie{Name: "debridup_session", Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 86400, Secure: r.TLS != nil})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
func (a *app) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "debridup_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Header.Get("Content-Type") != "application/json" {
		writeJSON(w, 415, map[string]string{"error": "Content-Type must be application/json"})
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid JSON"})
		return false
	}
	return true
}
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *app) scheduler() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		a.runDueMonitors()
		<-t.C
	}
}
func (a *app) runDueMonitors() {
	monitors, err := a.monitors()
	if err != nil {
		a.logger.Error("load monitors", "error", err)
		return
	}
	now := time.Now()
	for _, m := range monitors {
		if !m.Enabled {
			continue
		}
		a.runsMu.Lock()
		last := a.lastRuns[m.ID]
		due := last.IsZero() || now.Sub(last) >= time.Duration(m.IntervalSeconds)*time.Second
		if due {
			a.lastRuns[m.ID] = now
		}
		a.runsMu.Unlock()
		if due {
			go a.runMonitor(m)
		}
	}
}
func (a *app) runMonitor(m monitor) {
	secret, err := a.monitorSecret(m.ID)
	if err != nil {
		a.logger.Error("load monitor secret", "monitor", m.ID, "error", err)
		return
	}
	r := a.authCheck(m, secret)
	if _, err = a.recordResult(m, "authenticated", r); err != nil {
		a.logger.Error("record check", "monitor", m.ID, "error", err)
	}
	if m.PublicCheck {
		pr := a.publicCheck(m)
		if _, err = a.recordResult(m, "public", pr); err != nil {
			a.logger.Error("record public check", "monitor", m.ID, "error", err)
		}
	}
}

func (a *app) authCheck(m monitor, credential string) checkResult {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(m.TimeoutSeconds)*time.Second)
	defer cancel()
	provider, ok := providerDefinitions[m.Provider]
	if !ok {
		return checkResult{State: stateAPI, ErrorCode: "unsupported_provider", CheckedAt: started}
	}
	req, err := http.NewRequestWithContext(ctx, provider.Method, provider.Endpoint, nil)
	if err != nil {
		return checkResult{State: stateConnection, ErrorCode: "request_build", CheckedAt: started}
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "DebridUp/1.0")
	resp, err := a.client.Do(req)
	duration := time.Since(started).Milliseconds()
	if err != nil {
		return checkResult{State: stateConnection, DurationMS: duration, ErrorCode: transportCode(err), ErrorDetail: "request could not be completed", CheckedAt: started}
	}
	defer resp.Body.Close()
	if resp.Request != nil && (resp.Request.URL.Host != req.URL.Host || resp.Request.URL.Path != req.URL.Path) {
		return checkResult{State: stateAuthFailed, DurationMS: duration, HTTPStatus: resp.StatusCode, ErrorCode: "authentication_redirect", CheckedAt: started}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128<<10))
	if err != nil {
		return checkResult{State: stateConnection, DurationMS: duration, HTTPStatus: resp.StatusCode, ErrorCode: "read_error", CheckedAt: started}
	}
	r := checkResult{DurationMS: duration, HTTPStatus: resp.StatusCode, CheckedAt: started}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		r.State = stateAuthFailed
		r.ErrorCode = "authentication_rejected"
		return r
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		r.State = stateAPI
		r.ErrorCode = "rate_limited"
		return r
	}
	if resp.StatusCode >= 500 {
		r.State = stateAPI
		r.ErrorCode = "server_error"
		return r
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		r.State = stateAPI
		r.ErrorCode = "unexpected_status"
		return r
	}
	var payload any
	if json.Unmarshal(body, &payload) != nil {
		r.State = stateAPI
		r.ErrorCode = "invalid_response"
		return r
	}
	stateValue, code := classifyProviderPayload(m.Provider, payload)
	if stateValue == "" {
		r.State = stateHealthy
		return r
	}
	r.State = stateValue
	r.ErrorCode = code
	return r
}

func classifyProviderPayload(provider string, payload any) (string, string) {
	object, isObject := payload.(map[string]any)
	if !isObject {
		return "", ""
	}
	switch provider {
	case "torbox":
		if success, present := object["success"].(bool); present && !success {
			return classifyPayloadError(object)
		}
	case "premiumize", "alldebrid":
		if status, present := object["status"].(string); present && !strings.EqualFold(status, "success") {
			return classifyPayloadError(object)
		}
	case "debridlink":
		if success, present := object["success"].(bool); present && !success {
			return classifyPayloadError(object)
		}
	case "deepbrid":
		if errorValue, present := object["error"].(float64); present && errorValue != 0 {
			return classifyPayloadError(object)
		}
	case "offcloud":
		if _, present := object["error"]; present {
			return classifyPayloadError(object)
		}
	}
	return "", ""
}

func classifyPayloadError(payload map[string]any) (string, string) {
	encoded, _ := json.Marshal(payload)
	message := strings.ToLower(string(encoded))
	for _, marker := range []string{"auth", "apikey", "api key", "token", "credential", "unauthor", "logged in", "login"} {
		if strings.Contains(message, marker) {
			return stateAuthFailed, "authentication_rejected"
		}
	}
	return stateAPI, "api_error"
}

func (a *app) publicCheck(m monitor) checkResult {
	started := time.Now()
	provider, ok := providerDefinitions[m.Provider]
	if !ok || provider.PublicEndpoint == "" {
		return checkResult{State: stateAPI, ErrorCode: "unsupported_provider", CheckedAt: started}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(m.TimeoutSeconds)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, provider.PublicEndpoint, nil)
	if err != nil {
		return checkResult{State: stateConnection, ErrorCode: "request_build", CheckedAt: started}
	}
	req.Header.Set("User-Agent", "DebridUp/1.0")
	resp, err := a.client.Do(req)
	duration := time.Since(started).Milliseconds()
	if err != nil {
		return checkResult{State: stateConnection, DurationMS: duration, ErrorCode: transportCode(err), CheckedAt: started}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return checkResult{State: stateAPI, DurationMS: duration, HTTPStatus: resp.StatusCode, ErrorCode: "status_page_error", CheckedAt: started}
	}
	return checkResult{State: stateHealthy, DurationMS: duration, HTTPStatus: resp.StatusCode, CheckedAt: started}
}
func transportCode(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "connection_error"
}

func (a *app) recordResult(m monitor, source string, r checkResult) (int64, error) {
	tx, err := a.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO check_results(monitor_id,source,state,duration_ms,http_status,error_code,error_detail,checked_at) VALUES(?,?,?,?,?,?,?,?)`, m.ID, source, r.State, r.DurationMS, nullInt(r.HTTPStatus), nullString(r.ErrorCode), nullString(r.ErrorDetail), r.CheckedAt.Unix())
	if err != nil {
		return 0, err
	}
	checkID, _ := res.LastInsertId()
	if source != "authenticated" {
		return checkID, tx.Commit()
	}
	ms, err := stateFor(tx, m.ID, r.CheckedAt.Unix())
	if err != nil {
		return 0, err
	}
	var eventType string
	var incidentID int64
	previous := ms.Current
	if r.State == stateHealthy {
		ms.FailureStreak = 0
		ms.FailureStartedAt = 0
		ms.LastRaw = stateHealthy
		if ms.Current != stateHealthy {
			ms.RecoveryStreak++
			if ms.RecoveryStreak >= m.RecoveryThreshold {
				ms.Current = stateHealthy
				ms.StateSince = r.CheckedAt.Unix()
				ms.RecoveryStreak = 0
				eventType = "recovered"
				incidentID, err = resolveIncident(tx, m.ID, r.CheckedAt.Unix(), checkID)
				if err != nil {
					return 0, err
				}
			}
		} else {
			ms.RecoveryStreak = 0
		}
	} else {
		ms.RecoveryStreak = 0
		if ms.LastRaw == r.State {
			ms.FailureStreak++
		} else {
			ms.FailureStreak = 1
			ms.FailureStartedAt = r.CheckedAt.Unix()
		}
		ms.LastRaw = r.State
		if ms.Current == stateHealthy && ms.FailureStreak >= m.FailureThreshold {
			ms.Current = r.State
			ms.StateSince = ms.FailureStartedAt
			eventType = "opened"
			incidentID, err = openIncident(tx, m.ID, ms.FailureStartedAt, r.CheckedAt.Unix(), r.State, incidentSummary(r), checkID)
			if err != nil {
				return 0, err
			}
		} else if ms.Current != stateHealthy && ms.Current != r.State {
			ms.Current = r.State
			ms.StateSince = r.CheckedAt.Unix()
			eventType = "state_changed"
			incidentID, err = changeIncident(tx, m.ID, previous, r.State, incidentSummary(r), r.CheckedAt.Unix(), checkID)
			if err != nil {
				return 0, err
			}
		}
	}
	_, err = tx.Exec(`UPDATE monitor_states SET current_state=?,state_since=?,last_raw_state=?,failure_streak=?,recovery_streak=?,failure_started_at=?,last_check_at=? WHERE monitor_id=?`, ms.Current, ms.StateSince, ms.LastRaw, ms.FailureStreak, ms.RecoveryStreak, ms.FailureStartedAt, r.CheckedAt.Unix(), m.ID)
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	if eventType != "" {
		a.enqueueNotification(incidentID, eventType, m.Name, previous, ms.Current, incidentSummary(r), r.CheckedAt)
	}
	return checkID, nil
}
func nullInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}
func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}
func stateFor(tx *sql.Tx, id int64, now int64) (monitorState, error) {
	var s monitorState
	err := tx.QueryRow(`SELECT current_state,state_since,last_raw_state,failure_streak,recovery_streak,failure_started_at FROM monitor_states WHERE monitor_id=?`, id).Scan(&s.Current, &s.StateSince, &s.LastRaw, &s.FailureStreak, &s.RecoveryStreak, &s.FailureStartedAt)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.Exec(`INSERT INTO monitor_states(monitor_id,current_state,state_since,last_raw_state) VALUES(?,?,?,?)`, id, stateHealthy, now, stateHealthy)
		return monitorState{Current: stateHealthy, StateSince: now, LastRaw: stateHealthy}, err
	}
	return s, err
}
func openIncident(tx *sql.Tx, monitorID, opened, detected int64, state, summary string, checkID int64) (int64, error) {
	res, err := tx.Exec(`INSERT INTO incidents(monitor_id,opened_at,detected_at,initial_state,latest_state,summary) VALUES(?,?,?,?,?,?)`, monitorID, opened, detected, state, state, summary)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	_, err = tx.Exec(`INSERT INTO incident_events(incident_id,type,new_state,created_at,check_id) VALUES(?,?,?,?,?)`, id, "opened", state, detected, checkID)
	return id, err
}
func resolveIncident(tx *sql.Tx, monitorID, at, checkID int64) (int64, error) {
	var id int64
	err := tx.QueryRow(`SELECT id FROM incidents WHERE monitor_id=? AND resolved_at IS NULL ORDER BY id DESC LIMIT 1`, monitorID).Scan(&id)
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(`UPDATE incidents SET resolved_at=? WHERE id=?`, at, id)
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(`INSERT INTO incident_events(incident_id,type,new_state,created_at,check_id) VALUES(?,?,?,?,?)`, id, "recovered", stateHealthy, at, checkID)
	return id, err
}
func changeIncident(tx *sql.Tx, monitorID int64, previous, next, summary string, at, checkID int64) (int64, error) {
	var id int64
	err := tx.QueryRow(`SELECT id FROM incidents WHERE monitor_id=? AND resolved_at IS NULL ORDER BY id DESC LIMIT 1`, monitorID).Scan(&id)
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(`UPDATE incidents SET latest_state=?,summary=? WHERE id=?`, next, summary, id)
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(`INSERT INTO incident_events(incident_id,type,previous_state,new_state,created_at,check_id) VALUES(?,?,?,?,?,?)`, id, "state_changed", previous, next, at, checkID)
	return id, err
}

func incidentSummary(r checkResult) string {
	switch r.State {
	case stateAuthFailed:
		return "The provider rejected the configured credential. Verify or replace it."
	case stateConnection:
		if r.ErrorCode == "timeout" {
			return "The authenticated API request timed out before the provider responded."
		}
		return "DebridUp could not establish a connection to the provider API."
	case stateAPI:
		switch r.ErrorCode {
		case "server_error":
			if r.HTTPStatus != 0 {
				return fmt.Sprintf("The provider API returned HTTP %d, indicating a server-side failure.", r.HTTPStatus)
			}
			return "The provider API reported a server-side failure."
		case "unexpected_status":
			if r.HTTPStatus != 0 {
				return fmt.Sprintf("The provider API returned the unexpected HTTP status %d.", r.HTTPStatus)
			}
			return "The provider API returned an unexpected HTTP status."
		case "invalid_response":
			return "The provider API was reachable, but its response was invalid or could not be understood."
		case "api_error":
			return "The provider API returned an application-level error."
		case "rate_limited":
			return "The provider API rate-limited the authenticated health check."
		default:
			return "The provider API was reachable but did not complete the health check successfully."
		}
	case stateHealthy:
		return "The authenticated provider check succeeded again."
	default:
		return "The authenticated provider check failed."
	}
}

func incidentStateDescription(state string) string {
	switch state {
	case stateAuthFailed:
		return "Authentication is failing. Verify or replace the configured credential."
	case stateConnection:
		return "DebridUp could not connect to the provider API."
	case stateAPI:
		return "The provider API returned an error or invalid response."
	default:
		return "The authenticated provider check failed."
	}
}

func (a *app) monitors() ([]monitor, error) {
	rows, err := a.db.Query(`SELECT id,provider,name,enabled,interval_seconds,timeout_seconds,failure_threshold,recovery_threshold,public_check FROM monitors ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []monitor
	for rows.Next() {
		var m monitor
		var enabled, public int
		if err = rows.Scan(&m.ID, &m.Provider, &m.Name, &enabled, &m.IntervalSeconds, &m.TimeoutSeconds, &m.FailureThreshold, &m.RecoveryThreshold, &public); err != nil {
			return nil, err
		}
		m.Enabled = enabled == 1
		m.PublicCheck = public == 1
		out = append(out, m)
	}
	return out, rows.Err()
}
func (a *app) monitorByID(id int64) (monitor, error) {
	var m monitor
	var enabled, public int
	err := a.db.QueryRow(`SELECT id,provider,name,enabled,interval_seconds,timeout_seconds,failure_threshold,recovery_threshold,public_check FROM monitors WHERE id=?`, id).Scan(&m.ID, &m.Provider, &m.Name, &enabled, &m.IntervalSeconds, &m.TimeoutSeconds, &m.FailureThreshold, &m.RecoveryThreshold, &public)
	m.Enabled = enabled == 1
	m.PublicCheck = public == 1
	return m, err
}
func (a *app) monitorSecret(id int64) (string, error) {
	var nonce, ciphertext []byte
	err := a.db.QueryRow(`SELECT nonce,ciphertext FROM monitor_secrets WHERE monitor_id=?`, id).Scan(&nonce, &ciphertext)
	if err != nil {
		return "", err
	}
	plain, err := a.decrypt(nonce, ciphertext, "monitor:"+strconv.FormatInt(id, 10))
	return string(plain), err
}

func (a *app) listMonitors(w http.ResponseWriter, r *http.Request) {
	ms, err := a.monitors()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not load monitors"})
		return
	}
	type item struct {
		monitor
		State      string `json:"state"`
		StateSince *int64 `json:"stateSince"`
		LastCheck  *int64 `json:"lastCheck"`
		Configured bool   `json:"configured"`
	}
	out := make([]item, 0, len(ms))
	for _, m := range ms {
		var i item
		i.monitor = m
		var stateSince, lastCheck sql.NullInt64
		_ = a.db.QueryRow(`SELECT current_state,state_since,last_check_at FROM monitor_states WHERE monitor_id=?`, m.ID).Scan(&i.State, &stateSince, &lastCheck)
		if stateSince.Valid {
			v := stateSince.Int64
			i.StateSince = &v
		}
		if lastCheck.Valid {
			v := lastCheck.Int64
			i.LastCheck = &v
		}
		var n int
		_ = a.db.QueryRow(`SELECT COUNT(*) FROM monitor_secrets WHERE monitor_id=?`, m.ID).Scan(&n)
		i.Configured = n > 0
		if i.State == "" {
			i.State = "checking"
		}
		out = append(out, i)
	}
	writeJSON(w, 200, out)
}
func (a *app) createMonitor(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Provider          string `json:"provider"`
		Name              string `json:"name"`
		APIKey            string `json:"apiKey"`
		IntervalSeconds   int    `json:"intervalSeconds"`
		TimeoutSeconds    int    `json:"timeoutSeconds"`
		FailureThreshold  int    `json:"failureThreshold"`
		RecoveryThreshold int    `json:"recoveryThreshold"`
		PublicCheck       bool   `json:"publicCheck"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	in.Provider = strings.ToLower(strings.TrimSpace(in.Provider))
	if _, supported := providerDefinitions[in.Provider]; !supported {
		writeJSON(w, 400, map[string]string{"error": "unsupported provider"})
		return
	}
	if len(strings.TrimSpace(in.Name)) < 1 || len(in.Name) > 80 || len(strings.TrimSpace(in.APIKey)) < 8 {
		writeJSON(w, 400, map[string]string{"error": "name and API key are required"})
		return
	}
	if in.IntervalSeconds == 0 {
		in.IntervalSeconds = 60
	}
	if in.TimeoutSeconds == 0 {
		in.TimeoutSeconds = 15
	}
	if in.FailureThreshold == 0 {
		in.FailureThreshold = 3
	}
	if in.RecoveryThreshold == 0 {
		in.RecoveryThreshold = 2
	}
	if in.IntervalSeconds < 15 || in.IntervalSeconds > 3600 || in.TimeoutSeconds < 3 || in.TimeoutSeconds > 60 || in.FailureThreshold < 1 || in.FailureThreshold > 20 || in.RecoveryThreshold < 1 || in.RecoveryThreshold > 20 {
		writeJSON(w, 400, map[string]string{"error": "monitor values are out of range"})
		return
	}
	now := time.Now().Unix()
	res, err := a.db.Exec(`INSERT INTO monitors(provider,name,interval_seconds,timeout_seconds,failure_threshold,recovery_threshold,public_check,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, in.Provider, in.Name, in.IntervalSeconds, in.TimeoutSeconds, in.FailureThreshold, in.RecoveryThreshold, boolInt(in.PublicCheck), now, now)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not create monitor"})
		return
	}
	id, _ := res.LastInsertId()
	nonce, cipher, err := a.encrypt([]byte(strings.TrimSpace(in.APIKey)), "monitor:"+strconv.FormatInt(id, 10))
	if err == nil {
		_, err = a.db.Exec(`INSERT INTO monitor_secrets(monitor_id,nonce,ciphertext,updated_at) VALUES(?,?,?,?)`, id, nonce, cipher, now)
	}
	if err != nil {
		_, _ = a.db.Exec(`DELETE FROM monitors WHERE id=?`, id)
		writeJSON(w, 500, map[string]string{"error": "could not store credential"})
		return
	}
	m, _ := a.monitorByID(id)
	go a.runMonitor(m)
	writeJSON(w, 201, map[string]any{"id": id, "configured": true})
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func monitorID(r *http.Request) (int64, error) { return strconv.ParseInt(r.PathValue("id"), 10, 64) }
func (a *app) updateMonitor(w http.ResponseWriter, r *http.Request) {
	id, err := monitorID(r)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid monitor id"})
		return
	}
	var in struct {
		Name              string `json:"name"`
		APIKey            string `json:"apiKey"`
		Enabled           bool   `json:"enabled"`
		IntervalSeconds   int    `json:"intervalSeconds"`
		TimeoutSeconds    int    `json:"timeoutSeconds"`
		FailureThreshold  int    `json:"failureThreshold"`
		RecoveryThreshold int    `json:"recoveryThreshold"`
		PublicCheck       bool   `json:"publicCheck"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	in.APIKey = strings.TrimSpace(in.APIKey)
	if len(in.Name) < 1 || len(in.Name) > 80 {
		writeJSON(w, 400, map[string]string{"error": "name is required and must be 80 characters or fewer"})
		return
	}
	if in.APIKey != "" && len(in.APIKey) < 8 {
		writeJSON(w, 400, map[string]string{"error": "replacement API key is too short"})
		return
	}
	if in.IntervalSeconds < 15 || in.IntervalSeconds > 3600 || in.TimeoutSeconds < 3 || in.TimeoutSeconds > 60 || in.FailureThreshold < 1 || in.FailureThreshold > 20 || in.RecoveryThreshold < 1 || in.RecoveryThreshold > 20 {
		writeJSON(w, 400, map[string]string{"error": "monitor values are out of range"})
		return
	}
	var nonce, cipher []byte
	if in.APIKey != "" {
		nonce, cipher, err = a.encrypt([]byte(in.APIKey), "monitor:"+strconv.FormatInt(id, 10))
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "could not secure replacement credential"})
			return
		}
	}
	tx, err := a.db.Begin()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not update monitor"})
		return
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE monitors SET name=?,enabled=?,interval_seconds=?,timeout_seconds=?,failure_threshold=?,recovery_threshold=?,public_check=?,updated_at=? WHERE id=?`, in.Name, boolInt(in.Enabled), in.IntervalSeconds, in.TimeoutSeconds, in.FailureThreshold, in.RecoveryThreshold, boolInt(in.PublicCheck), time.Now().Unix(), id)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not update monitor"})
		return
	}
	updated, _ := result.RowsAffected()
	if updated == 0 {
		writeJSON(w, 404, map[string]string{"error": "monitor not found"})
		return
	}
	if in.APIKey != "" {
		_, err = tx.Exec(`INSERT INTO monitor_secrets(monitor_id,nonce,ciphertext,updated_at) VALUES(?,?,?,?) ON CONFLICT(monitor_id) DO UPDATE SET nonce=excluded.nonce,ciphertext=excluded.ciphertext,updated_at=excluded.updated_at`, id, nonce, cipher, time.Now().Unix())
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "could not update credential"})
			return
		}
	}
	if err = tx.Commit(); err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not update monitor"})
		return
	}
	if in.Enabled {
		m, loadErr := a.monitorByID(id)
		if loadErr == nil {
			a.runsMu.Lock()
			delete(a.lastRuns, id)
			a.runsMu.Unlock()
			go a.runMonitor(m)
		}
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *app) deleteMonitor(w http.ResponseWriter, r *http.Request) {
	id, err := monitorID(r)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid monitor id"})
		return
	}
	tx, err := a.db.Begin()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not delete monitor"})
		return
	}
	defer tx.Rollback()
	statements := []string{
		`DELETE FROM notification_outbox WHERE incident_id IN (SELECT id FROM incidents WHERE monitor_id=?)`,
		`DELETE FROM incident_events WHERE incident_id IN (SELECT id FROM incidents WHERE monitor_id=?)`,
		`DELETE FROM incidents WHERE monitor_id=?`,
		`DELETE FROM check_results WHERE monitor_id=?`,
		`DELETE FROM monitor_states WHERE monitor_id=?`,
		`DELETE FROM monitor_secrets WHERE monitor_id=?`,
	}
	for _, statement := range statements {
		if _, err = tx.Exec(statement, id); err != nil {
			writeJSON(w, 500, map[string]string{"error": "could not delete monitor history"})
			return
		}
	}
	result, err := tx.Exec(`DELETE FROM monitors WHERE id=?`, id)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not delete monitor"})
		return
	}
	deleted, _ := result.RowsAffected()
	if deleted == 0 {
		writeJSON(w, 404, map[string]string{"error": "monitor not found"})
		return
	}
	if err = tx.Commit(); err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not delete monitor"})
		return
	}
	a.runsMu.Lock()
	delete(a.lastRuns, id)
	a.runsMu.Unlock()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *app) testMonitor(w http.ResponseWriter, r *http.Request) {
	id, err := monitorID(r)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid monitor id"})
		return
	}
	m, err := a.monitorByID(id)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "monitor not found"})
		return
	}
	key, err := a.monitorSecret(id)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "monitor has no credential"})
		return
	}
	result := a.authCheck(m, key)
	_, err = a.recordResult(m, "authenticated", result)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not record check"})
		return
	}
	writeJSON(w, 200, result)
}
func (a *app) listChecks(w http.ResponseWriter, r *http.Request) {
	id, err := monitorID(r)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid monitor id"})
		return
	}
	rows, err := a.db.Query(`SELECT source,state,duration_ms,http_status,error_code,error_detail,checked_at FROM check_results WHERE monitor_id=? ORDER BY checked_at DESC LIMIT 300`, id)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not load checks"})
		return
	}
	defer rows.Close()
	type row struct {
		Source, State string
		DurationMS    int64
		HTTPStatus    *int    `json:"httpStatus"`
		ErrorCode     *string `json:"errorCode"`
		ErrorDetail   *string `json:"errorDetail"`
		CheckedAt     int64   `json:"checkedAt"`
	}
	out := make([]row, 0)
	for rows.Next() {
		var x row
		var httpStatus sql.NullInt64
		var code, detail sql.NullString
		if err = rows.Scan(&x.Source, &x.State, &x.DurationMS, &httpStatus, &code, &detail, &x.CheckedAt); err != nil {
			writeJSON(w, 500, map[string]string{"error": "could not load checks"})
			return
		}
		if httpStatus.Valid {
			v := int(httpStatus.Int64)
			x.HTTPStatus = &v
		}
		if code.Valid {
			v := code.String
			x.ErrorCode = &v
		}
		if detail.Valid {
			v := detail.String
			x.ErrorDetail = &v
		}
		out = append(out, x)
	}
	writeJSON(w, 200, out)
}
func (a *app) overview(w http.ResponseWriter, r *http.Request) {
	ms, err := a.monitors()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not load overview"})
		return
	}
	type item struct {
		ID                    int64 `json:"id"`
		Name, Provider, State string
		LastCheck             *int64   `json:"lastCheck"`
		Availability          *float64 `json:"availability"`
		Coverage              *float64 `json:"coverage"`
		P95MS                 *int64   `json:"p95Ms"`
		OpenIncident          bool     `json:"openIncident"`
	}
	out := make([]item, 0, len(ms))
	cutoff := time.Now().Add(-30 * 24 * time.Hour).Unix()
	for _, m := range ms {
		v := item{ID: m.ID, Name: m.Name, Provider: m.Provider, State: "checking"}
		var last sql.NullInt64
		_ = a.db.QueryRow(`SELECT current_state,last_check_at FROM monitor_states WHERE monitor_id=?`, m.ID).Scan(&v.State, &last)
		if last.Valid {
			x := last.Int64
			v.LastCheck = &x
		}
		var total, eligible, good int
		_ = a.db.QueryRow(`SELECT COUNT(*),SUM(CASE WHEN state!='auth_failed' THEN 1 ELSE 0 END),SUM(CASE WHEN state='healthy' THEN 1 ELSE 0 END) FROM check_results WHERE monitor_id=? AND source='authenticated' AND checked_at>=?`, m.ID, cutoff).Scan(&total, &eligible, &good)
		if eligible > 0 {
			x := float64(good) / float64(eligible) * 100
			v.Availability = &x
		}
		if total > 0 {
			x := float64(eligible) / float64(total) * 100
			v.Coverage = &x
		}
		latencyRows, _ := a.db.Query(`SELECT duration_ms FROM check_results WHERE monitor_id=? AND source='authenticated' AND checked_at>=? AND state='healthy' ORDER BY checked_at DESC LIMIT 1000`, m.ID, cutoff)
		var latencies []int64
		if latencyRows != nil {
			for latencyRows.Next() {
				var ms int64
				if latencyRows.Scan(&ms) == nil {
					latencies = append(latencies, ms)
				}
			}
			latencyRows.Close()
		}
		if len(latencies) > 0 {
			sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
			p := latencies[(len(latencies)*95+99)/100-1]
			v.P95MS = &p
		}
		var count int
		_ = a.db.QueryRow(`SELECT COUNT(*) FROM incidents WHERE monitor_id=? AND resolved_at IS NULL`, m.ID).Scan(&count)
		v.OpenIncident = count > 0
		out = append(out, v)
	}
	writeJSON(w, 200, map[string]any{"monitors": out, "generatedAt": time.Now().Unix()})
}
func (a *app) listIncidents(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT i.id,m.name,m.provider,i.opened_at,i.detected_at,i.resolved_at,i.initial_state,i.latest_state,i.summary FROM incidents i JOIN monitors m ON m.id=i.monitor_id ORDER BY i.opened_at DESC LIMIT 200`)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not load incidents"})
		return
	}
	type incidentEvent struct {
		Type      string `json:"type"`
		State     string `json:"state"`
		Summary   string `json:"summary"`
		CreatedAt int64  `json:"createdAt"`
	}
	type incident struct {
		ID                        int64 `json:"id"`
		Name, Provider            string
		OpenedAt, DetectedAt      int64
		ResolvedAt                *int64 `json:"resolvedAt"`
		InitialState, LatestState string
		Summary                   string          `json:"summary"`
		Events                    []incidentEvent `json:"events"`
	}
	out := make([]incident, 0)
	for rows.Next() {
		var x incident
		var resolved sql.NullInt64
		var summary sql.NullString
		if err = rows.Scan(&x.ID, &x.Name, &x.Provider, &x.OpenedAt, &x.DetectedAt, &resolved, &x.InitialState, &x.LatestState, &summary); err != nil {
			rows.Close()
			writeJSON(w, 500, map[string]string{"error": "could not load incidents"})
			return
		}
		if resolved.Valid {
			v := resolved.Int64
			x.ResolvedAt = &v
			if x.LatestState == stateHealthy {
				x.LatestState = x.InitialState
			}
		}
		if summary.Valid {
			x.Summary = summary.String
		} else {
			x.Summary = incidentStateDescription(x.LatestState)
		}
		x.Events = make([]incidentEvent, 0)
		out = append(out, x)
	}
	rows.Close()
	for i := range out {
		eventRows, queryErr := a.db.Query(`SELECT e.type,e.new_state,e.created_at,c.http_status,c.error_code FROM incident_events e LEFT JOIN check_results c ON c.id=e.check_id WHERE e.incident_id=? ORDER BY e.created_at ASC,e.id ASC`, out[i].ID)
		if queryErr != nil {
			writeJSON(w, 500, map[string]string{"error": "could not load incident log"})
			return
		}
		for eventRows.Next() {
			var event incidentEvent
			var httpStatus sql.NullInt64
			var errorCode sql.NullString
			if queryErr = eventRows.Scan(&event.Type, &event.State, &event.CreatedAt, &httpStatus, &errorCode); queryErr != nil {
				eventRows.Close()
				writeJSON(w, 500, map[string]string{"error": "could not load incident log"})
				return
			}
			if event.Type == "recovered" {
				event.Summary = "Authenticated checks recovered and the incident was resolved."
			} else {
				result := checkResult{State: event.State}
				if httpStatus.Valid {
					result.HTTPStatus = int(httpStatus.Int64)
				}
				if errorCode.Valid {
					result.ErrorCode = errorCode.String
				}
				event.Summary = incidentSummary(result)
			}
			out[i].Events = append(out[i].Events, event)
		}
		eventRows.Close()
	}
	writeJSON(w, 200, out)
}

func (a *app) getNtfy(w http.ResponseWriter, r *http.Request) {
	var enabled int
	var nonce, cipher []byte
	err := a.db.QueryRow(`SELECT enabled,nonce,ciphertext FROM notification_channels WHERE kind='ntfy' LIMIT 1`).Scan(&enabled, &nonce, &cipher)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, 200, map[string]any{"configured": false, "enabled": false})
		return
	}
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not load notification configuration"})
		return
	}
	writeJSON(w, 200, map[string]any{"configured": len(cipher) > 0, "enabled": enabled == 1})
}
func (a *app) putNtfy(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL     string `json:"url"`
		Enabled bool   `json:"enabled"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	in.URL = strings.TrimSpace(in.URL)
	if in.URL == "" {
		result, err := a.db.Exec(`UPDATE notification_channels SET enabled=?,updated_at=? WHERE kind='ntfy'`, boolInt(in.Enabled), time.Now().Unix())
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "could not save notification configuration"})
			return
		}
		updated, _ := result.RowsAffected()
		if updated == 0 {
			writeJSON(w, 400, map[string]string{"error": "enter an ntfy topic URL first"})
			return
		}
		writeJSON(w, 200, map[string]bool{"configured": true, "enabled": in.Enabled})
		return
	}
	if len(in.URL) > 2048 {
		writeJSON(w, 400, map[string]string{"error": "ntfy URL is too long"})
		return
	}
	normalizedURL, err := normalizeNtfyURL(in.URL)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	nonce, cipher, err := a.encrypt([]byte(normalizedURL), "channel:ntfy")
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not secure notification configuration"})
		return
	}
	_, err = a.db.Exec(`INSERT INTO notification_channels(kind,enabled,nonce,ciphertext,updated_at) VALUES('ntfy',?,?,?,?) ON CONFLICT(kind) DO UPDATE SET enabled=excluded.enabled,nonce=excluded.nonce,ciphertext=excluded.ciphertext,updated_at=excluded.updated_at`, boolInt(in.Enabled), nonce, cipher, time.Now().Unix())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not save notification configuration"})
		return
	}
	writeJSON(w, 200, map[string]bool{"configured": true, "enabled": in.Enabled})
}

func (a *app) testNtfy(w http.ResponseWriter, r *http.Request) {
	var nonce, cipher []byte
	err := a.db.QueryRow(`SELECT nonce,ciphertext FROM notification_channels WHERE kind='ntfy' LIMIT 1`).Scan(&nonce, &cipher)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, 400, map[string]string{"error": "configure an ntfy topic first"})
		return
	}
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not load notification configuration"})
		return
	}
	plain, err := a.decrypt(nonce, cipher, "channel:ntfy")
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not decrypt notification configuration"})
		return
	}
	err = a.sendNtfy(string(plain), "DebridUp test notification", "Your ntfy notification channel is configured correctly.", "white_check_mark", "test-"+strconv.FormatInt(time.Now().Unix(), 10))
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *app) enqueueNotification(incidentID int64, eventType, name, previous, next, summary string, at time.Time) {
	rows, err := a.db.Query(`SELECT id FROM notification_channels WHERE kind='ntfy' AND enabled=1`)
	if err != nil {
		return
	}
	defer rows.Close()
	message := fmt.Sprintf("%s: %s", name, summary)
	if eventType == "recovered" {
		message = fmt.Sprintf("%s: recovered", name)
	}
	payload, _ := json.Marshal(map[string]string{"title": "DebridUp " + eventType, "message": message, "event": eventType, "state": next, "previousState": previous, "at": at.UTC().Format(time.RFC3339)})
	for rows.Next() {
		var channelID int64
		if rows.Scan(&channelID) != nil {
			continue
		}
		_, _ = a.db.Exec(`INSERT INTO notification_outbox(channel_id,incident_id,event_type,payload,next_attempt_at) VALUES(?,?,?,?,?)`, channelID, incidentID, eventType, string(payload), time.Now().Unix())
	}
}
func (a *app) notificationWorker() {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		a.deliverNotifications()
		<-t.C
	}
}
func (a *app) deliverNotifications() {
	rows, err := a.db.Query(`SELECT o.id,o.channel_id,o.payload,o.attempts,c.nonce,c.ciphertext FROM notification_outbox o JOIN notification_channels c ON c.id=o.channel_id WHERE o.status IN ('pending','retry') AND o.next_attempt_at<=? ORDER BY o.id LIMIT 25`, time.Now().Unix())
	if err != nil {
		return
	}
	defer rows.Close()
	type job struct {
		id, channelID int64
		payload       string
		attempts      int
		nonce, cipher []byte
	}
	var jobs []job
	for rows.Next() {
		var j job
		if rows.Scan(&j.id, &j.channelID, &j.payload, &j.attempts, &j.nonce, &j.cipher) != nil {
			continue
		}
		jobs = append(jobs, j)
	}
	for _, j := range jobs {
		a.deliverNtfy(j)
	}
}
func (a *app) deliverNtfy(j struct {
	id, channelID int64
	payload       string
	attempts      int
	nonce, cipher []byte
}) {
	plain, err := a.decrypt(j.nonce, j.cipher, "channel:ntfy")
	if err == nil {
		var payload map[string]string
		err = json.Unmarshal([]byte(j.payload), &payload)
		if err == nil {
			err = a.sendNtfy(string(plain), payload["title"], payload["message"], "warning", strconv.FormatInt(j.id, 10))
			if err == nil {
				_, _ = a.db.Exec(`UPDATE notification_outbox SET status='delivered',attempts=attempts+1,delivered_at=?,last_error=NULL WHERE id=?`, time.Now().Unix(), j.id)
				return
			}
		}
	}
	if err == nil {
		err = errors.New("notification delivery failed")
	}
	attempt := j.attempts + 1
	delay := time.Minute * time.Duration(1<<min(attempt, 6))
	if delay > time.Hour {
		delay = time.Hour
	}
	_, _ = a.db.Exec(`UPDATE notification_outbox SET status='retry',attempts=?,next_attempt_at=?,last_error=? WHERE id=?`, attempt, time.Now().Add(delay).Unix(), safeErr(err), j.id)
}

func (a *app) sendNtfy(url, title, message, tags, eventID string) error {
	normalizedURL, err := normalizeNtfyURL(url)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, normalizedURL, bytes.NewBufferString(message))
	if err != nil {
		return errors.New("configured ntfy URL is invalid")
	}
	req.Header.Set("Title", title)
	req.Header.Set("Tags", tags)
	req.Header.Set("X-Event-ID", eventID)
	resp, err := a.client.Do(req)
	if err != nil {
		return errors.New("could not reach the ntfy server")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func normalizeNtfyURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", errors.New("ntfy URL must be a complete http or https URL")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = strings.TrimRight(parsed.RawPath, "/")
	if parsed.Path == "" {
		return "", errors.New("ntfy URL must include a topic path")
	}
	parsed.Fragment = ""
	return parsed.String(), nil
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func safeErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 240 {
		s = s[:240]
	}
	return s
}
