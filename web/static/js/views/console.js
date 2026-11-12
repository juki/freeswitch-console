// Console (admin): run API commands, history, quick actions.

import { api, instPath, isAbort } from '../api.js';
import { h } from '../dom.js';

const HISTORY_MAX = 50;
const history = []; // kept in memory for the page lifetime

export function mountConsole({ id, scope, root }) {
  const out = h('div', { class: 'console-out mono', role: 'log', tabIndex: 0 });
  const input = h('input', {
    id: 'console-input', type: 'text', class: 'mono', autocomplete: 'off', spellcheck: false,
    placeholder: 'API command, e.g. status',
  });
  const runBtn = h('button', { type: 'submit', class: 'primary', text: 'Run' });
  const quick = [['reloadxml', 'reloadxml'], ['reloadacl', 'reloadacl']].map(([label, action]) =>
    h('button', { type: 'button', text: label, on: { click: () => exec('> ' + label, 'actions', { action }) } }));
  const form = h('form', { class: 'console-form' },
    h('label', { for: 'console-input', class: 'visually-hidden', text: 'Command' }), input, runBtn);

  root.append(
    h('div', { class: 'toolbar' }, h('span', { class: 'muted', text: 'Quick actions' }), quick),
    out,
    form);

  let busy = false;
  let pos = history.length;
  let draft = '';

  function setBusy(b) {
    busy = b;
    input.disabled = b;
    runBtn.disabled = b;
    for (const q of quick) q.disabled = b;
    if (!b) input.focus();
  }

  function print(cls, text) {
    const follow = out.scrollTop + out.clientHeight >= out.scrollHeight - 24;
    out.append(h('div', { class: cls, text }));
    if (follow) out.scrollTop = out.scrollHeight;
  }

  async function exec(label, path, body) {
    if (busy) return;
    setBusy(true);
    print('con-cmd', label);
    try {
      const data = await api('POST', instPath(id, path), { body, signal: scope.signal });
      print('con-out', data && typeof data.output === 'string' ? data.output : '');
    } catch (err) {
      if (isAbort(err)) return;
      print('con-err', err.status === 503 ? 'Not connected: ' + err.message : 'Error: ' + err.message);
    } finally {
      if (!scope.disposed) setBusy(false);
    }
  }

  form.addEventListener('submit', (ev) => {
    ev.preventDefault();
    const cmd = input.value.trim();
    if (!cmd || busy) return;
    if (history[history.length - 1] !== cmd) history.push(cmd);
    if (history.length > HISTORY_MAX) history.splice(0, history.length - HISTORY_MAX);
    pos = history.length;
    draft = '';
    input.value = '';
    exec('> ' + cmd, 'console', { command: cmd });
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

  input.focus();
}
