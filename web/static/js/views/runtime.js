// Runtime settings (admin): switch-wide values changed on the running
// FreeSWITCH — log level, session limits, pausing new calls, global
// variables. Each shows the current value where the API reports one, and
// every change is confirmed with the reminder that it lasts only until
// FreeSWITCH restarts.

import { api, instPath, isAbort, rowsOf } from '../api.js';
import { h, DASH, cellText, fmtInt } from '../dom.js';
import { icon } from '../icons.js';
import { t } from '../i18n.js';
import { errorState, pill, skeleton, stateTone } from '../ui.js';
import { operate, transientNote } from '../actions.js';

const LEVELS = ['CONSOLE', 'ALERT', 'CRIT', 'ERR', 'WARNING', 'NOTICE', 'INFO', 'DEBUG'];

export function mountRuntime({ id, scope, root }) {
  const grid = h('div', { class: 'rt-grid' }, skeleton({ rows: 6, cols: 1 }));
  root.append(h('section', { class: 'panel' },
    h('div', { class: 'panel-head' }, h('h2', { class: 'panel-title', text: t('rt.title') })),
    h('div', { class: 'panel-body' },
      h('p', { class: 'note-inline warn' }, icon('info'), t('rt.intro')),
      grid)));

  let status = null;
  let vars = [];

  function card(titleText, current, controls, help) {
    return h('div', { class: 'rt-card' },
      h('div', { class: 'rt-head' }, h('div', { class: 'rt-title', text: titleText }), h('div', { class: 'rt-current' }, h('span', { class: 'faint', text: t('rt.current') }), current)),
      help ? h('p', { class: 'rt-help', text: help }) : null,
      h('div', { class: 'rt-controls' }, controls));
  }

  async function apply(body, title, message, label) {
    const out = await operate({
      id, scope, body,
      confirm: { title, message: message + ' ' + transientNote(), confirmLabel: label },
      done: label, failed: t('ops.failed', { op: label }),
    });
    if (out !== null) load();
  }

  function render() {
    const st = status && status.status;
    const count = (st && st.sessions && st.sessions.count) || {};
    const rate = (st && st.sessions && st.sessions.rate) || {};

    // log level: FreeSWITCH does not report the current one
    const level = h('select', { class: 'select', 'aria-label': t('rt.loglevel') }, LEVELS.map((n, i) => h('option', { value: String(i), text: n + ' (' + i + ')' })));
    level.value = '6';
    const levelCard = card(t('rt.loglevel'), h('span', { class: 'faint', text: t('rt.notReported') }), [
      level,
      h('button', {
        type: 'button', class: 'btn btn-sm',
        on: { click: () => apply({ action: 'fsctl', op: 'loglevel', value: Number(level.value) }, t('rt.loglevel.title'), t('rt.loglevel.text', { level: LEVELS[Number(level.value)] }), t('rt.apply')) },
      }, t('rt.apply')),
    ], t('rt.loglevel.help'));

    const number = (op, cur, min, max, titleKey) => {
      const input = h('input', { class: 'input mono', type: 'number', min: String(min), max: String(max), value: cur ? String(cur) : '' });
      const btn = h('button', { type: 'button', class: 'btn btn-sm', text: t('rt.apply') });
      btn.addEventListener('click', () => {
        const v = Number(input.value);
        if (!Number.isInteger(v) || v < min || v > max) {
          input.setAttribute('aria-invalid', 'true');
          input.title = t('rt.range', { min: fmtInt(min), max: fmtInt(max) });
          return;
        }
        input.removeAttribute('aria-invalid');
        apply({ action: 'fsctl', op, value: v }, titleKey === 'max' ? t('rt.max.title') : t('rt.sps.title'),
          titleKey === 'max' ? t('rt.max.text', { from: fmtInt(cur), to: fmtInt(v) }) : t('rt.sps.text', { from: fmtInt(cur), to: fmtInt(v) }), t('rt.apply'));
      });
      return [input, btn];
    };
    const maxCard = card(t('rt.max'), h('b', { class: 'num', text: fmtInt(count.limit) }), number('max_sessions', Number(count.limit), 1, 100000, 'max'), t('rt.max.help'));
    const spsCard = card(t('rt.sps'), h('b', { class: 'num', text: fmtInt(rate.max) }), number('sps', Number(rate.max), 1, 10000, 'sps'), t('rt.sps.help'));

    const sys = st ? String(st.systemStatus || '') : '';
    const pauseCard = card(t('rt.calls'), sys ? pill(sys, stateTone(sys) || (sys === 'ready' ? 'ok' : 'warn')) : h('span', { text: DASH }), [
      h('button', {
        type: 'button', class: 'btn btn-sm btn-danger',
        on: { click: () => apply({ action: 'fsctl', op: 'pause' }, t('rt.pause.title'), t('rt.pause.text'), t('rt.pause')) },
      }, icon('pause'), t('rt.pause')),
      h('button', {
        type: 'button', class: 'btn btn-sm',
        on: { click: () => apply({ action: 'fsctl', op: 'resume' }, t('rt.resume.title'), t('rt.resume.text'), t('rt.resume')) },
      }, icon('play'), t('rt.resume')),
    ], t('rt.calls.help'));

    // global variable: the current value of a known name is shown as you type
    const listId = 'rt-vars-' + id;
    const name = h('input', { class: 'input mono', type: 'text', placeholder: 'default_language', spellcheck: false, autocomplete: 'off', 'aria-label': t('rt.var.name') });
    name.setAttribute('list', listId);
    const value = h('input', { class: 'input mono', type: 'text', spellcheck: false, autocomplete: 'off', placeholder: t('rt.var.value'), 'aria-label': t('rt.var.value') });
    const cur = h('span', { class: 'mono', text: DASH });
    const showCurrent = () => {
      const row = vars.find((r) => cellText(r.name) === name.value.trim());
      cur.className = row && row.value === '********' ? 'masked' : 'mono';
      cur.textContent = !row ? (name.value.trim() ? t('rt.var.new') : DASH) : row.value === '********' ? t('cfg.masked') : cellText(row.value) || t('rt.var.empty');
    };
    name.addEventListener('input', showCurrent);
    const setBtn = h('button', { type: 'button', class: 'btn btn-sm', text: t('rt.var.set') });
    setBtn.addEventListener('click', () => {
      const n = name.value.trim();
      if (!/^[A-Za-z0-9_.-]+$/.test(n)) {
        name.setAttribute('aria-invalid', 'true');
        name.title = t('rt.var.rule');
        return;
      }
      if (/[\r\n]/.test(value.value) || value.value.length > 1024) {
        value.setAttribute('aria-invalid', 'true');
        return;
      }
      name.removeAttribute('aria-invalid');
      value.removeAttribute('aria-invalid');
      apply({ action: 'setvar', name: n, value: value.value }, t('rt.var.title', { name: n }), t('rt.var.text', { name: n }), t('rt.var.set'));
    });
    const varCard = card(t('rt.var'), cur, [
      name, value, setBtn,
      h('datalist', { id: listId }, vars.map((r) => h('option', { value: cellText(r.name) }))),
    ], t('rt.var.help'));
    varCard.classList.add('wide');

    grid.replaceChildren(levelCard, pauseCard, maxCard, spsCard, varCard);
  }

  function load() {
    Promise.all([
      api('GET', instPath(id, 'status'), { signal: scope.signal }),
      api('GET', instPath(id, 'vars'), { signal: scope.signal }).then(rowsOf, () => []),
    ]).then(([s, v]) => {
      status = s;
      vars = v;
      render();
    }, (err) => {
      if (!isAbort(err)) grid.replaceChildren(errorState(err));
    });
  }
  load();
}
