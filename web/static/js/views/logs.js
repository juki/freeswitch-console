// Logs (operator, admin): live tail from /stream?loglevel=N in a terminal pane.
//
// Every time the stream opens, the server first replays the lines it keeps
// (as ordinary log events), then sends a "backlog" event with their count,
// then live lines. So the pane is cleared when a stream opens, the replayed
// lines come first, and a divider "Live from 19:20:05 · 312 earlier lines
// replayed" marks where live starts. While no line is visible, a panel in the
// middle of the pane says why: still connecting, FreeSWITCH not connected,
// nothing logged at this level since fsconsole connected to the switch, or
// everything hidden by the filters.

import { instPath } from '../api.js';
import { h, fmtClock, fmtDuration, fmtInt } from '../dom.js';
import { icon } from '../icons.js';
import { plural, t } from '../i18n.js';
import { searchBox } from '../ui.js';

const LEVELS = ['CONSOLE', 'ALERT', 'CRIT', 'ERR', 'WARNING', 'NOTICE', 'INFO', 'DEBUG'];
const DEBUG = 7;
const CAP = 2000;

// FreeSWITCH log text: "[uuid ]2026-01-02 03:04:05.123456 [99.50% ][LEVEL] file.c:123 message"
const LINE = /^(?:[0-9a-f]{8}-[0-9a-f-]{27} )?\d{4}-\d\d-\d\d (\d\d:\d\d:\d\d\.\d{3})\d* (?:[\d.]+% )?\[\w+\] (\S+:\d+) ([\s\S]*)$/;

// Severity views over the received lines: everything, warnings and worse, errors and worse.
const SEVERITY = { all: 7, warn: 4, err: 3 };

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
  el.dataset.lvl = String(lvl);
  return el;
}

export function mountLogs({ id, scope, root, setState, ctx }) {
  const base = '#/i/' + encodeURIComponent(id) + '/';
  const select = h('select', { class: 'select', 'aria-label': t('logs.level'), title: t('logs.levelTitle') },
    LEVELS.map((name, i) => h('option', { value: String(i), text: name + ' (' + i + ')' })));
  select.value = '6';
  const pauseBtn = h('button', { type: 'button', class: 'btn btn-sm', 'aria-pressed': 'false' }, icon('pause'), t('logs.pause'));
  const clearBtn = h('button', { type: 'button', class: 'btn btn-sm' }, icon('eraser'), t('common.clear'));
  const info = h('span', { class: 'term-info', 'aria-live': 'polite' });
  const streamText = h('span');
  const streamState = h('span', { class: 'stream-state', role: 'status' }, h('span', { class: 'stream-dot' }), streamText);
  const pane = h('div', { class: 'term-body', role: 'log', tabIndex: 0, 'aria-label': t('logs.title') });
  const quiet = h('div', { class: 'term-quiet', hidden: true });
  const jumpLabel = h('span');
  const jump = h('button', { type: 'button', class: 'jump', hidden: true }, icon('arrowDown'), jumpLabel);
  let query = '';
  let severity = 'all';
  const search = searchBox(t('logs.filter'), (q) => {
    query = q;
    refilter();
  });
  search.el.classList.add('sm');

  const counts = { err: 0, warn: 0 };
  const facetCount = { err: h('span', { class: 'count', text: '0' }), warn: h('span', { class: 'count', text: '0' }) };
  const facets = new Map();
  const setSeverity = (key) => {
    severity = key;
    for (const [k, el] of facets) el.setAttribute('aria-pressed', String(k === key));
    refilter();
  };
  const facet = (key, label, cls) => {
    const b = h('button', { type: 'button', class: 'facet ' + (cls || ''), 'aria-pressed': String(key === severity), on: { click: () => setSeverity(key) } }, label, facetCount[key] || null);
    facets.set(key, b);
    return b;
  };

  root.append(h('div', { class: 'term' },
    h('div', { class: 'term-bar' },
      h('span', { class: 'term-title' }, icon('logs'), t('logs.title')), streamState, select,
      h('span', { class: 'term-sep' }),
      h('div', { class: 'facets', role: 'group', 'aria-label': t('logs.show') }, facet('all', t('logs.sev.all')), facet('warn', t('logs.sev.warn'), 'f-warn'), facet('err', t('logs.sev.err'), 'f-err')),
      search.el,
      h('span', { class: 'spacer' }), info, pauseBtn, clearBtn),
    h('div', { class: 'term-wrap' }, pane, quiet, jump)));

  let paused = false;
  let held = []; // lines received while paused
  let pending = []; // lines waiting for the next animation frame
  let frame = 0;
  let total = 0;
  let unseen = 0; // lines added while the user is scrolled up
  let stick = true; // follow the tail unless the user scrolled up
  let stream = 'connecting'; // connecting | live | reconnecting
  let fsConnected = true;
  let quietSince = 0; // since when the tail has been listening without a line
  let backlog = null; // lines the server replayed on this stream; null until it says
  let ticking = false;

  // ---- stream state ----

  const STREAM_TEXT = {
    connecting: () => t('logs.stream.connecting'),
    live: () => t('logs.stream.live'),
    reconnecting: () => t('logs.stream.reconnecting'),
    down: () => t('logs.stream.fsDown'),
  };

  function paintStream() {
    const s = stream === 'live' && !fsConnected ? 'down' : stream;
    streamState.className = 'stream-state s-' + s;
    streamText.textContent = STREAM_TEXT[s]();
  }

  function mark(text, kind) {
    const el = h('div', { class: kind ? 'log-mark ' + kind : 'log-mark', role: 'separator' }, h('span', { text }));
    el.dataset.mark = '1';
    pane.append(el);
    if (stick) pane.scrollTop = pane.scrollHeight;
  }

  function onStatus(s) {
    stream = s;
    if (s === 'live') {
      // the server replays its backlog on every open: start from an empty pane
      quietSince = Date.now();
      backlog = null;
      reset();
    }
    paintStream();
    updateQuiet();
  }

  // ---- the quiet panel ----

  function lineStats() {
    let lines = 0;
    let shown = 0;
    for (const el of pane.children) {
      if (el.dataset.mark) continue;
      lines++;
      if (!el.hidden) shown++;
    }
    return { lines, shown };
  }

  function action(label, iconName, onClick) {
    return h('button', { type: 'button', class: 'btn btn-sm', on: { click: onClick } }, icon(iconName), label);
  }

  function quietParts() {
    const { lines, shown } = lineStats();
    const lvl = level(select.value);
    if (shown) return null;
    if (lines) {
      return {
        cls: 'filtered', icon: 'search', title: t('logs.quiet.filteredTitle'),
        text: plural(lines, t('logs.quiet.filteredText.one', { n: fmtInt(lines) }), t('logs.quiet.filteredText.other', { n: fmtInt(lines) })),
        actions: [action(t('logs.quiet.showAll'), 'eraser', () => {
          search.input.value = '';
          query = '';
          setSeverity('all');
        })],
      };
    }
    // until the server has replayed its backlog the pane is still filling
    if (stream !== 'live' || backlog === null) {
      return { cls: 'waiting', icon: 'plug', title: stream === 'connecting' ? t('logs.quiet.connectingTitle') : t('logs.quiet.reconnectingTitle'), text: t('logs.quiet.connectingText') };
    }
    if (!fsConnected) return { cls: 'waiting', icon: 'plug', title: t('logs.quiet.fsDownTitle'), text: t('logs.quiet.fsDownText') };
    return {
      cls: 'listening', icon: 'activity', title: t('logs.quiet.listeningTitle'),
      text: t('logs.quiet.emptyBacklogText', { level: LEVELS[lvl], duration: fmtDuration((Date.now() - quietSince) / 1000) }),
      hint: lvl === DEBUG ? t('logs.quiet.backlogNote') : t('logs.quiet.listeningHint'),
      actions: [
        lvl < DEBUG ? action(t('logs.quiet.toDebug'), 'logs', () => {
          select.value = String(DEBUG);
          open();
        }) : null,
        ctx.can('console') ? h('a', { class: 'btn btn-sm', href: base + 'console' }, icon('terminal'), t('logs.quiet.openConsole')) : null,
      ].filter(Boolean),
    };
  }

  let shownKind = '';
  function updateQuiet() {
    const parts = quietParts();
    quiet.hidden = !parts;
    if (!parts) {
      shownKind = '';
      return;
    }
    const text = h('p', { class: 'term-quiet-text', text: parts.text });
    // Rebuilding the card each second would steal focus from its buttons, so a
    // "listening" card that is already up only gets its text refreshed.
    if (shownKind === parts.cls + ':' + parts.title && parts.cls === 'listening') {
      quiet.querySelector('.term-quiet-text').textContent = parts.text;
    } else {
      shownKind = parts.cls + ':' + parts.title;
      quiet.replaceChildren(h('div', { class: 'term-quiet-card ' + parts.cls },
        h('span', { class: 'term-quiet-icon' }, icon(parts.icon)),
        h('div', { class: 'term-quiet-title', text: parts.title }),
        text,
        parts.hint ? h('p', { class: 'term-quiet-hint', text: parts.hint }) : null,
        parts.actions && parts.actions.length ? h('div', { class: 'term-quiet-actions' }, parts.actions) : null));
    }
    // the "listening for" duration counts up while the card shows it
    if (parts.cls === 'listening' && !ticking) {
      ticking = true;
      scope.later(tick, 1000);
    }
  }

  function tick() {
    ticking = false;
    if (!quiet.hidden) updateQuiet();
  }

  // ---- filtering and the tail ----

  function visible(el) {
    return Number(el.dataset.lvl) <= SEVERITY[severity] && (!query || el.dataset.text.includes(query));
  }

  function refilter() {
    for (const el of pane.children) if (!el.dataset.mark) el.hidden = !visible(el);
    if (stick) pane.scrollTop = pane.scrollHeight;
    updateQuiet();
  }

  function paintJump() {
    jump.hidden = stick;
    jumpLabel.textContent = unseen ? plural(unseen, t('logs.newLines.one', { n: fmtInt(unseen) }), t('logs.newLines.other', { n: fmtInt(unseen) })) : t('logs.jump');
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
      ? t('logs.paused', { n: fmtInt(held.length) })
      : total ? plural(total, t('logs.lines.one', { n: fmtInt(total) }), t('logs.lines.other', { n: fmtInt(total) })) : '';
    facetCount.err.textContent = fmtInt(counts.err);
    facetCount.warn.textContent = fmtInt(counts.err + counts.warn);
  }

  function flush() {
    frame = 0;
    if (!pending.length) return;
    const frag = document.createDocumentFragment();
    for (const ev of pending) {
      const el = lineEl(ev);
      const lvl = Number(el.dataset.lvl);
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
    updateQuiet();
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

  function onState(st) {
    setState(st);
    if (st && typeof st === 'object') {
      fsConnected = Boolean(st.connected);
      paintStream();
      updateQuiet();
    }
  }

  // onBacklog marks where the replayed lines end and live begins.
  function onBacklog(data) {
    const n = Math.max(0, Number(data && data.lines) || 0);
    if (frame) cancelAnimationFrame(frame);
    flush(); // replayed lines still waiting for a frame go above the divider
    backlog = n;
    const time = fmtClock(Date.now(), true);
    mark(n
      ? plural(n, t('logs.mark.liveAfter.one', { time, n: fmtInt(n) }), t('logs.mark.liveAfter.other', { time, n: fmtInt(n) }))
      : t('logs.mark.liveNoBacklog', { time }), 'live');
    updateQuiet();
  }

  // reset empties the pane and the counters.
  function reset() {
    pane.replaceChildren();
    held = [];
    pending = [];
    total = 0;
    unseen = 0;
    counts.err = 0;
    counts.warn = 0;
    stick = true;
    status();
    paintJump();
  }

  let streamScope = null;
  function open() {
    if (streamScope) streamScope.dispose();
    streamScope = scope.child();
    stream = 'connecting';
    paintStream();
    updateQuiet();
    streamScope.stream('api/' + instPath(id, 'stream') + '?loglevel=' + level(select.value), { log: onLog, state: onState, backlog: onBacklog }, { onStatus });
  }
  select.addEventListener('change', open);

  pauseBtn.addEventListener('click', () => {
    paused = !paused;
    pauseBtn.replaceChildren(icon(paused ? 'play' : 'pause'), paused ? t('logs.resume') : t('logs.pause'));
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
    reset();
    quietSince = Date.now();
    if (stream === 'live') mark(t('logs.mark.cleared', { time: fmtClock(quietSince, true) }));
    updateQuiet();
  });

  open();
}
