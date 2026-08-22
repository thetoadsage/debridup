(() => {
  const themes = new Set(['graphite', 'neo-tokyo', 'sakura', 'terminal']);
  let theme = 'graphite';
  try {
    const saved = localStorage.getItem('debridup-theme');
    if (themes.has(saved)) theme = saved;
  } catch {
    // Keep the default when browser storage is unavailable.
  }
  document.documentElement.dataset.theme = theme;
})();
