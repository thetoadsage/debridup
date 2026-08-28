import {escapeHTML, formatLatency} from './dashboard-model.mjs';
import {formatTimestamp} from './timezone.mjs';

const RANGE_LABELS = Object.freeze({"24h": '24 hours', "7d": '7 days', "30d": '30 days'});
const KNOWN_STATES = new Set(['healthy', 'slow', 'degraded', 'outage', 'auth_failed', 'api_issue', 'connection_issue', 'checking', 'unknown', 'paused']);
const stateClass = value => KNOWN_STATES.has(value) ? value : 'unknown';
const stateLabel = value => ({healthy: 'Healthy', slow: 'Slow', degraded: 'Degraded', outage: 'Outage', auth_failed: 'Authentication issue', api_issue: 'API issue', connection_issue: 'Connection issue', checking: 'Checking', paused: 'Paused', unknown: 'Unknown'})[value] || 'Unknown';

export function validPoints(series) {
  return (Array.isArray(series) ? series : []).filter(point => Number.isFinite(point?.bucketStart));
}

// Unknown buckets and buckets without latency intentionally become null so the
// SVG path has visible gaps rather than inventing continuity in monitoring data.
export function latencyPath(series, key, width = 720, height = 230, pad = 34, scaleMax = null, yPad = 34, rightPad = pad) {
  const points = validPoints(series);
  const values = points.map(point => point[key]).filter(Number.isFinite);
  if (!points.length || !values.length) return '';
  const max = Number.isFinite(scaleMax) ? Math.max(1, scaleMax) : Math.max(1, ...values);
  const x = index => pad + (index * (width - pad - rightPad) / Math.max(1, points.length - 1));
  const y = value => height - yPad - ((value / max) * (height - yPad * 2));
  let path = '';
  let drawing = false;
  points.forEach((point, index) => {
    const value = point[key];
    if (!Number.isFinite(value)) { drawing = false; return; }
    path += `${drawing ? ' L' : 'M'}${x(index).toFixed(1)},${y(value).toFixed(1)}`;
    drawing = true;
  });
  return path;
}

function chartTicks(series, timeZone, left = 70, right = 686) {
  const points = validPoints(series);
  if (!points.length) return '';
  const slots = [0, Math.floor((points.length - 1) / 2), points.length - 1];
  return [...new Set(slots)].map(index => {
    const x = left + (index * (right - left) / Math.max(1, points.length - 1));
    const label = new Intl.DateTimeFormat(undefined, {timeZone: timeZone === 'browser' ? undefined : timeZone, month: 'short', day: 'numeric', hour: 'numeric'}).format(new Date(points[index].bucketStart * 1000));
    return `<text class="history-axis-label" x="${x.toFixed(1)}" y="258" text-anchor="middle">${escapeHTML(label)}</text>`;
  }).join('');
}

export function chartMarkup(provider, timeZone) {
  const series = validPoints(provider?.series);
  const numeric = series.flatMap(point => [point.p50Ms, point.p95Ms]).filter(Number.isFinite);
  if (!numeric.length) return '<div class="history-empty"><strong>No latency samples in this period</strong><p>Checks without a measured response time are shown as gaps, never estimated lines.</p></div>';
  const max = Math.max(1, ...numeric);
  const p50 = latencyPath(series, 'p50Ms', 720, 230, 70, max, 34, 34);
  const p95 = latencyPath(series, 'p95Ms', 720, 230, 70, max, 34, 34);
  const yLabel = value => `${Math.round(value)} ms`;
  return `<div class="chart-scroll"><svg class="latency-chart" viewBox="0 0 720 278" role="img" aria-labelledby="latency-chart-title latency-chart-description"><title id="latency-chart-title">Latency history for ${escapeHTML(provider?.name || 'selected provider')}</title><desc id="latency-chart-description">Solid line is p50 latency. Dashed line is p95 latency. Missing samples appear as gaps.</desc><g class="history-grid"><line x1="70" y1="34" x2="686" y2="34"></line><line x1="70" y1="115" x2="686" y2="115"></line><line x1="70" y1="196" x2="686" y2="196"></line></g><g class="history-axis"><text class="history-axis-label" x="62" y="38" text-anchor="end">${yLabel(max)}</text><text class="history-axis-label" x="62" y="119" text-anchor="end">${yLabel(max / 2)}</text><text class="history-axis-label" x="62" y="200" text-anchor="end">0 ms</text>${chartTicks(series, timeZone)}</g><path class="latency-line latency-p50" d="${p50}"></path><path class="latency-line latency-p95" d="${p95}"></path></svg></div>`;
}

function providerButton(provider, selected) {
  const state = stateClass(provider?.state);
  const availability = Number.isFinite(provider?.availability) ? `${provider.availability.toFixed(1)}% availability` : 'No availability data';
  return `<button type="button" class="history-provider ${selected ? 'selected' : ''}" data-history-provider="${Number(provider?.id) || 0}" aria-pressed="${selected}"><span class="state ${state}">${escapeHTML(stateLabel(provider?.state))}</span><span class="history-provider-name">${escapeHTML(provider?.name || 'Unnamed provider')}</span><span class="history-provider-detail">${availability}</span></button>`;
}

function timelineState(value) {
  if (value === 'healthy') return 'healthy';
  if (value === 'degraded' || value === 'slow') return 'degraded';
  if (value === 'outage' || value === 'auth_failed' || value === 'api_issue' || value === 'connection_issue') return 'outage';
  return 'unknown';
}

function statusTimeline(series, providerName, timeZone) {
  const points = validPoints(series);
  if (!points.length) return '<div class="history-status-empty">No status buckets in this period.</div>';
  const counts = new Map();
  for (const point of points) {
    const state = timelineState(point.state);
    counts.set(state, (counts.get(state) || 0) + 1);
  }
  const description = [...counts.entries()].map(([state, count]) => `${count} ${stateLabel(state).toLowerCase()}`).join(', ');
  const runs = [];
  for (const point of points) {
    const state = timelineState(point.state);
    const previous = runs[runs.length - 1];
    if (previous?.state === state) previous.points.push(point);
    else runs.push({state, points: [point]});
  }
  let offset = 0;
  const segments = runs.map(run => {
    const start = run.points[0].bucketStart;
    const end = run.points[run.points.length - 1].bucketStart;
    const title = `${stateLabel(run.state)} — ${formatTimestamp(start, timeZone)}${run.points.length > 1 ? ` to ${formatTimestamp(end, timeZone)}` : ''}`;
    const segment = `<rect class="history-status-segment ${run.state}" x="${offset}" y="0" width="${run.points.length}" height="1"><title>${escapeHTML(title)}</title></rect>`;
    offset += run.points.length;
    return segment;
  }).join('');
  const anchors = [points[0], points[Math.floor((points.length - 1) / 2)], points[points.length - 1]];
  const timeAnchors = anchors.map((point, index) => `<time datetime="${escapeHTML(new Date(point.bucketStart * 1000).toISOString())}">${escapeHTML(formatTimestamp(point.bucketStart, timeZone))}</time>`).filter((value, index, values) => values.indexOf(value) === index).join('');
  const legend = ['healthy', 'degraded', 'outage', 'unknown'].map(state => `<span><i class="history-status-swatch ${state}" aria-hidden="true"></i>${stateLabel(state)}</span>`).join('');
  return `<div class="history-status-block"><div class="history-status-heading"><strong>Status timeline</strong><span>${escapeHTML(description)}</span></div><svg class="history-status-track" viewBox="0 0 ${points.length} 1" preserveAspectRatio="none" role="img" aria-label="${escapeHTML(`${providerName} status timeline: ${description}`)}">${segments}</svg><div class="history-status-times">${timeAnchors}</div><div class="history-status-legend" aria-hidden="true">${legend}</div></div>`;
}

export function historyMarkup(data, selectedID, timeZone) {
  const providers = Array.isArray(data?.providers) ? data.providers : [];
  const selected = providers.find(provider => Number(provider.id) === Number(selectedID)) || providers[0];
  if (!selected) return '<div class="history-empty"><strong>No providers configured</strong><p>Add a provider in Settings to begin building service history.</p><a href="#settings">Open provider settings</a></div>';
  const availability = Number.isFinite(selected.availability) ? `${selected.availability.toFixed(1)}%` : '—';
  return `<div class="history-layout"><section class="history-provider-list" aria-label="Providers">${providers.map(provider => providerButton(provider, Number(provider.id) === Number(selected.id))).join('')}</section><div class="history-detail"><div class="history-metrics" aria-label="${escapeHTML(selected.name)} summary"><div><span>Availability</span><strong>${availability}</strong></div><div><span>p50 latency</span><strong>${escapeHTML(formatLatency(selected.p50Ms))}</strong></div><div><span>p95 latency</span><strong>${escapeHTML(formatLatency(selected.p95Ms))}</strong></div><div><span>Slowest</span><strong>${escapeHTML(formatLatency(selected.slowestMs))}</strong></div><div><span>Current state</span><strong>${escapeHTML(stateLabel(selected.state))}</strong></div></div>${statusTimeline(selected.series, selected.name || 'Selected provider', timeZone)}<div class="chart-legend" aria-label="Latency chart legend"><span><i class="legend-line p50" aria-hidden="true"></i>p50 response time</span><span><i class="legend-line p95" aria-hidden="true"></i>p95 response time</span><span>All times shown in ${escapeHTML(timeZone === 'browser' ? 'your browser time' : timeZone)}</span></div>${chartMarkup(selected, timeZone)}<details class="history-text-summary"><summary>Accessible data summary</summary><p>${escapeHTML(selected.name)} is currently ${escapeHTML(stateLabel(selected.state))}. Availability is ${availability}; p50 is ${escapeHTML(formatLatency(selected.p50Ms))}; p95 is ${escapeHTML(formatLatency(selected.p95Ms))}.</p><div class="table-scroll"><table class="provider-table history-summary-table"><caption>Bucketed service history for ${escapeHTML(selected.name)}</caption><thead><tr><th scope="col">Time</th><th scope="col">State</th><th scope="col">Availability</th><th scope="col">p50</th><th scope="col">p95</th></tr></thead><tbody>${validPoints(selected.series).map(point => `<tr><th scope="row">${escapeHTML(formatTimestamp(point.bucketStart, timeZone))}</th><td><span class="state ${stateClass(point.state)}">${escapeHTML(stateLabel(point.state))}</span></td><td>${Number.isFinite(point.availability) ? `${point.availability.toFixed(1)}%` : '—'}</td><td>${escapeHTML(formatLatency(point.p50Ms))}</td><td>${escapeHTML(formatLatency(point.p95Ms))}</td></tr>`).join('')}</tbody></table></div></details></div></div>`;
}

export function startServiceHistory({api, document, timeZone = 'browser'} = {}) {
  const root = document?.getElementById?.('history-content');
  const status = document?.getElementById?.('history-message');
  const rangeControl = document?.getElementById?.('history-range');
  if (!root || !rangeControl) return {refresh() {}, setTimeZone() {}};
  let range = rangeControl.value || '24h';
  let selectedID = null;
  let lastGood = null;
  let lastGoodRange = null;
  let requestVersion = 0;
  let zone = timeZone;
  function render(message = '') {
    if (lastGood) root.innerHTML = historyMarkup(lastGood, selectedID, zone);
    root.setAttribute('aria-busy', 'false');
    if (status) status.textContent = message;
  }
  async function refresh() {
    const version = ++requestVersion;
    rangeControl.disabled = true;
    if (!lastGood) root.innerHTML = '<div class="history-empty"><strong>Loading service history…</strong><p>Preparing bounded availability and latency rollups.</p></div>';
    try {
      const data = await api(`/api/dashboard?range=${encodeURIComponent(range)}`);
      if (version !== requestVersion) return;
      lastGood = data;
      lastGoodRange = range;
      const providers = Array.isArray(data.providers) ? data.providers : [];
      if (!providers.some(provider => Number(provider.id) === Number(selectedID))) selectedID = providers[0]?.id ?? null;
      render(`Showing ${RANGE_LABELS[range] || range} of service history.`);
    } catch (error) {
      if (version !== requestVersion) return;
      if (lastGood) {
        const retainedRange = RANGE_LABELS[lastGoodRange] || lastGoodRange || 'previous range';
        render(`Could not refresh ${RANGE_LABELS[range] || range}; showing the last successful ${retainedRange} graph.`);
      }
      else {
        root.innerHTML = `<div class="history-empty"><strong>Service history is unavailable</strong><p>${escapeHTML(error?.message || 'Try again shortly.')}</p><button type="button" class="quiet" data-history-retry>Retry</button></div>`;
        root.setAttribute('aria-busy', 'false');
      }
    } finally {
      if (version === requestVersion) rangeControl.disabled = false;
    }
  }
  rangeControl.addEventListener('change', event => { range = event.target.value; void refresh(); });
  root.addEventListener('click', event => {
    const retry = event.target?.closest?.('[data-history-retry]');
    if (retry) { void refresh(); return; }
    const provider = event.target?.closest?.('[data-history-provider]');
    if (provider) { selectedID = Number(provider.dataset.historyProvider); render(); }
  });
  void refresh();
  return {refresh, setTimeZone(value) { zone = value; render(); }};
}
