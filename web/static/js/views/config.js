// Configuration: effective config, dialplan and directory in an XML browser,
// global variables, modules.

import { api, instPath, isAbort, rowsOf } from '../api.js';
import { h, cellText, fmtInt } from '../dom.js';
import { t } from '../i18n.js';
import { dataTable, emptyState, panel, rowMatches, searchBox, segmented, sortState, tableHost, tag } from '../ui.js';
import { mountXmlBrowser } from './xmlbrowser.js';

const SUBS = {
  configuration: (o) => mountXmlBrowser({ ...o, kind: 'configuration' }),
  dialplan: (o) => mountXmlBrowser({ ...o, kind: 'dialplan' }),
  directory: (o) => mountXmlBrowser({ ...o, kind: 'directory' }),
  vars: mountVars,
  modules: mountModules,
};

export function mountConfig({ id, scope, root }) {
  const body = h('div');
  let sub = null;
  function show(key) {
    if (sub) sub.dispose();
    sub = scope.child();
    body.replaceChildren();
    SUBS[key]({ id, scope: sub, root: body });
  }
  const seg = segmented([
    { key: 'configuration', label: t('cfg.sub.configuration') },
    { key: 'dialplan', label: t('cfg.sub.dialplan') },
    { key: 'directory', label: t('cfg.sub.directory') },
    { key: 'vars', label: t('cfg.sub.vars') },
    { key: 'modules', label: t('cfg.sub.modules') },
  ], 'configuration', show);
  root.append(h('div', { class: 'toolbar' }, seg.el,
    h('span', { class: 'spacer' }),
    h('span', { class: 'faint toolbar-note', text: t('cfg.note') })), body);
  show('configuration');
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

function mountModules({ id, scope, root }) {
  let rows = null;
  let query = '';
  let type = '';
  const facets = h('div', { class: 'facets' });
  const search = searchBox(t('cfg.mod.filter'), (q) => {
    query = q;
    render();
  });
  const p = panel({ title: t('cfg.sub.modules'), flush: true, actions: search.el });
  search.el.classList.add('sm');
  const sort = sortState(() => render());
  const host = tableHost();
  p.body.append(h('div', { class: 'panel-body' }, facets), host.el);
  root.append(p.el);

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
      ],
    }));
  }

  api('GET', instPath(id, 'modules'), { signal: scope.signal }).then((data) => {
    rows = rowsOf(data);
    renderFacets();
    render();
  }, (err) => {
    if (!isAbort(err)) host.error(err);
  });
}
