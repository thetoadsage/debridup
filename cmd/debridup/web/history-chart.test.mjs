import test from 'node:test';
import assert from 'node:assert/strict';
import {chartMarkup, historyMarkup, latencyPath, validPoints} from './history-chart.mjs';

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

test('history markup has accessible selector, status text, and no inline styles', () => {
  const html = historyMarkup({providers: [{id: 5, name: '<Provider>', state: 'healthy', availability: 99.5, p50Ms: 20, p95Ms: 30, slowestMs: 45, series}]}, 5, 'UTC');
  assert.match(html, /data-history-provider="5"/);
  assert.match(html, /aria-pressed="true"/);
  assert.match(html, /Accessible data summary/);
  assert.match(html, /Status timeline/);
  assert.match(html, /&lt;Provider&gt;/);
  assert.doesNotMatch(html, /style=/);
});

test('history falls back to the first provider when a selected provider disappears', () => {
  const html = historyMarkup({providers: [{id: 6, name: 'Remaining', state: 'checking', series}]}, 5, 'UTC');
  assert.match(html, /data-history-provider="6"/);
  assert.match(html, /Checking/);
  assert.match(html, /aria-pressed="true"/);
});
