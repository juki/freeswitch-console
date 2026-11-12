// Console (admin): run FreeSWITCH API commands in a terminal pane.

import { api, instPath, isAbort } from '../api.js';
import { h } from '../dom.js';
import { icon } from '../icons.js';
import { t } from '../i18n.js';

const HISTORY_MAX = 50;
const history = []; // kept in memory for the page lifetime
const QUICK = ['status', 'version', 'sofia status', 'show channels', 'show calls', 'show registrations', 'reloadxml', 'reloadacl'];

export function mountConsole({ id, scope, root }) {
  const hint = h('div', { class: 'term-empty' }, icon('terminal'), h('span', { text: t('con.hint') }));
  const out = h('div', { class: 'term-body', role: 'log', tabIndex: 0 }, hint);
  const input = h('input', {
    class: 'con-input', type: 'text', autocomplete: 'off', spellcheck: false,
    'aria-label': t('con.command'), placeholder: 'status',
  });
  const runBtn = h('button', { type: 'submit', class: 'btn btn-sm', text: t('con.run') });
  const chips = QUICK.map((cmd) => h('button', { type: 'button', class: 'chip', text: cmd, on: { click: () => run(cmd) } }));
  const clearBtn = h('button', { type: 'button', class: 'btn btn-sm' }, icon('eraser'), t('common.clear'));
  const form = h('form', { class: 'con-form' }, h('span', { class: 'con-prompt', text: 'fs>' }), input, runBtn);

  root.append(h('div', { class: 'term console' },
    h('div', { class: 'term-bar' },
      h('span', { class: 'term-title' }, icon('terminal'), t('con.title')),
      h('span', { class: 'term-sep' }),
      h('div', { class: 'chips' }, chips),
      h('span', { class: 'spacer' }), clearBtn),
    out,
    form));

  let busy = false;
  let pos = history.length;
  let draft = '';

  function setBusy(b) {
    busy = b;
    input.disabled = b;
    runBtn.disabled = b;
    for (const c of chips) c.disabled = b;
    if (!b) input.focus();
  }

  function print(cls, text) {
    hint.remove();
    const follow = out.scrollTop + out.clientHeight >= out.scrollHeight - 24;
    out.append(h('div', { class: 'con-line ' + cls, text }));
    if (follow) out.scrollTop = out.scrollHeight;
  }

  async function run(cmd) {
    if (busy) return;
    if (history[history.length - 1] !== cmd) history.push(cmd);
    if (history.length > HISTORY_MAX) history.splice(0, history.length - HISTORY_MAX);
    pos = history.length;
    draft = '';
    setBusy(true);
    print('con-cmd', cmd);
    try {
      const data = await api('POST', instPath(id, 'console'), { body: { command: cmd }, signal: scope.signal });
      const text = data && typeof data.output === 'string' ? data.output.replace(/\s+$/, '') : '';
      print(/^-ERR/.test(text) ? 'con-err' : 'con-out', text || t('con.noOutput'));
    } catch (err) {
      if (isAbort(err)) return;
      print('con-err', err.status === 503 ? t('con.notConnected', { msg: err.message }) : t('con.error', { msg: err.message }));
    } finally {
      if (!scope.disposed) setBusy(false);
    }
  }

  form.addEventListener('submit', (ev) => {
    ev.preventDefault();
    const cmd = input.value.trim();
    if (!cmd || busy) return;
    input.value = '';
    run(cmd);
  });

  input.addEventListener('keydown', (ev) => {
    if (ev.key === 'ArrowUp') {
      if (!history.length || pos === 0) return;
      ev.preventDefault();
      if (pos === history.length) draft = input.value;
      pos--;
      input.value = history[pos];
    } else if (ev.key === 'ArrowDown') {
      if (pos >= history.length) return;
      ev.preventDefault();
      pos++;
      input.value = pos === history.length ? draft : history[pos];
    }
  });

  clearBtn.addEventListener('click', () => {
    out.replaceChildren(hint);
    input.focus();
  });

  input.focus();
}
