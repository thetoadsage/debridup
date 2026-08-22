function targetID(link) {
  const href = link?.getAttribute?.('href') || '';
  return href.startsWith('#') ? href.slice(1) : '';
}

export function setupSectionNavigation({document, window} = {}) {
  const links = Array.from(document?.querySelectorAll?.('.nav-link[href^="#"]') || []);
  const sections = links
    .map(link => ({link, id: targetID(link)}))
    .map(item => ({...item, section: document?.getElementById?.(item.id)}))
    .filter(item => item.id && item.section);
  if (!sections.length) return {stop() {}, sync() {}};
  let pendingID = null;

  function activationOffset() {
    return Number(window?.innerWidth) <= 760 ? 124 : 26;
  }

  function activate(id) {
    for (const item of sections) {
      const active = item.id === id;
      item.link.classList?.toggle('active', active);
      if (active) item.link.setAttribute?.('aria-current', 'page');
      else item.link.removeAttribute?.('aria-current');
    }
  }

  function activeSectionFromViewport() {
    const offset = activationOffset();
    let active = sections[0];
    for (const item of sections) {
      if (item.section.getBoundingClientRect?.().top <= offset) active = item;
      else break;
    }
    return active.id;
  }

  function sync() {
    const visibleID = activeSectionFromViewport();
    if (pendingID) {
      const pendingIndex = sections.findIndex(item => item.id === pendingID);
      const visibleIndex = sections.findIndex(item => item.id === visibleID);
      const pendingTop = sections[pendingIndex]?.section.getBoundingClientRect?.().top;
      if (pendingTop <= activationOffset() || visibleIndex > pendingIndex) {
        pendingID = null;
      } else {
        activate(pendingID);
        return;
      }
    }
    activate(visibleID);
  }

  function followLink(event) {
    const link = event?.currentTarget || event?.target;
    const id = targetID(link);
    if (id) {
      pendingID = id;
      activate(id);
    }
  }

  function followHash() {
    const id = String(window?.location?.hash || '').slice(1);
    if (sections.some(item => item.id === id)) {
      pendingID = id;
      activate(id);
    }
    else sync();
  }

  // sync() reads getBoundingClientRect() for every section, which forces
  // layout. Coalesce bursts of scroll events into one read per frame.
  let frame = null;
  function scheduleSync() {
    if (frame !== null) return;
    const request = window?.requestAnimationFrame;
    if (typeof request !== 'function') {
      sync();
      return;
    }
    frame = request.call(window, () => {
      frame = null;
      sync();
    });
  }

  for (const {link} of sections) link.addEventListener?.('click', followLink);
  window?.addEventListener?.('scroll', scheduleSync, {passive: true});
  window?.addEventListener?.('resize', scheduleSync);
  window?.addEventListener?.('hashchange', followHash);
  followHash();

  return {
    sync,
    stop() {
      for (const {link} of sections) link.removeEventListener?.('click', followLink);
      window?.removeEventListener?.('scroll', scheduleSync);
      window?.removeEventListener?.('resize', scheduleSync);
      window?.removeEventListener?.('hashchange', followHash);
      if (frame !== null) window?.cancelAnimationFrame?.(frame);
      frame = null;
    },
  };
}
