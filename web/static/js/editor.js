// A plain-text editor for configuration files, without dependencies.
//
// Technique: a transparent <textarea> lies exactly on top of a highlighted
// copy of the same text. Both sit in the same CSS grid cell, use the same
// font, line height, padding and tab size, and never wrap; the highlighted
// copy (one fixed-height block per line, colour-only spans) gives the cell
// its size and the textarea stretches over it. The textarea never scrolls on
// its own: one outer container scrolls both layers and the line-number
// gutter together, so nothing needs to be kept in sync and no element needs
// an inline style. The editor scrolls the caret into view itself.
//
// On every edit only the lines that changed are re-highlighted (common head
// and tail are kept), continuing past them only while a multi-line comment
// or tag state changes, so a 300 KB file stays responsive.

import { h } from './dom.js';

const LINE = 20; // px, must match .ed-ln / .ed-ta line-height in style.css
const PAD_TOP = 10; // px, must match the vertical padding of .ed-hl / .ed-ta

// ---------- XML highlighting, line by line with a carried state ----------

const S_TEXT = 0;
const S_COMMENT = 1;
const S_TAG = 2;
const NAME = /[A-Za-z_:][\w:.-]*/y;
const SPACE = /\s+/y;
const ATTR = /[A-Za-z_:][\w:.-]*/y;
const STRING = /"[^"]*"|'[^']*'/y;

function span(cls, text) {
  const el = document.createElement('span');
  el.className = cls;
  el.textContent = text;
  return el;
}

// highlightLine appends the tokens of one line to el and returns the state
// at its end.
function highlightLine(el, line, state) {
  let i = 0;
  const text = (to) => {
    if (to > i) el.append(line.slice(i, to));
    i = to;
  };
  while (i < line.length) {
    if (state === S_COMMENT) {
      const end = line.indexOf('-->', i);
      const to = end < 0 ? line.length : end + 3;
      el.append(span('x-c', line.slice(i, to)));
      i = to;
      if (end >= 0) state = S_TEXT;
      continue;
    }
    if (state === S_TAG) {
      SPACE.lastIndex = i;
      if (SPACE.test(line)) {
        text(SPACE.lastIndex);
        continue;
      }
      if (line.startsWith('/>', i) || line[i] === '>') {
        const n = line[i] === '>' ? 1 : 2;
        el.append(span('x-p', line.slice(i, i + n)));
        i += n;
        state = S_TEXT;
        continue;
      }
      if (line[i] === '=') {
        el.append(span('x-p', '='));
        i++;
        continue;
      }
      STRING.lastIndex = i;
      if (STRING.test(line)) {
        el.append(span('x-v', line.slice(i, STRING.lastIndex)));
        i = STRING.lastIndex;
        continue;
      }
      if (line[i] === '"' || line[i] === "'") {
        // a value that goes on past the end of the line
        el.append(span('x-v', line.slice(i)));
        i = line.length;
        continue;
      }
      ATTR.lastIndex = i;
      if (ATTR.test(line)) {
        el.append(span('x-a', line.slice(i, ATTR.lastIndex)));
        i = ATTR.lastIndex;
        continue;
      }
      text(i + 1);
      continue;
    }
    // S_TEXT
    const lt = line.indexOf('<', i);
    if (lt < 0) {
      text(line.length);
      break;
    }
    text(lt);
    if (line.startsWith('<!--', i)) {
      state = S_COMMENT;
      continue;
    }
    if (line.startsWith('<?', i) || line.startsWith('<!', i)) {
      const end = line.indexOf('>', i);
      const to = end < 0 ? line.length : end + 1;
      el.append(span('x-p', line.slice(i, to)));
      i = to;
      continue;
    }
    const close = line[i + 1] === '/';
    NAME.lastIndex = i + (close ? 2 : 1);
    if (NAME.test(line)) {
      el.append(span('x-p', close ? '</' : '<'), span('x-t', line.slice(i + (close ? 2 : 1), NAME.lastIndex)));
      i = NAME.lastIndex;
      state = S_TAG;
      continue;
    }
    text(i + 1);
  }
  return state;
}

// ---------- the editor ----------

// createEditor({xml, onChange, onSave, onFind, onCursor}) returns
// {el, value(), setValue(text), focus(), markError(line|null),
//  select(start, end), revealLine(n), destroy()}.
export function createEditor({ xml = true, label = '', onChange, onSave, onFind, onCursor } = {}) {
  const gutter = h('pre', { class: 'ed-gutter', 'aria-hidden': 'true' });
  const hl = h('div', { class: 'ed-hl', 'aria-hidden': 'true' });
  const ta = h('textarea', {
    class: 'ed-ta', spellcheck: false, autocomplete: 'off', rows: 1, cols: 1,
    'aria-label': label, 'aria-multiline': 'true',
  });
  ta.setAttribute('wrap', 'off');
  ta.setAttribute('autocapitalize', 'off');
  ta.setAttribute('autocorrect', 'off');
  const probe = h('span', { class: 'ed-probe', 'aria-hidden': 'true', text: '0'.repeat(100) });
  const code = h('div', { class: 'ed-code' }, hl, ta, probe);
  const scroller = h('div', { class: 'ed-scroll' }, h('div', { class: 'ed-inner' }, gutter, code));
  const el = h('div', { class: 'ed' }, scroller);

  let lines = [];
  let lineEls = [];
  let states = []; // state at the end of each line
  let curLine = -1;
  let errLine = -1;
  let charW = 0;
  let flashed = null;

  function makeLine(text, startState) {
    const div = document.createElement('div');
    div.className = 'ed-ln';
    const end = xml ? highlightLine(div, text, startState) : (div.append(text), S_TEXT);
    return { div, end };
  }

  function paintGutter() {
    let s = '';
    for (let i = 1; i <= lines.length; i++) s += i + '\n';
    gutter.textContent = s;
  }

  function rebuild() {
    const frag = document.createDocumentFragment();
    lineEls = [];
    states = [];
    let st = S_TEXT;
    for (const line of lines) {
      const { div, end } = makeLine(line, st);
      lineEls.push(div);
      states.push(end);
      frag.append(div);
      st = end;
    }
    hl.replaceChildren(frag);
    paintGutter();
    curLine = -1;
    errLine = -1;
  }

  // update re-highlights only what changed since the last call.
  function update() {
    const next = ta.value.split('\n');
    const prev = lines;
    let head = 0;
    const min = Math.min(prev.length, next.length);
    while (head < min && prev[head] === next[head]) head++;
    let tail = 0;
    while (tail < min - head && prev[prev.length - 1 - tail] === next[next.length - 1 - tail]) tail++;
    const removeEnd = prev.length - tail;
    const addEnd = next.length - tail;
    let st = head ? states[head - 1] : S_TEXT;
    const fresh = [];
    const freshStates = [];
    for (let i = head; i < addEnd; i++) {
      const { div, end } = makeLine(next[i], st);
      fresh.push(div);
      freshStates.push(end);
      st = end;
    }
    const before = lineEls[removeEnd] || null;
    for (let i = head; i < removeEnd; i++) lineEls[i].remove();
    const frag = document.createDocumentFragment();
    frag.append(...fresh);
    hl.insertBefore(frag, before);
    lineEls.splice(head, removeEnd - head, ...fresh);
    states.splice(head, removeEnd - head, ...freshStates);
    // a changed comment or tag state ripples into the following lines
    for (let i = addEnd; i < next.length && states[i] !== undefined; i++) {
      const prevEnd = states[i];
      const { div, end } = makeLine(next[i], st);
      hl.replaceChild(div, lineEls[i]);
      lineEls[i] = div;
      states[i] = end;
      st = end;
      // the state after this line is what it was: the rest is unaffected
      if (end === prevEnd) break;
    }
    const countChanged = prev.length !== next.length;
    lines = next;
    if (countChanged) paintGutter();
    if (curLine >= lines.length) curLine = -1;
    if (errLine >= 0 && (errLine >= lines.length || head <= errLine)) markError(null);
  }

  function caretLine() {
    const pos = ta.selectionStart;
    let line = 0;
    let idx = -1;
    let lastStart = 0;
    while ((idx = ta.value.indexOf('\n', idx + 1)) !== -1 && idx < pos) {
      line++;
      lastStart = idx + 1;
    }
    return { line, col: pos - lastStart };
  }

  function paintCursor() {
    const { line, col } = caretLine();
    if (line !== curLine) {
      if (lineEls[curLine]) lineEls[curLine].classList.remove('cur');
      curLine = line;
      if (lineEls[curLine]) lineEls[curLine].classList.add('cur');
    }
    if (onCursor) onCursor(line + 1, col + 1);
    return { line, col };
  }

  function measure() {
    if (!charW) charW = probe.getBoundingClientRect().width / 100 || 7.5;
    return charW;
  }

  // reveal scrolls the outer container so that the caret is visible.
  function reveal() {
    const { line, col } = paintCursor();
    const top = PAD_TOP + line * LINE;
    if (top < scroller.scrollTop + 4) scroller.scrollTop = Math.max(0, top - LINE);
    else if (top + LINE > scroller.scrollTop + scroller.clientHeight - 4) scroller.scrollTop = top + 2 * LINE - scroller.clientHeight;
    const x = gutter.offsetWidth + 12 + col * measure();
    const left = scroller.scrollLeft + gutter.offsetWidth;
    if (x < left + 8) scroller.scrollLeft = Math.max(0, x - gutter.offsetWidth - 40);
    else if (x > scroller.scrollLeft + scroller.clientWidth - 24) scroller.scrollLeft = x - scroller.clientWidth + 80;
  }

  // insert replaces the selection with text the way typing would, so the
  // browser's undo history keeps working.
  function insert(text) {
    ta.focus();
    let ok = false;
    try {
      ok = document.execCommand('insertText', false, text);
    } catch {
      ok = false;
    }
    if (!ok) {
      ta.setRangeText(text, ta.selectionStart, ta.selectionEnd, 'end');
      ta.dispatchEvent(new Event('input'));
    }
  }

  function lineStartOf(pos) {
    return ta.value.lastIndexOf('\n', pos - 1) + 1;
  }

  // indent shifts the selected lines right (two spaces) or left.
  function indent(out) {
    const v = ta.value;
    const s = ta.selectionStart;
    const e = ta.selectionEnd;
    if (!out && s === e) {
      insert('  ');
      return;
    }
    const from = lineStartOf(s);
    let to = v.indexOf('\n', e > s && v[e - 1] === '\n' ? e - 1 : e);
    if (to < 0) to = v.length;
    const block = v.slice(from, to);
    const changed = block.split('\n').map((l) => (out ? l.replace(/^ {1,2}|^\t/, '') : '  ' + l)).join('\n');
    if (changed === block) return;
    ta.setSelectionRange(from, to);
    insert(changed);
    ta.setSelectionRange(from, from + changed.length);
  }

  function newline() {
    const v = ta.value;
    const s = ta.selectionStart;
    const start = lineStartOf(s);
    const before = v.slice(start, s);
    let pad = (/^[ \t]*/.exec(before) || [''])[0];
    // one level deeper after an opening tag that is not closed on the line
    if (xml && /<[A-Za-z_][^<>]*>\s*$/.test(before) && !/\/>\s*$/.test(before) && !/<\/[^<>]*>\s*$/.test(before)) {
      const open = /<([A-Za-z_][\w:.-]*)[^<>]*>\s*$/.exec(before);
      if (open && !before.includes('</' + open[1])) pad += '  ';
    }
    insert('\n' + pad);
  }

  ta.addEventListener('input', () => {
    update();
    reveal();
    if (onChange) onChange();
  });
  ta.addEventListener('keydown', (ev) => {
    const mod = ev.metaKey || ev.ctrlKey;
    if (mod && !ev.altKey && (ev.key === 's' || ev.key === 'S')) {
      ev.preventDefault();
      if (onSave) onSave();
    } else if (mod && !ev.altKey && (ev.key === 'f' || ev.key === 'F') && onFind) {
      ev.preventDefault();
      onFind();
    } else if (ev.key === 'Tab' && !mod && !ev.altKey) {
      ev.preventDefault();
      indent(ev.shiftKey);
    } else if (ev.key === 'Enter' && !mod && !ev.altKey && !ev.shiftKey && !ev.isComposing) {
      ev.preventDefault();
      newline();
    } else {
      requestAnimationFrame(reveal);
    }
  });
  ta.addEventListener('mouseup', paintCursor);
  ta.addEventListener('focus', paintCursor);

  function markError(line) {
    if (lineEls[errLine]) lineEls[errLine].classList.remove('err');
    errLine = line === null || line === undefined ? -1 : line - 1;
    if (lineEls[errLine]) lineEls[errLine].classList.add('err');
  }

  return {
    el,
    value: () => ta.value,
    setValue(text) {
      ta.value = text;
      lines = text.split('\n');
      rebuild();
      scroller.scrollTop = 0;
      scroller.scrollLeft = 0;
      ta.setSelectionRange(0, 0);
    },
    focus() {
      ta.focus({ preventScroll: true });
      paintCursor();
    },
    markError,
    select(start, end) {
      ta.focus({ preventScroll: true });
      ta.setSelectionRange(start, end);
      reveal();
    },
    // flashLine puts the cursor at the start of line n and highlights the
    // line briefly (a CSS animation; no timer to clean up).
    flashLine(n) {
      if (flashed && flashed.isConnected) flashed.classList.remove('flash');
      this.revealLine(n);
      flashed = lineEls[n - 1] || null;
      if (flashed) {
        void flashed.offsetWidth; // restart the animation
        flashed.classList.add('flash');
      }
    },
    revealLine(n) {
      let pos = 0;
      for (let i = 1; i < n && pos >= 0; i++) pos = ta.value.indexOf('\n', pos) + 1;
      ta.focus({ preventScroll: true });
      ta.setSelectionRange(pos, pos);
      // put the line a third of the way down, so its context shows above it
      const top = PAD_TOP + (n - 1) * LINE;
      if (top < scroller.scrollTop || top > scroller.scrollTop + scroller.clientHeight - 2 * LINE) {
        scroller.scrollTop = Math.max(0, top - Math.floor(scroller.clientHeight / 3));
      }
      reveal();
    },
    setXml(on) {
      xml = on;
    },
  };
}
