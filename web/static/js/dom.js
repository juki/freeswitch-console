// DOM helpers. Text always goes in through textContent or text nodes; server
// data never reaches an HTML parser.

// h creates an element. props: "class", "text", "on" ({event: fn}), "for",
// attributes containing "-" or "role" (set as attributes), anything else is
// assigned as a DOM property. Children: nodes, strings, arrays; null is skipped.
export function h(tag, props, ...children) {
  const el = document.createElement(tag);
  if (props) {
    for (const [k, v] of Object.entries(props)) {
      if (v === undefined || v === null || v === false) continue;
      if (k === 'class') el.className = v;
      else if (k === 'text') el.textContent = String(v);
      else if (k === 'on') for (const [ev, fn] of Object.entries(v)) el.addEventListener(ev, fn);
      else if (k === 'for') el.htmlFor = v;
      else if (k === 'role' || k.includes('-')) el.setAttribute(k, v === true ? '' : String(v));
      else el[k] = v;
    }
  }
  appendAll(el, children);
  return el;
}

function appendAll(el, children) {
  for (const c of children) {
    if (c === null || c === undefined || c === false) continue;
    if (Array.isArray(c)) appendAll(el, c);
    else if (c instanceof Node) el.append(c);
    else el.append(document.createTextNode(String(c)));
  }
}

export const DASH = '–';

export function cellText(v) {
  if (v === null || v === undefined) return '';
  if (typeof v === 'object') return JSON.stringify(v);
  return String(v);
}

// columnsOf returns the union of keys across rows in first-seen order.
export function columnsOf(rows) {
  const seen = new Set();
  for (const r of rows) {
    if (r && typeof r === 'object') for (const k of Object.keys(r)) seen.add(k);
  }
  return [...seen];
}

// rowsTable renders rows as a table (or an empty-state paragraph).
// opts.actions(row) may return a node shown in a leading column.
// opts.columns overrides the column list; opts.wrap lets long cells wrap.
export function rowsTable(rows, opts = {}) {
  const list = rows.filter((r) => r && typeof r === 'object');
  if (!list.length) return h('p', { class: 'empty', text: opts.empty || 'No rows.' });
  const cols = opts.columns || columnsOf(list);
  const actions = opts.actions || null;
  const head = h('tr', null,
    actions ? h('th', { scope: 'col', class: 'actions-col', text: 'Action' }) : null,
    cols.map((c) => h('th', { scope: 'col', text: c })));
  const body = list.map((r) => h('tr', null,
    actions ? h('td', { class: 'actions-col' }, actions(r)) : null,
    cols.map((c) => {
      const t = cellText(r[c]);
      // max-width does not apply to table cells, so the limit sits on a span.
      return h('td', null, h('span', { class: 'cell', text: t, title: t.length > 40 ? t : undefined }));
    })));
  return h('table', { class: opts.wrap ? 'grid wrap' : 'grid' },
    h('thead', null, head), h('tbody', null, body));
}

// tableBox is a persistent horizontal/vertical scroll container whose
// content can be replaced without losing the scroll position.
export function tableBox() {
  const el = h('div', { class: 'table-wrap', tabIndex: 0 }, h('p', { class: 'empty', text: 'Loading…' }));
  return {
    el,
    show(node) {
      el.replaceChildren(node);
    },
    rows(rows, opts) {
      el.replaceChildren(rowsTable(rows, opts));
    },
    error(err) {
      el.replaceChildren(errorBox(err));
    },
  };
}

// errorBox renders an API error. 503 (instance not connected) is a calm
// informational state, everything else an error.
export function errorBox(err) {
  if (err && err.status === 503) {
    return h('div', { class: 'notice calm', role: 'status' },
      h('strong', { text: 'Not connected. ' }),
      'FreeSWITCH is not connected right now (', err.message, '). ',
      'Data appears again once the connection is back.');
  }
  const msg = err && err.message ? err.message : String(err);
  const prefix = err && err.status === 403 ? 'Not allowed: ' : 'Error: ';
  return h('div', { class: 'notice error', role: 'alert' }, prefix + msg);
}

export function okBox(text) {
  return h('div', { class: 'notice ok', role: 'status', text });
}

// outputBox shows command output verbatim with a title and a close button.
export function outputBox(title, text) {
  const box = h('div', { class: 'output-box' },
    h('div', { class: 'output-head' },
      h('span', { class: 'output-title', text: title }),
      h('button', { type: 'button', class: 'small', text: 'Close', on: { click: () => box.remove() } })),
    h('pre', { class: 'mono output', tabIndex: 0, text }));
  return box;
}

// showBanner adds a dismissible error banner below the header.
export function showBanner(text) {
  const host = document.getElementById('banners');
  const item = h('div', { class: 'banner', role: 'alert' },
    h('span', { text }),
    h('button', { type: 'button', class: 'small', text: 'Dismiss', on: { click: () => item.remove() } }));
  host.append(item);
}

export function clearBanners() {
  document.getElementById('banners').replaceChildren();
}

function validTime(s) {
  const d = new Date(s);
  return Number.isNaN(d.getTime()) || d.getFullYear() < 1971 ? null : d;
}

// stateView renders an InstanceView.state / stream state object.
export function stateView(st) {
  const ok = Boolean(st && st.connected);
  const since = st ? validTime(st.since) : null;
  const badge = h('span', {
    class: ok ? 'badge ok' : 'badge bad',
    text: ok ? 'connected' : 'disconnected',
    title: since ? 'Since ' + since.toLocaleString() : undefined,
  });
  const err = !ok && st && st.lastError ? h('span', { class: 'last-error', text: st.lastError }) : null;
  return [badge, err];
}

// fmtUptime formats the status "uptime" object, e.g. "3d 4h 12m".
export function fmtUptime(u) {
  if (!u || typeof u !== 'object') return DASH;
  const units = [['years', 'y'], ['days', 'd'], ['hours', 'h'], ['minutes', 'm']];
  const out = [];
  for (const [k, s] of units) {
    const n = Number(u[k]) || 0;
    if (n || out.length) out.push(n + s);
  }
  return out.length ? out.join(' ') : (Number(u.seconds) || 0) + 's';
}

export function fmtNum(v) {
  return v === null || v === undefined || v === '' ? DASH : String(v);
}

export function fmtPct(v) {
  const n = Number(v);
  return v === null || v === undefined || v === '' || !Number.isFinite(n) ? DASH : n.toFixed(1) + '%';
}
