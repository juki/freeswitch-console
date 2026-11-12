// Calls: channels and bridged calls, refreshed every 3s; admin can hang up.

import { api, instPath, isAbort, rowsOf } from '../api.js';
import { h, DASH, cellText, fmtDuration } from '../dom.js';
import { icon } from '../icons.js';
import { clip, confirmDialog, dataTable, detailBody, emptyState, iconButton, openDrawer, panel, rowMatches, searchBox, segmented, statePill, tableHost, tag, toast, toastError } from '../ui.js';

function party(name, num) {
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

function direction(dir) {
  const d = cellText(dir);
  const inbound = d === 'inbound';
  return h('span', { class: inbound ? 'dir in' : 'dir out' }, icon(inbound ? 'in' : 'out'), inbound ? 'In' : 'Out');
}

export function mountCalls({ id, scope, root, isAdmin }) {
  let mode = 'channels';
  let query = '';
  let channels = null;
  let calls = null;
  let selected = null;
  let lastError = null;
  let signature = '';

  const seg = segmented([{ key: 'channels', label: 'Channels' }, { key: 'calls', label: 'Bridged calls' }], mode, (k) => {
    mode = k;
    render();
  });
  const search = searchBox('Filter channels', (q) => {
    query = q;
    render();
  });
  const live = h('span', { class: 'faint', text: 'Live · refreshes every 3 s' });
  const p = panel({ title: 'Channels', flush: true });
  const host = tableHost();
  p.body.append(host.el);
  root.append(h('div', { class: 'toolbar' }, seg.el, search.el, h('span', { class: 'spacer' }), live), p.el);

  const tick = setInterval(() => {
    const now = Date.now() / 1000;
    for (const el of root.querySelectorAll('[data-epoch]')) el.textContent = fmtDuration(now - Number(el.dataset.epoch));
  }, 1000);
  scope.onCleanup(() => clearInterval(tick));

  async function hangup(row) {
    const who = cellText(row.cid_name) || cellText(row.cid_num) || cellText(row.name);
    const yes = await confirmDialog({
      title: 'Hang up this channel?',
      message: who + ' → ' + (cellText(row.dest) || 'unknown destination') + '. The call ends immediately.',
      confirmLabel: 'Hang up', danger: true,
    });
    if (!yes || scope.disposed) return;
    try {
      await api('DELETE', instPath(id, 'channels/' + encodeURIComponent(cellText(row.uuid))), { signal: scope.signal });
      toast('Channel hung up.');
      refresh();
    } catch (err) {
      toastError(err, 'Hang up failed');
    }
  }

  function details(row) {
    selected = cellText(row.uuid);
    const d = openDrawer({ title: cellText(row.name) || selected, subtitle: 'Channel ' + selected, onClose: () => { selected = null; } });
    if (isAdmin) {
      d.body.append(h('div', { class: 'drawer-actions' },
        h('button', { type: 'button', class: 'btn btn-danger', on: { click: () => hangup(row) } }, icon('hangup'), 'Hang up')));
    }
    d.body.append(...detailBody(row, ['uuid', 'direction', 'callstate', 'state', 'cid_name', 'cid_num', 'dest', 'application', 'application_data', 'read_codec', 'created']).filter(Boolean));
  }

  const hangupCol = isAdmin ? { label: '', right: true, render: (r) => iconButton('hangup', 'Hang up', () => hangup(r), 'btn-ghost btn-danger') } : null;

  const channelCols = [
    { label: 'Dir', render: (r) => direction(r.direction) },
    { label: 'Caller', render: (r) => party(r.cid_name, r.cid_num) },
    { label: 'Destination', mono: true, render: (r) => clip(r.dest) },
    { label: 'State', render: (r) => statePill(r.callstate) },
    { label: 'Application', mono: true, render: (r) => (r.application ? clip(cellText(r.application) + (r.application_data ? ' ' + cellText(r.application_data) : ''), 'medium') : '') },
    { label: 'Codec', render: (r) => codec(r.read_codec, r.read_rate) },
    { label: 'Duration', right: true, render: (r) => duration(r.created_epoch) },
    { label: 'Channel', mono: true, render: (r) => clip(r.name, 'faint narrow') },
    hangupCol,
  ].filter(Boolean);

  const callCols = [
    { label: 'From', render: (r) => party(r.cid_name, r.cid_num) },
    { label: 'To', render: (r) => party(r.b_callee_name || r.dest, r.b_callee_num || r.dest) },
    { label: 'State', render: (r) => statePill(r.callstate) },
    { label: 'A leg', mono: true, render: (r) => clip(r.name, 'faint medium') },
    { label: 'B leg', mono: true, render: (r) => clip(r.b_name, 'faint medium') },
    { label: 'Codec', render: (r) => codec(r.read_codec, r.read_rate) },
    { label: 'Duration', right: true, render: (r) => duration(r.call_created_epoch || r.created_epoch) },
    hangupCol,
  ].filter(Boolean);

  function render() {
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
    const sig = JSON.stringify([mode, query, selected, rows]);
    if (sig === signature) return;
    signature = sig;
    p.el.querySelector('.panel-title').textContent = mode === 'channels' ? 'Channels' : 'Bridged calls';
    p.setCount(query ? rows.length + ' of ' + all.length : all.length);
    host.show(dataTable({
      rows,
      columns: mode === 'channels' ? channelCols : callCols,
      keyOf: (r) => cellText(r.uuid),
      selected,
      onRow: details,
      empty: query
        ? emptyState({ icon: 'search', title: 'No match', text: 'Nothing matches the filter.' })
        : emptyState({ icon: 'calls', title: mode === 'channels' ? 'No active channels' : 'No bridged calls', text: 'This FreeSWITCH is idle right now. New calls show up here within seconds.' }),
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
      p.setCount(null);
    }
    render();
  }, 3000);
}
