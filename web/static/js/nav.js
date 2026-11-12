// Addresses inside an instance page, and the "way back" after a jump.
//
//   #/i/<id>/<tab>[/<sub>][?query]
//   #/i/<id>/config/files?path=sip_profiles/internal.xml&line=42   a file at a line
//   #/i/<id>/config/files?q=gateway                                 a search in files
//   #/i/<id>/config/dialplan?item=default                           an open context
//
// Views keep the address current with ctx.setHash(), so reloading the page
// or sharing the link comes back to the same place.

export function instHash(id, tab, sub, params) {
  let hash = '#/i/' + encodeURIComponent(id) + '/' + tab + (sub ? '/' + sub : '');
  if (params) {
    const q = new URLSearchParams();
    for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== null && v !== '') q.set(k, String(v));
    const s = q.toString();
    if (s) hash += '?' + s;
  }
  return hash;
}

export function fileHash(id, path, line) {
  return instHash(id, 'config', 'files', { path, line: line > 0 ? line : '' });
}

// The view an "Edit source" jump came from: {hash, label, path}. The Files
// editor offers to go back to it once the change has been applied.
let back = null;

export function setBack(b) {
  back = b;
}

export function getBack(path) {
  return back && (!path || back.path === path) ? back : null;
}

export function clearBack() {
  back = null;
}
