// "Edit source": from what a read-only view shows (a configuration, a
// dialplan context or extension, a directory domain or user, a global
// variable) to the line of the file that defines it, in the Files editor.
//
// The defining line is found with the files search (GET .../files/search,
// case-sensitive, literal). One match opens the file at that line; several
// open a chooser; none says where else the value may come from and offers a
// search. The action needs the admin role, the config-edit licence feature
// and config access for the instance; without the licence or the access it
// is shown in its locked or "set up" form, without the role not at all.

import { api, instPath, isAbort, rowsOf } from '../api.js';
import { h, fmtInt } from '../dom.js';
import { icon } from '../icons.js';
import { plural, t } from '../i18n.js';
import { openModal, paidState, toast } from '../ui.js';
import { fileHash, instHash, setBack } from '../nav.js';
import { instanceDialog } from './fleet.js';

// searchFiles is the one place the files search is called.
export async function searchFiles(id, q, { caseSensitive = false, limit = 100, signal } = {}) {
  const params = new URLSearchParams({ q, case: caseSensitive ? '1' : '0', limit: String(limit) });
  const data = await api('GET', instPath(id, 'files/search?' + params.toString()), { signal });
  return { rows: rowsOf(data), truncated: Boolean(data && data.truncated) };
}

// marked renders text with every occurrence of q wrapped in <mark>, as text
// nodes only (file content is untrusted).
export function marked(text, q, caseSensitive) {
  const s = String(text || '');
  if (!q) return [s];
  const hay = caseSensitive ? s : s.toLowerCase();
  const needle = caseSensitive ? q : q.toLowerCase();
  const out = [];
  let at = 0;
  for (let i = hay.indexOf(needle); i >= 0 && needle; i = hay.indexOf(needle, i + needle.length)) {
    if (i > at) out.push(s.slice(at, i));
    out.push(h('mark', { class: 'hit', text: s.slice(i, i + needle.length) }));
    at = i + needle.length;
  }
  if (at < s.length) out.push(s.slice(at));
  return out;
}

// What each kind of item looks like in a FreeSWITCH XML file.
const PATTERNS = {
  configuration: (name) => '<configuration name="' + name + '"',
  context: (name) => '<context name="' + name + '"',
  extension: (name) => '<extension name="' + name + '"',
  domain: (name) => '<domain name="' + name + '"',
  user: (name) => '<user id="' + name + '"',
  variable: (name) => 'data="' + name + '=',
};

// preferred tells whether a match lies where this kind of item is defined
// for the view it came from: a dialplan context under dialplan/ (chatplan
// has contexts of the same name), an extension in its own context's files,
// a domain or user under directory/, a configuration under autoload_configs/,
// a variable in vars.xml.
function preferred(kind, path, hint) {
  switch (kind) {
    case 'configuration': return path.startsWith('autoload_configs/');
    case 'context': return path.startsWith('dialplan/');
    case 'extension': return Boolean(hint) && (path === 'dialplan/' + hint + '.xml' || path.startsWith('dialplan/' + hint + '/'));
    case 'domain':
    case 'user': return path.startsWith('directory/');
    case 'variable': return path === 'vars.xml' || path.endsWith('/vars.xml');
    default: return false;
  }
}

const isPointer = (r) => /type\s*=\s*"pointer"/.test(r.text);

// rank puts the preferred matches first, a user's group pointers after its
// definition, and keeps the server's order otherwise.
function rank(kind, rows, hint) {
  return rows
    .map((r, i) => ({ ...r, preferred: preferred(kind, r.path, hint), pointer: kind === 'user' && isPointer(r), i }))
    .sort((a, b) => (b.preferred - a.preferred) || (a.pointer - b.pointer) || (a.i - b.i));
}

// sourceAction returns the "Edit source" control for one item, or null when
// the role cannot edit files. compact: an icon button for table rows.
// label: the name of the view to come back to.
export function sourceAction(ctx, { id, scope, kind, name, hint, label, back, compact = false }) {
  const access = ctx.access('files');
  if (access === 'role' || !name) return null;
  const inst = ctx.instances().find((i) => i.id === id);
  const state = access === 'licence' ? 'licence' : inst && !inst.configMode ? 'setup' : 'ok';
  const title = state === 'licence' ? t('src.lockedTitle') : state === 'setup' ? t('src.setupTitle') : t('src.title');
  const glyph = { ok: 'file', licence: 'lock', setup: 'sliders' }[state];
  const content = compact ? [icon(glyph)] : [icon(glyph), t('src.action')];
  const btn = h('button', {
    type: 'button', class: (compact ? 'btn btn-ghost btn-icon btn-sm' : 'btn btn-sm') + ' src-btn' + (state === 'ok' ? '' : ' locked'),
    title, 'aria-label': t('src.ariaFor', { name }),
  }, content);
  btn.addEventListener('click', (ev) => {
    ev.stopPropagation();
    if (state === 'licence') {
      lockedDialog();
    } else if (state === 'setup') {
      if (ctx.can('manage')) instanceDialog(ctx, inst, { focusConfig: true });
    } else {
      editSource({ id, scope, kind, name, hint, label, back, btn });
    }
  });
  return btn;
}

function lockedDialog() {
  const close = h('button', { type: 'button', class: 'btn', text: t('common.close') });
  const m = openModal({ title: t('src.action'), body: paidState('config-edit'), footer: [close] });
  m.dialog.classList.add('wide');
  close.addEventListener('click', m.close);
  close.focus();
}

function go(id, row, { label, back }) {
  setBack({ hash: back || location.hash, label, path: row.path });
  location.hash = fileHash(id, row.path, row.line);
}

// editSource finds the defining line and opens it, or lets the user choose.
export async function editSource({ id, scope, kind, name, hint, label, back, btn }) {
  const pattern = PATTERNS[kind](name);
  if (btn) btn.classList.add('busy');
  let found;
  try {
    found = await searchFiles(id, pattern, { caseSensitive: true, limit: 50, signal: scope.signal });
    // vanilla FreeSWITCH writes <domain name="$${domain}">: fall back to any domain
    if (kind === 'domain' && !found.rows.length) found = await searchFiles(id, '<domain name=', { caseSensitive: true, limit: 50, signal: scope.signal });
  } catch (err) {
    if (isAbort(err)) return;
    toast(err.message, 'error', t('src.failed'));
    return;
  } finally {
    if (btn) btn.classList.remove('busy');
  }
  const rows = rank(kind, found.rows, hint);
  const best = rows.filter((r) => r.preferred);
  // straight to the file when one match is clearly it
  if (!found.truncated && (best.length === 1 || rows.length === 1)) {
    go(id, best[0] || rows[0], { label, back });
    return;
  }
  if (!rows.length) {
    notFound(id, name, pattern, { label, back });
    return;
  }
  chooser(id, name, pattern, rows, found.truncated, { label, back });
}

function chooser(id, name, pattern, rows, truncated, opts) {
  const close = h('button', { type: 'button', class: 'btn', text: t('common.cancel') });
  const anyPreferred = rows.some((r) => r.preferred);
  const items = [];
  rows.forEach((r, i) => {
    // a divider between where it should be and other places it also appears
    if (anyPreferred && !r.preferred && (i === 0 || rows[i - 1].preferred)) items.push(h('div', { class: 'src-sep', text: t('src.elsewhere') }));
    items.push(h('button', { type: 'button', class: 'src-row' + (r.pointer ? ' pointer' : ''), role: 'option', on: { click: () => { m.close(); go(id, r, opts); } } },
      h('span', { class: 'src-path' },
        h('span', { class: 'mono', text: r.path }),
        h('span', { class: 'src-line num', text: t('src.line', { line: fmtInt(r.line) }) }),
        r.pointer ? h('span', { class: 'src-tag', text: t('src.pointer') }) : i === 0 && (r.preferred || !anyPreferred) ? h('span', { class: 'src-likely', text: t('src.likely') }) : null),
      h('code', { class: 'src-text' }, marked(r.text, pattern, true))));
  });
  const list = h('div', { class: 'src-list', role: 'listbox' }, items);
  list.addEventListener('keydown', (ev) => {
    if (ev.key !== 'ArrowDown' && ev.key !== 'ArrowUp') return;
    ev.preventDefault();
    const rowsEls = [...list.querySelectorAll('.src-row')];
    const i = rowsEls.indexOf(document.activeElement);
    rowsEls[(i + (ev.key === 'ArrowDown' ? 1 : rowsEls.length - 1)) % rowsEls.length].focus();
  });
  const m = openModal({
    title: t('src.choose.title', { name }),
    body: [
      h('p', { class: 'modal-text', text: plural(rows.length, t('src.choose.text.one', { n: fmtInt(rows.length) }), t('src.choose.text.other', { n: fmtInt(rows.length) })) }),
      list,
      truncated ? h('p', { class: 'note-inline warn' }, icon('info'), t('src.truncated')) : null,
    ],
    footer: [close],
  });
  m.dialog.classList.add('wide');
  close.addEventListener('click', m.close);
  list.querySelector('.src-row').focus();
}

function notFound(id, name, pattern, opts) {
  const close = h('button', { type: 'button', class: 'btn', text: t('common.close') });
  const search = h('button', { type: 'button', class: 'btn btn-primary' }, icon('search'), t('src.none.search'));
  const m = openModal({
    title: t('src.none.title', { name }),
    body: [
      h('p', { class: 'modal-text', text: t('src.none.text') }),
      h('p', { class: 'modal-text' }, t('src.none.looked'), ' ', h('code', { class: 'raw-error', text: pattern })),
    ],
    footer: [close, search],
  });
  close.addEventListener('click', m.close);
  search.addEventListener('click', () => {
    m.close();
    setBack({ hash: opts.back || location.hash, label: opts.label, path: '' });
    location.hash = instHash(id, 'config', 'files', { q: name });
  });
  search.focus();
}
