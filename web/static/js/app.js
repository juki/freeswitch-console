// Entry point: session, application shell (sidebar), hash routing and the
// shared instance list.

import { api, onUnauthorized, isAbort } from './api.js';
import { h } from './dom.js';
import { icon, brandMark } from './icons.js';
import { Scope } from './scope.js';
import { closeDrawer, confirmLeave, emptyState, hasUnsaved, toastError } from './ui.js';
import { onLangChange, t } from './i18n.js';
import { mountLogin } from './views/login.js';
import { mountFleet } from './views/fleet.js';
import { mountInstance, normalizeTab } from './views/instance.js';
import { mountUsers } from './views/users.js';
import { mountAudit } from './views/audit.js';
import { accountMenu, SOURCE_URL } from './views/account.js';

const app = document.getElementById('app');

// What each role may do. Views ask ctx.access(capability) — 'ok' or 'role'
// (this role cannot) — or ctx.can() for a plain yes. The server enforces the
// same rules (403 for the role).
//   logs     the live log tail
//   operate  hang up, reload XML/ACL, rescan profiles, register gateways
//   control  the admin-only runtime operations: profile start/stop/restart,
//            gateway removal, modules, switch settings, global variables
//   console  the API console
//   manage   add, edit and delete instances
//   users    change user accounts
//   audit    the audit log
//   files    edit configuration files
const CAPABILITIES = {
  viewer: [],
  operator: ['logs', 'operate'],
  admin: ['logs', 'operate', 'control', 'console', 'manage', 'users', 'audit', 'files'],
};

let me = null; // {username, role, managed}
let version = '';
let demo = false; // api/version says so: simulated switches, nobody signs in
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
  access(capability) {
    if (!me || !(CAPABILITIES[me.role] || []).includes(capability)) return 'role';
    return 'ok';
  },
  can(capability) {
    return ctx.access(capability) === 'ok';
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
  // setHash records where the user is inside a view (a sub-tab, an open
  // item or file) without re-routing, so a reload or a shared link comes back
  // to the same place.
  setHash(hash) {
    if (location.hash === hash) return;
    history.replaceState(null, '', hash);
    lastHash = hash;
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
  if (ctx.access('users') !== 'role') {
    adminLinks.users = h('a', { class: 'side-link', href: '#/admin/users', title: t('nav.users') }, icon('users'), h('span', { class: 'name', text: t('nav.users') }));
    admin.push(adminLinks.users);
  }
  if (ctx.access('audit') !== 'role') {
    adminLinks.audit = h('a', { class: 'side-link', href: '#/admin/audit', title: t('nav.audit') }, icon('shield'), h('span', { class: 'name', text: t('nav.audit') }));
    admin.push(adminLinks.audit);
  }
  const main = h('main', { class: 'main' });
  const foot = h('div', { class: 'side-foot' }, accountMenu({ me, version, onSignOut: demo ? null : logout }));
  const side = h('aside', { class: 'side' },
    h('a', { class: 'brand', href: '#/' }, brandMark(), h('span', { text: 'FreeSWITCH' }), h('span', { class: 'brand-tag', text: t('brand.tag') })),
    h('nav', { class: 'side-scroll', 'aria-label': t('nav.main') },
      fleetLink,
      h('div', { class: 'side-label' }, t('nav.instances'), count),
      list,
      admin.length ? h('div', { class: 'side-label' }, t('nav.admin')) : null,
      admin.length ? h('div', { class: 'side-list admin-list' }, admin) : null),
    foot);
  const col = h('div', { class: 'main-col' }, demo ? demoBanner() : null, main);
  app.replaceChildren(h('div', { class: 'shell' }, side, col));
  shell = { main, list, count, fleetLink, adminLinks, foot, col };
}

// demoBanner tops the main column on every page of the demo.
function demoBanner() {
  return h('div', { class: 'demo-banner', role: 'note' }, icon('info'),
    h('p', null, t('demo.banner'), ' ', h('a', { href: SOURCE_URL, target: '_blank', rel: 'noopener noreferrer', text: t('demo.link') })));
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
  // #/i/<id>/<tab>[/<sub>][?query] — see nav.js
  const m = hash.match(/^\/i\/([^/?]+)(?:\/([^/?]*))?(?:\/([^/?]+))?\/?(?:\?(.*))?$/);
  if (!m) return { kind: 'unknown' };
  try {
    return { kind: 'instance', id: decodeURIComponent(m[1]), tab: m[2] || '', sub: m[3] || '', params: new URLSearchParams(m[4] || '') };
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

let lastHash = location.hash;

// onHash runs on every hash change: unsaved edits are confirmed first, and
// the address goes back to where it was until the user agrees to leave.
function onHash() {
  const target = location.hash;
  if (target === lastHash) return;
  if (!hasUnsaved()) {
    lastHash = target;
    route();
    return;
  }
  history.replaceState(null, '', lastHash || '#/');
  confirmLeave().then((ok) => {
    if (!ok) return;
    lastHash = target;
    history.replaceState(null, '', target);
    route();
  });
}

function route() {
  if (!me || !shell) return;
  const r = parseRoute();
  if (r.kind === 'unknown' || (r.kind === 'admin' && ctx.access(ADMIN_PAGES[r.page].cap) === 'role')) {
    history.replaceState(null, '', '#/');
    route();
    return;
  }
  if (r.kind === 'instance') {
    const tab = normalizeTab(r.tab, ctx);
    const nav = tab === r.tab ? { sub: r.sub, params: r.params } : { sub: '', params: new URLSearchParams() };
    if (tab !== r.tab) history.replaceState(null, '', '#/i/' + encodeURIComponent(r.id) + '/' + tab);
    if (current && current.kind === 'instance' && current.id === r.id) {
      closeDrawer();
      current.tab = tab;
      current.view.showTab(tab, nav);
    } else {
      teardown();
      viewScope = new Scope();
      const view = mountInstance({ scope: viewScope, root: newPage(), id: r.id, ctx });
      current = { kind: 'instance', id: r.id, tab, view };
      view.showTab(tab, nav);
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
  lastHash = location.hash;
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
  if (!(await confirmLeave())) return;
  try {
    await api('POST', 'logout');
    showLogin();
  } catch (err) {
    if (err.status !== 401) toastError(err, t('nav.signOutFailed'));
  }
}

async function start() {
  appScope = new Scope();
  buildShell();
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
  // The version decorates the account menu (without it the menu says less)
  // and tells whether this is the demo (banner, no Sign out).
  if (!version) {
    api('GET', 'version', { signal: appScope.signal }).then((data) => {
      if (!data || typeof data.version !== 'string' || !data.version || !shell) return;
      version = data.version;
      demo = data.demo === true;
      if (demo) shell.col.prepend(demoBanner());
      shell.foot.replaceChildren(accountMenu({ me, version, onSignOut: demo ? null : logout }));
    }, () => {});
  }
  lastHash = location.hash;
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
window.addEventListener('hashchange', onHash);
// Closing or reloading the page with unsaved edits asks the browser's own question.
window.addEventListener('beforeunload', (ev) => {
  if (!hasUnsaved()) return;
  ev.preventDefault();
  ev.returnValue = '';
});
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
