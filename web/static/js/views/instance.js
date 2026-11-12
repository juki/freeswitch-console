// Instance page: header with connection state, tab bar, one mounted tab.

import { h, sinceText } from '../dom.js';
import { icon } from '../icons.js';
import { emptyState, pill } from '../ui.js';
import { address } from './fleet.js';
import { mountOverview } from './overview.js';
import { mountSip } from './sip.js';
import { mountCalls } from './calls.js';
import { mountConfig } from './config.js';
import { mountLogs } from './logs.js';
import { mountConsole } from './console.js';

const TABS = [
  { key: 'overview', label: 'Overview', icon: 'activity', mount: mountOverview },
  { key: 'sip', label: 'SIP', icon: 'radio', mount: mountSip },
  { key: 'calls', label: 'Calls', icon: 'calls', mount: mountCalls },
  { key: 'config', label: 'Configuration', icon: 'sliders', mount: mountConfig },
  { key: 'logs', label: 'Logs', icon: 'logs', admin: true, mount: mountLogs },
  { key: 'console', label: 'Console', icon: 'terminal', admin: true, mount: mountConsole },
];

// normalizeTab maps an unknown or not-permitted tab to "overview".
export function normalizeTab(tab, isAdmin) {
  const t = TABS.find((x) => x.key === tab);
  return t && (isAdmin || !t.admin) ? tab : 'overview';
}

export function mountInstance({ scope, root, id, ctx }) {
  const base = '#/i/' + encodeURIComponent(id) + '/';
  const crumb = h('span', { text: '…' });
  const title = h('h1', { class: 'page-title', text: '…' });
  const glyphDot = h('span', { class: 'dot' });
  const meta = h('div', { class: 'title-meta' });
  const links = new Map();
  const tabs = TABS.filter((t) => ctx.isAdmin || !t.admin);
  let tabScope = null;
  let currentTab = null;
  let missing = false;
  let addr = '';
  let lastState = null;

  const nav = h('nav', { class: 'tabs', 'aria-label': 'Instance sections' }, tabs.map((t) => {
    const a = h('a', { class: 'tab', href: base + t.key }, icon(t.icon), t.label);
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
  const body = h('div', { class: 'page-body' });

  root.replaceChildren(
    h('header', { class: 'masthead' },
      h('div', { class: 'masthead-in' },
        h('div', { class: 'crumbs' }, h('a', { href: '#/', text: 'Instances' }), icon('chevron'), crumb),
        h('div', { class: 'title-block' },
          h('span', { class: 'inst-glyph', 'aria-hidden': 'true' }, icon('server'), glyphDot),
          h('div', { class: 'spacer' }, title, meta)),
        nav)),
    body);

  function paintMeta() {
    const st = lastState;
    const ok = Boolean(st && st.connected);
    const since = st ? sinceText(st.since) : '';
    const sep = () => h('span', { class: 'sep' });
    glyphDot.className = st ? (ok ? 'dot ok' : 'dot bad') : 'dot';
    meta.replaceChildren(...[
      addr ? h('span', { class: 'mono', text: addr }) : null,
      st ? sep() : null,
      st ? pill(ok ? 'Connected' : 'Disconnected', ok ? 'ok' : 'bad') : null,
      since ? h('span', { text: 'for ' + since }) : null,
      !ok && st && st.lastError ? sep() : null,
      !ok && st && st.lastError ? h('span', { class: 'err mono', text: st.lastError }) : null,
    ].filter(Boolean));
  }

  function paintState(st) {
    if (!st || typeof st !== 'object') return;
    lastState = st;
    paintMeta();
  }

  // setState is called by tabs with states from the event stream.
  function setState(st) {
    paintState(st);
    ctx.patchState(id, st);
  }

  function sync() {
    if (!ctx.loaded()) return;
    const inst = ctx.instances().find((i) => i.id === id);
    if (!inst) {
      if (missing) return;
      missing = true;
      if (tabScope) tabScope.dispose();
      document.title = 'Not found – fsdeck';
      root.replaceChildren(h('div', { class: 'page-body' }, h('div', { class: 'panel' }, emptyState({
        icon: 'server', title: 'Instance not found',
        text: 'There is no instance with this id. It may have been deleted.',
        action: h('a', { class: 'btn', href: '#/', text: 'Back to instances' }),
      }))));
      return;
    }
    crumb.textContent = inst.name;
    title.textContent = inst.name;
    addr = address(inst);
    paintMeta();
    document.title = inst.name + ' – fsdeck';
    paintState(inst.state);
  }

  scope.onCleanup(ctx.onInstances(sync));
  scope.onCleanup(() => {
    document.title = 'fsdeck';
  });
  sync();

  function showTab(key) {
    if (missing) return;
    currentTab = key;
    for (const [k, a] of links) {
      if (k === key) a.setAttribute('aria-current', 'page');
      else a.removeAttribute('aria-current');
    }
    if (tabScope) tabScope.dispose();
    tabScope = scope.child();
    body.replaceChildren();
    TABS.find((t) => t.key === key).mount({ id, ctx, isAdmin: ctx.isAdmin, scope: tabScope, root: body, setState });
  }

  return { showTab, get tab() { return currentTab; } };
}
