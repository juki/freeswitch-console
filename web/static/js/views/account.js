// The account menu of the shell (who is signed in, change password, language,
// theme, about, sign out, version), the change-password dialog and the About
// dialog.

import { api } from '../api.js';
import { h } from '../dom.js';
import { brandMark, icon } from '../icons.js';
import { t } from '../i18n.js';
import { kv, langItems, openModal, popover, toast } from '../ui.js';

export const PASSWORD_MIN = 10;
export const PASSWORD_MAX = 256;

// roleLabel names a role for people; unknown roles show as they are.
export function roleLabel(role) {
  const names = { admin: t('role.admin'), operator: t('role.operator'), viewer: t('role.viewer') };
  return names[role] || String(role || '');
}

// passwordLength counts characters the way people do (code points).
export function passwordLength(s) {
  return Array.from(String(s)).length;
}

const THEMES = [
  { mode: 'auto', icon: 'contrast', label: () => t('theme.short.auto') },
  { mode: 'light', icon: 'sun', label: () => t('theme.short.light') },
  { mode: 'dark', icon: 'moon', label: () => t('theme.short.dark') },
];

// accountMenu is the user chip at the foot of the sidebar and its menu.
export function accountMenu({ me, version, onSignOut }) {
  const chip = h('button', { type: 'button', class: 'account-chip', title: t('account.menu') },
    h('span', { class: 'avatar', text: me.username.slice(0, 1) }),
    h('span', { class: 'who' }, h('span', { class: 'who-name', text: me.username }), h('span', { class: 'who-role', text: roleLabel(me.role) })),
    icon('updown', 'chip-caret'));
  chip.setAttribute('aria-label', t('account.menuFor', { name: me.username }));
  const panel = h('div', { class: 'menu account-menu' });
  const pop = popover(chip, panel, { up: true, align: 'start', label: t('account.menu') });

  const themeButtons = THEMES.map((th) => {
    const b = h('button', {
      type: 'button', class: 'theme-opt', role: 'menuitemradio', 'aria-checked': String(window.fsconsoleTheme.get() === th.mode), title: th.label(),
      on: {
        click: () => {
          window.fsconsoleTheme.set(th.mode);
          for (const x of themeButtons) x.setAttribute('aria-checked', String(x === b));
        },
      },
    }, icon(th.icon), h('span', { text: th.label() }));
    return b;
  });

  const passwordItem = me.managed
    ? h('div', { class: 'menu-item is-off', title: t('account.managedHint') },
      icon('key'), h('span', { class: 'menu-text' }, t('account.password'), h('span', { class: 'menu-sub', text: t('account.managedShort') })))
    : h('button', { type: 'button', class: 'menu-item', role: 'menuitem', on: { click: () => { pop.close(); changePasswordDialog(); } } },
      icon('key'), h('span', { class: 'menu-text', text: t('account.password') }));

  panel.append(
    h('div', { class: 'menu-head' },
      h('span', { class: 'avatar lg', text: me.username.slice(0, 1) }),
      h('div', { class: 'menu-who' },
        h('div', { class: 'menu-name', text: me.username }),
        h('div', { class: 'menu-role' }, h('span', { class: 'role-badge r-' + me.role, text: roleLabel(me.role) }), me.managed ? h('span', { class: 'menu-managed', text: t('account.managedTag') }) : null))),
    passwordItem,
    h('div', { class: 'menu-sep' }),
    h('div', { class: 'menu-label', text: t('lang.choose') }),
    ...langItems(() => pop.close()),
    h('div', { class: 'menu-sep' }),
    h('div', { class: 'menu-label', text: t('account.theme') }),
    h('div', { class: 'theme-opts', role: 'group' }, themeButtons),
    h('div', { class: 'menu-sep' }),
    h('button', { type: 'button', class: 'menu-item', role: 'menuitem', on: { click: () => { pop.close(); aboutDialog({ version }); } } },
      icon('info'), h('span', { class: 'menu-text' }, t('about.title'), h('span', { class: 'menu-sub', text: t('about.menuSub') }))),
    h('button', { type: 'button', class: 'menu-item', role: 'menuitem', on: { click: () => { pop.close(); onSignOut(); } } },
      icon('logout'), h('span', { class: 'menu-text', text: t('nav.signOut') })),
    h('div', { class: 'menu-foot' }, h('span', { text: 'fsconsole' }), version ? h('span', { class: 'mono', text: version }) : null));
  return pop.el;
}

// changePasswordDialog lets a signed-in user replace their own password. The
// rules are checked as the user types; the server's answer is shown in place.
export function changePasswordDialog() {
  const field = (id, label, input, note) => h('div', { class: 'field' }, h('label', { for: id, text: label }), input, note);
  const cur = h('input', { id: 'pw-current', class: 'input', type: 'password', autocomplete: 'current-password', required: true });
  const next = h('input', { id: 'pw-new', class: 'input', type: 'password', autocomplete: 'new-password', required: true });
  const again = h('input', { id: 'pw-again', class: 'input', type: 'password', autocomplete: 'new-password', required: true });
  const curError = h('span', { class: 'field-error', hidden: true });
  const lenNote = h('span', { class: 'check-note' });
  const matchNote = h('span', { class: 'check-note' });
  const error = h('div', { class: 'form-error', role: 'alert', hidden: true });
  const cancel = h('button', { type: 'button', class: 'btn', text: t('common.cancel') });
  const submit = h('button', { type: 'submit', class: 'btn btn-primary', text: t('account.pw.submit') });
  submit.setAttribute('form', 'pw-form');
  const form = h('form', { id: 'pw-form', class: 'modal-form', noValidate: true },
    h('p', { class: 'modal-text', text: t('account.pw.intro') }),
    field('pw-current', t('account.pw.current'), cur, curError),
    field('pw-new', t('account.pw.new'), next, lenNote),
    field('pw-again', t('account.pw.again'), again, matchNote),
    error);
  const m = openModal({ title: t('account.pw.title'), body: form, footer: [cancel, submit] });
  cancel.addEventListener('click', m.close);

  function check() {
    const n = passwordLength(next.value);
    const lenOk = n >= PASSWORD_MIN && n <= PASSWORD_MAX;
    lenNote.className = 'check-note ' + (lenOk ? 'ok' : n ? 'pending' : '');
    lenNote.replaceChildren(icon(lenOk ? 'check' : 'info'), n > PASSWORD_MAX
      ? t('account.pw.tooLong', { max: PASSWORD_MAX })
      : lenOk ? t('account.pw.lenOk') : n ? t('account.pw.more', { n: PASSWORD_MIN - n }) : t('account.pw.rule', { min: PASSWORD_MIN, max: PASSWORD_MAX }));
    const matchOk = lenOk && again.value === next.value;
    matchNote.className = 'check-note ' + (matchOk ? 'ok' : again.value ? 'bad' : '');
    matchNote.replaceChildren(icon(matchOk ? 'check' : 'info'), !again.value ? t('account.pw.repeat') : again.value === next.value ? (lenOk ? t('account.pw.match') : t('account.pw.repeat')) : t('account.pw.noMatch'));
    submit.disabled = !(cur.value && lenOk && matchOk);
  }
  for (const el of [cur, next, again]) el.addEventListener('input', check);
  cur.addEventListener('input', () => {
    curError.hidden = true;
    cur.removeAttribute('aria-invalid');
  });
  check();

  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    if (submit.disabled) return;
    submit.disabled = true;
    error.hidden = true;
    try {
      await api('POST', 'me/password', { body: { current: cur.value, new: next.value } });
      m.close();
      toast(t('account.pw.done'));
    } catch (err) {
      if (err.status === 400 && /current password/i.test(err.message)) {
        curError.textContent = t('account.pw.wrongCurrent');
        curError.hidden = false;
        cur.setAttribute('aria-invalid', 'true');
        cur.select();
      } else {
        error.textContent = err.message;
        error.hidden = false;
      }
      check();
    }
  });
  cur.focus();
}

// ---------- about ----------

// SOURCE_URL is where the source code of fsconsole is published. The AGPL asks
// that users of the console be offered it, so the About dialog links to it.
const SOURCE_URL = 'https://github.com/juki/freeswitch-console';

// aboutDialog shows what fsconsole is, its version, the terms it is published
// under and where its source code is.
export function aboutDialog({ version }) {
  const close = h('button', { type: 'button', class: 'btn btn-primary', text: t('common.close') });
  const m = openModal({
    title: t('about.title'),
    body: [
      h('div', { class: 'about-head' },
        brandMark(),
        h('div', { class: 'about-who' }, h('div', { class: 'about-name', text: 'FreeSWITCH Console' }), h('p', { class: 'modal-text', text: t('about.text') }))),
      kv([
        [t('about.k.version'), version ? h('span', { class: 'mono', text: version }) : ''],
        [t('about.k.licence'), 'GNU AGPL-3.0'],
        [t('about.k.source'), h('a', { href: SOURCE_URL, target: '_blank', rel: 'noopener noreferrer', text: SOURCE_URL })],
      ]),
    ],
    footer: [close],
  });
  close.addEventListener('click', m.close);
  close.focus();
}
