// Console (admin): run FreeSWITCH API commands in a terminal pane, with Tab
// completion and a syntax hint, like fs_cli.
//
// Tab sends the text before the cursor to .../console/complete; the answer
// says how many bytes before the cursor to replace ("replace") and with what
// ("text"). One match is applied with a space after it; several apply their
// common prefix and open a list above the input (Tab / Shift+Tab / arrows
// move, Enter applies, Escape closes, typing narrows it). Answers that arrive
// after the text or the cursor moved are dropped, and typing is never
// blocked. When there is nothing to complete, a second Tab leaves the field
// as usual. ↑ / ↓ recall history while the list is closed.

import { api, instPath, isAbort, rowsOf } from '../api.js';
import { h, fmtInt } from '../dom.js';
import { icon } from '../icons.js';
import { plural, t } from '../i18n.js';

const HISTORY_MAX = 50;
const history = []; // kept in memory for the page lifetime
const QUICK = ['status', 'version', 'sofia status', 'show channels', 'show calls', 'show registrations', 'reloadxml', 'reloadacl'];
const commandCache = new Map(); // instance id -> Promise of [{name, description, syntax}]
const BUSY_AFTER = 300; // ms before a slow completion shows a spinner

const encoder = new TextEncoder();

// charsForBytes returns how many UTF-16 units at the end of s make up the
// given number of UTF-8 bytes (the server counts in bytes).
export function charsForBytes(s, bytes) {
  const chars = Array.from(s);
  let n = 0;
  let b = 0;
  for (let i = chars.length - 1; i >= 0 && b < bytes; i--) {
    b += encoder.encode(chars[i]).length;
    n += chars[i].length;
  }
  return n;
}

function commandsOf(id, signal) {
  if (!commandCache.has(id)) {
    const p = api('GET', instPath(id, 'console/commands'), { signal }).then(rowsOf, (err) => {
      commandCache.delete(id); // try again next time
      throw err;
    });
    commandCache.set(id, p);
  }
  return commandCache.get(id);
}

export function mountConsole({ id, scope, root }) {
  const hint = h('div', { class: 'term-empty' }, icon('terminal'), h('span', { text: t('con.hint') }));
  const out = h('div', { class: 'term-body', role: 'log', tabIndex: 0 }, hint);
  const input = h('input', {
    class: 'con-input', type: 'text', autocomplete: 'off', spellcheck: false,
    'aria-label': t('con.command'), placeholder: 'status', 'aria-autocomplete': 'list',
  });
  const runBtn = h('button', { type: 'submit', class: 'btn btn-sm', text: t('con.run') });
  const chips = QUICK.map((cmd) => h('button', { type: 'button', class: 'chip', text: cmd, on: { click: () => run(cmd) } }));
  const clearBtn = h('button', { type: 'button', class: 'btn btn-sm' }, icon('eraser'), t('common.clear'));
  const spinner = h('span', { class: 'con-busy', hidden: true, 'aria-hidden': 'true' });
  const form = h('form', { class: 'con-form' }, h('span', { class: 'con-prompt', text: 'fs>' }), input, spinner, runBtn);
  const list = h('div', { class: 'con-list', role: 'listbox', 'aria-label': t('con.matches'), hidden: true });
  const syntax = h('div', { class: 'con-syntax', 'aria-live': 'polite' });

  root.append(h('div', { class: 'term console' },
    h('div', { class: 'term-bar' },
      h('span', { class: 'term-title' }, icon('terminal'), t('con.title')),
      h('span', { class: 'term-sep' }),
      h('div', { class: 'chips' }, chips),
      h('span', { class: 'spacer' }), clearBtn),
    out,
    list,
    form,
    syntax));

  let busy = false;
  let pos = history.length;
  let draft = '';
  let commands = null; // [{name, description, syntax}] once loaded

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
    closeList();
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

  // ---------- completion ----------

  let reqScope = null; // the completion request in flight
  let busyTimer = null;
  let nothingAt = null; // "value|caret" where the last Tab found nothing
  let anchor = 0; // where the word being completed starts, while the list is open
  let matches = [];
  let shown = [];
  let active = -1;
  scope.onCleanup(() => clearTimeout(busyTimer));

  function stopRequest() {
    if (reqScope) reqScope.dispose();
    reqScope = null;
    clearTimeout(busyTimer);
    spinner.hidden = true;
  }

  function closeList() {
    list.hidden = true;
    list.replaceChildren();
    matches = [];
    shown = [];
    active = -1;
    input.removeAttribute('aria-activedescendant');
  }

  function noMatch() {
    nothingAt = input.value + '|' + input.selectionStart;
    form.classList.remove('nomatch');
    void form.offsetWidth; // restart the animation
    form.classList.add('nomatch');
    syntax.replaceChildren(h('span', { class: 'con-syntax-note', text: t('con.noMatches') }));
  }

  // replaceWord puts `word` in place of value[from, caret) and keeps the rest.
  function replaceWord(from, word, space) {
    const v = input.value;
    const caret = input.selectionStart;
    const rest = v.slice(caret);
    const add = space && !rest.startsWith(' ') ? ' ' : '';
    input.value = v.slice(0, from) + word + add + rest;
    const at = from + word.length + add.length;
    input.setSelectionRange(at, at);
    paintSyntax();
  }

  async function complete() {
    const caret = input.selectionStart;
    const before = input.value.slice(0, caret);
    if (encoder.encode(before).length > 1024) return;
    stopRequest();
    reqScope = scope.child();
    const mine = reqScope;
    const asked = input.value + '|' + caret;
    busyTimer = setTimeout(() => {
      if (!mine.disposed) spinner.hidden = false;
    }, BUSY_AFTER);
    let res;
    try {
      res = await api('POST', instPath(id, 'console/complete'), { body: { line: before }, signal: mine.signal });
    } catch (err) {
      if (isAbort(err) || mine !== reqScope) return;
      stopRequest();
      // 502: FreeSWITCH's answer could not be read; treat it as nothing to complete
      if (err.status === 502) {
        noMatch();
        return;
      }
      syntax.replaceChildren(h('span', { class: 'con-syntax-note err', text: err.status === 503 ? t('con.completeOffline') : err.message }));
      return;
    }
    if (mine !== reqScope) return;
    stopRequest();
    // the user typed or moved on while we waited: this answer is stale
    if (input.value + '|' + input.selectionStart !== asked) return;
    const found = res && Array.isArray(res.matches) ? res.matches.map(String) : [];
    const replace = Math.max(0, Number(res && res.replace) || 0);
    const text = res && typeof res.text === 'string' ? res.text : '';
    if (!found.length) {
      noMatch();
      return;
    }
    nothingAt = null;
    const from = caret - charsForBytes(before, replace);
    if (found.length === 1) {
      replaceWord(from, text || found[0], true);
      return;
    }
    if (text) replaceWord(from, text, false);
    anchor = from;
    matches = found;
    openList();
  }

  function openList() {
    const partial = input.value.slice(anchor, input.selectionStart);
    shown = matches.filter((m) => m.startsWith(partial));
    if (!shown.length || input.selectionStart < anchor) {
      closeList();
      return;
    }
    active = -1;
    list.replaceChildren(
      h('div', { class: 'con-list-head', text: plural(shown.length, t('con.matchCount.one', { n: fmtInt(shown.length) }), t('con.matchCount.other', { n: fmtInt(shown.length) })) }),
      ...shown.map((m, i) => h('button', {
        type: 'button', class: 'con-match', role: 'option', id: 'con-m-' + i, tabIndex: -1,
        on: { mousedown: (ev) => ev.preventDefault(), click: () => apply(i) },
      }, h('b', { text: m.slice(0, partial.length) }), m.slice(partial.length))));
    list.hidden = false;
  }

  function move(d) {
    if (!shown.length) return;
    active = active < 0 ? (d > 0 ? 0 : shown.length - 1) : (active + d + shown.length) % shown.length;
    const items = list.querySelectorAll('.con-match');
    items.forEach((el, i) => el.setAttribute('aria-selected', String(i === active)));
    items[active].scrollIntoView({ block: 'nearest' });
    input.setAttribute('aria-activedescendant', 'con-m-' + active);
  }

  function apply(i) {
    const m = shown[i];
    closeList();
    replaceWord(anchor, m, true);
    input.focus();
  }

  // ---------- syntax hint ----------

  function paintSyntax() {
    const v = input.value.trimStart();
    const first = v.split(/\s+/)[0] || '';
    if (!v) {
      syntax.replaceChildren(h('span', { class: 'con-syntax-note', text: t('con.tabHint') }));
      return;
    }
    if (!commands) {
      syntax.replaceChildren();
      return;
    }
    const starting = commands.filter((c) => c.name.startsWith(first));
    const exact = commands.find((c) => c.name === first);
    if (exact && (v.length > first.length || starting.length === 1)) {
      syntax.replaceChildren(
        h('code', { class: 'con-syn', title: exact.name + (exact.syntax ? ' ' + exact.syntax : '') }, h('b', { text: exact.name }), exact.syntax ? ' ' + exact.syntax : ''),
        exact.description ? h('span', { class: 'con-syntax-desc', text: exact.description }) : null);
      return;
    }
    const n = starting.length;
    syntax.replaceChildren(h('span', {
      class: 'con-syntax-note',
      text: n ? plural(n, t('con.prefixCount.one', { n: fmtInt(n), word: first }), t('con.prefixCount.other', { n: fmtInt(n), word: first })) : t('con.unknownCommand', { word: first }),
    }));
  }

  commandsOf(id, scope.signal).then((rows) => {
    commands = rows.filter((r) => r && r.name).map((r) => ({ name: String(r.name), description: String(r.description || ''), syntax: String(r.syntax || '') }));
    if (!scope.disposed) paintSyntax();
  }, () => {});

  // ---------- keys ----------

  form.addEventListener('submit', (ev) => {
    ev.preventDefault();
    const cmd = input.value.trim();
    if (!cmd || busy) return;
    input.value = '';
    paintSyntax();
    run(cmd);
  });

  input.addEventListener('input', () => {
    nothingAt = null;
    form.classList.remove('nomatch');
    if (!list.hidden) openList(); // typing narrows the list or closes it
    paintSyntax();
  });

  input.addEventListener('keydown', (ev) => {
    const open = !list.hidden;
    if (ev.key === 'Tab' && !ev.ctrlKey && !ev.metaKey && !ev.altKey) {
      if (open) {
        ev.preventDefault();
        move(ev.shiftKey ? -1 : 1);
        return;
      }
      if (ev.shiftKey) return; // leave the field backwards
      // nothing to complete here last time: let Tab move focus as usual
      if (nothingAt === input.value + '|' + input.selectionStart) return;
      ev.preventDefault();
      complete();
    } else if (ev.key === 'Escape') {
      if (open) {
        ev.preventDefault();
        closeList();
      } else {
        // Escape then Tab leaves the field
        nothingAt = input.value + '|' + input.selectionStart;
      }
    } else if (open && (ev.key === 'ArrowDown' || ev.key === 'ArrowUp')) {
      ev.preventDefault();
      move(ev.key === 'ArrowDown' ? 1 : -1);
    } else if (open && ev.key === 'Enter' && active >= 0) {
      ev.preventDefault();
      apply(active);
    } else if (ev.key === 'ArrowUp') {
      if (!history.length || pos === 0) return;
      ev.preventDefault();
      if (pos === history.length) draft = input.value;
      pos--;
      input.value = history[pos];
      paintSyntax();
    } else if (ev.key === 'ArrowDown') {
      if (pos >= history.length) return;
      ev.preventDefault();
      pos++;
      input.value = pos === history.length ? draft : history[pos];
      paintSyntax();
    } else if (open && (ev.key === 'ArrowLeft' || ev.key === 'ArrowRight' || ev.key === 'Home' || ev.key === 'End')) {
      closeList();
    }
  });
  input.addEventListener('blur', () => {
    // a click on a match keeps focus (its mousedown is prevented); anything else closes
    if (!list.hidden) closeList();
  });

  clearBtn.addEventListener('click', () => {
    out.replaceChildren(hint);
    input.focus();
  });

  paintSyntax();
  input.focus();
}
