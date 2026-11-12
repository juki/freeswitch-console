// Entry point: session, application shell (sidebar), hash routing and the
// shared instance list.

import { api, onUnauthorized, isAbort } from './api.js';
import { h } from './dom.js';
import { icon, brandMark } from './icons.js';
import { Scope } from './scope.js';
import { closeDrawer, emptyState, toastError } from './ui.js';
import { onLangChange, t } from './i18n.js';
import { mountLogin } from './views/login.js';
import { mountFleet } from './views/fleet.js';
import { mountInstance, normalizeTab } from './views/instance.js';
import { mountUsers } from './views/users.js';
import { mountAudit } from './views/audit.js';
import { accountMenu } from './views/account.js';

const app = document.getElementById('app');

// What each role may do. Views ask ctx.can(capability); the server enforces
// the same rules and answers 403 otherwise.
//   logs     the live log tail
//   operate  hang up channels, reload XML/ACL, rescan SIP profiles
//   console  the API console
//   manage   add, edit and delete instances
//   users    user accounts
//   audit    the audit log
const CAPABILITIES = {
  viewer: [],
  operator: ['logs', 'operate'],
  admin: ['logs', 'operate', 'console', 'manage', 'users', 'audit'],
};

let me = null; // {username, role, managed}
let version = '';
let appScope = null; // lives while signed in
let viewScope = null;
let current = null; // {kind, id, tab, view}
let shell = null; // {main, list, count, fleetLink, adminLinks}
let instances = [];
let loaded = false;
let refresh = null;
const listeners = new Set();

// ctx is what views get to know about the session and the instance list.
const ctx = {
  can(capability) {
    return Boolean(me) && (CAPABILITIES[me.role] || []).includes(capability);
  },
  get me() {
    return me;
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

function buildShell() {
  const list = h('div', { class: 'side-list' });
  const count = h('span', { class: 'count' });
  const fleetLink = h('a', { class: 'side-link', href: '#/', title: t('nav.all') }, icon('grid'), h('span', { class: 'name', text: t('nav.all') }));
  const adminLinks = {};
  const admin = [];
  if (ctx.can('users')) {
    adminLinks.users = h('a', { class: 'side-link', href: '#/admin/users', title: t('nav.users') }, icon('users'), h('span', { class: 'name', text: t('nav.users') }));
    admin.push(adminLinks.users);
  }
  if (ctx.can('audit')) {
    adminLinks.audit = h('a', { class: 'side-link', href: '#/admin/audit', title: t('nav.audit') }, icon('shield'), h('span', { class: 'name', text: t('nav.audit') }));
    admin.push(adminLinks.audit);
  }
  const main = h('main', { class: 'main' });
  const side = h('aside', { class: 'side' },
    h('a', { class: 'brand', href: '#/' }, brandMark(), h('span', { text: 'fsdeck' }), h('span', { class: 'brand-tag', text: t('brand.tag') })),
    h('nav', { class: 'side-scroll', 'aria-label': t('nav.main') },
      fleetLink,
      h('div', { class: 'side-label' }, t('nav.instances'), count),
      list,
      admin.length ? h('div', { class: 'side-label' }, t('nav.admin')) : null,
      admin.length ? h('div', { class: 'side-list admin-list' }, admin) : null),
    h('div', { class: 'side-foot' }, accountMenu({ me, version, onSignOut: logout })));
  app.replaceChildren(h('div', { class: 'shell' }, side, main));
  shell = { main, list, count, fleetLink, adminLinks };
}

function renderSide() {
  if (!shell) return;
  const activeId = current && current.kind === 'instance' ? current.id : null;
  const tab = current && current.kind === 'instance' ? current.tab : 'overview';
  const mark = (el, on) => (on ? el.setAttribute('aria-current', 'page') : el.removeAttribute('aria-current'));
  mark(shell.fleetLink, current && current.kind === 'fleet');
  for (const [key, el] of Object.entries(shell.adminLinks)) mark(el, current && current.kind === 'admin' && current.page === key);
  shell.count.textContent = instances.length ? String(instances.length) : '';
  if (!instances.length) {
    shell.list.replaceChildren(h('div', { class: 'side-empty', text: loaded ? t('nav.none') : t('common.loadingDots') }));
    return;
  }
  shell.list.replaceChildren(...instances.map((inst) => {
    const ok = Boolean(inst.state && inst.state.connected);
    return h('a', {
      class: 'side-link',
      href: '#/i/' + encodeURIComponent(inst.id) + '/' + tab,
      title: ok ? inst.name : t('nav.instDown', { name: inst.name }),
      'aria-current': inst.id === activeId ? 'page' : undefined,
    }, h('span', { class: ok ? 'dot ok' : 'dot bad' }), h('span', { class: 'name', text: inst.name }), ok ? null : h('span', { class: 'side-meta', text: t('nav.down') }));
  }));
}

// ---------- routing ----------

function teardown() {
  closeDrawer();
  if (viewScope) viewScope.dispose();
  viewScope = null;
  current = null;
}

const ADMIN_PAGES = { users: { cap: 'users', mount: mountUsers }, audit: { cap: 'audit', mount: mountAudit } };

function parseRoute() {
  const hash = location.hash.replace(/^#/, '') || '/';
  if (hash === '/') return { kind: 'fleet' };
  const a = hash.match(/^\/admin\/(\w+)\/?$/);
  if (a && ADMIN_PAGES[a[1]]) return { kind: 'admin', page: a[1] };
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
  if (r.kind === 'unknown' || (r.kind === 'admin' && !ctx.can(ADMIN_PAGES[r.page].cap))) {
    history.replaceState(null, '', '#/');
    route();
    return;
  }
  if (r.kind === 'instance') {
    const tab = normalizeTab(r.tab, ctx);
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
  } else if (r.kind === 'admin') {
    teardown();
    viewScope = new Scope();
    current = { kind: 'admin', page: r.page };
    ADMIN_PAGES[r.page].mount({ scope: viewScope, root: newPage(), ctx });
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
  document.title = t('login.docTitle');
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
    if (err.status !== 401) toastError(err, t('nav.signOutFailed'));
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
  // The version only decorates the account menu; without it the menu says less.
  if (!version) {
    api('GET', 'version', { signal: appScope.signal }).then((data) => {
      if (!data || typeof data.version !== 'string' || !data.version || !shell) return;
      version = data.version;
      const foot = shell.main.parentNode.querySelector('.side-foot');
      if (foot) foot.replaceChildren(accountMenu({ me, version, onSignOut: logout }));
    }, () => {});
  }
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
      icon: 'alert', tone: 'error', title: t('boot.unreachable'), text: err.message,
      action: h('button', { type: 'button', class: 'btn', text: t('common.retry'), on: { click: boot } }),
    })));
    return;
  }
  start();
}

onUnauthorized(showLogin);
window.addEventListener('hashchange', route);
// A language change re-renders the current screen in place, on the same route.
onLangChange(() => {
  if (!me) {
    current = null;
    showLogin();
    return;
  }
  if (!shell) return;
  teardown();
  buildShell();
  route();
});
boot();
