// Instances list; admin can add, edit and delete.

import { api, isAbort } from '../api.js';
import { h, tableBox, errorBox, okBox, stateView } from '../dom.js';

function address(inst) {
  const host = String(inst.host || '');
  return (host.includes(':') ? '[' + host + ']' : host) + ':' + inst.port;
}

export function mountInstances({ scope, root, isAdmin }) {
  const msg = h('div', { class: 'msg-area' });
  const box = tableBox();
  let list = [];
  let form = null;

  const refresh = scope.poll(async (signal) => {
    try {
      const data = await api('GET', 'instances', { signal });
      list = Array.isArray(data) ? data : [];
      render();
    } catch (err) {
      if (isAbort(err)) throw err;
      box.error(err);
    }
  }, 5000);

  function render() {
    if (!list.length) {
      box.show(h('p', { class: 'empty', text: isAdmin ? 'No instances yet. Add one below.' : 'No instances configured.' }));
      return;
    }
    const head = h('tr', null,
      h('th', { scope: 'col', text: 'Name' }),
      h('th', { scope: 'col', text: 'Address' }),
      h('th', { scope: 'col', text: 'Connection' }),
      isAdmin ? h('th', { scope: 'col', text: 'Actions' }) : null);
    const rows = list.map((inst) => h('tr', null,
      h('td', null, h('a', { href: '#/i/' + encodeURIComponent(inst.id) + '/overview', text: inst.name })),
      h('td', { class: 'mono', text: address(inst) }),
      h('td', { class: 'state-cell' }, stateView(inst.state)),
      isAdmin ? h('td', { class: 'row-actions' },
        h('button', { type: 'button', class: 'small', text: 'Edit', on: { click: () => form.edit(inst) } }),
        h('button', { type: 'button', class: 'small danger', text: 'Delete', on: { click: () => remove(inst) } })) : null));
    box.show(h('table', { class: 'grid wrap' }, h('thead', null, head), h('tbody', null, rows)));
  }

  async function remove(inst) {
    if (!window.confirm('Delete instance "' + inst.name + '" (' + address(inst) + ')?')) return;
    msg.replaceChildren();
    try {
      await api('DELETE', 'instances/' + encodeURIComponent(inst.id), { signal: scope.signal });
      if (form.editing() === inst.id) form.reset();
      msg.replaceChildren(okBox('Deleted "' + inst.name + '".'));
      refresh();
    } catch (err) {
      if (!isAbort(err)) msg.replaceChildren(errorBox(err));
    }
  }

  root.replaceChildren(h('h1', { text: 'Instances' }), msg, box.el);
  if (isAdmin) {
    form = instanceForm(scope, () => {
      msg.replaceChildren();
      refresh();
    });
    root.append(form.el);
  }
}

function instanceForm(scope, onSaved) {
  const fName = h('input', { id: 'f-name', type: 'text', required: true, maxLength: 64, autocomplete: 'off' });
  const fHost = h('input', { id: 'f-host', type: 'text', required: true, autocomplete: 'off', spellcheck: false });
  const fPort = h('input', { id: 'f-port', type: 'number', required: true, min: '1', max: '65535', value: '8021' });
  const fPass = h('input', { id: 'f-pass', type: 'password', autocomplete: 'new-password' });
  const title = h('h2', { text: 'Add instance' });
  const submit = h('button', { type: 'submit', class: 'primary', text: 'Add' });
  const cancel = h('button', { type: 'button', text: 'Cancel', hidden: true });
  const msg = h('div', { class: 'form-msg' });
  let editing = null;

  const field = (id, label, input) => h('div', { class: 'field' }, h('label', { for: id, text: label }), input);
  const form = h('form', { class: 'panel inst-form' },
    title,
    h('div', { class: 'fields' },
      field('f-name', 'Name', fName),
      field('f-host', 'Host', fHost),
      field('f-port', 'Port', fPort),
      field('f-pass', 'ESL password', fPass)),
    msg,
    h('div', { class: 'form-buttons' }, submit, cancel));

  function reset() {
    editing = null;
    form.reset();
    fPort.value = '8021';
    fPass.placeholder = '';
    title.textContent = 'Add instance';
    submit.textContent = 'Add';
    cancel.hidden = true;
  }

  function edit(inst) {
    editing = inst.id;
    fName.value = inst.name || '';
    fHost.value = inst.host || '';
    fPort.value = String(inst.port || '');
    fPass.value = '';
    fPass.placeholder = 'Leave empty to keep the current password';
    title.textContent = 'Edit "' + inst.name + '"';
    submit.textContent = 'Save';
    cancel.hidden = false;
    msg.replaceChildren();
    form.scrollIntoView({ block: 'nearest' });
    fName.focus();
  }

  cancel.addEventListener('click', () => {
    reset();
    msg.replaceChildren();
  });

  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const body = {
      name: fName.value.trim(),
      host: fHost.value.trim(),
      port: Number(fPort.value),
      password: fPass.value,
    };
    submit.disabled = true;
    msg.replaceChildren();
    try {
      const wasEdit = editing !== null;
      if (wasEdit) {
        await api('PUT', 'instances/' + encodeURIComponent(editing), { body, signal: scope.signal });
      } else {
        await api('POST', 'instances', { body, signal: scope.signal });
      }
      reset();
      onSaved();
      msg.replaceChildren(okBox(wasEdit ? 'Saved "' + body.name + '".' : 'Added "' + body.name + '".'));
    } catch (err) {
      if (!isAbort(err)) msg.replaceChildren(errorBox(err));
    } finally {
      submit.disabled = false;
    }
  });

  return { el: form, edit, reset, editing: () => editing };
}
