// Structured readings of the dialplan and directory XML for the config
// browser. The XML is parsed with DOMParser in text/xml mode and only read
// (names, attributes); everything shown is built with h() and textContent.
// Attribute values such as ${destination_number} or regexes stay plain text.
//
// A reader returns {el, filter(q) -> [shown, total]} or null when the XML is
// not what it expects, in which case the browser falls back to the raw XML.

import { h } from '../dom.js';
import { wellFormed } from '../xml.js';
import { icon } from '../icons.js';
import { dataTable, detailBody, emptyState, openDrawer, pill, sortState, tag } from '../ui.js';

function parse(xml, root) {
  if (!wellFormed(xml)) return null;
  let doc;
  try {
    doc = new DOMParser().parseFromString(xml, 'text/xml');
  } catch {
    return null;
  }
  if (!doc || !doc.documentElement || doc.getElementsByTagName('parsererror').length) return null;
  const top = doc.documentElement;
  return top.nodeName === root ? top : top.getElementsByTagName(root)[0] || null;
}

function kids(el, ...names) {
  return [...el.children].filter((c) => names.includes(c.nodeName));
}

function attr(el, name) {
  return el.getAttribute(name) || '';
}

function plural(n, one, many) {
  return n + ' ' + (n === 1 ? one : many || one + 's');
}

// ---------- dialplan ----------

const COND_SKIP = new Set(['field', 'expression', 'break', 'regex', 'require-nested']);

// condition renders one <condition>: what it tests, then its ordered actions
// and anti-actions. Nested conditions are indented below their parent.
function condition(c, words) {
  const field = attr(c, 'field');
  const expr = attr(c, 'expression');
  const regexes = kids(c, 'regex');
  // time-of-day and other attribute tests: wday="2-6" hour="9-18"
  const other = [...c.attributes].filter((a) => !COND_SKIP.has(a.name));
  const test = h('div', { class: 'dp-if' }, h('span', { class: 'dp-kw', text: 'if' }));
  if (field || expr) {
    test.append(h('span', { class: 'dp-field', text: field || '(no field)' }), h('span', { class: 'dp-op', text: 'matches' }), h('code', { class: 'dp-expr', text: expr || '(empty)' }));
    words.push(field, expr);
  }
  if (regexes.length) {
    const mode = attr(c, 'regex') || 'all';
    test.append(h('span', { class: 'dp-op', text: (mode === 'any' ? 'any' : mode === 'xor' ? 'exactly one' : 'all') + ' of' }));
    const list = h('div', { class: 'dp-regexes' });
    for (const r of regexes) {
      list.append(h('div', { class: 'dp-regex' }, h('span', { class: 'dp-field', text: attr(r, 'field') }), h('span', { class: 'dp-op', text: 'matches' }), h('code', { class: 'dp-expr', text: attr(r, 'expression') })));
      words.push(attr(r, 'field'), attr(r, 'expression'));
    }
    test.append(list);
  }
  for (const a of other) {
    test.append(h('code', { class: 'dp-expr', text: a.name + '=' + a.value }));
    words.push(a.name, a.value);
  }
  if (!field && !expr && !regexes.length && !other.length) test.append(h('span', { class: 'dp-op', text: 'always' }));
  if (attr(c, 'break')) test.append(h('span', { class: 'tag', text: 'break ' + attr(c, 'break') }));

  const steps = kids(c, 'action', 'anti-action');
  const list = h('ol', { class: 'dp-actions' }, steps.map((a, i) => {
    const anti = a.nodeName === 'anti-action';
    words.push(attr(a, 'application'), attr(a, 'data'));
    return h('li', { class: anti ? 'dp-act anti' : 'dp-act' },
      h('span', { class: 'dp-step', text: anti ? 'else' : String(i + 1) }),
      h('span', { class: 'dp-app', text: attr(a, 'application') }),
      h('span', { class: 'dp-data', text: attr(a, 'data') }));
  }));
  const nested = kids(c, 'condition').map((n) => h('div', { class: 'dp-nested' }, condition(n, words)));
  return h('div', { class: 'dp-cond' }, test, steps.length ? list : null, nested);
}

export function readDialplan(xml) {
  const ctx = parse(xml, 'context');
  if (!ctx) return null;
  const exts = kids(ctx, 'extension');
  const items = exts.map((ext, i) => {
    const words = [attr(ext, 'name')];
    const conds = kids(ext, 'condition').map((c) => condition(c, words));
    const actions = ext.getElementsByTagName('action').length + ext.getElementsByTagName('anti-action').length;
    const el = h('article', { class: 'dp-ext' },
      h('header', { class: 'dp-head' },
        h('span', { class: 'dp-num', text: String(i + 1) }),
        h('span', { class: 'dp-name', text: attr(ext, 'name') || '(unnamed)' }),
        attr(ext, 'continue') === 'true' ? h('span', { class: 'tag', title: 'Routing continues with the next extension after this one matches', text: 'continue' }) : null,
        h('span', { class: 'spacer' }),
        h('span', { class: 'dp-meta', text: plural(conds.length, 'condition') + ' · ' + plural(actions, 'action') })),
      conds.length ? conds : h('div', { class: 'dp-cond faint', text: 'No conditions.' }));
    return { el, key: words.join(' ').toLowerCase() };
  });
  const none = h('div', { class: 'note-empty', text: 'No extension matches the filter.', hidden: true });
  const el = h('div', { class: 'dp' },
    h('p', { class: 'reader-note' }, icon('info'), 'Extensions are tried top to bottom; the first whose conditions all match runs its actions, unless it is marked continue.'),
    items.length ? items.map((it) => it.el) : emptyState({ icon: 'calls', title: 'No extensions', text: 'This context has no extensions.' }),
    none);
  return {
    el,
    filter(q) {
      let shown = 0;
      for (const it of items) {
        it.el.hidden = Boolean(q) && !it.key.includes(q);
        if (!it.el.hidden) shown++;
      }
      none.hidden = !items.length || shown > 0;
      return [shown, items.length];
    },
  };
}

// ---------- directory ----------

function settings(el) {
  const out = {};
  for (const p of el ? kids(el, 'param', 'variable') : []) out[attr(p, 'name')] = attr(p, 'value');
  return out;
}

export function readDirectory(xml) {
  const domain = parse(xml, 'domain');
  if (!domain) return null;
  const users = new Map(); // id -> record
  let pointers = 0;
  const groups = new Set();
  for (const u of domain.getElementsByTagName('user')) {
    const id = attr(u, 'id');
    if (!id) continue;
    let group = '';
    for (let p = u.parentNode; p && p !== domain; p = p.parentNode) {
      if (p.nodeName === 'group') {
        group = attr(p, 'name');
        break;
      }
    }
    if (group) groups.add(group);
    const rec = users.get(id) || { id, groups: [], params: {}, vars: {}, defined: false };
    users.set(id, rec);
    if (group && !rec.groups.includes(group)) rec.groups.push(group);
    if (attr(u, 'type') === 'pointer') {
      pointers++;
      continue;
    }
    rec.defined = true;
    rec.params = { ...rec.params, ...settings(kids(u, 'params')[0]) };
    rec.vars = { ...rec.vars, ...settings(kids(u, 'variables')[0]) };
  }
  const rows = [...users.values()].map((r) => ({
    ...r,
    callerName: r.vars.effective_caller_id_name || '',
    callerNum: r.vars.effective_caller_id_number || '',
    context: r.vars.user_context || '',
    callgroup: r.vars.callgroup || '',
    toll: r.vars.toll_allow || '',
    // the value is masked by the server; only whether one is set matters here
    secret: 'password' in r.params || 'a1-hash' in r.params,
  }));
  rows.forEach((r) => {
    r.key = [r.id, r.callerName, r.callerNum, r.context, r.callgroup, r.toll, ...r.groups].join(' ').toLowerCase();
  });

  const domainSettings = { ...settings(kids(domain, 'params')[0]), ...settings(kids(domain, 'variables')[0]) };
  const settingNames = Object.keys(domainSettings);
  const summary = h('div', { class: 'dir-summary' },
    h('span', null, h('b', { text: String(rows.filter((r) => r.defined).length) }), ' users'),
    h('span', null, h('b', { text: String(groups.size) }), groups.size === 1 ? ' group' : ' groups'),
    pointers ? h('span', null, h('b', { text: String(pointers) }), pointers === 1 ? ' group pointer' : ' group pointers') : null,
    h('span', null, h('b', { text: String(rows.filter((r) => r.defined && !r.secret).length) }), ' without a password'));
  const domainBox = settingNames.length
    ? h('details', { class: 'dir-domain' },
      h('summary', { text: 'Domain parameters and variables · ' + settingNames.length }),
      h('dl', { class: 'kv' }, settingNames.map((k) => h('div', { class: 'kv-row' }, h('dt', { class: 'mono', text: k }), h('dd', { class: 'mono', text: domainSettings[k] })))))
    : null;

  const host = h('div');
  const sort = sortState(() => render());
  let query = '';
  let selected = null;

  function details(r) {
    selected = r.id;
    render();
    const flat = { id: r.id, groups: r.groups.join(', ') };
    for (const [k, v] of Object.entries(r.params)) flat[k] = v;
    for (const [k, v] of Object.entries(r.vars)) flat[k] = v;
    const d = openDrawer({
      kicker: 'Directory user', title: r.id + (r.callerName ? ' · ' + r.callerName : ''), subtitle: r.defined ? 'groups: ' + (r.groups.join(', ') || 'none') : 'only referenced by a group pointer',
      onClose: () => {
        selected = null;
        render();
      },
    });
    d.body.append(...detailBody(flat, ['id', 'groups', 'effective_caller_id_name', 'effective_caller_id_number', 'user_context', 'callgroup', 'toll_allow', 'accountcode', 'password']).filter(Boolean));
  }

  function render() {
    const shown = rows.filter((r) => !query || r.key.includes(query));
    host.replaceChildren(dataTable({
      rows: shown,
      sort,
      keyOf: (r) => r.id,
      selected,
      onRow: details,
      empty: emptyState({ icon: query ? 'search' : 'user', title: query ? 'No match' : 'No users', text: query ? 'No user matches the filter.' : 'This domain defines no users.' }),
      columns: [
        { label: 'User', mono: true, sortBy: (r) => r.id, render: (r) => h('span', { class: 'cell-strong', text: r.id }) },
        { label: 'Caller ID', sortBy: (r) => r.callerName || r.callerNum, render: (r) => (r.callerName || r.callerNum ? h('span', null, h('span', { text: r.callerName || r.callerNum }), r.callerName && r.callerNum ? h('span', { class: 'sub mono', text: r.callerNum }) : null) : '') },
        { label: 'Groups', sortBy: (r) => r.groups.join(','), render: (r) => (r.groups.length ? h('span', { class: 'tags' }, r.groups.map(tag)) : '') },
        { label: 'Context', mono: true, sortBy: (r) => r.context, render: (r) => r.context },
        { label: 'Call group', mono: true, sortBy: (r) => r.callgroup, render: (r) => r.callgroup },
        { label: 'Toll allow', wrap: true, sortBy: (r) => r.toll, render: (r) => (r.toll ? h('span', { class: 'muted', text: r.toll.split(',').join(', ') }) : '') },
        {
          label: 'Password', sortBy: (r) => (r.defined ? (r.secret ? 1 : 0) : 2),
          render: (r) => (!r.defined ? h('span', { class: 'faint', text: 'pointer only' }) : r.secret ? pill('set', '', true) : pill('none', 'warn')),
        },
      ],
    }));
  }
  render();

  return {
    el: h('div', { class: 'dir-view' }, h('div', { class: 'dir-top' }, summary, domainBox), host),
    filter(q) {
      query = q;
      render();
      return [rows.filter((r) => !q || r.key.includes(q)).length, rows.length];
    },
  };
}

