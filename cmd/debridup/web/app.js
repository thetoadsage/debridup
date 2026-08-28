import {startDashboard} from './dashboard.mjs';
import {escapeHTML, formatState} from './dashboard-model.mjs';
import {setupThemePicker} from './theme.mjs';
import {formatTimestamp, setupTimeZonePicker} from './timezone.mjs';
import {setupSectionNavigation} from './navigation.mjs';
import {startServiceHistory} from './history-chart.mjs';

const $ = selector => document.querySelector(selector);
const SAFE_STATES = new Set(['healthy', 'slow', 'degraded', 'outage', 'auth_failed', 'api_issue', 'connection_issue', 'checking', 'unknown', 'paused']);
const stateClass = value => SAFE_STATES.has(value) ? value : 'unknown';
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

setupThemePicker({document, storage: window.localStorage});
setupSectionNavigation({document, window});
let dashboard;
let serviceHistory;
const timeZonePicker = setupTimeZonePicker({
  document,
  storage: window.localStorage,
  onChange: timeZone => {
    renderMonitorSettings(Array.from(monitorConfigs.values()));
    dashboard?.setTimeZone(timeZone);
    serviceHistory?.setTimeZone(timeZone);
  },
});

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

let renderedCardSignature = null;

function renderMonitorSettings(monitors) {
  const items = Array.isArray(monitors) ? monitors : [];
  monitorConfigs = new Map(items.map(monitor => [monitor.id, monitor]));

  // Skip the rewrite when nothing visible changed, and restore focus when it
  // does, so the refresh cycle cannot silently drop a focused Edit button.
  const signature = `${timeZonePicker.timeZone}|${JSON.stringify(items)}`;
  if (signature === renderedCardSignature) return;
  const focusedButton = document.activeElement?.closest?.('.edit-monitor, .delete-monitor');
  const focusedID = focusedButton?.dataset?.monitorId ?? null;
  const focusedAction = focusedButton?.classList?.contains('delete-monitor') ? 'delete' : 'edit';
  const enabledCount = items.filter(monitor => monitor.enabled).length;
  $('#provider-settings-summary').textContent = `${items.length} configured · ${enabledCount} enabled`;

  $('#provider-settings-list').innerHTML = items.length ? items.map(monitor => {
    const providerName = providerDetails[monitor.provider]?.name || monitor.provider;
    const monitorID = Number(monitor.id) || 0;
    const enabled = Boolean(monitor.enabled);
    const state = stateClass(monitor.state);
    return `<article class="provider-config-row"><div class="provider-config-identity"><p class="provider-name">${escapeHTML(monitor.name)}</p><span class="provider">${escapeHTML(providerName)}</span></div><div class="provider-config-details"><span class="config-detail ${enabled ? 'enabled' : 'paused'}">${enabled ? 'Enabled' : 'Paused'}</span><span class="config-detail config-state ${state}">${escapeHTML(formatState(monitor.state))}</span><span class="config-detail ${monitor.configured ? 'enabled' : 'paused'}">${monitor.configured ? 'Credential stored' : 'Credential required'}</span><span class="config-detail">Every ${Number(monitor.intervalSeconds) || 0}s</span><span class="config-detail">${Number(monitor.timeoutSeconds) || 0}s timeout</span><span class="config-detail">${Number(monitor.failureThreshold) || 0} failure${Number(monitor.failureThreshold) === 1 ? '' : 's'} / ${Number(monitor.recoveryThreshold) || 0} recovery</span><span class="config-detail">Last check: ${escapeHTML(formatTimestamp(monitor.lastCheck, timeZonePicker.timeZone))}</span>${monitor.publicCheck ? '<span class="config-detail">Public check on</span>' : ''}</div><div class="provider-config-actions"><button type="button" class="quiet edit-monitor" data-monitor-id="${monitorID}">Edit</button><button type="button" class="quiet danger-quiet delete-monitor" data-monitor-id="${monitorID}">Delete</button></div></article>`;
  }).join('') : '<article class="provider-empty-state"><div><strong>No providers configured</strong><p class="muted">Add a provider to start checking service health.</p></div><button type="button" id="add-first-monitor">Add your first provider</button></article>';
  renderedCardSignature = signature;
  if (focusedID !== null) {
    $(`.${focusedAction}-monitor[data-monitor-id="${focusedID}"]`)?.focus?.();
  }
}

// Delegated once, rather than rebinding a listener per card on every render.
$('#provider-settings-list').addEventListener('click', event => {
  if (event.target?.closest?.('#add-first-monitor')) {
    openCreateMonitor();
    return;
  }
  const button = event.target?.closest?.('.edit-monitor');
  if (button) {
    openEditMonitor(monitorConfigs.get(Number(button.dataset.monitorId)));
    return;
  }
  const deleteButton = event.target?.closest?.('.delete-monitor');
  if (deleteButton) void deleteMonitor(monitorConfigs.get(Number(deleteButton.dataset.monitorId)));
});

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
  for (const selector of ['#monitor-submit', '#delete-monitor', '#reset-monitor-stats', '#close-dialog']) {
    $(selector).disabled = false;
  }
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

function setMonitorActionBusy(busy, label = '') {
  for (const selector of ['#monitor-submit', '#delete-monitor', '#reset-monitor-stats', '#close-dialog']) {
    $(selector).disabled = busy;
  }
  if (label) $('#monitor-submit').textContent = label;
}

function showManagementError(error) {
  console.error(error);
  renderedCardSignature = null;
  $('#provider-settings-summary').textContent = 'Provider settings unavailable';
  $('#provider-settings-list').innerHTML = '<article class="provider-empty-state"><p class="error">Unable to load provider settings. Refresh the page to retry.</p></article>';
}

dashboard = startDashboard({
  api,
  document,
  window,
  timeZone: timeZonePicker.timeZone,
  onRefresh: () => {
    if ($('#monitor-dialog').open) return;
    void loadMonitorSettings().catch(showManagementError);
    void serviceHistory?.refresh();
  },
});
serviceHistory = startServiceHistory({api, document, timeZone: timeZonePicker.timeZone});

$('#report-range').addEventListener('change', event => {
  $('#download-report').href = `/api/report?range=${encodeURIComponent(event.target.value)}`;
});

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
  const submitLabel = editingMonitorID ? 'Save changes' : 'Add provider';
  setMonitorActionBusy(true, editingMonitorID ? 'Saving…' : 'Adding…');
  try {
    await api(editingMonitorID ? `/api/monitors/${editingMonitorID}` : '/api/monitors', {
      method: editingMonitorID ? 'PUT' : 'POST',
      body: JSON.stringify(payload),
    });
    $('#monitor-dialog').close();
    resetMonitorDialog();
    await Promise.all([loadMonitorSettings(), dashboard.refresh({supersede: true}), serviceHistory.refresh()]);
  } catch (error) {
    $('#monitor-error').textContent = error.message;
  } finally {
    if ($('#monitor-dialog').open) {
      setMonitorActionBusy(false);
      $('#monitor-submit').textContent = submitLabel;
    }
  }
});
async function deleteMonitor(config) {
  const monitorID = config?.id || editingMonitorID;
  if (!monitorID) return;
  if (!window.confirm(`Delete ${config?.name || 'this provider'}? This permanently removes its checks and incident history.`)) return;
  const rowActions = Array.from(document.querySelectorAll(`[data-monitor-id="${Number(monitorID)}"]`));
  for (const action of rowActions) action.disabled = true;
  setMonitorActionBusy(true, 'Deleting…');
  try {
    await api(`/api/monitors/${monitorID}`, {method: 'DELETE'});
    $('#monitor-dialog').close();
    resetMonitorDialog();
    await Promise.all([loadMonitorSettings(), dashboard.refresh({supersede: true}), serviceHistory.refresh()]);
  } catch (error) {
    if ($('#monitor-dialog').open) $('#monitor-error').textContent = error.message;
    else $('#provider-settings-summary').textContent = `Delete failed: ${error.message}`;
  } finally {
    for (const action of rowActions) action.disabled = false;
    if ($('#monitor-dialog').open) {
      setMonitorActionBusy(false);
      $('#monitor-submit').textContent = 'Save changes';
    }
  }
}
$('#delete-monitor').addEventListener('click', () => {
  void deleteMonitor(monitorConfigs.get(editingMonitorID));
});
$('#reset-monitor-stats').addEventListener('click', async () => {
  if (!editingMonitorID) return;
  const config = monitorConfigs.get(editingMonitorID);
  if (!window.confirm(`Reset all stats for ${config?.name || 'this provider'}? Its checks and incident history will be permanently cleared. Provider settings and credentials will be kept.`)) return;
  setMonitorActionBusy(true, 'Resetting…');
  try {
    await api(`/api/monitors/${editingMonitorID}/reset`, {method: 'POST'});
    $('#monitor-dialog').close();
    resetMonitorDialog();
    await Promise.all([loadMonitorSettings(), dashboard.refresh({supersede: true}), serviceHistory.refresh()]);
  } catch (error) {
    $('#monitor-error').textContent = error.message;
  } finally {
    if ($('#monitor-dialog').open) {
      setMonitorActionBusy(false);
      $('#monitor-submit').textContent = 'Save changes';
    }
  }
});
$('#reset-all-stats').addEventListener('click', async () => {
  if (!window.confirm('Reset stats for every provider? All checks and incident history will be permanently cleared. Provider settings, credentials, and notification settings will be kept.')) return;
  const button = $('#reset-all-stats');
  button.disabled = true;
  try {
    await api('/api/stats/reset', {method: 'POST'});
    await Promise.all([loadMonitorSettings(), dashboard.refresh({supersede: true}), serviceHistory.refresh()]);
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
