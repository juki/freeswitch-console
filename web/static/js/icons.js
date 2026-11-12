// A small stroke icon set drawn on a 24x24 grid. Each icon is a list of
// [tag, attributes] shapes; "d" strings are shorthand for a path.

import { s } from './dom.js';

const ICONS = {
  grid: [['rect', { x: 3.5, y: 3.5, width: 7, height: 7, rx: 1.6 }], ['rect', { x: 13.5, y: 3.5, width: 7, height: 7, rx: 1.6 }],
    ['rect', { x: 3.5, y: 13.5, width: 7, height: 7, rx: 1.6 }], ['rect', { x: 13.5, y: 13.5, width: 7, height: 7, rx: 1.6 }]],
  activity: ['M3 12h4l3-8 4 16 3-8h4'],
  radio: [['circle', { cx: 12, cy: 12, r: 2 }], 'M7.8 16.2a6 6 0 0 1 0-8.4', 'M16.2 7.8a6 6 0 0 1 0 8.4', 'M4.9 19.1a10 10 0 0 1 0-14.2', 'M19.1 4.9a10 10 0 0 1 0 14.2'],
  calls: ['M4 8h15', 'M15 4l4 4-4 4', 'M20 16H5', 'M9 12l-4 4 4 4'],
  sliders: ['M4 6h9', 'M19 6h1', 'M4 12h3', 'M13 12h7', 'M4 18h11', ['circle', { cx: 16, cy: 6, r: 2.2 }], ['circle', { cx: 10, cy: 12, r: 2.2 }], ['circle', { cx: 18, cy: 18, r: 2.2 }]],
  logs: ['M4 6h16', 'M4 10h10', 'M4 14h16', 'M4 18h8'],
  terminal: [['rect', { x: 2.5, y: 4, width: 19, height: 16, rx: 2.5 }], 'M7 9.5l3.2 2.7L7 15', 'M12.5 15h4.5'],
  plus: ['M12 5v14', 'M5 12h14'],
  edit: ['M4 20h4L19 9l-4-4L4 16v4z', 'M13.5 6.5l4 4'],
  trash: ['M4.5 7h15', 'M10 7V4.5h4V7', 'M6.8 7l1 12.5h8.4l1-12.5', 'M10.3 11v5', 'M13.7 11v5'],
  refresh: ['M20 12a8 8 0 1 1-2.6-5.9', 'M20 4v4.5h-4.5'],
  search: [['circle', { cx: 11, cy: 11, r: 6.5 }], 'M16 16l4.5 4.5'],
  copy: [['rect', { x: 9, y: 9, width: 11, height: 11, rx: 2 }], 'M5 15V6a2 2 0 0 1 2-2h8'],
  x: ['M6 6l12 12', 'M18 6L6 18'],
  check: ['M5 12.5l5 4.5 9-9.5'],
  sun: [['circle', { cx: 12, cy: 12, r: 4 }], 'M12 2.5v2.5', 'M12 19v2.5', 'M2.5 12H5', 'M19 12h2.5', 'M5.3 5.3l1.8 1.8', 'M16.9 16.9l1.8 1.8', 'M5.3 18.7l1.8-1.8', 'M16.9 7.1l1.8-1.8'],
  moon: ['M20 14.2A8.2 8.2 0 1 1 9.8 4a6.6 6.6 0 0 0 10.2 10.2z'],
  contrast: [['circle', { cx: 12, cy: 12, r: 8.5 }], ['path', { d: 'M12 3.5a8.5 8.5 0 0 1 0 17z', class: 'solid' }]],
  logout: ['M9 4H6a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h3', 'M14 8l4 4-4 4', 'M18 12H9'],
  chevron: ['M9 6l6 6-6 6'],
  in: ['M19 5L7 17', 'M7 9v8h8'],
  out: ['M5 19L17 7', 'M9 7h8v8'],
  pause: [['rect', { x: 6.5, y: 5, width: 3.6, height: 14, rx: 1 }], ['rect', { x: 13.9, y: 5, width: 3.6, height: 14, rx: 1 }]],
  play: ['M7 4.5v15l12-7.5z'],
  server: [['rect', { x: 3, y: 4, width: 18, height: 7, rx: 2 }], ['rect', { x: 3, y: 13, width: 18, height: 7, rx: 2 }], 'M7 7.5h.01', 'M7 16.5h.01'],
  alert: ['M12 4L2.8 20h18.4z', 'M12 10v4.5', 'M12 17.2h.01'],
  info: [['circle', { cx: 12, cy: 12, r: 8.5 }], 'M12 11v5', 'M12 8h.01'],
  user: [['circle', { cx: 12, cy: 8, r: 3.6 }], 'M5 20a7 7 0 0 1 14 0'],
  cpu: [['rect', { x: 7, y: 7, width: 10, height: 10, rx: 1.6 }], 'M10 3.5V7', 'M14 3.5V7', 'M10 17v3.5', 'M14 17v3.5', 'M3.5 10H7', 'M3.5 14H7', 'M17 10h3.5', 'M17 14h3.5'],
  clock: [['circle', { cx: 12, cy: 12, r: 8.5 }], 'M12 7.5V12l3 2'],
  gauge: ['M4.5 17a8.5 8.5 0 1 1 15 0', 'M12 13l4-4.5', ['circle', { cx: 12, cy: 13, r: 1.4 }]],
  plug: ['M9 3v5', 'M15 3v5', 'M6.5 8h11v3.5a5.5 5.5 0 0 1-11 0z', 'M12 17v4'],
  hangup: [['circle', { cx: 12, cy: 12, r: 8.5 }], 'M9 9l6 6', 'M15 9l-6 6'],
  box: ['M12 3l8 4.5v9L12 21l-8-4.5v-9z', 'M4 7.5l8 4.5 8-4.5', 'M12 12v9'],
  eraser: ['M4 20h16', 'M7 16l-2.5-2.5a1.5 1.5 0 0 1 0-2.1l7-7a1.5 1.5 0 0 1 2.1 0l5 5a1.5 1.5 0 0 1 0 2.1L13 17', 'M9.5 9.5l5 5'],
};

export function icon(name, cls) {
  const shapes = ICONS[name] || ICONS.info;
  return s('svg', { class: cls ? 'icon ' + cls : 'icon', viewBox: '0 0 24 24', 'aria-hidden': 'true' },
    shapes.map((sh) => (typeof sh === 'string' ? s('path', { d: sh }) : s(sh[0], sh[1]))));
}

// brandMark is the fsdeck logo: a stack of bars with a live dot.
export function brandMark() {
  return s('svg', { class: 'brand-mark', viewBox: '0 0 32 32', 'aria-hidden': 'true' },
    s('rect', { width: 32, height: 32, rx: 8, fill: '#5653d4' }),
    s('rect', { x: 7, y: 8.5, width: 18, height: 3.6, rx: 1.8, fill: '#fff' }),
    s('rect', { x: 7, y: 14.2, width: 13, height: 3.6, rx: 1.8, fill: '#fff', opacity: 0.82 }),
    s('rect', { x: 7, y: 19.9, width: 8, height: 3.6, rx: 1.8, fill: '#fff', opacity: 0.62 }),
    s('circle', { cx: 23, cy: 21.7, r: 2.2, fill: '#6fe3a5' }));
}
