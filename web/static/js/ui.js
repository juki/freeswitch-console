// Shared UI components: toasts, dialogs, the detail drawer, tables, pills,
// sparklines and meters.

import { h, s, cellText, DASH } from './dom.js';
import { icon } from './icons.js';

function cx(...names) {
  return names.filter(Boolean).join(' ') || undefined;
}

// ---------- feedback ----------

export function toast(message, kind = 'ok', title) {
  const item = h('div', { class: 'toast ' + kind },
    h('div', { class: 'toast-body' }, title ? h('div', { class: 'toast-title', text: title }) : null, message),
    h('button', { type: 'button', class: 'btn btn-ghost btn-icon btn-sm', 'aria-label': 'Dismiss', on: { click: () => item.remove() } }, icon('x')));
  document.getElementById('toasts').append(item);
  setTimeout(() => item.remove(), kind === 'error' ? 8000 : 4500);
}

// toastError reports a failed action; aborted requests are not failures.
export function toastError(err, title) {
  if (err && err.name === 'AbortError') return;
  const msg = err && err.message ? err.message : String(err);
  toast(err && err.status === 503 ? 'FreeSWITCH is not connected: ' + msg : msg, 'error', title);
}

// openModal shows a modal dialog and returns {dialog, close}. The dialog is
// removed from the document when it closes.
export function openModal({ title, body, footer, onClose }) {
  const dialog = h('dialog', { class: 'modal' },
    h('div', { class: 'modal-head' }, h('h2', { class: 'modal-title', text: title })),
    h('div', { class: 'modal-body' }, body),
    h('div', { class: 'modal-foot' }, footer));
  dialog.addEventListener('close', () => {
    dialog.remove();
    if (onClose) onClose();
  });
  document.body.append(dialog);
  dialog.showModal();
  return { dialog, close: () => dialog.close() };
}

// confirmDialog resolves to true when the user confirms.
export function confirmDialog({ title, message, confirmLabel = 'Confirm', danger = false }) {
  return new Promise((resolve) => {
    let result = false;
    const cancel = h('button', { type: 'button', class: 'btn', text: 'Cancel' });
    const ok = h('button', { type: 'button', class: danger ? 'btn btn-danger-solid' : 'btn btn-primary', text: confirmLabel });
    const m = openModal({
      title,
      body: h('p', { class: 'modal-text', text: message }),
      footer: [cancel, ok],
      onClose: () => resolve(result),
    });
    cancel.addEventListener('click', m.close);
    ok.addEventListener('click', () => {
      result = true;
      m.close();
    });
  });
}

// ---------- drawer ----------

let drawer = null;

export function closeDrawer() {
  if (!drawer) return;
  const d = drawer;
  drawer = null;
  d.backdrop.remove();
  d.el.remove();
  document.removeEventListener('keydown', d.onKey);
  if (d.onClose) d.onClose();
}

// openDrawer opens the right-hand detail panel (one at a time) and returns
// {body, setTitle, close}.
export function openDrawer({ title, subtitle, onClose }) {
  closeDrawer();
  const titleEl = h('div', { class: 'drawer-title', text: title });
  const subEl = h('div', { class: 'drawer-sub', text: subtitle || '' });
  const body = h('div', { class: 'drawer-body' });
  const el = h('aside', { class: 'drawer', role: 'dialog', 'aria-label': title },
    h('div', { class: 'drawer-head' },
      h('div', { class: 'spacer' }, titleEl, subEl),
      h('button', { type: 'button', class: 'btn btn-ghost btn-icon', 'aria-label': 'Close', on: { click: closeDrawer } }, icon('x'))),
    body);
  const backdrop = h('div', { class: 'drawer-backdrop', on: { click: closeDrawer } });
  const onKey = (ev) => {
    if (ev.key === 'Escape') closeDrawer();
  };
  document.addEventListener('keydown', onKey);
  document.body.append(backdrop, el);
  drawer = { el, backdrop, onKey, onClose };
  return {
    body,
    setTitle(t, sub) {
      titleEl.textContent = t;
      subEl.textContent = sub || '';
    },
    close: closeDrawer,
  };
}

// detailBody lists every field of a row: the non-empty ones as key/value
// pairs (primary keys first), the empty ones as a single muted line.
export function detailBody(row, primary = []) {
  const keys = Object.keys(row);
  const filled = keys.filter((k) => cellText(row[k]) !== '');
  const ordered = [...primary.filter((k) => filled.includes(k)), ...filled.filter((k) => !primary.includes(k))];
  const blank = keys.filter((k) => cellText(row[k]) === '');
  return [
    h('div', { class: 'drawer-section', text: 'Fields' }),
    kv(ordered.map((k) => [k, cellText(row[k])])),
    blank.length ? h('div', { class: 'drawer-section', text: 'Empty fields (' + blank.length + ')' }) : null,
    blank.length ? h('p', { class: 'mono faint', text: blank.join(', ') }) : null,
  ];
}

// ---------- small pieces ----------

export function pill(text, tone, plain) {
  return h('span', { class: cx('pill', tone && 'tone-' + tone, plain && 'plain'), text });
}

// stateTone maps FreeSWITCH state words to a colour tone.
export function stateTone(text) {
  const t = String(text || '').toUpperCase();
  if (/^(RUNNING|REGED|UP|ACTIVE|READY)\b/.test(t)) return 'ok';
  if (/^(EARLY|RINGING|RING_WAIT|TRYING|REGISTER|UNREGISTER)\b/.test(t)) return 'info';
  if (/^(HELD|UNHELD|UNREGED|EXPIRED)\b/.test(t)) return 'warn';
  if (/^(FAIL|DOWN|TIMEOUT|HANGUP|NOT READY)/.test(t)) return 'bad';
  return '';
}

export function statePill(text) {
  const t = cellText(text);
  return t ? pill(t, stateTone(t)) : h('span', { class: 'faint', text: DASH });
}

export function tag(text) {
  return h('span', { class: 'tag', text });
}

// clip truncates long text with an ellipsis and keeps the full value as a tooltip.
export function clip(text, cls) {
  const t = cellText(text);
  return h('span', { class: cx('clip', cls), text: t, title: t.length > 28 ? t : undefined });
}

export function iconButton(name, label, onClick, cls) {
  return h('button', { type: 'button', class: cx('btn btn-icon btn-sm', cls || 'btn-ghost'), 'aria-label': label, title: label, on: { click: onClick } }, icon(name));
}

export function emptyState({ icon: name = 'box', title, text, action, tone }) {
  return h('div', { class: cx('empty', tone) },
    icon(name),
    h('div', { class: 'empty-title', text: title }),
    text ? h('div', { class: 'empty-text', text }) : null,
    action || null);
}

// errorState renders an API error. 503 (instance not connected) is a calm
// state, everything else an error.
export function errorState(err) {
  if (err && err.status === 503) {
    return emptyState({ icon: 'plug', tone: 'calm', title: 'Not connected', text: 'fsdeck has no Event Socket connection to this FreeSWITCH right now. Data comes back when the connection does.' });
  }
  const msg = err && err.message ? err.message : String(err);
  return emptyState({ icon: 'alert', tone: 'error', title: err && err.status === 403 ? 'Not allowed' : 'Could not load', text: msg });
}

export function loading() {
  return h('div', { class: 'loading', text: 'Loading…' });
}

export function panel({ title, actions, flush }) {
  const countEl = h('span', { class: 'panel-count' });
  const body = h('div', { class: flush ? 'panel-body flush' : 'panel-body' });
  const el = h('section', { class: 'panel' },
    h('div', { class: 'panel-head' }, h('h2', { class: 'panel-title', text: title }), countEl, h('span', { class: 'spacer' }), actions || null),
    body);
  return {
    el,
    body,
    setCount(n) {
      countEl.textContent = n === null || n === undefined ? '' : String(n);
    },
  };
}

export function kv(pairs) {
  return h('dl', { class: 'kv' }, pairs.map(([k, v]) => [h('dt', { text: k }), h('dd', null, v === '' || v === null || v === undefined ? DASH : v)]));
}

export function searchBox(placeholder, onInput) {
  const input = h('input', { type: 'search', class: 'input', placeholder, 'aria-label': placeholder, spellcheck: false, autocomplete: 'off' });
  input.addEventListener('input', () => onInput(input.value.trim().toLowerCase()));
  return { el: h('label', { class: 'search' }, icon('search'), input), input };
}

// rowMatches reports whether any field of the row contains q (lower case).
export function rowMatches(row, q) {
  return !q || Object.values(row).some((v) => cellText(v).toLowerCase().includes(q));
}

export function segmented(options, value, onChange) {
  const buttons = new Map();
  const counts = new Map();
  const set = (key) => {
    for (const [k, b] of buttons) b.setAttribute('aria-pressed', String(k === key));
  };
  const el = h('div', { class: 'segmented', role: 'group' }, options.map((o) => {
    const count = h('span', { class: 'count' });
    const b = h('button', { type: 'button', on: { click: () => { set(o.key); onChange(o.key); } } }, o.label, count);
    buttons.set(o.key, b);
    counts.set(o.key, count);
    return b;
  }));
  set(value);
  return {
    el,
    set,
    setCount(key, n) {
      counts.get(key).textContent = n === null || n === undefined ? '' : String(n);
    },
  };
}

// ---------- tables ----------

// dataTable renders rows with curated columns. A column is
// {label, key | render(row), right, mono, wrap}. onRow makes rows clickable.
export function dataTable({ columns, rows, onRow, keyOf, selected, empty }) {
  if (!rows.length) return empty instanceof Node ? empty : emptyState({ title: empty || 'Nothing to show' });
  const head = h('tr', null, columns.map((c) => h('th', { scope: 'col', class: c.right ? 'right' : undefined, text: c.label })));
  const body = rows.map((row) => {
    const isSelected = keyOf && selected !== undefined && selected !== null && keyOf(row) === selected;
    const tr = h('tr', { class: cx(onRow && 'clickable', isSelected && 'selected'), tabIndex: onRow ? 0 : undefined },
      columns.map((c) => {
        const v = c.render ? c.render(row) : cellText(row[c.key]);
        return h('td', { class: cx(c.right && 'right', c.mono && 'mono', c.wrap && 'wrap') },
          v === '' || v === null || v === undefined ? h('span', { class: 'faint', text: DASH }) : v);
      }));
    if (onRow) {
      tr.addEventListener('click', (ev) => {
        if (!ev.target.closest('button, a')) onRow(row);
      });
      tr.addEventListener('keydown', (ev) => {
        if (ev.key === 'Enter' && ev.target === tr) onRow(row);
      });
    }
    return tr;
  });
  return h('table', { class: 'tbl' }, h('thead', null, head), h('tbody', null, body));
}

// tableHost is a scroll container whose content can be swapped without
// losing the scroll position.
export function tableHost() {
  const el = h('div', { class: 'tbl-wrap' }, loading());
  return {
    el,
    show(node) {
      el.replaceChildren(node);
    },
    error(err) {
      el.replaceChildren(errorState(err));
    },
  };
}

// ---------- charts ----------

// sparkline keeps the last `capacity` samples. zero anchors the scale at 0;
// otherwise the scale follows the data range.
export function sparkline({ tone, zero = true, capacity = 60 } = {}) {
  const W = 132;
  const H = 40;
  const PAD = 3;
  const area = s('path', { class: 'area', d: '' });
  const line = s('path', { class: 'line', d: '' });
  const last = s('circle', { class: 'last', r: 2.6, cx: -10, cy: -10 });
  const el = s('svg', { class: cx('spark', tone), viewBox: `0 0 ${W} ${H}`, 'aria-hidden': 'true' }, area, line, last);
  const values = [];

  function draw() {
    if (values.length < 2) return;
    let lo = zero ? 0 : Math.min(...values);
    let hi = Math.max(...values);
    if (hi - lo < 1e-9) {
      // a flat series: draw it in the middle (or on the floor when it is 0)
      hi = lo + 1;
      if (!zero || lo !== 0 || values[0] !== 0) lo -= 1;
    }
    // The series always spans the full width; it only starts scrolling once
    // `capacity` samples are in.
    const step = (W - 2 * PAD) / (values.length - 1);
    const x0 = PAD;
    const pts = values.map((v, i) => [x0 + i * step, H - PAD - ((v - lo) / (hi - lo)) * (H - 2 * PAD)]);
    const d = pts.map(([x, y], i) => (i ? 'L' : 'M') + x.toFixed(1) + ' ' + y.toFixed(1)).join(' ');
    line.setAttribute('d', d);
    area.setAttribute('d', d + ` L${pts[pts.length - 1][0].toFixed(1)} ${H} L${pts[0][0].toFixed(1)} ${H} Z`);
    last.setAttribute('cx', pts[pts.length - 1][0].toFixed(1));
    last.setAttribute('cy', pts[pts.length - 1][1].toFixed(1));
  }

  return {
    el,
    push(v) {
      const n = Number(v);
      if (!Number.isFinite(n)) return;
      values.push(n);
      if (values.length > capacity) values.shift();
      draw();
    },
  };
}

export function meter() {
  const fill = s('rect', { class: 'fill', x: 0, y: 0, width: 0, height: 6 });
  const el = s('svg', { class: 'meter', viewBox: '0 0 100 6', preserveAspectRatio: 'none', 'aria-hidden': 'true' },
    s('rect', { class: 'track', x: 0, y: 0, width: 100, height: 6 }), fill);
  return {
    el,
    set(fraction, tone) {
      const f = Math.min(1, Math.max(0, Number(fraction) || 0));
      // keep a sliver visible so a tiny non-zero load does not look like zero
      fill.setAttribute('width', f > 0 ? Math.max(1.2, f * 100).toFixed(1) : 0);
      el.setAttribute('class', cx('meter', tone));
    },
  };
}

// ---------- clipboard ----------

export async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // The Clipboard API needs a secure context; fall back to a selection copy.
    const ta = h('textarea', { class: 'sr-only', value: text, readOnly: true });
    document.body.append(ta);
    ta.select();
    let ok = false;
    try {
      ok = document.execCommand('copy');
    } catch {
      ok = false;
    }
    ta.remove();
    return ok;
  }
}
