// SIP: profiles, gateways and registrations, refreshed every 10s.

import { api, instPath, isAbort, rowsOf } from '../api.js';
import { h, DASH, cellText, fmtDuration, fmtInt } from '../dom.js';
import { icon } from '../icons.js';
import { clip, dataTable, detailBody, emptyState, openDrawer, panel, rowMatches, searchBox, statePill, tableHost, tag, toast, toastError } from '../ui.js';

// "sip:mod_sofia@10.0.4.21:5060;transport=ws" -> "10.0.4.21:5060"
function listenAddress(data) {
  return String(data || '').replace(/^sips?:(mod_sofia@)?/, '').replace(/;.*$/, '');
}

// "RUNNING (4) (TLS)" -> {state: "RUNNING", inUse: "4", transport: "TLS"}
function parseProfileState(state) {
  const m = String(state || '').match(/^(\S+)(?:\s+\((\d+)\))?(?:\s+\((\w+)\))?/);
  return m ? { state: m[1], inUse: m[2] || '', transport: m[3] || 'UDP/TCP' } : { state: String(state || ''), inUse: '', transport: '' };
}

function stripSip(v) {
  return String(v || '').replace(/^<?sips?:/, '').replace(/>$/, '');
}

export function mountSip({ id, scope, root, isAdmin }) {
  const profiles = panel({ title: 'Profiles', flush: true });
  const gateways = panel({ title: 'Gateways', flush: true });
  let query = '';
  const search = searchBox('Filter registrations', (q) => {
    query = q;
    renderRegs();
  });
  const regs = panel({ title: 'Registrations', flush: true, actions: search.el });
  const pHost = tableHost();
  const gHost = tableHost();
  const rHost = tableHost();
  const aliases = h('div', { class: 'panel-body row wrap', hidden: true });
  profiles.body.append(pHost.el, aliases);
  gateways.body.append(gHost.el);
  regs.body.append(rHost.el);
  root.append(profiles.el, gateways.el, regs.el);

  let regRows = null;
  let selected = null;

  function showDetails(title, subtitle, row, primary) {
    const d = openDrawer({ title, subtitle, onClose: () => { selected = null; } });
    d.body.append(...detailBody(row, primary).filter(Boolean));
  }

  async function rescan(profile) {
    try {
      const data = await api('POST', instPath(id, 'actions'), { body: { action: 'sofia_rescan', profile }, signal: scope.signal });
      toast((data && data.output) || 'Done.', 'ok', 'Rescanned ' + profile);
      refresh();
    } catch (err) {
      toastError(err, 'Rescan of ' + profile + ' failed');
    }
  }

  function renderProfiles(list) {
    const rows = list.filter((r) => r.type === 'profile');
    profiles.setCount(rows.length);
    pHost.show(dataTable({
      rows,
      empty: emptyState({ icon: 'radio', title: 'No SIP profiles', text: 'mod_sofia reports no profiles.' }),
      columns: [
        { label: 'Profile', render: (r) => h('span', { class: 'cell-strong', text: cellText(r.name) }) },
        { label: 'Listens on', mono: true, render: (r) => listenAddress(r.data) },
        { label: 'Transport', render: (r) => tag(parseProfileState(r.state).transport) },
        { label: 'State', render: (r) => statePill(parseProfileState(r.state).state) },
        { label: 'In use', right: true, render: (r) => h('span', { class: 'num', text: parseProfileState(r.state).inUse || DASH }) },
        isAdmin ? {
          label: '', right: true,
          render: (r) => h('button', { type: 'button', class: 'btn btn-sm', title: 'Reload gateways and settings of this profile from the XML', on: { click: () => rescan(cellText(r.name)) } }, icon('refresh'), 'Rescan'),
        } : null,
      ].filter(Boolean),
    }));
    const al = list.filter((r) => r.type === 'alias');
    aliases.hidden = !al.length;
    aliases.replaceChildren(h('span', { class: 'muted', text: 'Aliases' }), ...al.map((r) => tag(cellText(r.name) + ' → ' + cellText(r.data))));
  }

  function renderGateways(rows) {
    gateways.setCount(rows.length);
    gHost.show(dataTable({
      rows,
      keyOf: (r) => 'gw:' + r.name,
      selected,
      onRow: (r) => {
        selected = 'gw:' + r.name;
        showDetails(cellText(r.name), 'Gateway on profile ' + cellText(r.profile), r, ['name', 'profile', 'state', 'status', 'proxy', 'realm', 'username']);
      },
      empty: emptyState({ icon: 'plug', title: 'No gateways', text: 'No gateway is configured on any SIP profile.' }),
      columns: [
        { label: 'Gateway', render: (r) => h('span', null, h('span', { class: 'cell-strong', text: cellText(r.name) }), h('span', { class: 'sub', text: 'profile ' + cellText(r.profile) })) },
        { label: 'Registration', render: (r) => statePill(r.state) },
        { label: 'Status', render: (r) => statePill(r.status) },
        { label: 'Proxy', mono: true, render: (r) => clip(stripSip(r.proxy)) },
        { label: 'Calls in / out', right: true, render: (r) => h('span', { class: 'num', text: fmtInt(r['calls-in']) + ' / ' + fmtInt(r['calls-out']) }) },
        {
          label: 'Failed in / out', right: true,
          render: (r) => {
            const failed = (Number(r['failed-calls-in']) || 0) + (Number(r['failed-calls-out']) || 0);
            return h('span', { class: failed ? 'num text-bad' : 'num faint', text: fmtInt(r['failed-calls-in']) + ' / ' + fmtInt(r['failed-calls-out']) });
          },
        },
        { label: 'Up for', right: true, render: (r) => h('span', { class: 'num', text: Number(r['uptime-usec']) > 0 ? fmtDuration(Number(r['uptime-usec']) / 1e6) : DASH }) },
      ],
    }));
  }

  function renderRegs() {
    if (!regRows) return;
    const rows = regRows.filter((r) => rowMatches(r, query));
    regs.setCount(query ? rows.length + ' of ' + regRows.length : regRows.length);
    const now = Date.now() / 1000;
    rHost.show(dataTable({
      rows,
      keyOf: (r) => 'reg:' + r.token,
      selected,
      onRow: (r) => {
        selected = 'reg:' + r.token;
        showDetails(cellText(r.reg_user) + '@' + cellText(r.realm), 'Registration', r, ['reg_user', 'realm', 'url', 'network_ip', 'network_port', 'network_proto', 'expires']);
      },
      empty: query
        ? emptyState({ icon: 'search', title: 'No match', text: 'No registration matches the filter.' })
        : emptyState({ icon: 'user', title: 'No registrations', text: 'No endpoint is registered with this FreeSWITCH right now.' }),
      columns: [
        { label: 'User', render: (r) => h('span', null, h('span', { class: 'cell-strong', text: cellText(r.reg_user) }), h('span', { class: 'faint', text: '@' + cellText(r.realm) })) },
        { label: 'Contact', mono: true, render: (r) => clip(String(r.url || '').replace(/^sofia\/[^/]+\//, '')) },
        { label: 'Network', mono: true, render: (r) => cellText(r.network_ip) + ':' + cellText(r.network_port) },
        { label: 'Transport', render: (r) => (r.network_proto ? tag(String(r.network_proto).toUpperCase()) : '') },
        {
          label: 'Expires in', right: true,
          render: (r) => {
            const left = Number(r.expires) - now;
            return h('span', { class: 'num', text: Number.isFinite(left) ? (left > 0 ? fmtDuration(left) : 'expired') : DASH });
          },
        },
        { label: 'Host', render: (r) => h('span', { class: 'muted', text: cellText(r.hostname) }) },
      ],
    }));
  }

  const refresh = scope.poll(async (signal) => {
    const get = (path) => api('GET', instPath(id, path), { signal }).then(rowsOf);
    await Promise.all([
      get('sofia').then(renderProfiles, (err) => {
        if (isAbort(err)) throw err;
        profiles.setCount(null);
        aliases.hidden = true;
        pHost.error(err);
      }),
      get('gateways').then(renderGateways, (err) => {
        if (isAbort(err)) throw err;
        gateways.setCount(null);
        gHost.error(err);
      }),
      get('registrations').then((rows) => {
        regRows = rows;
        renderRegs();
      }, (err) => {
        if (isAbort(err)) throw err;
        regRows = null;
        regs.setCount(null);
        rHost.error(err);
      }),
    ]);
  }, 10000);
}
