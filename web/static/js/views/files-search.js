// Files view, search panel: search in all files of the instance, hits grouped
// per file; a hit opens the file at its line.

import { isAbort } from '../api.js';
import { h, fmtInt } from '../dom.js';
import { icon } from '../icons.js';
import { t } from '../i18n.js';
import { errorState, skeleton } from '../ui.js';
import { marked, searchFiles } from './source.js';
import { baseOf, dirOf } from './files-util.js';

// createSearch builds the search panel; openAt(path, line) opens a hit.
export function createSearch({ id, scope, openAt }) {
  // ---- search in all files ----
  let sCase = false;
  let sTimer = null;
  let sScope = null;
  scope.onCleanup(() => clearTimeout(sTimer));
  const sInput = h('input', { type: 'search', class: 'input', placeholder: t('fs.placeholder'), 'aria-label': t('fs.placeholder'), spellcheck: false, autocomplete: 'off' });
  const sCaseBtn = h('button', { type: 'button', class: 'btn btn-sm btn-icon case-btn', 'aria-pressed': 'false', title: t('fs.case'), 'aria-label': t('fs.case'), text: 'Aa' });
  const sGo = h('button', { type: 'button', class: 'btn btn-sm btn-icon', title: t('fs.run'), 'aria-label': t('fs.run') }, icon('search'));
  const sNote = h('div', { class: 'fs-note' });
  const sResults = h('div', { class: 'fs-results', role: 'listbox', 'aria-label': t('fs.results') });
  const searchPane = h('div', { class: 'fs-pane', hidden: true },
    h('div', { class: 'fs-head' }, h('label', { class: 'search sm fs-search' }, icon('search'), sInput), sCaseBtn, sGo),
    sNote, sResults);
  sCaseBtn.addEventListener('click', () => {
    sCase = !sCase;
    sCaseBtn.setAttribute('aria-pressed', String(sCase));
    if (sInput.value.trim().length >= 2) runSearch();
  });
  sGo.addEventListener('click', () => runSearch());
  sInput.addEventListener('keydown', (ev) => {
    if (ev.key === 'Enter') {
      ev.preventDefault();
      runSearch();
    } else if (ev.key === 'ArrowDown') {
      const first = sResults.querySelector('.fs-hit');
      if (first) {
        ev.preventDefault();
        first.focus();
      }
    }
  });
  sResults.addEventListener('keydown', (ev) => {
    if (ev.key !== 'ArrowDown' && ev.key !== 'ArrowUp') return;
    ev.preventDefault();
    const hits = [...sResults.querySelectorAll('.fs-hit')];
    const i = hits.indexOf(document.activeElement);
    if (ev.key === 'ArrowUp' && i <= 0) {
      sInput.focus();
      return;
    }
    hits[Math.min(hits.length - 1, i + (ev.key === 'ArrowDown' ? 1 : -1))].focus();
  });

  async function runSearch() {
    const q = sInput.value.trim();
    clearTimeout(sTimer);
    if (q.length < 2) {
      sNote.className = 'fs-note';
      sNote.textContent = q ? t('fs.tooShort') : '';
      sResults.replaceChildren(q ? '' : h('p', { class: 'fs-empty', text: t('fs.intro') }));
      return;
    }
    if (sScope) sScope.dispose();
    sScope = scope.child();
    const mine = sScope;
    sNote.className = 'fs-note';
    sNote.textContent = '';
    // a skeleton only if the answer takes noticeably long
    sTimer = setTimeout(() => {
      if (!mine.disposed) sResults.replaceChildren(skeleton({ rows: 6, cols: 1 }));
    }, 250);
    try {
      const res = await searchFiles(id, q, { caseSensitive: sCase, limit: 300, signal: mine.signal });
      clearTimeout(sTimer);
      renderResults(q, res);
    } catch (err) {
      clearTimeout(sTimer);
      if (isAbort(err)) return;
      sResults.replaceChildren(errorState(err));
    }
  }

  function renderResults(q, { rows, truncated }) {
    const files = new Map();
    for (const r of rows) {
      if (!files.has(r.path)) files.set(r.path, []);
      files.get(r.path).push(r);
    }
    sNote.className = truncated ? 'fs-note warn' : 'fs-note';
    sNote.replaceChildren(
      rows.length ? t('fs.count', { n: fmtInt(rows.length), files: fmtInt(files.size) }) : '',
      truncated ? h('span', { class: 'fs-trunc' }, icon('info'), t('fs.truncated')) : '');
    if (!rows.length) {
      sResults.replaceChildren(h('p', { class: 'fs-empty' }, t('fs.none', { q })));
      return;
    }
    sResults.replaceChildren(...[...files].map(([path, hits]) => h('div', { class: 'fs-file' },
      h('div', { class: 'fs-file-head', title: path },
        icon('file'),
        h('span', { class: 'fs-dir', text: dirOf(path) ? dirOf(path) + '/' : '' }), h('b', { text: baseOf(path) }),
        h('span', { class: 'fs-n', text: fmtInt(hits.length) })),
      hits.map((r) => h('button', {
        type: 'button', class: 'fs-hit', role: 'option', title: r.path + ':' + r.line + '  ' + r.text,
        on: { click: () => openAt(r.path, r.line) },
      }, h('span', { class: 'fs-line num', text: String(r.line) }), h('span', { class: 'fs-text' }, marked(r.text, q, sCase)))))));
  }

  return { searchPane, sInput, sResults, runSearch };
}
