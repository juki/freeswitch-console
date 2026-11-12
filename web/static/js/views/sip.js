// SIP: sofia status, gateways, registrations; admin can rescan a profile.

import { api, instPath, isAbort, rowsOf } from '../api.js';
import { h, tableBox, errorBox, outputBox } from '../dom.js';

export function mountSip({ id, scope, root, isAdmin }) {
  const output = h('div', { class: 'msg-area' });
  const sofia = tableBox();
  const gateways = tableBox();
  const regs = tableBox();

  root.append(
    h('section', { class: 'panel' }, h('h2', { text: 'Profiles, gateways and aliases' }), output, sofia.el),
    h('section', { class: 'panel' }, h('h2', { text: 'Gateways' }), gateways.el),
    h('section', { class: 'panel' }, h('h2', { text: 'Registrations' }), regs.el));

  function rescanButton(row) {
    if (row.type !== 'profile' || !row.name) return null;
    const btn = h('button', { type: 'button', class: 'small', text: 'Rescan' });
    btn.addEventListener('click', async () => {
      btn.disabled = true;
      try {
        const data = await api('POST', instPath(id, 'actions'), {
          body: { action: 'sofia_rescan', profile: String(row.name) },
          signal: scope.signal,
        });
        output.replaceChildren(outputBox('sofia profile ' + row.name + ' rescan', data && typeof data.output === 'string' ? data.output : ''));
      } catch (err) {
        if (!isAbort(err)) output.replaceChildren(errorBox(err));
      } finally {
        btn.disabled = false;
      }
    });
    return btn;
  }

  async function load(box, path, opts) {
    try {
      const data = await api('GET', instPath(id, path), { signal: scope.signal });
      box.rows(rowsOf(data), opts);
    } catch (err) {
      if (!isAbort(err)) box.error(err);
    }
  }

  load(sofia, 'sofia', { empty: 'No SIP profiles.', actions: isAdmin ? rescanButton : null });
  load(gateways, 'gateways', { empty: 'No gateways.' });
  load(regs, 'registrations', { empty: 'No registrations.' });
}
