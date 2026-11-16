// SIP: profiles, gateways and registrations, refreshed every 10s.

import { api, instPath, isAbort, rowsOf } from '../api.js';
import { h, DASH, cellText, fmtDuration, fmtInt } from '../dom.js';
import { icon } from '../icons.js';
import { t } from '../i18n.js';
import { clip, dataTable, detailBody, emptyState, openDrawer, openModal, panel, rowMatches, rowMenu, searchBox, sortState, statePill, tableHost, tag } from '../ui.js';
import { operate } from '../actions.js';

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

export function mountSip({ id, scope, root, ctx }) {
  const profiles = panel({ title: t('sip.profiles'), flush: true });
  const gateways = panel({ title: t('sip.gateways'), flush: true });
  let query = '';
  const search = searchBox(t('sip.filterRegs'), (q) => {
    query = q;
    renderRegs();
  }, 'sm');
  const regs = panel({ title: t('sip.regs'), flush: true, actions: search.el });
  const pHost = tableHost({ rows: 4, cols: 4 });
  const gHost = tableHost({ rows: 3, cols: 4 });
  // short tables: let their action menus overflow instead of being clipped
  pHost.el.classList.add('no-clip');
  gHost.el.classList.add('no-clip');
  const rHost = tableHost({ rows: 6, cols: 4 });
  const aliases = h('div', { class: 'panel-body row wrap', hidden: true });
  profiles.body.append(pHost.el, aliases);
  gateways.body.append(gHost.el);
  regs.body.append(rHost.el);
  root.append(profiles.el, gateways.el, regs.el);

  let sofiaRows = null;
  let gwRows = null;
  let regRows = null;
  let selected = null;
  const pSort = sortState(() => renderProfiles());
  const gSort = sortState(() => renderGateways());
  const rSort = sortState(() => renderRegs());

  function showDetails(kicker, title, subtitle, row, primary) {
    const d = openDrawer({ kicker, title, subtitle, onClose: () => { selected = null; } });
    d.body.append(...detailBody(row, primary).filter(Boolean));
  }

  const canControl = ctx.can('control');

  // profileOp runs "sofia profile <p> <op>"; the ones that drop calls or
  // registrations are confirmed with the profile named.
  async function profileOp(profile, op) {
    const confirms = {
      restart: { title: t('sip.op.restart.title', { profile }), message: t('sip.op.restart.text', { profile }), confirmLabel: t('sip.op.restart'), danger: true },
      stop: { title: t('sip.op.stop.title', { profile }), message: t('sip.op.stop.text', { profile }), confirmLabel: t('sip.op.stop'), danger: true },
      flush_inbound_reg: { title: t('sip.op.flush.title', { profile }), message: t('sip.op.flush.text', { profile }), confirmLabel: t('sip.op.flush'), danger: true },
      start: null,
      rescan: null,
    };
    const titles = {
      restart: t('sip.op.restart'), stop: t('sip.op.stop'), start: t('sip.op.start'), rescan: t('sip.rescan'), flush_inbound_reg: t('sip.op.flush'),
    };
    const out = await operate({
      id, scope, body: { action: 'sofia_profile', profile, op }, confirm: confirms[op],
      done: t('sip.op.done', { op: titles[op], name: profile }), failed: t('sip.op.failed', { op: titles[op], name: profile }),
    });
    if (out !== null) refresh();
  }

  // siptrace turns "sofia profile <p> siptrace on|off" on or off; the trace
  // goes to the FreeSWITCH log, so turning it on is confirmed.
  function siptrace(profile, on) {
    return operate({
      id, scope, body: { action: 'sofia_siptrace', profile, on },
      confirm: on ? { title: t('sip.trace.title', { profile }), message: t('sip.trace.text'), confirmLabel: t('sip.trace.confirm') } : null,
      done: on ? t('sip.trace.onDone', { profile }) : t('sip.trace.offDone', { profile }),
      failed: t('sip.op.failed', { op: on ? t('sip.trace.on') : t('sip.trace.off'), name: profile }),
    });
  }

  async function gatewayOp(r, op) {
    const gateway = cellText(r.name);
    const profile = cellText(r.profile);
    const confirms = {
      unregister: { title: t('sip.gw.unregister.title', { gateway }), message: t('sip.gw.unregister.text', { gateway }), confirmLabel: t('sip.gw.unregister') },
      kill: { title: t('sip.gw.kill.title', { gateway }), message: t('sip.gw.kill.text', { gateway, profile }), confirmLabel: t('sip.gw.kill'), danger: true },
      register: null,
    };
    const titles = { register: t('sip.gw.register'), unregister: t('sip.gw.unregister'), kill: t('sip.gw.kill') };
    const out = await operate({
      id, scope, body: { action: 'sofia_gateway', profile, gateway, op }, confirm: confirms[op],
      done: t('sip.op.done', { op: titles[op], name: gateway }), failed: t('sip.op.failed', { op: titles[op], name: gateway }),
    });
    if (out !== null) refresh();
  }

  // startDialog asks for the name of a stopped profile (it is no longer listed).
  function startDialog() {
    const input = h('input', { id: 'sp-name', class: 'input mono', type: 'text', autocomplete: 'off', spellcheck: false, placeholder: 'internal' });
    const cancel = h('button', { type: 'button', class: 'btn', text: t('common.cancel') });
    const ok = h('button', { type: 'submit', class: 'btn btn-primary', text: t('sip.op.start') });
    ok.setAttribute('form', 'sp-form');
    const form = h('form', { id: 'sp-form', class: 'modal-form' },
      h('p', { class: 'modal-text', text: t('sip.op.startText') }),
      h('div', { class: 'field' }, h('label', { for: 'sp-name', text: t('sip.col.profile') }), input));
    const m = openModal({ title: t('sip.op.startTitle'), body: form, footer: [cancel, ok] });
    cancel.addEventListener('click', m.close);
    form.addEventListener('submit', (ev) => {
      ev.preventDefault();
      const name = input.value.trim();
      if (!/^[A-Za-z0-9_.-]+$/.test(name)) {
        input.setAttribute('aria-invalid', 'true');
        return;
      }
      m.close();
      profileOp(name, 'start');
    });
    input.focus();
  }
  if (canControl) {
    profiles.el.querySelector('.panel-head').append(h('button', { type: 'button', class: 'btn btn-sm', on: { click: startDialog } }, icon('play'), t('sip.op.startTitle')));
  }

  function renderProfiles() {
    if (!sofiaRows) return;
    const list = sofiaRows;
    const rows = list.filter((r) => r.type === 'profile');
    profiles.setCount(rows.length);
    pHost.show(dataTable({
      rows,
      sort: pSort,
      empty: emptyState({ icon: 'radio', title: t('sip.noProfiles'), text: t('sip.noProfilesText') }),
      columns: [
        { label: t('sip.col.profile'), sortBy: (r) => cellText(r.name), render: (r) => h('span', { class: 'cell-strong', text: cellText(r.name) }) },
        { label: t('sip.col.listen'), mono: true, sortBy: (r) => listenAddress(r.data), render: (r) => listenAddress(r.data) },
        { label: t('sip.col.transport'), sortBy: (r) => parseProfileState(r.state).transport, render: (r) => tag(parseProfileState(r.state).transport) },
        { label: t('sip.col.state'), sortBy: (r) => parseProfileState(r.state).state, render: (r) => statePill(parseProfileState(r.state).state) },
        { label: t('sip.col.inUse'), right: true, sortBy: (r) => parseProfileState(r.state).inUse, render: (r) => h('span', { class: 'num', text: parseProfileState(r.state).inUse || DASH }) },
        ctx.can('operate') ? {
          label: '', right: true,
          render: (r) => {
            const name = cellText(r.name);
            return h('span', { class: 'row-actions' },
              h('button', { type: 'button', class: 'btn btn-sm', title: t('sip.rescanTitle'), on: { click: () => profileOp(name, 'rescan') } }, icon('refresh'), t('sip.rescan')),
              canControl ? rowMenu(t('sip.op.menu', { name }), [
                { label: t('sip.trace.on'), icon: 'activity', run: () => siptrace(name, true) },
                { label: t('sip.trace.off'), icon: 'pause', run: () => siptrace(name, false) },
                { sep: true },
                { label: t('sip.op.restart'), sub: t('sip.op.restartSub'), icon: 'refresh', danger: true, run: () => profileOp(name, 'restart') },
                { label: t('sip.op.stop'), sub: t('sip.op.stopSub'), icon: 'stop', danger: true, run: () => profileOp(name, 'stop') },
                { sep: true },
                { label: t('sip.op.flush'), sub: t('sip.op.flushSub'), icon: 'eraser', run: () => profileOp(name, 'flush_inbound_reg') },
              ]) : null);
          },
        } : null,
      ].filter(Boolean),
    }));
    const al = list.filter((r) => r.type === 'alias');
    aliases.hidden = !al.length;
    aliases.replaceChildren(h('span', { class: 'muted', text: t('sip.aliases') }), ...al.map((r) => tag(cellText(r.name) + ' → ' + cellText(r.data))));
  }

  function renderGateways() {
    if (!gwRows) return;
    const rows = gwRows;
    gateways.setCount(rows.length);
    gHost.show(dataTable({
      rows,
      sort: gSort,
      keyOf: (r) => 'gw:' + r.name,
      selected,
      onRow: (r) => {
        selected = 'gw:' + r.name;
        showDetails(t('sip.kind.gateway'), cellText(r.name), t('sip.onProfile', { profile: cellText(r.profile) }), r, ['name', 'profile', 'state', 'status', 'proxy', 'realm', 'username']);
      },
      empty: emptyState({ icon: 'plug', title: t('sip.noGateways'), text: t('sip.noGatewaysText') }),
      columns: [
        { label: t('sip.col.gateway'), sortBy: (r) => cellText(r.name), render: (r) => h('span', null, h('span', { class: 'cell-strong', text: cellText(r.name) }), h('span', { class: 'sub', text: t('sip.onProfile', { profile: cellText(r.profile) }) })) },
        { label: t('sip.col.registration'), sortBy: (r) => cellText(r.state), render: (r) => statePill(r.state) },
        { label: t('sip.col.status'), sortBy: (r) => cellText(r.status), render: (r) => statePill(r.status) },
        { label: t('sip.col.proxy'), mono: true, sortBy: (r) => stripSip(r.proxy), render: (r) => clip(stripSip(r.proxy)) },
        { label: t('sip.col.calls'), right: true, sortBy: (r) => (Number(r['calls-in']) || 0) + (Number(r['calls-out']) || 0), render: (r) => h('span', { class: 'num', text: fmtInt(r['calls-in']) + ' / ' + fmtInt(r['calls-out']) }) },
        {
          label: t('sip.col.failed'), right: true,
          sortBy: (r) => (Number(r['failed-calls-in']) || 0) + (Number(r['failed-calls-out']) || 0),
          render: (r) => {
            const failed = (Number(r['failed-calls-in']) || 0) + (Number(r['failed-calls-out']) || 0);
            return h('span', { class: failed ? 'num text-bad' : 'num faint', text: fmtInt(r['failed-calls-in']) + ' / ' + fmtInt(r['failed-calls-out']) });
          },
        },
        { label: t('sip.col.upFor'), right: true, sortBy: (r) => Number(r['uptime-usec']) || 0, render: (r) => h('span', { class: 'num', text: Number(r['uptime-usec']) > 0 ? fmtDuration(Number(r['uptime-usec']) / 1e6) : DASH }) },
        ctx.can('operate') ? {
          label: '', right: true,
          render: (r) => rowMenu(t('sip.op.menu', { name: cellText(r.name) }), [
            { label: t('sip.gw.register'), icon: 'refresh', run: () => gatewayOp(r, 'register') },
            { label: t('sip.gw.unregister'), icon: 'logout', run: () => gatewayOp(r, 'unregister') },
            canControl ? { sep: true } : null,
            canControl ? { label: t('sip.gw.kill'), sub: t('sip.gw.killSub'), icon: 'trash', danger: true, run: () => gatewayOp(r, 'kill') } : null,
          ]),
        } : null,
      ].filter(Boolean),
    }));
  }

  function renderRegs() {
    if (!regRows) return;
    const rows = regRows.filter((r) => rowMatches(r, query));
    regs.setCount(query ? t('common.nOfM', { n: fmtInt(rows.length), m: fmtInt(regRows.length) }) : regRows.length);
    const now = Date.now() / 1000;
    rHost.show(dataTable({
      rows,
      sort: rSort,
      keyOf: (r) => 'reg:' + r.token,
      selected,
      onRow: (r) => {
        selected = 'reg:' + r.token;
        showDetails(t('sip.kind.reg'), cellText(r.reg_user) + '@' + cellText(r.realm), cellText(r.url).replace(/^sofia\/[^/]+\//, ''), r, ['reg_user', 'realm', 'url', 'network_ip', 'network_port', 'network_proto', 'expires']);
      },
      empty: query
        ? emptyState({ icon: 'search', title: t('common.noMatch'), text: t('sip.noRegMatch') })
        : emptyState({ icon: 'user', title: t('sip.noRegs'), text: t('sip.noRegsText') }),
      columns: [
        { label: t('sip.col.user'), sortBy: (r) => cellText(r.reg_user), render: (r) => h('span', null, h('span', { class: 'cell-strong', text: cellText(r.reg_user) }), h('span', { class: 'faint', text: '@' + cellText(r.realm) })) },
        { label: t('sip.col.contact'), mono: true, sortBy: (r) => cellText(r.url), render: (r) => clip(String(r.url || '').replace(/^sofia\/[^/]+\//, '')) },
        { label: t('sip.col.network'), mono: true, sortBy: (r) => cellText(r.network_ip) + ':' + cellText(r.network_port), render: (r) => cellText(r.network_ip) + ':' + cellText(r.network_port) },
        { label: t('sip.col.transport'), sortBy: (r) => cellText(r.network_proto), render: (r) => (r.network_proto ? tag(String(r.network_proto).toUpperCase()) : '') },
        {
          label: t('sip.col.expires'), right: true,
          sortBy: (r) => Number(r.expires) || 0,
          render: (r) => {
            const left = Number(r.expires) - now;
            return h('span', { class: 'num', text: Number.isFinite(left) ? (left > 0 ? fmtDuration(left) : t('sip.expired')) : DASH });
          },
        },
        { label: t('sip.col.host'), sortBy: (r) => cellText(r.hostname), render: (r) => h('span', { class: 'muted', text: cellText(r.hostname) }) },
      ],
    }));
  }

  const refresh = scope.poll(async (signal) => {
    const get = (path) => api('GET', instPath(id, path), { signal }).then(rowsOf);
    await Promise.all([
      get('sofia').then((rows) => {
        sofiaRows = rows;
        renderProfiles();
      }, (err) => {
        if (isAbort(err)) throw err;
        sofiaRows = null;
        profiles.setCount(null);
        aliases.hidden = true;
        pHost.error(err);
      }),
      get('gateways').then((rows) => {
        gwRows = rows;
        renderGateways();
      }, (err) => {
        if (isAbort(err)) throw err;
        gwRows = null;
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
