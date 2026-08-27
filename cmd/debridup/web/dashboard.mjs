import {escapeHTML, formatLatency, formatTimestamp} from './dashboard-model.mjs';

const REFRESH_INTERVAL_MS = 30_000;
const STALE_AFTER_MS = 90_000;
const STATES = new Set(['healthy', 'slow', 'degraded', 'outage', 'auth_failed', 'api_issue', 'connection_issue', 'unknown', 'paused']);
const stateClass = value => STATES.has(value) ? value : 'unknown';
const displayState = value => ({healthy: 'Up', slow: 'Slow', auth_failed: 'Auth issue', api_issue: 'Down', connection_issue: 'Down', degraded: 'Down', outage: 'Down', paused: 'Paused', unknown: 'Unknown'})[value] || 'Unknown';

function diagnostic(check) {
  const parts = [];
  if (Number.isFinite(check.httpStatus)) parts.push(`HTTP ${check.httpStatus}`);
  if (check.errorCode) parts.push(check.errorCode);
  if (check.errorDetail) parts.push(check.errorDetail);
  return parts.join(' · ') || '—';
}

export function startDashboard({api, document, window, timeZone, onRefresh} = {}) {
  const status = document.getElementById('dashboard-status');
  const summary = document.getElementById('summary');
  const providers = document.getElementById('provider-table-body');
  const checks = document.getElementById('check-log-body');
  const refreshButton = document.getElementById('refresh');
  const moreButton = document.getElementById('load-more-checks');
  let timer = null;
  let activeRefresh = null;
  let stopped = false;
  let cursor = null;
  let rows = [];
  let health = null;
  let zone = timeZone;

  function schedule() {
    window.clearTimeout(timer);
    if (!stopped && !document.hidden) timer = window.setTimeout(() => void refresh(), REFRESH_INTERVAL_MS);
  }

  function renderHealth(data) {
    health = data;
    const s = data.summary || {};
    summary.innerHTML = `<h2 id="summary-heading" class="sr-only">Current status summary</h2><article class="summary-card"><span class="summary-label">Overall status</span><strong class="summary-value summary-state ${stateClass(s.overallState)}">${escapeHTML(displayState(s.overallState))}</strong></article><article class="summary-card"><span class="summary-label">Services up</span><strong class="summary-value">${Number(s.providersOnline) || 0}</strong></article><article class="summary-card"><span class="summary-label">Active incidents</span><strong class="summary-value">${Number(s.activeIncidents) || 0}</strong></article>`;
    const list = Array.isArray(data.providers) ? data.providers : [];
    providers.innerHTML = list.length ? list.map(p => `<tr><th scope="row">${escapeHTML(p.name || 'Unnamed provider')}</th><td><span class="state ${stateClass(p.state)}">${escapeHTML(displayState(p.state))}</span></td><td>${escapeHTML(formatLatency(p.latencyMs))}</td><td>${escapeHTML(formatTimestamp(p.lastCheck, zone))}</td><td>${p.activeIncident ? '<strong>Active incident</strong>' : '—'}</td></tr>`).join('') : '<tr><td colspan="5"><div class="dashboard-empty"><p>No providers configured.</p><a href="#provider-management">Add a provider to begin monitoring.</a></div></td></tr>';
    summary.setAttribute('aria-busy', 'false');
    providers.setAttribute('aria-busy', 'false');
  }

  function renderChecks() {
    checks.innerHTML = rows.length ? rows.map(c => `<tr><td>${escapeHTML(c.name)}</td><td>${escapeHTML(c.source)}</td><td><span class="state ${stateClass(c.state)}">${escapeHTML(displayState(c.state))}</span></td><td>${escapeHTML(formatLatency(c.durationMs))}</td><td>${escapeHTML(diagnostic(c))}</td><td>${escapeHTML(formatTimestamp(c.checkedAt, zone))}</td><td>${c.incidentId ? `#${Number(c.incidentId)}` : '—'}</td></tr>`).join('') : '<tr><td colspan="7" class="muted">No retained check responses.</td></tr>';
    checks.setAttribute('aria-busy', 'false');
    moreButton.hidden = !cursor;
  }

  function renderStatus(message) { status.innerHTML = message; }
  function age(data = health) {
    return data?.generatedAt ? Math.max(0, Date.now() - data.generatedAt * 1000) : 0;
  }

  function applyCheckPage(page, reset) {
    rows = reset ? (page.checks || []) : rows.concat(page.checks || []);
    cursor = page.nextBefore || null;
    renderChecks();
  }

  async function loadChecks(reset) {
    const suffix = reset || !cursor ? '' : `&before=${encodeURIComponent(cursor)}`;
    const page = await api(`/api/checks?limit=100${suffix}`);
    applyCheckPage(page, reset);
  }

  async function refresh() {
    if (stopped) return health;
    if (activeRefresh) return activeRefresh;
    refreshButton.disabled = true;
    activeRefresh = (async () => {
      try {
        const [data, page] = await Promise.all([
          api('/api/dashboard'),
          api('/api/checks?limit=100'),
        ]);
        renderHealth(data);
        applyCheckPage(page, true);
        try { onRefresh?.(); } catch (error) { console.error(error); }
        const stale = age(data) > STALE_AFTER_MS;
        renderStatus(`${stale ? 'Data is stale. ' : ''}Updated ${escapeHTML(formatTimestamp(data.generatedAt, zone))}`);
      } catch (error) {
        if (health) {
          const stale = age() > STALE_AFTER_MS;
          renderStatus(`${stale ? 'Data is stale. ' : ''}Unable to refresh. Showing the last successful data. <button class="status-retry" type="button">Retry</button>`);
        } else {
          renderStatus('Dashboard data is unavailable. <button class="status-retry" type="button">Retry</button>');
          summary.innerHTML = '<article class="summary-card"><span class="summary-label">Overall status</span><strong class="summary-value summary-state unknown">Unavailable</strong></article>';
          providers.innerHTML = '<tr><td colspan="5" class="muted">Current service health is unavailable.</td></tr>';
          checks.innerHTML = '<tr><td colspan="7" class="muted">Response history is unavailable.</td></tr>';
          summary.setAttribute('aria-busy', 'false');
          providers.setAttribute('aria-busy', 'false');
          checks.setAttribute('aria-busy', 'false');
        }
        status.querySelector?.('.status-retry')?.addEventListener('click', () => void refresh());
      } finally {
        activeRefresh = null;
        refreshButton.disabled = false;
        schedule();
      }
    })();
    return activeRefresh;
  }

  refreshButton?.addEventListener('click', () => void refresh());
  moreButton?.addEventListener('click', () => {
    moreButton.disabled = true;
    void loadChecks(false).catch(() => renderStatus('Unable to load more responses. Existing data is unchanged.')).finally(() => { moreButton.disabled = false; });
  });
  document.addEventListener('visibilitychange', () => {
    if (document.hidden) window.clearTimeout(timer);
    else void refresh();
  });
  void refresh();
  return {
    refresh,
    setTimeZone(value) {
      zone = value;
      if (health) renderHealth(health);
      renderChecks();
    },
    stop() {
      stopped = true;
      window.clearTimeout(timer);
    },
  };
}
