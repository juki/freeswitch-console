// timeChart: a single-series time chart in SVG. Time axis with clock labels,
// a value axis with a few gridlines, an area under a line, a crosshair with a
// tooltip on hover, and an empty state. Geometry goes through SVG attributes
// only (the CSP forbids inline styles); colours come from style.css.
//
//   const c = timeChart({ scope, height: 240 });
//   c.setSeries({ label: 'Active sessions', format: (v) => v + ' sessions', integer: true, yMin: 0 });
//   c.setData(points, { from, to });   // points: [[epochMs, value], ...] sorted by time

import { h, s } from './dom.js';

const M = { l: 46, r: 16, t: 14, b: 28 };
const TIME_STEPS = [1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 10800, 21600, 43200, 86400].map((x) => x * 1000);
let uid = 0;

function pad2(n) {
  return String(n).padStart(2, '0');
}

// clock formats a timestamp as local 24-hour time.
export function clock(t, seconds) {
  const d = new Date(t);
  return pad2(d.getHours()) + ':' + pad2(d.getMinutes()) + (seconds ? ':' + pad2(d.getSeconds()) : '');
}

// niceStep returns a 1/2/2.5/5 x 10^n step that cuts span into about count parts.
function niceStep(span, count) {
  const raw = span / count;
  const p = 10 ** Math.floor(Math.log10(raw));
  const f = raw / p;
  return (f <= 1 ? 1 : f <= 2 ? 2 : f <= 2.5 ? 2.5 : f <= 5 ? 5 : 10) * p;
}

// decimate keeps the lowest and the highest point of every pixel column, in
// time order, so a dense series keeps its peaks without drawing every sample.
function decimate(points, x0, pxPerMs) {
  const out = [];
  let col = null;
  let lo = null;
  let hi = null;
  const flush = () => {
    if (!lo) return;
    if (lo === hi) out.push(lo);
    else if (lo[0] <= hi[0]) out.push(lo, hi);
    else out.push(hi, lo);
  };
  for (const p of points) {
    const c = Math.floor((p[0] - x0) * pxPerMs);
    if (c !== col) {
      flush();
      col = c;
      lo = p;
      hi = p;
    } else {
      if (p[1] < lo[1]) lo = p;
      if (p[1] > hi[1]) hi = p;
    }
  }
  flush();
  return out;
}

// nearest returns the index of the point closest in time to t.
function nearest(points, t) {
  let lo = 0;
  let hi = points.length - 1;
  while (hi - lo > 1) {
    const mid = (lo + hi) >> 1;
    if (points[mid][0] < t) lo = mid;
    else hi = mid;
  }
  return Math.abs(points[lo][0] - t) <= Math.abs(points[hi][0] - t) ? lo : hi;
}

export function timeChart({ scope, height = 240, gapMs = 45000, emptyText = 'Collecting samples…' }) {
  const id = 'tc' + (++uid);
  const clipRect = s('rect');
  const svg = s('svg', { height, role: 'img' },
    s('defs', null,
      s('linearGradient', { id: id + 'g', x1: 0, y1: 0, x2: 0, y2: 1 },
        s('stop', { offset: 0, class: 'chart-stop-0' }),
        s('stop', { offset: 1, class: 'chart-stop-1' })),
      s('clipPath', { id: id + 'c' }, clipRect)));
  const gGrid = s('g');
  const gPlot = s('g', { 'clip-path': `url(#${id}c)` });
  const gNow = s('g');
  const cross = s('line', { class: 'chart-cross' });
  const dot = s('circle', { class: 'chart-dot', r: 4.5 });
  const tipRect = s('rect', { rx: 7, ry: 7 });
  const tipV = s('text', { class: 'v', x: 10, y: 18 });
  const tipT = s('text', { class: 't', x: 10, y: 33 });
  const tip = s('g', { class: 'chart-tip' }, tipRect, tipV, tipT);
  const gHover = s('g', { visibility: 'hidden', 'pointer-events': 'none' }, cross, dot, tip);
  const hit = s('rect', { class: 'chart-hit' });
  svg.append(gGrid, gPlot, gNow, gHover, hit);
  const el = h('div', { class: 'chart' }, svg);

  let width = 0;
  let series = { label: '', format: String, tick: String, integer: false, yMin: 0, yMax: undefined };
  let data = [];
  let from = 0;
  let to = 1;
  let pointerX = null; // svg x of the pointer while hovering
  let visible = []; // points in the drawn window
  let sx = () => 0;
  let sy = () => 0;
  let frame = 0;

  const ro = new ResizeObserver(() => {
    const w = Math.round(el.clientWidth);
    if (w && w !== width) {
      width = w;
      render();
    }
  });
  ro.observe(el);
  scope.onCleanup(() => {
    ro.disconnect();
    cancelAnimationFrame(frame);
  });

  function schedule() {
    if (!frame) {
      frame = requestAnimationFrame(() => {
        frame = 0;
        render();
      });
    }
  }

  function render() {
    if (!width) return;
    const W = width;
    const H = height;
    const pw = Math.max(10, W - M.l - M.r);
    const ph = H - M.t - M.b;
    svg.setAttribute('width', W);
    svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
    clipRect.setAttribute('x', M.l);
    clipRect.setAttribute('y', 0);
    clipRect.setAttribute('width', pw + 1);
    clipRect.setAttribute('height', H - M.b + 1);
    hit.setAttribute('x', M.l);
    hit.setAttribute('y', M.t);
    hit.setAttribute('width', pw);
    hit.setAttribute('height', ph);

    // the window plus one point on each side, so the line enters and leaves the edges
    let a = data.findIndex((p) => p[0] >= from);
    if (a < 0) a = data.length;
    visible = data.slice(Math.max(0, a - 1));

    // value scale
    const vals = visible.map((p) => p[1]);
    let lo = series.yMin !== undefined ? series.yMin : vals.length ? Math.min(...vals) : 0;
    let hi = series.yMax !== undefined ? series.yMax : vals.length ? Math.max(...vals) : 1;
    if (hi - lo < 1e-9) hi = lo + 1;
    if (series.yMax === undefined) hi += (hi - lo) * 0.12;
    let step = niceStep(hi - lo, 4);
    if (series.integer) step = Math.max(1, Math.ceil(step));
    if (series.yMax === undefined) hi = Math.ceil(hi / step - 1e-9) * step;
    if (series.yMin === undefined) lo = Math.floor(lo / step + 1e-9) * step;
    sx = (t) => M.l + ((t - from) / (to - from)) * pw;
    sy = (v) => M.t + ph - ((v - lo) / (hi - lo)) * ph;

    const grid = [];
    for (let v = lo, i = 0; v <= hi + step * 1e-6 && i < 20; v += step, i++) {
      const y = Math.round(sy(v)) + 0.5;
      grid.push(s('line', { class: i ? 'chart-grid' : 'chart-base', x1: M.l, x2: W - M.r, y1: y, y2: y }));
      grid.push(s('text', { class: 'chart-tick y', x: M.l - 10, y: y + 4 }, series.tick(Number(v.toFixed(6)))));
    }

    // time axis, aligned to the local clock
    const span = to - from;
    const target = Math.max(2, Math.floor(pw / 96));
    const tStep = TIME_STEPS.find((x) => span / x <= target) || TIME_STEPS[TIME_STEPS.length - 1];
    const off = new Date(from).getTimezoneOffset() * 60000;
    for (let t = Math.ceil((from - off) / tStep) * tStep + off; t <= to; t += tStep) {
      const x = sx(t);
      if (x < M.l + 18 || x > W - M.r - 18) continue;
      grid.push(s('line', { class: 'chart-grid', x1: Math.round(x) + 0.5, x2: Math.round(x) + 0.5, y1: H - M.b, y2: H - M.b + 4 }));
      grid.push(s('text', { class: 'chart-tick x', x, y: H - 8 }, clock(t, tStep < 60000)));
    }
    gGrid.replaceChildren(...grid);

    // line and area, broken where samples are missing
    gPlot.replaceChildren();
    gNow.replaceChildren();
    if (visible.length < 2) {
      gPlot.append(s('text', { class: 'chart-empty', x: M.l + pw / 2, y: M.t + ph / 2 + 4 }, data.length ? 'No samples in this range yet' : emptyText));
      svg.setAttribute('aria-label', series.label + ': no data yet');
      gHover.setAttribute('visibility', 'hidden');
      return;
    }
    const base = sy(lo);
    let line = '';
    let area = '';
    let seg = [];
    const flushSeg = () => {
      if (seg.length > 1) {
        const d = seg.map(([x, y], i) => (i ? 'L' : 'M') + x.toFixed(1) + ' ' + y.toFixed(1)).join(' ');
        line += d + ' ';
        area += d + ` L${seg[seg.length - 1][0].toFixed(1)} ${base.toFixed(1)} L${seg[0][0].toFixed(1)} ${base.toFixed(1)} Z `;
      }
      seg = [];
    };
    const drawn = visible.length > pw * 2 ? decimate(visible, from, pw / (to - from)) : visible;
    for (let i = 0; i < drawn.length; i++) {
      if (i && drawn[i][0] - drawn[i - 1][0] > gapMs) flushSeg();
      seg.push([sx(drawn[i][0]), sy(drawn[i][1])]);
    }
    flushSeg();
    gPlot.append(s('path', { d: area, fill: `url(#${id}g)` }), s('path', { class: 'chart-line', d: line }));
    // Sparse series (a FreeSWITCH heartbeat every 20s) show their samples, so
    // a few points read as measurements rather than as a broken line.
    const inWindow = visible.filter((p) => p[0] >= from);
    if (inWindow.length <= Math.min(60, pw / 14)) {
      for (const p of inWindow) gPlot.append(s('circle', { class: 'chart-pt', cx: sx(p[0]).toFixed(1), cy: sy(p[1]).toFixed(1), r: 2.5 }));
    }
    const last = visible[visible.length - 1];
    if (last[0] >= from) gNow.append(s('circle', { class: 'chart-now', cx: sx(last[0]), cy: sy(last[1]), r: 3 }));
    svg.setAttribute('aria-label', series.label + ': ' + series.format(last[1]) + ' at ' + clock(last[0], true));
    paintHover();
  }

  function paintHover() {
    const inWindow = visible.filter((p) => p[0] >= from);
    if (pointerX === null || !inWindow.length) {
      gHover.setAttribute('visibility', 'hidden');
      return;
    }
    const t = from + ((pointerX - M.l) / Math.max(1, width - M.l - M.r)) * (to - from);
    const p = inWindow[nearest(inWindow, t)];
    const x = sx(p[0]);
    const y = sy(p[1]);
    cross.setAttribute('x1', x);
    cross.setAttribute('x2', x);
    cross.setAttribute('y1', M.t);
    cross.setAttribute('y2', height - M.b);
    dot.setAttribute('cx', x);
    dot.setAttribute('cy', y);
    tipV.textContent = series.format(p[1]);
    tipT.textContent = clock(p[0], true);
    gHover.setAttribute('visibility', 'visible');
    const bw = Math.ceil(Math.max(tipV.getComputedTextLength(), tipT.getComputedTextLength())) + 20;
    tipRect.setAttribute('width', bw);
    tipRect.setAttribute('height', 42);
    let tx = x + 12;
    if (tx + bw > width - 4) tx = x - 12 - bw;
    const ty = Math.min(Math.max(M.t, y - 52), height - M.b - 46);
    tip.setAttribute('transform', `translate(${tx.toFixed(1)} ${ty.toFixed(1)})`);
  }

  hit.addEventListener('pointermove', (ev) => {
    const r = svg.getBoundingClientRect();
    pointerX = ev.clientX - r.left;
    paintHover();
  });
  hit.addEventListener('pointerleave', () => {
    pointerX = null;
    paintHover();
  });

  return {
    el,
    setSeries(opts) {
      series = { label: '', format: String, integer: false, yMin: undefined, yMax: undefined, ...opts, tick: opts.tick || opts.format || String };
      schedule();
    },
    setData(points, range) {
      data = points;
      from = range.from;
      to = Math.max(range.to, range.from + 1);
      schedule();
    },
  };
}
