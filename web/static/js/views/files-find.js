// Files view, find bar: find in the open file and step through the matches.

import { h, fmtInt } from '../dom.js';
import { icon } from '../icons.js';
import { t } from '../i18n.js';
import { searchBox } from '../ui.js';

// createFind builds the find bar for editor; it searches only while a file
// is open (state.cur).
export function createFind({ state, editor }) {
  let finds = [];
  let findAt = -1;
  const findCount = h('span', { class: 'find-count' });
  const findBox = searchBox(t('files.findHere'), () => runFind(), 'sm');
  const findBar = h('div', { class: 'fe-find', hidden: true }, findBox.el, findCount,
    h('button', { type: 'button', class: 'btn btn-ghost btn-sm btn-icon', 'aria-label': t('xb.prev'), title: t('xb.prevTitle'), on: { click: () => stepFind(-1) } }, icon('chevronUp')),
    h('button', { type: 'button', class: 'btn btn-ghost btn-sm btn-icon', 'aria-label': t('xb.next'), title: t('xb.nextTitle'), on: { click: () => stepFind(1) } }, icon('chevronDown')),
    h('button', { type: 'button', class: 'btn btn-ghost btn-sm btn-icon', 'aria-label': t('common.close'), on: { click: () => { findBar.hidden = true; editor.focus(); } } }, icon('x')));
  findBox.input.addEventListener('keydown', (ev) => {
    if (ev.key === 'Enter') {
      ev.preventDefault();
      stepFind(ev.shiftKey ? -1 : 1);
    } else if (ev.key === 'Escape') {
      findBar.hidden = true;
      editor.focus();
    }
  });

  // ---------- find ----------
  function openFind() {
    if (!state.cur) return;
    findBar.hidden = false;
    findBox.input.focus();
    findBox.input.select();
    if (findBox.input.value) runFind();
  }

  function runFind(keep) {
    const q = findBox.input.value.toLowerCase();
    finds = [];
    if (q) {
      const v = editor.value().toLowerCase();
      for (let i = v.indexOf(q); i >= 0 && finds.length < 10000; i = v.indexOf(q, i + q.length)) finds.push(i);
    }
    if (!keep) findAt = finds.length ? 0 : -1;
    else if (findAt >= finds.length) findAt = finds.length - 1;
    paintFind(!keep);
  }

  function paintFind(move) {
    findCount.textContent = !findBox.input.value ? '' : finds.length ? t('common.nOfM', { n: fmtInt(findAt + 1), m: fmtInt(finds.length) }) : t('common.noMatch');
    if (move && findAt >= 0) {
      editor.select(finds[findAt], finds[findAt] + findBox.input.value.length);
      findBox.input.focus();
    }
  }

  function stepFind(d) {
    if (!finds.length) return;
    findAt = (findAt + d + finds.length) % finds.length;
    paintFind(true);
  }

  return { findBar, findBox, openFind, runFind };
}
