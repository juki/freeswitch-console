// Configuration: effective config, dialplan and directory in an XML browser,
// global variables, modules, the config file editor (Files) and the switch's
// runtime settings. Reload XML / Reload ACL sit in the toolbar.

import { api, instPath, isAbort, rowsOf } from '../api.js';
import { h, cellText, fmtInt } from '../dom.js';
import { t } from '../i18n.js';
import { icon } from '../icons.js';
import { confirmLeave, dataTable, emptyState, panel, paidState, rowMatches, rowMenu, searchBox, segmented, sortState, tableHost, tag } from '../ui.js';
import { operate } from '../actions.js';
import { mountXmlBrowser } from './xmlbrowser.js';
import { mountFiles } from './files.js';
import { mountRuntime } from './runtime.js';

const SUBS = {
  configuration: (o) => mountXmlBrowser({ ...o, kind: 'configuration' }),
  dialplan: (o) => mountXmlBrowser({ ...o, kind: 'dialplan' }),
  directory: (o) => mountXmlBrowser({ ...o, kind: 'directory' }),
  vars: mountVars,
  modules: mountModules,
  files: (o) => (o.ctx.access('files') === 'licence' ? o.root.append(paidState('config-edit')) : mountFiles(o)),
  runtime: mountRuntime,
};

// The Files sub-tab remembers itself per page load, so a reload of the
// browser tab comes back to the editor.
let lastSub = 'configuration';

export function mountConfig({ id, scope, root, ctx }) {
  const body = h('div');
  let sub = null;
  let currentKey = null;
  const options = [
    { key: 'configuration', label: t('cfg.sub.configuration') },
    { key: 'dialplan', label: t('cfg.sub.dialplan') },
    { key: 'directory', label: t('cfg.sub.directory') },
    { key: 'vars', label: t('cfg.sub.vars') },
    { key: 'modules', label: t('cfg.sub.modules') },
    ctx.access('files') !== 'role' ? { key: 'files', label: t('cfg.sub.files') } : null,
    ctx.can('control') ? { key: 'runtime', label: t('cfg.sub.runtime') } : null,
  ].filter(Boolean);
  function show(key) {
    if (sub) sub.dispose();
    sub = scope.child();
    currentKey = key;
    lastSub = key;
    body.replaceChildren();
    SUBS[key]({ id, ctx, scope: sub, root: body });
    note.hidden = key === 'files' || key === 'runtime';
  }
  const seg = segmented(options, 'configuration', async (key) => {
    if (key === currentKey) return;
    if (!(await confirmLeave())) {
      seg.set(currentKey);
      return;
    }
    show(key);
  });
  const lockBtn = seg.el.querySelector('button:nth-child(' + (options.findIndex((o) => o.key === 'files') + 1) + ')');
  if (lockBtn && ctx.access('files') === 'licence') lockBtn.append(icon('lock', 'seg-lock'));
  const note = h('span', { class: 'faint toolbar-note', text: t('cfg.note') });
  const reload = (action, label) => h('button', {
    type: 'button', class: 'btn btn-sm', title: action === 'reloadxml' ? t('cfg.reloadxmlTitle') : t('cfg.reloadaclTitle'),
    on: { click: () => operate({ id, scope, body: { action }, done: label, failed: t('ops.failed', { op: label }) }) },
  }, icon('refresh'), label);
  root.append(h('div', { class: 'toolbar' }, seg.el,
    h('span', { class: 'spacer' }), note,
    ctx.can('operate') ? h('span', { class: 'row' }, reload('reloadxml', t('ops.reloadxml')), reload('reloadacl', t('ops.reloadacl'))) : null), body);
  const start = options.some((o) => o.key === lastSub) ? lastSub : 'configuration';
  seg.set(start);
  show(start);
}

function mountVars({ id, scope, root }) {
  let rows = null;
  let query = '';
  const search = searchBox(t('cfg.vars.filter'), (q) => {
    query = q;
    render();
  });
  const p = panel({ title: t('cfg.sub.vars'), flush: true, actions: search.el });
  search.el.classList.add('sm');
  const sort = sortState(() => render());
  const host = tableHost();
  p.body.append(host.el);
  root.append(p.el);

  function render() {
    if (!rows) return;
    const shown = rows.filter((r) => rowMatches(r, query));
    p.setCount(query ? t('common.nOfM', { n: fmtInt(shown.length), m: fmtInt(rows.length) }) : rows.length);
    host.show(dataTable({
      rows: shown,
      sort,
      empty: emptyState({ icon: 'search', title: query ? t('common.noMatch') : t('cfg.vars.none') }),
      columns: [
        { label: t('cfg.col.name'), mono: true, shrink: true, sortBy: (r) => cellText(r.name), render: (r) => h('span', { class: 'cell-strong', text: cellText(r.name) }) },
        { label: t('cfg.col.value'), mono: true, wrap: true, sortBy: (r) => cellText(r.value), render: (r) => (r.value === '********' ? h('span', { class: 'masked', text: t('cfg.masked') }) : cellText(r.value)) },
      ],
    }));
  }

  api('GET', instPath(id, 'vars'), { signal: scope.signal }).then((data) => {
    rows = rowsOf(data);
    render();
  }, (err) => {
    if (!isAbort(err)) host.error(err);
  });
}

function mountModules({ id, scope, root, ctx }) {
  const canControl = ctx.can('control');
  let rows = null;
  let query = '';
  let type = '';
  const facets = h('div', { class: 'facets' });
  const search = searchBox(t('cfg.mod.filter'), (q) => {
    query = q;
    render();
  });
  // admin: load a module by name
  const loadInput = h('input', { class: 'input mono', type: 'text', placeholder: 'mod_example', 'aria-label': t('mod.loadName'), spellcheck: false, autocomplete: 'off' });
  const loadForm = canControl ? h('form', { class: 'inline-form' }, loadInput,
    h('button', { type: 'submit', class: 'btn btn-sm' }, icon('plus'), t('mod.load'))) : null;
  if (loadForm) {
    loadForm.addEventListener('submit', (ev) => {
      ev.preventDefault();
      const name = loadInput.value.trim();
      if (!/^mod_[a-z0-9_]+$/.test(name)) {
        loadInput.setAttribute('aria-invalid', 'true');
        loadInput.title = t('mod.nameRule');
        return;
      }
      loadInput.removeAttribute('aria-invalid');
      moduleOp(name, 'load');
    });
  }
  const p = panel({ title: t('cfg.sub.modules'), flush: true, actions: h('span', { class: 'row' }, loadForm, search.el) });
  search.el.classList.add('sm');
  const sort = sortState(() => render());
  const host = tableHost();
  p.body.append(h('div', { class: 'panel-body' }, facets), host.el);
  root.append(p.el);

  async function moduleOp(module, op) {
    const label = { load: t('mod.load'), reload: t('mod.reload'), unload: t('mod.unload') }[op];
    const confirm = op === 'load' ? null : {
      title: op === 'reload' ? t('mod.reload.title', { module }) : t('mod.unload.title', { module }),
      message: op === 'reload' ? t('mod.reload.text', { module }) : t('mod.unload.text', { module }),
      confirmLabel: label, danger: op === 'unload',
    };
    const out = await operate({ id, scope, body: { action: 'module', module, op }, confirm, done: t('sip.op.done', { op: label, name: module }), failed: t('sip.op.failed', { op: label, name: module }) });
    if (out !== null) load();
  }

  function renderFacets() {
    const counts = new Map();
    for (const r of rows) counts.set(cellText(r.type), (counts.get(cellText(r.type)) || 0) + 1);
    const facet = (key, label, n) => h('button', { type: 'button', class: 'facet', 'aria-pressed': String(type === key), on: { click: () => { type = key; renderFacets(); render(); } } },
      label, h('span', { class: 'count', text: String(n) }));
    facets.replaceChildren(facet('', t('common.all'), rows.length), ...[...counts].sort((a, b) => b[1] - a[1]).map(([k, n]) => facet(k, k, n)));
  }

  function render() {
    if (!rows) return;
    const shown = rows.filter((r) => (!type || cellText(r.type) === type) && rowMatches(r, query));
    p.setCount(shown.length === rows.length ? rows.length : t('common.nOfM', { n: fmtInt(shown.length), m: fmtInt(rows.length) }));
    host.show(dataTable({
      rows: shown,
      sort,
      empty: emptyState({ icon: 'search', title: t('common.noMatch') }),
      columns: [
        { label: t('cfg.col.name'), mono: true, sortBy: (r) => cellText(r.name), render: (r) => h('span', { class: 'cell-strong', text: cellText(r.name) }) },
        { label: t('cfg.col.type'), sortBy: (r) => cellText(r.type), render: (r) => tag(cellText(r.type)) },
        { label: t('cfg.col.provider'), mono: true, key: 'ikey', sortBy: (r) => cellText(r.ikey) },
        { label: t('cfg.col.file'), mono: true, sortBy: (r) => cellText(r.filename), render: (r) => h('span', { class: 'faint', title: cellText(r.filename), text: cellText(r.filename).split('/').pop() }) },
        canControl ? {
          label: '', right: true,
          render: (r) => (/^mod_[a-z0-9_]+$/.test(cellText(r.ikey)) ? rowMenu(t('sip.op.menu', { name: cellText(r.ikey) }), [
            { label: t('mod.reload'), icon: 'refresh', run: () => moduleOp(cellText(r.ikey), 'reload') },
            { label: t('mod.unload'), icon: 'power', danger: true, run: () => moduleOp(cellText(r.ikey), 'unload') },
          ]) : ''),
        } : null,
      ].filter(Boolean),
    }));
  }

  function load() {
    api('GET', instPath(id, 'modules'), { signal: scope.signal }).then((data) => {
      rows = rowsOf(data);
      renderFacets();
      render();
    }, (err) => {
      if (!isAbort(err)) host.error(err);
    });
  }
  load();
}
