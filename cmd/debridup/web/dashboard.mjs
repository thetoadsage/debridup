import {
  createDashboardModel,
  escapeHTML,
  formatState,
} from './dashboard-model.mjs';
import {renderLatencyChart, renderPulse} from './chart.mjs';
import {createProviderDrawer} from './drawer.mjs';

const DEFAULT_RANGE = '24h';
const REFRESH_INTERVAL_MS = 30_000;
const RANGE_VALUES = new Set(['24h', '7d', '30d']);
const PROVIDER_STATES = new Set(['healthy', 'auth_failed', 'api_issue', 'connection_issue', 'checking', 'unknown']);
const SUMMARY_STATES = new Set(['healthy', 'degraded', 'outage', 'unknown']);
const PULSE_STATES = new Set(['healthy', 'degraded', 'outage', 'unknown']);
const STATE_SYMBOLS = Object.freeze({healthy: '✓', degraded: '!', outage: '×', unknown: '?'});

function safeState(value, allowed = PROVIDER_STATES) {
  return allowed.has(value) ? value : 'unknown';
}

function isAbortError(error) {
  return error?.name === 'AbortError';
}

function renderSummary(element, model) {
  if (!element) return;
  const state = safeState(model.summary.overallState, SUMMARY_STATES);
  element.innerHTML = `<h2 id="summary-heading" class="sr-only">Current status summary</h2><article class="summary-card"><span class="summary-label">Overall status</span><strong class="summary-value summary-state ${state}">${escapeHTML(model.summary.overallStateLabel)}</strong></article><article class="summary-card"><span class="summary-label">Providers online</span><strong class="summary-value">${Number(model.summary.providersOnline) || 0}</strong></article><article class="summary-card"><span class="summary-label">Active incidents</span><strong class="summary-value">${Number(model.summary.activeIncidents) || 0}</strong></article><article class="summary-card"><span class="summary-label">Checks completed today</span><strong class="summary-value">${Number(model.summary.checksToday) || 0}</strong></article>`;
}

function renderProviderTable(element, providers) {
  if (!element) return;
  if (!providers.length) {
    element.innerHTML = '<tr><td colspan="7"><div class="dashboard-empty"><p>No providers configured.</p><a href="#provider-management">Add a provider to begin monitoring.</a></div></td></tr>';
    return;
  }
  element.innerHTML = providers.map(provider => {
    const state = safeState(provider.state);
    const quality = state === 'unknown' ? '<span class="quality-note">Data unavailable</span>' : '';
    const strip = provider.series.slice(-12).map(point => {
      const pointState = safeState(point?.state, PULSE_STATES);
      return `<span class="status-segment ${pointState}"><span class="status-symbol" aria-hidden="true">${STATE_SYMBOLS[pointState]}</span><span class="sr-only">${escapeHTML(formatState(pointState))}</span></span>`;
    }).join('') || '<span class="muted">No range data</span>';
    return `<tr><th scope="row"><button class="provider-detail-trigger" type="button" data-provider-id="${Number(provider.id) || 0}">${escapeHTML(provider.name || 'Unnamed provider')}</button>${quality}</th><td><span class="state ${state}">${escapeHTML(provider.stateLabel)}</span></td><td>${escapeHTML(provider.availabilityLabel)}</td><td>${escapeHTML(provider.p50Label)}</td><td>${escapeHTML(provider.p95Label)}</td><td>${escapeHTML(provider.lastCheckLabel)}</td><td><span class="status-strip">${strip}</span></td></tr>`;
  }).join('');
}

function renderIncidents(element, incidents) {
  if (!element) return;
  if (!incidents.length) {
    element.innerHTML = '<div class="dashboard-empty"><p>No incidents in this range.</p></div>';
    return;
  }
  element.innerHTML = incidents.map(incident => {
    const state = safeState(incident.latestState);
    const resolution = Number.isFinite(incident.resolvedAt) ? 'Resolved' : 'Ongoing';
    return `<article class="incident"><div class="incident-heading"><strong>${escapeHTML(incident.name || 'Provider incident')}</strong><span class="state ${state}">${escapeHTML(incident.stateLabel)}</span><span class="incident-resolution ${resolution === 'Resolved' ? 'resolved' : 'open'}">${resolution}</span></div><p class="incident-summary">${escapeHTML(incident.summary || 'Provider state changed.')}</p><p class="incident-meta">Started ${escapeHTML(incident.openedAtLabel)}${resolution === 'Resolved' ? ` · Recovered ${escapeHTML(incident.resolvedAtLabel)}` : ''}</p></article>`;
  }).join('');
}

function renderAvailabilityComparison(element, providers) {
  if (!element) return;
  element.innerHTML = providers.length
    ? providers.map(provider => {
      const width = Number.isFinite(provider.availability)
        ? Math.max(0, Math.min(100, provider.availability))
        : 0;
      return `<div class="bar-row"><span>${escapeHTML(provider.name || 'Unnamed provider')}</span><div class="bar"><i style="width:${width}%"></i></div><strong>${escapeHTML(provider.availabilityLabel)}</strong></div>`;
    }).join('')
    : '<p class="muted">Availability appears after authenticated checks run.</p>';
}

function setBusy(elements, value) {
  for (const element of elements) element?.setAttribute('aria-busy', String(value));
}

function setLatencyAccessibility(element, label) {
  if (!element) return;
  element.setAttribute('role', 'group');
  element.setAttribute('aria-label', label);
}

export function startDashboard({api, document, window, timeZone} = {}) {
  if (typeof api !== 'function') throw new Error('dashboard api function is required');
  if (!document || !window) throw new Error('dashboard document and window are required');

  const status = document.getElementById('dashboard-status');
  const summary = document.getElementById('summary');
  const pulse = document.getElementById('provider-pulse');
  const providerTable = document.getElementById('provider-table-body');
  const latency = document.getElementById('latency-chart');
  const incidents = document.getElementById('incidents');
  const comparison = document.getElementById('comparison');
  const refreshButton = document.getElementById('refresh');
  const rangeButtons = Array.from(document.querySelectorAll('#range-controls [data-range]'));
  const busyElements = [summary, pulse, providerTable, latency, incidents];
  const drawerRoot = document.getElementById('provider-drawer');
  const drawer = drawerRoot ? createProviderDrawer(drawerRoot) : null;
  let range = DEFAULT_RANGE;
  let activeRefresh = null;
  let timer = null;
  let stopped = false;
  let lastPayload = null;
  let model = null;
  let displayTimeZone = timeZone;

  function now() {
    return window.Date?.now?.() ?? Date.now();
  }

  function clearTimer() {
    if (timer !== null) window.clearTimeout(timer);
    timer = null;
  }

  function scheduleRefresh() {
    clearTimer();
    if (stopped || document.hidden) return;
    timer = window.setTimeout(() => {
      timer = null;
      void refresh();
    }, REFRESH_INTERVAL_MS);
  }

  function updateRangeButtons() {
    for (const button of rangeButtons) {
      const selected = button.dataset.range === range;
      button.setAttribute('aria-pressed', String(selected));
      button.classList.toggle('active', selected);
    }
  }

  function renderModel(nextModel) {
    model = nextModel;
    renderSummary(summary, model);
    if (pulse) pulse.innerHTML = renderPulse(model.providers, displayTimeZone);
    renderProviderTable(providerTable, model.providers);
    if (latency) {
      latency.innerHTML = renderLatencyChart(model.providers, displayTimeZone);
      setLatencyAccessibility(latency, 'Provider latency comparison and text summary');
    }
    renderIncidents(incidents, model.incidents);
    renderAvailabilityComparison(comparison, model.providers);
    setBusy(busyElements, false);
  }

  function renderSuccessStatus() {
    if (!status || !model) return;
    status.innerHTML = model.stale
      ? `Data generated ${escapeHTML(model.ageLabel)} ago. <button class="status-retry" type="button" data-dashboard-retry>Retry now</button>`
      : `Updated ${escapeHTML(model.updatedLabel)}`;
  }

  function setRefreshBusy(value) {
    if (!refreshButton) return;
    refreshButton.disabled = value;
    refreshButton.textContent = value ? 'Refreshing…' : 'Refresh';
  }

  function renderFailureStatus() {
    if (!status) return;
    if (lastPayload) {
      if (RANGE_VALUES.has(lastPayload.range) && lastPayload.range !== range) {
        range = lastPayload.range;
        updateRangeButtons();
      }
      renderModel(createDashboardModel(lastPayload, now(), displayTimeZone));
      status.innerHTML = `Unable to refresh. Showing data from ${escapeHTML(model.ageLabel)} ago. <button class="status-retry" type="button" data-dashboard-retry>Retry</button>`;
    } else {
      if (summary) summary.innerHTML = '<h2 id="summary-heading" class="sr-only">Current status summary</h2><article class="summary-card"><span class="summary-label">Overall status</span><strong class="summary-value summary-state unknown">Unavailable</strong></article><article class="summary-card"><span class="summary-label">Providers online</span><strong class="summary-value">—</strong></article><article class="summary-card"><span class="summary-label">Active incidents</span><strong class="summary-value">—</strong></article><article class="summary-card"><span class="summary-label">Checks completed today</span><strong class="summary-value">—</strong></article>';
      if (pulse) pulse.innerHTML = '<div class="section-title"><div><h2 id="pulse-heading">Provider pulse</h2><p class="muted">Health changes across the selected range.</p></div></div><div class="dashboard-empty"><p>Dashboard data is unavailable.</p></div>';
      if (providerTable) providerTable.innerHTML = '<tr><td colspan="7"><div class="dashboard-empty"><p>Dashboard data is unavailable.</p></div></td></tr>';
      if (latency) {
        latency.innerHTML = '<div class="chart-empty"><strong>Dashboard data is unavailable.</strong><p>Retry to load provider latency.</p></div>';
        setLatencyAccessibility(latency, 'Provider latency unavailable');
      }
      if (incidents) incidents.innerHTML = '<div class="dashboard-empty"><p>Dashboard data is unavailable.</p></div>';
      if (comparison) comparison.innerHTML = '<p class="muted">Dashboard data is unavailable.</p>';
      setBusy(busyElements, false);
      status.innerHTML = 'Dashboard data is unavailable. <button class="status-retry" type="button" data-dashboard-retry>Retry</button>';
    }
  }

  function refresh({supersede = false} = {}) {
    if (stopped) return Promise.resolve(model);
    if (activeRefresh) {
      if (!supersede) return activeRefresh.promise;
      activeRefresh.controller.abort();
    }
    clearTimer();
    const Controller = window.AbortController || AbortController;
    const controller = new Controller();
    const token = Symbol('dashboard-refresh');
    setRefreshBusy(true);
    if (!model) setBusy(busyElements, true);
    let request;
    try {
      request = api(`/api/dashboard?range=${encodeURIComponent(range)}`, {
        signal: controller.signal,
        cache: 'no-store',
      });
    } catch (error) {
      request = Promise.reject(error);
    }
    const promise = Promise.resolve(request)
      .then(payload => {
        if (controller.signal.aborted || activeRefresh?.token !== token) return model;
        lastPayload = payload || {};
        if (RANGE_VALUES.has(lastPayload.range)) {
          range = lastPayload.range;
          updateRangeButtons();
        }
        renderModel(createDashboardModel(lastPayload, now(), displayTimeZone));
        renderSuccessStatus();
        return model;
      })
      .catch(error => {
        if (!isAbortError(error) && activeRefresh?.token === token) renderFailureStatus();
        return model;
      })
      .finally(() => {
        if (activeRefresh?.token !== token) return;
        activeRefresh = null;
        setRefreshBusy(false);
        scheduleRefresh();
      });
    activeRefresh = {controller, promise, token};
    return promise;
  }

  function selectRange(event) {
    const nextRange = event.currentTarget?.dataset?.range || event.target?.dataset?.range;
    if (!RANGE_VALUES.has(nextRange) || nextRange === range) return;
    range = nextRange;
    updateRangeButtons();
    void refresh({supersede: true});
  }

  function manualRefresh() {
    if (refreshButton?.disabled) return;
    if (status) status.textContent = 'Refreshing dashboard…';
    void refresh({supersede: true});
  }

  function retryRefresh(event) {
    if (event.target?.closest?.('[data-dashboard-retry]')) void refresh({supersede: true});
  }

  function openProvider(event) {
    const trigger = event.target?.closest?.('[data-provider-id]');
    if (!trigger || !model || !drawer) return;
    const provider = model.providers.find(item => item.id === Number(trigger.dataset.providerId));
    if (provider) drawer.open(provider, trigger);
  }

  function visibilityChanged() {
    if (document.hidden) {
      clearTimer();
      return;
    }
    void refresh({supersede: true});
  }

  function setTimeZone(value) {
    displayTimeZone = value;
    if (!lastPayload) return;
    renderModel(createDashboardModel(lastPayload, now(), displayTimeZone));
    renderSuccessStatus();
  }

  updateRangeButtons();
  for (const button of rangeButtons) button.addEventListener('click', selectRange);
  refreshButton?.addEventListener('click', manualRefresh);
  status?.addEventListener('click', retryRefresh);
  pulse?.addEventListener('click', openProvider);
  providerTable?.addEventListener('click', openProvider);
  document.addEventListener('visibilitychange', visibilityChanged);
  const ready = refresh();

  function stop() {
    stopped = true;
    clearTimer();
    activeRefresh?.controller.abort();
    activeRefresh = null;
    for (const button of rangeButtons) button.removeEventListener('click', selectRange);
    refreshButton?.removeEventListener('click', manualRefresh);
    status?.removeEventListener('click', retryRefresh);
    pulse?.removeEventListener('click', openProvider);
    providerTable?.removeEventListener('click', openProvider);
    document.removeEventListener('visibilitychange', visibilityChanged);
    drawer?.destroy();
  }

  return {ready, refresh, setTimeZone, stop};
}
