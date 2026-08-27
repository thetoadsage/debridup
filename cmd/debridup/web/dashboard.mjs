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

function valueFor(object, lowerCase, upperCase) {
  if (Object.prototype.hasOwnProperty.call(object || {}, lowerCase)) return object[lowerCase];
  return object?.[upperCase];
}

// The Go API deliberately keeps its established capitalized incident fields,
// while event fields use lower camel case. Keep that boundary here so the
// renderer is explicit about the contract (and remains compatible with a
// future lower-camel incident response).
export function normalizeIncident(item = {}) {
  return {
    name: valueFor(item, 'name', 'Name'),
    provider: valueFor(item, 'provider', 'Provider'),
    latestState: valueFor(item, 'latestState', 'LatestState'),
    summary: valueFor(item, 'summary', 'Summary'),
    openedAt: valueFor(item, 'openedAt', 'OpenedAt'),
    resolvedAt: valueFor(item, 'resolvedAt', 'ResolvedAt'),
    ongoing: Boolean(valueFor(item, 'ongoing', 'Ongoing')),
    events: Array.isArray(item.events) ? item.events : [],
  };
}

export function startDashboard({api, document, window, timeZone, onRefresh} = {}) {
  const status = document.getElementById('dashboard-status');
  const summary = document.getElementById('summary');
  const providers = document.getElementById('provider-table-body');
  const checks = document.getElementById('check-log-body');
  const incidents = document.getElementById('incident-list');
  const incidentMessage = document.getElementById('incident-message');
  const historyMessage = document.getElementById('history-message');
  const refreshButton = document.getElementById('refresh');
  const moreButton = document.getElementById('load-more-checks');
  let timer = null;
  let activeRefresh = null;
  let stopped = false;
  let cursor = null;
  let rows = [];
  let incidentRows = [];
  let health = null;
  let checksLoaded = false;
  let incidentsLoaded = false;
  let activeLoadMore = null;
  let zone = timeZone;

  function schedule() {
    window.clearTimeout(timer);
    if (!stopped && !document.hidden) timer = window.setTimeout(() => void refresh(), REFRESH_INTERVAL_MS);
  }

  function renderHealth(data) {
    health = data;
    const s = data.summary || {};
    const list = Array.isArray(data.providers) ? data.providers : [];
    const online = Number(s.providersOnline) || 0;
    const active = Number(s.activeIncidents) || 0;
    const enabled = list.filter(provider => provider?.enabled !== false).length;
    const onlineClass = enabled === 0 ? 'summary-card-neutral' : online === enabled ? 'summary-card-online' : 'summary-card-alert';
    summary.innerHTML = `<h2 id="summary-heading" class="sr-only">Current status summary</h2><article class="summary-card summary-card-status ${stateClass(s.overallState)}"><span class="summary-label">Overall status</span><strong class="summary-value summary-state ${stateClass(s.overallState)}">${escapeHTML(displayState(s.overallState))}</strong><span class="summary-detail">Live service assessment</span></article><article class="summary-card ${onlineClass}"><span class="summary-label">Providers online</span><strong class="summary-value">${online}</strong><span class="summary-detail">${enabled ? `${online} of ${enabled} enabled services` : 'No enabled services'}</span></article><article class="summary-card ${active ? 'summary-card-alert' : 'summary-card-clear'}"><span class="summary-label">Active incidents</span><strong class="summary-value">${active}</strong><span class="summary-detail">${active ? 'Needs attention now' : 'No open incidents'}</span></article>`;
    providers.innerHTML = list.length ? list.map(p => `<tr><th scope="row">${escapeHTML(p.name || 'Unnamed provider')}</th><td><span class="state ${stateClass(p.state)}">${escapeHTML(displayState(p.state))}</span></td><td>${escapeHTML(formatLatency(p.latencyMs))}</td><td>${escapeHTML(formatTimestamp(p.lastCheck, zone))}</td><td>${p.activeIncident ? '<strong>Active incident</strong>' : '—'}</td></tr>`).join('') : '<tr><td colspan="5"><div class="dashboard-empty"><p>No providers configured.</p><a href="#providers">Add a provider to begin monitoring.</a></div></td></tr>';
    summary.setAttribute('aria-busy', 'false');
    providers.setAttribute('aria-busy', 'false');
  }

  function renderChecks() {
    checks.innerHTML = rows.length ? rows.map(c => `<tr><td>${escapeHTML(c.name)}</td><td>${escapeHTML(c.source)}</td><td><span class="state ${stateClass(c.state)}">${escapeHTML(displayState(c.state))}</span></td><td>${escapeHTML(formatLatency(c.durationMs))}</td><td>${escapeHTML(diagnostic(c))}</td><td>${escapeHTML(formatTimestamp(c.checkedAt, zone))}</td><td>${c.incidentId ? `#${Number(c.incidentId)}` : '—'}</td></tr>`).join('') : '<tr><td colspan="7" class="muted">No retained check responses.</td></tr>';
    checks.setAttribute('aria-busy', 'false');
    moreButton.hidden = !cursor;
    if (historyMessage) historyMessage.textContent = rows.length ? `${rows.length} retained responses loaded` : 'No retained responses';
  }

  function renderIncidents(items) {
    if (!incidents) return;
    incidentRows = Array.isArray(items) ? items : [];
    const list = incidentRows;
    incidents.innerHTML = list.length ? list.map(rawItem => {
      const item = normalizeIncident(rawItem);
      const resolved = item.resolvedAt !== null && item.resolvedAt !== undefined;
      const state = resolved ? 'healthy' : stateClass(item.latestState);
      const events = item.events;
      const details = events.length ? `<details><summary>${events.length} event${events.length === 1 ? '' : 's'}</summary><ul class="incident-events">${events.map(event => `<li><strong>${escapeHTML(event.type || 'event')}</strong> · ${escapeHTML(event.summary || displayState(event.state))}<time>${escapeHTML(formatTimestamp(event.createdAt, zone))}</time></li>`).join('')}</ul></details>` : '—';
      return `<tr><td><span class="state ${state}">${resolved ? 'Resolved' : 'Ongoing'}</span></td><th scope="row"><strong>${escapeHTML(item.name || 'Unnamed provider')}</strong><small>${escapeHTML(item.provider || '')}</small></th><td class="incident-summary">${escapeHTML(item.summary || displayState(item.latestState))}</td><td>${escapeHTML(formatTimestamp(item.openedAt, zone))}</td><td>${resolved ? escapeHTML(formatTimestamp(item.resolvedAt, zone)) : '—'}</td><td>${details}</td></tr>`;
    }).join('') : '<tr><td colspan="6" class="muted">No incidents have been recorded.</td></tr>';
    incidents.setAttribute('aria-busy', 'false');
    if (incidentMessage) incidentMessage.textContent = list.length ? `${list.length} incidents loaded` : 'No recorded incidents';
  }

  function renderIncidentError() {
    if (!incidents) return;
    incidents.innerHTML = '<tr><td colspan="6" class="muted">Unable to load incidents. Refresh to retry.</td></tr>';
    incidents.setAttribute('aria-busy', 'false');
    if (incidentMessage) incidentMessage.textContent = 'Could not load incidents.';
  }

  function renderStatus(message) { status.innerHTML = message; }
  function age(data = health) {
    return data?.generatedAt ? Math.max(0, Date.now() - data.generatedAt * 1000) : 0;
  }

  function applyCheckPage(page, reset) {
    rows = reset ? (page.checks || []) : rows.concat(page.checks || []);
    cursor = page.nextBefore || null;
    checksLoaded = true;
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
    moreButton.disabled = true;
    activeRefresh = (async () => {
      try {
        // Finish a requested older page first. Otherwise its append can race a
        // fresh first page and make the log appear to jump or duplicate rows.
        if (activeLoadMore) await activeLoadMore;
        const [dashboardResult, checksResult, incidentResult] = await Promise.allSettled([
          api('/api/dashboard'),
          api('/api/checks?limit=100'),
          api('/api/incidents'),
        ]);
        if (dashboardResult.status === 'fulfilled') {
          const data = dashboardResult.value;
          renderHealth(data);
          try { onRefresh?.(); } catch (error) { console.error(error); }
          const stale = age(data) > STALE_AFTER_MS;
          renderStatus(`${stale ? 'Data is stale. ' : ''}Updated ${escapeHTML(formatTimestamp(data.generatedAt, zone))}`);
        } else if (health) {
          const stale = age() > STALE_AFTER_MS;
          renderStatus(`${stale ? 'Data is stale. ' : ''}Unable to refresh. Showing the last successful data. <button class="status-retry" type="button">Retry</button>`);
        } else {
          renderStatus('Dashboard data is unavailable. <button class="status-retry" type="button">Retry</button>');
          summary.innerHTML = '<article class="summary-card"><span class="summary-label">Overall status</span><strong class="summary-value summary-state unknown">Unavailable</strong></article>';
          providers.innerHTML = '<tr><td colspan="5" class="muted">Current service health is unavailable.</td></tr>';
          checks.innerHTML = '<tr><td colspan="7" class="muted">Response history is unavailable.</td></tr>';
          renderIncidentError();
          summary.setAttribute('aria-busy', 'false');
          providers.setAttribute('aria-busy', 'false');
          checks.setAttribute('aria-busy', 'false');
        }
        if (checksResult.status === 'fulfilled') {
          applyCheckPage(checksResult.value, true);
        } else if (!checksLoaded) {
          checks.innerHTML = '<tr><td colspan="7" class="muted">Response history is unavailable.</td></tr>';
          checks.setAttribute('aria-busy', 'false');
          if (historyMessage) historyMessage.textContent = 'Could not load response history.';
        } else if (historyMessage) {
          historyMessage.textContent = 'Could not refresh response history; showing retained results.';
        }
        if (incidentResult.status === 'fulfilled' && Array.isArray(incidentResult.value)) {
          incidentsLoaded = true;
          renderIncidents(incidentResult.value);
        } else if (!incidentsLoaded) {
          renderIncidentError();
        } else if (incidentMessage) {
          incidentMessage.textContent = 'Could not refresh incidents; showing retained results.';
        }
        status.querySelector?.('.status-retry')?.addEventListener('click', () => void refresh());
      } finally {
        activeRefresh = null;
        refreshButton.disabled = false;
        moreButton.disabled = false;
        schedule();
      }
    })();
    return activeRefresh;
  }

  refreshButton?.addEventListener('click', () => void refresh());
  moreButton?.addEventListener('click', () => {
    if (activeLoadMore || activeRefresh) return;
    moreButton.disabled = true;
    activeLoadMore = loadChecks(false).catch(() => {
      renderStatus('Unable to load more responses. Existing data is unchanged.');
      if (historyMessage) historyMessage.textContent = 'Could not load older responses.';
    }).finally(() => {
      activeLoadMore = null;
      moreButton.disabled = false;
    });
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
      renderIncidents(incidentRows);
    },
    stop() {
      stopped = true;
      window.clearTimeout(timer);
    },
  };
}
