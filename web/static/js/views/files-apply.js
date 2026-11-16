// Files view, apply bar: after a save, the reloads that fit the file,
// FreeSWITCH's answers as they are, and the way back to the previous version.

import { api, instPath, isAbort, rowsOf } from '../api.js';
import { h, fmtClock } from '../dom.js';
import { icon } from '../icons.js';
import { t } from '../i18n.js';
import { getBack } from '../nav.js';
import { baseOf } from './files-util.js';

const SKIP_MODULE_RELOAD = new Set(['acl', 'sofia', 'switch', 'modules', 'post_load_modules', 'console', 'logfile', 'event_socket']);

// createApply returns applyBar({restorable}), which shows the bar in msgs for
// state.cur; restoring writes state.cur and the editor, then calls changed().
export function createApply({ id, ctx, scope, state, filesPath, msgs, editor, changed, removeFile }) {
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
    const path = state.cur.path;
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
      else offerBack();
    }

    // offerBack: once FreeSWITCH has taken the change, go back to the view the
    // "Edit source" jump came from to see the effect (it reloads its data).
    function offerBack() {
      const b = getBack(path);
      if (!b || bar.querySelector('.apply-back')) return;
      bar.querySelector('.apply-actions').append(h('a', { class: 'btn btn-sm btn-primary apply-back', href: b.hash }, icon('undo'), t('src.backSee', { view: b.label })));
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
        const res = await api('POST', filesPath('/restore'), { body: { path, id: hist[0].id, baseSha256: state.cur.sha }, signal: scope.signal });
        const data = await api('GET', filesPath('/content?path=' + encodeURIComponent(path)), { signal: scope.signal });
        state.cur = { path, disk: data.content, sha: res.sha256 || data.sha256, size: data.size, mtime: data.mtime, isNew: false };
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

  return { applyBar };
}
