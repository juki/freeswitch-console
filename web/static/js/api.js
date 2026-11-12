// HTTP client for the fsdeck API. Paths are relative ("api/..."), so the
// console also works when served below a path prefix.

import { t } from './i18n.js';

// ApiError carries the HTTP status and, for a 402 (a licensed feature), the
// "feature" the server named.
export class ApiError extends Error {
  constructor(status, message, data) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.feature = data && typeof data.feature === 'string' ? data.feature : '';
  }
}

let unauthorized = () => {};

// onUnauthorized registers the handler called on any 401 (except login).
export function onUnauthorized(fn) {
  unauthorized = fn;
}

export function isAbort(err) {
  return Boolean(err) && err.name === 'AbortError';
}

// api performs a request and returns the decoded JSON body (null for 204 or
// an empty body). Non-2xx answers throw ApiError with the server's "error".
export async function api(method, path, { body, signal } = {}) {
  const init = {
    method,
    credentials: 'same-origin',
    headers: { Accept: 'application/json' },
    signal,
  };
  if (method !== 'GET') {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body === undefined ? {} : body);
  }
  let res;
  try {
    res = await fetch('api/' + path, init);
  } catch (err) {
    if (isAbort(err)) throw err;
    throw new ApiError(0, t('error.network'));
  }
  if (res.status === 401 && path !== 'login') {
    unauthorized();
    throw new ApiError(401, t('error.session'));
  }
  if (res.status === 204) return null;
  const text = await res.text();
  let data = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = null;
    }
  }
  if (!res.ok) {
    const msg = data && typeof data.error === 'string' && data.error ? data.error : `HTTP ${res.status}`;
    throw new ApiError(res.status, msg, data);
  }
  return data;
}

// instPath builds "instances/<id>[/<rest>]".
export function instPath(id, rest) {
  return 'instances/' + encodeURIComponent(id) + (rest ? '/' + rest : '');
}

// rowsOf returns the "rows" array of a list response, or [].
export function rowsOf(data) {
  return data && Array.isArray(data.rows) ? data.rows : [];
}
