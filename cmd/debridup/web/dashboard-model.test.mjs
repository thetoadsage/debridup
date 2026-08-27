import test from 'node:test';
import assert from 'node:assert/strict';
import {createDashboardModel, escapeHTML, formatLatency, formatState} from './dashboard-model.mjs';
import {applyTheme, normalizeTheme, THEMES} from './theme.mjs';
import {AUTO_TIME_ZONE, timeZoneDescription} from './timezone.mjs';
import {normalizeIncident} from './dashboard.mjs';

test('formats simple current-health fields safely', () => {
  assert.equal(formatLatency(81), '81 ms');
  assert.equal(formatLatency(null), '—');
  assert.equal(formatState('auth_failed'), 'Auth failed');
  assert.equal(escapeHTML('<service&>'), '&lt;service&amp;&gt;');
});

test('normalizes a current-status response without analytics data', () => {
  const model = createDashboardModel({
    generatedAt: 1_700_000_000,
    summary: {overallState: 'degraded', providersOnline: 1, activeIncidents: 1},
    providers: [{id: 1, name: 'Example', state: 'slow', latencyMs: 8000, lastCheck: 1_700_000_000, activeIncident: true}],
  }, 1_700_000_030_000, 'UTC');
  assert.equal(model.providers.length, 1);
  assert.equal(model.providers[0].stateLabel, 'Slow');
  assert.equal(model.summary.overallStateLabel, 'Degraded');
  assert.equal(model.stale, false);
});

test('marks an aged health response stale', () => {
  assert.equal(createDashboardModel({generatedAt: 100}, 191_000, 'UTC').stale, true);
});

test('keeps theme and time-zone preferences safe', () => {
  assert.equal(normalizeTheme('terminal'), 'terminal');
  assert.equal(normalizeTheme('not-a-theme'), 'graphite');
  assert.equal(Object.keys(THEMES).length, 4);
  assert.equal(AUTO_TIME_ZONE, 'browser');
  assert.match(timeZoneDescription('UTC'), /UTC/);
  const document = {documentElement: {dataset: {}}};
  applyTheme(document, 'sakura');
  assert.equal(document.documentElement.dataset.theme, 'sakura');
});

test('normalizes the incident API’s capitalized fields without changing event fields', () => {
  const incident = normalizeIncident({
    Name: '<Real-Debrid>',
    Provider: 'realdebrid',
    OpenedAt: 100,
    ResolvedAt: null,
    LatestState: 'auth_failed',
    Ongoing: true,
    summary: 'Authentication failed',
    events: [{type: 'opened', createdAt: 100, state: 'auth_failed'}],
  });
  assert.deepEqual(incident, {
    name: '<Real-Debrid>',
    provider: 'realdebrid',
    latestState: 'auth_failed',
    summary: 'Authentication failed',
    openedAt: 100,
    resolvedAt: null,
    ongoing: true,
    events: [{type: 'opened', createdAt: 100, state: 'auth_failed'}],
  });
});
