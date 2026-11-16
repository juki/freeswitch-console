// Recent hang-ups: the channels that ended while fsconsole was connected,
// kept in memory by the server (newest first). The third segment of Calls;
// it polls only while that segment is shown. A row opens a drawer with the
// cause explained, the key facts, the call's own log and all its variables.

import { api, instPath, isAbort, rowsOf } from '../api.js';
import { h, DASH, cellText, fmtClock, fmtDateTime, fmtDuration, fmtInt, fmtPct } from '../dom.js';
import { t } from '../i18n.js';
import { CAUSE_TONE, causeInfo } from '../causes.js';
import { clip, copyText, dataTable, detailBody, emptyState, errorState, iconButton, kv, openDrawer, pill, segmented, skeleton, sortState, tag, toast } from '../ui.js';
import { direction, party } from './calls.js';
import { lineEl } from './logs.js';

// the most rows asked for, whatever the server keeps
const LIMIT_MAX = 1000;

function endedBy(d) {
  switch (d) {
    case 'recv_bye': return t('hangups.by.recv_bye');
    case 'send_bye': return t('hangups.by.send_bye');
    case 'recv_cancel': return t('hangups.by.recv_cancel');
    case 'send_cancel': return t('hangups.by.send_cancel');
    case 'recv_refuse': return t('hangups.by.recv_refuse');
    case 'send_refuse': return t('hangups.by.send_refuse');
    default: return '';
  }
}

function ago(ms) {
  return t('hangups.ago', { duration: fmtDuration((Date.now() - Number(ms)) / 1000) });
}

const RANK = { ok: 0, unanswered: 1, failed: 2 };

// rowInfo explains the hang-up cause and, when a bridge failed during the
// call, that failure; cls is the worse class of the two.
function rowInfo(r) {
  const info = causeInfo(r.cause, r.context);
  const failure = cellText(r.originateFailure);
  const bridge = failure ? causeInfo(failure, r.context) : null;
  return { info, bridge, cls: bridge && RANK[bridge.cls] > RANK[info.cls] ? bridge.cls : info.cls };
}

// verdict is the cause pill, followed by the SIP status when there is one
// and the failed bridge when one is given.
function verdict(r, info, bridge) {
  const sip = Number(r.sipStatus) || 0;
  return h('span', { class: 'hu-result' }, pill(info.label, CAUSE_TONE[info.cls]), sip > 0 ? tag(String(sip)) : null,
    bridge ? pill(t('hangups.bridgeFailed', { label: bridge.label }), CAUSE_TONE[bridge.cls]) : null);
}

// the fields the search box looks at
function matches(r, q) {
  return !q || [r.cidName, r.cidNum, r.dest, r.cause, r.info.label, r.originateFailure, r.bridge ? r.bridge.label : '', r.uuid, r.gateway, r.profile, r.remoteIP]
    .some((v) => cellText(v).toLowerCase().includes(q));
}

// hangupsSegment builds the segment inside the Calls panel. show(on) starts
// or stops it; render(query) draws it while it is shown.
export function hangupsSegment({ id, scope, host, panel: p }) {
  let active = false;
  let live = null; // the scope of the polling, while shown
  let data = null; // {enabled, capacity, rows}
  let lastError = null;
  let filter = 'all';
  let query = '';
  let selected = null;
  let signature = '';
  const sort = sortState(() => render(query));

  const filterSeg = segmented([
    { key: 'all', label: t('common.all') },
    { key: 'failed', label: t('hangups.f.failed') },
    { key: 'unanswered', label: t('hangups.f.unanswered') },
  ], filter, (k) => {
    filter = k;
    render(query);
  }, 'sm');
  filterSeg.el.hidden = true;
  const note = h('p', { class: 'hu-note', hidden: true });
  host.el.before(note);

  const columns = [
    {
      label: t('hangups.col.ended'), sortBy: (r) => Number(r.t) || 0,
      render: (r) => h('span', null, h('span', { class: 'num', text: fmtClock(Number(r.t), true) }), h('span', { class: 'sub', 'data-ago': String(Number(r.t)), text: ago(r.t) })),
    },
    { label: t('hangups.col.dir'), sortBy: (r) => cellText(r.direction), render: (r) => direction(r.direction) },
    { label: t('calls.col.from'), sortBy: (r) => cellText(r.cidName) || cellText(r.cidNum), render: (r) => party(r.cidName, r.cidNum) },
    { label: t('calls.col.to'), mono: true, sortBy: (r) => cellText(r.dest), render: (r) => (r.dest ? clip(r.dest) : '') },
    {
      label: t('hangups.col.result'), sortBy: (r) => r.info.label,
      render: (r) => {
        const raw = [cellText(r.cause), cellText(r.originateFailure)].filter(Boolean).join(' · ');
        return h('span', null, verdict(r, r.info, r.bridge), raw ? h('span', { class: 'sub mono', text: raw }) : null);
      },
    },
    { label: t('hangups.col.endedBy'), sortBy: (r) => endedBy(r.disposition), render: (r) => endedBy(r.disposition) },
    {
      label: t('hangups.col.talk'), right: true, sortBy: (r) => (r.answered ? Number(r.billsec) || 0 : -1),
      render: (r) => (r.answered ? h('span', { class: 'num', text: fmtDuration(r.billsec) }) : h('span', { class: 'muted', text: t('hangups.notAnswered') })),
    },
    {
      label: t('hangups.col.via'), sortBy: (r) => cellText(r.gateway) || cellText(r.profile),
      render: (r) => (r.gateway ? tag(cellText(r.gateway)) : r.profile ? h('span', { class: 'muted', text: cellText(r.profile) }) : ''),
    },
  ];

  // ---- the drawer ----

  let drawerScope = null;

  function factGroup(title, pairs) {
    const list = pairs.filter(([, v]) => v !== '' && v !== null && v !== undefined);
    return list.length ? [h('div', { class: 'drawer-section', text: title }), kv(list)] : [];
  }

  function copyButton(label, text, done) {
    return iconButton('copy', label, async () => {
      if (await copyText(text)) toast(done);
      else toast(t('copy.denied'), 'error');
    });
  }

  function facts(r) {
    const uuid = cellText(r.uuid);
    const name = cellText(r.cidName);
    const numb = cellText(r.cidNum);
    const by = cellText(r.disposition);
    const mos = Number(r.mos) || 0;
    const quality = Number(r.quality) || 0;
    const other = cellText(r.otherUUID);
    const otherRow = other && data ? data.rows.find((x) => cellText(x.uuid) === other) : null;
    const mono = (v) => (cellText(v) ? h('span', { class: 'mono', text: cellText(v) }) : '');
    return [
      ...factGroup(t('hangups.g.call'), [
        [t('hangups.col.dir'), r.direction ? direction(r.direction) : ''],
        [t('calls.col.from'), name || numb ? h('span', null, name && name !== numb ? h('span', { class: 'cell-strong', text: name + ' ' }) : null, numb ? h('span', { class: 'mono', text: numb }) : null) : ''],
        [t('calls.col.to'), mono(r.dest)],
        [t('dir.col.context'), cellText(r.context)],
        [t('hangups.col.ended'), fmtDateTime(Number(r.t))],
        [t('hangups.k.duration'), fmtDuration(r.duration)],
        [t('hangups.col.talk'), r.answered ? fmtDuration(r.billsec) : t('hangups.notAnswered')],
        [t('hangups.col.endedBy'), endedBy(by) || mono(by)],
        [t('hangups.k.bridge'), mono(r.originateFailure)],
        [t('hangups.k.uuid'), h('span', { class: 'hu-uuid' }, h('span', { class: 'mono', text: uuid }), copyButton(t('hangups.copyUuid'), uuid, t('hangups.uuidCopied')))],
      ]),
      ...factGroup(t('hangups.g.sip'), [
        [t('sip.col.profile'), cellText(r.profile)],
        [t('sip.col.gateway'), cellText(r.gateway)],
        [t('hangups.k.remote'), mono(r.remoteIP)],
        [t('hangups.k.ua'), cellText(r.userAgent)],
        [t('hangups.k.sipStatus'), Number(r.sipStatus) > 0 ? mono(r.sipStatus) : ''],
        [t('hangups.k.q850'), Number(r.q850) > 0 ? mono(r.q850) : ''],
      ]),
      ...(r.codec || mos > 0 || quality > 0 ? factGroup(t('hangups.g.audio'), [
        [t('calls.col.codec'), cellText(r.codec)],
        [t('hangups.k.mos'), mos > 0 ? h('span', { class: mos < 3.1 ? 'num text-bad' : mos < 3.6 ? 'num text-warn' : 'num', text: fmtPct(mos, 2) }) : ''],
        [t('hangups.k.quality'), quality > 0 ? t('hangups.pct', { n: fmtPct(quality, Number.isInteger(quality) ? 0 : 1) }) : ''],
      ]) : []),
      ...factGroup(t('hangups.g.other'), [
        [t('hangups.k.uuid'), otherRow
          ? h('button', { type: 'button', class: 'link-btn', on: { click: () => details(otherRow) } }, t('hangups.openOther'))
          : mono(other)],
      ]),
    ];
  }

  function logSection(r, log) {
    const n = Array.isArray(log) ? log.length : Number(r.logLines) || 0;
    const heading = t('hangups.log', { n: fmtInt(n) });
    const text = Array.isArray(log) ? log.map((ev) => cellText(ev && ev.text).replace(/\r?\n$/, '')).join('\n') : '';
    const head = h('div', { class: 'hu-sec' }, h('div', { class: 'drawer-section', text: heading }),
      Array.isArray(log) && log.length ? copyButton(t('hangups.copyLog'), text, t('hangups.logCopied')) : null);
    if (!Array.isArray(log)) return [head, h('p', { class: 'note-empty', text: t('hangups.logHidden') })];
    if (!log.length) return [head, h('p', { class: 'note-empty', text: t('hangups.logNone') })];
    return [head, h('div', { class: 'hu-log', tabIndex: 0, 'aria-label': heading }, log.map((ev) => lineEl(ev || {})))];
  }

  function details(r) {
    const uuid = cellText(r.uuid);
    const { info, bridge, cls } = rowInfo(r);
    if (drawerScope) drawerScope.dispose();
    const ds = scope.child();
    drawerScope = ds;
    const from = cellText(r.cidNum) || cellText(r.cidName) || DASH;
    const d = openDrawer({
      kicker: t('hangups.kind'), title: t('hangups.title', { from, to: cellText(r.dest) || DASH }), subtitle: fmtDateTime(Number(r.t)),
      onClose: () => {
        ds.dispose();
        if (drawerScope === ds) drawerScope = null;
        selected = null;
      },
    });
    selected = uuid;
    const slot = h('div', null, skeleton({ rows: 4, cols: 1 }));
    d.body.append(
      h('div', { class: 'verdict ' + CAUSE_TONE[cls] },
        verdict(r, info),
        h('p', { class: 'verdict-text', text: info.text }),
        bridge ? h('p', { class: 'verdict-text', text: t('hangups.bridgeText', { label: bridge.label, raw: cellText(r.originateFailure), text: bridge.text }) }) : null,
        r.cause ? h('div', { class: 'verdict-raw mono', text: cellText(r.cause) }) : null),
      ...facts(r),
      slot);
    api('GET', instPath(id, 'hangups/' + encodeURIComponent(uuid)), { signal: ds.signal }).then((detail) => {
      const vars = detail && detail.vars && typeof detail.vars === 'object' ? detail.vars : {};
      slot.replaceChildren(
        ...logSection(r, detail ? detail.log : null),
        h('details', null,
          h('summary', { text: t('hangups.vars', { n: fmtInt(Object.keys(vars).length) }) }),
          ...detailBody(vars).filter(Boolean)));
    }, (err) => {
      if (isAbort(err) || ds.disposed) return;
      if (err.status === 404) {
        d.close();
        toast(t('hangups.gone'), 'info');
        return;
      }
      slot.replaceChildren(errorState(err));
    });
  }

  // ---- the table ----

  function render(q) {
    query = q;
    if (!active) return;
    if (lastError) {
      signature = '';
      p.setCount(null);
      host.error(lastError);
      return;
    }
    if (!data) return;
    p.setTitle(t('calls.hangups'));
    if (!data.enabled) {
      filterSeg.el.hidden = true;
      note.hidden = true;
      p.setCount(null);
      if (signature === 'off') return;
      signature = 'off';
      host.show(emptyState({ icon: 'power', title: t('hangups.off.title'), text: t('hangups.off.text') }));
      return;
    }
    filterSeg.el.hidden = false;
    note.hidden = false;
    note.textContent = t('hangups.note', { capacity: fmtInt(data.capacity) });
    const all = data.rows.map((r) => ({ ...r, ...rowInfo(r) }));
    const failed = all.filter((r) => r.cls === 'failed').length;
    const unanswered = all.filter((r) => r.cls === 'unanswered').length;
    filterSeg.setCount('all', fmtInt(all.length));
    filterSeg.setCount('failed', fmtInt(failed));
    filterSeg.setCount('unanswered', fmtInt(unanswered));
    const rows = all.filter((r) => (filter === 'all' || r.cls === filter) && matches(r, query));
    p.setCount(rows.length === all.length ? all.length : t('common.nOfM', { n: fmtInt(rows.length), m: fmtInt(all.length) }));
    // An unchanged table is left alone so hover, focus and scrolling survive.
    const sig = JSON.stringify([filter, query, selected, sort.col, sort.dir, data.rows]);
    if (sig === signature) return;
    signature = sig;
    host.show(dataTable({
      rows,
      columns,
      sort,
      keyOf: (r) => cellText(r.uuid),
      selected,
      onRow: details,
      empty: all.length
        ? emptyState({ icon: 'search', title: t('common.noMatch'), text: t('common.noMatchText') })
        : emptyState({ icon: 'history', title: t('hangups.none.title'), text: t('hangups.none.text') }),
    }));
  }

  async function load(signal) {
    const limit = data && data.capacity ? '?limit=' + Math.min(Number(data.capacity), LIMIT_MAX) : '';
    try {
      const res = await api('GET', instPath(id, 'hangups' + limit), { signal });
      data = { enabled: Boolean(res && res.enabled), capacity: Number(res && res.capacity) || 0, rows: rowsOf(res) };
      lastError = null;
    } catch (err) {
      if (isAbort(err)) throw err;
      lastError = err;
    }
    render(query);
  }

  function show(on) {
    if (on === active) return;
    active = on;
    host.el.classList.toggle('hu-tbl', on);
    filterSeg.el.hidden = true;
    note.hidden = true;
    signature = '';
    if (!on) {
      live.dispose();
      live = null;
      return;
    }
    live = scope.child();
    const tick = setInterval(() => {
      for (const el of host.el.querySelectorAll('[data-ago]')) el.textContent = ago(el.dataset.ago);
    }, 1000);
    live.onCleanup(() => clearInterval(tick));
    if (!data && !lastError) {
      p.setTitle(t('calls.hangups'));
      p.setCount(null);
      host.show(skeleton({ rows: 6, cols: 4 }));
    }
    live.poll(load, 3000);
  }

  return { filterEl: filterSeg.el, show, render };
}
