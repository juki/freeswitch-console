// Config: configuration list + XML viewer, global variables, modules.

import { api, instPath, isAbort, rowsOf } from '../api.js';
import { h, tableBox, errorBox, cellText } from '../dom.js';

const SUBS = [
  { key: 'configuration', label: 'Configuration', mount: mountConfigurations },
  { key: 'vars', label: 'Global variables', mount: (ctx) => mountFiltered(ctx, 'vars', 'No global variables.', true) },
  { key: 'modules', label: 'Modules', mount: (ctx) => mountFiltered(ctx, 'modules', 'No modules.', false) },
];

export function mountConfig({ id, scope, root }) {
  const body = h('div');
  const buttons = new Map();
  let sub = null;
  const bar = h('div', { class: 'subtabs', role: 'group', 'aria-label': 'Config views' }, SUBS.map((s) => {
    const b = h('button', { type: 'button', class: 'subtab', text: s.label, on: { click: () => show(s.key) } });
    buttons.set(s.key, b);
    return b;
  }));
  root.append(bar, body);

  function show(key) {
    for (const [k, b] of buttons) b.setAttribute('aria-pressed', String(k === key));
    if (sub) sub.dispose();
    sub = scope.child();
    body.replaceChildren();
    SUBS.find((s) => s.key === key).mount({ id, scope: sub, root: body });
  }
  show('configuration');
}

function filterInput(label) {
  return h('input', { type: 'search', class: 'filter', placeholder: 'Filter', 'aria-label': label, spellcheck: false });
}

function mountFiltered({ id, scope, root }, path, empty, wrap) {
  const filter = filterInput('Filter rows');
  const count = h('span', { class: 'muted' });
  const box = tableBox();
  let rows = null;
  root.append(h('div', { class: 'toolbar' }, filter, count), box.el);

  function render() {
    if (!rows) return;
    const q = filter.value.trim().toLowerCase();
    const shown = q ? rows.filter((r) => r && typeof r === 'object' &&
      Object.values(r).some((v) => cellText(v).toLowerCase().includes(q))) : rows;
    count.textContent = shown.length + ' of ' + rows.length;
    box.rows(shown, { empty: q ? 'No match.' : empty, wrap });
  }
  filter.addEventListener('input', render);

  api('GET', instPath(id, path), { signal: scope.signal }).then((data) => {
    rows = rowsOf(data);
    render();
  }, (err) => {
    if (!isAbort(err)) box.error(err);
  });
}

function mountConfigurations({ id, scope, root }) {
  const filter = filterInput('Filter configurations');
  const list = h('ul', { class: 'conf-list' });
  const listMsg = h('div');
  const title = h('h3', { class: 'conf-title', text: 'Select a configuration' });
  const copyBtn = h('button', { type: 'button', class: 'small', text: 'Copy', disabled: true });
  const copyMsg = h('span', { class: 'muted', 'aria-live': 'polite' });
  const viewMsg = h('div');
  const pre = h('pre', { class: 'mono xml', tabIndex: 0 });
  pre.hidden = true;
  let items = [];
  let loadScope = null;

  root.append(h('div', { class: 'conf-layout' },
    h('div', { class: 'conf-side' }, filter, listMsg, list),
    h('div', { class: 'conf-main' }, h('div', { class: 'conf-head' }, title, copyBtn, copyMsg), viewMsg, pre)));

  filter.addEventListener('input', () => {
    const q = filter.value.trim().toLowerCase();
    let shown = 0;
    for (const it of items) {
      it.li.hidden = Boolean(q) && !it.key.includes(q);
      if (!it.li.hidden) shown++;
    }
    listMsg.replaceChildren(items.length && !shown ? h('p', { class: 'empty', text: 'No match.' }) : '');
  });

  async function open(name, button) {
    for (const it of items) {
      it.btn.classList.toggle('selected', it.btn === button);
      it.btn.setAttribute('aria-pressed', String(it.btn === button));
    }
    if (loadScope) loadScope.dispose();
    loadScope = scope.child();
    title.textContent = name;
    copyBtn.disabled = true;
    copyMsg.textContent = '';
    viewMsg.replaceChildren(h('p', { class: 'muted', text: 'Loading…' }));
    pre.hidden = true;
    try {
      const data = await api('GET', instPath(id, 'config/' + encodeURIComponent(name)), { signal: loadScope.signal });
      pre.textContent = data && typeof data.xml === 'string' ? data.xml : '';
      pre.hidden = false;
      viewMsg.replaceChildren();
      copyBtn.disabled = false;
    } catch (err) {
      if (!isAbort(err)) viewMsg.replaceChildren(errorBox(err));
    }
  }

  copyBtn.addEventListener('click', async () => {
    let ok = false;
    try {
      await navigator.clipboard.writeText(pre.textContent);
      ok = true;
    } catch {
      // Clipboard API needs a secure context; fall back to a selection copy.
      const range = document.createRange();
      range.selectNodeContents(pre);
      const sel = window.getSelection();
      sel.removeAllRanges();
      sel.addRange(range);
      try {
        ok = document.execCommand('copy');
      } catch {
        ok = false;
      }
    }
    copyMsg.textContent = ok ? 'Copied.' : 'Copy failed; the text is selected.';
    scope.later(() => {
      copyMsg.textContent = '';
    }, 2500);
  });

  api('GET', instPath(id, 'config'), { signal: scope.signal }).then((data) => {
    const rows = rowsOf(data).filter((r) => r && r.name);
    items = rows.map((r) => {
      const btn = h('button', { type: 'button', class: 'conf-item', 'aria-pressed': 'false' },
        h('span', { class: 'conf-name', text: String(r.name) }),
        r.description ? h('span', { class: 'conf-desc', text: String(r.description) }) : null);
      btn.addEventListener('click', () => open(String(r.name), btn));
      const li = h('li', null, btn);
      return { li, btn, key: (String(r.name) + ' ' + cellText(r.description)).toLowerCase() };
    });
    list.replaceChildren(...items.map((it) => it.li));
    listMsg.replaceChildren(items.length ? '' : h('p', { class: 'empty', text: 'No configurations.' }));
  }, (err) => {
    if (!isAbort(err)) listMsg.replaceChildren(errorBox(err));
  });
}
