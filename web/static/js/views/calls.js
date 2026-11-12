// Calls: channels and calls, refreshed every 3s; admin can hang up.

import { api, instPath, isAbort, rowsOf } from '../api.js';
import { h, tableBox, errorBox, outputBox } from '../dom.js';

export function mountCalls({ id, scope, root, isAdmin }) {
  const msg = h('div', { class: 'msg-area' });
  const channels = tableBox();
  const calls = tableBox();

  root.append(
    msg,
    h('section', { class: 'panel' }, h('h2', { text: 'Channels' }), channels.el),
    h('section', { class: 'panel' }, h('h2', { text: 'Calls' }), calls.el));

  const refresh = scope.poll(async (signal) => {
    const [ch, cl] = await Promise.allSettled([
      api('GET', instPath(id, 'channels'), { signal }),
      api('GET', instPath(id, 'calls'), { signal }),
    ]);
    if (scope.disposed) return;
    const show = (box, res, opts) => {
      if (res.status === 'fulfilled') box.rows(rowsOf(res.value), opts);
      else if (!isAbort(res.reason)) box.error(res.reason);
    };
    show(channels, ch, { empty: 'No active channels.', actions: isAdmin ? hangupButton : null });
    show(calls, cl, { empty: 'No active calls.' });
  }, 3000);

  function hangupButton(row) {
    if (!row.uuid) return null;
    const uuid = String(row.uuid);
    const btn = h('button', { type: 'button', class: 'small danger', text: 'Hang up' });
    btn.addEventListener('click', async () => {
      const label = row.name ? ' (' + row.name + ')' : '';
      if (!window.confirm('Hang up channel ' + uuid + label + '?')) return;
      btn.disabled = true;
      try {
        const data = await api('DELETE', instPath(id, 'channels/' + encodeURIComponent(uuid)), { signal: scope.signal });
        msg.replaceChildren(outputBox('uuid_kill ' + uuid, data && typeof data.output === 'string' ? data.output : ''));
        refresh();
      } catch (err) {
        if (!isAbort(err)) msg.replaceChildren(errorBox(err));
        btn.disabled = false;
      }
    });
    return btn;
  }
}
