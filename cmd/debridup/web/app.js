const $ = selector => document.querySelector(selector);
const fmt = value => value == null ? '—' : `${value.toFixed(2)}%`;
const date = value => value ? new Date(value * 1000).toLocaleString() : 'No checks yet';
const state = value => (value || 'checking').replaceAll('_', ' ');

async function api(path, options = {}) {
  const response = await fetch(path, {...options, headers:{'Content-Type':'application/json', ...(options.headers || {})}});
  if (response.status === 401) { location.assign('/login.html'); throw new Error('unauthorized'); }
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error || 'Request failed');
  return body;
}

function chart(checks) {
  const points = checks.filter(c => c.Source === 'authenticated').slice(0, 120).reverse();
  if (points.length < 2) return '<p class="muted graph-empty">Response-time graph appears after two checks.</p>';
  const max = Math.max(1, ...points.map(c => c.DurationMS));
  const line = points.map((c, i) => `${(i / (points.length - 1) * 100).toFixed(2)},${(34 - Math.min(32, c.DurationMS / max * 32)).toFixed(2)}`).join(' ');
  const failures = points.map((c, i) => c.State === 'healthy' ? '' : `<circle cx="${(i / (points.length - 1) * 100).toFixed(2)}" cy="36" r="1.8" class="failure-dot"/>`).join('');
  return `<svg class="latency-chart" viewBox="0 0 100 38" preserveAspectRatio="none" aria-label="Recent response times"><polyline points="${line}"/>${failures}</svg>`;
}
function renderOverview(data, checksByID = {}) {
  $('#updated').textContent = `Updated ${date(data.generatedAt)}`;
  $('#cards').innerHTML = data.monitors.length ? data.monitors.map(m => `<article class="card"><div class="section-title"><div><p class="provider-name">${escapeHTML(m.Name)}</p><span class="provider">${escapeHTML(m.Provider)}</span></div><span class="state ${m.State}">${state(m.State)}</span></div><div class="metric-grid"><div class="metric"><strong>${fmt(m.Availability)}</strong><span>Availability</span></div><div class="metric"><strong>${fmt(m.Coverage)}</strong><span>Coverage</span></div><div class="metric"><strong>${m.P95MS == null ? '—' : `${m.P95MS} ms`}</strong><span>p95 response time</span></div><div class="metric"><strong>${date(m.LastCheck)}</strong><span>Last authenticated check</span></div></div><p class="graph-label">Recent authenticated response times</p>${chart(checksByID[m.ID] || [])}</article>`).join('') : '<article class="panel">No providers configured. Add TorBox or Premiumize to begin monitoring.</article>';
  $('#comparison').innerHTML = data.monitors.length ? data.monitors.map(m => `<div class="bar-row"><span>${escapeHTML(m.Name)}</span><div class="bar"><i style="width:${Math.max(0,Math.min(100,m.Availability || 0))}%"></i></div><strong>${fmt(m.Availability)}</strong></div>`).join('') : '<p class="muted">Availability appears after authenticated checks run.</p>';
}
function renderIncidents(items) {
  $('#incidents').innerHTML = items.length ? items.map(i => `<article class="incident"><p><strong>${escapeHTML(i.Name)}</strong> · <span class="state ${i.LatestState}">${state(i.LatestState)}</span></p><p class="muted">Started ${date(i.OpenedAt)}${i.ResolvedAt ? ` · Recovered ${date(i.ResolvedAt)}` : ' · Open'}</p></article>`).join('') : '<p class="muted">No incidents recorded.</p>';
}
function escapeHTML(text) { const e = document.createElement('span'); e.textContent = text || ''; return e.innerHTML; }
async function refresh() { const [overview, incidents, ntfy] = await Promise.all([api('/api/overview'), api('/api/incidents'), api('/api/notifications/ntfy')]); const checks = await Promise.all(overview.monitors.map(m => api(`/api/monitors/${m.ID}/checks`))); renderOverview(overview, Object.fromEntries(overview.monitors.map((m, i) => [m.ID, checks[i]]))); renderIncidents(incidents); $('#ntfy-enabled').checked = ntfy.enabled; $('#ntfy-status').textContent = ntfy.configured ? 'A topic is configured. Leave the field blank only if you do not want to change it.' : 'No ntfy topic configured.'; }

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
$('#ntfy-form').addEventListener('submit', async event => { event.preventDefault(); const url=$('#ntfy-url').value.trim(); if (!url) { $('#ntfy-status').textContent = 'Enter the complete ntfy topic URL to change this setting.'; return; } try { await api('/api/notifications/ntfy',{method:'PUT',body:JSON.stringify({url,enabled:$('#ntfy-enabled').checked})}); $('#ntfy-url').value=''; $('#ntfy-status').textContent='Notification settings saved.'; } catch(error) { $('#ntfy-status').textContent=error.message; } });
function showError(error) { console.error(error); $('#updated').textContent = 'Unable to refresh dashboard.'; }
refresh().catch(showError);
