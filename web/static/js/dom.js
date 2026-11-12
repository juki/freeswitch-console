// DOM helpers and formatters. Text always goes in through textContent or
// text nodes; server data never reaches an HTML parser.

// h creates an element. props: "class", "text", "on" ({event: fn}), "for",
// "role" and names containing "-" (set as attributes); anything else is
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

const SVG_NS = 'http://www.w3.org/2000/svg';

// s creates an SVG element; every prop is set as an attribute.
export function s(tag, attrs, ...children) {
  const el = document.createElementNS(SVG_NS, tag);
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (v !== undefined && v !== null && v !== false) el.setAttribute(k, String(v));
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

function blank(v) {
  return v === null || v === undefined || v === '';
}

export function fmtInt(v) {
  const n = Number(v);
  return blank(v) || !Number.isFinite(n) ? DASH : Math.round(n).toLocaleString('en-US');
}

export function fmtPct(v, digits = 1) {
  const n = Number(v);
  return blank(v) || !Number.isFinite(n) ? DASH : n.toFixed(digits);
}

// fmtUptime formats the status "uptime" object, e.g. "12d 4h 37m".
export function fmtUptime(u) {
  if (!u || typeof u !== 'object') return DASH;
  const units = [['years', 'y'], ['days', 'd'], ['hours', 'h'], ['minutes', 'm']];
  const out = [];
  for (const [k, suffix] of units) {
    const n = Number(u[k]) || 0;
    if (n || out.length) out.push(n + suffix);
  }
  return out.length ? out.slice(0, 3).join(' ') : (Number(u.seconds) || 0) + 's';
}

// fmtDuration formats seconds as "42s", "5m 12s", "1h 02m" or "3d 4h".
export function fmtDuration(sec) {
  const n = Math.max(0, Math.floor(Number(sec)));
  if (!Number.isFinite(n)) return DASH;
  if (n < 60) return n + 's';
  if (n < 3600) return Math.floor(n / 60) + 'm ' + String(n % 60).padStart(2, '0') + 's';
  if (n < 86400) return Math.floor(n / 3600) + 'h ' + String(Math.floor((n % 3600) / 60)).padStart(2, '0') + 'm';
  return Math.floor(n / 86400) + 'd ' + Math.floor((n % 86400) / 3600) + 'h';
}

// validTime parses a timestamp and rejects the zero time.
export function validTime(v) {
  const d = new Date(v);
  return Number.isNaN(d.getTime()) || d.getFullYear() < 1971 ? null : d;
}

// sinceText is the age of a timestamp, e.g. "3m 05s"; "" when unknown.
export function sinceText(v) {
  const d = validTime(v);
  return d ? fmtDuration((Date.now() - d.getTime()) / 1000) : '';
}
