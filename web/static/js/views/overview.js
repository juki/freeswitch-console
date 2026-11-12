// Overview: live load figures with sparklines, system facts and a summary of
// SIP and calls. /status is polled every 5s; heartbeats update in between.

import { api, instPath, isAbort, rowsOf } from '../api.js';
import { h, DASH, fmtInt, fmtPct, fmtUptime } from '../dom.js';
import { icon } from '../icons.js';
import { errorState, emptyState, kv, meter, panel, pill, sparkline, stateTone } from '../ui.js';

const GW_DOWN = /^(FAIL|DOWN|TIMEOUT|EXPIRED|UNREGED)/i;

export function mountOverview({ id, scope, root, setState }) {
  const base = '#/i/' + encodeURIComponent(id) + '/';
  const notice = h('div');
  const content = h('div', { hidden: true });
  root.append(notice, content);

  // ---- stat tiles ----
  function tile(label, iconName, unit, spark) {
    const value = h('span', { class: 'stat-value', text: DASH });
    const foot = h('div', { class: 'stat-foot' });
    const el = h('div', { class: 'stat' },
      h('div', { class: 'stat-label' }, icon(iconName), label),
      h('div', { class: 'stat-main' }, h('div', null, value, unit ? h('span', { class: 'stat-unit', text: unit }) : null), spark ? spark.el : null),
      foot);
    return { el, value, foot, spark };
  }
  const sessions = tile('Active sessions', 'activity', '', sparkline());
  const rate = tile('Session rate', 'gauge', '/s', sparkline());
  const idle = tile('Idle CPU', 'cpu', '%', sparkline({ tone: 'ok', zero: false }));
  const capMeter = meter();
  const capacity = tile('Capacity used', 'server', '%', { el: h('div', { class: 'stat-meter' }, capMeter.el) });

  // ---- system and glance panels ----
  const system = panel({ title: 'System' });
  const glance = panel({ title: 'SIP and calls', flush: true });
  function glanceItem(label, iconName, tab) {
    const value = h('div', { class: 'glance-value', text: DASH });
    const note = h('div', { class: 'glance-note' });
    const el = h('a', { class: 'glance-item', href: base + tab }, h('div', { class: 'glance-label' }, icon(iconName), label), value, note);
    return { el, value, note };
  }
  const gProfiles = glanceItem('SIP profiles', 'radio', 'sip');
  const gGateways = glanceItem('Gateways', 'plug', 'sip');
  const gRegs = glanceItem('Registrations', 'user', 'sip');
  const gCalls = glanceItem('Calls', 'calls', 'calls');
  glance.body.append(h('div', { class: 'glance' }, gProfiles.el, gGateways.el, gRegs.el, gCalls.el));

  content.append(
    h('div', { class: 'stat-grid' }, sessions.el, rate.el, idle.el, capacity.el),
    h('div', { class: 'grid-2' }, system.el, glance.el));

  const foot = (el, parts) => el.replaceChildren(...parts.flatMap(([label, value], i) => [i ? ' · ' : '', label + ' ', h('b', { text: value })]));

  let limit = null;
  let connected = null; // null until the first answer

  function setSessions(n) {
    const v = Number(n);
    if (!Number.isFinite(v)) return;
    sessions.value.textContent = fmtInt(v);
    sessions.spark.push(v);
    if (limit) {
      const f = v / limit;
      capacity.value.textContent = fmtPct(f * 100, f < 0.1 ? 1 : 0);
      capMeter.set(f, f > 0.9 ? 'bad' : f > 0.7 ? 'warn' : '');
      capacity.foot.replaceChildren(h('b', { text: fmtInt(v) }), ' of ', h('b', { text: fmtInt(limit) }), ' sessions');
    }
  }

  function setIdle(n) {
    const v = Number(n);
    if (!Number.isFinite(v)) return;
    idle.value.textContent = fmtPct(v);
    idle.spark.push(v);
  }

  function setRate(n) {
    const v = Number(n);
    if (!Number.isFinite(v)) return;
    rate.value.textContent = fmtInt(v);
    rate.spark.push(v);
  }

  function showDisconnected(st) {
    content.hidden = true;
    notice.replaceChildren(h('div', { class: 'panel' }, emptyState({
      icon: 'plug', tone: 'calm', title: 'Not connected',
      text: 'fsdeck has no Event Socket connection to this FreeSWITCH.' + (st && st.lastError ? ' Last error: ' + st.lastError : ''),
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
    setSessions(count.active);
    foot(sessions.foot, [['Peak', fmtInt(count.peak)], ['5 min peak', fmtInt(count.peak5Min)]]);
    setRate(r.current);
    foot(rate.foot, [['Limit', fmtInt(r.max) + '/s'], ['Peak', fmtInt(r.peak) + '/s']]);
    // idleCPU.allowed is the current idle percentage; idleCPU.used is the
    // configured minimum.
    if (st.idleCPU) setIdle(st.idleCPU.allowed);
    foot(idle.foot, [['Minimum allowed', fmtPct(st.idleCPU ? st.idleCPU.used : null, 0) + '%']]);
    const status = String(st.systemStatus || '');
    system.body.replaceChildren(kv([
      ['Status', status ? pill(status, stateTone(status) || (status === 'ready' ? 'ok' : 'warn')) : DASH],
      ['Hostname', data.hostname || DASH],
      ['Version', h('span', { class: 'mono', text: st.version || DASH })],
      ['Uptime', fmtUptime(st.uptime)],
      ['Sessions since start', fmtInt(count.total)],
      ['Session limit', fmtInt(count.limit)],
    ]));
  }

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
      setSessions(hb['Session-Count']);
      setIdle(hb['Idle-CPU']);
      // Session-Per-Sec is the remaining per-second budget; the last-second
      // rate (status "rate.current") is Session-Per-Sec-Last.
      setRate(hb['Session-Per-Sec-Last']);
    },
  });
}
