package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func sessionTestApp(t *testing.T) *app {
	t.Helper()
	a := testApp(t)
	if err := migrateDatabase(context.Background(), a.db); err != nil {
		t.Fatal(err)
	}
	a.cookieKey = []byte("cookie-key-for-tests-only-32bytes")
	a.sessions = newSessionStore(a.db)
	a.logins = newLoginLimiter()
	if err := a.ensureAdmin("correct-horse-battery-staple"); err != nil {
		t.Fatal(err)
	}
	return a
}

func loginRequest(password string) *http.Request {
	body := strings.NewReader(`{"password":` + strconv.Quote(password) + `}`)
	request := httptest.NewRequest(http.MethodPost, "/login", body)
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = "203.0.113.7:54321"
	return request
}

func TestLoginIssuesRevocableSession(t *testing.T) {
	a := sessionTestApp(t)

	response := httptest.NewRecorder()
	a.login(response, loginRequest("correct-horse-battery-staple"))
	if response.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", response.Code, response.Body.String())
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "debridup_session" {
		t.Fatalf("expected a session cookie, got %+v", cookies)
	}
	cookie := cookies[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie must be HttpOnly and SameSite=Strict: %+v", cookie)
	}

	authenticated := httptest.NewRequest(http.MethodGet, "/api/monitors", nil)
	authenticated.AddCookie(cookie)
	if !a.sessionOK(authenticated) {
		t.Fatal("freshly issued session was rejected")
	}

	// The session must be persisted, not only held in memory.
	var stored int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Fatalf("sessions rows = %d, want 1", stored)
	}

	// Signing out revokes the session for every holder of that cookie, not just
	// the browser that cleared it.
	logoutRequest := httptest.NewRequest(http.MethodPost, "/logout", nil)
	logoutRequest.AddCookie(cookie)
	a.logout(httptest.NewRecorder(), logoutRequest)

	if a.sessionOK(authenticated) {
		t.Fatal("session still valid after logout: the cookie is a bearer token again")
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Fatalf("sessions rows after logout = %d, want 0", stored)
	}
}

func TestSessionsSurviveRestartAndExpire(t *testing.T) {
	a := sessionTestApp(t)
	now := time.Now()

	live, _, err := a.sessions.issue(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	// An already-expired record, written directly.
	if _, err := a.db.Exec(`INSERT INTO sessions(id,created_at,expires_at) VALUES('stale',?,?)`, now.Add(-48*time.Hour).Unix(), now.Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}

	// Reload as a restart would.
	restarted := newSessionStore(a.db)
	if err := restarted.load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !restarted.valid(live, now) {
		t.Fatal("a live session did not survive the reload")
	}
	if restarted.valid("stale", now) {
		t.Fatal("an expired session survived the reload")
	}

	// A session past its expiry is rejected even while still in memory.
	if restarted.valid(live, now.Add(sessionTTL+time.Minute)) {
		t.Fatal("session accepted after its expiry")
	}

	restarted.prune(context.Background(), now.Add(sessionTTL+time.Minute))
	var remaining int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("sessions rows after prune = %d, want 0", remaining)
	}
}

func TestForgedAndUnknownSessionCookiesAreRejected(t *testing.T) {
	a := sessionTestApp(t)

	// Correctly signed, but no such session exists (for example, one revoked
	// by a logout on another device).
	id := "never-issued"
	value := signedCookieValue(a, id)
	request := httptest.NewRequest(http.MethodGet, "/api/monitors", nil)
	request.AddCookie(&http.Cookie{Name: "debridup_session", Value: value})
	if a.sessionOK(request) {
		t.Fatal("a signed cookie for an unknown session was accepted")
	}

	// A real session id with a broken signature.
	real, _, err := a.sessions.issue(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	tampered := httptest.NewRequest(http.MethodGet, "/api/monitors", nil)
	tampered.AddCookie(&http.Cookie{Name: "debridup_session", Value: real + ".not-a-signature"})
	if a.sessionOK(tampered) {
		t.Fatal("a cookie with an invalid signature was accepted")
	}
}

func TestLoginLocksOutAfterRepeatedFailures(t *testing.T) {
	a := sessionTestApp(t)

	for attempt := 0; attempt < loginFailureThreshold; attempt++ {
		response := httptest.NewRecorder()
		a.login(response, loginRequest("wrong-password"))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want 401", attempt, response.Code)
		}
	}

	// The next attempt is refused without hashing at all, even with the
	// correct password, because the client is now locked out.
	locked := httptest.NewRecorder()
	a.login(locked, loginRequest("correct-horse-battery-staple"))
	if locked.Code != http.StatusTooManyRequests {
		t.Fatalf("locked status = %d, want 429", locked.Code)
	}
	if locked.Header().Get("Retry-After") == "" {
		t.Fatal("a lockout must tell the client when to retry")
	}

	// A different client is unaffected.
	other := httptest.NewRecorder()
	otherRequest := loginRequest("correct-horse-battery-staple")
	otherRequest.RemoteAddr = "198.51.100.4:1111"
	a.login(other, otherRequest)
	if other.Code != http.StatusOK {
		t.Fatalf("unrelated client status = %d, want 200", other.Code)
	}
}

func TestSuccessfulLoginClearsFailureBudget(t *testing.T) {
	a := sessionTestApp(t)

	for attempt := 0; attempt < loginFailureThreshold-1; attempt++ {
		a.login(httptest.NewRecorder(), loginRequest("wrong-password"))
	}
	response := httptest.NewRecorder()
	a.login(response, loginRequest("correct-horse-battery-staple"))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	// Having signed in, the earlier failures must not count toward a lockout.
	for attempt := 0; attempt < loginFailureThreshold-1; attempt++ {
		failed := httptest.NewRecorder()
		a.login(failed, loginRequest("wrong-password"))
		if failed.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want 401", attempt, failed.Code)
		}
	}
}

// TestLoginHashingIsBounded is the memory-exhaustion guard: Argon2id costs
// 64 MiB per call, so only a bounded number may run at once.
func TestLoginHashingIsBounded(t *testing.T) {
	limiter := newLoginLimiter()
	for i := 0; i < loginMaxConcurrentHashes; i++ {
		if !limiter.acquireHashSlot(context.Background()) {
			t.Fatalf("slot %d should have been available", i)
		}
	}

	// With every slot held, a further attempt must not be admitted.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if limiter.acquireHashSlot(ctx) {
		t.Fatal("hashing admitted beyond the configured concurrency limit")
	}

	limiter.releaseHashSlot()
	if !limiter.acquireHashSlot(context.Background()) {
		t.Fatal("a released slot was not reusable")
	}
}

func TestLoginLimiterIsRaceFree(t *testing.T) {
	limiter := newLoginLimiter()
	var wg sync.WaitGroup
	now := time.Now()
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "client"
			if i%2 == 0 {
				key = "other"
			}
			limiter.lockedFor(key, now)
			limiter.recordFailure(key, now)
			if i%5 == 0 {
				limiter.recordSuccess(key)
			}
			limiter.forgetStale(now)
		}(i)
	}
	wg.Wait()
}

func TestSessionStoreIsRaceFree(t *testing.T) {
	a := sessionTestApp(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, _, err := a.sessions.issue(context.Background(), time.Now())
			if err != nil {
				return
			}
			a.sessions.valid(id, time.Now())
			a.sessions.revoke(context.Background(), id)
		}()
	}
	wg.Wait()
}

func TestLoginRejectsWhenSessionStoreIsUnavailable(t *testing.T) {
	a := testApp(t)
	if err := migrateDatabase(context.Background(), a.db); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	a.login(response, loginRequest("anything"))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] == "" {
		t.Fatal("an error response must explain what went wrong")
	}
}

func signedCookieValue(a *app, id string) string {
	mac := hmac.New(sha256.New, a.cookieKey)
	mac.Write([]byte(id))
	return base64.RawURLEncoding.EncodeToString([]byte(id)) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
