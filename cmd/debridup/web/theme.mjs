export const THEMES = Object.freeze({
  graphite: 'Graphite',
  'neo-tokyo': 'Neo Tokyo',
  sakura: 'Sakura',
  terminal: 'Terminal',
});

const STORAGE_KEY = 'debridup-theme';

export function normalizeTheme(value) {
  return Object.hasOwn(THEMES, value) ? value : 'graphite';
}

export function storedTheme(storage) {
  try {
    return normalizeTheme(storage?.getItem(STORAGE_KEY));
  } catch {
    return 'graphite';
  }
}

export function applyTheme(document, value) {
  const theme = normalizeTheme(value);
  if (document?.documentElement) document.documentElement.dataset.theme = theme;
  return theme;
}

export function setupThemePicker({document, storage} = {}) {
  const picker = document?.getElementById('theme-select');
  const current = applyTheme(document, document?.documentElement?.dataset?.theme || storedTheme(storage));
  if (!picker) return {theme: current, stop() {}};

  picker.value = current;
  const changeTheme = event => {
    const theme = applyTheme(document, event?.target?.value);
    picker.value = theme;
    try {
      storage?.setItem(STORAGE_KEY, theme);
    } catch {
      // A blocked storage API should not prevent an in-session theme change.
    }
  };
  picker.addEventListener('change', changeTheme);
  return {
    get theme() { return normalizeTheme(document.documentElement.dataset.theme); },
    stop() { picker.removeEventListener('change', changeTheme); },
  };
}
