const $ = selector => document.querySelector(selector);
const fmt = value => value == null ? '—' : `${value.toFixed(2)}%`;
const date = value => value ? new Date(value * 1000).toLocaleString() : 'No checks yet';
const state = value => (value || 'checking').replaceAll('_', ' ');
let ntfyConfigured = false;
let editingMonitorID = null;
let monitorConfigs = new Map();
let refreshSequence = 0;
const providerDetails = {
  torbox: {name: 'TorBox', credential: 'API key'},
  premiumize: {name: 'Premiumize', credential: 'API key'},
  alldebrid: {name: 'AllDebrid', credential: 'API key'},
  realdebrid: {name: 'Real-Debrid', credential: 'API token'},
  torrin: {name: 'Torrin', credential: 'API key'},
  pikpak: {name: 'PikPak', credential: 'Access token', help: 'Use an access token accepted by the PikPak user API. Replace it when it expires.'},
  offcloud: {name: 'Offcloud', credential: 'API key'},
  debridlink: {name: 'Debrid-Link', credential: 'API key'},
  easydebrid: {name: 'EasyDebrid', credential: 'API key'},
  debrider: {name: 'Debrider', credential: 'API key'},
  deepbrid: {name: 'Deepbrid', credential: 'API key'},
};

function renderNtfy(config) {
  ntfyConfigured = Boolean(config.configured);
  const enabled = ntfyConfigured && Boolean(config.enabled);
  const badge = $('#ntfy-badge');
  badge.className = `channel-badge ${!ntfyConfigured ? 'unconfigured' : enabled ? 'enabled' : 'disabled'}`;
  badge.textContent = !ntfyConfigured ? 'Not configured' : enabled ? 'Enabled' : 'Configured · Disabled';
  $('#ntfy-detail').textContent = !ntfyConfigured ? 'Add a topic URL to configure this channel.' : enabled ? 'A topic URL is stored securely and notifications are active.' : 'A topic URL is stored securely, but notifications are paused.';
  $('#ntfy-url-label').textContent = ntfyConfigured ? 'Replace topic URL' : 'Topic URL';
  $('#ntfy-url').placeholder = ntfyConfigured ? 'Enter a new URL only to replace the stored one' : 'https://ntfy.sh/your-private-topic';
  $('#ntfy-url-help').textContent = ntfyConfigured ? 'The existing URL is stored securely and hidden. Leave this blank to keep it.' : 'The URL is encrypted and will not be shown again.';
  $('#ntfy-enabled').checked = enabled;
  $('#ntfy-test').disabled = !ntfyConfigured;
}

function notificationMessage(message, type = '') {
  const output = $('#ntfy-message');
  output.className = `channel-message ${type}`;
  output.textContent = message;
}

async function api(path, options = {}) {
  const response = await fetch(path, {...options, headers:{'Content-Type':'application/json', ...(options.headers || {})}});
  if (response.status === 401) { location.assign('/login.html'); throw new Error('unauthorized'); }
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error || 'Request failed');
  return body;
}

function smoothPath(points) {
  return points.slice(1).reduce((path, point, index) => {
    const previous = points[index];
    const midpoint = (previous.x + point.x) / 2;
    return `${path} C ${midpoint.toFixed(2)} ${previous.y.toFixed(2)}, ${midpoint.toFixed(2)} ${point.y.toFixed(2)}, ${point.x.toFixed(2)} ${point.y.toFixed(2)}`;
  }, `M ${points[0].x.toFixed(2)} ${points[0].y.toFixed(2)}`);
}

function shortTime(value) {
  return value ? new Date(value * 1000).toLocaleTimeString([], {hour: 'numeric', minute: '2-digit'}) : '';
}

function chart(checks, monitorID) {
  checks = Array.isArray(checks) ? checks : [];
  const points = checks.filter(c => c.Source === 'authenticated').slice(0, 120).reverse();
  if (points.length < 2) return `<section class="latency-panel latency-empty"><div><p class="latency-title">Response time</p><p class="muted">Waiting for another authenticated check</p></div><span class="latency-pulse" aria-hidden="true"></span></section>`;

  const width = 320;
  const top = 10;
  const bottom = 78;
  const durations = points.map(point => Math.max(0, Number(point.DurationMS) || 0));
  const max = Math.max(1, ...durations) * 1.12;
  const coordinates = points.map((point, index) => ({
    x: index / (points.length - 1) * width,
    y: bottom - durations[index] / max * (bottom - top),
    point,
  }));
  const line = smoothPath(coordinates);
  const area = `${line} L ${width} ${bottom} L 0 ${bottom} Z`;
  const average = Math.round(durations.reduce((sum, value) => sum + value, 0) / durations.length);
  const latest = durations[durations.length - 1];
  const gradientID = `latency-fill-${Number(monitorID) || 0}`;
  const markers = coordinates.map(({x, y, point}, index) => {
    const failed = point.State !== 'healthy';
    if (!failed && index !== coordinates.length - 1) return '';
    const label = failed ? `${state(point.State)} · ${durations[index]} ms` : `Latest · ${latest} ms`;
    return `<span class="chart-marker ${failed ? 'failure' : 'latest'}" style="left:${(x / width * 100).toFixed(3)}%;top:${(y / 96 * 100).toFixed(3)}%" title="${escapeHTML(label)}" aria-label="${escapeHTML(label)}"></span>`;
  }).join('');

  return `<section class="latency-panel"><div class="latency-head"><div><p class="latency-title">Response time</p><p class="latency-subtitle">Last ${points.length} authenticated checks</p></div><div class="latency-stats"><span><strong>${latest}</strong> ms<small>Latest</small></span><span><strong>${average}</strong> ms<small>Average</small></span></div></div><div class="chart-plot"><svg class="latency-chart" viewBox="0 0 ${width} 96" preserveAspectRatio="none" role="img" aria-label="Response times from ${shortTime(points[0].checkedAt)} to ${shortTime(points[points.length - 1].checkedAt)}"><defs><linearGradient id="${gradientID}" x1="0" y1="0" x2="0" y2="1"><stop offset="0%" stop-color="var(--blue)" stop-opacity=".28"/><stop offset="100%" stop-color="var(--blue)" stop-opacity="0"/></linearGradient></defs><g class="chart-grid"><line x1="0" y1="24" x2="320" y2="24"/><line x1="0" y1="51" x2="320" y2="51"/><line x1="0" y1="78" x2="320" y2="78"/></g><path class="chart-area" d="${area}" fill="url(#${gradientID})"/><path class="chart-line" d="${line}"/></svg>${markers}</div><div class="chart-axis"><span>${shortTime(points[0].checkedAt)}</span><span>History</span><span>${shortTime(points[points.length - 1].checkedAt)}</span></div></section>`;
}
function renderOverview(data, checksByID = {}, configsByID = new Map()) {
  $('#updated').textContent = `Updated ${date(data.generatedAt)}`;
  $('#cards').innerHTML = data.monitors.length ? data.monitors.map(m => `<article class="card"><div class="section-title"><div><p class="provider-name">${escapeHTML(m.Name)}</p><span class="provider">${escapeHTML(providerDetails[m.Provider]?.name || m.Provider)}</span></div><div class="card-status"><span class="state ${m.State}">${state(m.State)}</span><button type="button" class="quiet edit-monitor" data-monitor-id="${m.id}">Edit settings</button></div></div><div class="metric-grid"><div class="metric"><strong>${fmt(m.availability)}</strong><span>Availability</span></div><div class="metric"><strong>${fmt(m.coverage)}</strong><span>Coverage</span></div><div class="metric"><strong>${m.p95Ms == null ? '—' : `${m.p95Ms} ms`}</strong><span>p95 response time</span></div><div class="metric"><strong>${date(m.lastCheck)}</strong><span>Last authenticated check</span></div></div>${chart(checksByID[m.id] || [], m.id)}</article>`).join('') : '<article class="panel">No providers configured. Add a provider to begin monitoring.</article>';
  $('#comparison').innerHTML = data.monitors.length ? data.monitors.map(m => `<div class="bar-row"><span>${escapeHTML(m.Name)}</span><div class="bar"><i style="width:${Math.max(0,Math.min(100,m.availability || 0))}%"></i></div><strong>${fmt(m.availability)}</strong></div>`).join('') : '<p class="muted">Availability appears after authenticated checks run.</p>';
  document.querySelectorAll('.edit-monitor').forEach(button => button.addEventListener('click', () => openEditMonitor(configsByID.get(Number(button.dataset.monitorId)))));
}
function renderIncidents(items) {
	items = Array.isArray(items) ? items : [];
  $('#incidents').innerHTML = items.length ? items.map(i => { const events = Array.isArray(i.events) ? i.events : []; const endedAt = i.resolvedAt || Math.floor(Date.now() / 1000); return `<article class="incident"><div class="incident-heading"><strong>${escapeHTML(i.Name)}</strong><span class="state ${i.LatestState}">${state(i.LatestState)}</span><span class="incident-resolution ${i.resolvedAt ? 'resolved' : 'open'}">${i.resolvedAt ? 'Resolved' : 'Ongoing'}</span></div><p class="incident-summary">${escapeHTML(i.summary)}</p><p class="incident-meta">Started ${date(i.OpenedAt)} · ${formatDuration(endedAt - i.OpenedAt)}${i.resolvedAt ? ` · Recovered ${date(i.resolvedAt)}` : ''}</p>${events.length ? `<details class="incident-log"><summary>Event log · ${events.length} ${events.length === 1 ? 'entry' : 'entries'}</summary><ol>${events.map(event => `<li><time>${date(event.createdAt)}</time><span>${escapeHTML(event.summary)}</span></li>`).join('')}</ol></details>` : ''}</article>`; }).join('') : '<p class="muted">No incidents recorded.</p>';
}
function formatDuration(seconds) { seconds = Math.max(0, seconds || 0); if (seconds < 60) return `${seconds}s`; if (seconds < 3600) return `${Math.floor(seconds / 60)}m ${seconds % 60}s`; const hours = Math.floor(seconds / 3600); const minutes = Math.floor((seconds % 3600) / 60); return `${hours}h ${minutes}m`; }
function escapeHTML(text) { const e = document.createElement('span'); e.textContent = text || ''; return e.innerHTML; }
async function refresh() { const sequence = ++refreshSequence; const [overview, incidents, ntfy, settings] = await Promise.all([api('/api/overview'), api('/api/incidents'), api('/api/notifications/ntfy'), api('/api/monitors')]); const monitors = Array.isArray(overview.monitors) ? overview.monitors : []; const checks = await Promise.all(monitors.map(m => api(`/api/monitors/${m.id}/checks`).catch(() => []))); if (sequence !== refreshSequence) return; monitorConfigs = new Map((Array.isArray(settings) ? settings : []).map(m => [m.id, m])); renderOverview({...overview, monitors}, Object.fromEntries(monitors.map((m, i) => [m.id, checks[i]])), monitorConfigs); renderIncidents(incidents); renderNtfy(ntfy); }

function resetMonitorDialog() {
  editingMonitorID = null;
  $('#monitor-form').reset();
  $('#provider').disabled = false;
  $('#api-key').required = true;
  setCredentialCopy($('#provider').value, false);
  $('#monitor-enabled').checked = true;
  $('#monitor-dialog-eyebrow').textContent = 'NEW MONITOR';
  $('#monitor-dialog-title').textContent = 'Add provider';
  $('#monitor-submit').textContent = 'Add provider';
  $('#delete-monitor').hidden = true;
  $('#reset-monitor-stats').hidden = true;
  $('#monitor-error').textContent = '';
}

function setCredentialCopy(provider, replacing) {
  const details = providerDetails[provider] || {credential: 'API key'};
  const sentenceCredential = details.credential === 'Access token' ? 'access token' : details.credential;
  $('#api-key-label').textContent = replacing ? `Replace ${sentenceCredential}` : details.credential;
  $('#api-key-help').textContent = replacing ? `The existing ${sentenceCredential} is stored securely. Leave this blank to keep it.` : (details.help || `Enter the ${sentenceCredential} issued by the provider.`);
}

function openCreateMonitor() {
  resetMonitorDialog();
  $('#monitor-dialog').showModal();
}

function openEditMonitor(config) {
  if (!config) return;
  resetMonitorDialog();
  editingMonitorID = config.id;
  $('#provider').value = config.provider;
  $('#provider').disabled = true;
  $('#name').value = config.name;
  $('#interval').value = config.intervalSeconds;
  $('#timeout').value = config.timeoutSeconds;
  $('#failure').value = config.failureThreshold;
  $('#recovery').value = config.recoveryThreshold;
  $('#monitor-enabled').checked = config.enabled;
  $('#public-check').checked = config.publicCheck;
  $('#api-key').required = false;
  setCredentialCopy(config.provider, true);
  $('#monitor-dialog-eyebrow').textContent = config.provider.toUpperCase();
  $('#monitor-dialog-title').textContent = 'Edit provider';
  $('#monitor-submit').textContent = 'Save changes';
  $('#delete-monitor').hidden = false;
  $('#reset-monitor-stats').hidden = false;
  $('#monitor-dialog').showModal();
}

$('#refresh').addEventListener('click', () => refresh().catch(showError));
$('#logout').addEventListener('click', async () => { await fetch('/logout',{method:'POST'}); location.assign('/login.html'); });
$('#add-monitor').addEventListener('click', openCreateMonitor);
$('#close-dialog').addEventListener('click', () => { $('#monitor-dialog').close(); resetMonitorDialog(); });
$('#provider').addEventListener('change', event => {
  const details = providerDetails[event.target.value];
  $('#name').value = details?.name || event.target.value;
  setCredentialCopy(event.target.value, false);
});
$('#monitor-form').addEventListener('submit', async event => {
  event.preventDefault(); $('#monitor-error').textContent = '';
  const payload = {name:$('#name').value,apiKey:$('#api-key').value,intervalSeconds:+$('#interval').value,timeoutSeconds:+$('#timeout').value,failureThreshold:+$('#failure').value,recoveryThreshold:+$('#recovery').value,publicCheck:$('#public-check').checked};
  if (editingMonitorID) payload.enabled = $('#monitor-enabled').checked; else payload.provider = $('#provider').value;
  try { await api(editingMonitorID ? `/api/monitors/${editingMonitorID}` : '/api/monitors',{method:editingMonitorID ? 'PUT' : 'POST',body:JSON.stringify(payload)}); $('#monitor-dialog').close(); resetMonitorDialog(); await refresh(); } catch (error) { $('#monitor-error').textContent = error.message; }
});
$('#delete-monitor').addEventListener('click', async () => { if (!editingMonitorID) return; const config = monitorConfigs.get(editingMonitorID); if (!window.confirm(`Delete ${config?.name || 'this provider'}? This permanently removes its checks and incident history.`)) return; try { await api(`/api/monitors/${editingMonitorID}`, {method:'DELETE'}); $('#monitor-dialog').close(); resetMonitorDialog(); await refresh(); } catch(error) { $('#monitor-error').textContent = error.message; } });
$('#reset-monitor-stats').addEventListener('click', async () => { if (!editingMonitorID) return; const config = monitorConfigs.get(editingMonitorID); if (!window.confirm(`Reset all stats for ${config?.name || 'this provider'}? Its checks and incident history will be permanently cleared. Provider settings and credentials will be kept.`)) return; try { await api(`/api/monitors/${editingMonitorID}/reset`, {method:'POST'}); $('#monitor-dialog').close(); resetMonitorDialog(); await refresh(); } catch(error) { $('#monitor-error').textContent = error.message; } });
$('#reset-all-stats').addEventListener('click', async () => { if (!window.confirm('Reset stats for every provider? All checks and incident history will be permanently cleared. Provider settings, credentials, and notification settings will be kept.')) return; const button = $('#reset-all-stats'); button.disabled = true; try { await api('/api/stats/reset', {method:'POST'}); await refresh(); } catch(error) { showError(error); } finally { button.disabled = false; } });
$('#ntfy-form').addEventListener('submit', async event => { event.preventDefault(); const url=$('#ntfy-url').value.trim(); if (!url && !ntfyConfigured) { notificationMessage('Enter the complete ntfy topic URL first.', 'error'); return; } try { notificationMessage('Saving…'); const result = await api('/api/notifications/ntfy',{method:'PUT',body:JSON.stringify({url,enabled:$('#ntfy-enabled').checked})}); $('#ntfy-url').value=''; renderNtfy(result); notificationMessage(result.enabled ? 'Settings saved. Incident notifications are enabled.' : 'Settings saved. The channel is configured but disabled.', 'success'); } catch(error) { notificationMessage(error.message, 'error'); } });
$('#ntfy-test').addEventListener('click', async () => { const button = $('#ntfy-test'); button.disabled = true; notificationMessage('Sending test notification…'); try { await api('/api/notifications/ntfy/test', {method:'POST'}); notificationMessage('Test notification delivered successfully.', 'success'); } catch(error) { notificationMessage(`Test failed: ${error.message}`, 'error'); } finally { button.disabled = !ntfyConfigured; } });
function showError(error) { console.error(error); $('#updated').textContent = `Unable to refresh dashboard: ${error.message || 'unknown error'}`; }
refresh().catch(showError);
setInterval(() => refresh().catch(showError), 30_000);
