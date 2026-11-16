// Calls: channels and bridged calls, refreshed every 3s; admin can hang up.
// The third segment, recent hang-ups, is in hangups.js.

import { api, instPath, isAbort, rowsOf } from '../api.js';
import { h, DASH, cellText, fmtDuration, fmtInt } from '../dom.js';
import { icon } from '../icons.js';
import { t } from '../i18n.js';
import { hangupsSegment } from './hangups.js';
import { clip, confirmDialog, dataTable, detailBody, emptyState, iconButton, openDrawer, panel, rowMatches, searchBox, segmented, sortState, statePill, tableHost, tag, toast, toastError } from '../ui.js';

export function party(name, num) {
  const n = cellText(name);
  const d = cellText(num);
  if (!n || n === d) return h('span', { class: 'cell-strong', text: d || DASH });
  return h('span', null, h('span', { class: 'cell-strong', text: n }), d ? h('span', { class: 'sub mono', text: d }) : null);
}

function codec(name, rate) {
  const c = cellText(name);
  if (!c) return '';
  const khz = Number(rate) / 1000;
  return tag(c + (khz ? ' ' + khz + 'k' : ''));
}

// duration renders a live counter; the ticker below updates it every second.
function duration(epoch) {
  const e = Number(epoch);
  if (!e) return '';
  return h('span', { class: 'num', 'data-epoch': String(e), text: fmtDuration(Date.now() / 1000 - e) });
}

export function direction(dir) {
  const d = cellText(dir);
  const inbound = d === 'inbound';
  return h('span', { class: inbound ? 'dir in' : 'dir out' }, icon(inbound ? 'in' : 'out'), inbound ? t('calls.in') : t('calls.out'));
}

export function mountCalls({ id, scope, root, ctx }) {
  const canOperate = ctx.can('operate');
  let mode = 'channels';
  let query = '';
  let channels = null;
  let calls = null;
  let selected = null;
  let lastError = null;
  let signature = '';
  const sorts = { channels: sortState(() => render()), calls: sortState(() => render()) };

  const seg = segmented([{ key: 'channels', label: t('calls.channels') }, { key: 'calls', label: t('calls.bridged') }, { key: 'hangups', label: t('calls.hangups') }], mode, (k) => {
    mode = k;
    signature = '';
    search.input.placeholder = k === 'hangups' ? t('hangups.filter') : t('calls.filter');
    search.input.setAttribute('aria-label', search.input.placeholder);
    hangups.show(k === 'hangups');
    render();
  });
  const search = searchBox(t('calls.filter'), (q) => {
    query = q;
    render();
  });
  const live = h('span', { class: 'live', text: t('calls.live') });
  const p = panel({ title: t('calls.channels'), flush: true });
  const host = tableHost({ rows: 6, cols: 4 });
  p.body.append(host.el);
  const hangups = hangupsSegment({ id, scope, host, panel: p });
  root.append(h('div', { class: 'toolbar' }, seg.el, search.el, hangups.filterEl, h('span', { class: 'spacer' }), live), p.el);

  const tick = setInterval(() => {
    const now = Date.now() / 1000;
    for (const el of root.querySelectorAll('[data-epoch]')) el.textContent = fmtDuration(now - Number(el.dataset.epoch));
  }, 1000);
  scope.onCleanup(() => clearInterval(tick));

  async function hangup(row) {
    const who = cellText(row.cid_name) || cellText(row.cid_num) || cellText(row.name);
    const yes = await confirmDialog({
      title: t('calls.hangup.title'),
      message: t('calls.hangup.text', { who, dest: cellText(row.dest) || t('calls.unknownDest') }),
      confirmLabel: t('calls.hangup'), danger: true,
    });
    if (!yes || scope.disposed) return;
    try {
      await api('DELETE', instPath(id, 'channels/' + encodeURIComponent(cellText(row.uuid))), { signal: scope.signal });
      toast(t('calls.hungUp'));
      refresh();
    } catch (err) {
      toastError(err, t('calls.hangupFailed'));
    }
  }

  function details(row) {
    selected = cellText(row.uuid);
    const d = openDrawer({ kicker: t('calls.kind'), title: cellText(row.name) || selected, subtitle: selected, onClose: () => { selected = null; } });
    if (canOperate) {
      d.body.append(h('div', { class: 'drawer-actions' },
        h('button', { type: 'button', class: 'btn btn-danger', on: { click: () => hangup(row) } }, icon('hangup'), t('calls.hangup'))));
    }
    d.body.append(...detailBody(row, ['uuid', 'direction', 'callstate', 'state', 'cid_name', 'cid_num', 'dest', 'application', 'application_data', 'read_codec', 'created']).filter(Boolean));
  }

  const hangupCol = canOperate ? { label: '', right: true, render: (r) => iconButton('hangup', t('calls.hangup'), () => hangup(r), 'btn-ghost btn-danger') } : null;

  const channelCols = [
    { label: t('calls.col.dir'), sortBy: (r) => cellText(r.direction), render: (r) => direction(r.direction) },
    { label: t('calls.col.caller'), sortBy: (r) => cellText(r.cid_name) || cellText(r.cid_num), render: (r) => party(r.cid_name, r.cid_num) },
    { label: t('calls.col.dest'), mono: true, sortBy: (r) => cellText(r.dest), render: (r) => clip(r.dest) },
    { label: t('calls.col.state'), sortBy: (r) => cellText(r.callstate), render: (r) => statePill(r.callstate) },
    { label: t('calls.col.app'), mono: true, sortBy: (r) => cellText(r.application), render: (r) => (r.application ? clip(cellText(r.application) + (r.application_data ? ' ' + cellText(r.application_data) : ''), 'medium') : '') },
    { label: t('calls.col.codec'), sortBy: (r) => cellText(r.read_codec), render: (r) => codec(r.read_codec, r.read_rate) },
    // a later start is a shorter call, hence the sign
    { label: t('calls.col.duration'), right: true, sortBy: (r) => -(Number(r.created_epoch) || 0), render: (r) => duration(r.created_epoch) },
    { label: t('calls.col.channel'), mono: true, sortBy: (r) => cellText(r.name), render: (r) => clip(r.name, 'faint narrow') },
    hangupCol,
  ].filter(Boolean);

  const callCols = [
    { label: t('calls.col.from'), sortBy: (r) => cellText(r.cid_name) || cellText(r.cid_num), render: (r) => party(r.cid_name, r.cid_num) },
    { label: t('calls.col.to'), sortBy: (r) => cellText(r.b_callee_num || r.dest), render: (r) => party(r.b_callee_name || r.dest, r.b_callee_num || r.dest) },
    { label: t('calls.col.state'), sortBy: (r) => cellText(r.callstate), render: (r) => statePill(r.callstate) },
    { label: t('calls.col.aleg'), mono: true, sortBy: (r) => cellText(r.name), render: (r) => clip(r.name, 'faint medium') },
    { label: t('calls.col.bleg'), mono: true, sortBy: (r) => cellText(r.b_name), render: (r) => clip(r.b_name, 'faint medium') },
    { label: t('calls.col.codec'), sortBy: (r) => cellText(r.read_codec), render: (r) => codec(r.read_codec, r.read_rate) },
    { label: t('calls.col.duration'), right: true, sortBy: (r) => -(Number(r.call_created_epoch || r.created_epoch) || 0), render: (r) => duration(r.call_created_epoch || r.created_epoch) },
    hangupCol,
  ].filter(Boolean);

  function render() {
    if (mode === 'hangups') {
      // the other two segments keep their counts current
      if (!lastError && channels && calls) {
        seg.setCount('channels', channels.length);
        seg.setCount('calls', calls.filter((c) => c.b_uuid).length);
      }
      hangups.render(query);
      return;
    }
    if (lastError) {
      signature = '';
      host.error(lastError);
      return;
    }
    if (!channels || !calls) return;
    const bridged = calls.filter((c) => c.b_uuid);
    seg.setCount('channels', channels.length);
    seg.setCount('calls', bridged.length);
    const all = mode === 'channels' ? channels : bridged;
    const rows = all.filter((r) => rowMatches(r, query));
    // An unchanged table is left alone so hover and focus survive the refresh.
    const sort = sorts[mode];
    const sig = JSON.stringify([mode, query, selected, sort.col, sort.dir, rows]);
    if (sig === signature) return;
    signature = sig;
    p.setTitle(mode === 'channels' ? t('calls.channels') : t('calls.bridged'));
    p.setCount(query ? t('common.nOfM', { n: fmtInt(rows.length), m: fmtInt(all.length) }) : all.length);
    host.show(dataTable({
      rows,
      columns: mode === 'channels' ? channelCols : callCols,
      sort,
      keyOf: (r) => cellText(r.uuid),
      selected,
      onRow: details,
      empty: query
        ? emptyState({ icon: 'search', title: t('common.noMatch'), text: t('common.noMatchText') })
        : emptyState({ icon: 'calls', title: mode === 'channels' ? t('calls.noChannels') : t('calls.noBridged'), text: t('calls.idleText') }),
    }));
  }

  const refresh = scope.poll(async (signal) => {
    try {
      const [ch, ca] = await Promise.all([
        api('GET', instPath(id, 'channels'), { signal }),
        api('GET', instPath(id, 'calls'), { signal }),
      ]);
      channels = rowsOf(ch);
      calls = rowsOf(ca);
      lastError = null;
    } catch (err) {
      if (isAbort(err)) throw err;
      lastError = err;
      seg.setCount('channels', null);
      seg.setCount('calls', null);
      if (mode !== 'hangups') p.setCount(null);
    }
    render();
  }, 3000);
}
