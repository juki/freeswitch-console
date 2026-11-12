// Sign-in view.

import { api } from '../api.js';
import { h } from '../dom.js';
import { brandMark, icon } from '../icons.js';
import { num, plural, t } from '../i18n.js';
import { Scope } from '../scope.js';
import { langMenu } from '../ui.js';

let viewScope = null; // owns the lockout countdown of the mounted form

export function mountLogin({ root, onLogin }) {
  if (viewScope) viewScope.dispose();
  const scope = new Scope();
  viewScope = scope;
  const user = h('input', { id: 'login-user', class: 'input', type: 'text', value: 'admin', required: true, autocomplete: 'username', spellcheck: false });
  const pass = h('input', { id: 'login-pass', class: 'input', type: 'password', required: true, autocomplete: 'current-password' });
  const error = h('div', { class: 'form-error', role: 'alert', hidden: true });
  const submit = h('button', { type: 'submit', class: 'btn btn-primary', text: t('login.submit') });
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
    submit.textContent = blocked ? t('login.retryIn', { s: num(left) }) : t('login.submit');
    if (lock && showing) {
      error.replaceChildren(icon('clock'), h('span', { text: plural(left, t('login.locked.one', { user: lock.user, n: num(left) }), t('login.locked.other', { user: lock.user, n: num(left) })) }));
    }
  }

  function tick() {
    paint();
    if (lock) scope.later(tick, 1000);
  }
  user.addEventListener('input', paint);

  const form = h('form', { class: 'login-form' },
    h('div', { class: 'field' }, h('label', { for: 'login-user', text: t('login.username') }), user,
      h('span', { class: 'hint', text: t('login.userHint') })),
    h('div', { class: 'field' }, h('label', { for: 'login-pass', text: t('login.password') }), pass),
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
      if (!u || !u.role) throw new Error(t('login.unexpected'));
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
        error.textContent = err.status === 401 ? t('login.wrong') : err.message;
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
        h('span', { class: 'login-kicker', text: t('login.kicker') }),
        h('p', { class: 'login-headline', text: t('login.headline') }),
        h('p', { class: 'login-lede', text: t('login.lede') }),
        h('div', { class: 'login-features' },
          feature('activity', t('login.f1.title'), t('login.f1.text')),
          feature('radio', t('login.f2.title'), t('login.f2.text')),
          feature('terminal', t('login.f3.title'), t('login.f3.text')))),
      h('p', { class: 'login-legal', text: t('login.legal') })),
    h('main', { class: 'login-main' },
      h('div', { class: 'login-lang' }, langMenu({ label: true })),
      h('div', { class: 'login-card' },
        h('div', { class: 'login-brand' }, brandMark(), 'fsdeck'),
        h('h1', { class: 'login-title', text: t('login.title') }),
        h('p', { class: 'login-sub', text: t('login.sub') }),
        form,
        h('p', { class: 'login-foot', text: t('login.foot') })))));
  pass.focus();
}
