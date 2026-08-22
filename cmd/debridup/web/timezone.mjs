export const AUTO_TIME_ZONE = 'browser';
const STORAGE_KEY = 'debridup-time-zone';

export function browserTimeZone(intl = Intl) {
  try {
    return intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
  } catch {
    return 'UTC';
  }
}

export function normalizeTimeZone(value, intl = Intl) {
  if (value === AUTO_TIME_ZONE) return AUTO_TIME_ZONE;
  try {
    new intl.DateTimeFormat('en-US', {timeZone: value}).format();
    return value;
  } catch {
    return AUTO_TIME_ZONE;
  }
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

export function formatTimestamp(value, timeZone = AUTO_TIME_ZONE, intl = Intl) {
  if (!Number.isFinite(value)) return 'No checks yet';
  return new intl.DateTimeFormat('en-US', {
    timeZone: resolveTimeZone(timeZone, intl),
    timeZoneName: 'short',
    month: 'short',
    day: 'numeric',
    year: 'numeric',
    hour: 'numeric',
    minute: '2-digit',
    hour12: true,
  }).format(new Date(value * 1000));
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
