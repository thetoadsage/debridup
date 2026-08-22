import {escapeHTML, formatLatency, formatPercentage} from './dashboard-model.mjs';
import {formatTimestamp} from './timezone.mjs';

const CHART_COLORS = ['#56a9ff', '#35d19d', '#ffbe58', '#ff7182', '#a78bfa', '#22d3ee'];
const PULSE_STATES = new Set(['healthy', 'degraded', 'outage', 'unknown']);
const STATE_SYMBOLS = Object.freeze({healthy: '✓', degraded: '!', outage: '×', unknown: '?'});

function stateLabel(value) {
  const normalized = String(value || 'unknown').replaceAll('_', ' ');
  return normalized.charAt(0).toUpperCase() + normalized.slice(1);
}

function bucketTime(value, timeZone) {
  if (!Number.isFinite(value)) return 'unknown time';
  return formatTimestamp(value, timeZone);
}

function measuredPoints(provider) {
  return (Array.isArray(provider?.series) ? provider.series : [])
    .filter(point => Number.isFinite(point?.bucketStart) && Number.isFinite(point?.p95Ms));
}

export function renderPulse(providers, timeZone) {
  const heading = '<div class="section-title"><div><h2 id="pulse-heading">Provider pulse</h2><p class="muted">Health changes across the selected range.</p></div><span class="status-key"><span><b class="status-key-symbol healthy" aria-hidden="true">✓</b>Healthy</span><span><b class="status-key-symbol degraded" aria-hidden="true">!</b>Degraded</span><span><b class="status-key-symbol outage" aria-hidden="true">×</b>Outage</span><span><b class="status-key-symbol unknown" aria-hidden="true">?</b>No data</span></span></div>';
  const rows = (Array.isArray(providers) ? providers : []).map(provider => {
    const name = escapeHTML(provider?.name || 'Unnamed provider');
    const series = Array.isArray(provider?.series) ? provider.series : [];
    const buckets = series.map((point, index) => {
      const rawState = PULSE_STATES.has(point?.state) ? point.state : 'unknown';
      const label = [
        `${name}: ${stateLabel(rawState)}`,
        bucketTime(point?.bucketStart, timeZone),
        `${formatPercentage(point?.availability)} availability`,
        `p95 ${formatLatency(point?.p95Ms)}`,
      ].join(', ');
      // Roving tabindex: the row is a single tab stop and arrow keys move
      // between buckets, so a 90-bucket range does not add 90 tab stops.
      return `<button class="pulse-bucket ${rawState}" type="button" tabindex="${index === 0 ? '0' : '-1'}" data-provider-id="${Number(provider?.id) || 0}" data-bucket-index="${index}"><span class="pulse-symbol" aria-hidden="true">${STATE_SYMBOLS[rawState]}</span><span class="sr-only">${label}</span></button>`;
    }).join('');
    const track = buckets || '<span class="pulse-empty">No checks in this range</span>';
    const trackAttributes = buckets
      ? ` role="toolbar" aria-orientation="horizontal" aria-label="${name}: ${series.length} status buckets, use arrow keys to review"`
      : '';
    return `<div class="pulse-row"><span class="pulse-provider">${name}</span><div class="pulse-track"${trackAttributes} style="--pulse-bucket-count:${Math.max(1, series.length)}">${track}</div></div>`;
  });

  return rows.length
    ? `${heading}<div class="pulse-rows">${rows.join('')}</div>`
    : `${heading}<div class="dashboard-empty"><p>No providers configured.</p><a href="#provider-management">Add a provider to begin monitoring.</a></div>`;
}

export function renderLatencyChart(providers, timeZone) {
  const plottedProviders = (Array.isArray(providers) ? providers : [])
    .map(provider => ({provider, points: measuredPoints(provider)}))
    .filter(item => item.points.length > 0);
  const totalMeasured = plottedProviders.reduce((total, item) => total + item.points.length, 0);
  if (totalMeasured < 2) {
    return '<div class="chart-empty"><strong>Not enough latency data</strong><p>At least two measured latency points are needed for a comparison.</p></div>';
  }

  const width = 680;
  const height = 260;
  const inset = {top: 20, right: 18, bottom: 42, left: 58};
  const plotWidth = width - inset.left - inset.right;
  const plotHeight = height - inset.top - inset.bottom;
  const allPoints = plottedProviders.flatMap(item => item.points);
  const firstBucket = Math.min(...allPoints.map(point => point.bucketStart));
  const lastBucket = Math.max(...allPoints.map(point => point.bucketStart));
  const timeSpan = Math.max(1, lastBucket - firstBucket);
  const maximum = Math.max(1, ...allPoints.map(point => point.p95Ms));
  const x = bucket => inset.left + ((bucket - firstBucket) / timeSpan) * plotWidth;
  const y = latency => inset.top + plotHeight - (latency / maximum) * plotHeight;
  const tickValues = [maximum, maximum / 2, 0];
  const axes = tickValues.map(value => {
    const coordinate = y(value);
    return `<g class="latency-axis-tick"><line x1="${inset.left}" y1="${coordinate.toFixed(2)}" x2="${width - inset.right}" y2="${coordinate.toFixed(2)}"></line><text x="${inset.left - 8}" y="${(coordinate + 4).toFixed(2)}" text-anchor="end">${Math.round(value)} ms</text></g>`;
  }).join('');
  const paths = plottedProviders.map((item, index) => {
    const path = item.points.map((point, pointIndex) => `${pointIndex === 0 ? 'M' : 'L'} ${x(point.bucketStart).toFixed(2)} ${y(point.p95Ms).toFixed(2)}`).join(' ');
    return `<path class="latency-series" data-provider-id="${Number(item.provider?.id) || 0}" d="${path}" fill="none" stroke="${CHART_COLORS[index % CHART_COLORS.length]}" stroke-width="3" stroke-linecap="round" stroke-linejoin="round" vector-effect="non-scaling-stroke"></path>`;
  }).join('');
  const markers = plottedProviders.map((item, index) => item.points.map(point => `<circle class="latency-point" cx="${x(point.bucketStart).toFixed(2)}" cy="${y(point.p95Ms).toFixed(2)}" r="3.5" fill="${CHART_COLORS[index % CHART_COLORS.length]}"><title>${escapeHTML(item.provider?.name || 'Unnamed provider')}, ${escapeHTML(bucketTime(point.bucketStart, timeZone))}, ${formatLatency(point.p95Ms)}</title></circle>`).join('')).join('');
  const legend = plottedProviders.map((item, index) => `<li><span class="legend-line" style="background:${CHART_COLORS[index % CHART_COLORS.length]}"></span>${escapeHTML(item.provider?.name || 'Unnamed provider')} <span>p95</span></li>`).join('');
  const summaries = plottedProviders.map(item => `<tr><th scope="row">${escapeHTML(item.provider?.name || 'Unnamed provider')}</th><td>${item.points.length}</td><td>${formatLatency(item.provider?.p50Ms)}</td><td>${formatLatency(item.provider?.p95Ms)}</td></tr>`).join('');

  return `<div class="latency-visual"><svg class="latency-svg" viewBox="0 0 ${width} ${height}" role="img" aria-labelledby="latency-svg-title latency-svg-description"><title id="latency-svg-title">Provider p95 latency comparison</title><desc id="latency-svg-description">Latency in milliseconds for each provider across the selected range. Exact values follow in a table.</desc><g class="latency-axes">${axes}<line x1="${inset.left}" y1="${inset.top}" x2="${inset.left}" y2="${height - inset.bottom}"></line><line x1="${inset.left}" y1="${height - inset.bottom}" x2="${width - inset.right}" y2="${height - inset.bottom}"></line><text x="${inset.left}" y="${height - 14}" text-anchor="start">${escapeHTML(bucketTime(firstBucket, timeZone))}</text><text x="${width - inset.right}" y="${height - 14}" text-anchor="end">${escapeHTML(bucketTime(lastBucket, timeZone))}</text></g>${paths}${markers}</svg><ul class="chart-legend" aria-label="Latency chart legend">${legend}</ul></div><div class="table-scroll chart-table-scroll"><table class="chart-summary"><caption>Latency summary for the selected range</caption><thead><tr><th scope="col">Provider</th><th scope="col">Measured points</th><th scope="col">p50</th><th scope="col">p95</th></tr></thead><tbody>${summaries}</tbody></table></div>`;
}
