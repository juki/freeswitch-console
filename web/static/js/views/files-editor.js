// Files view, editor pane: loading a file into the editor, the dirty state and
// the XML check, saving through the diff review (and the answers to a refused
// save), and deleting.

import { api, isAbort } from '../api.js';
import { h, fmtInt } from '../dom.js';
import { icon } from '../icons.js';
import { plural, t } from '../i18n.js';
import { actionDialog, confirmLeave, emptyState, openModal, setLeaveGuard, toast } from '../ui.js';
import { diffLines, diffSummary, renderDiff } from '../diff.js';
import { wellFormed } from '../xml.js';
import { fileHash, getBack } from '../nav.js';
import { baseOf, dirOf } from './files-util.js';

// createEditorPane builds the right side around editor, status and findBar.
// It owns state.cur (with the tree, apply and history parts writing it too);
// the other parts are reached through the callbacks passed in.
export function createEditorPane({ id, ctx, scope, state, filesPath, editor, status, stLines, stXml, findBar, findBox, openFind, runFind, loadDir, renderTree, refreshTree, applyBar, historyDrawer }) {
  const { open, cache } = state;

  // ---------- layout ----------
  const pathEl = h('div', { class: 'fe-path' });
  const backLink = h('a', { class: 'btn btn-sm btn-ghost fe-back', hidden: true });
  const badge = h('span', { class: 'fe-badge', hidden: true });
  const findBtn = h('button', { type: 'button', class: 'btn btn-sm btn-ghost btn-icon', title: t('files.findTitle'), 'aria-label': t('files.findTitle'), on: { click: () => openFind() } }, icon('search'));
  const histBtn = h('button', { type: 'button', class: 'btn btn-sm', on: { click: () => historyDrawer() } }, icon('history'), t('files.history'));
  const delBtn = h('button', { type: 'button', class: 'btn btn-sm btn-ghost btn-icon btn-danger', title: t('files.delete'), 'aria-label': t('files.delete'), on: { click: () => removeFile() } }, icon('trash'));
  const saveBtn = h('button', { type: 'button', class: 'btn btn-sm btn-primary', title: t('files.saveTitle'), on: { click: () => save() } }, icon('save'), t('files.save'));
  const msgs = h('div', { class: 'fe-msgs' });

  const placeholder = emptyState({ icon: 'file', title: t('files.pick'), text: t('files.pickText') });
  const editorWrap = h('div', { class: 'fe-editor', hidden: true },
    h('div', { class: 'fe-head' }, backLink, pathEl, badge, h('span', { class: 'spacer' }), findBtn, histBtn, delBtn, saveBtn),
    h('p', { class: 'fe-secret' }, icon('lock'), t('files.secrets')),
    msgs, findBar, editor.el, status);
  const main = h('div', { class: 'fe-main' }, placeholder, editorWrap);

  // ---------- state ----------
  let xmlTimer = null;
  scope.onCleanup(() => clearTimeout(xmlTimer));
  const dirty = () => Boolean(state.cur) && (state.cur.isNew || editor.value() !== state.cur.disk);
  scope.onCleanup(setLeaveGuard(dirty));

  function changed() {
    const d = dirty();
    badge.hidden = !d;
    badge.textContent = state.cur && state.cur.isNew ? t('files.newBadge') : t('files.unsaved');
    saveBtn.disabled = !d;
    stLines.textContent = plural(editor.value().split('\n').length, t('files.lines.one', { n: fmtInt(editor.value().split('\n').length) }), t('files.lines.other', { n: fmtInt(editor.value().split('\n').length) }));
    clearTimeout(xmlTimer);
    if (state.cur && /\.xml$/i.test(state.cur.path)) xmlTimer = setTimeout(checkXml, 300);
    else stXml.replaceChildren();
    if (!findBar.hidden && findBox.input.value) runFind(true);
  }

  function checkXml() {
    const ok = wellFormed(editor.value());
    stXml.className = 'fe-xml ' + (ok ? 'ok' : 'bad');
    stXml.replaceChildren(icon(ok ? 'check' : 'alert'), ok ? t('files.xmlOk') : t('files.xmlBad'));
  }

  // ---------- open / delete ----------
  function showEditor() {
    placeholder.remove();
    editorWrap.hidden = false;
    pathEl.replaceChildren(...state.cur.path.split('/').flatMap((part, i, all) => [i ? h('span', { class: 'fe-sep', text: '/' }) : null, h('span', { class: i === all.length - 1 ? 'fe-file' : 'fe-dir', text: part })]).filter(Boolean));
    histBtn.disabled = state.cur.isNew;
    delBtn.disabled = state.cur.isNew;
    paintBack();
    if (!state.cur.isNew) ctx.setHash(fileHash(id, state.cur.path));
    editor.setXml(/\.xml$/i.test(state.cur.path));
    msgs.replaceChildren();
    editor.markError(null);
    changed();
    renderTree();
  }

  // paintBack shows "← Back to Dialplan" while the file is the one an
  // "Edit source" jump opened.
  function paintBack() {
    const b = state.cur && getBack(state.cur.path);
    backLink.hidden = !b;
    if (!b) return;
    backLink.href = b.hash;
    backLink.replaceChildren(icon('undo'), t('src.back', { view: b.label }));
  }

  // openAt opens a file at a line: the cursor goes there and the line flashes.
  async function openAt(path, line) {
    if (state.cur && state.cur.path === path) {
      if (line) {
        editor.flashLine(line);
        ctx.setHash(fileHash(id, path, line));
      }
      return;
    }
    // reveal the file in the tree too
    const parts = path.split('/');
    for (let i = 1; i < parts.length; i++) open.add(parts.slice(0, i).join('/'));
    await openFile(path);
    if (state.cur && state.cur.path === path && line) {
      editor.flashLine(line);
      ctx.setHash(fileHash(id, path, line));
    }
    for (const d of open) if (!cache.has(d)) loadDir(d).then(renderTree, () => {});
  }

  async function openFile(path, { force = false } = {}) {
    if (!force && state.cur && state.cur.path === path) return;
    if (!force && !(await confirmLeave())) return;
    state.selectedDir = dirOf(path);
    try {
      const data = await api('GET', filesPath('/content?path=' + encodeURIComponent(path)), { signal: scope.signal });
      state.cur = { path, disk: data.content, sha: data.sha256, size: data.size, mtime: data.mtime, isNew: false };
      editor.setValue(data.content);
      showEditor();
      editor.focus();
    } catch (err) {
      if (isAbort(err)) return;
      toast(err.message, 'error', t('files.openFailed', { path }));
    }
  }

  async function removeFile() {
    if (!state.cur || state.cur.isNew) return;
    const path = state.cur.path;
    const ok = await actionDialog({
      title: t('files.del.title', { path: baseOf(path) }),
      message: t('files.del.text', { path }),
      confirmLabel: t('files.delete'), danger: true,
      run: () => api('DELETE', filesPath('/content?path=' + encodeURIComponent(path) + '&baseSha256=' + encodeURIComponent(state.cur.sha)), { signal: scope.signal }),
    });
    if (!ok || scope.disposed) return;
    toast(t('files.deleted', { path }));
    state.cur = null;
    editorWrap.hidden = true;
    main.prepend(placeholder);
    await refreshTree();
  }

  // ---------- review and save ----------
  function reviewDialog({ title, before, after, note, confirmLabel, run }) {
    const diff = diffLines(before, after);
    const error = h('div', { class: 'form-error', role: 'alert', hidden: true });
    const cancel = h('button', { type: 'button', class: 'btn', text: t('common.cancel') });
    const ok = h('button', { type: 'button', class: 'btn btn-primary' }, icon('save'), confirmLabel);
    const xmlWarn = state.cur && /\.xml$/i.test(state.cur.path) && !wellFormed(after)
      ? h('p', { class: 'note-inline bad' }, icon('alert'), t('files.review.notWellFormed')) : null;
    const m = openModal({
      title,
      body: [
        h('div', { class: 'review-head' }, h('span', { class: 'mono', text: state.cur.path }), diffSummary(diff)),
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
    if (!state.cur || !dirty()) return;
    const content = editor.value();
    const wasNew = state.cur.isNew;
    reviewDialog({
      title: t('files.review.title'),
      before: state.cur.disk,
      after: content,
      note: wasNew ? t('files.review.newNote') : t('files.review.note'),
      confirmLabel: t('files.save'),
      run: async (m) => {
        try {
          const res = await api('PUT', filesPath('/content'), { body: { path: state.cur.path, content, baseSha256: state.cur.sha }, signal: scope.signal });
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
    state.cur.disk = content;
    state.cur.sha = res.sha256;
    state.cur.size = res.size;
    state.cur.mtime = res.mtime;
    state.cur.isNew = false;
    histBtn.disabled = false;
    delBtn.disabled = false;
    editor.markError(null);
    changed();
    toast(t('files.saved', { path: state.cur.path }));
    applyBar({ restorable: !wasNew });
    loadDir(dirOf(state.cur.path)).then(renderTree, () => {});
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
    const body = h('div', { class: 'modal-form' }, h('p', { class: 'modal-text', text: gone ? t('files.conflict.gone', { path: state.cur.path }) : t('files.conflict.text', { path: state.cur.path }) }));
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
        const disk = await api('GET', filesPath('/content?path=' + encodeURIComponent(state.cur.path)), { signal: scope.signal });
        const diff = diffLines(disk.content, mine);
        body.append(h('div', { class: 'review-head' }, h('span', { class: 'faint', text: t('files.conflict.diffLabel') }), diffSummary(diff)), h('div', { class: 'review-diff' }, renderDiff(diff)));
      } catch (e) {
        if (!isAbort(e)) body.append(h('div', { class: 'form-error', text: e.message }));
      }
    });
    reload.addEventListener('click', () => {
      m.close();
      openFile(state.cur.path, { force: true });
    });
    overwrite.addEventListener('click', async () => {
      overwrite.disabled = true;
      try {
        const res = await api('PUT', filesPath('/content'), { body: { path: state.cur.path, content: mine, baseSha256: err.currentSha256 || '' }, signal: scope.signal });
        m.close();
        saved(res, mine, gone);
      } catch (e) {
        if (isAbort(e)) return;
        overwrite.disabled = false;
        body.append(h('div', { class: 'form-error', role: 'alert', text: e.message }));
      }
    });
  }

  return { main, msgs, changed, save, showEditor, openAt, openFile, removeFile, reviewDialog };
}
