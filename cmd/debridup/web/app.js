import {startDashboard} from './dashboard.mjs';
import {escapeHTML, formatState, formatTimestamp} from './dashboard-model.mjs';

const $ = selector => document.querySelector(selector);
const SAFE_STATES = new Set(['healthy', 'auth_failed', 'api_issue', 'connection_issue', 'checking', 'unknown']);
let ntfyConfigured = false;
let editingMonitorID = null;
let monitorConfigs = new Map();
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

function stateClass(value) {
  return SAFE_STATES.has(value) ? value : 'unknown';
}

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
  const response = await fetch(path, {
    ...options,
    headers: {'Content-Type': 'application/json', ...(options.headers || {})},
  });
  if (response.status === 401) {
    window.location.assign('/login.html');
    throw new Error('unauthorized');
  }
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error || 'Request failed');
  return body;
}

function renderMonitorSettings(monitors) {
  const items = Array.isArray(monitors) ? monitors : [];
  monitorConfigs = new Map(items.map(monitor => [monitor.id, monitor]));
  $('#cards').innerHTML = items.length ? items.map(monitor => {
    const state = stateClass(monitor.state);
    const providerName = providerDetails[monitor.provider]?.name || monitor.provider;
    return `<article class="card"><div class="section-title"><div><p class="provider-name">${escapeHTML(monitor.name)}</p><span class="provider">${escapeHTML(providerName)}</span></div><div class="card-status"><span class="state ${state}">${escapeHTML(formatState(monitor.state))}</span><button type="button" class="quiet edit-monitor" data-monitor-id="${Number(monitor.id) || 0}">Edit settings</button></div></div><div class="metric-grid"><div class="metric"><strong>${Number(monitor.intervalSeconds) || 0}s</strong><span>Check interval</span></div><div class="metric"><strong>${Number(monitor.timeoutSeconds) || 0}s</strong><span>Timeout</span></div><div class="metric"><strong>${Number(monitor.failureThreshold) || 0}</strong><span>Failure confirmations</span></div><div class="metric"><strong>${escapeHTML(formatTimestamp(monitor.lastCheck))}</strong><span>Last check</span></div></div></article>`;
  }).join('') : '<article class="card"><p>No providers configured. Add a provider to begin monitoring.</p></article>';
  document.querySelectorAll('.edit-monitor').forEach(button => button.addEventListener('click', () => openEditMonitor(monitorConfigs.get(Number(button.dataset.monitorId)))));
}

async function loadMonitorSettings() {
  renderMonitorSettings(await api('/api/monitors'));
}

async function loadNotificationSettings() {
  renderNtfy(await api('/api/notifications/ntfy'));
}

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

function showManagementError(error) {
  console.error(error);
  $('#cards').innerHTML = '<article class="card"><p class="error">Unable to load provider settings. Refresh the page to retry.</p></article>';
}

const dashboard = startDashboard({api, document, window});

$('#logout').addEventListener('click', async () => {
  await fetch('/logout', {method: 'POST'});
  window.location.assign('/login.html');
});
$('#add-monitor').addEventListener('click', openCreateMonitor);
$('#close-dialog').addEventListener('click', () => {
  $('#monitor-dialog').close();
  resetMonitorDialog();
});
$('#provider').addEventListener('change', event => {
  const details = providerDetails[event.target.value];
  $('#name').value = details?.name || event.target.value;
  setCredentialCopy(event.target.value, false);
});
$('#monitor-form').addEventListener('submit', async event => {
  event.preventDefault();
  $('#monitor-error').textContent = '';
  const payload = {
    name: $('#name').value,
    apiKey: $('#api-key').value,
    intervalSeconds: +$('#interval').value,
    timeoutSeconds: +$('#timeout').value,
    failureThreshold: +$('#failure').value,
    recoveryThreshold: +$('#recovery').value,
    publicCheck: $('#public-check').checked,
  };
  if (editingMonitorID) payload.enabled = $('#monitor-enabled').checked;
  else payload.provider = $('#provider').value;
  try {
    await api(editingMonitorID ? `/api/monitors/${editingMonitorID}` : '/api/monitors', {
      method: editingMonitorID ? 'PUT' : 'POST',
      body: JSON.stringify(payload),
    });
    $('#monitor-dialog').close();
    resetMonitorDialog();
    await Promise.all([loadMonitorSettings(), dashboard.refresh({supersede: true})]);
  } catch (error) {
    $('#monitor-error').textContent = error.message;
  }
});
$('#delete-monitor').addEventListener('click', async () => {
  if (!editingMonitorID) return;
  const config = monitorConfigs.get(editingMonitorID);
  if (!window.confirm(`Delete ${config?.name || 'this provider'}? This permanently removes its checks and incident history.`)) return;
  try {
    await api(`/api/monitors/${editingMonitorID}`, {method: 'DELETE'});
    $('#monitor-dialog').close();
    resetMonitorDialog();
    await Promise.all([loadMonitorSettings(), dashboard.refresh({supersede: true})]);
  } catch (error) {
    $('#monitor-error').textContent = error.message;
  }
});
$('#reset-monitor-stats').addEventListener('click', async () => {
  if (!editingMonitorID) return;
  const config = monitorConfigs.get(editingMonitorID);
  if (!window.confirm(`Reset all stats for ${config?.name || 'this provider'}? Its checks and incident history will be permanently cleared. Provider settings and credentials will be kept.`)) return;
  try {
    await api(`/api/monitors/${editingMonitorID}/reset`, {method: 'POST'});
    $('#monitor-dialog').close();
    resetMonitorDialog();
    await dashboard.refresh({supersede: true});
  } catch (error) {
    $('#monitor-error').textContent = error.message;
  }
});
$('#reset-all-stats').addEventListener('click', async () => {
  if (!window.confirm('Reset stats for every provider? All checks and incident history will be permanently cleared. Provider settings, credentials, and notification settings will be kept.')) return;
  const button = $('#reset-all-stats');
  button.disabled = true;
  try {
    await api('/api/stats/reset', {method: 'POST'});
    await dashboard.refresh({supersede: true});
  } catch (error) {
    showManagementError(error);
  } finally {
    button.disabled = false;
  }
});
$('#ntfy-form').addEventListener('submit', async event => {
  event.preventDefault();
  const url = $('#ntfy-url').value.trim();
  if (!url && !ntfyConfigured) {
    notificationMessage('Enter the complete ntfy topic URL first.', 'error');
    return;
  }
  try {
    notificationMessage('Saving…');
    const result = await api('/api/notifications/ntfy', {
      method: 'PUT',
      body: JSON.stringify({url, enabled: $('#ntfy-enabled').checked}),
    });
    $('#ntfy-url').value = '';
    renderNtfy(result);
    notificationMessage(result.enabled ? 'Settings saved. Incident notifications are enabled.' : 'Settings saved. The channel is configured but disabled.', 'success');
  } catch (error) {
    notificationMessage(error.message, 'error');
  }
});
$('#ntfy-test').addEventListener('click', async () => {
  const button = $('#ntfy-test');
  button.disabled = true;
  notificationMessage('Sending test notification…');
  try {
    await api('/api/notifications/ntfy/test', {method: 'POST'});
    notificationMessage('Test notification delivered successfully.', 'success');
  } catch (error) {
    notificationMessage(`Test failed: ${error.message}`, 'error');
  } finally {
    button.disabled = !ntfyConfigured;
  }
});

void loadMonitorSettings().catch(showManagementError);
void loadNotificationSettings().catch(error => {
  console.error(error);
  notificationMessage('Unable to load notification settings.', 'error');
});
