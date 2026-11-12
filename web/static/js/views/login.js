// Sign-in view.

import { api } from '../api.js';
import { h } from '../dom.js';
import { brandMark, icon } from '../icons.js';
import { Scope } from '../scope.js';

let viewScope = null; // owns the lockout countdown of the mounted form

function seconds(n) {
  return n === 1 ? '1 second' : n + ' seconds';
}

export function mountLogin({ root, onLogin }) {
  if (viewScope) viewScope.dispose();
  const scope = new Scope();
  viewScope = scope;
  const user = h('input', { id: 'login-user', class: 'input', type: 'text', value: 'admin', required: true, autocomplete: 'username', spellcheck: false });
  const pass = h('input', { id: 'login-pass', class: 'input', type: 'password', required: true, autocomplete: 'current-password' });
  const error = h('div', { class: 'form-error', role: 'alert', hidden: true });
  const submit = h('button', { type: 'submit', class: 'btn btn-primary', text: 'Sign in' });
  let busy = false;
  // After too many failures the server refuses this username from this
  // address for a while (429). Signing in as that user is pointless until then;
  // another username is not affected.
  let lock = null; // {user, until}

  function paint() {
    const left = lock ? Math.ceil((lock.until - Date.now()) / 1000) : 0;
    const showing = error.className === 'form-error warn';
    if (lock && left <= 0) {
      lock = null;
      if (showing) error.hidden = true;
    }
    const blocked = Boolean(lock) && user.value.trim() === lock.user;
    submit.disabled = busy || blocked;
    submit.textContent = blocked ? 'Try again in ' + left + 's' : 'Sign in';
    if (lock && showing) {
      error.replaceChildren(icon('clock'), h('span', { text: 'Too many failed sign-in attempts for “' + lock.user + '”. Wait ' + seconds(left) + ', then try again.' }));
    }
  }

  function tick() {
    paint();
    if (lock) scope.later(tick, 1000);
  }
  user.addEventListener('input', paint);

  const form = h('form', { class: 'login-form' },
    h('div', { class: 'field' }, h('label', { for: 'login-user', text: 'Username' }), user,
      h('span', { class: 'hint', text: 'admin, or viewer for read-only access' })),
    h('div', { class: 'field' }, h('label', { for: 'login-pass', text: 'Password' }), pass),
    error,
    submit);

  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    if (submit.disabled) return;
    const name = user.value.trim();
    busy = true;
    paint();
    error.hidden = true;
    try {
      const u = await api('POST', 'login', { body: { username: name, password: pass.value } });
      if (!u || !u.role) throw new Error('Unexpected answer from the server.');
      scope.dispose();
      onLogin(u);
      return;
    } catch (err) {
      if (scope.disposed) return;
      if (err.status === 429) {
        // "too many failed attempts; try again in 42s"
        const m = /(\d+)\s*s\b/.exec(err.message || '');
        const wasLocked = Boolean(lock);
        lock = { user: name, until: Date.now() + (m ? Number(m[1]) : 60) * 1000 };
        error.className = 'form-error warn';
        error.hidden = false;
        if (!wasLocked) tick();
      } else {
        error.className = 'form-error';
        error.textContent = err.status === 401 ? 'Wrong username or password.' : err.message;
        error.hidden = false;
      }
      pass.select();
    }
    busy = false;
    paint();
  });

  const feature = (name, title, text) => h('div', { class: 'login-feature' },
    h('span', { class: 'login-feature-icon' }, icon(name)), h('div', null, h('b', { text: title }), h('span', { text })));

  root.replaceChildren(h('div', { class: 'login' },
    h('section', { class: 'login-hero', 'aria-hidden': 'true' },
      h('div', { class: 'login-brand' }, brandMark(), 'fsdeck'),
      h('div', { class: 'login-pitch' },
        h('span', { class: 'login-kicker', text: 'Web console for FreeSWITCH' }),
        h('p', { class: 'login-headline', text: 'Operate FreeSWITCH from your browser.' }),
        h('p', { class: 'login-lede', text: 'One place for the live state of every switch: load, SIP, calls, configuration and logs.' }),
        h('div', { class: 'login-features' },
          feature('activity', 'Live load', 'Sessions, call rate and CPU, streamed from the Event Socket.'),
          feature('radio', 'SIP at a glance', 'Profiles, gateways and registrations with their state.'),
          feature('terminal', 'Hands on', 'Hang up channels, tail logs and run API commands.'))),
      h('p', { class: 'login-legal', text: 'Open source · ships as a single binary' })),
    h('main', { class: 'login-main' },
      h('div', { class: 'login-card' },
        h('div', { class: 'login-brand' }, brandMark(), 'fsdeck'),
        h('h1', { class: 'login-title', text: 'Sign in' }),
        h('p', { class: 'login-sub', text: 'Use your fsdeck account to continue.' }),
        form,
        h('p', { class: 'login-foot', text: 'Connects over the Event Socket. Nothing to install on FreeSWITCH.' })))));
  pass.focus();
}
