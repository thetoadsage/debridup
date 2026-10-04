import test from 'node:test';
import assert from 'node:assert/strict';
import {providerColorClass} from './provider-colors.mjs';
import {comparisonChartMarkup, historyMarkup} from './history-chart.mjs';
import {storedTheme, setupThemePicker, THEMES} from './theme.mjs';

const names = ['torbox', 'premiumize', 'alldebrid', 'realdebrid', 'torrin', 'pikpak', 'offcloud', 'debridlink', 'easydebrid', 'debrider', 'deepbrid'];
const providers = names.map((provider, index) => ({id: index + 1, provider, name: provider, series: [{bucketStart: 1, p50Ms: 10}, {bucketStart: 2, p50Ms: 20}]}));
test('all supported providers have unique, order-independent colors shared by chart and legend', () => {
  assert.equal(new Set(providers.map(providerColorClass)).size, 11);
  for (const ordered of [providers, [...providers].reverse(), providers.slice(3)]) {
    const chart = comparisonChartMarkup(ordered, 'UTC');
    const history = historyMarkup({providers: ordered}, 'all', 'UTC');
    for (const provider of ordered) {
      const color = providerColorClass(provider);
      assert.match(chart, new RegExp(`comparison-line ${color} [^"\\n]+" data-comparison-provider="${provider.id}"`));
      assert.match(history, new RegExp(`comparison-swatch ${color} `));
    }
  }
});
test('unrecognized provider identity cannot inject markup', () => {
  assert.match(providerColorClass({provider: '<script>', id: NaN}), /^provider-color-\d+$/);
});
test('all theme selections persist and blocked storage remains usable', () => {
  for (const theme of Object.keys(THEMES)) {
    let change;
    let saved;
    const picker = {addEventListener(type, fn) { change = fn; }, removeEventListener() {}};
    const document = {documentElement: {dataset: {}}, getElementById() { return picker; }};
    setupThemePicker({document, storage: {getItem() {return theme;}, setItem(key, value) {saved = value;}}});
    assert.equal(document.documentElement.dataset.theme, theme);
    change({target: {value: theme}});
    assert.equal(saved, theme);
  }
  assert.equal(storedTheme({getItem() {throw Error('blocked');}}), 'graphite');
});
