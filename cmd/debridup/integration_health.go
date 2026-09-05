package main

import (
	"net/http"
	"time"
)

// providerHealth exports only availability, without sessions or provider secrets.
// It reads persisted authenticated results; polling never triggers provider calls.
func (a *app) providerHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	respond := func(code int, state string) {
		writeJSON(w, code, struct {
			OK    bool   `json:"ok"`
			State string `json:"state"`
		}{code == http.StatusOK, state})
	}
	provider := r.PathValue("provider")
	if _, exists := providerDefinitions[provider]; !exists {
		respond(http.StatusNotFound, "not_found")
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT m.enabled,m.interval_seconds,m.timeout_seconds,
		COALESCE(s.current_state,''),COALESCE(s.last_check_at,0)
		FROM monitors m LEFT JOIN monitor_states s ON s.monitor_id=m.id WHERE m.provider=?`, provider)
	if err != nil {
		respond(http.StatusServiceUnavailable, "unavailable")
		return
	}
	defer rows.Close()
	now := time.Now().Unix()
	found, enabled, up := false, false, true
	for rows.Next() {
		var active bool
		var interval, timeout, checked int64
		var state string
		if err := rows.Scan(&active, &interval, &timeout, &state, &checked); err != nil {
			respond(http.StatusServiceUnavailable, "unavailable")
			return
		}
		found = true
		if !active {
			continue
		}
		enabled = true
		// Allow two polling intervals plus a probe timeout before calling data stale.
		// Newly created/reset monitors default to healthy but have no check timestamp.
		if state != stateHealthy || checked <= 0 || checked > now || now-checked > 2*interval+timeout {
			up = false
		}
	}
	if err := rows.Err(); err != nil {
		respond(http.StatusServiceUnavailable, "unavailable")
	} else if !found {
		respond(http.StatusNotFound, "not_found")
	} else if !enabled {
		respond(http.StatusServiceUnavailable, "paused")
	} else if !up {
		respond(http.StatusServiceUnavailable, "unhealthy")
	} else {
		respond(http.StatusOK, "healthy")
	}
}
