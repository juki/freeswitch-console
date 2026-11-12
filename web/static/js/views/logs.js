// Logs (admin): live tail from /stream?loglevel=N.

import { instPath } from '../api.js';
import { h } from '../dom.js';

const LEVELS = ['CONSOLE', 'ALERT', 'CRIT', 'ERR', 'WARNING', 'NOTICE', 'INFO', 'DEBUG'];
const CAP = 2000;

function level(n) {
  const v = Number(n);
  return Number.isInteger(v) && v >= 0 && v <= 7 ? v : 0;
}

export function mountLogs({ id, scope, root, setState }) {
  const select = h('select', { id: 'log-level' },
    LEVELS.map((name, i) => h('option', { value: String(i), text: i + ' ' + name })));
  select.value = '6';
  const pauseBtn = h('button', { type: 'button', text: 'Pause' });
  const clearBtn = h('button', { type: 'button', text: 'Clear' });
  const info = h('span', { class: 'muted', 'aria-live': 'polite' });
  const pane = h('div', { class: 'log-pane mono', role: 'log', tabIndex: 0 });

  root.append(
    h('div', { class: 'toolbar' },
      h('label', { for: 'log-level', text: 'Level' }), select, pauseBtn, clearBtn, info),
    pane);

  let paused = false;
  let held = []; // lines received while paused
  let pending = []; // lines waiting for the next animation frame
  let frame = 0;
  let stick = true; // follow the tail unless the user scrolled up

  pane.addEventListener('scroll', () => {
    stick = pane.scrollTop + pane.clientHeight >= pane.scrollHeight - 24;
  });

  function cap(arr) {
    if (arr.length > CAP) arr.splice(0, arr.length - CAP);
  }

  function flush() {
    frame = 0;
    const frag = document.createDocumentFragment();
    for (const ev of pending) {
      const text = String(ev.text === undefined || ev.text === null ? '' : ev.text).replace(/\r?\n$/, '');
      frag.append(h('div', { class: 'log-line lvl-' + level(ev.level), text }));
    }
    pending = [];
    pane.append(frag);
    let extra = pane.childElementCount - CAP;
    while (extra-- > 0) pane.firstElementChild.remove();
    if (stick) pane.scrollTop = pane.scrollHeight;
  }

  function schedule() {
    if (!frame && pending.length) frame = requestAnimationFrame(flush);
  }
  scope.onCleanup(() => cancelAnimationFrame(frame));

  function onLog(ev) {
    if (paused) {
      held.push(ev);
      cap(held);
      info.textContent = 'Paused, ' + held.length + ' new line' + (held.length === 1 ? '' : 's') + ' held.';
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
    streamScope.stream('api/' + instPath(id, 'stream') + '?loglevel=' + level(select.value), {
      log: onLog,
      state: setState,
    });
  }

  select.addEventListener('change', open);

  pauseBtn.addEventListener('click', () => {
    paused = !paused;
    pauseBtn.textContent = paused ? 'Resume' : 'Pause';
    pauseBtn.setAttribute('aria-pressed', String(paused));
    if (paused) {
      info.textContent = 'Paused.';
    } else {
      pending.push(...held);
      held = [];
      cap(pending);
      info.textContent = '';
      schedule();
    }
  });

  clearBtn.addEventListener('click', () => {
    pane.replaceChildren();
    held = [];
    pending = [];
    stick = true;
    if (paused) info.textContent = 'Paused.';
  });

  open();
}
