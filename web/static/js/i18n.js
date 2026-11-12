// Languages. t(KEY, {name: value}) looks KEY up in the active catalog
// (falling back to English, then to the key itself) and fills {name}
// placeholders. Values are inserted as plain text: callers put the result in
// textContent, never into an HTML parser. tx() does the same but returns a
// list of strings and nodes, for sentences that contain styled values.
//
// Keys are literal strings at every call site (a test parses the code and the
// catalogs). English plurals use a pair of keys, KEY.one and KEY.other, both
// passed to plural(n, one, other), which picks one with Intl.PluralRules;
// Chinese catalogs give both keys the same text.
//
// The language is the stored choice (localStorage "fsdeck-lang"), else the
// browser's first supported language (zh* -> zh-CN), else English.
// setLang() stores the choice and tells the listeners, which re-render.

import en from './locales/en.js';
import zhCN from './locales/zh-CN.js';

const CATALOGS = { en, 'zh-CN': zhCN };
export const LANGS = [
  { code: 'en', label: 'English', short: 'EN' },
  { code: 'zh-CN', label: '简体中文', short: '中' },
];
const KEY = 'fsdeck-lang';

function supported(code) {
  const c = String(code || '').toLowerCase();
  if (c.startsWith('zh')) return 'zh-CN';
  if (c.startsWith('en')) return 'en';
  return '';
}

function detect() {
  try {
    const stored = supported(localStorage.getItem(KEY));
    if (stored) return stored;
  } catch {
    // storage disabled
  }
  for (const l of navigator.languages || [navigator.language]) {
    const s = supported(l);
    if (s) return s;
  }
  return 'en';
}

let current = detect();
let rules = new Intl.PluralRules(current);
const listeners = new Set();
document.documentElement.lang = current;

export function lang() {
  return current;
}

export function setLang(code) {
  const next = supported(code) || 'en';
  try {
    localStorage.setItem(KEY, next);
  } catch {
    // not persisted
  }
  if (next === current) return;
  current = next;
  rules = new Intl.PluralRules(current);
  numberFormats.clear();
  document.documentElement.lang = current;
  for (const fn of [...listeners]) fn(current);
}

// onLangChange registers fn(code); returns a function that removes it.
export function onLangChange(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

function lookup(key) {
  const cat = CATALOGS[current];
  if (Object.prototype.hasOwnProperty.call(cat, key)) return cat[key];
  if (Object.prototype.hasOwnProperty.call(en, key)) return en[key];
  return key;
}

export function t(key, vars) {
  const s = lookup(key);
  if (!vars) return s;
  return s.replace(/\{(\w+)\}/g, (m, name) => (Object.prototype.hasOwnProperty.call(vars, name) ? String(vars[name]) : m));
}

// tx returns the translated sentence as a list of strings and nodes: a
// placeholder whose value is a Node keeps that node (e.g. a <b> value).
export function tx(key, vars) {
  const out = [];
  const s = lookup(key);
  let last = 0;
  s.replace(/\{(\w+)\}/g, (m, name, at) => {
    if (at > last) out.push(s.slice(last, at));
    const v = vars && Object.prototype.hasOwnProperty.call(vars, name) ? vars[name] : m;
    out.push(v instanceof Node ? v : String(v));
    last = at + m.length;
    return m;
  });
  if (last < s.length) out.push(s.slice(last));
  return out;
}

// plural picks the English-style singular or plural form for n.
export function plural(n, one, other) {
  return rules.select(Number(n)) === 'one' ? one : other;
}

const numberFormats = new Map();

// num formats a number for the active language; digits fixes the decimals.
export function num(v, digits) {
  const k = digits === undefined ? 'i' : 'f' + digits;
  let f = numberFormats.get(k);
  if (!f) {
    f = new Intl.NumberFormat(current, digits === undefined ? { maximumFractionDigits: 0 } : { minimumFractionDigits: digits, maximumFractionDigits: digits });
    numberFormats.set(k, f);
  }
  return f.format(v);
}
