package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	sessionTTL = 24 * time.Hour

	// Argon2id is deliberately expensive: 64 MiB and 4 threads per call. Without
	// a cap, unauthenticated POST /login requests allocate that much each and
	// can exhaust memory on the small hosts this project targets.
	loginMaxConcurrentHashes = 2
	loginHashWaitTimeout     = 5 * time.Second

	// Per-client throttling. The app is single-admin, so this can be strict
	// without inconveniencing a legitimate operator.
	loginFailureThreshold = 5
	loginFailureWindow    = 15 * time.Minute
	loginLockoutBase      = time.Minute
	loginLockoutMax       = 15 * time.Minute
)

// sessionStore keeps issued sessions so that signing out actually revokes a
// session rather than only clearing the cookie in one browser. Records are
// persisted so a restart does not sign the operator out, and mirrored in memory
// so authenticating a request costs no database round trip.
type sessionStore struct {
	db     *sql.DB
	mu     sync.RWMutex
	active map[string]int64
}

func newSessionStore(db *sql.DB) *sessionStore {
	return &sessionStore{db: db, active: make(map[string]int64)}
}

func (s *sessionStore) load(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at<=?`, time.Now().Unix()); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,expires_at FROM sessions`)
	if err != nil {
		return err
	}
	defer rows.Close()
	loaded := make(map[string]int64)
	for rows.Next() {
		var id string
		var expiresAt int64
		if err := rows.Scan(&id, &expiresAt); err != nil {
			return err
		}
		loaded[id] = expiresAt
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.active = loaded
	s.mu.Unlock()
	return nil
}

func (s *sessionStore) issue(ctx context.Context, now time.Time) (string, int64, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", 0, err
	}
	id := base64.RawURLEncoding.EncodeToString(raw)
	expiresAt := now.Add(sessionTTL).Unix()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO sessions(id,created_at,expires_at) VALUES(?,?,?)`, id, now.Unix(), expiresAt); err != nil {
		return "", 0, err
	}
	s.mu.Lock()
	s.active[id] = expiresAt
	s.mu.Unlock()
	return id, expiresAt, nil
}

func (s *sessionStore) valid(id string, now time.Time) bool {
	s.mu.RLock()
	expiresAt, present := s.active[id]
	s.mu.RUnlock()
	return present && now.Unix() < expiresAt
}

func (s *sessionStore) revoke(ctx context.Context, id string) {
	s.mu.Lock()
	delete(s.active, id)
	s.mu.Unlock()
	_, _ = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id=?`, id)
}

// prune drops expired sessions from both memory and storage.
func (s *sessionStore) prune(ctx context.Context, now time.Time) {
	cutoff := now.Unix()
	s.mu.Lock()
	for id, expiresAt := range s.active {
		if expiresAt <= cutoff {
			delete(s.active, id)
		}
	}
	s.mu.Unlock()
	_, _ = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at<=?`, cutoff)
}

// loginLimiter bounds both how many password hashes run at once and how often a
// single client may attempt a sign-in.
type loginLimiter struct {
	hashSlots chan struct{}
	mu        sync.Mutex
	clients   map[string]*loginAttempts
}

type loginAttempts struct {
	failures    int
	firstFailed time.Time
	lockedUntil time.Time
	lockouts    int
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{
		hashSlots: make(chan struct{}, loginMaxConcurrentHashes),
		clients:   make(map[string]*loginAttempts),
	}
}

// clientKey identifies the caller for throttling. RemoteAddr is used directly:
// the app is meant to sit behind a trusted reverse proxy, and trusting a
// forwarded header would let anyone reset their own budget.
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// lockedFor reports how long the client must wait, if it is currently locked.
func (l *loginLimiter) lockedFor(key string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	record, present := l.clients[key]
	if !present {
		return 0
	}
	if now.Before(record.lockedUntil) {
		return record.lockedUntil.Sub(now)
	}
	// The failure window has passed with no lockout in force; start fresh.
	if !record.firstFailed.IsZero() && now.Sub(record.firstFailed) > loginFailureWindow {
		delete(l.clients, key)
	}
	return 0
}

func (l *loginLimiter) recordFailure(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	record, present := l.clients[key]
	if !present || (!record.firstFailed.IsZero() && now.Sub(record.firstFailed) > loginFailureWindow) {
		record = &loginAttempts{}
		l.clients[key] = record
	}
	if record.failures == 0 {
		record.firstFailed = now
	}
	record.failures++
	if record.failures >= loginFailureThreshold {
		lockout := loginLockoutBase << min(record.lockouts, 4)
		if lockout > loginLockoutMax {
			lockout = loginLockoutMax
		}
		record.lockedUntil = now.Add(lockout)
		record.lockouts++
		record.failures = 0
		record.firstFailed = time.Time{}
	}
}

func (l *loginLimiter) recordSuccess(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.clients, key)
}

// acquireHashSlot bounds concurrent Argon2id work. It fails rather than queuing
// without limit, so a burst is rejected cheaply instead of piling up.
func (l *loginLimiter) acquireHashSlot(ctx context.Context) bool {
	timer := time.NewTimer(loginHashWaitTimeout)
	defer timer.Stop()
	select {
	case l.hashSlots <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

func (l *loginLimiter) releaseHashSlot() { <-l.hashSlots }

// forgetStale drops throttling state for clients that have gone quiet.
func (l *loginLimiter) forgetStale(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, record := range l.clients {
		if now.After(record.lockedUntil) && (record.firstFailed.IsZero() || now.Sub(record.firstFailed) > loginFailureWindow) {
			delete(l.clients, key)
		}
	}
}
