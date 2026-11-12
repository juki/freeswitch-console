// Entry point: session check, login, hash routing and view lifecycle.

import { api, onUnauthorized, isAbort } from './api.js';
import { h, showBanner, clearBanners } from './dom.js';
import { Scope } from './scope.js';
import { mountLogin } from './views/login.js';
import { mountInstances } from './views/instances.js';
import { mountInstance, normalizeTab } from './views/instance.js';

const main = document.getElementById('main');
const topbar = document.getElementById('topbar');
const who = document.getElementById('who');
const logoutBtn = document.getElementById('logout');

let me = null; // {username, role}
let viewScope = null;
let current = null; // {kind, id, view}

function teardown() {
  if (viewScope) viewScope.dispose();
  viewScope = null;
  current = null;
}

function parseRoute() {
  const hash = location.hash.replace(/^#/, '') || '/';
  if (hash === '/') return { kind: 'instances' };
  const m = hash.match(/^\/i\/([^/]+)(?:\/([^/]*))?\/?$/);
  if (m) {
    try {
      return { kind: 'instance', id: decodeURIComponent(m[1]), tab: m[2] || '' };
    } catch {
      return { kind: 'unknown' };
    }
  }
  return { kind: 'unknown' };
}

function route() {
  if (!me) return;
  const isAdmin = me.role === 'admin';
  const r = parseRoute();
  if (r.kind === 'unknown') {
    history.replaceState(null, '', '#/');
    route();
    return;
  }
  if (r.kind === 'instance') {
    const tab = normalizeTab(r.tab, isAdmin);
    if (tab !== r.tab) {
      history.replaceState(null, '', '#/i/' + encodeURIComponent(r.id) + '/' + tab);
    }
    if (current && current.kind === 'instance' && current.id === r.id) {
      current.view.showTab(tab);
      return;
    }
    teardown();
    viewScope = new Scope();
    const view = mountInstance({ scope: viewScope, root: main, id: r.id, isAdmin });
    current = { kind: 'instance', id: r.id, view };
    view.showTab(tab);
    return;
  }
  teardown();
  viewScope = new Scope();
  current = { kind: 'instances' };
  mountInstances({ scope: viewScope, root: main, isAdmin });
}

function showLogin() {
  if (!me && current && current.kind === 'login') return;
  teardown();
  me = null;
  topbar.hidden = true;
  clearBanners();
  current = { kind: 'login' };
  mountLogin({
    root: main,
    onLogin: async (user) => {
      try {
        const u = user && user.role ? user : await api('GET', 'me');
        if (!u || !u.role) throw new Error('unexpected answer from /api/me');
        me = u;
      } catch (err) {
        if (!isAbort(err) && err.status !== 401) showBanner(err.message);
        return;
      }
      current = null;
      start();
    },
  });
}

function start() {
  who.textContent = me.username + ' (' + me.role + ')';
  topbar.hidden = false;
  route();
}

async function boot() {
  try {
    const user = await api('GET', 'me');
    if (!user || !user.role) throw new Error('unexpected answer from /api/me');
    me = user;
  } catch (err) {
    if (err.status === 401) return; // onUnauthorized already showed the login view
    main.replaceChildren(
      h('div', { class: 'notice error', role: 'alert' }, 'Could not reach fsdeck: ' + err.message),
      h('button', { type: 'button', text: 'Retry', on: { click: boot } }));
    return;
  }
  start();
}

onUnauthorized(showLogin);
window.addEventListener('hashchange', route);
logoutBtn.addEventListener('click', async () => {
  logoutBtn.disabled = true;
  try {
    await api('POST', 'logout');
    showLogin();
  } catch (err) {
    if (err.status !== 401) showBanner('Logout failed: ' + err.message);
  } finally {
    logoutBtn.disabled = false;
  }
});

boot();
