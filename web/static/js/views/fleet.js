// Fleet view: one card per FreeSWITCH instance; admin can add, edit, delete.

import { api, instPath, isAbort } from '../api.js';
import { h, DASH, fmtInt, fmtPct, fmtUptime, sinceText } from '../dom.js';
import { icon } from '../icons.js';
import { plural, t } from '../i18n.js';
import { confirmDialog, connectionHint, emptyState, iconButton, openModal, pill, skeleton, tag, toast, toastError } from '../ui.js';

export function address(inst) {
  const host = String(inst.host || '');
  return (host.includes(':') ? '[' + host + ']' : host) + ':' + inst.port;
}

// shortVersion keeps "1.10.12" of "1.10.12 -release-10-a88d069d6f 64bit".
export function shortVersion(v) {
  return String(v || '').split(' ')[0];
}

export function mountFleet({ scope, root, ctx }) {
  document.title = t('fleet.docTitle');
  const sub = h('p', { class: 'page-sub' });
  const summary = h('section', { class: 'panel summary', 'aria-label': t('fleet.summary') });
  const grid = h('div', { class: 'fleet' },
    [0, 1, 2].map(() => h('div', { class: 'inst-card' }, skeleton({ rows: 5, cols: 1 }))));
  const addBtn = ctx.can('manage')
    ? h('button', { type: 'button', class: 'btn btn-primary', on: { click: () => instanceDialog(null) } }, icon('plus'), t('fleet.add'))
    : null;
  root.replaceChildren(
    h('header', { class: 'masthead' },
      h('div', { class: 'masthead-in solo' },
        h('div', { class: 'page-head' }, h('div', { class: 'spacer' }, h('h1', { class: 'page-title', text: t('fleet.title') }), sub), addBtn))),
    h('div', { class: 'page-body' }, summary, grid));
  summary.hidden = true;

  const statuses = new Map(); // id -> /status answer
  let signature = '';

  function model(inst) {
    const ok = Boolean(inst.state && inst.state.connected);
    const st = ok ? statuses.get(inst.id) : null;
    const s = st && st.status ? st.status : null;
    const count = (s && s.sessions && s.sessions.count) || {};
    return {
      inst, ok,
      error: (inst.state && inst.state.lastError) || '',
      since: sinceText(inst.state && inst.state.since),
      sessions: s ? fmtInt(count.active) : DASH,
      idle: s && s.idleCPU ? fmtPct(s.idleCPU.allowed) + '%' : DASH,
      uptime: s ? fmtUptime(s.uptime) : DASH,
      version: s ? shortVersion(s.version) : '',
      hostname: (st && st.hostname) || '',
    };
  }

  function card(m) {
    const inst = m.inst;
    return h('article', { class: m.ok ? 'inst-card' : 'inst-card down' },
      h('div', { class: 'inst-top' },
        h('span', { class: m.ok ? 'dot ok' : 'dot bad' }),
        h('a', { class: 'inst-name', href: '#/i/' + encodeURIComponent(inst.id) + '/overview', text: inst.name }),
        ctx.can('manage') ? h('div', { class: 'inst-actions' },
          iconButton('edit', t('fleet.editNamed', { name: inst.name }), () => instanceDialog(inst)),
          iconButton('trash', t('fleet.deleteNamed', { name: inst.name }), () => remove(inst))) : null),
      h('div', { class: 'inst-addr' },
        h('span', { class: 'mono', text: address(inst) }),
        h('span', { class: 'spacer' }),
        pill(m.ok ? t('state.connected') : t('state.disconnected'), m.ok ? 'ok' : 'bad')),
      m.ok
        ? h('div', { class: 'inst-stats' }, stat(t('fleet.card.sessions'), m.sessions), stat(t('fleet.card.idle'), m.idle), stat(t('fleet.card.uptime'), m.uptime))
        : h('div', { class: 'inst-error', title: m.error || undefined }, icon('alert'), h('span', { class: 'spacer' },
          h('span', { class: 'inst-error-text', text: connectionHint(m.error) || m.error || t('state.connecting') }),
          connectionHint(m.error) ? h('span', { class: 'raw', text: m.error }) : null)),
      h('div', { class: 'inst-foot' },
        m.version ? tag(m.version) : null,
        m.hostname ? h('span', { text: m.hostname }) : null,
        h('span', { class: 'spacer' }),
        m.since ? h('span', { text: m.ok ? t('fleet.card.upFor', { d: m.since }) : t('fleet.card.downFor', { d: m.since }) }) : null));
  }

  function cell(iconName, label, value, note, tone) {
    return h('div', { class: 'summary-item' },
      h('div', { class: 'summary-label' }, icon(iconName), label),
      h('div', { class: tone ? 'summary-value ' + tone : 'summary-value' }, value),
      h('div', { class: 'summary-note', text: note }));
  }

  function renderSummary(list) {
    const up = list.filter((i) => i.state && i.state.connected);
    const down = list.length - up.length;
    let sessions = 0;
    let limit = 0;
    let known = 0;
    for (const i of up) {
      const st = statuses.get(i.id);
      const count = st && st.status && st.status.sessions && st.status.sessions.count;
      if (!count) continue;
      known++;
      sessions += Number(count.active) || 0;
      limit += Number(count.limit) || 0;
    }
    summary.hidden = !list.length;
    summary.replaceChildren(
      cell('server', t('fleet.sum.instances'), fmtInt(list.length), t('fleet.sum.instancesNote')),
      cell('plug', t('fleet.sum.connected'), [fmtInt(up.length), h('span', { class: 'of', text: ' / ' + fmtInt(list.length) })], up.length === list.length ? t('fleet.sum.allUp') : t('fleet.sum.connectedNote'), up.length === list.length ? 'ok' : ''),
      cell('alert', t('fleet.sum.down'), fmtInt(down), down ? t('fleet.sum.downNote') : t('fleet.sum.noneDown'), down ? 'bad' : ''),
      cell('activity', t('fleet.sum.sessions'), known ? fmtInt(sessions) : DASH,
        known ? (limit ? t('fleet.sum.ofLimit', { pct: fmtPct((sessions / limit) * 100, 1), limit: fmtInt(limit) }) : t('fleet.sum.acrossUp')) : t('fleet.sum.waiting')));
  }

  function stat(label, value) {
    return h('div', null, h('div', { class: 'inst-stat-label', text: label }), h('div', { class: 'inst-stat-value', text: value, title: value }));
  }

  function render() {
    if (!ctx.loaded()) return;
    const list = ctx.instances();
    const up = list.filter((i) => i.state && i.state.connected).length;
    const vars = { n: fmtInt(list.length), up: fmtInt(up), down: fmtInt(list.length - up) };
    sub.textContent = !list.length
      ? t('fleet.sub.empty')
      : up < list.length
        ? plural(list.length, t('fleet.sub.someDown.one', vars), t('fleet.sub.someDown.other', vars))
        : plural(list.length, t('fleet.sub.allUp.one', vars), t('fleet.sub.allUp.other', vars));
    renderSummary(list);
    if (!list.length) {
      signature = '';
      grid.replaceChildren(h('div', { class: 'panel fleet-empty' }, emptyState({
        icon: 'server',
        title: t('fleet.empty.title'),
        text: ctx.can('manage') ? t('fleet.empty.admin') : t('fleet.empty.viewer'),
        action: ctx.can('manage') ? h('button', { type: 'button', class: 'btn btn-primary', on: { click: () => instanceDialog(null) } }, icon('plus'), t('fleet.add')) : null,
      })));
      return;
    }
    const models = list.map(model);
    // Re-rendering identical cards would restart hover transitions.
    const sig = JSON.stringify(models.map((m) => [m.inst.id, m.inst.name, address(m.inst), m.ok, m.error, m.since.replace(/\d+s$/, ''), m.sessions, m.idle, m.uptime, m.version, m.hostname]));
    if (sig === signature) return;
    signature = sig;
    grid.replaceChildren(...models.map(card));
  }

  async function loadStatuses(list) {
    await Promise.all(list.filter((i) => i.state && i.state.connected).map(async (i) => {
      try {
        statuses.set(i.id, await api('GET', instPath(i.id, 'status'), { signal: scope.signal }));
      } catch (err) {
        if (!isAbort(err)) statuses.delete(i.id);
      }
    }));
    if (!scope.disposed) render();
  }

  scope.onCleanup(ctx.onInstances((list) => {
    render();
    loadStatuses(list);
  }));
  render();
  loadStatuses(ctx.instances());

  async function remove(inst) {
    const yes = await confirmDialog({
      title: t('fleet.del.title', { name: inst.name }),
      message: t('fleet.del.text', { addr: address(inst) }),
      confirmLabel: t('common.delete'), danger: true,
    });
    if (!yes || scope.disposed) return;
    try {
      await api('DELETE', instPath(inst.id), { signal: scope.signal });
      toast(t('fleet.del.done', { name: inst.name }));
      ctx.refreshInstances();
    } catch (err) {
      toastError(err, t('fleet.del.failed'));
    }
  }

  function instanceDialog(inst) {
    const editing = Boolean(inst);
    const name = h('input', { id: 'f-name', class: 'input', type: 'text', required: true, maxLength: 64, autocomplete: 'off', value: editing ? inst.name : '', placeholder: 'fs-prod-01' });
    const host = h('input', { id: 'f-host', class: 'input mono', type: 'text', required: true, autocomplete: 'off', spellcheck: false, value: editing ? inst.host : '', placeholder: '10.0.4.21' });
    const port = h('input', { id: 'f-port', class: 'input mono', type: 'number', required: true, min: '1', max: '65535', value: editing ? String(inst.port) : '8021' });
    const pass = h('input', { id: 'f-pass', class: 'input', type: 'password', autocomplete: 'new-password', placeholder: editing ? t('fleet.form.keepPass') : '' });
    const error = h('div', { class: 'form-error', role: 'alert', hidden: true });
    const cancel = h('button', { type: 'button', class: 'btn', text: t('common.cancel') });
    const submit = h('button', { type: 'submit', class: 'btn btn-primary', text: editing ? t('fleet.form.save') : t('fleet.add') });
    submit.setAttribute('form', 'inst-form');
    const field = (id, label, input, hint) => h('div', { class: 'field' }, h('label', { for: id, text: label }), input, hint ? h('span', { class: 'hint', text: hint }) : null);
    const form = h('form', { id: 'inst-form', class: 'modal-form' },
      field('f-name', t('fleet.form.name'), name),
      h('div', { class: 'form-row' }, field('f-host', t('fleet.form.host'), host), field('f-port', t('fleet.form.port'), port)),
      field('f-pass', t('fleet.form.pass'), pass, editing ? null : t('fleet.form.passHint')),
      error);
    const m = openModal({ title: editing ? t('fleet.form.editTitle', { name: inst.name }) : t('fleet.add'), body: form, footer: [cancel, submit] });
    cancel.addEventListener('click', m.close);
    form.addEventListener('submit', async (ev) => {
      ev.preventDefault();
      const body = { name: name.value.trim(), host: host.value.trim(), port: Number(port.value), password: pass.value };
      submit.disabled = true;
      error.hidden = true;
      try {
        if (editing) await api('PUT', instPath(inst.id), { body });
        else await api('POST', 'instances', { body });
        m.close();
        toast(editing ? t('fleet.form.saved', { name: body.name }) : t('fleet.form.added', { name: body.name }));
        ctx.refreshInstances();
      } catch (err) {
        error.textContent = err.message;
        error.hidden = false;
        submit.disabled = false;
      }
    });
    name.focus();
  }
}
