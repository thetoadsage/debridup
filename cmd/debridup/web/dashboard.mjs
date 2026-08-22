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
const PROVIDER_STATES = new Set(['healthy', 'degraded', 'auth_failed', 'api_issue', 'connection_issue', 'checking', 'unknown']);
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
    const transient = Boolean(incident.transient);
    const resolution = transient ? 'Possible degradation' : Number.isFinite(incident.resolvedAt) ? 'Resolved' : 'Ongoing';
    const resolutionClass = transient ? 'degraded' : resolution === 'Resolved' ? 'resolved' : 'open';
    const timing = transient
      ? `Detected ${escapeHTML(incident.openedAtLabel)} · Notification not sent; failure threshold not reached.`
      : `Started ${escapeHTML(incident.openedAtLabel)}${resolution === 'Resolved' ? ` · Recovered ${escapeHTML(incident.resolvedAtLabel)}` : ''}`;
    return `<article class="incident"><div class="incident-heading"><strong>${escapeHTML(incident.name || 'Provider incident')}</strong><span class="state ${state}">${escapeHTML(incident.stateLabel)}</span><span class="incident-resolution ${resolutionClass}">${resolution}</span></div><p class="incident-summary">${escapeHTML(incident.summary || 'Provider state changed.')}</p><p class="incident-meta">${timing}</p></article>`;
  }).join('');
}

function renderAvailabilityComparison(element, providers) {
  if (!element) return;
  if (!providers.length) {
    element.innerHTML = '<p class="muted">Availability appears after authenticated checks run.</p>';
    return;
  }
  element.innerHTML = providers.map(provider => {
    const width = Number.isFinite(provider.availability)
      ? Math.max(0, Math.min(100, provider.availability))
      : 0;
    return `<div class="bar-row"><span>${escapeHTML(provider.name || 'Unnamed provider')}</span><div class="bar"><i data-width="${width}"></i></div><strong>${escapeHTML(provider.availabilityLabel)}</strong></div>`;
  }).join('');
  // A strict style-src CSP blocks style="width:...%" as an inline style, so
  // the width is set through the CSSOM instead, which is unaffected.
  for (const bar of element.querySelectorAll('.bar > i[data-width]')) {
    bar.style.setProperty('width', `${bar.dataset.width}%`);
  }
}

function setBusy(elements, value) {
  for (const element of elements) element?.setAttribute('aria-busy', String(value));
}

function setLatencyAccessibility(element, label) {
  if (!element) return;
  element.setAttribute('role', 'group');
  element.setAttribute('aria-label', label);
}

// A full innerHTML swap destroys whatever had focus. Describe the focused
// element by its stable data attributes so it can be found again afterwards.
function focusSelector(element) {
  if (!element || !element.closest) return null;
  const bucket = element.closest('.pulse-bucket[data-provider-id][data-bucket-index]');
  if (bucket) {
    return `.pulse-bucket[data-provider-id="${bucket.dataset.providerId}"][data-bucket-index="${bucket.dataset.bucketIndex}"]`;
  }
  const providerTrigger = element.closest('.provider-detail-trigger[data-provider-id]');
  if (providerTrigger) {
    return `.provider-detail-trigger[data-provider-id="${providerTrigger.dataset.providerId}"]`;
  }
  return null;
}

// The rendered regions are driven entirely by the payload and the display time
// zone, so an unchanged pair means the DOM would be rewritten identically.
function renderSignature(payload, timeZone) {
  if (!payload) return null;
  try {
    const {generatedAt, ...rest} = payload;
    return `${timeZone}|${JSON.stringify(rest)}`;
  } catch {
    return null;
  }
}

export function startDashboard({api, document, window, timeZone, onRefresh} = {}) {
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
  let renderedSignature = null;
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

  function renderModel(nextModel, {signature = null} = {}) {
    model = nextModel;
    // Values the drawer shows include elapsed durations, so refresh it even
    // when the payload itself is unchanged.
    drawer?.refresh(model.providers);
    if (signature !== null && signature === renderedSignature) return;

    const active = document.activeElement;
    const restoreTo = focusSelector(active);
    const restoreBucketRow = restoreTo?.startsWith('.pulse-bucket') ? active?.closest?.('.pulse-track') : null;
    const restoreScroll = restoreBucketRow?.scrollLeft ?? 0;

    renderSummary(summary, model);
    if (pulse) {
      pulse.innerHTML = renderPulse(model.providers, displayTimeZone);
      const bucketCount = model.providers[0]?.series?.length;
      pulse.style.setProperty('--pulse-bucket-count', Number.isFinite(bucketCount) && bucketCount > 0 ? String(bucketCount) : '1');
    }
    renderProviderTable(providerTable, model.providers);
    if (latency) {
      latency.innerHTML = renderLatencyChart(model.providers, displayTimeZone);
      setLatencyAccessibility(latency, 'Provider latency comparison and text summary');
    }
    renderIncidents(incidents, model.incidents);
    renderAvailabilityComparison(comparison, model.providers);
    setBusy(busyElements, false);
    renderedSignature = signature;

    if (restoreTo) {
      const replacement = document.querySelector(restoreTo);
      if (replacement) {
        if (replacement.closest?.('.pulse-track')) setRovingTarget(replacement);
        replacement.focus?.();
        const track = replacement.closest?.('.pulse-track');
        if (track && restoreScroll) track.scrollLeft = restoreScroll;
      }
    }
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
      renderModel(createDashboardModel(lastPayload, now(), displayTimeZone), {
        signature: renderSignature(lastPayload, displayTimeZone),
      });
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
      renderedSignature = null;
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
        renderModel(createDashboardModel(lastPayload, now(), displayTimeZone), {
          signature: renderSignature(lastPayload, displayTimeZone),
        });
        renderSuccessStatus();
        try {
          onRefresh?.(model);
        } catch {
          // A dependent panel must never break the dashboard refresh loop.
        }
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

  // Roving tabindex bookkeeping: exactly one bucket per row stays tabbable.
  function setRovingTarget(bucket) {
    const track = bucket?.closest?.('.pulse-track');
    if (!track) return;
    for (const sibling of track.querySelectorAll('.pulse-bucket')) {
      sibling.setAttribute('tabindex', sibling === bucket ? '0' : '-1');
    }
  }

  function pulseKeydown(event) {
    const current = event.target?.closest?.('.pulse-bucket');
    if (!current) return;
    const track = current.closest('.pulse-track');
    if (!track) return;
    const buckets = Array.from(track.querySelectorAll('.pulse-bucket'));
    const index = buckets.indexOf(current);
    if (index === -1) return;
    let next = null;
    switch (event.key) {
      case 'ArrowRight': next = buckets[Math.min(index + 1, buckets.length - 1)]; break;
      case 'ArrowLeft': next = buckets[Math.max(index - 1, 0)]; break;
      case 'Home': next = buckets[0]; break;
      case 'End': next = buckets[buckets.length - 1]; break;
      default: return;
    }
    if (!next || next === current) {
      event.preventDefault();
      return;
    }
    event.preventDefault();
    setRovingTarget(next);
    next.focus();
  }

  function pulseFocusIn(event) {
    const bucket = event.target?.closest?.('.pulse-bucket');
    if (bucket) setRovingTarget(bucket);
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
    renderModel(createDashboardModel(lastPayload, now(), displayTimeZone), {
      signature: renderSignature(lastPayload, displayTimeZone),
    });
    renderSuccessStatus();
  }

  updateRangeButtons();
  for (const button of rangeButtons) button.addEventListener('click', selectRange);
  refreshButton?.addEventListener('click', manualRefresh);
  status?.addEventListener('click', retryRefresh);
  pulse?.addEventListener('click', openProvider);
  pulse?.addEventListener('keydown', pulseKeydown);
  pulse?.addEventListener('focusin', pulseFocusIn);
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
    pulse?.removeEventListener('keydown', pulseKeydown);
    pulse?.removeEventListener('focusin', pulseFocusIn);
    providerTable?.removeEventListener('click', openProvider);
    document.removeEventListener('visibilitychange', visibilityChanged);
    drawer?.destroy();
  }

  return {ready, refresh, setTimeZone, stop};
}
