// Shared UI components: toasts, dialogs, the detail drawer, tables, pills,
// skeletons, sparklines and meters. The time-series chart is in chart.js.

import { h, s, cellText, DASH } from './dom.js';
import { LANGS, lang, setLang, t } from './i18n.js';
import { icon } from './icons.js';

function cx(...names) {
  return names.filter(Boolean).join(' ') || undefined;
}

// ---------- feedback ----------

const TOAST_MAX = 4;
const TOAST_ICONS = { ok: 'check', error: 'alert', info: 'info' };

export function toast(message, kind = 'ok', title) {
  const host = document.getElementById('toasts');
  const item = h('div', { class: 'toast ' + kind, role: kind === 'error' ? 'alert' : undefined },
    h('span', { class: 'toast-icon' }, icon(TOAST_ICONS[kind] || 'info')),
    h('div', { class: 'toast-body' }, title ? h('div', { class: 'toast-title', text: title }) : null, message),
    h('button', { type: 'button', class: 'btn btn-ghost btn-icon btn-sm', 'aria-label': t('common.dismiss'), on: { click: () => item.remove() } }, icon('x')));
  host.append(item);
  // A burst of toasts keeps only the newest few on screen.
  while (host.childElementCount > TOAST_MAX) host.firstElementChild.remove();
  setTimeout(() => item.remove(), kind === 'error' ? 8000 : 4500);
}

// toastError reports a failed action; aborted requests are not failures.
export function toastError(err, title) {
  if (err && err.name === 'AbortError') return;
  const msg = err && err.message ? err.message : String(err);
  toast(err && err.status === 503 ? t('error.notConnectedMsg', { msg }) : msg, 'error', title);
}

// openModal shows a modal dialog and returns {dialog, close}. The dialog is
// removed from the document when it closes, and focus goes back to where it was.
export function openModal({ title, body, footer, onClose }) {
  const back = document.activeElement;
  const dialog = h('dialog', { class: 'modal', 'aria-label': title },
    h('div', { class: 'modal-head' }, h('h2', { class: 'modal-title', text: title })),
    h('div', { class: 'modal-body' }, body),
    h('div', { class: 'modal-foot' }, footer));
  dialog.addEventListener('close', () => {
    dialog.remove();
    if (back && back.isConnected && typeof back.focus === 'function') back.focus();
    if (onClose) onClose();
  });
  document.body.append(dialog);
  dialog.showModal();
  return { dialog, close: () => dialog.close() };
}

// confirmDialog resolves to true when the user confirms.
export function confirmDialog({ title, message, confirmLabel, danger = false }) {
  return new Promise((resolve) => {
    let result = false;
    const cancel = h('button', { type: 'button', class: 'btn', text: t('common.cancel') });
    const ok = h('button', { type: 'button', class: danger ? 'btn btn-danger-solid' : 'btn btn-primary', text: confirmLabel || t('common.confirm') });
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
    // a destructive action never gets the default focus
    (danger ? cancel : ok).focus();
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
  if (d.back && d.back.isConnected && typeof d.back.focus === 'function') d.back.focus({ preventScroll: true });
  if (d.onClose) d.onClose();
}

// openDrawer opens the right-hand detail panel (one at a time) and returns
// {body, setTitle, close}.
export function openDrawer({ title, subtitle, kicker, onClose }) {
  const back = drawer ? drawer.back : document.activeElement;
  closeDrawer();
  const titleEl = h('div', { class: 'drawer-title', text: title });
  const subEl = h('div', { class: 'drawer-sub', text: subtitle || '' });
  const body = h('div', { class: 'drawer-body' });
  const closeBtn = h('button', { type: 'button', class: 'btn btn-ghost btn-icon', 'aria-label': t('common.close'), title: t('drawer.closeEsc'), on: { click: closeDrawer } }, icon('x'));
  const el = h('aside', { class: 'drawer', role: 'dialog', 'aria-label': title },
    h('div', { class: 'drawer-head' },
      h('div', { class: 'spacer' }, kicker ? h('div', { class: 'drawer-kicker', text: kicker }) : null, titleEl, subEl),
      closeBtn),
    body);
  const backdrop = h('div', { class: 'drawer-backdrop', on: { click: closeDrawer } });
  const onKey = (ev) => {
    if (ev.key === 'Escape' && !document.querySelector('dialog[open]')) closeDrawer();
  };
  document.addEventListener('keydown', onKey);
  document.body.append(backdrop, el);
  drawer = { el, backdrop, onKey, onClose, back };
  closeBtn.focus({ preventScroll: true });
  return {
    body,
    setTitle(t, sub) {
      titleEl.textContent = t;
      subEl.textContent = sub || '';
    },
    close: closeDrawer,
  };
}

// copyList is a key/value list whose values copy themselves on click.
function copyList(pairs) {
  return h('dl', { class: 'kv' }, pairs.map(([k, v]) => {
    const glyph = h('span', { class: 'copy-glyph' }, icon('copy'));
    const btn = h('button', { type: 'button', class: 'copy-val', title: t('drawer.copyValue'), 'aria-label': t('drawer.copyKey', { key: k }) }, h('span', { class: 'copy-text', text: v }), glyph);
    const reset = () => {
      if (!btn.classList.contains('copied')) return;
      btn.classList.remove('copied');
      glyph.replaceChildren(icon('copy'));
    };
    btn.addEventListener('click', async () => {
      if (await copyText(v)) {
        btn.classList.add('copied');
        glyph.replaceChildren(icon('check'));
      } else {
        toast(t('copy.denied'), 'error');
      }
    });
    btn.addEventListener('mouseleave', reset);
    btn.addEventListener('blur', reset);
    const row = h('div', { class: 'kv-row' }, h('dt', { text: k }), h('dd', null, btn));
    row.dataset.k = (k + ' ' + v).toLowerCase();
    return row;
  }));
}

// detailBody lists every field of a row: the primary keys first, then the
// other non-empty fields alphabetically, then the empty ones as one muted
// line. A filter box narrows the lists; every value copies on click.
export function detailBody(row, primary = []) {
  const keys = Object.keys(row);
  const filled = keys.filter((k) => cellText(row[k]) !== '');
  const top = primary.filter((k) => filled.includes(k));
  const rest = filled.filter((k) => !primary.includes(k)).sort((a, b) => a.localeCompare(b));
  const blank = keys.filter((k) => cellText(row[k]) === '');
  const section = (title, list) => ({
    head: h('div', { class: 'drawer-section', text: title }),
    dl: copyList(list.map((k) => [k, cellText(row[k])])),
  });
  const parts = [
    top.length ? section(t('drawer.summary'), top) : null,
    rest.length ? section(top.length ? t('drawer.otherFields', { n: rest.length }) : t('drawer.fields', { n: rest.length }), rest) : null,
  ].filter(Boolean);
  const none = h('p', { class: 'note-empty', text: t('drawer.noFieldMatch'), hidden: true });
  const filter = searchBox(t('drawer.filterFields', { n: filled.length }), (q) => {
    let any = false;
    for (const p of parts) {
      let shown = 0;
      for (const r of p.dl.children) {
        r.hidden = Boolean(q) && !r.dataset.k.includes(q);
        if (!r.hidden) shown++;
      }
      p.head.hidden = !shown;
      p.dl.hidden = !shown;
      any = any || shown > 0;
    }
    none.hidden = any;
  }, 'sm');
  return [
    h('div', { class: 'drawer-filter' }, filter.el),
    ...parts.flatMap((p) => [p.head, p.dl]),
    none,
    blank.length ? h('details', null,
      h('summary', { text: t('drawer.emptyFields', { n: blank.length }) }),
      h('p', { class: 'mono faint', text: blank.join(', ') })) : null,
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

// popover attaches a floating panel to a trigger button: click toggles it,
// Escape or a click outside closes it, arrow keys move between the panel's
// items. Listeners exist only while it is open. align: 'end' (default),
// 'start' or 'center'; up opens it above the trigger.
export function popover(trigger, panel, { up = false, align = 'end', label } = {}) {
  panel.classList.add('pop-panel', 'align-' + align);
  if (up) panel.classList.add('up');
  panel.setAttribute('role', 'menu');
  if (label) panel.setAttribute('aria-label', label);
  panel.hidden = true;
  trigger.setAttribute('aria-haspopup', 'menu');
  trigger.setAttribute('aria-expanded', 'false');
  const el = h('div', { class: 'pop' }, trigger, panel);
  const items = () => [...panel.querySelectorAll('button:not([disabled]), a[href]')];
  const onDown = (ev) => {
    if (!el.contains(ev.target)) close();
  };
  const onKey = (ev) => {
    if (ev.key === 'Escape') {
      ev.stopPropagation();
      close();
      trigger.focus();
    } else if (ev.key === 'ArrowDown' || ev.key === 'ArrowUp') {
      ev.preventDefault();
      const list = items();
      if (!list.length) return;
      const i = list.indexOf(document.activeElement);
      list[(i + (ev.key === 'ArrowDown' ? 1 : list.length - 1)) % list.length].focus();
    }
  };
  function open() {
    panel.hidden = false;
    trigger.setAttribute('aria-expanded', 'true');
    document.addEventListener('pointerdown', onDown);
    el.addEventListener('keydown', onKey);
    const first = panel.querySelector('[aria-checked="true"]') || items()[0];
    if (first) first.focus();
  }
  function close() {
    panel.hidden = true;
    trigger.setAttribute('aria-expanded', 'false');
    document.removeEventListener('pointerdown', onDown);
    el.removeEventListener('keydown', onKey);
  }
  trigger.addEventListener('click', () => (panel.hidden ? open() : close()));
  return { el, open, close };
}

// langItems are the menu entries that switch the language.
export function langItems(onPick) {
  return LANGS.map((l) => h('button', {
    type: 'button', class: 'menu-item', role: 'menuitemradio', lang: l.code, 'aria-checked': String(l.code === lang()),
    on: { click: () => { if (onPick) onPick(); setLang(l.code); } },
  }, h('span', { class: 'lang-code', text: l.short }), h('span', { class: 'menu-text', text: l.label }), l.code === lang() ? icon('check', 'menu-check') : null));
}

// langMenu is the stand-alone language picker of the sign-in screen: a globe
// button with the language name that opens a small menu.
export function langMenu() {
  const active = LANGS.find((l) => l.code === lang()) || LANGS[0];
  const btn = h('button', { type: 'button', class: 'btn btn-ghost btn-sm lang-btn', 'aria-label': t('lang.choose'), title: t('lang.choose') },
    icon('globe'), h('span', { text: active.label }));
  const panel = h('div', { class: 'menu' });
  const pop = popover(btn, panel, { label: t('lang.choose') });
  panel.append(...langItems(() => pop.close()));
  return pop.el;
}

export function iconButton(name, label, onClick, cls) {
  return h('button', { type: 'button', class: cx('btn btn-icon btn-sm', cls || 'btn-ghost'), 'aria-label': label, title: label, on: { click: onClick } }, icon(name));
}

export function emptyState({ icon: name = 'box', title, text, action, tone }) {
  return h('div', { class: cx('empty', tone) },
    h('span', { class: 'empty-icon' }, icon(name)),
    h('div', { class: 'empty-title', text: title }),
    text ? h('div', { class: 'empty-text', text }) : null,
    action || null);
}

// PAID describes the licensed features for the "needs a licence" state.
const PAID = {
  users: () => ({ icon: 'users', title: t('paid.users.title'), text: t('paid.users.text') }),
  audit: () => ({ icon: 'shield', title: t('paid.audit.title'), text: t('paid.audit.text') }),
  'config-edit': () => ({ icon: 'sliders', title: t('paid.config.title'), text: t('paid.config.text') }),
};

// paidState is what a screen shows when the server answers 402: what the
// feature does and that this server has no licence for it.
export function paidState(feature, serverText) {
  const f = (PAID[feature] || PAID.users)();
  return h('div', { class: 'paid' },
    h('div', { class: 'paid-card' },
      h('span', { class: 'paid-icon' }, icon(f.icon), h('span', { class: 'paid-lock' }, icon('lock'))),
      h('span', { class: 'paid-kicker', text: t('paid.kicker') }),
      h('h2', { class: 'paid-title', text: f.title }),
      h('p', { class: 'paid-text', text: f.text }),
      h('p', { class: 'paid-note', text: t('paid.note') }),
      serverText ? h('code', { class: 'raw-error', text: serverText }) : null));
}

// errorState renders an API error. 503 (instance not connected) is a calm
// state, 402 the licence state, everything else an error.
export function errorState(err) {
  if (err && err.status === 503) {
    return emptyState({ icon: 'plug', tone: 'calm', title: t('error.notConnected'), text: t('error.notConnectedText') });
  }
  if (err && err.status === 402) return paidState(err.feature, err.message);
  const msg = err && err.message ? err.message : String(err);
  return emptyState({ icon: 'alert', tone: 'error', title: err && err.status === 403 ? t('error.forbidden') : t('error.loadFailed'), text: msg });
}

// actionDialog asks for confirmation and runs the action while the dialog
// stays open: a failure is shown inside it, next to the button that caused
// it; success closes it. Resolves to true when the action succeeded.
export function actionDialog({ title, message, confirmLabel, danger = false, run }) {
  return new Promise((resolve) => {
    let done = false;
    const error = h('div', { class: 'form-error', role: 'alert', hidden: true });
    const cancel = h('button', { type: 'button', class: 'btn', text: t('common.cancel') });
    const ok = h('button', { type: 'button', class: danger ? 'btn btn-danger-solid' : 'btn btn-primary', text: confirmLabel });
    const m = openModal({
      title,
      body: [h('p', { class: 'modal-text', text: message }), error],
      footer: [cancel, ok],
      onClose: () => resolve(done),
    });
    if (danger) m.dialog.classList.add('danger');
    cancel.addEventListener('click', m.close);
    ok.addEventListener('click', async () => {
      ok.disabled = true;
      cancel.disabled = true;
      error.hidden = true;
      try {
        await run();
        done = true;
        m.close();
      } catch (err) {
        error.textContent = err && err.message ? err.message : String(err);
        error.hidden = false;
        ok.disabled = false;
        cancel.disabled = false;
      }
    });
    (danger ? cancel : ok).focus();
  });
}

// connectionHint turns an ESL connection error into advice; "" when the
// error is not one it recognises.
export function connectionHint(error) {
  const e = String(error || '').toLowerCase();
  if (e.includes('authentication failed')) return t('hint.auth');
  if (e.includes('access denied')) return t('hint.acl');
  if (e.includes('connection refused')) return t('hint.refused');
  if (e.includes('no such host')) return t('hint.dns');
  if (e.includes('timeout') || e.includes('timed out') || e.includes('no route') || e.includes('unreachable')) return t('hint.timeout');
  if (e.includes('eof') || e.includes('reset by peer')) return t('hint.eof');
  return '';
}

const WIDTHS = ['w-75', 'w-90', 'w-45', 'w-60', 'w-30'];

// skeleton is a placeholder shaped like the content it stands for: table
// rows (cols > 1) or a list of lines (cols = 1).
export function skeleton({ rows = 6, cols = 4 } = {}) {
  return h('div', { class: 'skel', 'aria-busy': 'true', 'aria-label': t('common.loading') },
    Array.from({ length: rows }, (_, i) => (cols > 1
      ? h('div', { class: 'skel-row' }, Array.from({ length: cols }, (_, j) => h('span', { class: 'skel-bar ' + WIDTHS[(i + j) % WIDTHS.length] })))
      : h('span', { class: 'skel-bar ' + WIDTHS[i % WIDTHS.length] }))));
}

export function panel({ title, actions, flush }) {
  const titleEl = h('h2', { class: 'panel-title', text: title });
  const countEl = h('span', { class: 'panel-count' });
  const body = h('div', { class: flush ? 'panel-body flush' : 'panel-body' });
  const el = h('section', { class: 'panel' },
    h('div', { class: 'panel-head' }, titleEl, countEl, h('span', { class: 'spacer' }), actions || null),
    body);
  return {
    el,
    body,
    setTitle(t) {
      titleEl.textContent = t;
    },
    setCount(n) {
      countEl.textContent = n === null || n === undefined ? '' : String(n);
    },
  };
}

export function kv(pairs) {
  return h('dl', { class: 'kv' }, pairs.map(([k, v]) => h('div', { class: 'kv-row' },
    h('dt', { text: k }), h('dd', null, v === '' || v === null || v === undefined ? DASH : v))));
}

export function searchBox(placeholder, onInput, size) {
  const input = h('input', { type: 'search', class: 'input', placeholder, 'aria-label': placeholder, spellcheck: false, autocomplete: 'off' });
  input.addEventListener('input', () => onInput(input.value.trim().toLowerCase()));
  return { el: h('label', { class: cx('search', size) }, icon('search'), input), input };
}

// rowMatches reports whether any field of the row contains q (lower case).
export function rowMatches(row, q) {
  return !q || Object.values(row).some((v) => cellText(v).toLowerCase().includes(q));
}

export function segmented(options, value, onChange, size) {
  const buttons = new Map();
  const counts = new Map();
  const set = (key) => {
    for (const [k, b] of buttons) b.setAttribute('aria-pressed', String(k === key));
  };
  const el = h('div', { class: cx('segmented', size), role: 'group' }, options.map((o) => {
    const count = h('span', { class: 'count' });
    const b = h('button', { type: 'button', title: o.title, on: { click: () => { set(o.key); onChange(o.key); } } }, o.label, count);
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

function compare(a, b) {
  const x = Number(a);
  const y = Number(b);
  if (a !== '' && b !== '' && Number.isFinite(x) && Number.isFinite(y)) return x - y;
  return String(a).localeCompare(String(b), undefined, { numeric: true, sensitivity: 'base' });
}

// sortState is the sort of one table, owned by the view so that it survives
// re-renders. onChange is called after the user clicks a header.
export function sortState(onChange) {
  return { col: -1, dir: 1, onChange };
}

// dataTable renders rows with curated columns. A column is
// {label, key | render(row), right, mono, wrap, shrink, sortBy(row)}.
// onRow makes rows clickable; sort (a sortState) makes columns with sortBy
// sortable: a click cycles ascending, descending, original order.
export function dataTable({ columns, rows, onRow, keyOf, selected, empty, sort }) {
  if (!rows.length) return empty instanceof Node ? empty : emptyState({ title: empty || t('common.nothing') });
  const sortCol = sort && sort.col >= 0 ? columns[sort.col] : null;
  if (sortCol && sortCol.sortBy) {
    rows = [...rows].sort((a, b) => compare(sortCol.sortBy(a), sortCol.sortBy(b)) * sort.dir);
  }
  const head = h('tr', null, columns.map((c, i) => {
    const sortable = Boolean(sort && c.sortBy);
    const th = h('th', { scope: 'col', class: cx(c.right && 'right', c.shrink && 'shrink', sortable && 'sortable') },
      sortable ? h('span', { class: 'th' }, c.label, icon('sort')) : c.label);
    if (!sortable) return th;
    th.tabIndex = 0;
    th.setAttribute('aria-sort', sort.col === i ? (sort.dir > 0 ? 'ascending' : 'descending') : 'none');
    const toggle = () => {
      if (sort.col !== i) Object.assign(sort, { col: i, dir: 1 });
      else if (sort.dir > 0) sort.dir = -1;
      else sort.col = -1;
      sort.onChange();
    };
    th.addEventListener('click', toggle);
    th.addEventListener('keydown', (ev) => {
      if (ev.key === 'Enter' || ev.key === ' ') {
        ev.preventDefault();
        toggle();
      }
    });
    return th;
  }));
  const body = rows.map((row) => {
    const isSelected = keyOf && selected !== undefined && selected !== null && keyOf(row) === selected;
    const tr = h('tr', { class: cx(onRow && 'clickable', isSelected && 'selected'), tabIndex: onRow ? 0 : undefined },
      columns.map((c) => {
        const v = c.render ? c.render(row) : cellText(row[c.key]);
        return h('td', { class: cx(c.right && 'right', c.mono && 'mono', c.wrap && 'wrap', c.shrink && 'shrink') },
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
// losing the scroll position. It shows a skeleton until the first show().
export function tableHost({ rows = 4, cols = 4 } = {}) {
  const el = h('div', { class: 'tbl-wrap' }, skeleton({ rows, cols }));
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

// ---------- small charts ----------

// sparkline keeps the last `capacity` samples and stretches to its box.
// zero anchors the scale at 0; otherwise the scale follows the data range.
export function sparkline({ zero = true, capacity = 60 } = {}) {
  const W = 120;
  const H = 38;
  const PAD = 3;
  const area = s('path', { class: 'area', d: '' });
  const line = s('path', { class: 'line', d: '' });
  const el = s('svg', { class: 'spark', viewBox: `0 0 ${W} ${H}`, preserveAspectRatio: 'none', 'aria-hidden': 'true' }, area, line);
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
    const step = W / (values.length - 1);
    const pts = values.map((v, i) => [i * step, H - PAD - ((v - lo) / (hi - lo)) * (H - 2 * PAD - 4)]);
    const d = pts.map(([x, y], i) => (i ? 'L' : 'M') + x.toFixed(1) + ' ' + y.toFixed(1)).join(' ');
    line.setAttribute('d', d);
    area.setAttribute('d', d + ` L${W} ${H} L0 ${H} Z`);
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
    // set replaces the series, e.g. with the tail of a loaded history.
    set(list) {
      values.length = 0;
      for (const v of list) {
        const n = Number(v);
        if (Number.isFinite(n)) values.push(n);
      }
      values.splice(0, Math.max(0, values.length - capacity));
      if (values.length < 2) {
        line.setAttribute('d', '');
        area.setAttribute('d', '');
      }
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
