// Line diff for the config editor's review and history views.
//
// diffLines strips the common head and tail, then runs Myers' O(ND)
// algorithm on what is left. Past MAX_D edits (a rewritten file) it stops
// searching and reports the remaining middle as removed-then-added, which is
// still a correct diff, only not a minimal one. renderDiff shows the result
// with old/new line numbers, three lines of context around each change and
// the unchanged stretches folded. Line text only ever goes into textContent.

import { h, fmtInt } from './dom.js';
import { plural, t } from './i18n.js';

const MAX_D = 1500;

// myers returns the edit script for a -> b as ops {type: 'eq'|'del'|'add', a, b}
// (indexes into a and b), or null when more than MAX_D edits are needed.
function myers(a, b) {
  const n = a.length;
  const m = b.length;
  const max = n + m;
  const off = max;
  let v = new Int32Array(2 * max + 2);
  const trace = [];
  for (let d = 0; d <= Math.min(max, MAX_D); d++) {
    trace.push(v.slice(off - d, off + d + 1));
    for (let k = -d; k <= d; k += 2) {
      let x = k === -d || (k !== d && v[off + k - 1] < v[off + k + 1]) ? v[off + k + 1] : v[off + k - 1] + 1;
      let y = x - k;
      while (x < n && y < m && a[x] === b[y]) {
        x++;
        y++;
      }
      v[off + k] = x;
      if (x >= n && y >= m) return backtrack(trace, a, b, d, off);
    }
  }
  return null;

  function backtrack(tr, aa, bb, dEnd) {
    const ops = [];
    let x = aa.length;
    let y = bb.length;
    for (let d = dEnd; d > 0; d--) {
      const vd = tr[d]; // v as it was before step d, indexes -d..d shifted
      const at = (k) => vd[k + d];
      const k = x - y;
      const prevK = k === -d || (k !== d && at(k - 1) < at(k + 1)) ? k + 1 : k - 1;
      const prevX = at(prevK);
      const prevY = prevX - prevK;
      while (x > prevX && y > prevY) ops.push({ type: 'eq', a: --x, b: --y });
      if (x === prevX) ops.push({ type: 'add', a: x, b: --y });
      else ops.push({ type: 'del', a: --x, b: y });
    }
    while (x > 0 && y > 0) ops.push({ type: 'eq', a: --x, b: --y });
    return ops.reverse();
  }
}

// diffLines compares two texts line by line.
export function diffLines(before, after) {
  const a = before === '' ? [] : before.split('\n');
  const b = after === '' ? [] : after.split('\n');
  let head = 0;
  while (head < a.length && head < b.length && a[head] === b[head]) head++;
  let tail = 0;
  while (tail < a.length - head && tail < b.length - head && a[a.length - 1 - tail] === b[b.length - 1 - tail]) tail++;
  const ops = [];
  for (let i = 0; i < head; i++) ops.push({ type: 'eq', a: i, b: i });
  const midA = a.slice(head, a.length - tail);
  const midB = b.slice(head, b.length - tail);
  const mid = myers(midA, midB);
  if (mid) {
    for (const o of mid) ops.push({ type: o.type, a: o.a + head, b: o.b + head });
  } else {
    midA.forEach((_, i) => ops.push({ type: 'del', a: head + i, b: head }));
    midB.forEach((_, i) => ops.push({ type: 'add', a: head + midA.length, b: head + i }));
  }
  for (let i = 0; i < tail; i++) ops.push({ type: 'eq', a: a.length - tail + i, b: b.length - tail + i });
  let added = 0;
  let removed = 0;
  for (const o of ops) {
    if (o.type === 'add') added++;
    else if (o.type === 'del') removed++;
  }
  return { ops, a, b, added, removed };
}

// renderDiff draws a diff with `context` unchanged lines around changes.
export function renderDiff(diff, context = 3) {
  const { ops, a, b } = diff;
  const keep = new Uint8Array(ops.length);
  ops.forEach((o, i) => {
    if (o.type === 'eq') return;
    for (let j = Math.max(0, i - context); j <= Math.min(ops.length - 1, i + context); j++) keep[j] = 1;
  });
  const rows = [];
  let skipped = 0;
  const flushSkip = () => {
    if (!skipped) return;
    rows.push(h('div', { class: 'df-fold' }, h('span', { text: plural(skipped, t('diff.folded.one', { n: fmtInt(skipped) }), t('diff.folded.other', { n: fmtInt(skipped) })) })));
    skipped = 0;
  };
  ops.forEach((o, i) => {
    if (!keep[i]) {
      skipped++;
      return;
    }
    flushSkip();
    const text = o.type === 'add' ? b[o.b] : a[o.a];
    rows.push(h('div', { class: 'df-row df-' + o.type },
      h('span', { class: 'df-no', text: o.type === 'add' ? '' : String(o.a + 1) }),
      h('span', { class: 'df-no', text: o.type === 'del' ? '' : String(o.b + 1) }),
      h('span', { class: 'df-sign', text: o.type === 'add' ? '+' : o.type === 'del' ? '−' : '' }),
      h('span', { class: 'df-text', text })));
  });
  flushSkip();
  if (!diff.added && !diff.removed) rows.push(h('div', { class: 'df-same', text: t('diff.same') }));
  return h('div', { class: 'df', role: 'table' }, rows);
}

// diffSummary is the "+12 −3" pair shown above a diff.
export function diffSummary(diff) {
  return h('span', { class: 'df-sum' },
    h('span', { class: 'df-plus', text: '+' + fmtInt(diff.added) }),
    h('span', { class: 'df-minus', text: '−' + fmtInt(diff.removed) }));
}
