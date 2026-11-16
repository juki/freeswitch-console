// Files view, history: the versions the server kept of the open file, a diff
// or full view of one, and restoring it through the review dialog.

import { api, isAbort, rowsOf } from '../api.js';
import { h, fmtBytes, fmtDateTime } from '../dom.js';
import { icon } from '../icons.js';
import { t } from '../i18n.js';
import { confirmLeave, errorState, openDrawer, closeDrawer, openModal, segmented, skeleton, toast } from '../ui.js';
import { diffLines, diffSummary, renderDiff } from '../diff.js';
import { baseOf } from './files-util.js';

const shortSha = (s) => String(s || '').slice(0, 10);

// createHistory returns historyDrawer() for state.cur. A restore goes through
// reviewDialog, then writes state.cur and the editor and calls showEditor()
// and applyBar().
export function createHistory({ scope, state, filesPath, editor, showEditor, applyBar, reviewDialog }) {
  // ---------- history ----------
  async function historyDrawer() {
    if (!state.cur || state.cur.isNew) return;
    const path = state.cur.path;
    const d = openDrawer({ kicker: t('files.history'), title: baseOf(path), subtitle: path });
    d.body.append(skeleton({ rows: 6, cols: 1 }));
    try {
      const rows = rowsOf(await api('GET', filesPath('/history?path=' + encodeURIComponent(path)), { signal: scope.signal }));
      const list = h('div', { class: 'hist' },
        h('div', { class: 'hist-row now' },
          h('div', { class: 'hist-main' }, h('div', { class: 'hist-time', text: t('files.hist.current') }), h('div', { class: 'hist-meta mono', text: fmtBytes(state.cur.size) + ' · ' + shortSha(state.cur.sha) }))),
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
    const diff = diffLines(data.content, state.cur.disk);
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
        before: state.cur.disk,
        after: data.content,
        note: t('files.review.restoreNote'),
        confirmLabel: t('files.hist.restore'),
        run: async (rm) => {
          const res = await api('POST', filesPath('/restore'), { body: { path, id: r.id, baseSha256: state.cur.sha }, signal: scope.signal });
          rm.close();
          state.cur = { path, disk: data.content, sha: res.sha256, size: res.size, mtime: res.mtime, isNew: false };
          editor.setValue(data.content);
          showEditor();
          toast(t('files.restored', { path }));
          applyBar({ restorable: true });
        },
      });
    });
  }

  return { historyDrawer };
}
