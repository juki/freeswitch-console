// XML highlighter for the config viewer. The server emits one element per
// line, so each line is tokenized on its own; anything unexpected falls back
// to plain text. Output is built from DOM nodes, never parsed as HTML.

import { h } from './dom.js';

const NAME = '[A-Za-z_:][\\w:.-]*';
const OPEN = new RegExp('(</?)(' + NAME + ')', 'y');
const ATTR = new RegExp('(\\s+)(' + NAME + ')(=)("[^"]*"|\'[^\']*\')', 'y');
const CLOSE = /(\s*)(\/?>)/y;
const TEXT = /[^<]+/y;

function span(cls, text) {
  return h('span', { class: cls, text });
}

function tokenizeLine(line) {
  const out = [];
  let i = 0;
  while (i < line.length) {
    if (line[i] !== '<') {
      TEXT.lastIndex = i;
      const m = TEXT.exec(line);
      out.push(m[0]);
      i = TEXT.lastIndex;
      continue;
    }
    OPEN.lastIndex = i;
    const open = OPEN.exec(line);
    if (!open) return null;
    out.push(span('x-p', open[1]), span('x-t', open[2]));
    i = OPEN.lastIndex;
    for (;;) {
      ATTR.lastIndex = i;
      const a = ATTR.exec(line);
      if (!a) break;
      const masked = a[4].slice(1, -1) === '********';
      out.push(a[1], span('x-a', a[2]), span('x-p', a[3]), span(masked ? 'x-m' : 'x-v', a[4]));
      i = ATTR.lastIndex;
    }
    CLOSE.lastIndex = i;
    const close = CLOSE.exec(line);
    if (!close) return null;
    out.push(close[1], span('x-p', close[2]));
    i = CLOSE.lastIndex;
  }
  return out;
}

// renderXML returns a <pre> with one highlighted, numbered row per line.
export function renderXML(text) {
  const lines = String(text).replace(/\n+$/, '').split('\n');
  return h('pre', { class: 'xml', tabIndex: 0 },
    lines.map((line) => h('span', { class: 'xl' }, tokenizeLine(line) || line)));
}
