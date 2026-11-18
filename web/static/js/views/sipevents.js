// SIP events: registrations, failed attempts and gateway changes, kept in
// memory by the server (newest first). The second mode of the SIP page; it
// polls only while shown. A row opens a drawer with the facts and the
// event's SIP headers.

import { api, instPath, isAbort, rowsOf } from '../api.js';
import { h, DASH, cellText, fmtClock, fmtDateTime, fmtDuration, fmtInt } from '../dom.js';
import { icon } from '../icons.js';
import { plural, t } from '../i18n.js';
import { clip, dataTable, detailBody, emptyState, errorState, kv, meter, openDrawer, panel, pill, searchBox, segmented, skeleton, sortState, tableHost, tag, toast } from '../ui.js';

// the most rows asked for, whatever the server keeps
const LIMIT_MAX = 1000;
const HOUR = 3600 * 1000;
// failure sources shown before "Show all"
const FAIL_ROWS = 8;
// user names shown per failure source before "+n"
const FAIL_USERS = 4;

const GW_KINDS = ['gateway_state', 'gateway_add', 'gateway_delete'];

const FILTERS = {
  all: () => true,
  failures: (r) => r.kind === 'register_failure',
  regs: (r) => r.kind === 'register' || r.kind === 'unregister' || r.kind === 'expire',
  gateways: (r) => GW_KINDS.includes(r.kind),
};

function ago(ms) {
  return t('hangups.ago', { duration: fmtDuration((Date.now() - Number(ms)) / 1000) });
}

function upper(v) {
  return cellText(v).toUpperCase();
}

// gatewayTone: a bad state or ping wins over a good one; the rest is neutral.
function gatewayTone(r) {
  const state = upper(r.state);
  const ping = upper(r.pingStatus);
  if (state === 'FAIL_WAIT' || state === 'DOWN' || state === 'FAILED' || ping === 'DOWN') return 'bad';
  if (state === 'REGED' || ping === 'UP') return 'ok';
  return '';
}

// the event pill of each kind: [text, tone]
const KINDS = {
  register_failure: (r) => (upper(r.method) === 'INVITE' ? [t('sipev.kind.inviteFailure'), 'bad'] : [t('sipev.kind.registerFailure'), 'bad']),
  register: () => [t('sipev.kind.register'), 'ok'],
  unregister: () => [t('sipev.kind.unregister'), ''],
  expire: () => [t('sipev.kind.expire'), 'warn'],
  gateway_state: (r) => [t('sipev.kind.gatewayState', { state: cellText(r.state) || cellText(r.pingStatus) || DASH }), gatewayTone(r)],
  gateway_add: () => [t('sipev.kind.gatewayAdd'), ''],
  gateway_delete: () => [t('sipev.kind.gatewayDelete'), 'warn'],
};

function eventLabel(r) {
  const fn = KINDS[r.kind];
  return fn ? fn(r) : [cellText(r.kind) || DASH, ''];
}

function isGateway(r) {
  return GW_KINDS.includes(r.kind);
}

// who is "user@domain" for registrations and the gateway name for gateways.
function whoText(r) {
  if (isGateway(r)) return cellText(r.gateway);
  const user = cellText(r.user);
  const domain = cellText(r.domain);
  return user && domain ? user + '@' + domain : user || domain;
}

// address is "ip:port", with an IPv6 address in brackets.
function address(r) {
  const ip = cellText(r.ip);
  if (!ip) return '';
  const port = Number(r.port) || 0;
  if (!port) return ip;
  return (ip.includes(':') ? '[' + ip + ']' : ip) + ':' + port;
}

function timeCell(ms) {
  if (!Number(ms)) return '';
  return h('span', null, h('span', { class: 'num', text: fmtClock(Number(ms), true) }), h('span', { class: 'sub', 'data-ago': String(Number(ms)), text: ago(ms) }));
}

// the fields the search box looks at
function matches(r, q) {
  return !q || [r.user, r.domain, r.ip, address(r), r.gateway, r.profile, r.userAgent, r.state]
    .some((v) => cellText(v).toLowerCase().includes(q));
}

// sipEventsMode builds the Events mode of the SIP page. show(on) starts or
// stops it; focus(query, filter) sets the search box and the filter.
export function sipEventsMode({ id, scope }) {
  let active = false;
  let live = null; // the scope of the polling, while shown
  let data = null; // {enabled, capacity, rows}
  let lastError = null;
  let fails = null; // failure sources
  let failError = null;
  let filter = 'all';
  let query = '';
  let selected = null;
  let showAll = false;
  let evSig = '';
  let failSig = '';
  const evSort = sortState(() => render());
  const failSort = sortState(() => render());

  // ---- layout ----

  const summary = h('section', { class: 'panel summary sev-sum', 'aria-label': t('sipev.summary'), hidden: true });

  const failNote = h('p', { class: 'hu-note', text: t('sipev.fail.note') });
  const failHost = tableHost({ rows: 3, cols: 5 });
  const moreBtn = h('button', { type: 'button', class: 'link-btn', on: { click: () => { showAll = true; render(); } } });
  const failMore = h('div', { class: 'sev-more', hidden: true }, moreBtn);
  const failPanel = panel({ title: t('sipev.fail.title'), flush: true });
  failPanel.body.append(failNote, failHost.el, failMore);

  const search = searchBox(t('sipev.filter'), (q) => {
    query = q;
    render();
  }, 'sm');
  const filterSeg = segmented([
    { key: 'all', label: t('common.all') },
    { key: 'failures', label: t('sipev.f.failures') },
    { key: 'regs', label: t('sipev.f.regs') },
    { key: 'gateways', label: t('sipev.f.gateways') },
  ], filter, (k) => {
    filter = k;
    render();
  }, 'sm');
  const evNote = h('p', { class: 'hu-note', hidden: true });
  const evHost = tableHost({ rows: 6, cols: 5 });
  evHost.el.classList.add('sev-tbl');
  const evPanel = panel({ title: t('sipev.title'), flush: true, actions: [search.el, filterSeg.el] });
  evPanel.el.classList.add('sev-panel');
  evPanel.body.append(evNote, evHost.el);

  const offPanel = h('section', { class: 'panel', hidden: true },
    emptyState({ icon: 'power', title: t('sipev.off.title'), text: t('sipev.off.text') }));

  const el = h('div', { hidden: true }, summary, failPanel.el, evPanel.el, offPanel);

  // ---- summary ----

  function cell(iconName, label, value, note, tone) {
    return h('div', { class: 'summary-item' },
      h('div', { class: 'summary-label' }, icon(iconName), label),
      h('div', { class: tone ? 'summary-value ' + tone : 'summary-value' }, value),
      h('div', { class: 'summary-note', text: note }));
  }

  function renderSummary(rows) {
    const since = Date.now() - HOUR;
    const hour = rows.filter((r) => Number(r.t) >= since);
    const failed = hour.filter((r) => r.kind === 'register_failure');
    const sources = new Set(failed.map((r) => cellText(r.ip)).filter(Boolean)).size;
    const registered = hour.filter((r) => r.kind === 'register').length;
    const gone = hour.filter((r) => r.kind === 'unregister' || r.kind === 'expire').length;
    const gw = hour.filter(isGateway);
    // rows are newest first
    const lastGw = gw.length ? cellText(gw[0].gateway) : '';
    summary.replaceChildren(
      cell('alert', t('sipev.sum.failed'), fmtInt(failed.length),
        plural(sources, t('sipev.sum.failedNote.one', { n: fmtInt(sources) }), t('sipev.sum.failedNote.other', { n: fmtInt(sources) })), failed.length ? 'bad' : ''),
      cell('user', t('sipev.sum.regs'), fmtInt(registered), t('sipev.sum.regsNote', { n: fmtInt(gone) })),
      cell('plug', t('sipev.sum.gateways'), fmtInt(gw.length), lastGw || DASH));
  }

  // ---- failed registrations by source ----

  function users(list) {
    const all = Array.isArray(list) ? list.map(cellText).filter(Boolean) : [];
    if (!all.length) return '';
    const more = all.length - FAIL_USERS;
    return h('span', { class: 'sev-users' }, all.slice(0, FAIL_USERS).map(tag),
      more > 0 ? h('span', { class: 'muted', text: t('sipev.fail.more', { n: fmtInt(more) }) }) : null);
  }

  function attempts(n, max) {
    const m = meter();
    m.set(max > 0 ? n / max : 0, 'bad');
    return h('span', { class: 'sev-att' }, m.el, h('span', { class: 'num', text: fmtInt(n) }));
  }

  function renderFails() {
    if (failError) {
      failSig = '';
      failPanel.setCount(null);
      failMore.hidden = true;
      failHost.error(failError);
      return;
    }
    if (!fails) return;
    failPanel.setCount(fails.length);
    failMore.hidden = showAll || fails.length <= FAIL_ROWS;
    moreBtn.textContent = t('sipev.fail.showAll', { n: fmtInt(fails.length) });
    const sig = JSON.stringify([showAll, failSort.col, failSort.dir, fails]);
    if (sig === failSig) return;
    failSig = sig;
    if (!fails.length) {
      failHost.show(h('p', { class: 'note-empty', text: t('sipev.fail.empty') }));
      return;
    }
    const max = Math.max(...fails.map((r) => Number(r.attempts) || 0));
    const table = dataTable({
      rows: fails,
      sort: failSort,
      onRow: (r) => focus(cellText(r.ip)),
      columns: [
        { label: t('sipev.col.source'), mono: true, sortBy: (r) => cellText(r.ip), render: (r) => cellText(r.ip) },
        { label: t('sipev.col.attempts'), right: true, sortBy: (r) => Number(r.attempts) || 0, render: (r) => attempts(Number(r.attempts) || 0, max) },
        { label: t('sipev.col.users'), sortBy: (r) => (Array.isArray(r.users) ? r.users.length : 0), render: (r) => users(r.users) },
        { label: t('hangups.k.ua'), sortBy: (r) => cellText(r.userAgent), render: (r) => (r.userAgent ? clip(r.userAgent, 'medium') : '') },
        { label: t('sipev.col.last'), sortBy: (r) => Number(r.last) || 0, render: (r) => timeCell(r.last) },
      ],
    });
    // the cut comes after the sort, so "Show all" reveals the rest of the sorted list
    if (!showAll && table.tBodies[0]) {
      for (const tr of [...table.tBodies[0].rows].slice(FAIL_ROWS)) tr.hidden = true;
    }
    failHost.show(table);
  }

  // ---- the events table ----

  const columns = [
    { label: t('sipev.col.time'), sortBy: (r) => Number(r.t) || 0, render: (r) => timeCell(r.t) },
    {
      label: t('sipev.col.event'), sortBy: (r) => eventLabel(r)[0],
      render: (r) => {
        const [text, tone] = eventLabel(r);
        const status = Number(r.status) || 0;
        return h('span', { class: 'hu-result' }, pill(text, tone), status > 0 ? tag(String(status)) : null);
      },
    },
    {
      label: t('sipev.col.who'), sortBy: whoText,
      render: (r) => {
        if (isGateway(r)) return r.gateway ? h('span', { class: 'cell-strong', text: cellText(r.gateway) }) : '';
        const user = cellText(r.user);
        const domain = cellText(r.domain);
        if (!user && !domain) return '';
        return h('span', null, user ? h('span', { class: 'cell-strong', text: user }) : null, domain ? h('span', { class: 'muted', text: '@' + domain }) : null);
      },
    },
    { label: t('sipev.col.from'), mono: true, sortBy: address, render: address },
    { label: t('sip.col.profile'), sortBy: (r) => cellText(r.profile), render: (r) => (r.profile ? h('span', { class: 'muted', text: cellText(r.profile) }) : '') },
    {
      label: t('sipev.col.detail'), sortBy: (r) => (isGateway(r) ? cellText(r.pingStatus) : cellText(r.userAgent)),
      render: (r) => {
        if (!isGateway(r)) return r.userAgent ? clip(r.userAgent, 'medium') : '';
        const parts = [r.pingStatus ? t('sipev.ping', { status: cellText(r.pingStatus) }) : '', cellText(r.phrase)].filter(Boolean);
        return parts.length ? clip(parts.join(' · '), 'medium') : '';
      },
    },
  ];

  function render() {
    if (!active) return;
    if (data && !data.enabled && !lastError) {
      summary.hidden = true;
      failPanel.el.hidden = true;
      evPanel.el.hidden = true;
      offPanel.hidden = false;
      return;
    }
    offPanel.hidden = true;
    failPanel.el.hidden = false;
    evPanel.el.hidden = false;
    renderFails();
    if (lastError) {
      evSig = '';
      summary.hidden = true;
      evNote.hidden = true;
      evPanel.setCount(null);
      for (const k of Object.keys(FILTERS)) filterSeg.setCount(k, null);
      evHost.error(lastError);
      return;
    }
    if (!data) return;
    summary.hidden = false;
    renderSummary(data.rows);
    evNote.hidden = false;
    evNote.textContent = t('sipev.note', { capacity: fmtInt(data.capacity) });
    const all = data.rows;
    for (const [k, fn] of Object.entries(FILTERS)) filterSeg.setCount(k, fmtInt(all.filter(fn).length));
    const rows = all.filter((r) => FILTERS[filter](r) && matches(r, query));
    evPanel.setCount(rows.length === all.length ? all.length : t('common.nOfM', { n: fmtInt(rows.length), m: fmtInt(all.length) }));
    // An unchanged table is left alone so hover, focus and scrolling survive.
    const sig = JSON.stringify([filter, query, selected, evSort.col, evSort.dir, all]);
    if (sig === evSig) return;
    evSig = sig;
    evHost.show(dataTable({
      rows,
      columns,
      sort: evSort,
      keyOf: (r) => Number(r.seq),
      selected,
      onRow: details,
      empty: all.length
        ? emptyState({ icon: 'search', title: t('common.noMatch'), text: t('common.noMatchText') })
        : emptyState({ icon: 'radio', title: t('sipev.none.title'), text: t('sipev.none.text') }),
    }));
  }

  // ---- the drawer ----

  let drawerScope = null;

  function factGroup(title, pairs) {
    const list = pairs.filter(([, v]) => v !== '' && v !== null && v !== undefined);
    return list.length ? [h('div', { class: 'drawer-section', text: title }), kv(list)] : [];
  }

  function facts(r) {
    const mono = (v) => (cellText(v) ? h('span', { class: 'mono', text: cellText(v) }) : '');
    const pos = (v) => (Number(v) > 0 ? mono(Number(v)) : '');
    return [
      ...factGroup(t('sipev.g.event'), [
        [t('sipev.k.time'), Number(r.t) ? fmtDateTime(Number(r.t)) : ''],
        [t('sip.col.profile'), cellText(r.profile)],
        [t('sipev.k.status'), pos(r.status)],
        [t('sipev.k.phrase'), cellText(r.phrase)],
      ]),
      ...factGroup(t('sipev.g.reg'), [
        [t('sip.col.user'), cellText(r.user)],
        [t('sipev.k.domain'), cellText(r.domain)],
        [t('sip.col.contact'), mono(r.contact)],
        [t('sipev.k.expires'), pos(r.expires)],
        [t('hangups.k.ua'), cellText(r.userAgent)],
      ]),
      ...factGroup(t('sipev.g.source'), [
        [t('sipev.k.address'), mono(r.ip)],
        [t('sipev.k.port'), pos(r.port)],
      ]),
      ...factGroup(t('sipev.g.gateway'), [
        [t('sipev.k.name'), cellText(r.gateway)],
        [t('sip.col.state'), cellText(r.state) ? pill(cellText(r.state), gatewayTone({ state: r.state })) : ''],
        [t('sipev.k.ping'), cellText(r.pingStatus) ? pill(cellText(r.pingStatus), gatewayTone({ pingStatus: r.pingStatus })) : ''],
      ]),
    ];
  }

  function details(r) {
    const seq = Number(r.seq);
    const [label] = eventLabel(r);
    const who = whoText(r);
    if (drawerScope) drawerScope.dispose();
    const ds = scope.child();
    drawerScope = ds;
    const d = openDrawer({
      kicker: t('sipev.kind'), title: who ? label + ' · ' + who : label, subtitle: Number(r.t) ? fmtDateTime(Number(r.t)) : '',
      onClose: () => {
        ds.dispose();
        if (drawerScope === ds) drawerScope = null;
        selected = null;
        if (!scope.disposed) render();
      },
    });
    selected = seq;
    render();
    const slot = h('div', null, skeleton({ rows: 4, cols: 1 }));
    // native append() would print "null", so the optional verdict is filtered out
    d.body.append(...[
      r.kind === 'register_failure'
        ? h('div', { class: 'verdict bad' }, h('p', { class: 'verdict-text', text: t('sipev.failText', { method: cellText(r.method) || 'REGISTER' }) }))
        : null,
      ...facts(r),
      slot].filter(Boolean));
    api('GET', instPath(id, 'sipevents/' + encodeURIComponent(String(seq))), { signal: ds.signal }).then((detail) => {
      const headers = detail && detail.headers && typeof detail.headers === 'object' ? detail.headers : {};
      slot.replaceChildren(
        h('details', null,
          h('summary', { text: t('sipev.headers', { n: fmtInt(Object.keys(headers).length) }) }),
          ...detailBody(headers).filter(Boolean)));
    }, (err) => {
      if (isAbort(err) || ds.disposed) return;
      if (err.status === 404) {
        d.close();
        toast(t('sipev.gone'), 'info');
        return;
      }
      slot.replaceChildren(errorState(err));
    });
  }

  // ---- loading ----

  async function load(signal) {
    const limit = data && data.capacity ? '?limit=' + Math.min(Number(data.capacity), LIMIT_MAX) : '';
    await Promise.all([
      api('GET', instPath(id, 'sipevents' + limit), { signal }).then((res) => {
        data = { enabled: Boolean(res && res.enabled), capacity: Number(res && res.capacity) || 0, rows: rowsOf(res) };
        lastError = null;
      }, (err) => {
        if (isAbort(err)) throw err;
        lastError = err;
      }),
      api('GET', instPath(id, 'sipevents/failures?minutes=60'), { signal }).then((res) => {
        fails = rowsOf(res);
        failError = null;
      }, (err) => {
        if (isAbort(err)) throw err;
        failError = err;
      }),
    ]);
    render();
  }

  function show(on) {
    if (on === active) return;
    active = on;
    el.hidden = !on;
    evSig = '';
    failSig = '';
    if (!on) {
      live.dispose();
      live = null;
      return;
    }
    live = scope.child();
    const tick = setInterval(() => {
      for (const n of el.querySelectorAll('[data-ago]')) n.textContent = ago(n.dataset.ago);
    }, 1000);
    live.onCleanup(() => clearInterval(tick));
    render();
    live.poll(load, 5000);
  }

  // focus fills the search box (and, if given, sets the filter).
  function focus(q, f) {
    if (f && FILTERS[f]) {
      filter = f;
      filterSeg.set(f);
    }
    search.input.value = q;
    query = q.trim().toLowerCase();
    render();
    search.input.focus();
  }

  return { el, show, focus };
}
