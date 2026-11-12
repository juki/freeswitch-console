// Instance shell: header with connection badge, tab bar, one mounted tab.

import { api, isAbort } from '../api.js';
import { h, stateView } from '../dom.js';
import { mountOverview } from './overview.js';
import { mountSip } from './sip.js';
import { mountCalls } from './calls.js';
import { mountConfig } from './config.js';
import { mountLogs } from './logs.js';
import { mountConsole } from './console.js';

const TABS = [
  { key: 'overview', label: 'Overview', admin: false, mount: mountOverview },
  { key: 'sip', label: 'SIP', admin: false, mount: mountSip },
  { key: 'calls', label: 'Calls', admin: false, mount: mountCalls },
  { key: 'config', label: 'Config', admin: false, mount: mountConfig },
  { key: 'logs', label: 'Logs', admin: true, mount: mountLogs },
  { key: 'console', label: 'Console', admin: true, mount: mountConsole },
];

// normalizeTab maps an unknown or not-permitted tab to "overview".
export function normalizeTab(tab, isAdmin) {
  const t = TABS.find((x) => x.key === tab);
  return t && (isAdmin || !t.admin) ? tab : 'overview';
}

export function mountInstance({ scope, root, id, isAdmin }) {
  const base = '#/i/' + encodeURIComponent(id) + '/';
  const title = h('h1', { class: 'inst-title', text: 'Instance' });
  const addr = h('span', { class: 'mono muted' });
  const state = h('span', { class: 'state-cell' });
  const tabs = TABS.filter((t) => isAdmin || !t.admin);
  const links = new Map();
  let tabScope = null;

  const nav = h('nav', { class: 'tabs', 'aria-label': 'Instance sections' }, tabs.map((t) => {
    const a = h('a', { class: 'tab', href: base + t.key, text: t.label });
    // Clicking the current tab remounts it (reloads its data).
    a.addEventListener('click', (ev) => {
      if (location.hash === base + t.key) {
        ev.preventDefault();
        showTab(t.key);
      }
    });
    links.set(t.key, a);
    return a;
  }));
  const body = h('section', { class: 'tab-body' });

  root.replaceChildren(
    h('div', { class: 'inst-head' }, title, addr, state),
    nav,
    body);

  function setState(st) {
    if (st && typeof st === 'object') state.replaceChildren(...stateView(st).filter(Boolean));
  }

  async function loadInfo() {
    try {
      const list = await api('GET', 'instances', { signal: scope.signal });
      const inst = Array.isArray(list) ? list.find((i) => i.id === id) : null;
      if (!inst) {
        if (tabScope) tabScope.dispose();
        root.replaceChildren(
          h('h1', { text: 'Instance not found' }),
          h('p', null, 'There is no instance with id ', h('code', { text: id }), '. ',
            h('a', { href: '#/', text: 'Back to instances' })));
        return;
      }
      title.textContent = inst.name;
      addr.textContent = inst.host + ':' + inst.port;
      document.title = inst.name + ' – fsdeck';
      setState(inst.state);
    } catch (err) {
      if (!isAbort(err)) state.replaceChildren(h('span', { class: 'last-error', text: err.message }));
    }
  }

  scope.onCleanup(() => {
    document.title = 'fsdeck';
  });

  function showTab(key) {
    for (const [k, a] of links) {
      if (k === key) a.setAttribute('aria-current', 'page');
      else a.removeAttribute('aria-current');
    }
    if (tabScope) tabScope.dispose();
    tabScope = scope.child();
    body.replaceChildren();
    const tab = TABS.find((t) => t.key === key);
    tab.mount({ id, isAdmin, scope: tabScope, root: body, setState });
    loadInfo();
  }

  return { showTab };
}
