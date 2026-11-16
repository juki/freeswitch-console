// Files view, tree: the lazily loaded directory tree, its name filter, refresh,
// and the new-file dialog.

import { api, isAbort, rowsOf } from '../api.js';
import { h, fmtBytes, fmtDateTime } from '../dom.js';
import { icon } from '../icons.js';
import { t } from '../i18n.js';
import { confirmLeave, errorState, openModal, searchBox, skeleton, toast } from '../ui.js';

const EXT = /\.(xml|lua|conf|tpl|txt|json|cfg|ini)$/i;
const FILE_NAME = /^[A-Za-z0-9_.-]+$/;

const join = (dir, name) => (dir ? dir + '/' + name : name);

// createTree builds the tree side. It reads and writes state.open,
// state.cache, state.selectedDir and reads state.cur; a picked file goes to
// openFile(path), a new one into editor followed by showEditor().
export function createTree({ id, ctx, scope, root, inst, state, filesPath, setupState, main, editor, openFile, showEditor }) {
  const { open, cache } = state;

  // ---------- layout ----------
  let filter = '';
  const filterBox = searchBox(t('files.filter'), (q) => {
    filter = q;
    renderTree();
  }, 'sm');
  const treeEl = h('div', { class: 'fe-tree', role: 'tree', 'aria-label': t('files.tree') }, skeleton({ rows: 10, cols: 1 }));
  const newBtn = h('button', { type: 'button', class: 'btn btn-sm btn-icon', title: t('files.new'), 'aria-label': t('files.new'), on: { click: () => newFileDialog() } }, icon('plus'));
  const refreshBtn = h('button', { type: 'button', class: 'btn btn-sm btn-icon btn-ghost', title: t('files.refresh'), 'aria-label': t('files.refresh'), on: { click: () => refreshTree() } }, icon('refresh'));

  const treeHead = h('div', { class: 'fe-side-head' }, filterBox.el, newBtn, refreshBtn);

  // ---------- tree ----------
  async function loadDir(dir) {
    const data = await api('GET', filesPath('?path=' + encodeURIComponent(dir)), { signal: scope.signal });
    const rows = rowsOf(data);
    cache.set(dir, rows);
    return rows;
  }

  function matches(dir, row) {
    if (!filter) return true;
    if (row.name.toLowerCase().includes(filter)) return true;
    if (!row.dir) return false;
    const sub = cache.get(join(dir, row.name));
    return Boolean(sub) && sub.some((r) => matches(join(dir, row.name), r));
  }

  function renderTree() {
    const out = [];
    const walk = (dir, depth) => {
      for (const row of cache.get(dir) || []) {
        if (!matches(dir, row)) continue;
        const p = join(dir, row.name);
        const isOpen = row.dir && (open.has(p) || (filter && cache.has(p)));
        const sel = state.cur && state.cur.path === p;
        const btn = h('button', {
          type: 'button', class: 'fe-node d' + Math.min(depth, 8) + (row.dir ? ' dir' : '') + (sel ? ' sel' : ''), role: 'treeitem',
          title: row.dir ? p : t('files.nodeTitle', { path: p, size: fmtBytes(row.size), time: fmtDateTime(row.mtime) }),
          on: { click: () => (row.dir ? toggle(p) : openFile(p)) },
        }, row.dir ? icon('chevron', isOpen ? 'tw open' : 'tw') : h('span', { class: 'tw-space' }), icon(row.dir ? 'folder' : 'file', 'fi'),
        h('span', { class: 'fe-name', text: row.name }), row.dir ? null : h('span', { class: 'fe-size', text: fmtBytes(row.size) }));
        if (row.dir) btn.setAttribute('aria-expanded', String(Boolean(isOpen)));
        out.push(btn);
        if (isOpen) walk(p, depth + 1);
      }
    };
    walk('', 0);
    treeEl.replaceChildren(...(out.length ? out : [h('div', { class: 'note-empty', text: filter ? t('files.noMatch') : t('files.empty') })]));
  }

  async function toggle(dir) {
    state.selectedDir = dir;
    if (open.has(dir)) {
      open.delete(dir);
      renderTree();
      return;
    }
    open.add(dir);
    try {
      if (!cache.has(dir)) await loadDir(dir);
    } catch (err) {
      if (isAbort(err)) return;
      open.delete(dir);
      toast(err.message, 'error', t('files.listFailed'));
    }
    renderTree();
  }

  async function refreshTree() {
    try {
      const dirs = ['', ...[...open]];
      cache.clear();
      for (const d of dirs) {
        try {
          await loadDir(d);
        } catch (err) {
          if (isAbort(err)) return;
          if (d === '') throw err;
          open.delete(d);
        }
      }
      renderTree();
    } catch (err) {
      if (isAbort(err)) return;
      if (err.status === 409) {
        root.replaceChildren(setupState(ctx, inst || { id }));
      } else {
        treeEl.replaceChildren(errorState(err));
        main.replaceChildren(errorState(err));
      }
    }
  }

  // ---------- new ----------
  function newFileDialog() {
    const dirs = ['', ...[...cache.keys()].filter((d) => d)].sort();
    const dirSel = h('select', { id: 'nf-dir', class: 'select mono' }, dirs.map((d) => h('option', { value: d, text: d ? d + '/' : t('files.rootDir') })));
    dirSel.value = cache.has(state.selectedDir) ? state.selectedDir : '';
    const name = h('input', { id: 'nf-name', class: 'input mono', type: 'text', autocomplete: 'off', spellcheck: false, placeholder: 'my-gateway.xml' });
    const note = h('span', { class: 'check-note' });
    const cancel = h('button', { type: 'button', class: 'btn', text: t('common.cancel') });
    const ok = h('button', { type: 'submit', class: 'btn btn-primary', text: t('files.create') });
    ok.setAttribute('form', 'nf-form');
    const form = h('form', { id: 'nf-form', class: 'modal-form', noValidate: true },
      h('p', { class: 'modal-text', text: t('files.newText') }),
      h('div', { class: 'form-row even' },
        h('div', { class: 'field' }, h('label', { for: 'nf-dir', text: t('files.dir') }), dirSel),
        h('div', { class: 'field' }, h('label', { for: 'nf-name', text: t('files.name') }), name, note)));
    const m = openModal({ title: t('files.new'), body: form, footer: [cancel, ok] });
    m.dialog.classList.add('wide');
    cancel.addEventListener('click', m.close);
    const check = () => {
      const n = name.value.trim();
      const exists = (cache.get(dirSel.value) || []).some((r) => r.name === n);
      const valid = FILE_NAME.test(n) && EXT.test(n) && !exists;
      note.className = 'check-note ' + (valid ? 'ok' : n ? 'bad' : '');
      note.replaceChildren(icon(valid ? 'check' : 'info'), !n ? t('files.nameRule') : exists ? t('files.exists') : valid ? t('files.nameOk') : t('files.nameRule'));
      ok.disabled = !valid;
      return valid;
    };
    name.addEventListener('input', check);
    dirSel.addEventListener('change', check);
    check();
    form.addEventListener('submit', async (ev) => {
      ev.preventDefault();
      if (!check() || !(await confirmLeave())) return;
      m.close();
      const path = join(dirSel.value, name.value.trim());
      state.cur = { path, disk: '', sha: '', size: 0, mtime: null, isNew: true };
      editor.setValue(/\.xml$/i.test(path) ? '<include>\n  \n</include>\n' : '');
      showEditor();
      editor.focus();
      editor.select(11, 11);
    });
    name.focus();
  }

  return { treeHead, treeEl, loadDir, renderTree, refreshTree };
}
