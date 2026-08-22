const EMPTY_SUMMARY = Object.freeze({
  overallState: 'unknown',
  providersOnline: 0,
  activeIncidents: 0,
  checksToday: 0,
});

import {formatTimestamp} from './timezone.mjs';

export {formatTimestamp} from './timezone.mjs';

const ESCAPED_CHARACTERS = Object.freeze({
  '&': '&amp;',
  '<': '&lt;',
  '>': '&gt;',
  '"': '&quot;',
  "'": '&#39;',
});

function asArray(value) {
  return Array.isArray(value) ? value : [];
}

function isNumber(value) {
  return typeof value === 'number' && Number.isFinite(value);
}

export function formatLatency(value) {
  return Number.isFinite(value) ? `${value} ms` : '—';
}

export function formatPercentage(value) {
  return Number.isFinite(value) ? `${value.toFixed(2)}%` : '—';
}

export function formatState(value) {
  const normalized = String(value || 'unknown').replaceAll('_', ' ');
  return normalized.charAt(0).toUpperCase() + normalized.slice(1);
}

export function formatDuration(value) {
  if (!isNumber(value)) return '—';
  let seconds = Math.max(0, Math.floor(value));
  if (seconds === 0) return 'just now';
  if (seconds < 60) return `${seconds} ${seconds === 1 ? 'second' : 'seconds'}`;
  const units = [
    ['day', 86400],
    ['hour', 3600],
    ['minute', 60],
  ];
  const parts = [];
  for (const [label, size] of units) {
    const count = Math.floor(seconds / size);
    if (count > 0) {
      parts.push(`${count} ${label}${count === 1 ? '' : 's'}`);
      seconds -= count * size;
    }
    if (parts.length === 2) break;
  }
  return parts.join(' ');
}

export function escapeHTML(value) {
  return String(value ?? '').replace(/[&<>"']/g, character => ESCAPED_CHARACTERS[character]);
}

export function createDashboardModel(payload = {}, now = Date.now(), timeZone) {
  const nowSeconds = Math.floor(now / 1000);
  const generatedAt = isNumber(payload.generatedAt) ? payload.generatedAt : 0;
  const ageSeconds = generatedAt > 0
    ? Math.max(0, Math.floor((now - generatedAt * 1000) / 1000))
    : 0;
  const incidents = asArray(payload.incidents).map(incident => ({
    ...incident,
    stateLabel: formatState(incident?.latestState),
    openedAtLabel: formatTimestamp(incident?.openedAt, timeZone),
    resolvedAtLabel: isNumber(incident?.resolvedAt) ? formatTimestamp(incident.resolvedAt, timeZone) : 'Ongoing',
  }));
  const providers = asArray(payload.providers).map(provider => {
    const providerIncidents = incidents.filter(incident => incident.monitorId === provider?.id);
    return {
      ...provider,
      stateLabel: formatState(provider?.state),
      stateDurationLabel: isNumber(provider?.stateSince)
        ? formatDuration(nowSeconds - provider.stateSince)
        : '—',
      lastCheckLabel: formatTimestamp(provider?.lastCheck, timeZone),
      availabilityLabel: formatPercentage(provider?.availability),
      p50Label: formatLatency(provider?.p50Ms),
      p95Label: formatLatency(provider?.p95Ms),
      slowestLabel: formatLatency(provider?.slowestMs),
      latestEvent: providerIncidents[0]?.summary || 'No recent events.',
      series: asArray(provider?.series).map(point => ({...point})),
      incidents: providerIncidents,
    };
  });
  const summary = {...EMPTY_SUMMARY, ...(payload.summary || {})};

  return {
    generatedAt,
    range: payload.range || '24h',
    summary: {...summary, overallStateLabel: formatState(summary.overallState)},
    providers,
    incidents,
    ageSeconds,
    ageLabel: formatDuration(ageSeconds),
    updatedLabel: generatedAt > 0 ? formatTimestamp(generatedAt, timeZone) : 'Update unavailable',
    stale: ageSeconds > 90,
  };
}
