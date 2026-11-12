import { api } from '../api.js';
import { h } from '../dom.js';

export function mountLogin({ root, onLogin }) {
  const user = h('input', { id: 'login-user', type: 'text', name: 'username', autocomplete: 'username', required: true, spellcheck: false });
  const pass = h('input', { id: 'login-pass', type: 'password', name: 'password', autocomplete: 'current-password', required: true });
  const btn = h('button', { type: 'submit', class: 'primary', text: 'Sign in' });
  const msg = h('div', { class: 'form-msg' });

  const form = h('form', { class: 'login-form' },
    h('label', { for: 'login-user', text: 'Username' }), user,
    h('label', { for: 'login-pass', text: 'Password' }), pass,
    msg, btn);

  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    btn.disabled = true;
    msg.replaceChildren();
    try {
      const me = await api('POST', 'login', { body: { username: user.value.trim(), password: pass.value } });
      onLogin(me);
    } catch (err) {
      const text = err.status === 401 ? 'Invalid username or password.' : err.message;
      msg.replaceChildren(h('div', { class: 'notice error', role: 'alert', text }));
      pass.value = '';
      pass.focus();
    } finally {
      btn.disabled = false;
    }
  });

  root.replaceChildren(h('section', { class: 'login' },
    h('h1', { text: 'fsdeck' }),
    h('p', { class: 'muted', text: 'Sign in as admin or viewer.' }),
    form));
  user.focus();
}
