import test from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';

import {
  createDashboardModel,
  escapeHTML,
  formatDuration,
  formatLatency,
  formatPercentage,
  formatState,
  formatTimestamp,
} from './dashboard-model.mjs';
import {renderLatencyChart, renderPulse} from './chart.mjs';
import {createProviderDrawer} from './drawer.mjs';
import {startDashboard} from './dashboard.mjs';
import {applyTheme, normalizeTheme, setupThemePicker, storedTheme, THEMES} from './theme.mjs';
import {AUTO_TIME_ZONE, formatTimestamp as formatInTimeZone, setupTimeZonePicker, timeZoneDescription} from './timezone.mjs';
import {setupSectionNavigation} from './navigation.mjs';

class FakeElement {
  constructor() {
    this.attributes = new Map();
    this.className = '';
    this.hidden = false;
    this.innerHTML = '';
    this.listeners = new Map();
    this.textContent = '';
    this.focusCount = 0;
    this.dataset = {};
    const classes = new Set();
    this.classList = {
      add: (...names) => names.forEach(name => classes.add(name)),
      remove: (...names) => names.forEach(name => classes.delete(name)),
      contains: name => classes.has(name),
      toggle: (name, force) => {
        if (force === false) classes.delete(name);
        else if (force === true || !classes.has(name)) classes.add(name);
        else classes.delete(name);
      },
    };
  }

  addEventListener(type, listener) {
    const listeners = this.listeners.get(type) || [];
    listeners.push(listener);
    this.listeners.set(type, listeners);
  }

  removeEventListener(type, listener) {
    this.listeners.set(type, (this.listeners.get(type) || []).filter(item => item !== listener));
  }

  emit(type, event = {}) {
    event.target ??= this;
    for (const listener of this.listeners.get(type) || []) listener(event);
  }

  setAttribute(name, value) {
    this.attributes.set(name, String(value));
  }

  getAttribute(name) {
    return this.attributes.get(name) ?? null;
  }

  removeAttribute(name) {
    this.attributes.delete(name);
  }

  focus() {
    this.focusCount += 1;
  }
}

class FakeDashboardDocument {
  constructor() {
    this.hidden = false;
    this.listeners = new Map();
    this.elements = new Map([
      'dashboard-status', 'updated', 'summary', 'provider-pulse', 'provider-table-body',
      'latency-chart', 'incidents', 'comparison', 'refresh',
    ].map(id => [id, new FakeElement()]));
    this.ranges = ['24h', '7d', '30d'].map(range => {
      const button = new FakeElement();
      button.dataset.range = range;
      return button;
    });
  }

  addEventListener(type, listener) {
    const listeners = this.listeners.get(type) || [];
    listeners.push(listener);
    this.listeners.set(type, listeners);
  }

  removeEventListener(type, listener) {
    this.listeners.set(type, (this.listeners.get(type) || []).filter(item => item !== listener));
  }

  emit(type) {
    for (const listener of this.listeners.get(type) || []) listener();
  }

  getElementById(id) {
    return this.elements.get(id) || null;
  }

  querySelectorAll(selector) {
    return selector === '#range-controls [data-range]' ? this.ranges : [];
  }
}

class FakeDashboardWindow {
  constructor(now = 1787320830000) {
    this.now = now;
    this.AbortController = AbortController;
    this.Date = {now: () => this.now};
    this.nextTimer = 0;
    this.timers = new Map();
  }

  setTimeout(callback) {
    const id = ++this.nextTimer;
    this.timers.set(id, callback);
    return id;
  }

  clearTimeout(id) {
    this.timers.delete(id);
  }
}

function deferredRequest(signal) {
  let resolve;
  let reject;
  const promise = new Promise((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  signal?.addEventListener('abort', () => {
    const error = new Error('aborted');
    error.name = 'AbortError';
    reject(error);
  });
  return {promise, resolve, reject};
}

function dashboardPayload(overrides = {}) {
  return {
    generatedAt: 1787320800,
    range: '24h',
    summary: {overallState: 'healthy', providersOnline: 1, activeIncidents: 0, checksToday: 12},
    providers: [{
      id: 1,
      name: 'Provider Alpha',
      provider: 'alpha',
      state: 'healthy',
      availability: 99.5,
      p50Ms: 80,
      p95Ms: 120,
      series: [
        {bucketStart: 1787317200, state: 'healthy', availability: 100, p95Ms: 100},
        {bucketStart: 1787318100, state: 'healthy', availability: 100, p95Ms: 120},
      ],
    }],
    incidents: [],
    ...overrides,
  };
}

async function flushPromises() {
  await Promise.resolve();
  await Promise.resolve();
}

function providerDrawerFixture() {
  const root = new FakeElement();
  const overlay = new FakeElement();
  const closeButton = new FakeElement();
  const lastFocusable = new FakeElement();
  const title = new FakeElement();
  const state = new FakeElement();
  const incidentList = new FakeElement();
  const values = Object.fromEntries([
    'state-duration', 'availability', 'p50', 'p95', 'last-check', 'slowest', 'latest-event',
  ].map(name => [name, new FakeElement()]));
  const selectorMap = new Map([
    ['#provider-drawer-close', closeButton],
    ['#provider-drawer-title', title],
    ['[data-drawer-state]', state],
    ['.drawer-incident-list', incidentList],
    ...Object.entries(values).map(([name, element]) => [`[data-drawer-value="${name}"]`, element]),
  ]);
  root.querySelector = selector => selectorMap.get(selector) || null;
  root.querySelectorAll = () => [closeButton, lastFocusable];
  root.ownerDocument = {
    body: {classList: {add() {}, remove() {}}},
    getElementById: id => id === 'provider-drawer-overlay' ? overlay : null,
  };
  return {root, overlay, closeButton, lastFocusable, title, state, incidentList, values};
}

test('creates a stable empty model', () => {
  const model = createDashboardModel({
    generatedAt: 1787320800,
    range: '24h',
    summary: {},
    providers: [],
    incidents: [],
  }, 1787320830000);

  assert.deepEqual(model.providers, []);
  assert.deepEqual(model.incidents, []);
  assert.equal(model.ageSeconds, 30);
  assert.equal(model.stale, false);
});

test('formats missing and measured latency', () => {
  assert.equal(formatLatency(null), '—');
  assert.equal(formatLatency(138), '138 ms');
});

test('formats missing and measured percentages to two decimals', () => {
  assert.equal(formatPercentage(undefined), '—');
  assert.equal(formatPercentage(98.125), '98.13%');
});

test('marks a response stale only after ninety seconds', () => {
  const fresh = createDashboardModel({generatedAt: 1787320800}, 1787320890000);
  const stale = createDashboardModel({generatedAt: 1787320800}, 1787320891000);

  assert.equal(fresh.ageSeconds, 90);
  assert.equal(fresh.stale, false);
  assert.equal(stale.ageSeconds, 91);
  assert.equal(stale.stale, true);
});

test('normalizes absent arrays and attaches incidents to providers', () => {
  const model = createDashboardModel({
    generatedAt: 1787320800,
    range: '7d',
    providers: [{
      id: 42,
      name: 'Provider Alpha',
      provider: 'alpha',
      state: 'healthy',
      series: null,
    }],
    incidents: [{
      id: 7,
      monitorId: 42,
      name: 'Provider Alpha',
      summary: 'Recovered after a brief interruption.',
      openedAt: 1787317200,
      resolvedAt: 1787319000,
      latestState: 'healthy',
    }],
  }, 1787320830000);

  assert.deepEqual(model.providers[0].series, []);
  assert.equal(model.providers[0].incidents.length, 1);
  assert.equal(model.providers[0].incidents[0].id, 7);
});

test('escapes every HTML-sensitive character in one centralized helper', () => {
  assert.equal(
    escapeHTML(`<status code="bad" reason='slow'>&`),
    '&lt;status code=&quot;bad&quot; reason=&#39;slow&#39;&gt;&amp;',
  );
});

test('formats deterministic state, duration, and explicitly selected timestamp labels', () => {
  assert.equal(formatState('connection_issue'), 'Connection issue');
  assert.equal(formatDuration(3660), '1 hour 1 minute');
  assert.equal(formatTimestamp(1787320800, 'UTC'), 'Aug 21, 2026, 2:00 PM UTC');
  assert.equal(formatTimestamp(1787320800, 'America/Chicago'), 'Aug 21, 2026, 9:00 AM CDT');
  assert.equal(formatTimestamp(null, 'UTC'), 'No checks yet');
});

test('derives provider labels and state age from the injected clock', () => {
  const model = createDashboardModel({
    generatedAt: 1787320800,
    providers: [{
      id: 1,
      name: 'Provider Alpha',
      state: 'connection_issue',
      stateSince: 1787317200,
      lastCheck: 1787320500,
      availability: 99.5,
      p50Ms: 80,
      p95Ms: null,
      slowestMs: 420,
    }],
  }, 1787320800000, 'UTC');

  assert.equal(model.providers[0].stateLabel, 'Connection issue');
  assert.equal(model.providers[0].stateDurationLabel, '1 hour');
  assert.equal(model.providers[0].availabilityLabel, '99.50%');
  assert.equal(model.providers[0].p50Label, '80 ms');
  assert.equal(model.providers[0].p95Label, '—');
  assert.equal(model.providers[0].slowestLabel, '420 ms');
  assert.equal(model.providers[0].lastCheckLabel, 'Aug 21, 2026, 1:55 PM UTC');
});

test('renders one accessible pulse button for every server bucket', () => {
  const markup = renderPulse([{
    id: 1,
    name: 'Provider <Alpha>',
    series: [
      {bucketStart: 1787317200, state: 'healthy', availability: 100, p95Ms: 120},
      {bucketStart: 1787318100, state: 'outage', availability: 0, p95Ms: 450},
    ],
  }]);

  assert.equal((markup.match(/<button/g) || []).length, 2);
  assert.match(markup, /id="pulse-heading"/);
  assert.match(markup, /Provider &lt;Alpha&gt;/);
  assert.match(markup, /Provider &lt;Alpha&gt;: Healthy/);
  assert.match(markup, /Provider &lt;Alpha&gt;: Outage/);
});

test('renders visible pulse symbols alongside accessible bucket names', () => {
  const markup = renderPulse([{
    id: 1,
    name: 'Provider Alpha',
    series: [
      {bucketStart: 1787317200, state: 'healthy'},
      {bucketStart: 1787318100, state: 'degraded'},
      {bucketStart: 1787319000, state: 'outage'},
      {bucketStart: 1787319900, state: 'unknown'},
    ],
  }]);

  assert.equal((markup.match(/class="pulse-symbol"/g) || []).length, 4);
  assert.match(markup, /<span class="pulse-symbol" aria-hidden="true">✓<\/span>/);
  assert.match(markup, /<span class="pulse-symbol" aria-hidden="true">!<\/span>/);
  assert.match(markup, /<span class="pulse-symbol" aria-hidden="true">×<\/span>/);
  assert.match(markup, /<span class="pulse-symbol" aria-hidden="true">\?<\/span>/);
});

test('renders the maximum server bucket count into a bounded pulse grid', () => {
  const markup = renderPulse([{
    id: 1,
    name: 'Provider Alpha',
    series: Array.from({length: 96}, (_, index) => ({
      bucketStart: 1787317200 + index * 900,
      state: 'healthy',
    })),
  }]);

  assert.equal((markup.match(/<button/g) || []).length, 96);
  assert.match(markup, /class="pulse-track" style="--pulse-bucket-count:96"/);
});

test('renders explicit empty latency markup with fewer than two measured points', () => {
  const markup = renderLatencyChart([{
    id: 1,
    name: 'Provider Alpha',
    series: [{bucketStart: 1787317200, p95Ms: 120}],
  }]);

  assert.match(markup, /class="chart-empty"/);
  assert.match(markup, /two measured latency points/i);
  assert.doesNotMatch(markup, /<svg/);
});

test('renders latency paths with axes, units, legend, and a summary table', () => {
  const markup = renderLatencyChart([{
    id: 1,
    name: 'Provider Alpha',
    p50Ms: 105,
    p95Ms: 150,
    series: [
      {bucketStart: 1787317200, p95Ms: 100},
      {bucketStart: 1787318100, p95Ms: 150},
    ],
  }]);

  assert.match(markup, /<svg[^>]+role="img"/);
  assert.match(markup, /<path[^>]+class="latency-series"/);
  assert.match(markup, />150 ms</);
  assert.match(markup, /class="chart-legend"/);
  assert.match(markup, /Provider Alpha/);
  assert.match(markup, /<table class="chart-summary"/);
  assert.match(markup, />105 ms</);
});

test('renders a latency comparison when two measured points belong to different providers', () => {
  const markup = renderLatencyChart([
    {
      id: 1,
      name: 'Provider Alpha',
      series: [{bucketStart: 1787317200, p95Ms: 100}],
    },
    {
      id: 2,
      name: 'Provider Beta',
      series: [{bucketStart: 1787318100, p95Ms: 150}],
    },
  ]);

  assert.match(markup, /<svg[^>]+role="img"/);
  assert.equal((markup.match(/class="latency-series"/g) || []).length, 2);
  assert.equal((markup.match(/class="latency-point"/g) || []).length, 2);
});

test('opens and populates the provider drawer before focusing its close button', () => {
  const fixture = providerDrawerFixture();
  const trigger = new FakeElement();
  const drawer = createProviderDrawer(fixture.root);

  drawer.open({
    name: 'Provider Alpha',
    state: 'healthy',
    stateLabel: 'Healthy',
    stateDurationLabel: '12 minutes',
    availabilityLabel: '99.50%',
    p50Label: '80 ms',
    p95Label: '120 ms',
    lastCheckLabel: 'Aug 21, 2026, 2:00 PM UTC',
    slowestLabel: '420 ms',
    latestEvent: 'Recovered.',
    incidents: [{summary: '<Recovered>', openedAtLabel: 'Aug 21, 2026, 1:00 PM UTC'}],
  }, trigger);

  assert.equal(fixture.root.hidden, false);
  assert.equal(fixture.overlay.hidden, false);
  assert.equal(fixture.root.getAttribute('aria-hidden'), 'false');
  assert.equal(fixture.title.textContent, 'Provider Alpha');
  assert.equal(fixture.values.availability.textContent, '99.50%');
  assert.match(fixture.incidentList.innerHTML, /&lt;Recovered&gt;/);
  assert.equal(fixture.closeButton.focusCount, 1);
});

test('traps drawer focus and restores the captured trigger on close', () => {
  const fixture = providerDrawerFixture();
  const trigger = new FakeElement();
  const drawer = createProviderDrawer(fixture.root);
  drawer.open({name: 'Provider Alpha', incidents: []}, trigger);

  let prevented = 0;
  fixture.root.emit('keydown', {key: 'Tab', target: fixture.lastFocusable, shiftKey: false, preventDefault: () => { prevented += 1; }});
  fixture.root.emit('keydown', {key: 'Tab', target: fixture.closeButton, shiftKey: true, preventDefault: () => { prevented += 1; }});
  assert.equal(prevented, 2);
  assert.equal(fixture.closeButton.focusCount, 2);
  assert.equal(fixture.lastFocusable.focusCount, 1);

  fixture.root.emit('keydown', {key: 'Escape', target: fixture.closeButton, preventDefault: () => { prevented += 1; }});
  assert.equal(fixture.root.hidden, true);
  assert.equal(fixture.overlay.hidden, true);
  assert.equal(fixture.root.getAttribute('aria-hidden'), 'true');
  assert.equal(trigger.focusCount, 1);

  drawer.open({name: 'Provider Alpha', incidents: []}, trigger);
  fixture.overlay.emit('click');
  assert.equal(trigger.focusCount, 2);
});

test('restores drawer focus to the connected replacement after dashboard rerender', () => {
  const fixture = providerDrawerFixture();
  const originalTrigger = new FakeElement();
  originalTrigger.dataset.providerId = '1';
  originalTrigger.isConnected = true;
  const replacementTrigger = new FakeElement();
  fixture.root.ownerDocument.querySelector = selector => selector === '.provider-detail-trigger[data-provider-id="1"]'
    ? replacementTrigger
    : null;
  const drawer = createProviderDrawer(fixture.root);

  drawer.open({id: 1, name: 'Provider Alpha', incidents: []}, originalTrigger);
  originalTrigger.isConnected = false;
  drawer.close();

  assert.equal(originalTrigger.focusCount, 0);
  assert.equal(replacementTrigger.focusCount, 1);
});

test('starts at 24h and range switching aborts the obsolete single request', async () => {
  const document = new FakeDashboardDocument();
  const window = new FakeDashboardWindow();
  const requests = [];
  const api = (path, options) => {
    const request = deferredRequest(options.signal);
    requests.push({path, options, request});
    return request.promise;
  };
  const dashboard = startDashboard({api, document, window});

  assert.equal(requests.length, 1);
  assert.equal(requests[0].path, '/api/dashboard?range=24h');
  document.ranges[1].emit('click');
  assert.equal(requests[0].options.signal.aborted, true);
  assert.equal(requests.length, 2);
  assert.equal(requests[1].path, '/api/dashboard?range=7d');
  assert.equal(document.ranges[0].getAttribute('aria-pressed'), 'false');
  assert.equal(document.ranges[1].getAttribute('aria-pressed'), 'true');

  requests[1].request.resolve(dashboardPayload({range: '7d'}));
  await flushPromises();
  assert.match(document.getElementById('summary').innerHTML, /Providers online/);
  assert.match(document.getElementById('provider-table-body').innerHTML, /Provider Alpha/);
  assert.match(document.getElementById('comparison').innerHTML, /99\.50%/);
  dashboard.stop();
});

test('reverts the selected range when a range refresh fails and old data remains visible', async () => {
  const document = new FakeDashboardDocument();
  const window = new FakeDashboardWindow();
  const requests = [];
  const api = (path, options) => {
    const request = deferredRequest(options.signal);
    requests.push({path, options, request});
    return request.promise;
  };
  const dashboard = startDashboard({api, document, window});
  requests[0].request.resolve(dashboardPayload());
  await dashboard.ready;
  const renderedTable = document.getElementById('provider-table-body').innerHTML;

  document.ranges[1].emit('click');
  const rangeRefresh = dashboard.refresh();
  requests[1].request.reject(new Error('temporarily unavailable'));
  await rangeRefresh;

  assert.equal(document.ranges[0].getAttribute('aria-pressed'), 'true');
  assert.equal(document.ranges[1].getAttribute('aria-pressed'), 'false');
  assert.equal(document.getElementById('provider-table-body').innerHTML, renderedTable);
  dashboard.stop();
});

test('renders visible symbols in provider table status history', async () => {
  const document = new FakeDashboardDocument();
  const window = new FakeDashboardWindow();
  const dashboard = startDashboard({
    api: () => Promise.resolve(dashboardPayload({
      providers: [{
        ...dashboardPayload().providers[0],
        series: [
          {bucketStart: 1787317200, state: 'healthy'},
          {bucketStart: 1787318100, state: 'outage'},
        ],
      }],
    })),
    document,
    window,
  });

  await dashboard.ready;

  const markup = document.getElementById('provider-table-body').innerHTML;
  assert.equal((markup.match(/class="status-symbol"/g) || []).length, 2);
  assert.match(markup, /<span class="status-symbol" aria-hidden="true">✓<\/span>/);
  assert.match(markup, /<span class="status-symbol" aria-hidden="true">×<\/span>/);
  dashboard.stop();
});

test('disables and deduplicates manual refreshes while one is active', async () => {
  const document = new FakeDashboardDocument();
  const window = new FakeDashboardWindow();
  const requests = [];
  const api = (path, options) => {
    const request = deferredRequest(options.signal);
    requests.push({path, options, request});
    return request.promise;
  };
  const dashboard = startDashboard({api, document, window});
  requests[0].request.resolve(dashboardPayload());
  await dashboard.ready;

  const refreshButton = document.getElementById('refresh');
  refreshButton.emit('click');
  refreshButton.emit('click');
  assert.equal(requests.length, 2);
  assert.equal(refreshButton.disabled, true);
  assert.equal(refreshButton.textContent, 'Refreshing…');
  assert.equal(requests[1].options.cache, 'no-store');
  assert.match(document.getElementById('dashboard-status').textContent, /Refreshing dashboard/);
  requests[1].request.resolve(dashboardPayload());
  await flushPromises();
  assert.equal(refreshButton.disabled, false);
  assert.equal(refreshButton.textContent, 'Refresh');
  dashboard.stop();
});

test('sidebar provider navigation targets the provider table before incidents', () => {
  const markup = readFileSync(new URL('./index.html', import.meta.url), 'utf8');
  assert.match(markup, /class="nav-link" href="#providers-panel">Providers<\/a>/);
  const providers = markup.indexOf('id="providers-panel"');
  const incidents = markup.indexOf('id="incidents-panel"');
  assert.ok(providers >= 0 && incidents >= 0 && providers < incidents);
});

test('sidebar navigation follows clicks, hashes, and the visible section', () => {
  const links = ['dashboard-main', 'providers-panel', 'incidents-panel', 'notification-settings'].map(id => {
    const link = new FakeElement();
    link.setAttribute('href', `#${id}`);
    return link;
  });
  const positions = {
    'dashboard-main': 0,
    'providers-panel': 360,
    'incidents-panel': 780,
    'notification-settings': 1180,
  };
  const sections = Object.fromEntries(Object.keys(positions).map(id => [id, {
    getBoundingClientRect: () => ({top: positions[id]}),
  }]));
  const listeners = new Map();
  const navigationDocument = {
    getElementById: id => sections[id] || null,
    querySelectorAll: selector => selector === '.nav-link[href^="#"]' ? links : [],
  };
  const navigationWindow = {
    innerWidth: 1280,
    location: {hash: ''},
    addEventListener: (type, listener) => listeners.set(type, listener),
    removeEventListener: type => listeners.delete(type),
  };

  const navigation = setupSectionNavigation({document: navigationDocument, window: navigationWindow});
  assert.equal(links[0].getAttribute('aria-current'), 'page');

  links[1].emit('click');
  assert.equal(links[1].getAttribute('aria-current'), 'page');
  assert.equal(links[0].getAttribute('aria-current'), null);

  positions['dashboard-main'] = -700;
  positions['providers-panel'] = -180;
  positions['incidents-panel'] = 36;
  listeners.get('scroll')();
  assert.equal(links[1].getAttribute('aria-current'), 'page');

  positions['incidents-panel'] = -20;
  listeners.get('scroll')();
  assert.equal(links[2].getAttribute('aria-current'), 'page');

  navigationWindow.location.hash = '#notification-settings';
  listeners.get('hashchange')();
  assert.equal(links[3].getAttribute('aria-current'), 'page');
  navigation.stop();
});

test('offers four named themes and falls back safely to graphite', () => {
  assert.deepEqual(Object.keys(THEMES), ['graphite', 'neo-tokyo', 'sakura', 'terminal']);
  assert.equal(normalizeTheme('sakura'), 'sakura');
  assert.equal(normalizeTheme('not-a-theme'), 'graphite');
  assert.equal(storedTheme({getItem: () => { throw new Error('blocked'); }}), 'graphite');
});

test('theme picker applies and persists a selected theme', () => {
  const picker = new FakeElement();
  picker.value = '';
  const document = {
    documentElement: {dataset: {}},
    getElementById: id => id === 'theme-select' ? picker : null,
  };
  const values = new Map([['debridup-theme', 'neo-tokyo']]);
  const storage = {
    getItem: key => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, value),
  };

  const controller = setupThemePicker({document, storage});
  assert.equal(document.documentElement.dataset.theme, 'neo-tokyo');
  assert.equal(picker.value, 'neo-tokyo');

  picker.value = 'terminal';
  picker.emit('change');
  assert.equal(controller.theme, 'terminal');
  assert.equal(values.get('debridup-theme'), 'terminal');

  controller.stop();
  picker.value = 'sakura';
  picker.emit('change');
  assert.equal(document.documentElement.dataset.theme, 'terminal');
});

test('theme application rejects unknown values', () => {
  const document = {documentElement: {dataset: {}}};
  assert.equal(applyTheme(document, 'unknown'), 'graphite');
  assert.equal(document.documentElement.dataset.theme, 'graphite');
});

test('time-zone picker defaults to browser time and persists an explicit selection', () => {
  const picker = new FakeElement();
  const detail = new FakeElement();
  const document = {
    getElementById: id => id === 'time-zone' ? picker : id === 'time-zone-detail' ? detail : null,
  };
  const values = new Map();
  const storage = {
    getItem: key => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, value),
  };
  const changed = [];

  const controller = setupTimeZonePicker({document, storage, onChange: value => changed.push(value)});
  assert.equal(controller.timeZone, AUTO_TIME_ZONE);
  assert.match(detail.textContent, /browser’s local time/);

  picker.value = 'America/Chicago';
  picker.emit('change');
  assert.equal(controller.timeZone, 'America/Chicago');
  assert.equal(values.get('debridup-time-zone'), 'America/Chicago');
  assert.deepEqual(changed, ['America/Chicago']);
  assert.equal(timeZoneDescription('America/Chicago'), 'Using America/Chicago.');
  assert.equal(formatInTimeZone(1787320800, controller.timeZone), 'Aug 21, 2026, 9:00 AM CDT');
});

test('keeps the last successful model on failure and renders its age with retry', async () => {
  const document = new FakeDashboardDocument();
  const window = new FakeDashboardWindow();
  let calls = 0;
  const api = () => {
    calls += 1;
    return calls === 1 ? Promise.resolve(dashboardPayload()) : Promise.reject(new Error('temporarily unavailable'));
  };
  const dashboard = startDashboard({api, document, window});
  await dashboard.ready;
  const renderedTable = document.getElementById('provider-table-body').innerHTML;
  window.now = 1787320925000;

  document.getElementById('refresh').emit('click');
  await flushPromises();
  assert.equal(document.getElementById('provider-table-body').innerHTML, renderedTable);
  assert.match(document.getElementById('dashboard-status').innerHTML, /Showing data from 2 minutes ago/);
  assert.match(document.getElementById('dashboard-status').innerHTML, /data-dashboard-retry/);
  dashboard.stop();
});

test('replaces initial loading regions with explicit unavailable states on failure', async () => {
  const document = new FakeDashboardDocument();
  const window = new FakeDashboardWindow();
  const dashboard = startDashboard({
    api: () => Promise.reject(new Error('temporarily unavailable')),
    document,
    window,
  });

  await dashboard.ready;

  for (const id of ['summary', 'provider-pulse', 'provider-table-body', 'latency-chart', 'incidents']) {
    assert.match(document.getElementById(id).innerHTML, /unavailable/i, `${id} did not leave its loading state`);
  }
  assert.match(document.getElementById('dashboard-status').innerHTML, /data-dashboard-retry/);
  assert.equal(document.getElementById('latency-chart').getAttribute('role'), 'group');
  assert.match(document.getElementById('latency-chart').getAttribute('aria-label'), /unavailable/i);
  assert.doesNotMatch(document.getElementById('latency-chart').getAttribute('aria-label'), /loading/i);
  dashboard.stop();
});

test('pauses scheduled refresh while hidden and refreshes on visibility restoration', async () => {
  const document = new FakeDashboardDocument();
  const window = new FakeDashboardWindow();
  let calls = 0;
  const dashboard = startDashboard({
    api: () => { calls += 1; return Promise.resolve(dashboardPayload()); },
    document,
    window,
  });
  await dashboard.ready;
  assert.equal(window.timers.size, 1);

  document.hidden = true;
  document.emit('visibilitychange');
  assert.equal(window.timers.size, 0);
  document.hidden = false;
  document.emit('visibilitychange');
  await flushPromises();
  assert.equal(calls, 2);
  dashboard.stop();
});
