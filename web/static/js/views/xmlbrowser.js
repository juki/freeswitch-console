// The two-pane XML browser behind Configuration, Dialplan and Directory: a
// filterable list on the left; on the right one item as highlighted XML with
// find-in-file, line wrap and copy, or, where a reader exists, a structured
// view (extensions, users) with the raw XML one click away.

import { api, instPath, isAbort, rowsOf } from '../api.js';
import { h, cellText, fmtInt } from '../dom.js';
import { plural, t } from '../i18n.js';
import { icon } from '../icons.js';
import { copyText, emptyState, errorState, searchBox, segmented, skeleton, toast } from '../ui.js';
import { renderXML } from '../xml.js';
import { readDialplan, readDirectory } from './readers.js';

// count renders "12 extensions" with the singular/plural pair picked for n.
function count(n, one, other) {
  const v = Number(n);
  return Number.isFinite(v) ? plural(v, one(fmtInt(v)), other(fmtInt(v))) : '';
}

const KINDS = {
  configuration: {
    list: 'config', item: 'config/', prefer: 'sofia.conf',
    sub: (r) => cellText(r.description),
    text: () => ({
      listFilter: t('xb.cfg.filter'), select: t('xb.cfg.select'), empty: t('xb.cfg.empty'), noMatch: t('xb.cfg.noMatch'),
      none: t('xb.cfg.none'), noneText: t('xb.cfg.noneText'), unreadable: '',
    }),
  },
  dialplan: {
    list: 'dialplan', item: 'dialplan/', prefer: 'default',
    sub: (r) => count(r.count, (n) => t('xb.dp.count.one', { n }), (n) => t('xb.dp.count.other', { n })),
    reader: readDialplan,
    text: () => ({
      listFilter: t('xb.dp.filter'), select: t('xb.dp.select'), empty: t('xb.dp.empty'), noMatch: t('xb.dp.noMatch'),
      none: t('xb.dp.none'), noneText: t('xb.dp.noneText'), unreadable: t('xb.dp.unreadable'),
      readerLabel: t('xb.dp.reader'), readerFind: t('xb.dp.readerFind'),
    }),
  },
  directory: {
    list: 'directory', item: 'directory/', prefer: '',
    sub: (r) => count(r.count, (n) => t('xb.dir.count.one', { n }), (n) => t('xb.dir.count.other', { n })),
    reader: readDirectory,
    text: () => ({
      listFilter: t('xb.dir.filter'), select: t('xb.dir.select'), empty: t('xb.dir.empty'), noMatch: t('xb.dir.noMatch'),
      none: t('xb.dir.none'), noneText: t('xb.dir.noneText'), unreadable: t('xb.dir.unreadable'),
      readerLabel: t('xb.dir.reader'), readerFind: t('xb.dir.readerFind'),
    }),
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
  const T = K.text();
  const list = h('div', { class: 'conf-list' });
  const title = h('div', { class: 'conf-title', text: T.select });
  const desc = h('div', { class: 'faint' });
  const copyBtn = h('button', { type: 'button', class: 'btn btn-sm', disabled: true, title: t('xb.copyTitle') }, icon('copy'), t('xb.copy'));
  const wrapBtn = h('button', { type: 'button', class: 'btn btn-sm btn-icon', 'aria-pressed': 'false', 'aria-label': t('xb.wrap'), title: t('xb.wrap') }, icon('wrap'));
  const findCount = h('span', { class: 'find-count', 'aria-live': 'polite' });
  const prevBtn = h('button', { type: 'button', class: 'btn btn-ghost btn-sm btn-icon', 'aria-label': t('xb.prev'), title: t('xb.prevTitle'), disabled: true }, icon('chevronUp'));
  const nextBtn = h('button', { type: 'button', class: 'btn btn-ghost btn-sm btn-icon', 'aria-label': t('xb.next'), title: t('xb.nextTitle'), disabled: true }, icon('chevronDown'));
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

  const search = searchBox(T.listFilter, (q) => {
    let shown = 0;
    for (const it of items) {
      it.btn.hidden = Boolean(q) && !it.key.includes(q);
      if (!it.btn.hidden) shown++;
    }
    noMatch.hidden = !items.length || shown > 0;
  });
  const noMatch = h('div', { class: 'note-empty', text: T.noMatch, hidden: true });
  const find = searchBox(t('xb.find'), (q) => {
    needle = q;
    runFind();
  }, 'sm');
  find.input.disabled = true;
  const modeSeg = K.reader ? segmented([{ key: 'read', label: T.readerLabel }, { key: 'xml', label: t('xb.xml') }], mode, (k) => {
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
    findCount.textContent = !needle ? '' : hits.length ? t('common.nOfM', { n: fmtInt(current + 1), m: fmtInt(hits.length) }) : t('common.noMatch');
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
      findCount.textContent = needle ? t('common.nOfM', { n: fmtInt(shown), m: fmtInt(total) }) : '';
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
    find.input.placeholder = read ? T.readerFind : t('xb.find');
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
          notice.replaceChildren(icon('alert'), T.unreadable);
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
    if (await copyText(raw)) toast(t('xb.copied', { name: title.textContent }));
    else toast(t('copy.denied'), 'error');
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
      list.replaceChildren(h('div', { class: 'note-empty', text: t('xb.nothing') }));
      view.replaceChildren(emptyState({ icon: 'sliders', title: T.none, text: T.noneText }));
      return;
    }
    open(items.find((it) => it.name === K.prefer) || items[0]);
  }, (err) => {
    if (isAbort(err)) return;
    list.replaceChildren();
    view.replaceChildren(errorState(err));
  });
  view.append(emptyState({ icon: 'sliders', title: T.select, text: T.empty }));
}
