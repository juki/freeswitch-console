// Alerts: what needs attention on the switches (the active alerts and the
// recent history, polled every 5 s while the page is shown) and, for
// administrators, where the notifications go: the four rules and up to five
// webhooks. The server keeps alerts and history in memory only.

import { api, isAbort } from '../api.js';
import { h, DASH, cellText, fmtClock, fmtDateTime, fmtDuration, fmtInt, fmtPct } from '../dom.js';
import { icon } from '../icons.js';
import { LANGS, lang, t, tx } from '../i18n.js';
import { instHash } from '../nav.js';
import { actionDialog, clip, connectionHint, dataTable, emptyState, errorState, openModal, panel, pill, rowMenu, searchBox, segmented, skeleton, sortState, tableHost, tag, toast } from '../ui.js';

const RULES = ['instanceDown', 'gatewayDown', 'idleCpuLow', 'sessionsHigh'];
// the load rules warn; the others (instance, gateway) are bad
const LOAD_RULES = ['idleCpuLow', 'sessionsHigh'];
const FORMATS = ['slack', 'dingtalk', 'wecom', 'feishu', 'json'];
const ROBOTS = ['dingtalk', 'wecom', 'feishu'];
const TARGETS_MAX = 5;
const NAME_MAX = 40;
const URL_MAX = 2048;
// the rule parameters, whole numbers within these bounds (as the server checks)
const BOUNDS = { forSeconds: [0, 86400], below: [1, 99], above: [1, 100] };
// the parameter each rule has besides forSeconds
const EXTRA = { idleCpuLow: 'below', sessionsHigh: 'above' };

function ruleLabel(rule) {
  switch (rule) {
    case 'instanceDown': return t('alerts.rule.instanceDown');
    case 'gatewayDown': return t('alerts.rule.gatewayDown');
    case 'idleCpuLow': return t('alerts.rule.idleCpuLow');
    case 'sessionsHigh': return t('alerts.rule.sessionsHigh');
    default: return cellText(rule);
  }
}

function ruleText(rule) {
  switch (rule) {
    case 'instanceDown': return t('alerts.rule.instanceDown.text');
    case 'gatewayDown': return t('alerts.rule.gatewayDown.text');
    case 'idleCpuLow': return t('alerts.rule.idleCpuLow.text');
    case 'sessionsHigh': return t('alerts.rule.sessionsHigh.text');
    default: return '';
  }
}

function ruleTone(rule) {
  return LOAD_RULES.includes(rule) ? 'warn' : 'bad';
}

function formatLabel(f) {
  switch (f) {
    case 'slack': return t('alerts.fmt.slack');
    case 'dingtalk': return t('alerts.fmt.dingtalk');
    case 'wecom': return t('alerts.fmt.wecom');
    case 'feishu': return t('alerts.fmt.feishu');
    case 'json': return t('alerts.fmt.json');
    default: return cellText(f);
  }
}

function formatHint(f) {
  if (f === 'slack') return t('alerts.wh.hint.slack');
  if (ROBOTS.includes(f)) return t('alerts.wh.hint.robot');
  return t('alerts.wh.hint.json');
}

function langLabel(code) {
  const l = LANGS.find((x) => x.code === code);
  return l ? l.label : cellText(code);
}

// pct shows a percentage with one decimal only when it has one.
function pct(v) {
  const n = Number(v) || 0;
  return fmtPct(n, Number.isInteger(n) ? 0 : 1);
}

function elapsed(ms) {
  return fmtDuration((Date.now() - Number(ms)) / 1000);
}

function ago(ms) {
  return t('hangups.ago', { duration: elapsed(ms) });
}

function lasting(ms) {
  return t('alerts.for', { duration: elapsed(ms) });
}

function http(status) {
  return t('alerts.http', { status: String(status) });
}

// timeCell is the clock with seconds and a second line that the page's
// ticker keeps current (data-ago: "… ago", data-for: "for …").
function timeCell(ms, kind) {
  const n = Number(ms) || 0;
  if (!n) return '';
  return h('span', { title: fmtDateTime(n) },
    h('span', { class: 'num', text: fmtClock(n, true) }),
    h('span', { class: 'sub', ['data-' + kind]: String(n), text: kind === 'for' ? lasting(n) : ago(n) }));
}

// detailParts is the one-line description of an alert, by rule.
function detailParts(r) {
  switch (r.rule) {
    case 'instanceDown': {
      const reason = cellText(r.reason);
      const hint = connectionHint(reason);
      const el = clip(hint || reason || t('alerts.detail.notConnected'));
      if (hint) el.title = hint + '\n' + reason;
      return [el];
    }
    case 'gatewayDown': {
      const state = cellText(r.state);
      return [
        h('span', { class: 'cell-strong', text: cellText(r.subject) }),
        state ? tag(state) : null,
        cellText(r.ping).toUpperCase() === 'DOWN' ? h('span', { class: 'muted', text: t('sipev.ping', { status: 'DOWN' }) }) : null,
      ];
    }
    case 'idleCpuLow':
      return [h('span', { text: t('alerts.detail.cpu', { value: pct(r.value), threshold: pct(r.threshold) }) })];
    case 'sessionsHigh':
      return [h('span', { text: t('alerts.detail.sessions', { sessions: fmtInt(r.sessions), limit: fmtInt(r.limit), value: pct(r.value), threshold: pct(r.threshold) }) })];
    default:
      return [];
  }
}

function detailCell(r, history) {
  const parts = detailParts(r);
  if (history && r.event === 'resolved') parts.push(h('span', { class: 'muted', text: '· ' + t('alerts.after', { duration: fmtDuration((Number(r.durationMs) || 0) / 1000) }) }));
  return h('span', { class: 'al-detail' }, parts);
}

// notified sums up the deliveries of a history entry; the title lists the
// failed ones.
function notified(r) {
  const list = Array.isArray(r.deliveries) ? r.deliveries.filter(Boolean) : [];
  if (!list.length) return h('span', { class: 'faint', text: DASH });
  const failed = list.filter((d) => !d.ok);
  if (!failed.length) return h('span', { class: 'text-ok', text: t('alerts.sent', { n: fmtInt(list.length) }) });
  return h('span', {
    class: 'text-bad',
    text: t('alerts.failed', { failed: fmtInt(failed.length), n: fmtInt(list.length) }),
    title: failed.map((d) => cellText(d.target) + ': ' + (cellText(d.error) || (Number(d.status) ? http(d.status) : DASH))).join('\n'),
  });
}

// the fields the history search looks at
function matches(r, q) {
  return !q || [r.instance, r.subject, ruleLabel(r.rule), r.state, r.reason]
    .some((v) => cellText(v).toLowerCase().includes(q));
}

const HISTORY_FILTERS = {
  all: () => true,
  firing: (r) => r.event === 'firing',
  resolved: (r) => r.event === 'resolved',
};

export function mountAlerts({ scope, root, ctx }) {
  document.title = t('alerts.docTitle');

  // ---- active ----

  const activePanel = panel({ title: t('alerts.active'), flush: true });
  const activeHost = tableHost({ rows: 2, cols: 4 });
  activePanel.body.append(activeHost.el);
  const activeSort = sortState(() => render());
  activeSort.col = 0; // oldest first

  const instanceLink = (r) => h('a', {
    class: 'cell-strong',
    href: instHash(cellText(r.instanceId), r.rule === 'gatewayDown' ? 'sip' : 'overview'),
    text: cellText(r.instance),
  });

  const activeColumns = [
    { label: t('alerts.col.since'), sortBy: (r) => Number(r.since) || 0, render: (r) => timeCell(r.since, 'for') },
    { label: t('alerts.col.instance'), sortBy: (r) => cellText(r.instance), render: instanceLink },
    { label: t('alerts.col.alert'), sortBy: (r) => ruleLabel(r.rule), render: (r) => pill(ruleLabel(r.rule), ruleTone(r.rule)) },
    { label: t('alerts.col.detail'), render: (r) => detailCell(r, false) },
  ];

  // ---- history ----

  let filter = 'all';
  let query = '';
  const search = searchBox(t('alerts.history.filter'), (q) => {
    query = q;
    render();
  }, 'sm');
  const filterSeg = segmented([
    { key: 'all', label: t('common.all') },
    { key: 'firing', label: t('alerts.ev.fired') },
    { key: 'resolved', label: t('alerts.ev.resolved') },
  ], filter, (k) => {
    filter = k;
    render();
  }, 'sm');
  const historyPanel = panel({ title: t('alerts.history'), flush: true, actions: [search.el, filterSeg.el] });
  historyPanel.el.classList.add('al-hist');
  const historyHost = tableHost({ rows: 6, cols: 5 });
  historyHost.el.classList.add('al-hist-tbl');
  historyPanel.body.append(h('p', { class: 'hu-note', text: t('alerts.history.note') }), historyHost.el);
  const historySort = sortState(() => render());

  const historyColumns = [
    { label: t('alerts.col.time'), sortBy: (r) => Number(r.t) || 0, render: (r) => timeCell(r.t, 'ago') },
    {
      label: t('alerts.col.event'), sortBy: (r) => cellText(r.event),
      render: (r) => (r.event === 'resolved' ? pill(t('alerts.ev.resolved'), 'ok') : pill(t('alerts.ev.fired'), ruleTone(r.rule))),
    },
    { label: t('alerts.col.instance'), sortBy: (r) => cellText(r.instance), render: (r) => h('span', { class: 'cell-strong', text: cellText(r.instance) }) },
    { label: t('alerts.col.alert'), sortBy: (r) => ruleLabel(r.rule), render: (r) => ruleLabel(r.rule) },
    { label: t('alerts.col.detail'), render: (r) => detailCell(r, true) },
    { label: t('alerts.col.notified'), render: notified },
  ];

  // ---- page ----

  const stack = h('div', { class: 'stack' }, activePanel.el, historyPanel.el);
  root.replaceChildren(
    h('header', { class: 'masthead' },
      h('div', { class: 'masthead-in solo' },
        h('div', { class: 'page-head' }, h('div', { class: 'spacer' },
          h('h1', { class: 'page-title', text: t('alerts.title') }),
          h('p', { class: 'page-sub', text: t('alerts.sub') }))))),
    h('div', { class: 'page-body' }, stack));
  // the panel exists only where the server allows the settings
  if (ctx.can('manage')) stack.append(notifications({ scope, ctx }));

  let data = null; // {active, history}
  let lastError = null;
  let activeSig = '';
  let historySig = '';

  function render() {
    if (lastError) {
      activeSig = '';
      historySig = '';
      activePanel.setCount(null);
      historyPanel.setCount(null);
      for (const k of Object.keys(HISTORY_FILTERS)) filterSeg.setCount(k, null);
      activeHost.error(lastError);
      historyHost.error(lastError);
      return;
    }
    if (!data) return;
    activePanel.setCount(data.active.length);
    // An unchanged table is left alone so hover, focus and scrolling survive.
    const aSig = JSON.stringify([activeSort.col, activeSort.dir, data.active]);
    if (aSig !== activeSig) {
      activeSig = aSig;
      activeHost.show(dataTable({
        rows: data.active,
        columns: activeColumns,
        sort: activeSort,
        empty: emptyState({ icon: 'check', tone: 'ok', title: t('alerts.active.none.title'), text: t('alerts.active.none.text') }),
      }));
    }
    const all = data.history;
    for (const [k, fn] of Object.entries(HISTORY_FILTERS)) filterSeg.setCount(k, fmtInt(all.filter(fn).length));
    const rows = all.filter((r) => HISTORY_FILTERS[filter](r) && matches(r, query));
    historyPanel.setCount(rows.length === all.length ? all.length : t('common.nOfM', { n: fmtInt(rows.length), m: fmtInt(all.length) }));
    const hSig = JSON.stringify([filter, query, historySort.col, historySort.dir, all]);
    if (hSig === historySig) return;
    historySig = hSig;
    historyHost.show(dataTable({
      rows,
      columns: historyColumns,
      sort: historySort,
      empty: all.length
        ? emptyState({ icon: 'search', title: t('common.noMatch'), text: t('common.noMatchText') })
        : emptyState({ icon: 'history', title: t('alerts.history.none.title'), text: t('alerts.history.none.text') }),
    }));
  }

  async function load(signal) {
    try {
      const res = await api('GET', 'alerts', { signal });
      data = {
        active: Array.isArray(res && res.active) ? res.active : [],
        history: Array.isArray(res && res.history) ? res.history : [],
      };
      lastError = null;
    } catch (err) {
      if (isAbort(err)) throw err;
      lastError = err;
    }
    render();
  }

  const tick = setInterval(() => {
    for (const el of root.querySelectorAll('[data-ago]')) el.textContent = ago(el.dataset.ago);
    for (const el of root.querySelectorAll('[data-for]')) el.textContent = lasting(el.dataset.for);
  }, 1000);
  scope.onCleanup(() => clearInterval(tick));
  scope.poll(load, 5000);
}

// ---------- notification settings (administrators) ----------

function field(id, label, input, note) {
  return h('div', { class: 'field' }, h('label', { for: id, text: label }), input, note || null);
}

function charCount(s) {
  return Array.from(String(s)).length;
}

function urlOk(v) {
  if (v.length > URL_MAX) return false;
  try {
    const u = new URL(v);
    return (u.protocol === 'http:' || u.protocol === 'https:') && Boolean(u.host);
  } catch {
    return false;
  }
}

// rulesBody keeps exactly the fields the server accepts for each rule.
function rulesBody(rules) {
  const out = {};
  for (const key of RULES) {
    const r = rules[key];
    out[key] = { enabled: Boolean(r.enabled), forSeconds: Number(r.forSeconds) || 0 };
    if (EXTRA[key]) out[key][EXTRA[key]] = Number(r[EXTRA[key]]) || 0;
  }
  return out;
}

// targetsBody is the full list of targets, without urlHint (the server
// keeps the URL of every known id).
function targetsBody(targets) {
  return targets.map((x) => ({ id: x.id, name: x.name, format: x.format, language: x.language, enabled: x.enabled }));
}

function normalize(s) {
  const rules = {};
  const src = (s && s.rules) || {};
  for (const key of RULES) {
    const r = src[key] || {};
    rules[key] = { enabled: r.enabled !== false, forSeconds: Number(r.forSeconds) || 0 };
    if (EXTRA[key]) rules[key][EXTRA[key]] = Number(r[EXTRA[key]]) || 0;
  }
  const targets = (Array.isArray(s && s.targets) ? s.targets : []).filter(Boolean).map((x) => ({
    id: cellText(x.id), name: cellText(x.name), format: cellText(x.format), language: cellText(x.language), enabled: Boolean(x.enabled), urlHint: cellText(x.urlHint),
  }));
  return { gatewayEvents: !(s && s.gatewayEvents === false), rules, targets };
}

// notifications is the settings panel: the rules (saved together) and the
// webhooks (each change saved at once). In the demo it is read-only.
function notifications({ scope, ctx }) {
  const p = panel({ title: t('alerts.notify.title'), flush: true });
  p.el.classList.add('al-notify');
  p.body.append(skeleton({ rows: 4, cols: 3 }));

  let saved = null; // the settings as the server has them
  let demo = false;
  let rows = []; // the rule rows
  let checkRules = () => {};
  let renderTargets = () => {};

  Promise.all([api('GET', 'alerts/settings', { signal: scope.signal }), ctx.isDemo()]).then(([s, d]) => {
    if (scope.disposed) return;
    saved = normalize(s);
    demo = d;
    build();
  }, (err) => {
    if (isAbort(err) || scope.disposed) return;
    if (err.status === 403) {
      p.el.remove();
      return;
    }
    p.body.replaceChildren(errorState(err));
  });

  // put saves the whole settings object and takes the server's answer as
  // the new baseline; rule edits not yet saved stay in their inputs.
  async function put(body) {
    const res = await api('PUT', 'alerts/settings', { body, signal: scope.signal });
    if (res && typeof res === 'object') saved = normalize(res);
    renderTargets();
    checkRules();
  }

  function build() {
    // -- rules --
    const rulesError = h('div', { class: 'form-error', role: 'alert', hidden: true });
    const saveBtn = h('button', { type: 'button', class: 'btn btn-primary', text: t('alerts.rules.save'), disabled: true });
    rows = RULES.map((key) => ruleRow(key));
    let serverError = '';
    checkRules = () => {
      let valid = true;
      let dirty = false;
      for (const r of rows) {
        valid = r.check() && valid;
        dirty = dirty || r.dirty();
      }
      if (!valid) {
        rulesError.textContent = t('alerts.rules.bounds');
        rulesError.hidden = false;
      } else {
        rulesError.textContent = serverError;
        rulesError.hidden = !serverError;
      }
      saveBtn.disabled = demo || !valid || !dirty;
    };
    for (const r of rows) {
      r.onChange(() => {
        serverError = '';
        checkRules();
      });
    }
    saveBtn.addEventListener('click', async () => {
      if (saveBtn.disabled) return;
      saveBtn.disabled = true;
      serverError = '';
      const rules = {};
      for (const r of rows) rules[r.key] = r.value();
      try {
        await put({ rules: rulesBody(rules), targets: targetsBody(saved.targets) });
        toast(t('alerts.rules.saved'));
      } catch (err) {
        if (isAbort(err)) return;
        serverError = err.message;
      }
      checkRules();
    });
    const rulesBlock = h('div', { class: 'al-block' },
      h('h3', { class: 'al-block-title', text: t('alerts.rules.title') }),
      h('div', { class: 'al-rules' }, rows.map((r) => r.el)),
      rulesError,
      h('div', { class: 'al-block-foot' }, h('p', { class: 'al-note', text: t('alerts.rules.note') }), saveBtn));

    // -- webhooks --
    const addBtn = h('button', { type: 'button', class: 'btn btn-sm', on: { click: () => webhookDialog(null) } }, icon('plus'), t('alerts.wh.add'));
    // a disabled button shows no tooltip in every browser; the wrapper does
    const addWrap = h('span', null, addBtn);
    const host = h('div', { class: 'tbl-wrap no-clip' });
    renderTargets = () => {
      const full = saved.targets.length >= TARGETS_MAX;
      addBtn.disabled = demo || full;
      addWrap.title = full ? t('alerts.wh.max') : '';
      host.replaceChildren(saved.targets.length
        ? dataTable({ rows: saved.targets, columns: targetColumns })
        : h('p', { class: 'note-empty', text: t('alerts.wh.none') }));
    };
    const whBlock = h('div', { class: 'al-block al-wh' },
      h('div', { class: 'al-block-head' },
        h('div', { class: 'spacer' },
          h('h3', { class: 'al-block-title', text: t('alerts.wh.title') }),
          h('p', { class: 'al-block-sub', text: t('alerts.wh.sub') })),
        addWrap),
      host);

    // native replaceChildren() would print "null", so the demo note is filtered
    p.body.replaceChildren(...[
      demo ? h('div', { class: 'al-demo' }, h('p', { class: 'note-inline' }, icon('info'), t('alerts.notify.demo'))) : null,
      rulesBlock,
      whBlock,
    ].filter(Boolean));
    renderTargets();
    checkRules();
  }

  // ruleRow is one rule: a switch, the label and its explanation, and the
  // parameters as small number inputs inside their sentence.
  function ruleRow(key) {
    const base = saved.rules[key];
    // without SIP events the gateway rule cannot work: shown, not editable
    const off = key === 'gatewayDown' && !saved.gatewayEvents;
    const locked = demo || off;
    const id = 'al-rule-' + key;
    const sw = h('input', { id, type: 'checkbox', class: 'switch', checked: base.enabled, disabled: locked });
    const inputs = {};
    const num = (name) => {
      const [min, max] = BOUNDS[name];
      const el = h('input', { type: 'number', class: 'input al-num', min, max, step: 1, inputMode: 'numeric', value: String(base[name]), disabled: locked });
      inputs[name] = el;
      return el;
    };
    const params = [];
    if (key === 'idleCpuLow') params.push(h('label', { class: 'al-param' }, tx('alerts.param.below', { n: num('below') })));
    if (key === 'sessionsHigh') params.push(h('label', { class: 'al-param' }, tx('alerts.param.above', { n: num('above') })));
    params.push(h('label', { class: 'al-param' }, tx('alerts.param.for', { n: num('forSeconds') })));
    const el = h('div', { class: off ? 'al-rule off' : 'al-rule' },
      sw,
      h('div', { class: 'al-rule-text' },
        h('label', { class: 'switch-title', for: id, text: ruleLabel(key) }),
        h('span', { class: 'switch-text', text: ruleText(key) }),
        off ? h('span', { class: 'check-note pending' }, icon('info'), t('alerts.rules.needsSip')) : null),
      h('div', { class: 'al-params' }, params));

    const parse = (name) => {
      const v = inputs[name].value.trim();
      const n = Number(v);
      const [min, max] = BOUNDS[name];
      return v !== '' && Number.isInteger(n) && n >= min && n <= max ? n : null;
    };
    return {
      key,
      el,
      onChange(fn) {
        sw.addEventListener('change', fn);
        for (const input of Object.values(inputs)) input.addEventListener('input', fn);
      },
      // value is what the row would save; a locked row keeps the saved values
      value() {
        const cur = saved.rules[key];
        if (off) return { ...cur };
        const out = { enabled: sw.checked };
        for (const name of Object.keys(inputs)) out[name] = parse(name) ?? cur[name];
        return out;
      },
      check() {
        let ok = true;
        for (const [name, input] of Object.entries(inputs)) {
          const good = locked || parse(name) !== null;
          if (good) input.removeAttribute('aria-invalid');
          else input.setAttribute('aria-invalid', 'true');
          ok = ok && good;
        }
        return ok;
      },
      dirty() {
        if (locked) return false;
        const cur = saved.rules[key];
        if (sw.checked !== cur.enabled) return true;
        return Object.keys(inputs).some((name) => parse(name) !== cur[name]);
      },
    };
  }

  const targetColumns = [
    { label: t('alerts.wh.name'), render: (x) => h('span', { class: 'cell-strong', text: x.name }) },
    { label: t('alerts.wh.type'), render: (x) => tag(formatLabel(x.format)) },
    { label: t('alerts.wh.dest'), mono: true, render: (x) => x.urlHint },
    { label: t('alerts.wh.language'), render: (x) => langLabel(x.language) },
    { label: t('alerts.wh.enabled'), render: (x) => (x.enabled ? pill(t('alerts.wh.on'), 'ok') : pill(t('alerts.wh.off'), '')) },
    {
      label: '', right: true,
      render: (x) => {
        const testBtn = h('button', { type: 'button', class: 'btn btn-sm', disabled: demo, text: t('alerts.wh.test') });
        testBtn.addEventListener('click', () => test(x, testBtn));
        return h('span', { class: 'row-actions' }, testBtn, rowMenu(t('sip.op.menu', { name: x.name }), [
          { label: t('alerts.wh.edit'), icon: 'edit', disabled: demo, run: () => webhookDialog(x) },
          { label: t('alerts.wh.remove'), icon: 'trash', danger: true, disabled: demo, run: () => remove(x) },
        ]));
      },
    },
  ];

  async function test(x, btn) {
    btn.disabled = true;
    btn.classList.add('busy');
    try {
      const res = await api('POST', 'alerts/test', { body: { id: x.id }, signal: scope.signal });
      const status = Number(res && res.status) || 0;
      if (res && res.ok) {
        toast(t('alerts.test.ok', { name: x.name, status: String(status) }));
      } else {
        const detail = [status ? http(status) : '', cellText(res && res.detail)].filter(Boolean).join(' · ') || DASH;
        toast(t('alerts.test.failed', { name: x.name, detail }), 'error');
      }
    } catch (err) {
      if (isAbort(err)) return;
      toast(t('alerts.test.failed', { name: x.name, detail: err.message }), 'error');
    } finally {
      btn.disabled = demo;
      btn.classList.remove('busy');
    }
  }

  async function remove(x) {
    const ok = await actionDialog({
      title: t('alerts.wh.removeTitle', { name: x.name }),
      message: t('alerts.wh.removeText', { host: x.urlHint || DASH }),
      confirmLabel: t('alerts.wh.remove'),
      danger: true,
      run: () => put({ rules: rulesBody(saved.rules), targets: targetsBody(saved.targets.filter((y) => y.id !== x.id)) }),
    });
    if (!ok || scope.disposed) return;
    toast(t('alerts.wh.removed', { name: x.name }));
  }

  // webhookDialog adds a webhook (x null) or edits one. Saving sends the
  // whole settings object; the rules go as saved, not as edited on the page.
  function webhookDialog(x) {
    const editing = Boolean(x);
    const name = h('input', { id: 'wh-name', class: 'input', type: 'text', autocomplete: 'off', spellcheck: false, maxLength: NAME_MAX, value: editing ? x.name : '' });
    const nameNote = h('span', { class: 'check-note bad', hidden: true }, icon('info'), t('alerts.wh.nameTaken'));
    const format = h('select', { id: 'wh-format', class: 'select' }, FORMATS.map((f) => h('option', { value: f, text: formatLabel(f) })));
    format.value = editing && FORMATS.includes(x.format) ? x.format : FORMATS[0];
    const hint = h('span', { class: 'hint' });
    const url = h('input', { id: 'wh-url', class: 'input mono', type: 'text', autocomplete: 'off', spellcheck: false, maxLength: URL_MAX, placeholder: 'https://' });
    const urlNote = h('span', { class: 'check-note' });
    const language = h('select', { id: 'wh-lang', class: 'select' }, LANGS.map((l) => h('option', { value: l.code, lang: l.code, text: l.label })));
    const wanted = editing ? x.language : lang();
    language.value = LANGS.some((l) => l.code === wanted) ? wanted : LANGS[0].code;
    const enabled = h('input', { id: 'wh-enabled', type: 'checkbox', class: 'switch', checked: editing ? x.enabled : true });
    const error = h('div', { class: 'form-error', role: 'alert', hidden: true });
    const cancel = h('button', { type: 'button', class: 'btn', text: t('common.cancel') });
    const submit = h('button', { type: 'submit', class: 'btn btn-primary', text: t('alerts.wh.save') });
    submit.setAttribute('form', 'wh-form');
    const form = h('form', { id: 'wh-form', class: 'modal-form', noValidate: true },
      field('wh-name', t('alerts.wh.name'), name, nameNote),
      field('wh-format', t('alerts.wh.type'), format, hint),
      field('wh-url', t('alerts.wh.url'), url, urlNote),
      field('wh-lang', t('alerts.wh.msgLang'), language),
      h('label', { class: 'switch-row', for: 'wh-enabled' }, enabled, h('span', null, h('span', { class: 'switch-title', text: t('alerts.wh.enabled') }))),
      error);
    const m = openModal({ title: editing ? t('alerts.wh.editTitle', { name: x.name }) : t('alerts.wh.add'), body: form, footer: [cancel, submit] });
    cancel.addEventListener('click', m.close);

    function check() {
      hint.textContent = formatHint(format.value);
      const n = name.value.trim();
      const taken = saved.targets.some((y) => y.name === n && (!editing || y.id !== x.id));
      const nameOk = charCount(n) >= 1 && charCount(n) <= NAME_MAX && !taken;
      nameNote.hidden = !taken;
      if (taken) name.setAttribute('aria-invalid', 'true');
      else name.removeAttribute('aria-invalid');
      const u = url.value.trim();
      const uOk = u ? urlOk(u) : editing;
      const bad = Boolean(u) && !uOk;
      urlNote.className = 'check-note' + (bad ? ' bad' : '');
      urlNote.replaceChildren(icon('info'), bad
        ? t('alerts.wh.urlRule')
        : editing && !u ? t('alerts.wh.urlKeep', { urlHint: x.urlHint || DASH }) : t('alerts.wh.urlStored'));
      if (bad) url.setAttribute('aria-invalid', 'true');
      else url.removeAttribute('aria-invalid');
      const changed = !editing || Boolean(u) || n !== x.name || format.value !== x.format || language.value !== x.language || enabled.checked !== x.enabled;
      submit.disabled = !(nameOk && uOk && changed);
    }
    for (const el of [name, url]) el.addEventListener('input', check);
    for (const el of [format, language, enabled]) el.addEventListener('change', check);
    check();

    form.addEventListener('submit', async (ev) => {
      ev.preventDefault();
      if (submit.disabled) return;
      submit.disabled = true;
      error.hidden = true;
      const entry = { name: name.value.trim(), format: format.value, language: language.value, enabled: enabled.checked };
      if (editing) entry.id = x.id;
      if (url.value.trim()) entry.url = url.value.trim();
      const targets = targetsBody(saved.targets);
      const at = editing ? targets.findIndex((y) => y.id === x.id) : -1;
      if (at >= 0) targets[at] = entry;
      else targets.push(entry);
      try {
        await put({ rules: rulesBody(saved.rules), targets });
        m.close();
        toast(editing ? t('alerts.wh.saved', { name: entry.name }) : t('alerts.wh.added', { name: entry.name }));
      } catch (err) {
        if (isAbort(err)) return;
        error.textContent = err.message;
        error.hidden = false;
        check();
      }
    });
    name.focus();
  }

  return p.el;
}
