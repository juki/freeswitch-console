// Configuration: effective config with an XML viewer, global variables, modules.

import { api, instPath, isAbort, rowsOf } from '../api.js';
import { h, cellText } from '../dom.js';
import { icon } from '../icons.js';
import { copyText, dataTable, emptyState, errorState, panel, rowMatches, searchBox, segmented, skeleton, sortState, tableHost, tag, toast } from '../ui.js';
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

// Find in file uses the CSS Custom Highlight API where the browser has it to
// mark the matched text; otherwise (and in addition) matching lines are tinted.
const HL = typeof CSS !== 'undefined' && CSS.highlights && typeof Highlight === 'function';

// rangesIn returns a Range for every occurrence of q (lower case) in el's text.
function rangesIn(el, q) {
  const nodes = [];
  const walker = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
  let text = '';
  for (let n = walker.nextNode(); n; n = walker.nextNode()) {
    nodes.push([n, text.length]);
    text += n.data;
  }
  // at maps a text offset to [node, offset]; an end offset may sit at a node's end
  const at = (pos) => {
    let i = nodes.length - 1;
    while (i > 0 && nodes[i][1] >= pos) i--;
    return [nodes[i][0], pos - nodes[i][1]];
  };
  const out = [];
  const lower = text.toLowerCase();
  for (let i = lower.indexOf(q); i >= 0 && nodes.length; i = lower.indexOf(q, i + q.length)) {
    const r = new Range();
    r.setStart(...at(i));
    r.setEnd(...at(i + q.length));
    out.push(r);
  }
  return out;
}

function mountConfigurations({ id, scope, root }) {
  const list = h('div', { class: 'conf-list' });
  const title = h('div', { class: 'conf-title', text: 'Select a configuration' });
  const desc = h('div', { class: 'faint' });
  const copyBtn = h('button', { type: 'button', class: 'btn btn-sm', disabled: true }, icon('copy'), 'Copy');
  const wrapBtn = h('button', { type: 'button', class: 'btn btn-sm btn-icon', 'aria-pressed': 'false', 'aria-label': 'Wrap long lines', title: 'Wrap long lines' }, icon('wrap'));
  const findCount = h('span', { class: 'find-count', 'aria-live': 'polite' });
  const prevBtn = h('button', { type: 'button', class: 'btn btn-ghost btn-sm btn-icon', 'aria-label': 'Previous match', title: 'Previous match (Shift+Enter)', disabled: true }, icon('chevronUp'));
  const nextBtn = h('button', { type: 'button', class: 'btn btn-ghost btn-sm btn-icon', 'aria-label': 'Next match', title: 'Next match (Enter)', disabled: true }, icon('chevronDown'));
  const view = h('div', { class: 'conf-view' });
  let items = [];
  let raw = '';
  let loadScope = null;
  let pre = null;
  let wrap = false;
  let needle = '';
  let hits = []; // matching .xl lines
  let current = -1;

  const search = searchBox('Filter configurations', (q) => {
    let shown = 0;
    for (const it of items) {
      it.btn.hidden = Boolean(q) && !it.key.includes(q);
      if (!it.btn.hidden) shown++;
    }
    noMatch.hidden = !items.length || shown > 0;
  });
  const noMatch = h('div', { class: 'note-empty', text: 'No configuration matches.', hidden: true });
  const find = searchBox('Find in file', (q) => {
    needle = q;
    runFind();
  }, 'sm');
  find.input.disabled = true;

  const p = h('section', { class: 'panel' },
    h('div', { class: 'conf' },
      h('div', { class: 'conf-side' }, h('div', { class: 'conf-search' }, search.el), list),
      h('div', { class: 'conf-main' },
        h('div', { class: 'conf-head' }, h('div', { class: 'spacer' }, title, desc), find.el, findCount, prevBtn, nextBtn, wrapBtn, copyBtn),
        view)));
  root.append(p);
  list.append(skeleton({ rows: 10, cols: 1 }));

  function clearHighlights() {
    if (HL) {
      CSS.highlights.delete('fsdeck-find');
      CSS.highlights.delete('fsdeck-find-current');
    }
  }
  scope.onCleanup(clearHighlights);

  function showCurrent() {
    hits.forEach((el, i) => el.classList.toggle('hit-current', i === current));
    findCount.textContent = !needle ? '' : hits.length ? current + 1 + ' of ' + hits.length : 'No match';
    prevBtn.disabled = nextBtn.disabled = hits.length < 2;
    if (HL) {
      CSS.highlights.delete('fsdeck-find-current');
      if (current >= 0) CSS.highlights.set('fsdeck-find-current', new Highlight(...rangesIn(hits[current], needle)));
    }
    const el = hits[current];
    if (el) view.scrollTop = Math.max(0, el.offsetTop - view.clientHeight / 2);
  }

  function runFind() {
    for (const el of hits) el.classList.remove('hit', 'hit-current');
    clearHighlights();
    hits = [];
    current = -1;
    if (pre && needle) {
      hits = [...pre.children].filter((el) => el.textContent.toLowerCase().includes(needle));
      for (const el of hits) el.classList.add('hit');
      if (HL && hits.length) CSS.highlights.set('fsdeck-find', new Highlight(...hits.flatMap((el) => rangesIn(el, needle))));
      current = hits.length ? 0 : -1;
    }
    showCurrent();
  }

  function step(d) {
    if (!hits.length) return;
    current = (current + d + hits.length) % hits.length;
    showCurrent();
  }
  prevBtn.addEventListener('click', () => step(-1));
  nextBtn.addEventListener('click', () => step(1));
  find.input.addEventListener('keydown', (ev) => {
    if (ev.key === 'Enter') {
      ev.preventDefault();
      step(ev.shiftKey ? -1 : 1);
    }
  });
  wrapBtn.addEventListener('click', () => {
    wrap = !wrap;
    wrapBtn.setAttribute('aria-pressed', String(wrap));
    if (pre) pre.classList.toggle('wrap', wrap);
    showCurrent();
  });

  async function open(item) {
    for (const it of items) it.btn.setAttribute('aria-pressed', String(it === item));
    if (loadScope) loadScope.dispose();
    loadScope = scope.child();
    title.textContent = item.name;
    desc.textContent = item.description;
    copyBtn.disabled = true;
    find.input.disabled = true;
    pre = null;
    runFind();
    view.replaceChildren(skeleton({ rows: 14, cols: 1 }));
    try {
      const data = await api('GET', instPath(id, 'config/' + encodeURIComponent(item.name)), { signal: loadScope.signal });
      raw = data && typeof data.xml === 'string' ? data.xml : '';
      pre = renderXML(raw);
      pre.classList.toggle('wrap', wrap);
      pre.classList.toggle('has-ranges', HL);
      view.replaceChildren(pre);
      view.scrollTop = 0;
      copyBtn.disabled = false;
      find.input.disabled = false;
      runFind();
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
  search.el.classList.add('sm');
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
