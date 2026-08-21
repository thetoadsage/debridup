package main

import (
	"fmt"
	"strconv"
	"sync"
	"time"
)

const defaultMaxConcurrentChecks = 4

func parseMaxConcurrentChecks(value string) (int, error) {
	if value == "" {
		return defaultMaxConcurrentChecks, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 || limit > 32 {
		return 0, fmt.Errorf("DEBRIDUP_MAX_CONCURRENT_CHECKS must be an integer from 1 through 32")
	}
	return limit, nil
}

type runCoordinator struct {
	mu       sync.Mutex
	lastRuns map[int64]time.Time
	inFlight map[int64]struct{}
	active   int
	limit    int
}

func newRunCoordinator(limit int) *runCoordinator {
	return &runCoordinator{
		lastRuns: make(map[int64]time.Time),
		inFlight: make(map[int64]struct{}),
		limit:    limit,
	}
}

func (c *runCoordinator) Claim(id int64, now time.Time, interval time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	last := c.lastRuns[id]
	if !last.IsZero() && now.Sub(last) < interval {
		return false
	}
	if _, running := c.inFlight[id]; running || c.active >= c.limit {
		return false
	}
	c.lastRuns[id] = now
	c.inFlight[id] = struct{}{}
	c.active++
	return true
}

func (c *runCoordinator) Release(id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, running := c.inFlight[id]; !running {
		return
	}
	delete(c.inFlight, id)
	c.active--
}

func (c *runCoordinator) Forget(id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.lastRuns, id)
}

func (a *app) scheduler() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		a.runDueMonitorsAt(time.Now())
		<-ticker.C
	}
}

func (a *app) runDueMonitorsAt(now time.Time) {
	monitors, err := a.monitors()
	if err != nil {
		a.logger.Error("load monitors", "error", err)
		return
	}
	for _, m := range monitors {
		if !m.Enabled || !a.runs.Claim(m.ID, now, time.Duration(m.IntervalSeconds)*time.Second) {
			continue
		}
		go func(m monitor) {
			defer a.runs.Release(m.ID)
			a.runMonitor(m)
		}(m)
	}
}

func (a *app) runMonitor(m monitor) {
	secret, err := a.monitorSecret(m.ID)
	if err != nil {
		a.logger.Error("load monitor secret", "monitor", m.ID, "error", err)
		return
	}
	result := a.authCheck(m, secret)
	if _, err = a.recordResult(m, "authenticated", result); err != nil {
		a.logger.Error("record check", "monitor", m.ID, "error", err)
	}
	if m.PublicCheck {
		publicResult := a.publicCheck(m)
		if _, err = a.recordResult(m, "public", publicResult); err != nil {
			a.logger.Error("record public check", "monitor", m.ID, "error", err)
		}
	}
}
