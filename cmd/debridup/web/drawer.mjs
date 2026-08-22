import {escapeHTML, formatState} from './dashboard-model.mjs';

const FOCUSABLE_SELECTOR = [
  'a[href]',
  'button:not([disabled])',
  'input:not([disabled])',
  'select:not([disabled])',
  'textarea:not([disabled])',
  '[tabindex]:not([tabindex="-1"])',
].join(',');
const SAFE_STATES = new Set(['healthy', 'auth_failed', 'api_issue', 'connection_issue', 'checking', 'unknown']);

export function createProviderDrawer(root) {
  if (!root) throw new Error('provider drawer root is required');
  const document = root.ownerDocument;
  const overlay = document?.getElementById('provider-drawer-overlay');
  const closeButton = root.querySelector('#provider-drawer-close');
  const title = root.querySelector('#provider-drawer-title');
  const state = root.querySelector('[data-drawer-state]');
  const incidentList = root.querySelector('.drawer-incident-list');
  let trigger = null;
  let openState = false;

  function setValue(name, value) {
    const target = root.querySelector(`[data-drawer-value="${name}"]`);
    if (target) target.textContent = value || '—';
  }

  function close() {
    if (!openState) return;
    openState = false;
    root.hidden = true;
    root.setAttribute('aria-hidden', 'true');
    if (overlay) overlay.hidden = true;
    document?.body?.classList?.remove('drawer-open');
    const capturedTrigger = trigger;
    trigger = null;
    capturedTrigger?.focus?.();
  }

  function open(provider = {}, nextTrigger = null) {
    trigger = nextTrigger;
    openState = true;
    const rawState = SAFE_STATES.has(provider.state) ? provider.state : 'unknown';
    if (title) title.textContent = provider.name || 'Provider';
    if (state) {
      state.className = `state ${rawState}`;
      state.textContent = provider.stateLabel || formatState(provider.state);
    }
    setValue('state-duration', provider.stateDurationLabel);
    setValue('availability', provider.availabilityLabel);
    setValue('p50', provider.p50Label);
    setValue('p95', provider.p95Label);
    setValue('last-check', provider.lastCheckLabel);
    setValue('slowest', provider.slowestLabel);
    setValue('latest-event', provider.latestEvent);
    if (incidentList) {
      const incidents = Array.isArray(provider.incidents) ? provider.incidents : [];
      incidentList.innerHTML = incidents.length
        ? incidents.map(incident => `<li><strong>${escapeHTML(incident.summary || 'Provider event')}</strong><span>${escapeHTML(incident.openedAtLabel || '')}</span></li>`).join('')
        : '<li class="muted">No incidents in this range.</li>';
    }
    if (overlay) overlay.hidden = false;
    root.hidden = false;
    root.setAttribute('aria-hidden', 'false');
    document?.body?.classList?.add('drawer-open');
    closeButton?.focus?.();
  }

  function onKeydown(event) {
    if (!openState) return;
    if (event.key === 'Escape') {
      event.preventDefault();
      close();
      return;
    }
    if (event.key !== 'Tab') return;
    const focusable = Array.from(root.querySelectorAll(FOCUSABLE_SELECTOR));
    if (focusable.length === 0) {
      event.preventDefault();
      root.focus?.();
      return;
    }
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (event.shiftKey && event.target === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && event.target === last) {
      event.preventDefault();
      first.focus();
    }
  }

  function destroy() {
    close();
    closeButton?.removeEventListener('click', close);
    overlay?.removeEventListener('click', close);
    root.removeEventListener('keydown', onKeydown);
  }

  closeButton?.addEventListener('click', close);
  overlay?.addEventListener('click', close);
  root.addEventListener('keydown', onKeydown);

  return {open, close, destroy};
}
