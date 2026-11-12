// Scope owns everything a view starts: an AbortController for its fetches,
// polling timers, EventSources and cleanup callbacks. dispose() tears it all
// down, so a detached view never keeps running.

import { api, isAbort } from './api.js';

export class Scope {
  constructor() {
    this.ctrl = new AbortController();
    this.cleanups = new Set();
    this.disposed = false;
  }

  get signal() {
    return this.ctrl.signal;
  }

  onCleanup(fn) {
    if (this.disposed) {
      fn();
      return () => {};
    }
    this.cleanups.add(fn);
    return () => this.cleanups.delete(fn);
  }

  // child returns a scope that is disposed with this one.
  child() {
    const c = new Scope();
    const remove = this.onCleanup(() => c.dispose());
    c.onCleanup(remove);
    return c;
  }

  dispose() {
    if (this.disposed) return;
    this.disposed = true;
    this.ctrl.abort();
    const fns = [...this.cleanups].reverse();
    this.cleanups.clear();
    for (const fn of fns) {
      try {
        fn();
      } catch (err) {
        console.error(err);
      }
    }
  }

  later(fn, ms) {
    const t = setTimeout(() => {
      remove();
      if (!this.disposed) fn();
    }, ms);
    const remove = this.onCleanup(() => clearTimeout(t));
  }

  // poll runs fn(signal) now and then ms after each run finishes. It skips
  // runs while the document is hidden and resumes when it becomes visible.
  // Returns a function that triggers an immediate run.
  poll(fn, ms) {
    let timer = null;
    let running = false;
    let again = false;
    const run = async () => {
      clearTimeout(timer);
      timer = null;
      if (this.disposed) return;
      if (running) {
        again = true;
        return;
      }
      if (document.hidden) return;
      running = true;
      try {
        await fn(this.signal);
      } catch (err) {
        if (!isAbort(err)) console.error(err);
      }
      running = false;
      if (this.disposed) return;
      if (again) {
        again = false;
        run();
      } else if (!document.hidden) {
        timer = setTimeout(run, ms);
      }
    };
    const onVisible = () => {
      if (!document.hidden && timer === null && !running) run();
    };
    document.addEventListener('visibilitychange', onVisible);
    this.onCleanup(() => {
      clearTimeout(timer);
      document.removeEventListener('visibilitychange', onVisible);
    });
    run();
    return run;
  }

  // stream opens an EventSource on url and dispatches JSON-decoded events to
  // handlers by event name. When the browser gives up (a non-200 answer), the
  // session is checked (a 401 leads to the login view) and the stream is
  // reopened after 5s.
  stream(url, handlers) {
    let es = null;
    let retry = null;
    const connect = () => {
      retry = null;
      if (this.disposed) return;
      const source = new EventSource(url);
      es = source;
      for (const [name, fn] of Object.entries(handlers)) {
        source.addEventListener(name, (ev) => {
          if (this.disposed) return;
          let data;
          try {
            data = JSON.parse(ev.data);
          } catch {
            return;
          }
          fn(data);
        });
      }
      source.addEventListener('error', () => {
        if (this.disposed || source.readyState !== EventSource.CLOSED) return;
        es = null;
        api('GET', 'me', { signal: this.signal })
          .catch(() => {})
          .then(() => {
            if (!this.disposed) retry = setTimeout(connect, 5000);
          });
      });
    };
    connect();
    this.onCleanup(() => {
      clearTimeout(retry);
      if (es) es.close();
      es = null;
    });
  }
}
