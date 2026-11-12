// Overview: /status every 5s plus live heartbeat/state from the stream.

import { api, instPath, isAbort } from '../api.js';
import { h, errorBox, DASH, fmtUptime, fmtNum, fmtPct } from '../dom.js';

export function mountOverview({ id, scope, root, setState }) {
  const v = {};
  const value = (key, cls) => (v[key] = h('span', { class: cls, text: DASH }));
  const single = (label, key) => h('div', { class: 'card' },
    h('div', { class: 'card-label', text: label }), value(key, 'card-value'));
  const multi = (label, items) => h('div', { class: 'card' },
    h('div', { class: 'card-label', text: label }),
    h('dl', { class: 'kv' }, items.map(([l, key]) => [h('dt', { text: l }), h('dd', null, value(key, 'kv-value'))])));

  const notice = h('div', { class: 'msg-area' });
  const cards = h('div', { class: 'cards' },
    single('System status', 'systemStatus'),
    single('Hostname', 'hostname'),
    single('Version', 'version'),
    single('Uptime', 'uptime'),
    single('Idle CPU', 'idle'),
    multi('Sessions', [['Active', 'active'], ['Peak', 'peak'], ['Peak 5 min', 'peak5'], ['Limit', 'limit'], ['Total', 'total']]),
    multi('Session rate (per second)', [['Current', 'rateCur'], ['Max', 'rateMax'], ['Peak', 'ratePeak']]));
  cards.hidden = true;
  root.append(notice, cards);

  const set = (key, text) => {
    v[key].textContent = text;
  };

  function showDisconnected(st) {
    cards.hidden = true;
    notice.replaceChildren(h('div', { class: 'notice calm', role: 'status' },
      h('strong', { text: 'Disconnected. ' }),
      'fsdeck has no Event Socket connection to this FreeSWITCH.',
      st && st.lastError ? h('div', { class: 'mono last-error', text: st.lastError }) : null));
  }

  let connected = null; // last known; null until the first answer

  function apply(data) {
    if (data.state) setState(data.state);
    const s = data.status;
    connected = Boolean(s && typeof s === 'object');
    if (!connected) {
      showDisconnected(data.state);
      return;
    }
    notice.replaceChildren();
    cards.hidden = false;
    const count = (s.sessions && s.sessions.count) || {};
    const rate = (s.sessions && s.sessions.rate) || {};
    set('systemStatus', fmtNum(s.systemStatus));
    set('hostname', fmtNum(data.hostname));
    set('version', fmtNum(s.version));
    set('uptime', fmtUptime(s.uptime));
    // idleCPU.allowed is the current idle percentage (switch_core_idle_cpu);
    // idleCPU.used is the configured minimum.
    set('idle', fmtPct(s.idleCPU && s.idleCPU.allowed));
    set('active', fmtNum(count.active));
    set('peak', fmtNum(count.peak));
    set('peak5', fmtNum(count.peak5Min));
    set('limit', fmtNum(count.limit));
    set('total', fmtNum(count.total));
    set('rateCur', fmtNum(rate.current));
    set('rateMax', fmtNum(rate.max));
    set('ratePeak', fmtNum(rate.peak));
  }

  const refresh = scope.poll(async (signal) => {
    try {
      const data = await api('GET', instPath(id, 'status'), { signal });
      if (data) apply(data);
    } catch (err) {
      if (isAbort(err)) throw err;
      notice.replaceChildren(errorBox(err));
    }
  }, 5000);

  scope.stream('api/' + instPath(id, 'stream'), {
    state: (st) => {
      setState(st);
      if (!st.connected) showDisconnected(st);
      else if (connected === false) refresh(); // reconnected: reload now
      connected = Boolean(st.connected);
    },
    heartbeat: (hb) => {
      if (hb['Session-Count'] !== undefined) set('active', fmtNum(hb['Session-Count']));
      if (hb['Idle-CPU'] !== undefined) set('idle', fmtPct(hb['Idle-CPU']));
      // Session-Per-Sec is the remaining per-second budget; the last-second
      // rate (status "rate.current") is Session-Per-Sec-Last.
      if (hb['Session-Per-Sec-Last'] !== undefined) set('rateCur', fmtNum(hb['Session-Per-Sec-Last']));
    },
  });
}
