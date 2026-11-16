// Audit log (admin): who signed in, from where, and every change made through
// fsconsole, newest first. The search goes to the server (?q=, debounced); the
// result chips filter the loaded rows. Reading the log is itself audited, so
// it is loaded on demand and never polled.

import { api, isAbort } from '../api.js';
import { h, cellText, fmtDateTime, fmtInt, fmtStamp } from '../dom.js';
import { icon } from '../icons.js';
import { t } from '../i18n.js';
import { clip, dataTable, detailBody, emptyState, errorState, openDrawer, panel, pill, searchBox, segmented, skeleton, sortState } from '../ui.js';

const LIMITS = [200, 500, 1000];
const RESULT_TONE = { ok: 'ok', denied: 'warn', error: 'bad' };

// actionLabel names an audit action for people; the raw action is shown next to it.
export function actionLabel(action) {
  const a = String(action || '');
  const known = {
    login: () => t('audit.act.login'),
    'login.failed': () => t('audit.act.loginFailed'),
    'login.blocked': () => t('audit.act.loginBlocked'),
    logout: () => t('audit.act.logout'),
    'instance.create': () => t('audit.act.instanceCreate'),
    'instance.update': () => t('audit.act.instanceUpdate'),
    'instance.delete': () => t('audit.act.instanceDelete'),
    'channel.hangup': () => t('audit.act.hangup'),
    'action.reloadxml': () => t('audit.act.reloadxml'),
    'action.reloadacl': () => t('audit.act.reloadacl'),
    'action.sofia_rescan': () => t('audit.act.rescan'),
    'console.run': () => t('audit.act.console'),
    'user.create': () => t('audit.act.userCreate'),
    'user.update': () => t('audit.act.userUpdate'),
    'user.delete': () => t('audit.act.userDelete'),
    'user.list': () => t('audit.act.userList'),
    'audit.read': () => t('audit.act.auditRead'),
    'password.change': () => t('audit.act.password'),
    'action.sofia_profile': () => t('audit.act.sofiaProfile'),
    'action.sofia_gateway': () => t('audit.act.sofiaGateway'),
    'action.module': () => t('audit.act.module'),
    'action.fsctl': () => t('audit.act.fsctl'),
    'action.setvar': () => t('audit.act.setvar'),
    'config.write': () => t('audit.act.configWrite'),
    'config.delete': () => t('audit.act.configDelete'),
    'config.restore': () => t('audit.act.configRestore'),
    'config.list': () => t('audit.act.configList'),
    'config.read': () => t('audit.act.configRead'),
    'config.history': () => t('audit.act.configHistory'),
    'config.backup': () => t('audit.act.configBackup'),
  };
  if (known[a]) return known[a]();
  if (a.startsWith('action.')) return t('audit.act.other', { name: a.slice(7) });
  return a;
}

function resultLabel(r) {
  return ({ ok: t('audit.result.ok'), denied: t('audit.result.denied'), error: t('audit.result.error') })[r] || String(r || '');
}

export function mountAudit({ scope, root }) {
  document.title = t('audit.docTitle');
  let rows = null;
  let enabled = true;
  let query = '';
  let result = '';
  let limit = LIMITS[0];
  let selected = null;
  let loadScope = null;
  let timer = null;
  scope.onCleanup(() => clearTimeout(timer));

  const search = searchBox(t('audit.search'), (q) => {
    clearTimeout(timer);
    timer = setTimeout(() => {
      query = q;
      load();
    }, 400);
  });
  const refreshBtn = h('button', { type: 'button', class: 'btn', on: { click: () => load() } }, icon('refresh'), t('audit.refresh'));
  const facets = h('div', { class: 'facets', role: 'group', 'aria-label': t('audit.col.result') });
  const limitSeg = segmented(LIMITS.map((n) => ({ key: String(n), label: fmtInt(n) })), String(limit), (k) => {
    limit = Number(k);
    load();
  }, 'sm');
  const note = h('span', { class: 'panel-note' });
  const p = panel({ title: t('audit.events'), flush: true, actions: h('span', { class: 'row' }, h('span', { class: 'faint', text: t('audit.limit') }), limitSeg.el) });
  p.el.querySelector('.panel-count').after(note);
  const host = h('div', { class: 'tbl-wrap tall audit-tbl' }, skeleton({ rows: 8, cols: 5 }));
  p.body.append(host);
  const toolbar = h('div', { class: 'toolbar' }, search.el, facets);
  const body = h('div', { class: 'page-body' }, toolbar, p.el);
  root.replaceChildren(
    h('header', { class: 'masthead' },
      h('div', { class: 'masthead-in solo' },
        h('div', { class: 'crumbs' }, h('span', { text: t('nav.admin') }), icon('chevron'), h('span', { text: t('nav.audit') })),
        h('div', { class: 'page-head' }, h('div', { class: 'spacer' }, h('h1', { class: 'page-title', text: t('audit.title') }), h('p', { class: 'page-sub', text: t('audit.sub') })), refreshBtn))),
    body);
  const sort = sortState(() => render());

  function renderFacets() {
    const counts = { '': rows.length, ok: 0, denied: 0, error: 0 };
    for (const r of rows) if (r.result in counts) counts[r.result]++;
    const facet = (key, label, cls) => h('button', {
      type: 'button', class: 'facet ' + (cls || ''), 'aria-pressed': String(result === key),
      on: { click: () => { result = key; renderFacets(); render(); } },
    }, label, h('span', { class: 'count', text: fmtInt(counts[key]) }));
    facets.replaceChildren(facet('', t('common.all')), facet('ok', t('audit.result.ok'), 'f-ok'), facet('denied', t('audit.result.denied'), 'f-warn'), facet('error', t('audit.result.error'), 'f-err'));
  }

  function details(r) {
    selected = r;
    render();
    const d = openDrawer({
      kicker: t('audit.kind'), title: actionLabel(r.action), subtitle: fmtDateTime(r.time),
      onClose: () => {
        selected = null;
        render();
      },
    });
    d.body.append(...detailBody(r, ['time', 'user', 'ip', 'action', 'target', 'result', 'error', 'detail']).filter(Boolean));
  }

  function render() {
    if (!rows) return;
    const shown = rows.filter((r) => !result || r.result === result);
    p.setCount(result ? t('common.nOfM', { n: fmtInt(shown.length), m: fmtInt(rows.length) }) : fmtInt(rows.length));
    note.textContent = rows.length >= limit ? t('audit.capped', { n: fmtInt(limit) }) : '';
    host.replaceChildren(dataTable({
      rows: shown,
      sort,
      keyOf: (r) => r,
      selected,
      onRow: details,
      empty: query || result
        ? emptyState({ icon: 'search', title: t('common.noMatch'), text: t('audit.noMatch') })
        : emptyState({ icon: 'shield', title: t('audit.none'), text: t('audit.noneText') }),
      columns: [
        { label: t('audit.col.time'), sortBy: (r) => r.time, render: (r) => h('span', { class: 'num nowrap', text: fmtStamp(r.time), title: fmtDateTime(r.time) }) },
        { label: t('audit.col.user'), sortBy: (r) => cellText(r.user), render: (r) => h('span', { class: 'cell-strong', text: cellText(r.user) }) },
        {
          label: t('audit.col.action'), sortBy: (r) => cellText(r.action),
          render: (r) => h('span', { class: 'action-cell' }, h('span', { text: actionLabel(r.action) }), h('span', { class: 'sub mono', text: cellText(r.action) })),
        },
        { label: t('audit.col.target'), mono: true, sortBy: (r) => cellText(r.target), render: (r) => clip(r.target, 'narrow') },
        { label: t('audit.col.detail'), mono: true, sortBy: (r) => cellText(r.detail), render: (r) => (r.detail || r.error ? clip(r.error ? r.error + (r.detail ? ' · ' + r.detail : '') : r.detail, r.error ? 'medium err-text' : 'medium faint') : '') },
        { label: t('audit.col.result'), sortBy: (r) => cellText(r.result), render: (r) => pill(resultLabel(r.result), RESULT_TONE[r.result] || '') },
        { label: t('audit.col.ip'), mono: true, sortBy: (r) => cellText(r.ip), render: (r) => h('span', { class: 'faint', text: cellText(r.ip) }) },
      ],
    }));
  }

  function load() {
    if (loadScope) loadScope.dispose();
    loadScope = scope.child();
    refreshBtn.disabled = true;
    const q = new URLSearchParams({ limit: String(limit) });
    if (query) q.set('q', query);
    api('GET', 'audit?' + q.toString(), { signal: loadScope.signal }).then((data) => {
      refreshBtn.disabled = false;
      enabled = !(data && data.enabled === false);
      rows = data && Array.isArray(data.rows) ? data.rows : [];
      if (!enabled) {
        toolbar.hidden = true;
        host.replaceChildren(emptyState({ icon: 'shield', tone: 'calm', title: t('audit.off'), text: t('audit.offText') }));
        p.setCount(null);
        return;
      }
      toolbar.hidden = false;
      renderFacets();
      render();
    }, (err) => {
      if (isAbort(err)) return;
      refreshBtn.disabled = false;
      rows = null;
      p.setCount(null);
      note.textContent = '';
      host.replaceChildren(errorState(err));
    });
  }
  load();
}
