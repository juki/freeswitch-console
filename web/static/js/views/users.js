// Users (admin): the accounts that can sign in to fsdeck, their roles, and
// the dialogs to add, edit and delete them. Accounts set by the environment
// (FSDECK_ADMIN_PASSWORD, FSDECK_VIEWER_PASSWORD) are listed but read-only,
// and nobody is offered actions the server refuses on their own account.

import { api, isAbort } from '../api.js';
import { h, DASH, fmtDate, fmtDateTime, fmtInt } from '../dom.js';
import { icon } from '../icons.js';
import { plural, t } from '../i18n.js';
import { actionDialog, dataTable, emptyState, errorState, openModal, panel, pill, searchBox, skeleton, sortState, toast } from '../ui.js';
import { PASSWORD_MAX, PASSWORD_MIN, passwordLength, roleLabel } from './account.js';

const USERNAME = /^[a-z0-9][a-z0-9._-]{1,31}$/;
const ROLES = ['admin', 'operator', 'viewer'];
const ROLE_ORDER = { admin: 0, operator: 1, viewer: 2 };

function roleDescription(role) {
  return ({ admin: t('role.desc.admin'), operator: t('role.desc.operator'), viewer: t('role.desc.viewer') })[role] || '';
}

function roleBadge(role) {
  return h('span', { class: 'role-badge r-' + role, text: roleLabel(role) });
}

// rolePicker renders the three roles as radio cards with what each may do.
function rolePicker(name, value, disabled) {
  const inputs = [];
  const el = h('div', { class: 'role-picker', role: 'radiogroup' }, ROLES.map((role) => {
    const input = h('input', { type: 'radio', name, value: role, checked: role === value, disabled: Boolean(disabled) });
    inputs.push(input);
    return h('label', { class: 'role-card' }, input,
      h('span', { class: 'role-card-body' },
        h('span', { class: 'role-card-head' }, roleBadge(role)),
        h('span', { class: 'role-card-text', text: roleDescription(role) })));
  }));
  return { el, value: () => (inputs.find((i) => i.checked) || {}).value };
}

// passwordFields is a new-password input with a live length check, plus a
// confirmation input. optional: an empty password is valid (keep the old one).
function passwordFields(prefix, optional) {
  const pass = h('input', { id: prefix + '-pass', class: 'input', type: 'password', autocomplete: 'new-password' });
  const again = h('input', { id: prefix + '-again', class: 'input', type: 'password', autocomplete: 'new-password' });
  const lenNote = h('span', { class: 'check-note' });
  const matchNote = h('span', { class: 'check-note' });
  function check() {
    const n = passwordLength(pass.value);
    const empty = n === 0 && optional;
    const lenOk = empty || (n >= PASSWORD_MIN && n <= PASSWORD_MAX);
    lenNote.className = 'check-note ' + (empty ? '' : lenOk ? 'ok' : n ? 'pending' : '');
    lenNote.replaceChildren(icon(lenOk && !empty ? 'check' : 'info'), empty
      ? t('users.pw.keep')
      : n > PASSWORD_MAX ? t('account.pw.tooLong', { max: PASSWORD_MAX })
        : lenOk ? t('account.pw.lenOk') : n ? t('account.pw.more', { n: PASSWORD_MIN - n }) : t('account.pw.rule', { min: PASSWORD_MIN, max: PASSWORD_MAX }));
    const matchOk = again.value === pass.value;
    again.disabled = empty;
    matchNote.hidden = empty;
    matchNote.className = 'check-note ' + (!again.value ? '' : matchOk ? 'ok' : 'bad');
    matchNote.replaceChildren(icon(again.value && matchOk ? 'check' : 'info'), !again.value ? t('account.pw.repeat') : matchOk ? t('account.pw.match') : t('account.pw.noMatch'));
    return lenOk && (empty || (matchOk && again.value !== ''));
  }
  return { pass, again, lenNote, matchNote, check, value: () => pass.value };
}

function field(id, label, input, note) {
  return h('div', { class: 'field' }, h('label', { for: id, text: label }), input, note || null);
}

export function mountUsers({ scope, root, ctx }) {
  document.title = t('users.docTitle');
  const me = ctx.me;
  const sub = h('p', { class: 'page-sub' });
  let query = '';
  const search = searchBox(t('users.filter'), (q) => {
    query = q;
    render();
  }, 'sm');
  const addBtn = h('button', { type: 'button', class: 'btn btn-primary', on: { click: () => addDialog() } }, icon('plus'), t('users.add'));
  const p = panel({ title: t('users.accounts'), flush: true, actions: search.el });
  const host = h('div', { class: 'tbl-wrap users-tbl' }, skeleton({ rows: 5, cols: 4 }));
  p.body.append(host);
  const legend = h('section', { class: 'panel role-legend' },
    h('div', { class: 'panel-head' }, h('h2', { class: 'panel-title', text: t('users.roles') })),
    h('div', { class: 'role-legend-grid' }, ROLES.map((role) => h('div', { class: 'role-legend-item' }, roleBadge(role), h('p', { text: roleDescription(role) })))));
  root.replaceChildren(
    h('header', { class: 'masthead' },
      h('div', { class: 'masthead-in solo' },
        h('div', { class: 'crumbs' }, h('span', { text: t('nav.admin') }), icon('chevron'), h('span', { text: t('nav.users') })),
        h('div', { class: 'page-head' }, h('div', { class: 'spacer' }, h('h1', { class: 'page-title', text: t('users.title') }), sub), addBtn))),
    h('div', { class: 'page-body' }, h('div', { class: 'stack' }, p.el, legend)));

  let users = null;
  const sort = sortState(() => render());
  let loadScope = null;

  const isMe = (u) => u.username === me.username;

  function render() {
    if (!users) return;
    const admins = users.filter((u) => u.role === 'admin' && !u.disabled).length;
    sub.textContent = plural(users.length, t('users.sub.one', { n: fmtInt(users.length), admins: fmtInt(admins) }), t('users.sub.other', { n: fmtInt(users.length), admins: fmtInt(admins) }));
    const shown = users.filter((u) => !query || (u.username + ' ' + u.role + ' ' + roleLabel(u.role)).toLowerCase().includes(query));
    p.setCount(query ? t('common.nOfM', { n: fmtInt(shown.length), m: fmtInt(users.length) }) : users.length);
    host.replaceChildren(dataTable({
      rows: shown,
      sort,
      empty: query
        ? emptyState({ icon: 'search', title: t('common.noMatch'), text: t('users.noMatch') })
        : emptyState({ icon: 'users', title: t('users.none'), text: t('users.noneText') }),
      columns: [
        {
          label: t('users.col.user'), sortBy: (u) => u.username,
          render: (u) => h('span', { class: 'user-cell' },
            h('span', { class: 'avatar sm', text: u.username.slice(0, 1) }),
            h('span', { class: 'cell-strong', text: u.username }),
            isMe(u) ? h('span', { class: 'you-tag', text: t('users.you') }) : null),
        },
        { label: t('users.col.role'), sortBy: (u) => ROLE_ORDER[u.role], render: (u) => roleBadge(u.role) },
        {
          label: t('users.col.status'), sortBy: (u) => (u.managed ? 0 : u.disabled ? 2 : 1),
          render: (u) => h('span', { class: 'status-cell' },
            u.disabled ? pill(t('users.status.disabled'), '') : pill(t('users.status.active'), 'ok'),
            u.managed ? h('span', { class: 'managed-tag', title: t('users.managedHint') }, icon('lock'), t('users.status.managed')) : null),
        },
        {
          label: t('users.col.created'), sortBy: (u) => u.createdAt || '',
          render: (u) => (u.createdAt ? h('span', { class: 'num', text: fmtDate(u.createdAt), title: fmtDateTime(u.createdAt) }) : h('span', { class: 'faint', text: u.managed ? t('users.fromEnv') : DASH })),
        },
        { label: '', right: true, render: (u) => actions(u) },
      ],
    }));
  }

  // actions offers edit and delete only where the server would allow them,
  // and says why not otherwise.
  function actions(u) {
    const why = u.managed ? t('users.managedHint') : '';
    const edit = h('button', { type: 'button', class: 'btn btn-sm', disabled: Boolean(why), title: why || t('users.editNamed', { name: u.username }), on: { click: () => editDialog(u) } }, icon('edit'), t('users.edit'));
    const delWhy = why || (isMe(u) ? t('users.selfDelete') : '');
    const del = h('button', {
      type: 'button', class: 'btn btn-sm btn-icon btn-ghost btn-danger', disabled: Boolean(delWhy),
      title: delWhy || t('users.deleteNamed', { name: u.username }), 'aria-label': t('users.deleteNamed', { name: u.username }),
      on: { click: () => remove(u) },
    }, icon('trash'));
    // a disabled button shows no tooltip in every browser; the wrapper does
    return h('span', { class: 'row-actions' }, h('span', { title: why || undefined }, edit), h('span', { title: delWhy || undefined }, del));
  }

  function load() {
    if (loadScope) loadScope.dispose();
    loadScope = scope.child();
    api('GET', 'users', { signal: loadScope.signal }).then((data) => {
      users = Array.isArray(data) ? data : [];
      addBtn.disabled = false;
      render();
    }, (err) => {
      if (isAbort(err)) return;
      users = null;
      sub.textContent = '';
      p.setCount(null);
      if (err.status === 402) {
        // the whole screen is the licence notice
        addBtn.hidden = true;
        p.el.hidden = true;
        legend.replaceWith(errorState(err));
        return;
      }
      host.replaceChildren(errorState(err));
    });
  }
  addBtn.disabled = true;
  load();

  async function remove(u) {
    const ok = await actionDialog({
      title: t('users.del.title', { name: u.username }),
      message: t('users.del.text', { name: u.username }),
      confirmLabel: t('users.del.confirm'),
      danger: true,
      run: () => api('DELETE', 'users/' + encodeURIComponent(u.id), { signal: scope.signal }),
    });
    if (!ok || scope.disposed) return;
    toast(t('users.del.done', { name: u.username }));
    load();
  }

  function addDialog() {
    const name = h('input', { id: 'u-name', class: 'input mono', type: 'text', autocomplete: 'off', spellcheck: false, maxLength: 32, placeholder: 'jane.doe' });
    const nameNote = h('span', { class: 'check-note' });
    const roles = rolePicker('u-role', 'viewer');
    const pw = passwordFields('u', false);
    const error = h('div', { class: 'form-error', role: 'alert', hidden: true });
    const cancel = h('button', { type: 'button', class: 'btn', text: t('common.cancel') });
    const submit = h('button', { type: 'submit', class: 'btn btn-primary', text: t('users.add') });
    submit.setAttribute('form', 'u-form');
    const form = h('form', { id: 'u-form', class: 'modal-form', noValidate: true },
      field('u-name', t('users.form.username'), name, nameNote),
      h('div', { class: 'field' }, h('span', { class: 'field-label', text: t('users.form.role') }), roles.el),
      h('div', { class: 'form-row even' }, field('u-pass', t('users.form.password'), pw.pass, pw.lenNote), field('u-again', t('account.pw.again'), pw.again, pw.matchNote)),
      error);
    const m = openModal({ title: t('users.add'), body: form, footer: [cancel, submit] });
    m.dialog.classList.add('wide');
    cancel.addEventListener('click', m.close);
    const taken = () => (users || []).some((u) => u.username === name.value.trim());
    function check() {
      const v = name.value.trim();
      const okName = USERNAME.test(v) && !taken();
      nameNote.className = 'check-note ' + (okName ? 'ok' : v ? 'bad' : '');
      nameNote.replaceChildren(icon(okName ? 'check' : 'info'), !v ? t('users.form.nameRule') : taken() ? t('users.form.nameTaken') : okName ? t('users.form.nameOk') : t('users.form.nameRule'));
      const okPw = pw.check();
      submit.disabled = !(okName && okPw);
    }
    for (const el of [name, pw.pass, pw.again]) el.addEventListener('input', check);
    check();
    form.addEventListener('submit', async (ev) => {
      ev.preventDefault();
      if (submit.disabled) return;
      submit.disabled = true;
      error.hidden = true;
      try {
        const u = await api('POST', 'users', { body: { username: name.value.trim(), role: roles.value(), password: pw.value() }, signal: scope.signal });
        m.close();
        toast(t('users.added', { name: (u && u.username) || name.value.trim() }));
        load();
      } catch (err) {
        if (isAbort(err)) return;
        error.textContent = err.message;
        error.hidden = false;
        check();
      }
    });
    name.focus();
  }

  function editDialog(u) {
    const self = isMe(u);
    const roles = rolePicker('e-role', u.role, self);
    const enabled = h('input', { id: 'e-enabled', type: 'checkbox', class: 'switch', checked: !u.disabled, disabled: self });
    const pw = passwordFields('e', true);
    const error = h('div', { class: 'form-error', role: 'alert', hidden: true });
    const cancel = h('button', { type: 'button', class: 'btn', text: t('common.cancel') });
    const submit = h('button', { type: 'submit', class: 'btn btn-primary', text: t('fleet.form.save') });
    submit.setAttribute('form', 'e-form');
    const form = h('form', { id: 'e-form', class: 'modal-form', noValidate: true },
      self ? h('p', { class: 'note-inline' }, icon('info'), t('users.selfNote')) : null,
      h('div', { class: 'field' }, h('span', { class: 'field-label', text: t('users.form.role') }), roles.el),
      h('div', { class: 'field' }, h('span', { class: 'field-label', text: t('users.form.access') }),
        h('label', { class: 'switch-row', for: 'e-enabled' }, enabled,
          h('span', null, h('span', { class: 'switch-title', text: t('users.form.enabled') }), h('span', { class: 'switch-text', text: t('users.form.enabledText') })))),
      h('div', { class: 'form-row even' }, field('e-pass', t('users.form.reset'), pw.pass, pw.lenNote), field('e-again', t('account.pw.again'), pw.again, pw.matchNote)),
      error);
    const m = openModal({ title: t('users.editTitle', { name: u.username }), body: form, footer: [cancel, submit] });
    m.dialog.classList.add('wide');
    cancel.addEventListener('click', m.close);
    function changes() {
      const body = {};
      if (roles.value() !== u.role) body.role = roles.value();
      if (enabled.checked === Boolean(u.disabled)) body.disabled = !enabled.checked;
      if (pw.value()) body.password = pw.value();
      return body;
    }
    function check() {
      const okPw = pw.check();
      submit.disabled = !(okPw && Object.keys(changes()).length);
    }
    for (const el of [pw.pass, pw.again]) el.addEventListener('input', check);
    for (const el of [enabled, ...form.querySelectorAll('input[type=radio]')]) el.addEventListener('change', check);
    check();
    form.addEventListener('submit', async (ev) => {
      ev.preventDefault();
      if (submit.disabled) return;
      submit.disabled = true;
      error.hidden = true;
      try {
        await api('PUT', 'users/' + encodeURIComponent(u.id), { body: changes(), signal: scope.signal });
        m.close();
        toast(t('users.saved', { name: u.username }));
        load();
      } catch (err) {
        if (isAbort(err)) return;
        error.textContent = err.message;
        error.hidden = false;
        check();
      }
    });
  }
}
