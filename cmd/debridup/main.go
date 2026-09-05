package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/cipher"
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
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
	_ "modernc.org/sqlite"
)

//go:embed web/*
var webFS embed.FS

const (
	stateHealthy    = "healthy"
	stateDegraded   = "degraded"
	stateAuthFailed = "auth_failed"
	stateAPI        = "api_issue"
	stateConnection = "connection_issue"
	maxKeyInputSize = 4 << 10
)

var errInvalidEncryptionKey = errors.New("invalid encryption key")

type app struct {
	db        *sql.DB
	key       []byte
	client    *http.Client
	logger    *slog.Logger
	runs      *runCoordinator
	cookieKey []byte
	sessions  *sessionStore
	logins    *loginLimiter

	// The master key is fixed for the process lifetime, so the AEAD is built
	// once instead of on every encrypt/decrypt.
	aeadOnce sync.Once
	aead     cipher.AEAD
	aeadErr  error
}

func (a *app) cipher() (cipher.AEAD, error) {
	a.aeadOnce.Do(func() {
		a.aead, a.aeadErr = chacha20poly1305.NewX(a.key)
	})
	return a.aead, a.aeadErr
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

type incidentPeriod struct {
	OpenedAt   int64
	ResolvedAt int64
}

// providerAuth records how a provider expects its credential to be presented.
// Every provider currently uses a bearer token; the field exists so the choice
// is explicit per provider rather than an implicit property of authCheck, and
// so switching one is a single-field change.
type providerAuth uint8

const (
	authBearer providerAuth = iota
	authQueryParam
)

type providerDefinition struct {
	Endpoint       string
	PublicEndpoint string
	Method         string
	Auth           providerAuth
	// AuthParam names the query parameter carrying the credential when Auth is
	// authQueryParam. It is ignored for authBearer.
	AuthParam string
	// ErrorFields lists the payload fields that carry a provider's own error
	// text, so classification reads those instead of the whole response body.
	ErrorFields []string
}

var providerDefinitions = map[string]providerDefinition{
	"torbox":     {Endpoint: "https://api.torbox.app/v1/api/user/me", PublicEndpoint: "https://status.torbox.app/", Method: http.MethodGet, Auth: authBearer, ErrorFields: []string{"detail", "error", "message"}},
	"premiumize": {Endpoint: "https://www.premiumize.me/api/account/info", PublicEndpoint: "https://premiumize.reamaze.com/status", Method: http.MethodGet, Auth: authBearer, ErrorFields: []string{"message", "error"}},
	"alldebrid":  {Endpoint: "https://api.alldebrid.com/v4/user", PublicEndpoint: "https://api.alldebrid.com/v4/ping", Method: http.MethodGet, Auth: authBearer, ErrorFields: []string{"error", "message", "code"}},
	"realdebrid": {Endpoint: "https://api.real-debrid.com/rest/1.0/user", PublicEndpoint: "https://api.real-debrid.com/rest/1.0/time", Method: http.MethodGet, Auth: authBearer, ErrorFields: []string{"error", "error_code"}},
	"torrin":     {Endpoint: "https://torrin.app/api/stats", PublicEndpoint: "https://torrin.app/api/stats/public", Method: http.MethodGet, Auth: authBearer, ErrorFields: []string{"error", "message"}},
	"pikpak":     {Endpoint: "https://user.mypikpak.com/v1/user/me", PublicEndpoint: "https://mypikpak.com/", Method: http.MethodGet, Auth: authBearer, ErrorFields: []string{"error", "error_description", "error_code"}},
	"offcloud":   {Endpoint: "https://offcloud.com/api/account/info", PublicEndpoint: "https://offcloud.com/", Method: http.MethodGet, Auth: authBearer, ErrorFields: []string{"error"}},
	"debridlink": {Endpoint: "https://debrid-link.com/api/v2/account/infos", PublicEndpoint: "https://www.debrid-link.com/webapp/status", Method: http.MethodGet, Auth: authBearer, ErrorFields: []string{"error", "error_description"}},
	"easydebrid": {Endpoint: "https://easydebrid.com/api/v1/user/details", PublicEndpoint: "https://easydebrid.com/", Method: http.MethodGet, Auth: authBearer, ErrorFields: []string{"error", "message"}},
	"debrider":   {Endpoint: "https://debrider.app/api/v1/tasks", PublicEndpoint: "https://stats.uptimerobot.com/shklobtEFJ/801337046", Method: http.MethodGet, Auth: authBearer, ErrorFields: []string{"error", "message", "detail"}},
	"deepbrid":   {Endpoint: "https://www.deepbrid.com/api/v1/user", PublicEndpoint: "https://www.deepbrid.com/api/v1/hosts", Method: http.MethodGet, Auth: authBearer, ErrorFields: []string{"message", "error"}},
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
	if err := run(); err != nil {
		// Startup problems are ordinary misconfiguration (a missing key file, a
		// short password); an operator needs the reason, not a stack dump.
		slog.Default().Error("DebridUp could not start", "error", err)
		os.Exit(1)
	}
}

func run() error {
	maxConcurrentChecks, err := parseMaxConcurrentChecks(os.Getenv("DEBRIDUP_MAX_CONCURRENT_CHECKS"))
	if err != nil {
		return err
	}
	retention, err := parseRetention(os.Getenv("DEBRIDUP_HISTORY_RETENTION"))
	if err != nil {
		return err
	}
	dataDir := env("DEBRIDUP_DATA_DIR", "./data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory %q: %w", dataDir, err)
	}
	key, err := loadKey()
	if err != nil {
		return err
	}
	db, err := openDatabase(filepath.Join(dataDir, "debridup.db"))
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	cookieHash := sha256.Sum256(key)
	a := &app{db: db, key: key, cookieKey: cookieHash[:], client: &http.Client{Timeout: 65 * time.Second}, logger: slog.Default(), runs: newRunCoordinator(maxConcurrentChecks)}
	if err := migrateDatabase(context.Background(), db); err != nil {
		return fmt.Errorf("apply database migrations: %w", err)
	}
	if err := a.ensureAdmin(os.Getenv("DEBRIDUP_ADMIN_PASSWORD")); err != nil {
		return err
	}
	a.sessions = newSessionStore(db)
	a.logins = newLoginLimiter()
	if err := a.sessions.load(context.Background()); err != nil {
		return fmt.Errorf("load sessions: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var workers sync.WaitGroup
	for _, worker := range []func(context.Context){
		a.scheduler,
		a.notificationWorker,
		func(ctx context.Context) { a.retentionWorker(ctx, retention) },
	} {
		workers.Add(1)
		go func(worker func(context.Context)) {
			defer workers.Done()
			worker(ctx)
		}(worker)
	}

	addr := env("DEBRIDUP_ADDR", ":8080")
	server := &http.Server{
		Addr:    addr,
		Handler: a.routes(),
		// A manual provider test runs an authenticated check inline and may take
		// up to the 60s per-monitor timeout, so the write budget sits above it.
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	a.logger.Info("DebridUp started", "addr", addr)

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		stop()
		workers.Wait()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve %s: %w", addr, err)
		}
		return nil
	case <-ctx.Done():
	}

	a.logger.Info("DebridUp shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	shutdownErr := server.Shutdown(shutdownCtx)
	workers.Wait()
	if shutdownErr != nil {
		return fmt.Errorf("shut down cleanly: %w", shutdownErr)
	}
	return nil
}

// log returns a usable logger even when the app was built without one.
func (a *app) log() *slog.Logger {
	if a.logger != nil {
		return a.logger
	}
	return slog.Default()
}

func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func loadKey() ([]byte, error) {
	var v string
	if rawFD := os.Getenv("DEBRIDUP_ENCRYPTION_KEY_FD"); rawFD != "" {
		fd, err := strconv.Atoi(rawFD)
		if err != nil || fd < 0 {
			return nil, errInvalidEncryptionKey
		}
		file := os.NewFile(uintptr(fd), "encryption-key")
		if file == nil {
			return nil, errInvalidEncryptionKey
		}
		b, readErr := io.ReadAll(io.LimitReader(file, maxKeyInputSize+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(b) > maxKeyInputSize {
			return nil, errInvalidEncryptionKey
		}
		v = string(b)
	} else if p := os.Getenv("DEBRIDUP_ENCRYPTION_KEY_FILE"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, errInvalidEncryptionKey
		}
		v = string(b)
	} else {
		v = os.Getenv("DEBRIDUP_ENCRYPTION_KEY")
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
	if err != nil || len(b) != chacha20poly1305.KeySize {
		return nil, errInvalidEncryptionKey
	}
	return b, nil
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
	c, err := a.cipher()
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
	c, err := a.cipher()
	if err != nil {
		return nil, err
	}
	return c.Open(nil, nonce, ciphertext, []byte(aad))
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]bool{"ok": true}) })
	mux.HandleFunc("GET /readyz", a.readiness)
	mux.HandleFunc("GET /integrations/health/{provider}", a.providerHealth)
	mux.HandleFunc("POST /login", a.login)
	mux.HandleFunc("POST /logout", a.logout)
	mux.HandleFunc("GET /api/dashboard", a.auth(a.dashboard))
	mux.HandleFunc("GET /api/checks", a.auth(a.listAllChecks))
	mux.HandleFunc("GET /api/report", a.auth(a.report))
	mux.HandleFunc("GET /api/overview", a.auth(a.overview))
	mux.HandleFunc("GET /api/monitors", a.auth(a.listMonitors))
	mux.HandleFunc("POST /api/monitors", a.auth(a.createMonitor))
	mux.HandleFunc("PUT /api/monitors/{id}", a.auth(a.updateMonitor))
	mux.HandleFunc("DELETE /api/monitors/{id}", a.auth(a.deleteMonitor))
	mux.HandleFunc("POST /api/monitors/{id}/reset", a.auth(a.resetMonitorStats))
	mux.HandleFunc("POST /api/monitors/{id}/test", a.auth(a.testMonitor))
	mux.HandleFunc("GET /api/monitors/{id}/checks", a.auth(a.listChecks))
	mux.HandleFunc("POST /api/stats/reset", a.auth(a.resetAllStats))
	mux.HandleFunc("GET /api/incidents", a.auth(a.listIncidents))
	mux.HandleFunc("GET /api/notifications/ntfy", a.auth(a.getNtfy))
	mux.HandleFunc("PUT /api/notifications/ntfy", a.auth(a.putNtfy))
	mux.HandleFunc("POST /api/notifications/ntfy/test", a.auth(a.testNtfy))
	static, _ := fs.Sub(webFS, "web")
	files := staticFileServer(static)
	mux.Handle("GET /login.html", files)
	mux.Handle("GET /login.js", files)
	mux.Handle("GET /theme-init.js", files)
	mux.Handle("GET /app.css", files)
	mux.Handle("/", a.auth(func(w http.ResponseWriter, r *http.Request) { files.ServeHTTP(w, r) }))
	return securityHeaders(compress(mux))
}

// buildETag derives a single tag from the embedded asset bundle. The files only
// change when the binary does, so one tag for the whole set is sufficient and
// needs no build step.
var buildETag = sync.OnceValue(func() string {
	digest := sha256.New()
	_ = fs.WalkDir(webFS, "web", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		contents, readErr := webFS.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		_, _ = digest.Write([]byte(path))
		_, _ = digest.Write(contents)
		return nil
	})
	return `"` + base64.RawURLEncoding.EncodeToString(digest.Sum(nil)[:16]) + `"`
})

// staticFileServer serves the embedded assets as cacheable resources. The
// blanket no-store in securityHeaders is right for API responses carrying
// provider state, but it made every navigation refetch all CSS and JS.
func staticFileServer(static fs.FS) http.Handler {
	files := http.FileServer(http.FS(static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		etag := buildETag()
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "public, max-age=300, must-revalidate")
		if match := r.Header.Get("If-None-Match"); match != "" {
			for _, candidate := range strings.Split(match, ",") {
				if strings.TrimSpace(candidate) == etag {
					w.WriteHeader(http.StatusNotModified)
					return
				}
			}
		}
		files.ServeHTTP(w, r)
	})
}

type gzipResponseWriter struct {
	http.ResponseWriter
	writer      *gzip.Writer
	compressing bool
	wroteHeader bool
}

// compressibleTypes are the response bodies worth gzipping. Everything else
// (images, already-compressed payloads) is passed through untouched.
func compressibleType(contentType string) bool {
	base, _, _ := strings.Cut(contentType, ";")
	base = strings.TrimSpace(base)
	if strings.HasPrefix(base, "text/") {
		return true
	}
	switch base {
	case "application/json", "application/javascript", "application/xhtml+xml", "image/svg+xml":
		return true
	}
	return false
}

func (g *gzipResponseWriter) WriteHeader(status int) {
	if g.wroteHeader {
		return
	}
	g.wroteHeader = true
	header := g.ResponseWriter.Header()
	// A 304 and a 204 must not carry a body, so they are never wrapped.
	if status == http.StatusOK && compressibleType(header.Get("Content-Type")) {
		g.compressing = true
		header.Set("Content-Encoding", "gzip")
		header.Del("Content-Length")
	}
	g.ResponseWriter.WriteHeader(status)
}

func (g *gzipResponseWriter) Write(p []byte) (int, error) {
	if !g.wroteHeader {
		if g.ResponseWriter.Header().Get("Content-Type") == "" {
			g.ResponseWriter.Header().Set("Content-Type", http.DetectContentType(p))
		}
		g.WriteHeader(http.StatusOK)
	}
	if g.compressing {
		return g.writer.Write(p)
	}
	return g.ResponseWriter.Write(p)
}

var gzipWriters = sync.Pool{New: func() any { return gzip.NewWriter(io.Discard) }}

// compress gzips text responses for clients that accept it. The dashboard
// payload is highly repetitive JSON, which matters most over a VPN or a slow
// remote link.
func compress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Vary is set regardless, so a shared cache never serves a gzipped body
		// to a client that did not ask for one.
		w.Header().Add("Vary", "Accept-Encoding")
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		writer := gzipWriters.Get().(*gzip.Writer)
		writer.Reset(w)
		wrapped := &gzipResponseWriter{ResponseWriter: w, writer: writer}
		defer func() {
			if wrapped.compressing {
				_ = writer.Close()
			}
			writer.Reset(io.Discard)
			gzipWriters.Put(writer)
		}()
		next.ServeHTTP(wrapped, r)
	})
}

func (a *app) readiness(w http.ResponseWriter, r *http.Request) {
	if err := databaseReady(r.Context(), a.db); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"code": "database_unavailable", "error": "database is not ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// contentSecurityPolicy is strict because every script and style is
// same-origin and embedded, there are no inline handlers, and nothing external
// is loaded.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
		// Default to no-store; the static handler replaces this for embedded
		// assets, which carry no account state and change only on deploy.
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

// sessionCookie returns the session id carried by a correctly signed cookie.
func (a *app) sessionCookie(r *http.Request) (string, bool) {
	c, err := r.Cookie("debridup_session")
	if err != nil {
		return "", false
	}
	p := strings.Split(c.Value, ".")
	if len(p) != 2 {
		return "", false
	}
	data, err := base64.RawURLEncoding.DecodeString(p[0])
	if err != nil {
		return "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(p[1])
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, a.cookieKey)
	mac.Write(data)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return "", false
	}
	return string(data), true
}

func (a *app) sessionOK(r *http.Request) bool {
	id, signed := a.sessionCookie(r)
	if !signed || a.sessions == nil {
		return false
	}
	// The signature only proves the cookie came from this server. Validity is
	// decided by the session record, so signing out actually revokes access.
	return a.sessions.valid(id, time.Now())
}

func (a *app) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if a.sessions == nil || a.logins == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "sign-in is unavailable"})
		return
	}

	now := time.Now()
	key := clientKey(r)
	if wait := a.logins.lockedFor(key, now); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many sign-in attempts, try again shortly"})
		return
	}
	// Verifying a password costs 64 MiB of Argon2id memory. Bound how many run
	// at once so a burst of unauthenticated requests cannot exhaust the host.
	if !a.logins.acquireHashSlot(r.Context()) {
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "sign-in is busy, try again shortly"})
		return
	}
	var hash string
	_ = a.db.QueryRowContext(r.Context(), "SELECT value FROM settings WHERE key='admin_hash'").Scan(&hash)
	verified := verifyPassword(hash, in.Password)
	a.logins.releaseHashSlot()

	if !verified {
		a.logins.recordFailure(key, now)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}
	a.logins.recordSuccess(key)

	id, expiresAt, err := a.sessions.issue(r.Context(), now)
	if err != nil {
		a.log().Error("issue session", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not start a session"})
		return
	}
	data := []byte(id)
	mac := hmac.New(sha256.New, a.cookieKey)
	mac.Write(data)
	value := base64.RawURLEncoding.EncodeToString(data) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	http.SetCookie(w, &http.Cookie{Name: "debridup_session", Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: int(time.Unix(expiresAt, 0).Sub(now).Seconds()), Secure: r.TLS != nil})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) logout(w http.ResponseWriter, r *http.Request) {
	if id, signed := a.sessionCookie(r); signed && a.sessions != nil {
		a.sessions.revoke(r.Context(), id)
	}
	http.SetCookie(w, &http.Cookie{Name: "debridup_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
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

func (a *app) retentionWorker(ctx context.Context, retention time.Duration) {
	prune := func() {
		now := time.Now().UTC()
		if _, err := pruneHistory(ctx, a.db, now.Add(-retention)); err != nil {
			a.log().Error("history prune failed", "error", err)
		}
		if a.sessions != nil {
			a.sessions.prune(ctx, now)
		}
		if a.logins != nil {
			a.logins.forgetStale(now)
		}
	}
	prune()
	for {
		now := time.Now().UTC()
		next := now.Truncate(24 * time.Hour).Add(24 * time.Hour)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			prune()
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
	endpoint, err := authenticatedEndpoint(provider, credential)
	if err != nil {
		return checkResult{State: stateConnection, ErrorCode: "request_build", CheckedAt: started}
	}
	req, err := http.NewRequestWithContext(ctx, provider.Method, endpoint, nil)
	if err != nil {
		return checkResult{State: stateConnection, ErrorCode: "request_build", CheckedAt: started}
	}
	if provider.Auth == authBearer {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
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

func authenticatedEndpoint(provider providerDefinition, credential string) (string, error) {
	if provider.Auth != authQueryParam || provider.AuthParam == "" {
		return provider.Endpoint, nil
	}
	parsed, err := url.Parse(provider.Endpoint)
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	query.Set(provider.AuthParam, credential)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func classifyProviderPayload(provider string, payload any) (string, string) {
	object, isObject := payload.(map[string]any)
	if !isObject {
		return "", ""
	}
	definition := providerDefinitions[provider]
	switch provider {
	case "torbox":
		if success, present := object["success"].(bool); present && !success {
			return classifyPayloadError(object, definition.ErrorFields)
		}
	case "premiumize", "alldebrid":
		if status, present := object["status"].(string); present && !strings.EqualFold(status, "success") {
			return classifyPayloadError(object, definition.ErrorFields)
		}
	case "debridlink":
		if success, present := object["success"].(bool); present && !success {
			return classifyPayloadError(object, definition.ErrorFields)
		}
	case "deepbrid":
		if errorValue, present := object["error"].(float64); present && errorValue != 0 {
			return classifyPayloadError(object, definition.ErrorFields)
		}
	case "offcloud":
		if _, present := object["error"]; present {
			return classifyPayloadError(object, definition.ErrorFields)
		}
	}
	return "", ""
}

var authenticationMarkers = []string{"auth", "apikey", "api key", "token", "credential", "unauthor", "logged in", "login"}

// collectErrorText gathers the string values reachable from one payload field.
// A provider may report an error as a bare string, or as a nested object such
// as {"error": {"code": "AUTH_BAD_APIKEY", "message": "..."}}.
func collectErrorText(value any, depth int, out *[]string) {
	if depth > 3 {
		return
	}
	switch typed := value.(type) {
	case string:
		*out = append(*out, typed)
	case map[string]any:
		for _, nested := range typed {
			collectErrorText(nested, depth+1, out)
		}
	case []any:
		for _, nested := range typed {
			collectErrorText(nested, depth+1, out)
		}
	}
}

// classifyPayloadError inspects only the provider's declared error fields.
// Scanning the whole encoded payload used to match field *names* and unrelated
// values, so a rate-limit error alongside a field like "authMethod" was
// reported as an authentication failure.
func classifyPayloadError(payload map[string]any, fields []string) (string, string) {
	var texts []string
	for _, field := range fields {
		if value, present := payload[field]; present {
			collectErrorText(value, 0, &texts)
		}
	}
	for _, text := range texts {
		lowered := strings.ToLower(text)
		for _, marker := range authenticationMarkers {
			if strings.Contains(lowered, marker) {
				return stateAuthFailed, "authentication_rejected"
			}
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
	// Dashboard buckets are maintained here, in the same transaction as the
	// check, so a snapshot never sees a summary that disagrees with the raw rows.
	if err = refreshRollupsFor(context.Background(), tx, m.ID, r.CheckedAt.Unix()); err != nil {
		return 0, err
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
	type item struct {
		monitor
		State      string `json:"state"`
		StateSince *int64 `json:"stateSince"`
		LastCheck  *int64 `json:"lastCheck"`
		Configured bool   `json:"configured"`
	}
	// One query: the per-monitor state lookup and the secret-existence count
	// used to be issued separately for every row.
	rows, err := a.db.QueryContext(r.Context(), `
	SELECT m.id,m.provider,m.name,m.enabled,m.interval_seconds,m.timeout_seconds,m.failure_threshold,m.recovery_threshold,m.public_check,
	       s.current_state,s.state_since,s.last_check_at,
	       EXISTS(SELECT 1 FROM monitor_secrets ms WHERE ms.monitor_id=m.id)
	FROM monitors m LEFT JOIN monitor_states s ON s.monitor_id=m.id
	ORDER BY m.id`)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not load monitors"})
		return
	}
	defer rows.Close()
	out := make([]item, 0)
	for rows.Next() {
		var i item
		var enabled, public, configured int
		var currentState sql.NullString
		var stateSince, lastCheck sql.NullInt64
		if err = rows.Scan(&i.ID, &i.Provider, &i.Name, &enabled, &i.IntervalSeconds, &i.TimeoutSeconds, &i.FailureThreshold, &i.RecoveryThreshold, &public,
			&currentState, &stateSince, &lastCheck, &configured); err != nil {
			writeJSON(w, 500, map[string]string{"error": "could not load monitors"})
			return
		}
		i.Enabled = enabled == 1
		i.PublicCheck = public == 1
		i.Configured = configured == 1
		if currentState.Valid {
			i.State = currentState.String
		}
		if stateSince.Valid {
			v := stateSince.Int64
			i.StateSince = &v
		}
		if lastCheck.Valid {
			v := lastCheck.Int64
			i.LastCheck = &v
		}
		if i.State == "" {
			i.State = "checking"
		}
		out = append(out, i)
	}
	if err = rows.Err(); err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not load monitors"})
		return
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
	a.runs.RequestImmediate(m.ID)
	if a.runs.Claim(m.ID, time.Now(), time.Duration(m.IntervalSeconds)*time.Second) == claimAccepted {
		go func(m monitor) {
			defer a.runs.Release(m.ID)
			a.runMonitor(m)
		}(m)
	}
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
			a.runs.RequestImmediate(m.ID)
		}
		if loadErr == nil && a.runs.Claim(m.ID, time.Now(), time.Duration(m.IntervalSeconds)*time.Second) == claimAccepted {
			go func(m monitor) {
				defer a.runs.Release(m.ID)
				a.runMonitor(m)
			}(m)
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
		`DELETE FROM check_rollups WHERE monitor_id=?`,
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
	a.runs.Forget(id)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *app) resetMonitorStats(w http.ResponseWriter, r *http.Request) {
	id, err := monitorID(r)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid monitor id"})
		return
	}
	if err = a.resetHistory(&id); errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, 404, map[string]string{"error": "monitor not found"})
		return
	} else if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not reset provider stats"})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *app) resetAllStats(w http.ResponseWriter, r *http.Request) {
	if err := a.resetHistory(nil); err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not reset all stats"})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *app) resetHistory(monitorID *int64) error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if monitorID == nil {
		statements := []string{
			`DELETE FROM notification_outbox WHERE incident_id IS NOT NULL`,
			`DELETE FROM incident_events`,
			`DELETE FROM incidents`,
			`DELETE FROM check_results`,
			`DELETE FROM check_rollups`,
			`DELETE FROM monitor_states`,
		}
		for _, statement := range statements {
			if _, err = tx.Exec(statement); err != nil {
				return err
			}
		}
		return tx.Commit()
	}

	var exists int
	if err = tx.QueryRow(`SELECT 1 FROM monitors WHERE id=?`, *monitorID).Scan(&exists); err != nil {
		return err
	}
	statements := []string{
		`DELETE FROM notification_outbox WHERE incident_id IN (SELECT id FROM incidents WHERE monitor_id=?)`,
		`DELETE FROM incident_events WHERE incident_id IN (SELECT id FROM incidents WHERE monitor_id=?)`,
		`DELETE FROM incidents WHERE monitor_id=?`,
		`DELETE FROM check_results WHERE monitor_id=?`,
		`DELETE FROM check_rollups WHERE monitor_id=?`,
		`DELETE FROM monitor_states WHERE monitor_id=?`,
	}
	for _, statement := range statements {
		if _, err = tx.Exec(statement, *monitorID); err != nil {
			return err
		}
	}
	return tx.Commit()
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
	switch a.runs.ClaimManual(m.ID, time.Now()) {
	case claimOverlap:
		writeJSON(w, http.StatusConflict, map[string]string{"error": "monitor check already in progress"})
		return
	case claimCapacity:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "monitor check capacity unavailable"})
		return
	}
	defer a.runs.Release(m.ID)
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

// overviewLatencySamples caps how many recent healthy checks the overview p95
// considers, preserving the figure this endpoint has always reported.
const overviewLatencySamples = 1000

// coverageBucketWidth is the rollup width the coverage figure is summed from.
// It is the widest available, so the fewest rows are read.
var coverageBucketWidth = rollupBucketWidths[len(rollupBucketWidths)-1]

func (a *app) overview(w http.ResponseWriter, r *http.Request) {
	type item struct {
		ID                    int64 `json:"id"`
		Name, Provider, State string
		LastCheck             *int64   `json:"lastCheck"`
		Availability          *float64 `json:"availability"`
		Coverage              *float64 `json:"coverage"`
		P95MS                 *int64   `json:"p95Ms"`
		OpenIncident          bool     `json:"openIncident"`
	}

	ctx := r.Context()
	now := time.Now().Unix()
	cutoff := now - int64(30*24*time.Hour/time.Second)
	fail := func() { writeJSON(w, 500, map[string]string{"error": "could not load overview"}) }

	// Previously six queries per monitor. Each block below is one query across
	// all monitors, grouped by monitor_id.
	rows, err := a.db.QueryContext(ctx, `
	SELECT m.id,m.name,m.provider,COALESCE(s.current_state,'checking'),s.last_check_at
	FROM monitors m LEFT JOIN monitor_states s ON s.monitor_id=m.id ORDER BY m.id`)
	if err != nil {
		fail()
		return
	}
	out := make([]item, 0)
	position := make(map[int64]int)
	for rows.Next() {
		var v item
		var last sql.NullInt64
		if err = rows.Scan(&v.ID, &v.Name, &v.Provider, &v.State, &last); err != nil {
			rows.Close()
			fail()
			return
		}
		if last.Valid {
			x := last.Int64
			v.LastCheck = &x
		}
		position[v.ID] = len(out)
		out = append(out, v)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		fail()
		return
	}
	rows.Close()
	if len(out) == 0 {
		writeJSON(w, 200, map[string]any{"monitors": out, "generatedAt": now})
		return
	}

	// Per-monitor rather than batched: with the (monitor_id, source, checked_at)
	// index each of these is a contiguous index range scan, whereas ranking
	// every row across all monitors to pick a percentile measured an order of
	// magnitude slower. N is the monitor count, and each query is index-optimal.
	firstChecks := make(map[int64]int64, len(out))
	for i := range out {
		id := out[i].ID

		var firstCheck sql.NullInt64
		if err = a.db.QueryRowContext(ctx, `SELECT MIN(checked_at) FROM check_results WHERE monitor_id=? AND source='authenticated'`, id).
			Scan(&firstCheck); err != nil {
			fail()
			return
		}
		if firstCheck.Valid {
			firstChecks[id] = firstCheck.Int64
		}

		// Coverage comes from the rollups for every bucket wholly inside the
		// window, and from raw rows only for the single bucket straddling the
		// cutoff. Reading the state of all ~43k rows per monitor was by far the
		// dominant cost of this endpoint; the result is still exact.
		boundary := bucketStartFor(cutoff, coverageBucketWidth)
		var bucketTotal, bucketEligible sql.NullInt64
		if err = a.db.QueryRowContext(ctx, `SELECT SUM(total),SUM(eligible) FROM check_rollups
		WHERE monitor_id=? AND bucket_width=? AND bucket_start>=?`,
			id, coverageBucketWidth, boundary+coverageBucketWidth).Scan(&bucketTotal, &bucketEligible); err != nil {
			fail()
			return
		}
		var partialTotal, partialEligible int
		if err = a.db.QueryRowContext(ctx, `SELECT COUNT(*),COUNT(CASE WHEN state!='auth_failed' THEN 1 END)
		FROM check_results WHERE monitor_id=? AND source='authenticated' AND checked_at>=? AND checked_at<?`,
			id, cutoff, boundary+coverageBucketWidth).Scan(&partialTotal, &partialEligible); err != nil {
			fail()
			return
		}
		total := partialTotal + int(bucketTotal.Int64)
		eligible := partialEligible + int(bucketEligible.Int64)
		if total > 0 {
			coverage := float64(eligible) / float64(total) * 100
			out[i].Coverage = &coverage
		}

		// Nearest-rank p95 over the most recent healthy checks. The count and
		// the value are two seeks against the same index range, rather than
		// pulling every latency into memory to sort.
		var healthy int
		if err = a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (
		 SELECT 1 FROM check_results WHERE monitor_id=? AND source='authenticated' AND checked_at>=? AND state='healthy'
		 ORDER BY checked_at DESC LIMIT ?)`, id, cutoff, overviewLatencySamples).Scan(&healthy); err != nil {
			fail()
			return
		}
		if healthy == 0 {
			continue
		}
		var p95 int64
		if err = a.db.QueryRowContext(ctx, `SELECT duration_ms FROM (
		 SELECT duration_ms FROM check_results WHERE monitor_id=? AND source='authenticated' AND checked_at>=? AND state='healthy'
		 ORDER BY checked_at DESC LIMIT ?)
		ORDER BY duration_ms LIMIT 1 OFFSET ?`, id, cutoff, overviewLatencySamples, (healthy*95+99)/100-1).Scan(&p95); err != nil {
			fail()
			return
		}
		out[i].P95MS = &p95
	}

	// Incident periods for every monitor at once, plus the open-incident flag.
	incidentRows, err := a.db.QueryContext(ctx, `
	SELECT monitor_id,opened_at,resolved_at FROM incidents
	WHERE opened_at<? AND (resolved_at IS NULL OR resolved_at>?)
	ORDER BY monitor_id,opened_at`, now, cutoff)
	if err != nil {
		fail()
		return
	}
	periodsByMonitor := make(map[int64][]incidentPeriod)
	for incidentRows.Next() {
		var monitorID int64
		var period incidentPeriod
		var resolved sql.NullInt64
		if err = incidentRows.Scan(&monitorID, &period.OpenedAt, &resolved); err != nil {
			incidentRows.Close()
			fail()
			return
		}
		if resolved.Valid {
			period.ResolvedAt = resolved.Int64
		} else if index, known := position[monitorID]; known {
			out[index].OpenIncident = true
		}
		periodsByMonitor[monitorID] = append(periodsByMonitor[monitorID], period)
	}
	if err = incidentRows.Err(); err != nil {
		incidentRows.Close()
		fail()
		return
	}
	incidentRows.Close()

	for i := range out {
		firstCheck, observed := firstChecks[out[i].ID]
		if !observed {
			continue
		}
		observedStart := max(firstCheck, cutoff)
		// Only periods overlapping the observed window contribute downtime,
		// matching the previous per-monitor query's WHERE clause.
		periods := make([]incidentPeriod, 0, len(periodsByMonitor[out[i].ID]))
		for _, period := range periodsByMonitor[out[i].ID] {
			if period.ResolvedAt == 0 || period.ResolvedAt > observedStart {
				periods = append(periods, period)
			}
		}
		out[i].Availability = confirmedAvailability(observedStart, now, periods)
	}

	writeJSON(w, 200, map[string]any{"monitors": out, "generatedAt": now})
}

func confirmedAvailability(observedStart, now int64, periods []incidentPeriod) *float64 {
	if observedStart <= 0 || observedStart > now {
		return nil
	}
	if observedStart == now {
		value := 100.0
		return &value
	}
	observedSeconds := now - observedStart
	var downtimeSeconds int64
	for _, period := range periods {
		start := max(period.OpenedAt, observedStart)
		end := period.ResolvedAt
		if end == 0 || end > now {
			end = now
		}
		if end > start {
			downtimeSeconds += end - start
		}
	}
	if downtimeSeconds > observedSeconds {
		downtimeSeconds = observedSeconds
	}
	value := float64(observedSeconds-downtimeSeconds) / float64(observedSeconds) * 100
	return &value
}

// listIncidents returns the newest 200 incidents, newest first, with each
// incident's timeline in chronological order. The bounded response makes the
// endpoint suitable for the interactive incident view without growing with the
// full retention period. Event check metadata is included only while its raw
// check result is retained; detailed provider error text is intentionally not
// exposed here.
func (a *app) listIncidents(w http.ResponseWriter, r *http.Request) {
	type incidentEventCheck struct {
		ID         int64   `json:"id"`
		Source     string  `json:"source"`
		DurationMS int64   `json:"durationMs"`
		HTTPStatus *int64  `json:"httpStatus,omitempty"`
		ErrorCode  *string `json:"errorCode,omitempty"`
	}
	type incidentEvent struct {
		Type      string              `json:"type"`
		State     string              `json:"state"`
		Summary   string              `json:"summary"`
		CreatedAt int64               `json:"createdAt"`
		Check     *incidentEventCheck `json:"check,omitempty"`
	}
	type incident struct {
		ID                        int64 `json:"id"`
		Name, Provider            string
		OpenedAt, DetectedAt      int64
		ResolvedAt                *int64 `json:"resolvedAt"`
		Ongoing                   bool   `json:"ongoing"`
		InitialState, LatestState string
		Summary                   string          `json:"summary"`
		Events                    []incidentEvent `json:"events"`
	}

	rows, err := a.db.QueryContext(r.Context(), `SELECT i.id,m.name,m.provider,i.opened_at,i.detected_at,i.resolved_at,i.initial_state,i.latest_state,i.summary FROM incidents i JOIN monitors m ON m.id=i.monitor_id ORDER BY i.opened_at DESC,i.id DESC LIMIT 200`)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not load incidents"})
		return
	}
	out := make([]incident, 0)
	index := make(map[int64]int)
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
		} else {
			x.Ongoing = true
		}
		if summary.Valid {
			x.Summary = summary.String
		} else {
			x.Summary = incidentStateDescription(x.LatestState)
		}
		x.Events = make([]incidentEvent, 0)
		index[x.ID] = len(out)
		out = append(out, x)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		writeJSON(w, 500, map[string]string{"error": "could not load incidents"})
		return
	}
	rows.Close()
	if len(out) == 0 {
		writeJSON(w, 200, out)
		return
	}

	// One query for every incident's events, rather than one query per
	// incident. Ordering by incident then time lets the rows be appended
	// straight onto the matching incident.
	ids := make([]any, 0, len(out))
	for i := range out {
		ids = append(ids, out[i].ID)
	}
	query := `SELECT e.incident_id,e.type,e.new_state,e.created_at,c.id,c.source,c.duration_ms,c.http_status,c.error_code
	FROM incident_events e LEFT JOIN check_results c ON c.id=e.check_id
	WHERE e.incident_id IN (?` + strings.Repeat(",?", len(ids)-1) + `)
	ORDER BY e.incident_id,e.created_at ASC,e.id ASC`
	eventRows, err := a.db.QueryContext(r.Context(), query, ids...)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not load incident log"})
		return
	}
	defer eventRows.Close()
	for eventRows.Next() {
		var incidentID int64
		var event incidentEvent
		var checkID sql.NullInt64
		var source sql.NullString
		var durationMS sql.NullInt64
		var httpStatus sql.NullInt64
		var errorCode sql.NullString
		if err = eventRows.Scan(&incidentID, &event.Type, &event.State, &event.CreatedAt, &checkID, &source, &durationMS, &httpStatus, &errorCode); err != nil {
			writeJSON(w, 500, map[string]string{"error": "could not load incident log"})
			return
		}
		position, known := index[incidentID]
		if !known {
			continue
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
		if checkID.Valid {
			check := incidentEventCheck{ID: checkID.Int64, Source: source.String, DurationMS: durationMS.Int64}
			if httpStatus.Valid {
				value := httpStatus.Int64
				check.HTTPStatus = &value
			}
			if errorCode.Valid {
				value := errorCode.String
				check.ErrorCode = &value
			}
			event.Check = &check
		}
		out[position].Events = append(out[position].Events, event)
	}
	if err = eventRows.Err(); err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not load incident log"})
		return
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
func (a *app) notificationWorker(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		a.deliverNotifications()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
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
