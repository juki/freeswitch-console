// Files (admin): the FreeSWITCH configuration files of one instance, as they
// are on disk.
//
// Left: a lazily loaded tree. Right: the editor (editor.js), with find, live
// XML well-formedness, and a status bar. Saving always goes through a review
// of the line diff against the version on disk; the server keeps the version
// it replaces in the file's history. After a save FreeSWITCH has not read the
// file yet, so an apply bar offers the reloads that fit the file and shows
// FreeSWITCH's answers as they are; if a reload fails, the previous version
// is one click away. Unsaved edits are guarded against every way of leaving.
//
// mountFiles is the shell: layout, the shared state object, routing (path,
// line, q) and the wiring between the parts, each in its own module:
//   files-tree.js     tree, name filter, refresh, new-file dialog
//   files-search.js   search in all files
//   files-editor.js   editor pane: open, dirty state, XML check, save with review, delete
//   files-find.js     find bar of the editor
//   files-apply.js    apply bar after a save
//   files-history.js  history drawer and restore

import { instPath } from '../api.js';
import { h, fmtInt } from '../dom.js';
import { icon } from '../icons.js';
import { t } from '../i18n.js';
import { segmented } from '../ui.js';
import { createEditor } from '../editor.js';
import { instanceDialog } from './fleet.js';
import { instHash } from '../nav.js';
import { createSearch } from './files-search.js';
import { createFind } from './files-find.js';
import { createEditorPane } from './files-editor.js';
import { createTree } from './files-tree.js';
import { createApply } from './files-apply.js';
import { createHistory } from './files-history.js';

const openDirs = new Map(); // instance id -> Set of open directories, kept while the page lives

export function mountFiles({ id, ctx, scope, root, params = new URLSearchParams() }) {
  const inst = ctx.instances().find((i) => i.id === id);
  if (inst && !inst.configMode) {
    root.append(setupState(ctx, inst));
    return;
  }
  const filesPath = (rest) => instPath(id, 'files' + rest);
  if (!openDirs.has(id)) openDirs.set(id, new Set());
  const open = openDirs.get(id);
  const cache = new Map(); // dir -> rows

  // ---------- state ----------
  // Shared by the parts, which always go through this object for cur and
  // selectedDir (they are replaced, not only changed).
  const state = {
    open, // directories open in the tree
    cache, // dir -> rows
    cur: null, // {path, disk, sha, size, mtime, isNew}
    selectedDir: '',
  };

  // ---------- layout ----------
  // The callbacks below wrap functions of parts created further down; they
  // only run after mountFiles has returned.
  const { searchPane, sInput, sResults, runSearch } = createSearch({ id, scope, openAt: (path, line) => openAt(path, line) });
  const sideTabs = segmented([{ key: 'tree', label: t('fs.tabFiles') }, { key: 'search', label: t('fs.tabSearch') }], 'tree', (k) => showSide(k), 'sm');
  function showSide(k) {
    sideTabs.set(k);
    const tree = k === 'tree';
    treeHead.hidden = !tree;
    treeEl.hidden = !tree;
    searchPane.hidden = tree;
    // results need room: the side panel widens while searching
    const grid = treeEl.closest('.fe-grid');
    if (grid) grid.classList.toggle('searching', !tree);
    if (!tree) {
      sInput.focus();
      sInput.select();
    }
  }

  const stPos = h('span', { class: 'num' });
  const stLines = h('span', { class: 'num' });
  const stXml = h('span', { class: 'fe-xml' });
  const status = h('div', { class: 'fe-status' }, stPos, stLines, h('span', { class: 'spacer' }), stXml, h('span', { text: 'UTF-8' }));

  const editor = createEditor({
    label: t('files.editorLabel'),
    onChange: () => changed(),
    onSave: () => save(),
    onFind: () => openFind(),
    onCursor: (line, col) => {
      stPos.textContent = t('files.pos', { line: fmtInt(line), col: fmtInt(col) });
    },
  });
  const { findBar, findBox, openFind, runFind } = createFind({ state, editor });
  const { main, msgs, changed, save, showEditor, openAt, openFile, removeFile, reviewDialog } = createEditorPane({
    id, ctx, scope, state, filesPath, editor, status, stLines, stXml, findBar, findBox, openFind, runFind,
    loadDir: (dir) => loadDir(dir),
    renderTree: () => renderTree(),
    refreshTree: () => refreshTree(),
    applyBar: (opts) => applyBar(opts),
    historyDrawer: () => historyDrawer(),
  });
  const { treeHead, treeEl, loadDir, renderTree, refreshTree } = createTree({ id, ctx, scope, root, inst, state, filesPath, setupState, main, editor, openFile, showEditor });
  const { applyBar } = createApply({ id, ctx, scope, state, filesPath, msgs, editor, changed, removeFile });
  const { historyDrawer } = createHistory({ scope, state, filesPath, editor, showEditor, applyBar, reviewDialog });

  root.append(h('section', { class: 'panel fe' },
    h('div', { class: 'fe-grid' },
      h('div', { class: 'fe-side' }, h('div', { class: 'fe-side-tabs' }, sideTabs.el), treeHead, treeEl, searchPane),
      main)));

  // ---------- routing ----------
  refreshTree().then(() => {
    if (scope.disposed) return;
    const q = params.get('q');
    if (q) {
      showSide('search');
      sInput.value = q;
      runSearch();
      ctx.setHash(instHash(id, 'config', 'files'));
    }
    const path = params.get('path');
    if (path) openAt(path, Number(params.get('line')) || 0);
  });
  sResults.replaceChildren(h('p', { class: 'fs-empty', text: t('fs.intro') }));
}

// setupState explains the two ways to give fsconsole the configuration files and
// opens the instance dialog at that section.
function setupState(ctx, inst) {
  const action = ctx.can('manage') && inst && inst.name
    ? h('button', { type: 'button', class: 'btn btn-primary', on: { click: () => instanceDialog(ctx, inst, { focusConfig: true, onSaved: () => ctx.refreshInstances() }) } }, icon('sliders'), t('files.setup.action'))
    : null;
  return h('div', { class: 'setup' },
    h('div', { class: 'setup-card' },
      h('span', { class: 'empty-icon' }, icon('file')),
      h('h2', { class: 'setup-title', text: t('files.setup.title') }),
      h('p', { class: 'setup-text', text: t('files.setup.text') }),
      h('div', { class: 'setup-modes' },
        h('div', { class: 'setup-mode' }, h('b', { text: t('fleet.cfg.local') }), h('p', { text: t('fleet.cfg.localText') })),
        h('div', { class: 'setup-mode' }, h('b', { text: t('fleet.cfg.agent') }), h('p', { text: t('fleet.cfg.agentText') }))),
      action));
}
