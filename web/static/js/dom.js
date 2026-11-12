// DOM helpers and formatters. Text always goes in through textContent or
// text nodes; server data never reaches an HTML parser. Formatters follow the
// active language (i18n.js).

import { num, t } from './i18n.js';

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
  return blank(v) || !Number.isFinite(n) ? DASH : num(Math.round(n));
}

export function fmtPct(v, digits = 1) {
  const n = Number(v);
  return blank(v) || !Number.isFinite(n) ? DASH : num(n, digits);
}

// fmtUptime formats the status "uptime" object: the three largest units from
// the first non-zero one, e.g. "12d 4h 37m" or "12天4小时37分".
export function fmtUptime(u) {
  if (!u || typeof u !== 'object') return DASH;
  const v = (k) => Number(u[k]) || 0;
  const y = v('years');
  const d = v('days');
  const h = v('hours');
  const m = v('minutes');
  if (y) return t('uptime.ydh', { y, d, h });
  if (d) return t('uptime.dhm', { d, h, m });
  if (h) return t('uptime.hm', { h, m });
  if (m) return t('uptime.m', { m });
  return t('uptime.s', { s: v('seconds') });
}

const pad2 = (n) => String(n).padStart(2, '0');

// fmtDuration formats seconds as "42s", "5m 12s", "1h 02m" or "3d 4h"
// (Chinese: "42秒", "5分12秒", "1小时02分", "3天4小时").
export function fmtDuration(sec) {
  const n = Math.max(0, Math.floor(Number(sec)));
  if (!Number.isFinite(n)) return DASH;
  if (n < 60) return t('dur.s', { s: n });
  if (n < 3600) return t('dur.ms', { m: Math.floor(n / 60), s: pad2(n % 60) });
  if (n < 86400) return t('dur.hm', { h: Math.floor(n / 3600), m: pad2(Math.floor((n % 3600) / 60)) });
  return t('dur.dh', { d: Math.floor(n / 86400), h: Math.floor((n % 86400) / 3600) });
}

// fmtSpan is fmtDuration without trailing zero units: "5m", "2h", "1h 30m".
export function fmtSpan(sec) {
  const n = Math.max(0, Math.round(Number(sec)));
  if (n >= 3600 && n % 3600 === 0) return t('span.h', { h: n / 3600 });
  if (n >= 60 && n < 3600 && n % 60 === 0) return t('span.m', { m: n / 60 });
  return fmtDuration(n);
}

let clockFormats = null;

// fmtClock formats a timestamp as local 24-hour time, with seconds if asked.
export function fmtClock(ms, seconds) {
  if (!clockFormats || clockFormats.lang !== document.documentElement.lang) {
    const opts = { hour: '2-digit', minute: '2-digit', hourCycle: 'h23' };
    clockFormats = {
      lang: document.documentElement.lang,
      hm: new Intl.DateTimeFormat(document.documentElement.lang, opts),
      hms: new Intl.DateTimeFormat(document.documentElement.lang, { ...opts, second: '2-digit' }),
    };
  }
  return (seconds ? clockFormats.hms : clockFormats.hm).format(new Date(ms));
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

const dateFormats = new Map();

function dateFormat(kind) {
  const key = document.documentElement.lang + ':' + kind;
  let f = dateFormats.get(key);
  if (!f) {
    const opts = {
      date: { dateStyle: 'medium' },
      datetime: { dateStyle: 'medium', timeStyle: 'medium', hourCycle: 'h23' },
      short: { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23' },
    }[kind];
    f = new Intl.DateTimeFormat(document.documentElement.lang, opts);
    dateFormats.set(key, f);
  }
  return f;
}

// fmtDate formats a timestamp as a date ("Nov 12, 2026" / "2026年11月12日").
export function fmtDate(v) {
  const d = validTime(v);
  return d ? dateFormat('date').format(d) : DASH;
}

// fmtDateTime formats a timestamp with date and time, seconds included.
export function fmtDateTime(v) {
  const d = validTime(v);
  return d ? dateFormat('datetime').format(d) : DASH;
}

// fmtStamp is the compact form for dense tables: "Nov 12, 20:07:13".
export function fmtStamp(v) {
  const d = validTime(v);
  return d ? dateFormat('short').format(d) : DASH;
}
