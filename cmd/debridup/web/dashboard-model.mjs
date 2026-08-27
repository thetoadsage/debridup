const EMPTY_SUMMARY = Object.freeze({
  overallState: 'unknown',
  providersOnline: 0,
  activeIncidents: 0,
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

export function formatState(value) {
  const normalized = String(value || 'unknown').replaceAll('_', ' ');
  return normalized.charAt(0).toUpperCase() + normalized.slice(1);
}

export function escapeHTML(value) {
  return String(value ?? '').replace(/[&<>"']/g, character => ESCAPED_CHARACTERS[character]);
}

export function createDashboardModel(payload = {}, now = Date.now(), timeZone) {
  const generatedAt = isNumber(payload.generatedAt) ? payload.generatedAt : 0;
  const ageSeconds = generatedAt > 0
    ? Math.max(0, Math.floor((now - generatedAt * 1000) / 1000))
    : 0;
  const providers = asArray(payload.providers).map(provider => ({
    ...provider,
    stateLabel: formatState(provider?.state),
    latencyLabel: formatLatency(provider?.latencyMs),
    lastCheckLabel: formatTimestamp(provider?.lastCheck, timeZone),
  }));
  const summary = {...EMPTY_SUMMARY, ...(payload.summary || {})};

  return {
    generatedAt,
    summary: {...summary, overallStateLabel: formatState(summary.overallState)},
    providers,
    ageSeconds,
    updatedLabel: generatedAt > 0 ? formatTimestamp(generatedAt, timeZone) : 'Update unavailable',
    stale: ageSeconds > 90,
  };
}
