// Configuration: effective config with an XML viewer, global variables, modules.

import { api, instPath, isAbort, rowsOf } from '../api.js';
import { h, cellText } from '../dom.js';
import { icon } from '../icons.js';
import { copyText, dataTable, emptyState, errorState, loading, panel, rowMatches, searchBox, segmented, sortState, tableHost, tag, toast } from '../ui.js';
import { renderXML } from '../xml.js';

const SUBS = {
  configuration: mountConfigurations,
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
    { key: 'configuration', label: 'Configuration' },
    { key: 'vars', label: 'Global variables' },
    { key: 'modules', label: 'Modules' },
  ], 'configuration', show);
  root.append(h('div', { class: 'toolbar' }, seg.el,
    h('span', { class: 'spacer' }),
    h('span', { class: 'faint', text: 'Read-only view of what FreeSWITCH has loaded. Secrets are masked.' })), body);
  show('configuration');
}

function mountConfigurations({ id, scope, root }) {
  const list = h('div', { class: 'conf-list' });
  const title = h('div', { class: 'conf-title', text: 'Select a configuration' });
  const desc = h('div', { class: 'faint' });
  const copyBtn = h('button', { type: 'button', class: 'btn btn-sm', disabled: true }, icon('copy'), 'Copy');
  const view = h('div', { class: 'conf-view' });
  let items = [];
  let raw = '';
  let loadScope = null;

  const search = searchBox('Filter configurations', (q) => {
    let shown = 0;
    for (const it of items) {
      it.btn.hidden = Boolean(q) && !it.key.includes(q);
      if (!it.btn.hidden) shown++;
    }
    noMatch.hidden = !items.length || shown > 0;
  });
  const noMatch = h('div', { class: 'loading', text: 'No match.', hidden: true });

  const p = h('section', { class: 'panel' },
    h('div', { class: 'conf' },
      h('div', { class: 'conf-side' }, h('div', { class: 'conf-search' }, search.el), list),
      h('div', { class: 'conf-main' },
        h('div', { class: 'conf-head' }, h('div', { class: 'spacer' }, title, desc), copyBtn),
        view)));
  root.append(p);
  list.append(loading());

  async function open(item) {
    for (const it of items) it.btn.setAttribute('aria-pressed', String(it === item));
    if (loadScope) loadScope.dispose();
    loadScope = scope.child();
    title.textContent = item.name;
    desc.textContent = item.description;
    copyBtn.disabled = true;
    view.replaceChildren(loading());
    try {
      const data = await api('GET', instPath(id, 'config/' + encodeURIComponent(item.name)), { signal: loadScope.signal });
      raw = data && typeof data.xml === 'string' ? data.xml : '';
      view.replaceChildren(renderXML(raw));
      view.scrollTop = 0;
      copyBtn.disabled = false;
    } catch (err) {
      if (!isAbort(err)) view.replaceChildren(errorState(err));
    }
  }

  copyBtn.addEventListener('click', async () => {
    if (await copyText(raw)) toast('Copied ' + title.textContent + ' to the clipboard.');
    else toast('The browser did not allow copying.', 'error');
  });

  api('GET', instPath(id, 'config'), { signal: scope.signal }).then((data) => {
    items = rowsOf(data).filter((r) => r && r.name).map((r) => {
      const item = { name: String(r.name), description: cellText(r.description) };
      item.key = (item.name + ' ' + item.description).toLowerCase();
      item.btn = h('button', { type: 'button', class: 'conf-item', 'aria-pressed': 'false', on: { click: () => open(item) } },
        h('span', { class: 'conf-name', text: item.name }),
        item.description ? h('span', { class: 'conf-desc', text: item.description }) : null);
      return item;
    });
    list.replaceChildren(...items.map((it) => it.btn), noMatch);
    if (!items.length) {
      list.replaceChildren(emptyState({ icon: 'sliders', title: 'No configurations' }));
      view.replaceChildren();
      return;
    }
    open(items.find((it) => it.name === 'sofia.conf') || items[0]);
  }, (err) => {
    if (isAbort(err)) return;
    list.replaceChildren();
    view.replaceChildren(errorState(err));
  });
  view.append(emptyState({ icon: 'sliders', title: 'Select a configuration', text: 'Pick a file on the left to see it as FreeSWITCH parsed it.' }));
}

function mountVars({ id, scope, root }) {
  let rows = null;
  let query = '';
  const search = searchBox('Filter variables', (q) => {
    query = q;
    render();
  });
  const p = panel({ title: 'Global variables', flush: true, actions: search.el });
  const sort = sortState(() => render());
  const host = tableHost();
  p.body.append(host.el);
  root.append(p.el);

  function render() {
    if (!rows) return;
    const shown = rows.filter((r) => rowMatches(r, query));
    p.setCount(query ? shown.length + ' of ' + rows.length : rows.length);
    host.show(dataTable({
      rows: shown,
      sort,
      empty: emptyState({ icon: 'search', title: query ? 'No match' : 'No global variables' }),
      columns: [
        { label: 'Name', mono: true, shrink: true, sortBy: (r) => cellText(r.name), render: (r) => h('span', { class: 'cell-strong', text: cellText(r.name) }) },
        { label: 'Value', mono: true, wrap: true, sortBy: (r) => cellText(r.value), render: (r) => (r.value === '********' ? h('span', { class: 'masked', text: 'masked' }) : cellText(r.value)) },
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
  const search = searchBox('Filter modules', (q) => {
    query = q;
    render();
  });
  const p = panel({ title: 'Modules', flush: true, actions: search.el });
  const sort = sortState(() => render());
  const host = tableHost();
  p.body.append(h('div', { class: 'panel-body' }, facets), host.el);
  root.append(p.el);

  function renderFacets() {
    const counts = new Map();
    for (const r of rows) counts.set(cellText(r.type), (counts.get(cellText(r.type)) || 0) + 1);
    const facet = (key, label, n) => h('button', { type: 'button', class: 'facet', 'aria-pressed': String(type === key), on: { click: () => { type = key; renderFacets(); render(); } } },
      label, h('span', { class: 'count', text: String(n) }));
    facets.replaceChildren(facet('', 'All', rows.length), ...[...counts].sort((a, b) => b[1] - a[1]).map(([k, n]) => facet(k, k, n)));
  }

  function render() {
    if (!rows) return;
    const shown = rows.filter((r) => (!type || cellText(r.type) === type) && rowMatches(r, query));
    p.setCount(shown.length === rows.length ? rows.length : shown.length + ' of ' + rows.length);
    host.show(dataTable({
      rows: shown,
      sort,
      empty: emptyState({ icon: 'search', title: 'No match' }),
      columns: [
        { label: 'Name', mono: true, sortBy: (r) => cellText(r.name), render: (r) => h('span', { class: 'cell-strong', text: cellText(r.name) }) },
        { label: 'Type', sortBy: (r) => cellText(r.type), render: (r) => tag(cellText(r.type)) },
        { label: 'Provided by', mono: true, key: 'ikey', sortBy: (r) => cellText(r.ikey) },
        { label: 'File', mono: true, sortBy: (r) => cellText(r.filename), render: (r) => h('span', { class: 'faint', title: cellText(r.filename), text: cellText(r.filename).split('/').pop() }) },
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
