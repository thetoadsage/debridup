export const AUTO_TIME_ZONE = 'browser';
const STORAGE_KEY = 'debridup-time-zone';

const FORMAT_OPTIONS = Object.freeze({
  timeZoneName: 'short',
  month: 'short',
  day: 'numeric',
  year: 'numeric',
  hour: 'numeric',
  minute: '2-digit',
  hour12: true,
});

// Constructing an Intl.DateTimeFormat is orders of magnitude more expensive
// than formatting with an existing one, and the dashboard formats many check
// timestamps on every refresh. Cache per injected Intl object so a test double
// still gets its own formatters.
const formatterCache = new WeakMap();
const validationCache = new WeakMap();
const browserZoneCache = new WeakMap();

function cacheFor(store, intl) {
  let cache = store.get(intl);
  if (!cache) {
    cache = new Map();
    store.set(intl, cache);
  }
  return cache;
}

export function browserTimeZone(intl = Intl) {
  if (browserZoneCache.has(intl)) return browserZoneCache.get(intl);
  let zone;
  try {
    zone = intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
  } catch {
    zone = 'UTC';
  }
  browserZoneCache.set(intl, zone);
  return zone;
}

export function normalizeTimeZone(value, intl = Intl) {
  if (value === AUTO_TIME_ZONE) return AUTO_TIME_ZONE;
  const cache = cacheFor(validationCache, intl);
  if (cache.has(value)) return cache.get(value);
  let normalized;
  try {
    new intl.DateTimeFormat('en-US', {timeZone: value}).format();
    normalized = value;
  } catch {
    normalized = AUTO_TIME_ZONE;
  }
  cache.set(value, normalized);
  return normalized;
}

export function resolveTimeZone(value, intl = Intl) {
  const normalized = normalizeTimeZone(value, intl);
  return normalized === AUTO_TIME_ZONE ? browserTimeZone(intl) : normalized;
}

export function storedTimeZone(storage, intl = Intl) {
  try {
    return normalizeTimeZone(storage?.getItem(STORAGE_KEY), intl);
  } catch {
    return AUTO_TIME_ZONE;
  }
}

function formatterFor(zone, intl) {
  const cache = cacheFor(formatterCache, intl);
  let formatter = cache.get(zone);
  if (!formatter) {
    formatter = new intl.DateTimeFormat('en-US', {timeZone: zone, ...FORMAT_OPTIONS});
    cache.set(zone, formatter);
  }
  return formatter;
}

export function formatTimestamp(value, timeZone = AUTO_TIME_ZONE, intl = Intl) {
  if (!Number.isFinite(value)) return 'No checks yet';
  return formatterFor(resolveTimeZone(timeZone, intl), intl).format(new Date(value * 1000));
}

export function timeZoneDescription(value, intl = Intl) {
  const normalized = normalizeTimeZone(value, intl);
  return normalized === AUTO_TIME_ZONE
    ? `Using this browser’s local time (${browserTimeZone(intl)}).`
    : `Using ${normalized}.`;
}

export function setupTimeZonePicker({document, storage, onChange, intl = Intl} = {}) {
  const picker = document?.getElementById('time-zone');
  const detail = document?.getElementById('time-zone-detail');
  let timeZone = storedTimeZone(storage, intl);

  function render() {
    if (picker) picker.value = timeZone;
    if (detail) detail.textContent = timeZoneDescription(timeZone, intl);
  }

  function change(event) {
    timeZone = normalizeTimeZone(event?.target?.value, intl);
    try {
      storage?.setItem(STORAGE_KEY, timeZone);
    } catch {
      // Keep the in-session choice if browser storage is unavailable.
    }
    render();
    onChange?.(timeZone);
  }

  render();
  picker?.addEventListener('change', change);
  return {
    get timeZone() { return timeZone; },
    stop() { picker?.removeEventListener('change', change); },
  };
}
