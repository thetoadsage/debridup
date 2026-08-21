const $ = selector => document.querySelector(selector);
const fmt = value => value == null ? '—' : `${value.toFixed(2)}%`;
const date = value => value ? new Date(value * 1000).toLocaleString() : 'No checks yet';
const state = value => (value || 'checking').replaceAll('_', ' ');
let ntfyConfigured = false;

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

function chart(checks) {
	checks = Array.isArray(checks) ? checks : [];
  const points = checks.filter(c => c.Source === 'authenticated').slice(0, 120).reverse();
  if (points.length < 2) return '<p class="muted graph-empty">Response-time graph appears after two checks.</p>';
  const max = Math.max(1, ...points.map(c => c.DurationMS));
  const line = points.map((c, i) => `${(i / (points.length - 1) * 100).toFixed(2)},${(34 - Math.min(32, c.DurationMS / max * 32)).toFixed(2)}`).join(' ');
  const failures = points.map((c, i) => c.State === 'healthy' ? '' : `<circle cx="${(i / (points.length - 1) * 100).toFixed(2)}" cy="36" r="1.8" class="failure-dot"/>`).join('');
  return `<svg class="latency-chart" viewBox="0 0 100 38" preserveAspectRatio="none" aria-label="Recent response times"><polyline points="${line}"/>${failures}</svg>`;
}
function renderOverview(data, checksByID = {}) {
  $('#updated').textContent = `Updated ${date(data.generatedAt)}`;
  $('#cards').innerHTML = data.monitors.length ? data.monitors.map(m => `<article class="card"><div class="section-title"><div><p class="provider-name">${escapeHTML(m.Name)}</p><span class="provider">${escapeHTML(m.Provider)}</span></div><span class="state ${m.State}">${state(m.State)}</span></div><div class="metric-grid"><div class="metric"><strong>${fmt(m.availability)}</strong><span>Availability</span></div><div class="metric"><strong>${fmt(m.coverage)}</strong><span>Coverage</span></div><div class="metric"><strong>${m.p95Ms == null ? '—' : `${m.p95Ms} ms`}</strong><span>p95 response time</span></div><div class="metric"><strong>${date(m.lastCheck)}</strong><span>Last authenticated check</span></div></div><p class="graph-label">Recent authenticated response times</p>${chart(checksByID[m.id] || [])}</article>`).join('') : '<article class="panel">No providers configured. Add TorBox or Premiumize to begin monitoring.</article>';
  $('#comparison').innerHTML = data.monitors.length ? data.monitors.map(m => `<div class="bar-row"><span>${escapeHTML(m.Name)}</span><div class="bar"><i style="width:${Math.max(0,Math.min(100,m.availability || 0))}%"></i></div><strong>${fmt(m.availability)}</strong></div>`).join('') : '<p class="muted">Availability appears after authenticated checks run.</p>';
}
function renderIncidents(items) {
	items = Array.isArray(items) ? items : [];
  $('#incidents').innerHTML = items.length ? items.map(i => `<article class="incident"><p><strong>${escapeHTML(i.Name)}</strong> · <span class="state ${i.LatestState}">${state(i.LatestState)}</span></p><p class="muted">Started ${date(i.OpenedAt)}${i.ResolvedAt ? ` · Recovered ${date(i.ResolvedAt)}` : ' · Open'}</p></article>`).join('') : '<p class="muted">No incidents recorded.</p>';
}
function escapeHTML(text) { const e = document.createElement('span'); e.textContent = text || ''; return e.innerHTML; }
async function refresh() { const [overview, incidents, ntfy] = await Promise.all([api('/api/overview'), api('/api/incidents'), api('/api/notifications/ntfy')]); const monitors = Array.isArray(overview.monitors) ? overview.monitors : []; const checks = await Promise.all(monitors.map(m => api(`/api/monitors/${m.id}/checks`).catch(() => []))); renderOverview({...overview, monitors}, Object.fromEntries(monitors.map((m, i) => [m.id, checks[i]]))); renderIncidents(incidents); renderNtfy(ntfy); }

$('#refresh').addEventListener('click', () => refresh().catch(showError));
$('#logout').addEventListener('click', async () => { await fetch('/logout',{method:'POST'}); location.assign('/login.html'); });
$('#add-monitor').addEventListener('click', () => $('#monitor-dialog').showModal());
$('#close-dialog').addEventListener('click', () => $('#monitor-dialog').close());
$('#provider').addEventListener('change', e => { $('#name').value = e.target.value === 'torbox' ? 'TorBox' : 'Premiumize'; });
$('#monitor-form').addEventListener('submit', async event => {
  event.preventDefault(); $('#monitor-error').textContent = '';
  const payload = {provider:$('#provider').value,name:$('#name').value,apiKey:$('#api-key').value,intervalSeconds:+$('#interval').value,timeoutSeconds:+$('#timeout').value,failureThreshold:+$('#failure').value,recoveryThreshold:+$('#recovery').value,publicCheck:$('#public-check').checked};
  try { await api('/api/monitors',{method:'POST',body:JSON.stringify(payload)}); $('#monitor-dialog').close(); event.target.reset(); await refresh(); } catch (error) { $('#monitor-error').textContent = error.message; }
});
$('#ntfy-form').addEventListener('submit', async event => { event.preventDefault(); const url=$('#ntfy-url').value.trim(); if (!url && !ntfyConfigured) { notificationMessage('Enter the complete ntfy topic URL first.', 'error'); return; } try { notificationMessage('Saving…'); const result = await api('/api/notifications/ntfy',{method:'PUT',body:JSON.stringify({url,enabled:$('#ntfy-enabled').checked})}); $('#ntfy-url').value=''; renderNtfy(result); notificationMessage(result.enabled ? 'Settings saved. Incident notifications are enabled.' : 'Settings saved. The channel is configured but disabled.', 'success'); } catch(error) { notificationMessage(error.message, 'error'); } });
$('#ntfy-test').addEventListener('click', async () => { const button = $('#ntfy-test'); button.disabled = true; notificationMessage('Sending test notification…'); try { await api('/api/notifications/ntfy/test', {method:'POST'}); notificationMessage('Test notification delivered successfully.', 'success'); } catch(error) { notificationMessage(`Test failed: ${error.message}`, 'error'); } finally { button.disabled = !ntfyConfigured; } });
function showError(error) { console.error(error); $('#updated').textContent = `Unable to refresh dashboard: ${error.message || 'unknown error'}`; }
refresh().catch(showError);
setInterval(() => refresh().catch(showError), 30_000);
