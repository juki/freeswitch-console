// Entry point: session, application shell (sidebar), hash routing and the
// shared instance list.

import { api, onUnauthorized, isAbort } from './api.js';
import { h, fmtDate } from './dom.js';
import { icon, brandMark } from './icons.js';
import { Scope } from './scope.js';
import { closeDrawer, confirmLeave, emptyState, hasUnsaved, toastError } from './ui.js';
import { onLangChange, t } from './i18n.js';
import { mountLogin } from './views/login.js';
import { mountFleet } from './views/fleet.js';
import { mountInstance, normalizeTab } from './views/instance.js';
import { mountUsers } from './views/users.js';
import { mountAudit } from './views/audit.js';
import { accountMenu } from './views/account.js';

const app = document.getElementById('app');

// What each role may do, and which licence feature a capability also needs.
// Views ask ctx.access(capability) — 'ok', 'role' (this role cannot) or
// 'licence' (the role can, the licence does not) — or ctx.can() for a plain
// yes. The server enforces the same rules (403 for the role, 402 for the
// licence).
//   logs     the live log tail
//   operate  hang up, reload XML/ACL, rescan profiles, register gateways
//   control  the admin-only runtime operations: profile start/stop/restart,
//            gateway removal, modules, switch settings, global variables
//   console  the API console
//   manage   add, edit and delete instances
//   users    change user accounts (the list itself is readable without a licence)
//   audit    the audit log
//   files    edit configuration files
const CAPABILITIES = {
  viewer: [],
  operator: ['logs', 'operate'],
  admin: ['logs', 'operate', 'control', 'console', 'manage', 'users', 'audit', 'files'],
};
const FEATURES = { users: 'users', audit: 'audit', files: 'config-edit' };

let me = null; // {username, role, managed}
let version = '';
let license = null; // GET /api/license; null while unknown
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
    return FEATURES[capability] && !ctx.licensed(FEATURES[capability]) ? 'licence' : 'ok';
  },
  can(capability) {
    return ctx.access(capability) === 'ok';
  },
  // licensed reports whether a licence feature is on; unknown counts as on,
  // so a failed /api/license never locks a screen (the server still decides).
  licensed(feature) {
    return !license || (Array.isArray(license.features) && license.features.includes(feature));
  },
  get license() {
    return license;
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
  const lock = (cap) => (ctx.access(cap) === 'licence' ? h('span', { class: 'side-lock', title: t('paid.kicker') }, icon('lock')) : null);
  if (ctx.access('users') !== 'role') {
    adminLinks.users = h('a', { class: 'side-link', href: '#/admin/users', title: t('nav.users') }, icon('users'), h('span', { class: 'name', text: t('nav.users') }), lock('users'));
    admin.push(adminLinks.users);
  }
  if (ctx.access('audit') !== 'role') {
    adminLinks.audit = h('a', { class: 'side-link', href: '#/admin/audit', title: t('nav.audit') }, icon('shield'), h('span', { class: 'name', text: t('nav.audit') }), lock('audit'));
    admin.push(adminLinks.audit);
  }
  const main = h('main', { class: 'main' });
  const foot = h('div', { class: 'side-foot' }, accountMenu({ me, version, license, onSignOut: logout }));
  const side = h('aside', { class: 'side' },
    h('a', { class: 'brand', href: '#/' }, brandMark(), h('span', { text: 'fsdeck' }), editionTag()),
    h('nav', { class: 'side-scroll', 'aria-label': t('nav.main') },
      fleetLink,
      h('div', { class: 'side-label' }, t('nav.instances'), count),
      list,
      admin.length ? h('div', { class: 'side-label' }, t('nav.admin')) : null,
      admin.length ? h('div', { class: 'side-list admin-list' }, admin) : null),
    foot);
  app.replaceChildren(h('div', { class: 'shell' }, side, h('div', { class: 'main-col' }, licenceBanner(), main)));
  shell = { main, list, count, fleetLink, adminLinks, foot };
}

// editionTag is the quiet edition mark next to the brand.
function editionTag() {
  if (!license) return h('span', { class: 'brand-tag', text: t('brand.tag') });
  const enterprise = license.edition === 'enterprise';
  return h('span', { class: enterprise ? 'brand-tag ent' : 'brand-tag', title: enterprise ? license.licensee || '' : t('lic.communityHint') }, enterprise ? t('lic.enterprise') : t('lic.community'));
}

// licenceBanner warns once, above every page, when a licence key is present
// but expired or invalid; Community without a key gets no banner.
function licenceBanner() {
  if (!license || (license.status !== 'expired' && license.status !== 'invalid')) return null;
  const text = license.status === 'expired'
    ? t('lic.banner.expired', { date: license.expires ? fmtDate(license.expires) : '' })
    : t('lic.banner.invalid');
  return h('div', { class: 'lic-banner', role: 'status' }, icon('alert'), h('span', { text }));
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
  // The licence decides which screens are unlocked; ask before drawing them.
  try {
    const data = await api('GET', 'license', { signal: appScope.signal });
    license = data && typeof data === 'object' ? data : null;
  } catch (err) {
    if (isAbort(err)) return;
    license = null;
  }
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
  // The version only decorates the account menu; without it the menu says less.
  if (!version) {
    api('GET', 'version', { signal: appScope.signal }).then((data) => {
      if (!data || typeof data.version !== 'string' || !data.version || !shell) return;
      version = data.version;
      shell.foot.replaceChildren(accountMenu({ me, version, license, onSignOut: logout }));
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
