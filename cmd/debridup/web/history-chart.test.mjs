import test from 'node:test';
import assert from 'node:assert/strict';
import {chartMarkup, comparisonChartMarkup, historyMarkup, latencyPath, startServiceHistory, validPoints} from './history-chart.mjs';

const series = [
  {bucketStart: 1, state: 'healthy', availability: 100, p50Ms: 40, p95Ms: 80},
  {bucketStart: 2, state: 'unknown', availability: null, p50Ms: null, p95Ms: null},
  {bucketStart: 3, state: 'slow', availability: 90, p50Ms: 90, p95Ms: 150},
];

test('latency paths preserve unknown bucket gaps', () => {
  const path = latencyPath(series, 'p50Ms');
  assert.match(path, /^M/);
  assert.equal((path.match(/M/g) || []).length, 2);
  assert.equal(validPoints([...series, {bucketStart: 'nope'}]).length, 3);
});

test('chart markup labels both lines and avoids an invented line without samples', () => {
  assert.match(chartMarkup({name: 'Safe provider', series}, 'UTC'), /latency-p50/);
  assert.match(chartMarkup({name: 'Safe provider', series}, 'UTC'), /latency-p95/);
  assert.match(chartMarkup({name: 'Empty', series: []}, 'UTC'), /No latency samples/);
});

test('chart markup remains finite for zero and constant latency samples', () => {
  const zeroes = [{bucketStart: 1, p50Ms: 0, p95Ms: 0}, {bucketStart: 2, p50Ms: 0, p95Ms: 0}];
  const html = chartMarkup({name: 'Zeroes', series: zeroes}, 'UTC');
  assert.match(html, /latency-p50/);
  assert.doesNotMatch(html, /NaN|Infinity/);
});

test('p50 and p95 paths share the same vertical scale', () => {
  const scaled = [{bucketStart: 1, p50Ms: 25, p95Ms: 100}, {bucketStart: 2, p50Ms: 50, p95Ms: 100}];
  const html = chartMarkup({name: 'Scaled', series: scaled}, 'UTC');
  assert.match(html, /latency-p50[^>]+d="M70\.0,155\.5 L686\.0,115\.0"/);
  assert.match(html, /latency-p95[^>]+d="M70\.0,34\.0 L686\.0,34\.0"/);
});

test('history markup has accessible selector, readable status timeline, and matching legend', () => {
  const html = historyMarkup({providers: [{id: 5, name: '<Provider>', state: 'healthy', availability: 99.5, p50Ms: 20, p95Ms: 30, slowestMs: 45, series}]}, 5, 'UTC');
  assert.match(html, /data-history-provider="5"/);
  assert.match(html, /aria-pressed="true"/);
  assert.match(html, /Accessible data summary/);
  assert.match(html, /Status timeline/);
  assert.match(html, /&lt;Provider&gt;/);
  assert.match(html, /history-status-swatch healthy/);
  assert.match(html, /history-status-times/);
  assert.doesNotMatch(html, /style=/);
});

test('status timeline groups adjacent states into proportional semantic runs', () => {
  const html = historyMarkup({providers: [{id: 5, name: 'Provider', state: 'healthy', series: [
    {bucketStart: 1, state: 'healthy'}, {bucketStart: 2, state: 'healthy'},
    {bucketStart: 3, state: 'outage'}, {bucketStart: 4, state: 'unknown'},
  ]}]}, 5, 'UTC');
  assert.equal((html.match(/history-status-segment /g) || []).length, 3);
  assert.match(html, /class="history-status-segment healthy" x="0" y="0" width="2"/);
  assert.match(html, /class="history-status-segment outage" x="2" y="0" width="1"/);
  assert.match(html, /history-status-swatch outage/);
});

test('history falls back to the first provider when a selected provider disappears', () => {
  const html = historyMarkup({providers: [{id: 6, name: 'Remaining', state: 'checking', series}]}, 5, 'UTC');
  assert.match(html, /data-history-provider="6"/);
  assert.match(html, /Checking/);
  assert.match(html, /aria-pressed="true"/);
});

test('all-services comparison uses one shared scale and preserves provider gaps', () => {
  const providers = [
    {id: 1, name: 'Fast & safe', state: 'healthy', p50Ms: 50, series: [
      {bucketStart: 1, p50Ms: 25}, {bucketStart: 2, p50Ms: null}, {bucketStart: 3, p50Ms: 50},
    ]},
    {id: 2, name: '<Slow>', state: 'paused', p50Ms: 100, series: [
      {bucketStart: 1, p50Ms: 100}, {bucketStart: 2, p50Ms: 100}, {bucketStart: 3, p50Ms: 100},
    ]},
    {id: 3, name: 'No samples', state: 'unknown', series: []},
  ];
  const html = comparisonChartMarkup(providers, 'UTC');
  assert.match(html, /data-comparison-provider="1"[^>]+d="M70\.0,155\.5M686\.0,115\.0"/);
  assert.match(html, /data-comparison-provider="2"[^>]+d="M70\.0,34\.0 L378\.0,34\.0 L686\.0,34\.0"/);
  assert.match(html, /<title>Fast &amp; safe p50 latency<\/title>/);
  assert.doesNotMatch(html, /data-comparison-provider="3"/);
  assert.doesNotMatch(html, /NaN|Infinity/);

  const comparison = historyMarkup({providers}, 'all', 'UTC');
  assert.match(comparison, /data-history-provider="all" aria-pressed="true"/);
  assert.match(comparison, /Compare 3 configured services/);
  assert.match(comparison, /Fast &amp; safe/);
  assert.match(comparison, /&lt;Slow&gt;/);
  assert.match(comparison, /No samples/);
  assert.match(comparison, /Service history comparison/);
  assert.match(comparison, /Paused/);
  assert.doesNotMatch(comparison, /style=/);
});

test('all-services selection survives a history refresh', async () => {
  let rootClick;
  const root = {innerHTML: '', setAttribute() {}, addEventListener(type, handler) { if (type === 'click') rootClick = handler; }};
  const range = {value: '24h', disabled: false, addEventListener() {}};
  const status = {textContent: ''};
  const document = {getElementById(id) { return {"history-content": root, "history-range": range, "history-message": status}[id]; }};
  const data = {providers: [
    {id: 1, name: 'One', state: 'healthy', series: [{bucketStart: 1, p50Ms: 10}]},
    {id: 2, name: 'Two', state: 'healthy', series: [{bucketStart: 1, p50Ms: 20}]},
  ]};
  const history = startServiceHistory({api: async () => data, document, timeZone: 'UTC'});
  await new Promise(resolve => setTimeout(resolve, 0));
  rootClick({target: {closest: selector => selector === '[data-history-provider]' ? {dataset: {historyProvider: 'all'}} : null}});
  assert.match(root.innerHTML, /data-history-provider="all" aria-pressed="true"/);
  await history.refresh();
  assert.match(root.innerHTML, /data-history-provider="all" aria-pressed="true"/);
});
