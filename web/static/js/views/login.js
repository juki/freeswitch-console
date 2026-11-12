// Sign-in view.

import { api } from '../api.js';
import { h } from '../dom.js';
import { brandMark } from '../icons.js';

export function mountLogin({ root, onLogin }) {
  const user = h('input', { id: 'login-user', class: 'input', type: 'text', value: 'admin', required: true, autocomplete: 'username', spellcheck: false });
  const pass = h('input', { id: 'login-pass', class: 'input', type: 'password', required: true, autocomplete: 'current-password' });
  const error = h('div', { class: 'form-error', role: 'alert', hidden: true });
  const submit = h('button', { type: 'submit', class: 'btn btn-primary', text: 'Sign in' });

  const form = h('form', { class: 'login-form' },
    h('div', { class: 'field' }, h('label', { for: 'login-user', text: 'Username' }), user,
      h('span', { class: 'hint', text: 'admin, or viewer for read-only access' })),
    h('div', { class: 'field' }, h('label', { for: 'login-pass', text: 'Password' }), pass),
    error,
    submit);

  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    submit.disabled = true;
    error.hidden = true;
    try {
      const u = await api('POST', 'login', { body: { username: user.value.trim(), password: pass.value } });
      if (!u || !u.role) throw new Error('Unexpected answer from the server.');
      onLogin(u);
    } catch (err) {
      error.textContent = err.status === 401 ? 'Wrong username or password.' : err.message;
      error.hidden = false;
      submit.disabled = false;
      pass.select();
    }
  });

  root.replaceChildren(h('div', { class: 'login' },
    h('div', { class: 'login-card' },
      h('div', { class: 'login-brand' }, brandMark(), 'fsdeck'),
      h('h1', { class: 'login-title', text: 'Sign in' }),
      h('p', { class: 'login-sub', text: 'Operate FreeSWITCH from your browser.' }),
      form,
      h('p', { class: 'login-foot', text: 'Connects over the Event Socket. Nothing to install on FreeSWITCH.' }))));
  pass.focus();
}
