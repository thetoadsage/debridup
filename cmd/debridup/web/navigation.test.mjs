import test from 'node:test';
import assert from 'node:assert/strict';
import {setupSectionNavigation} from './navigation.mjs';

function classList() {
  const names = new Set();
  return {
    contains: name => names.has(name),
    toggle(name, enabled) {
      if (enabled) names.add(name);
      else names.delete(name);
    },
  };
}

function eventTarget(attributes = {}) {
  const listeners = new Map();
  return {
    attributes: {...attributes},
    classList: classList(),
    addEventListener(type, listener) { listeners.set(type, listener); },
    removeEventListener(type, listener) {
      if (listeners.get(type) === listener) listeners.delete(type);
    },
    dispatch(type, event = {}) { listeners.get(type)?.({...event, currentTarget: this, target: this}); },
    getAttribute(name) { return this.attributes[name] ?? null; },
    setAttribute(name, value) { this.attributes[name] = String(value); },
    removeAttribute(name) { delete this.attributes[name]; },
  };
}

function fixture(hash = '') {
  const labels = {dashboard: 'Dashboard', incidents: 'Incidents', history: 'Service history', settings: 'Settings'};
  const links = Object.entries(labels).map(([view, label]) => ({
    ...eventTarget({'data-view-link': view, href: `#${view}`}),
    dataset: {view, viewLink: view},
    textContent: label,
  }));
  const views = Object.keys(labels).map(view => ({
    ...eventTarget({'data-view': view}),
    dataset: {view},
    hidden: false,
  }));
  const main = {...eventTarget({id: 'app-main'}), focused: 0, focus() { this.focused += 1; }};
  const document = {
    title: 'DebridUp',
    querySelectorAll(selector) {
      if (selector === '[data-view-link]') return links;
      if (selector === '[data-view]') return views;
      return [];
    },
    getElementById(id) { return id === 'app-main' ? main : null; },
  };
  const listeners = new Map();
  const window = {
    location: {hash},
    addEventListener(type, listener) { listeners.set(type, listener); },
    removeEventListener(type, listener) {
      if (listeners.get(type) === listener) listeners.delete(type);
    },
    dispatch(type) { listeners.get(type)?.(); },
  };
  return {document, window, links, views, main, labels};
}

function activeView(views) {
  return views.find(view => !view.hidden);
}

test('uses the dashboard for an empty or invalid hash and exposes one view', () => {
  for (const hash of ['', '#missing', '#providers']) {
    const {document, window, links, views} = fixture(hash);
    const navigation = setupSectionNavigation({document, window});
    assert.equal(activeView(views).dataset.view, 'dashboard');
    assert.equal(views.filter(view => !view.hidden).length, 1);
    assert.equal(activeView(views).getAttribute('aria-hidden'), 'false');
    assert.equal(links[0].classList.contains('active'), true);
    assert.equal(links[0].getAttribute('aria-current'), 'page');
    assert.equal(document.title, 'Dashboard · DebridUp');
    navigation.stop();
  }
});

test('activates the hash-selected view, updates the title, and focuses main content', () => {
  const {document, window, links, views, main} = fixture('#history');
  const navigation = setupSectionNavigation({document, window});
  assert.equal(activeView(views).dataset.view, 'history');
  assert.equal(links[2].classList.contains('active'), true);
  assert.equal(document.title, 'Service history · DebridUp');

  window.location.hash = '#settings';
  window.dispatch('hashchange');
  assert.equal(activeView(views).dataset.view, 'settings');
  assert.equal(links[3].getAttribute('aria-current'), 'page');
  assert.equal(document.title, 'Settings · DebridUp');
  assert.equal(main.focused, 1);
  navigation.stop();
});

test('current-route clicks focus main content and stop removes route handlers', () => {
  const {document, window, links, views, main} = fixture('#incidents');
  const navigation = setupSectionNavigation({document, window});
  let prevented = 0;
  links[1].dispatch('click', {preventDefault() { prevented += 1; }});
  assert.equal(activeView(views).dataset.view, 'incidents');
  assert.equal(prevented, 1);
  assert.equal(main.focused, 1);

  navigation.stop();
  links[1].dispatch('click', {preventDefault() { prevented += 1; }});
  window.location.hash = '#settings';
  window.dispatch('hashchange');
  assert.equal(activeView(views).dataset.view, 'incidents');
  assert.equal(prevented, 1);
});
