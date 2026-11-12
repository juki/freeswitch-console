// Overview: live load figures, a load chart, system facts and a summary of
// SIP and calls. /status is polled every 5s; heartbeats update in between.
//
// The chart combines two sources. /history is the server's ring of one sample
// per FreeSWITCH heartbeat (up to 2 hours). Live samples come from the status
// polls and heartbeat events this page receives; they are denser but only
// exist while the overview is open, and are kept per instance for the page
// lifetime so switching tabs keeps them. The rule:
//   - live samples win where they exist: a server sample that falls inside a
//     stretch of live samples (no gap over 45s) is dropped, because it is the
//     same heartbeat the page already saw, so nothing is counted twice;
//   - everywhere else the server samples fill in;
//   - server samples newer than the browser clock are dropped, and later live
//     samples are only appended, so time never runs backwards;
//   - if /history fails the chart is live-only.

import { api, instPath, isAbort, rowsOf } from '../api.js';
import { h, DASH, fmtInt, fmtPct, fmtSpan, fmtUptime } from '../dom.js';
import { plural, t, tx } from '../i18n.js';
import { icon } from '../icons.js';
import { clock, timeChart } from '../chart.js';
import { connectionHint, errorState, emptyState, kv, meter, panel, pill, segmented, skeleton, sparkline, stateTone } from '../ui.js';

const GW_DOWN = /^(FAIL|DOWN|TIMEOUT|EXPIRED|UNREGED)/i;
const KEEP_MS = 2 * 60 * 60 * 1000;
const KEEP_N = 20000;
const RUN_GAP = 45000;
const liveCache = new Map(); // instance id -> live samples [{t (ms), sessions, rate, idle}]

function trim(list, now) {
  let cut = 0;
  while (cut < list.length && (list.length - cut > KEEP_N || list[cut].t < now - KEEP_MS)) cut++;
  if (cut) list.splice(0, cut);
}

// combine merges server history with live samples (see the rule at the top).
function combine(server, live, now) {
  const runs = [];
  for (const x of live) {
    const r = runs[runs.length - 1];
    if (r && x.t - r[1] <= RUN_GAP) r[1] = x.t;
    else runs.push([x.t, x.t]);
  }
  // server times are whole seconds, hence the 1s of slack around each run
  const kept = server.filter((x) => x.t <= now && !runs.some(([a, b]) => x.t >= a - 1000 && x.t <= b + 1000));
  return kept.concat(live).sort((a, b) => a.t - b.t);
}

// Labels are functions so they follow the language at the time of use.
const METRICS = {
  sessions: {
    label: () => t('ov.m.sessions'), icon: 'activity', unit: () => '', integer: true, yMin: 0,
    format: (v) => plural(v, t('ov.fmt.sessions.one', { n: fmtInt(v) }), t('ov.fmt.sessions.other', { n: fmtInt(v) })), tick: (v) => fmtInt(v),
  },
  rate: {
    label: () => t('ov.m.rate'), icon: 'gauge', unit: () => t('ov.unit.perSec'), integer: true, yMin: 0,
    format: (v) => t('ov.fmt.rate', { n: fmtInt(v) }), tick: (v) => fmtInt(v),
  },
  idle: {
    label: () => t('ov.m.idle'), icon: 'cpu', unit: () => '%', yMin: 0, yMax: 100,
    format: (v) => t('ov.fmt.idle', { n: fmtPct(v) }), tick: (v) => fmtInt(v) + '%',
  },
  capacity: {
    label: () => t('ov.m.capacity'), icon: 'server', unit: () => '%', yMin: 0,
    format: (v) => t('ov.fmt.capacity', { n: fmtPct(v, v < 10 ? 1 : 0) }), tick: (v) => fmtPct(v, v % 1 ? 1 : 0) + '%',
  },
};

const RANGES = [
  { key: '5', label: () => t('ov.range.5'), title: () => t('ov.range.5.title'), ms: 5 * 60e3 },
  { key: '30', label: () => t('ov.range.30'), title: () => t('ov.range.30.title'), ms: 30 * 60e3 },
  { key: '120', label: () => t('ov.range.120'), title: () => t('ov.range.120.title'), ms: 120 * 60e3 },
];

function tileSkeleton() {
  return h('div', { class: 'stat' }, skeleton({ rows: 3, cols: 1 }));
}

export function mountOverview({ id, scope, root, setState }) {
  const base = '#/i/' + encodeURIComponent(id) + '/';
  const live = liveCache.get(id) || [];
  liveCache.set(id, live);
  let series = live.slice(); // what the chart draws: history + live, oldest first
  let historyState = 'loading'; // loading | ok | failed
  let metric = 'sessions';
  let range = RANGES[1];

  const notice = h('div', { class: 'stack' },
    h('div', { class: 'stat-grid' }, tileSkeleton(), tileSkeleton(), tileSkeleton(), tileSkeleton()),
    h('div', { class: 'panel' }, skeleton({ rows: 6, cols: 1 })));
  const content = h('div', { class: 'stack', hidden: true });
  root.append(notice, content);

  // ---- stat tiles: each one also picks the series of the chart ----
  const tiles = {};
  function tile(key, graph) {
    const m = METRICS[key];
    const value = h('span', { class: 'stat-value', text: DASH });
    const foot = h('span', { class: 'stat-foot', text: ' ' });
    const el = h('button', { type: 'button', class: 'stat', title: t('ov.tile.show', { metric: m.label() }), on: { click: () => pick(key) } },
      h('span', { class: 'stat-label' }, icon(m.icon), m.label()),
      h('span', { class: 'stat-main' }, value, m.unit() ? h('span', { class: 'stat-unit', text: m.unit() }) : null),
      foot,
      graph);
    tiles[key] = { el, value, foot };
    return el;
  }
  const sparks = { sessions: sparkline(), rate: sparkline(), idle: sparkline({ zero: false }) };
  const capMeter = meter();
  const grid = h('div', { class: 'stat-grid' },
    tile('sessions', h('span', { class: 'stat-graph' }, sparks.sessions.el)),
    tile('rate', h('span', { class: 'stat-graph' }, sparks.rate.el)),
    tile('idle', h('span', { class: 'stat-graph' }, sparks.idle.el)),
    tile('capacity', h('span', { class: 'stat-graph meter-slot' }, capMeter.el)));

  // ---- load chart ----
  const chart = timeChart({ scope, height: 260 });
  const since = h('span', { class: 'panel-note' });
  const rangeSeg = segmented(RANGES.map((r) => ({ key: r.key, label: r.label(), title: r.title() })), range.key, (k) => {
    range = RANGES.find((r) => r.key === k);
    draw();
  }, 'sm');
  const chartPanel = panel({ title: METRICS[metric].label(), actions: rangeSeg.el });
  chartPanel.el.classList.add('chart-panel');
  chartPanel.el.querySelector('.panel-count').after(since);
  chartPanel.body.append(chart.el);

  // ---- system and glance panels ----
  const system = panel({ title: t('ov.system') });
  const glance = panel({ title: t('ov.glance'), flush: true });
  function glanceItem(label, iconName, tab) {
    const value = h('div', { class: 'glance-value', text: DASH });
    const note = h('div', { class: 'glance-note' });
    const el = h('a', { class: 'glance-item', href: base + tab }, h('div', { class: 'glance-label' }, icon(iconName), label, icon('chevron', 'go')), value, note);
    return { el, value, note };
  }
  const gProfiles = glanceItem(t('ov.g.profiles'), 'radio', 'sip');
  const gGateways = glanceItem(t('ov.g.gateways'), 'plug', 'sip');
  const gRegs = glanceItem(t('ov.g.regs'), 'user', 'sip');
  const gCalls = glanceItem(t('ov.g.calls'), 'calls', 'calls');
  glance.body.append(h('div', { class: 'glance' }, gProfiles.el, gGateways.el, gRegs.el, gCalls.el));

  content.append(grid, chartPanel.el, h('div', { class: 'grid-2' }, system.el, glance.el));

  function pick(key) {
    metric = key;
    for (const [k, tl] of Object.entries(tiles)) tl.el.setAttribute('aria-pressed', String(k === key));
    chartPanel.setTitle(METRICS[key].label());
    chart.setSeries({ ...METRICS[key], label: METRICS[key].label() });
    draw();
  }

  function value(x, key) {
    if (key !== 'capacity') return x[key];
    return limit && Number.isFinite(x.sessions) ? (x.sessions / limit) * 100 : undefined;
  }

  function draw() {
    const now = Date.now();
    const points = [];
    for (const x of series) {
      const v = value(x, metric);
      if (Number.isFinite(v)) points.push([x.t, v]);
    }
    // The window grows with the data (at least two minutes) up to the chosen
    // range, so a short history fills the chart instead of hugging its edge.
    const first = points.length ? points[0][0] : now;
    const from = Math.max(now - range.ms, Math.min(first, now - 120e3));
    chart.setData(points, { from, to: now });
    since.textContent = caption(from, now);
  }

  // caption says what the chart shows: the span, and where history and live data start.
  function caption(from, now) {
    const parts = [t('ov.cap.last', { span: fmtSpan((now - from) / 1000) })];
    const firstLive = live.find((x) => x.t >= from);
    const firstServer = series.find((x) => x.t >= from && x.src === 'history');
    if (firstServer) parts.push(t('ov.cap.history', { time: clock(firstServer.t) }));
    if (firstLive) parts.push(t('ov.cap.live', { time: clock(firstLive.t, true) }));
    if (historyState === 'failed') parts.push(t('ov.cap.noHistory'));
    return parts.join(' · ');
  }

  function seedSparks() {
    for (const k of ['sessions', 'rate', 'idle']) sparks[k].set(series.slice(-60).map((x) => x[k]));
  }

  // record stores one live sample (any field may be missing) and redraws.
  function record(sample) {
    const now = Date.now();
    const x = { t: now, ...sample };
    live.push(x);
    trim(live, now);
    const last = series[series.length - 1];
    if (!last || now >= last.t) series.push(x);
    trim(series, now);
    draw();
  }

  seedSparks();

  api('GET', instPath(id, 'history'), { signal: scope.signal }).then((data) => {
    const server = (data && Array.isArray(data.samples) ? data.samples : [])
      .filter((x) => x && Number.isFinite(Number(x.t)))
      .map((x) => ({ t: Number(x.t) * 1000, sessions: Number(x.sessions), rate: Number(x.rate), idle: Number(x.idle), src: 'history' }));
    const now = Date.now();
    historyState = 'ok';
    series = combine(server, live, now);
    trim(series, now);
    seedSparks();
    draw();
  }, (err) => {
    if (isAbort(err)) return;
    historyState = 'failed';
    draw();
  });

  const b = (text) => h('b', { text });

  let limit = null;
  let connected = null; // null until the first answer

  function num(v) {
    const n = Number(v);
    return v === null || v === undefined || v === '' || !Number.isFinite(n) ? undefined : n;
  }

  function update({ sessions, rate, idle }) {
    const sample = {};
    if (sessions !== undefined) {
      sample.sessions = sessions;
      tiles.sessions.value.textContent = fmtInt(sessions);
      sparks.sessions.push(sessions);
      if (limit) {
        const f = sessions / limit;
        tiles.capacity.value.textContent = fmtPct(f * 100, f < 0.1 ? 1 : 0);
        capMeter.set(f, f > 0.9 ? 'bad' : f > 0.7 ? 'warn' : '');
        tiles.capacity.foot.replaceChildren(...tx('ov.foot.capacity', { n: b(fmtInt(sessions)), limit: b(fmtInt(limit)) }));
      }
    }
    if (rate !== undefined) {
      sample.rate = rate;
      tiles.rate.value.textContent = fmtInt(rate);
      sparks.rate.push(rate);
    }
    if (idle !== undefined) {
      sample.idle = idle;
      tiles.idle.value.textContent = fmtPct(idle);
      sparks.idle.push(idle);
    }
    if (Object.keys(sample).length) record(sample);
  }

  function showDisconnected(st) {
    content.hidden = true;
    notice.replaceChildren(h('div', { class: 'panel' }, emptyState({
      icon: 'plug', tone: 'calm', title: t('error.notConnected'),
      text: connectionHint(st && st.lastError) || t('ov.disc.text'),
      action: st && st.lastError ? h('code', { class: 'raw-error', text: st.lastError }) : null,
    })));
  }

  function apply(data) {
    if (data.state) setState(data.state);
    const st = data.status;
    connected = Boolean(st && typeof st === 'object');
    if (!connected) {
      showDisconnected(data.state);
      return;
    }
    notice.replaceChildren();
    content.hidden = false;
    const count = (st.sessions && st.sessions.count) || {};
    const r = (st.sessions && st.sessions.rate) || {};
    limit = Number(count.limit) || null;
    // idleCPU.allowed is the current idle percentage; idleCPU.used is the
    // configured minimum.
    update({ sessions: num(count.active), rate: num(r.current), idle: st.idleCPU ? num(st.idleCPU.allowed) : undefined });
    tiles.sessions.foot.replaceChildren(...tx('ov.foot.sessions', { peak: b(fmtInt(count.peak)), peak5: b(fmtInt(count.peak5Min)) }));
    tiles.rate.foot.replaceChildren(...tx('ov.foot.rate', { max: b(fmtInt(r.max)), peak: b(fmtInt(r.peak)) }));
    tiles.idle.foot.replaceChildren(...tx('ov.foot.idle', { min: b(fmtPct(st.idleCPU ? st.idleCPU.used : null, 0)) }));
    if (!limit) tiles.capacity.foot.textContent = t('ov.foot.noLimit');
    const status = String(st.systemStatus || '');
    system.body.replaceChildren(kv([
      [t('ov.sys.status'), status ? pill(status, stateTone(status) || (status === 'ready' ? 'ok' : 'warn')) : DASH],
      [t('ov.sys.hostname'), data.hostname || DASH],
      [t('ov.sys.version'), h('span', { class: 'mono', text: st.version || DASH })],
      [t('ov.sys.uptime'), fmtUptime(st.uptime)],
      [t('ov.sys.total'), h('span', { class: 'num', text: fmtInt(count.total) })],
      [t('ov.sys.limit'), h('span', { class: 'num', text: fmtInt(count.limit) })],
    ]));
  }

  pick(metric);

  const refresh = scope.poll(async (signal) => {
    try {
      const data = await api('GET', instPath(id, 'status'), { signal });
      if (data) apply(data);
    } catch (err) {
      if (isAbort(err)) throw err;
      content.hidden = true;
      notice.replaceChildren(h('div', { class: 'panel' }, errorState(err)));
    }
  }, 5000);

  // ---- SIP and calls summary, every 10s ----
  async function rows(path, signal) {
    return rowsOf(await api('GET', instPath(id, path), { signal }));
  }
  function fail(item, err) {
    item.value.textContent = DASH;
    item.note.replaceChildren(h('span', { class: 'bad', text: err.status === 503 ? t('ov.g.notConnected') : err.message }));
  }

  scope.poll(async (signal) => {
    await Promise.all([
      rows('sofia', signal).then((list) => {
        const profiles = list.filter((r) => r.type === 'profile');
        const running = profiles.filter((r) => /^RUNNING/i.test(r.state || '')).length;
        gProfiles.value.textContent = fmtInt(profiles.length);
        gProfiles.note.replaceChildren(running === profiles.length ? t('ov.g.allRunning') : h('span', { class: 'bad', text: t('ov.g.notRunning', { n: fmtInt(profiles.length - running) }) }));
        const gws = list.filter((r) => r.type === 'gateway');
        const down = gws.filter((r) => GW_DOWN.test(r.state || '')).length;
        gGateways.value.textContent = fmtInt(gws.length);
        gGateways.note.replaceChildren(!gws.length ? t('ov.g.noGateways') : down ? h('span', { class: 'bad', text: t('ov.g.gwDown', { n: fmtInt(down) }) }) : t('ov.g.allUp'));
      }, (err) => {
        if (isAbort(err)) throw err;
        fail(gProfiles, err);
        fail(gGateways, err);
      }),
      rows('registrations', signal).then((list) => {
        gRegs.value.textContent = fmtInt(list.length);
        gRegs.note.textContent = plural(list.length, t('ov.g.regsNote.one'), t('ov.g.regsNote.other'));
      }, (err) => {
        if (isAbort(err)) throw err;
        fail(gRegs, err);
      }),
      Promise.all([rows('channels', signal), rows('calls', signal)]).then(([channels, calls]) => {
        gCalls.value.textContent = fmtInt(calls.filter((c) => c.b_uuid).length);
        gCalls.note.textContent = plural(channels.length, t('ov.g.callsNote.one', { n: fmtInt(channels.length) }), t('ov.g.callsNote.other', { n: fmtInt(channels.length) }));
      }, (err) => {
        if (isAbort(err)) throw err;
        fail(gCalls, err);
      }),
    ]);
  }, 10000);

  scope.stream('api/' + instPath(id, 'stream'), {
    state: (st) => {
      setState(st);
      if (!st.connected) showDisconnected(st);
      else if (connected === false) refresh(); // reconnected: reload now
      connected = Boolean(st.connected);
    },
    heartbeat: (hb) => {
      if (!connected) return;
      // Session-Per-Sec is the remaining per-second budget; the last-second
      // rate (status "rate.current") is Session-Per-Sec-Last.
      update({ sessions: num(hb['Session-Count']), rate: num(hb['Session-Per-Sec-Last']), idle: num(hb['Idle-CPU']) });
    },
  });
}
