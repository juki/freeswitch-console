// Runtime operations on one FreeSWITCH (POST .../actions). Every operation
// shows FreeSWITCH's reply as it came; operations that interrupt calls or
// change the running switch are confirmed first, in a dialog that keeps a
// failure in view next to the button that caused it.

import { api, instPath } from './api.js';
import { t } from './i18n.js';
import { actionDialog, toast, toastError } from './ui.js';

// operate runs one action. confirm: {title, message, confirmLabel, danger} or
// null; done: the toast title on success. Resolves to the reply text, or
// null when cancelled or failed.
export async function operate({ id, scope, body, confirm, done, failed }) {
  let output = null;
  const post = async () => {
    const data = await api('POST', instPath(id, 'actions'), { body, signal: scope.signal });
    output = (data && typeof data.output === 'string' && data.output.trim()) || t('common.done');
  };
  if (confirm) {
    const ok = await actionDialog({ ...confirm, run: post });
    if (!ok || scope.disposed) return null;
  } else {
    try {
      await post();
    } catch (err) {
      toastError(err, failed);
      return null;
    }
  }
  toast(output, /^-ERR/.test(output) ? 'error' : 'ok', done);
  return output;
}

// The note every switch-wide setting carries: it does not survive a restart.
export function transientNote() {
  return t('ops.transient');
}
