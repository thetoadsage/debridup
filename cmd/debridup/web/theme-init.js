(() => {
  const themes = new Set(['graphite', 'neo-tokyo', 'sakura', 'terminal', 'porcelain', 'ocean', 'ember', 'moss', 'ascii', 'matrix']);
  let theme = 'graphite';
  try {
    const saved = localStorage.getItem('debridup-theme');
    if (themes.has(saved)) theme = saved;
  } catch {
    // Keep the default when browser storage is unavailable.
  }
  document.documentElement.dataset.theme = theme;
})();
