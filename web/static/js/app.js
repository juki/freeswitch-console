// Entry point: session, application shell (sidebar), hash routing and the
// shared instance list.

import { api, onUnauthorized, isAbort } from './api.js';
import { h } from './dom.js';
import { icon, brandMark } from './icons.js';
import { Scope } from './scope.js';
import { closeDrawer, emptyState, iconButton, toastError } from './ui.js';
import { mountLogin } from './views/login.js';
import { mountFleet } from './views/fleet.js';
import { mountInstance, normalizeTab } from './views/instance.js';

const app = document.getElementById('app');

let me = null; // {username, role}
let appScope = null; // lives while signed in
let viewScope = null;
let current = null; // {kind, id, tab, view}
let shell = null; // {main, list, count, fleetLink}
let instances = [];
let loaded = false;
let refresh = null;
const listeners = new Set();

// ctx is what views get to know about the session and the instance list.
const ctx = {
  get isAdmin() {
    return Boolean(me) && me.role === 'admin';
  },
  instances: () => instances,
  loaded: () => loaded,
  onInstances(fn) {
    listeners.add(fn);
    return () => listeners.delete(fn);
  },
  refreshInstances() {
    if (refresh) refresh();
  },
  // patchState applies a connection state learned from an event stream, so
  // the sidebar does not wait for the next poll.
  patchState(id, state) {
    const inst = instances.find((i) => i.id === id);
    if (!inst || !state || (inst.state && inst.state.connected === state.connected && inst.state.lastError === state.lastError)) return;
    inst.state = state;
    publish();
  },
};

function publish() {
  renderSide();
  for (const fn of [...listeners]) fn(instances);
}

// ---------- shell ----------

const THEMES = { auto: ['contrast', 'Theme: follow system'], light: ['sun', 'Theme: light'], dark: ['moon', 'Theme: dark'] };

function themeButton() {
  const btn = h('button', { type: 'button', class: 'btn btn-ghost btn-icon btn-sm' });
  const paint = () => {
    const [name, label] = THEMES[window.fsdeckTheme.get()] || THEMES.auto;
    btn.replaceChildren(icon(name));
    btn.title = label;
    btn.setAttribute('aria-label', label);
  };
  btn.addEventListener('click', () => {
    const order = ['auto', 'light', 'dark'];
    window.fsdeckTheme.set(order[(order.indexOf(window.fsdeckTheme.get()) + 1) % order.length]);
    paint();
  });
  paint();
  return btn;
}

function buildShell() {
  const list = h('div', { class: 'side-list' });
  const count = h('span', { class: 'count' });
  const fleetLink = h('a', { class: 'side-link', href: '#/', title: 'All instances' }, icon('grid'), h('span', { class: 'name', text: 'All instances' }));
  const main = h('main', { class: 'main' });
  const side = h('aside', { class: 'side' },
    h('a', { class: 'brand', href: '#/' }, brandMark(), h('span', { text: 'fsdeck' }), h('span', { class: 'brand-tag', text: 'Console' })),
    h('nav', { class: 'side-scroll', 'aria-label': 'Instances' },
      fleetLink,
      h('div', { class: 'side-label' }, 'Instances', count),
      list),
    h('div', { class: 'side-foot' },
      h('div', { class: 'avatar', text: me.username.slice(0, 1) }),
      h('div', { class: 'who' }, h('div', { class: 'who-name', text: me.username }), h('div', { class: 'who-role', text: me.role === 'admin' ? 'Administrator' : 'Read only' })),
      h('span', { class: 'spacer' }),
      themeButton(),
      iconButton('logout', 'Sign out', logout)));
  app.replaceChildren(h('div', { class: 'shell' }, side, main));
  shell = { main, list, count, fleetLink };
}

function renderSide() {
  if (!shell) return;
  const activeId = current && current.kind === 'instance' ? current.id : null;
  const tab = current && current.kind === 'instance' ? current.tab : 'overview';
  if (current && current.kind === 'fleet') shell.fleetLink.setAttribute('aria-current', 'page');
  else shell.fleetLink.removeAttribute('aria-current');
  shell.count.textContent = instances.length ? String(instances.length) : '';
  if (!instances.length) {
    shell.list.replaceChildren(h('div', { class: 'side-empty', text: loaded ? 'No instances yet' : 'Loading…' }));
    return;
  }
  shell.list.replaceChildren(...instances.map((inst) => {
    const ok = Boolean(inst.state && inst.state.connected);
    return h('a', {
      class: 'side-link',
      href: '#/i/' + encodeURIComponent(inst.id) + '/' + tab,
      title: inst.name + (ok ? '' : ' (disconnected)'),
      'aria-current': inst.id === activeId ? 'page' : undefined,
    }, h('span', { class: ok ? 'dot ok' : 'dot bad' }), h('span', { class: 'name', text: inst.name }), ok ? null : h('span', { class: 'side-meta', text: 'down' }));
  }));
}

// ---------- routing ----------

function teardown() {
  closeDrawer();
  if (viewScope) viewScope.dispose();
  viewScope = null;
  current = null;
}

function parseRoute() {
  const hash = location.hash.replace(/^#/, '') || '/';
  if (hash === '/') return { kind: 'fleet' };
  const m = hash.match(/^\/i\/([^/]+)(?:\/([^/]*))?\/?$/);
  if (!m) return { kind: 'unknown' };
  try {
    return { kind: 'instance', id: decodeURIComponent(m[1]), tab: m[2] || '' };
  } catch {
    return { kind: 'unknown' };
  }
}

function newPage() {
  const page = h('div', { class: 'page' });
  shell.main.replaceChildren(page);
  shell.main.scrollTop = 0;
  return page;
}

function route() {
  if (!me || !shell) return;
  const r = parseRoute();
  if (r.kind === 'unknown') {
    history.replaceState(null, '', '#/');
    route();
    return;
  }
  if (r.kind === 'instance') {
    const tab = normalizeTab(r.tab, ctx.isAdmin);
    if (tab !== r.tab) history.replaceState(null, '', '#/i/' + encodeURIComponent(r.id) + '/' + tab);
    if (current && current.kind === 'instance' && current.id === r.id) {
      closeDrawer();
      current.tab = tab;
      current.view.showTab(tab);
    } else {
      teardown();
      viewScope = new Scope();
      const view = mountInstance({ scope: viewScope, root: newPage(), id: r.id, ctx });
      current = { kind: 'instance', id: r.id, tab, view };
      view.showTab(tab);
    }
  } else {
    teardown();
    viewScope = new Scope();
    current = { kind: 'fleet' };
    mountFleet({ scope: viewScope, root: newPage(), ctx });
  }
  renderSide();
}

// ---------- session ----------

function showLogin() {
  if (!me && current && current.kind === 'login') return;
  teardown();
  if (appScope) appScope.dispose();
  appScope = null;
  refresh = null;
  shell = null;
  me = null;
  instances = [];
  loaded = false;
  current = { kind: 'login' };
  document.title = 'Sign in – fsdeck';
  mountLogin({
    root: app,
    onLogin(user) {
      me = user;
      current = null;
      start();
    },
  });
}

async function logout() {
  try {
    await api('POST', 'logout');
    showLogin();
  } catch (err) {
    if (err.status !== 401) toastError(err, 'Sign out failed');
  }
}

function start() {
  buildShell();
  appScope = new Scope();
  refresh = appScope.poll(async (signal) => {
    try {
      const data = await api('GET', 'instances', { signal });
      instances = Array.isArray(data) ? data : [];
      loaded = true;
      publish();
    } catch (err) {
      if (isAbort(err)) throw err;
      // keep the last known list; the next poll retries
    }
  }, 5000);
  route();
}

async function boot() {
  try {
    const user = await api('GET', 'me');
    if (!user || !user.role) throw new Error('unexpected answer from /api/me');
    me = user;
  } catch (err) {
    if (err.status === 401) return; // onUnauthorized already showed the login view
    app.replaceChildren(h('div', { class: 'boot' }, emptyState({
      icon: 'alert', tone: 'error', title: 'Could not reach fsdeck', text: err.message,
      action: h('button', { type: 'button', class: 'btn', text: 'Retry', on: { click: boot } }),
    })));
    return;
  }
  start();
}

onUnauthorized(showLogin);
window.addEventListener('hashchange', route);
boot();
