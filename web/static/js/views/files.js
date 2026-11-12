// Files (admin, licence feature config-edit): the FreeSWITCH configuration
// files of one instance, as they are on disk.
//
// Left: a lazily loaded tree. Right: the editor (editor.js), with find, live
// XML well-formedness, and a status bar. Saving always goes through a review
// of the line diff against the version on disk; the server keeps the version
// it replaces in the file's history. After a save FreeSWITCH has not read the
// file yet, so an apply bar offers the reloads that fit the file and shows
// FreeSWITCH's answers as they are; if a reload fails, the previous version
// is one click away. Unsaved edits are guarded against every way of leaving.

import { api, instPath, isAbort, rowsOf } from '../api.js';
import { h, fmtBytes, fmtClock, fmtDateTime, fmtInt } from '../dom.js';
import { icon } from '../icons.js';
import { plural, t } from '../i18n.js';
import { actionDialog, confirmLeave, emptyState, errorState, openDrawer, closeDrawer, openModal, paidState, searchBox, segmented, setLeaveGuard, skeleton, toast } from '../ui.js';
import { createEditor } from '../editor.js';
import { diffLines, diffSummary, renderDiff } from '../diff.js';
import { wellFormed } from '../xml.js';
import { instanceDialog } from './fleet.js';

const EXT = /\.(xml|lua|conf|tpl|txt|json|cfg|ini)$/i;
const FILE_NAME = /^[A-Za-z0-9_.-]+$/;
const openDirs = new Map(); // instance id -> Set of open directories, kept while the page lives
const SKIP_MODULE_RELOAD = new Set(['acl', 'sofia', 'switch', 'modules', 'post_load_modules', 'console', 'logfile', 'event_socket']);

const dirOf = (p) => (p.includes('/') ? p.slice(0, p.lastIndexOf('/')) : '');
const baseOf = (p) => p.slice(p.lastIndexOf('/') + 1);
const join = (dir, name) => (dir ? dir + '/' + name : name);
const shortSha = (s) => String(s || '').slice(0, 10);

export function mountFiles({ id, ctx, scope, root }) {
  const inst = ctx.instances().find((i) => i.id === id);
  if (inst && !inst.configMode) {
    root.append(setupState(ctx, inst));
    return;
  }
  const filesPath = (rest) => instPath(id, 'files' + rest);
  if (!openDirs.has(id)) openDirs.set(id, new Set());
  const open = openDirs.get(id);
  const cache = new Map(); // dir -> rows

  // ---------- layout ----------
  let filter = '';
  const filterBox = searchBox(t('files.filter'), (q) => {
    filter = q;
    renderTree();
  }, 'sm');
  const treeEl = h('div', { class: 'fe-tree', role: 'tree', 'aria-label': t('files.tree') }, skeleton({ rows: 10, cols: 1 }));
  const newBtn = h('button', { type: 'button', class: 'btn btn-sm btn-icon', title: t('files.new'), 'aria-label': t('files.new'), on: { click: () => newFileDialog() } }, icon('plus'));
  const refreshBtn = h('button', { type: 'button', class: 'btn btn-sm btn-icon btn-ghost', title: t('files.refresh'), 'aria-label': t('files.refresh'), on: { click: () => refreshTree() } }, icon('refresh'));

  const pathEl = h('div', { class: 'fe-path' });
  const badge = h('span', { class: 'fe-badge', hidden: true });
  const findBtn = h('button', { type: 'button', class: 'btn btn-sm btn-ghost btn-icon', title: t('files.findTitle'), 'aria-label': t('files.findTitle'), on: { click: () => openFind() } }, icon('search'));
  const histBtn = h('button', { type: 'button', class: 'btn btn-sm', on: { click: () => historyDrawer() } }, icon('history'), t('files.history'));
  const delBtn = h('button', { type: 'button', class: 'btn btn-sm btn-ghost btn-icon btn-danger', title: t('files.delete'), 'aria-label': t('files.delete'), on: { click: () => removeFile() } }, icon('trash'));
  const saveBtn = h('button', { type: 'button', class: 'btn btn-sm btn-primary', title: t('files.saveTitle'), on: { click: () => save() } }, icon('save'), t('files.save'));
  const msgs = h('div', { class: 'fe-msgs' });

  let finds = [];
  let findAt = -1;
  const findCount = h('span', { class: 'find-count' });
  const findBox = searchBox(t('xb.find'), () => runFind(), 'sm');
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
  const placeholder = emptyState({ icon: 'file', title: t('files.pick'), text: t('files.pickText') });
  const editorWrap = h('div', { class: 'fe-editor', hidden: true },
    h('div', { class: 'fe-head' }, pathEl, badge, h('span', { class: 'spacer' }), findBtn, histBtn, delBtn, saveBtn),
    h('p', { class: 'fe-secret' }, icon('lock'), t('files.secrets')),
    msgs, findBar, editor.el, status);
  const main = h('div', { class: 'fe-main' }, placeholder, editorWrap);
  root.append(h('section', { class: 'panel fe' },
    h('div', { class: 'fe-grid' },
      h('div', { class: 'fe-side' }, h('div', { class: 'fe-side-head' }, filterBox.el, newBtn, refreshBtn), treeEl),
      main)));

  // ---------- state ----------
  let cur = null; // {path, disk, sha, size, mtime, isNew}
  let selectedDir = '';
  let xmlTimer = null;
  scope.onCleanup(() => clearTimeout(xmlTimer));
  const dirty = () => Boolean(cur) && (cur.isNew || editor.value() !== cur.disk);
  scope.onCleanup(setLeaveGuard(dirty));

  function changed() {
    const d = dirty();
    badge.hidden = !d;
    badge.textContent = cur && cur.isNew ? t('files.newBadge') : t('files.unsaved');
    saveBtn.disabled = !d;
    stLines.textContent = plural(editor.value().split('\n').length, t('files.lines.one', { n: fmtInt(editor.value().split('\n').length) }), t('files.lines.other', { n: fmtInt(editor.value().split('\n').length) }));
    clearTimeout(xmlTimer);
    if (cur && /\.xml$/i.test(cur.path)) xmlTimer = setTimeout(checkXml, 300);
    else stXml.replaceChildren();
    if (!findBar.hidden && findBox.input.value) runFind(true);
  }

  function checkXml() {
    const ok = wellFormed(editor.value());
    stXml.className = 'fe-xml ' + (ok ? 'ok' : 'bad');
    stXml.replaceChildren(icon(ok ? 'check' : 'alert'), ok ? t('files.xmlOk') : t('files.xmlBad'));
  }

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
        const sel = cur && cur.path === p;
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
    selectedDir = dir;
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
      } else if (err.status === 402 || err.status === 404) {
        root.replaceChildren(paidState('config-edit', err.status === 402 ? err.message : ''));
      } else {
        treeEl.replaceChildren(errorState(err));
        main.replaceChildren(errorState(err));
      }
    }
  }
  refreshTree();

  // ---------- open / new / delete ----------
  function showEditor() {
    placeholder.remove();
    editorWrap.hidden = false;
    pathEl.replaceChildren(...cur.path.split('/').flatMap((part, i, all) => [i ? h('span', { class: 'fe-sep', text: '/' }) : null, h('span', { class: i === all.length - 1 ? 'fe-file' : 'fe-dir', text: part })]).filter(Boolean));
    histBtn.disabled = cur.isNew;
    delBtn.disabled = cur.isNew;
    editor.setXml(/\.xml$/i.test(cur.path));
    msgs.replaceChildren();
    editor.markError(null);
    changed();
    renderTree();
  }

  async function openFile(path, { force = false } = {}) {
    if (!force && cur && cur.path === path) return;
    if (!force && !(await confirmLeave())) return;
    selectedDir = dirOf(path);
    try {
      const data = await api('GET', filesPath('/content?path=' + encodeURIComponent(path)), { signal: scope.signal });
      cur = { path, disk: data.content, sha: data.sha256, size: data.size, mtime: data.mtime, isNew: false };
      editor.setValue(data.content);
      showEditor();
      editor.focus();
    } catch (err) {
      if (isAbort(err)) return;
      toast(err.message, 'error', t('files.openFailed', { path }));
    }
  }

  function newFileDialog() {
    const dirs = ['', ...[...cache.keys()].filter((d) => d)].sort();
    const dirSel = h('select', { id: 'nf-dir', class: 'select mono' }, dirs.map((d) => h('option', { value: d, text: d ? d + '/' : t('files.rootDir') })));
    dirSel.value = cache.has(selectedDir) ? selectedDir : '';
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
      cur = { path, disk: '', sha: '', size: 0, mtime: null, isNew: true };
      editor.setValue(/\.xml$/i.test(path) ? '<include>\n  \n</include>\n' : '');
      showEditor();
      editor.focus();
      editor.select(11, 11);
    });
    name.focus();
  }

  async function removeFile() {
    if (!cur || cur.isNew) return;
    const path = cur.path;
    const ok = await actionDialog({
      title: t('files.del.title', { path: baseOf(path) }),
      message: t('files.del.text', { path }),
      confirmLabel: t('files.delete'), danger: true,
      run: () => api('DELETE', filesPath('/content?path=' + encodeURIComponent(path) + '&baseSha256=' + encodeURIComponent(cur.sha)), { signal: scope.signal }),
    });
    if (!ok || scope.disposed) return;
    toast(t('files.deleted', { path }));
    cur = null;
    editorWrap.hidden = true;
    main.prepend(placeholder);
    await refreshTree();
  }

  // ---------- find ----------
  function openFind() {
    if (!cur) return;
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

  // ---------- review and save ----------
  function reviewDialog({ title, before, after, note, confirmLabel, run }) {
    const diff = diffLines(before, after);
    const error = h('div', { class: 'form-error', role: 'alert', hidden: true });
    const cancel = h('button', { type: 'button', class: 'btn', text: t('common.cancel') });
    const ok = h('button', { type: 'button', class: 'btn btn-primary' }, icon('save'), confirmLabel);
    const xmlWarn = cur && /\.xml$/i.test(cur.path) && !wellFormed(after)
      ? h('p', { class: 'note-inline bad' }, icon('alert'), t('files.review.notWellFormed')) : null;
    const m = openModal({
      title,
      body: [
        h('div', { class: 'review-head' }, h('span', { class: 'mono', text: cur.path }), diffSummary(diff)),
        h('div', { class: 'review-diff' }, renderDiff(diff)),
        xmlWarn,
        h('p', { class: 'note-inline' }, icon('history'), note),
        error,
      ],
      footer: [cancel, ok],
    });
    m.dialog.classList.add('xwide');
    cancel.addEventListener('click', m.close);
    ok.addEventListener('click', async () => {
      ok.disabled = true;
      cancel.disabled = true;
      error.hidden = true;
      try {
        await run(m);
      } catch (err) {
        if (isAbort(err)) return;
        error.textContent = err.message;
        error.hidden = false;
        ok.disabled = false;
        cancel.disabled = false;
      }
    });
    ok.focus();
  }

  function save() {
    if (!cur || !dirty()) return;
    const content = editor.value();
    const wasNew = cur.isNew;
    reviewDialog({
      title: t('files.review.title'),
      before: cur.disk,
      after: content,
      note: wasNew ? t('files.review.newNote') : t('files.review.note'),
      confirmLabel: t('files.save'),
      run: async (m) => {
        try {
          const res = await api('PUT', filesPath('/content'), { body: { path: cur.path, content, baseSha256: cur.sha }, signal: scope.signal });
          m.close();
          saved(res, content, wasNew);
        } catch (err) {
          if (err.status === 422) {
            m.close();
            invalid(err);
          } else if (err.status === 409 && !/not set up/i.test(err.message)) {
            m.close();
            conflict(err, content);
          } else {
            throw err;
          }
        }
      },
    });
  }

  function saved(res, content, wasNew) {
    cur.disk = content;
    cur.sha = res.sha256;
    cur.size = res.size;
    cur.mtime = res.mtime;
    cur.isNew = false;
    histBtn.disabled = false;
    delBtn.disabled = false;
    editor.markError(null);
    changed();
    toast(t('files.saved', { path: cur.path }));
    applyBar({ restorable: !wasNew });
    loadDir(dirOf(cur.path)).then(renderTree, () => {});
  }

  // invalid shows the server's 422 at its line.
  function invalid(err) {
    const line = Number(err.line) || 0;
    msgs.replaceChildren(h('div', { class: 'fe-alert bad', role: 'alert' }, icon('alert'),
      h('div', { class: 'spacer' }, h('div', { class: 'fe-alert-title', text: line ? t('files.invalidAt', { line: fmtInt(line) }) : t('files.invalid') }), h('code', { class: 'fe-alert-raw', text: err.message })),
      line ? h('button', { type: 'button', class: 'btn btn-sm', on: { click: () => editor.revealLine(line) } }, t('files.goLine', { line: fmtInt(line) })) : null));
    if (line) {
      editor.markError(line);
      editor.revealLine(line);
    }
  }

  // conflict: the file changed on disk after it was read.
  function conflict(err, mine) {
    const gone = !err.currentSha256;
    const body = h('div', { class: 'modal-form' }, h('p', { class: 'modal-text', text: gone ? t('files.conflict.gone', { path: cur.path }) : t('files.conflict.text', { path: cur.path }) }));
    const cancel = h('button', { type: 'button', class: 'btn', text: t('files.conflict.keep') });
    const compare = h('button', { type: 'button', class: 'btn' }, icon('diff'), t('files.conflict.compare'));
    const reload = h('button', { type: 'button', class: 'btn', text: t('files.conflict.reload') });
    const overwrite = h('button', { type: 'button', class: 'btn btn-danger-solid', text: t('files.conflict.overwrite') });
    if (gone) {
      compare.hidden = true;
      reload.hidden = true;
    }
    const m = openModal({ title: t('files.conflict.title'), body, footer: [cancel, compare, reload, overwrite] });
    m.dialog.classList.add('xwide');
    cancel.addEventListener('click', m.close);
    compare.addEventListener('click', async () => {
      compare.disabled = true;
      try {
        const disk = await api('GET', filesPath('/content?path=' + encodeURIComponent(cur.path)), { signal: scope.signal });
        const diff = diffLines(disk.content, mine);
        body.append(h('div', { class: 'review-head' }, h('span', { class: 'faint', text: t('files.conflict.diffLabel') }), diffSummary(diff)), h('div', { class: 'review-diff' }, renderDiff(diff)));
      } catch (e) {
        if (!isAbort(e)) body.append(h('div', { class: 'form-error', text: e.message }));
      }
    });
    reload.addEventListener('click', () => {
      m.close();
      openFile(cur.path, { force: true });
    });
    overwrite.addEventListener('click', async () => {
      overwrite.disabled = true;
      try {
        const res = await api('PUT', filesPath('/content'), { body: { path: cur.path, content: mine, baseSha256: err.currentSha256 || '' }, signal: scope.signal });
        m.close();
        saved(res, mine, gone);
      } catch (e) {
        if (isAbort(e)) return;
        overwrite.disabled = false;
        body.append(h('div', { class: 'form-error', role: 'alert', text: e.message }));
      }
    });
  }

  // ---------- apply ----------
  function applyActions(path) {
    const list = [{ key: 'reloadxml', label: t('ops.reloadxml'), body: { action: 'reloadxml' } }];
    let profile = '';
    let m = /^sip_profiles\/([A-Za-z0-9_.-]+)\.xml$/.exec(path) || /^sip_profiles\/([A-Za-z0-9_.-]+)\//.exec(path);
    if (m) {
      profile = m[1];
      list.push({ key: 'rescan', label: t('files.apply.rescan', { profile }), body: { action: 'sofia_profile', profile, op: 'rescan' } });
    }
    if (path === 'autoload_configs/acl.conf.xml') list.push({ key: 'acl', label: t('ops.reloadacl'), body: { action: 'reloadacl' } });
    m = /^autoload_configs\/([a-z0-9_]+)\.conf\.xml$/.exec(path);
    if (m && !SKIP_MODULE_RELOAD.has(m[1]) && ctx.can('control')) {
      const module = 'mod_' + m[1];
      list.push({ key: 'module', label: t('files.apply.module', { module }), body: { action: 'module', module, op: 'reload' } });
    }
    return { list, profile };
  }

  function applyBar({ restorable }) {
    const path = cur.path;
    const { list, profile } = applyActions(path);
    const out = h('pre', { class: 'apply-out', hidden: true });
    const fix = h('div', { class: 'apply-fix', hidden: true });
    const buttons = list.map((a) => {
      const b = h('button', { type: 'button', class: 'btn btn-sm' }, icon('refresh'), a.label);
      b.addEventListener('click', () => run(a, b));
      return b;
    });
    const bar = h('div', { class: 'fe-alert apply', role: 'status' },
      h('div', { class: 'apply-top' }, icon('check'),
        h('div', { class: 'spacer' },
          h('div', { class: 'fe-alert-title', text: t('files.apply.title', { path: baseOf(path), time: fmtClock(Date.now(), true) }) }),
          h('div', { class: 'fe-alert-text', text: t('files.apply.text') })),
        h('button', { type: 'button', class: 'btn btn-ghost btn-sm btn-icon', 'aria-label': t('common.dismiss'), on: { click: () => bar.remove() } }, icon('x'))),
      h('div', { class: 'apply-actions' }, buttons),
      profile ? h('p', { class: 'apply-note' }, icon('info'), t('files.apply.restartNote', { profile })) : null,
      out, fix);
    msgs.replaceChildren(bar);

    async function run(a, btn) {
      btn.disabled = true;
      out.hidden = false;
      const line = h('div', { class: 'apply-line' }, h('span', { class: 'apply-cmd', text: '› ' + a.label }));
      out.append(line);
      let failed = false;
      try {
        const data = await api('POST', instPath(id, 'actions'), { body: a.body, signal: scope.signal });
        const text = (data && typeof data.output === 'string' ? data.output.trim() : '') || t('common.done');
        failed = /^-ERR/.test(text);
        line.append(h('span', { class: failed ? 'apply-err' : 'apply-ok', text }));
      } catch (err) {
        if (isAbort(err)) return;
        failed = true;
        line.append(h('span', { class: 'apply-err', text: err.message }));
      }
      btn.disabled = false;
      if (failed) offerFix();
    }

    function offerFix() {
      if (!fix.hidden) return;
      fix.hidden = false;
      fix.append(h('span', { class: 'apply-fix-text', text: restorable ? t('files.apply.failRestore') : t('files.apply.failNew') }),
        restorable
          ? h('button', { type: 'button', class: 'btn btn-sm btn-danger', on: { click: () => restorePrevious() } }, icon('undo'), t('files.apply.restore'))
          : h('button', { type: 'button', class: 'btn btn-sm btn-danger', on: { click: () => removeFile() } }, icon('trash'), t('files.apply.deleteNew')));
    }

    // restorePrevious puts back the version this save replaced and reloads.
    async function restorePrevious() {
      try {
        const hist = rowsOf(await api('GET', filesPath('/history?path=' + encodeURIComponent(path)), { signal: scope.signal }));
        if (!hist.length) throw new Error(t('files.noHistory'));
        const res = await api('POST', filesPath('/restore'), { body: { path, id: hist[0].id, baseSha256: cur.sha }, signal: scope.signal });
        const data = await api('GET', filesPath('/content?path=' + encodeURIComponent(path)), { signal: scope.signal });
        cur = { path, disk: data.content, sha: res.sha256 || data.sha256, size: data.size, mtime: data.mtime, isNew: false };
        editor.setValue(data.content);
        changed();
        const line = h('div', { class: 'apply-line' }, h('span', { class: 'apply-cmd', text: '› ' + t('files.apply.restored') }));
        out.append(line);
        const r = await api('POST', instPath(id, 'actions'), { body: { action: 'reloadxml' }, signal: scope.signal });
        const text = (r && r.output ? String(r.output).trim() : '') || t('common.done');
        out.append(h('div', { class: 'apply-line' }, h('span', { class: 'apply-cmd', text: '› ' + t('ops.reloadxml') }), h('span', { class: /^-ERR/.test(text) ? 'apply-err' : 'apply-ok', text })));
        fix.hidden = true;
      } catch (err) {
        if (isAbort(err)) return;
        out.append(h('div', { class: 'apply-line' }, h('span', { class: 'apply-err', text: err.message })));
      }
    }
  }

  // ---------- history ----------
  async function historyDrawer() {
    if (!cur || cur.isNew) return;
    const path = cur.path;
    const d = openDrawer({ kicker: t('files.history'), title: baseOf(path), subtitle: path });
    d.body.append(skeleton({ rows: 6, cols: 1 }));
    try {
      const rows = rowsOf(await api('GET', filesPath('/history?path=' + encodeURIComponent(path)), { signal: scope.signal }));
      const list = h('div', { class: 'hist' },
        h('div', { class: 'hist-row now' },
          h('div', { class: 'hist-main' }, h('div', { class: 'hist-time', text: t('files.hist.current') }), h('div', { class: 'hist-meta mono', text: fmtBytes(cur.size) + ' · ' + shortSha(cur.sha) }))),
        rows.length ? rows.map((r) => h('div', { class: 'hist-row' },
          h('div', { class: 'hist-main' },
            h('div', { class: 'hist-time', text: fmtDateTime(r.time) }),
            h('div', { class: 'hist-meta' }, h('span', { class: 'cell-strong', text: r.user || '–' }), h('span', { class: 'mono', text: ' · ' + fmtBytes(r.size) + ' · ' + shortSha(r.sha256) }))),
          h('button', { type: 'button', class: 'btn btn-sm', on: { click: () => viewVersion(path, r) } }, icon('diff'), t('files.hist.view')))) : h('p', { class: 'note-empty', text: t('files.hist.none') }));
      d.body.replaceChildren(h('p', { class: 'reader-note' }, icon('info'), t('files.hist.note')), list);
    } catch (err) {
      if (!isAbort(err)) d.body.replaceChildren(errorState(err));
    }
  }

  async function viewVersion(path, r) {
    let data;
    try {
      data = await api('GET', filesPath('/backup?path=' + encodeURIComponent(path) + '&id=' + encodeURIComponent(r.id)), { signal: scope.signal });
    } catch (err) {
      if (!isAbort(err)) toast(err.message, 'error');
      return;
    }
    const diff = diffLines(data.content, cur.disk);
    const view = h('div', { class: 'review-diff' }, renderDiff(diff));
    const seg = segmented([{ key: 'diff', label: t('files.hist.diff') }, { key: 'full', label: t('files.hist.full') }], 'diff', (k) => {
      view.replaceChildren(k === 'diff' ? renderDiff(diff) : h('pre', { class: 'hist-full', text: data.content }));
    }, 'sm');
    const close = h('button', { type: 'button', class: 'btn', text: t('common.close') });
    const restore = h('button', { type: 'button', class: 'btn btn-primary' }, icon('undo'), t('files.hist.restore'));
    const m = openModal({
      title: t('files.hist.versionTitle', { time: fmtDateTime(r.time) }),
      body: [h('div', { class: 'review-head' }, seg.el, h('span', { class: 'faint', text: t('files.hist.diffLabel') }), diffSummary(diff)), view],
      footer: [close, restore],
    });
    m.dialog.classList.add('xwide');
    close.addEventListener('click', m.close);
    restore.addEventListener('click', async () => {
      m.close();
      if (!(await confirmLeave())) return;
      closeDrawer();
      reviewDialog({
        title: t('files.hist.restoreTitle'),
        before: cur.disk,
        after: data.content,
        note: t('files.review.restoreNote'),
        confirmLabel: t('files.hist.restore'),
        run: async (rm) => {
          const res = await api('POST', filesPath('/restore'), { body: { path, id: r.id, baseSha256: cur.sha }, signal: scope.signal });
          rm.close();
          cur = { path, disk: data.content, sha: res.sha256, size: res.size, mtime: res.mtime, isNew: false };
          editor.setValue(data.content);
          showEditor();
          toast(t('files.restored', { path }));
          applyBar({ restorable: true });
        },
      });
    });
  }
}

// setupState explains the two ways to give fsdeck the configuration files and
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
