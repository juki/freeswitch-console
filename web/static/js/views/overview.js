// Overview: live load figures, a load chart, system facts and a summary of
// SIP and calls. /status is polled every 5s; heartbeats update in between.
// The chart is fed from those same samples, kept per instance for 30 minutes
// while the page is open, so switching tabs does not lose the curve.

import { api, instPath, isAbort, rowsOf } from '../api.js';
import { h, DASH, fmtInt, fmtPct, fmtUptime } from '../dom.js';
import { icon } from '../icons.js';
import { clock, timeChart } from '../chart.js';
import { connectionHint, errorState, emptyState, kv, meter, panel, pill, segmented, skeleton, sparkline, stateTone } from '../ui.js';

const GW_DOWN = /^(FAIL|DOWN|TIMEOUT|EXPIRED|UNREGED)/i;
const KEEP_MS = 30 * 60 * 1000;
const KEEP_N = 5000;
const history = new Map(); // instance id -> [{t, sessions, rate, idle, capacity}]

const METRICS = {
  sessions: {
    label: 'Active sessions', icon: 'activity', unit: '', integer: true, yMin: 0,
    format: (v) => fmtInt(v) + (v === 1 ? ' session' : ' sessions'), tick: (v) => fmtInt(v),
  },
  rate: {
    label: 'Session rate', icon: 'gauge', unit: '/s', integer: true, yMin: 0,
    format: (v) => fmtInt(v) + ' new / s', tick: (v) => fmtInt(v),
  },
  idle: {
    label: 'Idle CPU', icon: 'cpu', unit: '%', yMin: 0, yMax: 100,
    format: (v) => fmtPct(v) + '% idle', tick: (v) => v + '%',
  },
  capacity: {
    label: 'Capacity used', icon: 'server', unit: '%', yMin: 0,
    format: (v) => fmtPct(v, v < 10 ? 1 : 0) + '% of the session limit', tick: (v) => v + '%',
  },
};

const RANGES = [
  { key: '1', label: '1m', ms: 60e3 },
  { key: '5', label: '5m', ms: 5 * 60e3 },
  { key: '15', label: '15m', ms: 15 * 60e3 },
  { key: '30', label: '30m', ms: 30 * 60e3 },
];

function tileSkeleton() {
  return h('div', { class: 'stat' }, skeleton({ rows: 3, cols: 1 }));
}

export function mountOverview({ id, scope, root, setState }) {
  const base = '#/i/' + encodeURIComponent(id) + '/';
  const samples = history.get(id) || [];
  history.set(id, samples);
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
    const el = h('button', { type: 'button', class: 'stat', title: 'Show ' + m.label.toLowerCase() + ' in the chart', on: { click: () => pick(key) } },
      h('span', { class: 'stat-label' }, icon(m.icon), m.label),
      h('span', { class: 'stat-main' }, value, m.unit ? h('span', { class: 'stat-unit', text: m.unit }) : null),
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
  const rangeSeg = segmented(RANGES.map((r) => ({ key: r.key, label: r.label, title: 'Last ' + r.label.replace('m', ' minutes') })), range.key, (k) => {
    range = RANGES.find((r) => r.key === k);
    draw();
  }, 'sm');
  const chartPanel = panel({ title: METRICS[metric].label, actions: rangeSeg.el });
  chartPanel.el.classList.add('chart-panel');
  chartPanel.el.querySelector('.panel-count').after(since);
  chartPanel.body.append(chart.el);

  // ---- system and glance panels ----
  const system = panel({ title: 'System' });
  const glance = panel({ title: 'SIP and calls', flush: true });
  function glanceItem(label, iconName, tab) {
    const value = h('div', { class: 'glance-value', text: DASH });
    const note = h('div', { class: 'glance-note' });
    const el = h('a', { class: 'glance-item', href: base + tab }, h('div', { class: 'glance-label' }, icon(iconName), label, icon('chevron', 'go')), value, note);
    return { el, value, note };
  }
  const gProfiles = glanceItem('SIP profiles', 'radio', 'sip');
  const gGateways = glanceItem('Gateways', 'plug', 'sip');
  const gRegs = glanceItem('Registrations', 'user', 'sip');
  const gCalls = glanceItem('Calls', 'calls', 'calls');
  glance.body.append(h('div', { class: 'glance' }, gProfiles.el, gGateways.el, gRegs.el, gCalls.el));

  content.append(grid, chartPanel.el, h('div', { class: 'grid-2' }, system.el, glance.el));

  function pick(key) {
    metric = key;
    for (const [k, t] of Object.entries(tiles)) t.el.setAttribute('aria-pressed', String(k === key));
    chartPanel.setTitle(METRICS[key].label);
    chart.setSeries(METRICS[key]);
    draw();
  }

  function draw() {
    const now = Date.now();
    const points = samples.filter((x) => Number.isFinite(x[metric])).map((x) => [x.t, x[metric]]);
    // The window grows with the history (at least a minute) up to the chosen range.
    const first = points.length ? points[0][0] : now;
    chart.setData(points, { from: Math.max(now - range.ms, Math.min(first, now - 60e3)), to: now });
    since.textContent = samples.length ? 'live since ' + clock(samples[0].t) : 'live';
  }

  // record stores one sample (any field may be missing) and redraws.
  function record(sample) {
    const t = Date.now();
    samples.push({ t, ...sample });
    while (samples.length > KEEP_N || (samples.length && samples[0].t < t - KEEP_MS)) samples.shift();
    draw();
  }

  // Sparklines pick up where the cached history left off.
  for (const x of samples.slice(-60)) {
    for (const k of ['sessions', 'rate', 'idle']) sparks[k].push(x[k]);
  }

  const foot = (el, parts) => el.replaceChildren(...parts.flatMap(([label, value], i) => [i ? ' · ' : '', label + ' ', h('b', { text: value })]));

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
        sample.capacity = f * 100;
        tiles.capacity.value.textContent = fmtPct(f * 100, f < 0.1 ? 1 : 0);
        capMeter.set(f, f > 0.9 ? 'bad' : f > 0.7 ? 'warn' : '');
        tiles.capacity.foot.replaceChildren(h('b', { text: fmtInt(sessions) }), ' of ', h('b', { text: fmtInt(limit) }), ' sessions');
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
      icon: 'plug', tone: 'calm', title: 'Not connected',
      text: (connectionHint(st && st.lastError) || 'fsdeck has no Event Socket connection to this FreeSWITCH.') + (st && st.lastError ? ' (' + st.lastError + ')' : ''),
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
    foot(tiles.sessions.foot, [['Peak', fmtInt(count.peak)], ['5 min peak', fmtInt(count.peak5Min)]]);
    foot(tiles.rate.foot, [['Limit', fmtInt(r.max) + '/s'], ['Peak', fmtInt(r.peak) + '/s']]);
    foot(tiles.idle.foot, [['Minimum allowed', fmtPct(st.idleCPU ? st.idleCPU.used : null, 0) + '%']]);
    if (!limit) tiles.capacity.foot.textContent = 'No session limit reported';
    const status = String(st.systemStatus || '');
    system.body.replaceChildren(kv([
      ['Status', status ? pill(status, stateTone(status) || (status === 'ready' ? 'ok' : 'warn')) : DASH],
      ['Hostname', data.hostname || DASH],
      ['Version', h('span', { class: 'mono', text: st.version || DASH })],
      ['Uptime', fmtUptime(st.uptime)],
      ['Sessions since start', h('span', { class: 'num', text: fmtInt(count.total) })],
      ['Session limit', h('span', { class: 'num', text: fmtInt(count.limit) })],
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
    item.note.replaceChildren(h('span', { class: 'bad', text: err.status === 503 ? 'not connected' : err.message }));
  }

  scope.poll(async (signal) => {
    await Promise.all([
      rows('sofia', signal).then((list) => {
        const profiles = list.filter((r) => r.type === 'profile');
        const running = profiles.filter((r) => /^RUNNING/i.test(r.state || '')).length;
        gProfiles.value.textContent = fmtInt(profiles.length);
        gProfiles.note.replaceChildren(running === profiles.length ? 'all running' : h('span', { class: 'bad', text: profiles.length - running + ' not running' }));
        const gws = list.filter((r) => r.type === 'gateway');
        const down = gws.filter((r) => GW_DOWN.test(r.state || '')).length;
        gGateways.value.textContent = fmtInt(gws.length);
        gGateways.note.replaceChildren(!gws.length ? 'none configured' : down ? h('span', { class: 'bad', text: down + ' down' }) : 'all up');
      }, (err) => {
        if (isAbort(err)) throw err;
        fail(gProfiles, err);
        fail(gGateways, err);
      }),
      rows('registrations', signal).then((list) => {
        gRegs.value.textContent = fmtInt(list.length);
        gRegs.note.textContent = list.length === 1 ? 'endpoint registered' : 'endpoints registered';
      }, (err) => {
        if (isAbort(err)) throw err;
        fail(gRegs, err);
      }),
      Promise.all([rows('channels', signal), rows('calls', signal)]).then(([channels, calls]) => {
        gCalls.value.textContent = fmtInt(calls.filter((c) => c.b_uuid).length);
        gCalls.note.textContent = 'bridged · ' + channels.length + (channels.length === 1 ? ' channel' : ' channels');
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
