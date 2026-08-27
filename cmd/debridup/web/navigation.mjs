export function setupSectionNavigation({document, window} = {}) {
  const views = Array.from(document?.querySelectorAll?.('[data-view]') || []);
  const links = Array.from(document?.querySelectorAll?.('[data-view-link]') || []);
  const main = document?.getElementById?.('app-main');
  const valid = new Set(views.map(view => view.dataset?.view));
  if (!views.length || !links.length) return {stop() {}, sync() {}, activeView: () => 'dashboard'};
  let current = null;
  const routeFromHash = () => {
    const route = String(window?.location?.hash || '').replace(/^#/, '');
    return valid.has(route) ? route : 'dashboard';
  };
  function activate(next, {focus = false} = {}) {
    current = valid.has(next) ? next : 'dashboard';
    for (const view of views) {
      const active = view.dataset?.view === current;
      view.hidden = !active;
      view.setAttribute?.('aria-hidden', String(!active));
    }
    for (const link of links) {
      const active = link.dataset?.viewLink === current;
      link.classList?.toggle('active', active);
      if (active) link.setAttribute?.('aria-current', 'page'); else link.removeAttribute?.('aria-current');
    }
    const label = links.find(link => link.dataset?.viewLink === current)?.textContent?.trim() || 'Dashboard';
    if (document) document.title = `${label} · DebridUp`;
    if (focus) main?.focus?.({preventScroll: true});
  }
  function sync({focus = false} = {}) { activate(routeFromHash(), {focus}); }
  function onHashChange() { sync({focus: true}); }
  function onClick(event) {
    const view = event?.currentTarget?.dataset?.viewLink;
    if (view && routeFromHash() === view) { event.preventDefault?.(); activate(view, {focus: true}); }
  }
  for (const link of links) link.addEventListener?.('click', onClick);
  window?.addEventListener?.('hashchange', onHashChange);
  sync();

  return {
    sync,
    activeView: () => current,
    stop() {
      for (const link of links) link.removeEventListener?.('click', onClick);
      window?.removeEventListener?.('hashchange', onHashChange);
    },
  };
}
