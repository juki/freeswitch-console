// Logs (admin): live tail from /stream?loglevel=N in a terminal pane.

import { instPath } from '../api.js';
import { h } from '../dom.js';
import { icon } from '../icons.js';
import { searchBox } from '../ui.js';

const LEVELS = ['CONSOLE', 'ALERT', 'CRIT', 'ERR', 'WARNING', 'NOTICE', 'INFO', 'DEBUG'];
const CAP = 2000;

// FreeSWITCH log text: "[uuid ]2026-01-02 03:04:05.123456 [99.50% ][LEVEL] file.c:123 message"
const LINE = /^(?:[0-9a-f]{8}-[0-9a-f-]{27} )?\d{4}-\d\d-\d\d (\d\d:\d\d:\d\d\.\d{3})\d* (?:[\d.]+% )?\[\w+\] (\S+:\d+) ([\s\S]*)$/;

function level(n) {
  const v = Number(n);
  return Number.isInteger(v) && v >= 0 && v <= 7 ? v : 0;
}

function lineEl(ev) {
  const lvl = level(ev.level);
  const text = String(ev.text === undefined || ev.text === null ? '' : ev.text).replace(/\r?\n$/, '');
  const m = LINE.exec(text);
  const el = h('div', { class: 'log-line lvl-' + lvl },
    h('span', { class: 'log-time', text: m ? m[1] : '' }),
    h('span', { class: 'log-lvl', text: LEVELS[lvl] }),
    m ? h('span', { class: 'log-msg' }, h('span', { class: 'src', text: m[2] + ' ' }), m[3]) : h('span', { class: 'log-msg', text }));
  el.dataset.text = text.toLowerCase();
  return el;
}

// Severity views over the received lines: everything, warnings and worse, errors and worse.
const SEVERITY = { all: 7, warn: 4, err: 3 };

export function mountLogs({ id, scope, root, setState }) {
  const select = h('select', { class: 'select', 'aria-label': 'Stream log level', title: 'Level FreeSWITCH streams at' },
    LEVELS.map((name, i) => h('option', { value: String(i), text: name + ' (' + i + ')' })));
  select.value = '6';
  const pauseBtn = h('button', { type: 'button', class: 'btn btn-sm', 'aria-pressed': 'false' }, icon('pause'), 'Pause');
  const clearBtn = h('button', { type: 'button', class: 'btn btn-sm' }, icon('eraser'), 'Clear');
  const info = h('span', { class: 'term-info', 'aria-live': 'polite' });
  const empty = h('div', { class: 'term-empty' }, icon('logs'),
    h('span', { text: 'Waiting for log lines… An idle FreeSWITCH logs very little; try a lower level such as DEBUG.' }));
  const pane = h('div', { class: 'term-body', role: 'log', tabIndex: 0 }, empty);
  const jumpLabel = h('span');
  const jump = h('button', { type: 'button', class: 'jump', hidden: true }, icon('arrowDown'), jumpLabel);
  let query = '';
  let severity = 'all';
  const search = searchBox('Filter lines', (q) => {
    query = q;
    refilter();
  });
  search.el.classList.add('sm');

  const counts = { err: 0, warn: 0 };
  const facetCount = { err: h('span', { class: 'count', text: '0' }), warn: h('span', { class: 'count', text: '0' }) };
  const facets = new Map();
  const facet = (key, label, cls) => {
    const b = h('button', { type: 'button', class: 'facet ' + (cls || ''), 'aria-pressed': String(key === severity), on: { click: () => {
      severity = key;
      for (const [k, el] of facets) el.setAttribute('aria-pressed', String(k === key));
      refilter();
    } } }, label, facetCount[key] || null);
    facets.set(key, b);
    return b;
  };

  root.append(h('div', { class: 'term' },
    h('div', { class: 'term-bar' },
      h('span', { class: 'term-title' }, icon('logs'), 'Live log'), select,
      h('span', { class: 'term-sep' }),
      h('div', { class: 'facets', role: 'group', 'aria-label': 'Show' }, facet('all', 'All'), facet('warn', 'Warnings+', 'f-warn'), facet('err', 'Errors', 'f-err')),
      search.el,
      h('span', { class: 'spacer' }), info, pauseBtn, clearBtn),
    h('div', { class: 'term-wrap' }, pane, jump)));

  let paused = false;
  let held = []; // lines received while paused
  let pending = []; // lines waiting for the next animation frame
  let frame = 0;
  let total = 0;
  let unseen = 0; // lines added while the user is scrolled up
  let stick = true; // follow the tail unless the user scrolled up

  function visible(el) {
    return Number(el.dataset.lvl) <= SEVERITY[severity] && (!query || el.dataset.text.includes(query));
  }

  function refilter() {
    for (const el of pane.children) if (el !== empty) el.hidden = !visible(el);
    if (stick) pane.scrollTop = pane.scrollHeight;
  }

  function paintJump() {
    jump.hidden = stick;
    jumpLabel.textContent = unseen ? unseen.toLocaleString('en-US') + (unseen === 1 ? ' new line' : ' new lines') : 'Jump to latest';
  }

  pane.addEventListener('scroll', () => {
    stick = pane.scrollTop + pane.clientHeight >= pane.scrollHeight - 24;
    if (stick) unseen = 0;
    paintJump();
  });
  jump.addEventListener('click', () => {
    stick = true;
    unseen = 0;
    pane.scrollTop = pane.scrollHeight;
    paintJump();
    pane.focus({ preventScroll: true });
  });

  function cap(arr) {
    if (arr.length > CAP) arr.splice(0, arr.length - CAP);
  }

  function status() {
    info.textContent = paused
      ? 'Paused · ' + held.length + ' held'
      : total ? total.toLocaleString('en-US') + (total === 1 ? ' line' : ' lines') : '';
    facetCount.err.textContent = counts.err.toLocaleString('en-US');
    facetCount.warn.textContent = (counts.err + counts.warn).toLocaleString('en-US');
  }

  function flush() {
    frame = 0;
    if (!pending.length) return;
    empty.remove();
    const frag = document.createDocumentFragment();
    for (const ev of pending) {
      const el = lineEl(ev);
      const lvl = level(ev.level);
      el.dataset.lvl = String(lvl);
      if (lvl <= 3) counts.err++;
      else if (lvl === 4) counts.warn++;
      if (!visible(el)) el.hidden = true;
      frag.append(el);
    }
    total += pending.length;
    if (!stick) unseen += pending.length;
    pending = [];
    pane.append(frag);
    let extra = pane.childElementCount - CAP;
    while (extra-- > 0) pane.firstElementChild.remove();
    if (stick) pane.scrollTop = pane.scrollHeight;
    status();
    paintJump();
  }

  function schedule() {
    if (!frame && pending.length) frame = requestAnimationFrame(flush);
  }
  scope.onCleanup(() => cancelAnimationFrame(frame));

  function onLog(ev) {
    if (paused) {
      held.push(ev);
      cap(held);
      status();
      return;
    }
    pending.push(ev);
    cap(pending);
    schedule();
  }

  let streamScope = null;
  function open() {
    if (streamScope) streamScope.dispose();
    streamScope = scope.child();
    streamScope.stream('api/' + instPath(id, 'stream') + '?loglevel=' + level(select.value), { log: onLog, state: setState });
  }
  select.addEventListener('change', open);

  pauseBtn.addEventListener('click', () => {
    paused = !paused;
    pauseBtn.replaceChildren(icon(paused ? 'play' : 'pause'), paused ? 'Resume' : 'Pause');
    pauseBtn.setAttribute('aria-pressed', String(paused));
    if (!paused) {
      pending.push(...held);
      held = [];
      cap(pending);
      schedule();
    }
    status();
  });

  clearBtn.addEventListener('click', () => {
    pane.replaceChildren(empty);
    held = [];
    pending = [];
    total = 0;
    unseen = 0;
    counts.err = 0;
    counts.warn = 0;
    stick = true;
    status();
    paintJump();
  });

  open();
}
