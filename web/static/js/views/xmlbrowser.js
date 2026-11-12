// The two-pane XML browser behind Configuration, Dialplan and Directory: a
// filterable list on the left; on the right one item as highlighted XML with
// find-in-file, line wrap and copy, or, where a reader exists, a structured
// view (extensions, users) with the raw XML one click away.

import { api, instPath, isAbort, rowsOf } from '../api.js';
import { h, cellText } from '../dom.js';
import { icon } from '../icons.js';
import { copyText, emptyState, errorState, searchBox, segmented, skeleton, toast } from '../ui.js';
import { renderXML } from '../xml.js';
import { readDialplan, readDirectory } from './readers.js';

function count(n, one, many) {
  const v = Number(n);
  return Number.isFinite(v) ? v.toLocaleString('en-US') + ' ' + (v === 1 ? one : many) : '';
}

const KINDS = {
  configuration: {
    list: 'config', item: 'config/', listFilter: 'Filter configurations', prefer: 'sofia.conf', noun: 'configuration',
    sub: (r) => cellText(r.description),
    empty: 'Pick a file on the left to see it as FreeSWITCH parsed it.',
  },
  dialplan: {
    list: 'dialplan', item: 'dialplan/', listFilter: 'Filter contexts', prefer: 'default', noun: 'context',
    sub: (r) => count(r.count, 'extension', 'extensions'),
    empty: 'Pick a dialplan context on the left.',
    reader: readDialplan, readerLabel: 'Extensions', readerFind: 'Filter extensions',
  },
  directory: {
    list: 'directory', item: 'directory/', listFilter: 'Filter domains', prefer: '', noun: 'domain',
    sub: (r) => count(r.count, 'user entry', 'user entries'),
    empty: 'Pick a directory domain on the left.',
    reader: readDirectory, readerLabel: 'Users', readerFind: 'Filter users',
  },
};

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

export function mountXmlBrowser({ id, scope, root, kind }) {
  const K = KINDS[kind];
  const list = h('div', { class: 'conf-list' });
  const title = h('div', { class: 'conf-title', text: 'Select a ' + K.noun });
  const desc = h('div', { class: 'faint' });
  const copyBtn = h('button', { type: 'button', class: 'btn btn-sm', disabled: true, title: 'Copy the XML' }, icon('copy'), 'Copy');
  const wrapBtn = h('button', { type: 'button', class: 'btn btn-sm btn-icon', 'aria-pressed': 'false', 'aria-label': 'Wrap long lines', title: 'Wrap long lines' }, icon('wrap'));
  const findCount = h('span', { class: 'find-count', 'aria-live': 'polite' });
  const prevBtn = h('button', { type: 'button', class: 'btn btn-ghost btn-sm btn-icon', 'aria-label': 'Previous match', title: 'Previous match (Shift+Enter)', disabled: true }, icon('chevronUp'));
  const nextBtn = h('button', { type: 'button', class: 'btn btn-ghost btn-sm btn-icon', 'aria-label': 'Next match', title: 'Next match (Enter)', disabled: true }, icon('chevronDown'));
  const notice = h('div', { class: 'reader-fallback', hidden: true });
  const view = h('div', { class: 'conf-view' });
  let items = [];
  let raw = '';
  let loadScope = null;
  let pre = null; // the raw XML view of the open item
  let reading = null; // the structured view of the open item, if any
  let mode = K.reader ? 'read' : 'xml';
  let wrap = false;
  let needle = '';
  let hits = []; // matching .xl lines
  let current = -1;

  const search = searchBox(K.listFilter, (q) => {
    let shown = 0;
    for (const it of items) {
      it.btn.hidden = Boolean(q) && !it.key.includes(q);
      if (!it.btn.hidden) shown++;
    }
    noMatch.hidden = !items.length || shown > 0;
  });
  const noMatch = h('div', { class: 'note-empty', text: 'No ' + K.noun + ' matches.', hidden: true });
  const find = searchBox('Find in file', (q) => {
    needle = q;
    runFind();
  }, 'sm');
  find.input.disabled = true;
  const modeSeg = K.reader ? segmented([{ key: 'read', label: K.readerLabel }, { key: 'xml', label: 'XML' }], mode, (k) => {
    mode = k;
    showMode();
  }, 'sm') : null;
  if (modeSeg) modeSeg.el.hidden = true;

  root.append(h('section', { class: 'panel' },
    h('div', { class: 'conf' },
      h('div', { class: 'conf-side' }, h('div', { class: 'conf-search' }, search.el), list),
      h('div', { class: 'conf-main' },
        h('div', { class: 'conf-head' }, h('div', { class: 'spacer' }, title, desc), modeSeg ? modeSeg.el : null, find.el, findCount, prevBtn, nextBtn, wrapBtn, copyBtn),
        notice,
        view))));
  list.append(skeleton({ rows: 10, cols: 1 }));

  function clearHighlights() {
    if (HL) {
      CSS.highlights.delete('fsdeck-find');
      CSS.highlights.delete('fsdeck-find-current');
    }
  }
  scope.onCleanup(clearHighlights);

  const structured = () => mode === 'read' && reading;

  function showCurrent() {
    if (structured()) return;
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
    if (structured()) {
      const [shown, total] = reading.filter(needle);
      findCount.textContent = needle ? shown + ' of ' + total : '';
      return;
    }
    if (pre && needle) {
      hits = [...pre.children].filter((el) => el.textContent.toLowerCase().includes(needle));
      for (const el of hits) el.classList.add('hit');
      if (HL && hits.length) CSS.highlights.set('fsdeck-find', new Highlight(...hits.flatMap((el) => rangesIn(el, needle))));
      current = hits.length ? 0 : -1;
    }
    showCurrent();
  }

  // showMode swaps between the structured view and the raw XML of the open item.
  function showMode() {
    const read = structured();
    prevBtn.hidden = nextBtn.hidden = wrapBtn.hidden = Boolean(read);
    find.input.placeholder = read ? K.readerFind : 'Find in file';
    find.input.setAttribute('aria-label', find.input.placeholder);
    if (!pre && !read) return;
    view.replaceChildren(read ? reading.el : pre);
    view.scrollTop = 0;
    runFind();
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
    notice.hidden = true;
    pre = null;
    reading = null;
    runFind();
    view.replaceChildren(skeleton({ rows: 14, cols: 1 }));
    try {
      const data = await api('GET', instPath(id, K.item + encodeURIComponent(item.name)), { signal: loadScope.signal });
      raw = data && typeof data.xml === 'string' ? data.xml : '';
      pre = renderXML(raw);
      pre.classList.toggle('wrap', wrap);
      pre.classList.toggle('has-ranges', HL);
      if (K.reader) {
        reading = K.reader(raw);
        if (!reading) {
          notice.replaceChildren(icon('alert'), 'This XML could not be read as a ' + K.noun + ', so it is shown as is.');
          notice.hidden = false;
        }
      }
      if (modeSeg) modeSeg.el.hidden = !reading;
      copyBtn.disabled = false;
      find.input.disabled = false;
      showMode();
    } catch (err) {
      if (!isAbort(err)) view.replaceChildren(errorState(err));
    }
  }

  copyBtn.addEventListener('click', async () => {
    if (await copyText(raw)) toast('Copied ' + title.textContent + ' to the clipboard.');
    else toast('The browser did not allow copying.', 'error');
  });

  api('GET', instPath(id, K.list), { signal: scope.signal }).then((data) => {
    items = rowsOf(data).filter((r) => r && r.name).map((r) => {
      const item = { name: String(r.name), description: K.sub(r) };
      item.key = (item.name + ' ' + item.description).toLowerCase();
      item.btn = h('button', { type: 'button', class: 'conf-item', 'aria-pressed': 'false', on: { click: () => open(item) } },
        h('span', { class: 'conf-name', text: item.name }),
        item.description ? h('span', { class: 'conf-desc', text: item.description }) : null);
      return item;
    });
    list.replaceChildren(...items.map((it) => it.btn), noMatch);
    if (!items.length) {
      list.replaceChildren(h('div', { class: 'note-empty', text: 'Nothing here.' }));
      view.replaceChildren(emptyState({ icon: 'sliders', title: 'No ' + K.noun + 's', text: 'FreeSWITCH reports no ' + K.noun + 's in this section.' }));
      return;
    }
    open(items.find((it) => it.name === K.prefer) || items[0]);
  }, (err) => {
    if (isAbort(err)) return;
    list.replaceChildren();
    view.replaceChildren(errorState(err));
  });
  view.append(emptyState({ icon: 'sliders', title: 'Select a ' + K.noun, text: K.empty }));
}
