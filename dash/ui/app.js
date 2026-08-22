// context-guru dashboard. No framework, no build step, no CDN — the page is three
// files in a Go binary. State is a plain object, rendering is direct DOM writes,
// and charts are hand-drawn SVG. Everything that reads provider- or agent-supplied
// text goes through textContent or el(), never innerHTML, because a tool output in
// a transcript is attacker-influenced content (gateway interpolates it; we do not).
'use strict';

// ── tiny DOM helpers ───────────────────────────────────────────────────────
const $ = (s, r = document) => r.querySelector(s);
const $$ = (s, r = document) => Array.from(r.querySelectorAll(s));

/** el(tag, props, ...children) — children are appended as text unless they are Nodes. */
function el(tag, props, ...kids) {
  const n = document.createElement(tag);
  if (props) for (const [k, v] of Object.entries(props)) {
    if (v === null || v === undefined || v === false) continue;
    if (k === 'class') n.className = v;
    else if (k === 'style') setStyle(n, v);
    else if (k === 'text') n.textContent = String(v);
    else if (k === 'html') throw new Error('el(): raw html is not allowed');
    else if (k.startsWith('on')) n.addEventListener(k.slice(2), v);
    else n.setAttribute(k, String(v));
  }
  for (const kid of kids.flat()) {
    if (kid === null || kid === undefined || kid === false) continue;
    n.appendChild(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return n;
}
const clear = (n) => { while (n.firstChild) n.removeChild(n.firstChild); return n; };

/**
 * setStyle applies "prop:value;prop:value" via the CSSOM.
 *
 * Not as a style ATTRIBUTE: the page ships a strict `style-src 'self'` CSP, which
 * blocks inline style attributes — and that CSP is worth keeping, because the diff
 * view renders tool output the model was fed, i.e. attacker-influenced text. Going
 * through el.style.setProperty is exempt from style-src and equally expressive.
 */
function setStyle(node, decls) {
  for (const part of String(decls).split(';')) {
    const i = part.indexOf(':');
    if (i < 0) continue;
    const prop = part.slice(0, i).trim();
    const val = part.slice(i + 1).trim();
    if (prop) node.style.setProperty(prop, val);
  }
}
const svgEl = (tag, attrs) => {
  const n = document.createElementNS('http://www.w3.org/2000/svg', tag);
  for (const [k, v] of Object.entries(attrs || {})) n.setAttribute(k, String(v));
  return n;
};

// ── formatting ─────────────────────────────────────────────────────────────
const nf = new Intl.NumberFormat();
function num(v) { return v === null || v === undefined ? '—' : nf.format(Math.round(v)); }
function compact(v) {
  if (v === null || v === undefined) return '—';
  const a = Math.abs(v);
  if (a >= 1e9) return (v / 1e9).toFixed(a >= 1e10 ? 0 : 1) + 'B';
  if (a >= 1e6) return (v / 1e6).toFixed(a >= 1e7 ? 0 : 1) + 'M';
  if (a >= 1e3) return (v / 1e3).toFixed(a >= 1e4 ? 0 : 1) + 'k';
  return nf.format(Math.round(v));
}
/**
 * savedPerTok is what ONE token removed from this request was worth, read off the
 * request's own billed figures: (baseline − actual) ÷ tokens removed.
 *
 * There used to be a rate table here — 3.00/3.75/0.30 per MTok, hardcoded — and it was
 * sonnet-class while this deployment bills opus, so every net figure derived from it was
 * ~27% wrong, in a direction nobody could see. The request already carries the answer,
 * priced server-side at write time from its OWN model and the tiers it actually paid, which
 * is the same rule dash's baselineDeltaUSD and CompRow.SavedUSD use. So the browser asks
 * the row instead of guessing.
 *
 * null, not 0, when the request is not fully priced: an unknown must not render as "worth
 * nothing".
 */
function savedPerTok(e) {
  if (e.token_accounting !== 'complete') return null;
  const removed = (e.tokens_before || 0) - (e.tokens_after || 0);
  if (removed <= 0) return null;
  return ((e.baseline_cost_usd || 0) - (e.cost_usd || 0)) / removed;
}

/** netUSD prices one recorded compaction call: what its removals were worth, less the call. */
function netUSD(x, perTok) {
  if (perTok === null || perTok === undefined) return null;
  return (x.saved_tokens || 0) * perTok - (x.cost_usd || 0);
}

function usd(v) {
  if (v === null || v === undefined) return '—';
  const a = Math.abs(v);
  if (a === 0) return '$0';
  if (a < 0.01) return (v < 0 ? '-' : '') + '$' + a.toFixed(4);
  if (a < 1000) return (v < 0 ? '-' : '') + '$' + a.toFixed(2);
  return (v < 0 ? '-' : '') + '$' + nf.format(Math.round(a));
}
function pct(v, digits = 1) { return v === null || v === undefined ? '—' : v.toFixed(digits) + '%'; }
function ms(v) {
  if (!v) return '0 ms';
  return v >= 1000 ? (v / 1000).toFixed(2) + ' s' : v.toFixed(v < 10 ? 1 : 0) + ' ms';
}
// Timestamps are epoch ms on the wire and formatted here, in the VIEWER's locale.
// The server never stores or sends a formatted date — a locale string cannot be
// range-queried, sorted, or bucketed.
function when(tsMs) {
  if (!tsMs) return '—';
  const d = new Date(tsMs);
  const today = new Date();
  const sameDay = d.toDateString() === today.toDateString();
  return sameDay ? d.toLocaleTimeString() : d.toLocaleString();
}
function dur(msv) {
  if (!msv || msv < 0) return '—';
  // Below a second, show the actual milliseconds: rounding a component's 300 ms of
  // total hot-path time to "0s" hides exactly the cost this view exists to expose.
  if (msv < 1000) return ms(msv);
  const s = Math.round(msv / 1000);
  if (s < 60) return s + 's';
  const m = Math.floor(s / 60);
  if (m < 60) return m + 'm ' + (s % 60) + 's';
  return Math.floor(m / 60) + 'h ' + (m % 60) + 'm';
}
function firstOf(csv) { return (csv || '').split(',')[0] || '—'; }

/**
 * wasRewritten answers "did compaction change this request" from the records that are
 * ALWAYS written — the component rows and the server's own compaction outcome — and
 * never from whether the transcript happens to be stored.
 *
 * Content capture is not retroactive: a request that ran before capture was switched on
 * has component rows and no content rows. Inferring "nothing was rewritten" from the
 * empty content told the reader a request had passed through untouched directly above a
 * table showing four components acting on it and 2,149 tokens becoming 492.
 */
function wasRewritten(e) {
  // uncompressed_reason is the server saying why it did NOT compact ('' = it did).
  if (e.uncompressed_reason) return false;
  if ((e.components || []).some((c) => c.acted && !c.reverted)) return true;
  return (e.tokens_after || 0) < (e.tokens_before || 0);
}

/**
 * modeLabel prints an operating mode in BOTH vocabularies at once.
 *
 * Settings configures `sync`/`observe`; a captured request records `active`/`observe`/
 * `bypass`. Same dimension, two names, and nothing said so — so a user who set `sync`
 * went looking for it in a Mode column that only ever says `active`.
 */
function modeLabel(m) {
  if (m === 'active') return 'active (sync)';
  if (m === 'bypass') return 'bypass (this request)';
  return m || '—';
}

// ── state ──────────────────────────────────────────────────────────────────
const state = {
  view: 'overview',
  filter: {},
  // The time range, Grafana's model: `from` and `to` are each EITHER a relative token
  // ('now-6h', 'now') or an absolute epoch-ms number. 0 means unbounded, which is what
  // "All time" is. Keeping a relative window relative is the whole point — the old
  // state.range froze the window at the moment it was resolved, and one refresh() that
  // fires stats + series + breakdown resolved Date.now() three times and produced three
  // slightly different windows for one screen of numbers.
  from: 0,
  to: 'now',
  // nowMs is stamped ONCE per refresh() and every relative token in that repaint resolves
  // against it. That is the fix for the three-windows bug above.
  nowMs: 0,
  // sort is the client-side sort of the components table: a field name and a direction, or
  // '' for the server's own order (saved_unique DESC, which the bar chart beside it shares).
  sort: '',
  dir: 'desc',
  reqCursor: 0,
  reqStack: [],
  sessOffset: 0,
  live: [],
  overview: null,
  // dim is which dimension the Usage breakdown groups by. Not a FILTER — it selects a
  // view of the same filtered window, so it is deliberately outside state.filter and
  // outside the URL: it narrows nothing, and a chip for it would claim it did.
  // ponytail: not URL-persisted; put it in the query string if sharing a breakdown link matters.
  dim: 'model',
  // What the drawer is showing, and part of the URL: {req: id} | {diff: session} | null.
  drawer: null,
  // ac aborts every fetch belonging to the PREVIOUS filter state. Without it a slow
  // response for "agent=bob" could land after a fast one for "agent=bob + preset=x"
  // and repaint the table with rows that do not match the filter bar.
  ac: null,
};

/** RANGE_UNIT_MS is the suffix table for a relative token: `now-90m`, `now-7d`. */
const RANGE_UNIT_MS = { s: 1000, m: 60000, h: 3600000, d: 86400000, w: 604800000 };

/**
 * resolveTime turns one endpoint into epoch ms against a FIXED `now`, or 0 for unbounded.
 * Accepts 'now', 'now-<n><unit>', a number, or a numeric string (the URL gives us strings).
 */
function resolveTime(v, nowMs) {
  if (v === 'now') return nowMs;
  if (typeof v === 'string') {
    const m = /^now-(\d+)([smhdw])$/.exec(v);
    if (m) return nowMs - Number(m[1]) * RANGE_UNIT_MS[m[2]];
    const n = Number(v);
    return Number.isFinite(n) && n > 0 ? n : 0;
  }
  return Number(v) > 0 ? Number(v) : 0;
}
/** The window as the server sees it: [since, until), either bound 0 for unbounded. */
function rangeMs() {
  const now = state.nowMs || Date.now();
  return [resolveTime(state.from, now), state.to === 'now' ? 0 : resolveTime(state.to, now)];
}
/**
 * writeRange stamps since/until onto a URLSearchParams. `until` is OMITTED while `to` is
 * 'now', so a live window stays live rather than being pinned to this repaint.
 *
 * It lives here rather than being passed through qs()'s `extra` because that argument drops
 * any value that is '', 0 or undefined — so `until: 0` could never mean "unbounded" through it.
 */
function writeRange(p) {
  const [since, until] = rangeMs();
  if (since > 0) p.set('since', String(since));
  if (until > 0) p.set('until', String(until));
  return p;
}
/** hasRange is whether the time filter is narrowing anything at all. */
function hasRange() { const [a, b] = rangeMs(); return a > 0 || b > 0; }

function qs(extra) {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(state.filter)) if (v) p.set(k, v);
  writeRange(p);
  for (const [k, v] of Object.entries(extra || {})) if (v !== '' && v !== 0 && v !== undefined) p.set(k, String(v));
  const s = p.toString();
  return s ? '?' + s : '';
}

/** An aborted fetch is not a failure to report: it means the user moved on. */
function aborted(err) { return !!err && (err.name === 'AbortError' || err.code === 20); }

async function api(path, extra) {
  const res = await fetch('/api/' + path + qs(extra), {
    headers: { accept: 'application/json' },
    signal: state.ac ? state.ac.signal : undefined,
  });
  if (!res.ok) {
    let msg = res.status + ' ' + res.statusText;
    try { const j = await res.json(); if (j.error) msg = j.error; } catch (_) { /* not json */ }
    const e = new Error(msg); e.status = res.status; throw e;
  }
  return res.json();
}

/**
 * emptyState renders "there is nothing here" with the reason.
 *
 * opts.error marks it as a FAILURE rather than an absence. That distinction is the
 * whole value of the helper: "no requests match your filter" and "the query blew up"
 * looked identical before, so a broken backend read as a quiet afternoon. opts.action
 * attaches the one button that fixes it, where one exists.
 */
function emptyState(host, title, detail, opts = {}) {
  const box = el('div', { class: 'empty' + (opts.error ? ' error' : '') },
    el('strong', { text: title }), detail || '');
  if (opts.action) box.appendChild(el('div', { class: 'empty-action' }, opts.action));
  clear(host).appendChild(box);
  return box;
}
function errorState(host, title, err) {
  return emptyState(host, title, String((err && err.message) || err || ''), { error: true });
}
function loadingState(host, rows = 3) {
  clear(host);
  for (let i = 0; i < rows; i++) {
    host.appendChild(el('div', { class: 'skel', style: 'margin:8px 0;width:' + (100 - i * 12) + '%' }));
  }
}
/** A skeleton that occupies a table body, so the header does not jump when rows land. */
function loadingRows(body, cols, rows = 4) {
  clear(body);
  for (let i = 0; i < rows; i++) {
    body.appendChild(el('tr', {}, el('td', { colspan: String(cols) },
      el('div', { class: 'skel', style: 'width:' + (96 - i * 9) + '%' }))));
  }
}
/** Fill a table body with one full-width message row. */
function tableMessage(body, cols, title, detail, opts = {}) {
  const cell = el('td', { colspan: String(cols) });
  clear(body).appendChild(el('tr', {}, cell));
  emptyState(cell, title, detail, opts);
}

// ── charts ─────────────────────────────────────────────────────────────────
// A tiny SVG line/area/bar renderer. Native SVG covers everything the issue asks
// for; a vendored chart library would add 45 KB to buy the tooltip below.
const CH = { w: 900, h: 220, pad: { t: 12, r: 14, b: 26, l: 56 } };

/**
 * SERIES is the categorical hue order, and it is an ORDER, not a pool.
 *
 * A hue belongs to an entity for as long as that entity is on screen: "with
 * context-guru" is blue in every chart it appears in, the baseline is orange in every
 * chart it appears in. The two rules that follow from that, and that this file used to
 * break in three places:
 *
 *   - Never index by rank (`--s${i % 5 + 1}`). Colouring the Nth bar by N means a
 *     filter that drops one component repaints every component below it, so the reader
 *     re-learns the legend on every interaction.
 *   - One measure across many categories is ONE hue. Twelve components' savings is a
 *     magnitude comparison; twelve colours imply twelve different things are plotted.
 *
 * Steps come from --s1..--s4, validated for the lightness band, chroma floor, adjacent
 * CVD separation and surface contrast in both themes. Four is the whole set; a fifth
 * series means the chart needs splitting, not a new colour.
 */
const SERIES = ['var(--s1)', 'var(--s2)', 'var(--s3)', 'var(--s4)'];

/**
 * ticks returns axis values a person can read.
 *
 * Dividing the range into n equal parts is arithmetically fine and produces
 * "$0 / $24.29 / $48.59 / $72.88 / $97.17" — five numbers nobody can compare at a
 * glance and none of which is a round figure. This snaps the step to 1, 2, 2.5 or 5
 * times a power of ten, so the labels land on values with meaning.
 */
function ticks(min, max, n = 4) {
  if (!isFinite(min) || !isFinite(max) || max === min) return [min || 0];
  const raw = (max - min) / n;
  const mag = Math.pow(10, Math.floor(Math.log10(raw)));
  const norm = raw / mag;
  const step = (norm <= 1 ? 1 : norm <= 2 ? 2 : norm <= 2.5 ? 2.5 : norm <= 5 ? 5 : 10) * mag;
  const out = [];
  for (let v = Math.ceil(min / step) * step; v <= max + step * 0.001; v += step) out.push(v);
  // A range flatter than one step still needs its two ends labelled.
  return out.length >= 2 ? out : [min, max];
}

/**
 * geom sizes a chart's viewBox to the host's actual CSS width.
 *
 * The charts used to render into a fixed 900x220 viewBox with
 * preserveAspectRatio="none", which stretched every one of them to whatever width its
 * panel happened to be — so in the two-column grid the axis labels were squashed
 * horizontally and the strokes were thicker vertically than horizontally. Matching the
 * viewBox to the real width makes the scale 1:1: a 2px stroke is 2px and an 11px label
 * is 11px, in the wide panel and the narrow one alike.
 */
function geom(host) {
  const w = Math.max(420, Math.round(host.clientWidth || CH.w));
  return { w, h: Math.round(Math.min(320, Math.max(200, w * 0.30))), pad: CH.pad };
}

/** Axis timestamps need to be short. when() is for a table cell, where there is room. */
function whenAxis(tsMs) {
  if (!tsMs) return '';
  const d = new Date(tsMs);
  const time = d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
  const sameDay = d.toDateString() === new Date().toDateString();
  return sameDay ? time : d.toLocaleDateString([], { month: 'short', day: 'numeric' }) + ' ' + time;
}

/**
 * lineChart(host, series, opts)
 * series: [{name, color, points:[[x,y],…], area?:bool, dashed?:bool}]
 * opts: {yFmt, xFmt, tipFmt, stacked?}
 */
function lineChart(host, series, opts = {}) {
  clear(host);
  const live = series.filter((s) => s.points && s.points.length);
  if (!live.length) { emptyState(host, 'No data in this window', 'Send traffic through the proxy, or widen the time range.'); return; }
  const yFmt = opts.yFmt || compact, xFmt = opts.xFmt || whenAxis;

  const xs = live.flatMap((s) => s.points.map((p) => p[0]));
  const ys = live.flatMap((s) => s.points.map((p) => p[1]));
  const xMin = Math.min(...xs), xMax = Math.max(...xs);
  // opts.yMax pins the top of the scale when the measure has a KNOWN ceiling: a mean out
  // of five whose axis stops at the highest observed value draws 4.0 hard against the top
  // of the plot, which reads as "as good as it gets". Real data above the pin still wins,
  // so this can only widen the axis and never clips a point.
  const yMin = Math.min(0, ...ys), yMax = Math.max(opts.yMax || 0, ...ys) || 1;
  const { w, h, pad } = geom(host);
  const px = (x) => pad.l + (xMax === xMin ? 0 : ((x - xMin) / (xMax - xMin)) * (w - pad.l - pad.r));
  const py = (y) => h - pad.b - ((y - yMin) / (yMax - yMin || 1)) * (h - pad.t - pad.b);

  const svg = svgEl('svg', { viewBox: `0 0 ${w} ${h}`, role: 'img' });
  svg.setAttribute('aria-label', opts.label || 'time series chart');

  for (const t of ticks(yMin, yMax)) {
    svg.appendChild(svgEl('line', { class: 'gridline', x1: pad.l, x2: w - pad.r, y1: py(t), y2: py(t) }));
    const lab = svgEl('text', { class: 'axis-text', x: pad.l - 6, y: py(t) + 3, 'text-anchor': 'end' });
    lab.textContent = yFmt(t);
    svg.appendChild(lab);
  }
  svg.appendChild(svgEl('line', { class: 'axis', x1: pad.l, x2: w - pad.r, y1: h - pad.b, y2: h - pad.b }));
  for (const t of [xMin, (xMin + xMax) / 2, xMax]) {
    const lab = svgEl('text', {
      class: 'axis-text', x: px(t), y: h - pad.b + 14,
      'text-anchor': t === xMin ? 'start' : t === xMax ? 'end' : 'middle',
    });
    lab.textContent = xFmt(t);
    svg.appendChild(lab);
  }

  // Shaded band between the first two series (the "money saved" area).
  if (opts.band && live.length >= 2) {
    const a = live[0].points, b = live[1].points;
    const dPath = a.map((p, i) => `${i ? 'L' : 'M'}${px(p[0])},${py(p[1])}`).join('') +
      b.slice().reverse().map((p) => `L${px(p[0])},${py(p[1])}`).join('') + 'Z';
    svg.appendChild(svgEl('path', { d: dPath, fill: live[0].color, opacity: '0.14' }));
  }

  for (const s of live) {
    const d = s.points.map((p, i) => `${i ? 'L' : 'M'}${px(p[0])},${py(p[1])}`).join('');
    if (s.area) {
      svg.appendChild(svgEl('path', {
        d: d + `L${px(s.points[s.points.length - 1][0])},${py(yMin)}L${px(s.points[0][0])},${py(yMin)}Z`,
        fill: s.color, opacity: '0.13',
      }));
    }
    svg.appendChild(svgEl('path', {
      d, fill: 'none', stroke: s.color, 'stroke-width': 2,
      'stroke-linejoin': 'round', 'stroke-linecap': 'round',
      'stroke-dasharray': s.dashed ? '5 4' : null,
    }));
    // A path through a single point renders nothing, so one bucket of traffic would
    // look identical to no traffic. Draw explicit markers on short series.
    if (s.points.length <= 12) {
      for (const pt of s.points) {
        svg.appendChild(svgEl('circle', { cx: px(pt[0]), cy: py(pt[1]), r: 3.5, fill: s.color }));
      }
    }
  }

  const hover = svgEl('line', { class: 'axis', x1: 0, x2: 0, y1: pad.t, y2: h - pad.b, opacity: '0' });
  svg.appendChild(hover);
  // A transparent capture rect over the plot area. Without it, pointer events only
  // land on the rendered strokes — an SVG's own box is not a hit target — so the
  // tooltip fires on a 2px line and nowhere else. Added last so it sits on top.
  svg.appendChild(svgEl('rect', {
    x: pad.l, y: pad.t, width: Math.max(0, w - pad.l - pad.r), height: Math.max(0, h - pad.t - pad.b),
    fill: 'transparent',
  }));
  host.appendChild(svg);

  const tip = el('div', { class: 'tooltip' });
  host.appendChild(tip);
  svg.addEventListener('pointerleave', () => { tip.classList.remove('show'); hover.setAttribute('opacity', '0'); });
  svg.addEventListener('pointermove', (ev) => {
    const rect = svg.getBoundingClientRect();
    const relX = ((ev.clientX - rect.left) / rect.width) * w;
    const dataX = xMin + ((relX - pad.l) / (w - pad.l - pad.r)) * (xMax - xMin);
    let best = null;
    for (const p of live[0].points) if (!best || Math.abs(p[0] - dataX) < Math.abs(best[0] - dataX)) best = p;
    if (!best) return;
    hover.setAttribute('x1', px(best[0])); hover.setAttribute('x2', px(best[0]));
    hover.setAttribute('opacity', '0.5');
    const lines = [xFmt(best[0])];
    for (const s of live) {
      const p = s.points.find((q) => q[0] === best[0]);
      if (p) lines.push(s.name + ': ' + (opts.tipFmt || yFmt)(p[1]));
    }
    tip.textContent = lines.join('\n');
    tip.classList.add('show');
    const hostRect = host.getBoundingClientRect();
    tip.style.left = Math.min(hostRect.width - 190, Math.max(0, ev.clientX - hostRect.left + 12)) + 'px';
    tip.style.top = Math.max(0, ev.clientY - hostRect.top - 10) + 'px';
  });

  host.appendChild(el('div', { class: 'legend' }, ...live.map((s) =>
    el('span', {}, el('i', { style: 'background:' + s.color }), s.name))));
}

/**
 * barRows(host, rows) — rows: [{label, value, display, max, desc, formula, color, available}]
 *
 * `formula` stays in the layout; `desc` folds into a disclosure. That split is the point:
 * five bars each carrying three lines of prose put fifteen lines of documentation between
 * the reader and the next number — but two ratios that differ ONLY in their divisor must
 * never look like the same measurement, so the arithmetic stays on screen and only the
 * explanation becomes opt-in.
 */
function barRows(host, rows, opts = {}) {
  clear(host);
  if (!rows.length) { emptyState(host, 'Nothing to show yet', opts.emptyDetail || ''); return; }
  const max = Math.max(...rows.map((r) => Math.abs(r.max !== undefined ? r.max : r.value)), 1);
  const wrap = el('div', { class: 'bars' });
  for (const r of rows) {
    const width = r.available === false ? 0 : Math.min(100, (Math.abs(r.value) / max) * 100);
    const row = el('div', { class: 'bar-row' },
      el('div', { class: 'bar-label', text: r.label }),
      el('div', { class: 'bar-track' }, el('div', {
        class: 'bar-fill' + (r.value < 0 ? ' neg' : ''),
        style: 'width:' + width + '%' + (r.color ? ';background:' + r.color : ''),
      })),
      el('div', { class: 'bar-val' + (r.available === false ? ' na' : ''), text: r.display }));
    if (r.formula) row.appendChild(el('div', { class: 'bar-formula', text: r.formula }));
    wrap.appendChild(row);
    if (r.desc) wrap.appendChild(whyBlock('', r.desc, r.descSummary || opts.descSummary || ('Why: ' + r.label)));
  }
  host.appendChild(wrap);
}

/**
 * whyBlock is the single disclosure used everywhere a long explanation used to sit
 * permanently in the layout. Native <details>: focusable and keyboard-operable for free,
 * no JS, and collapsed by default so the prose costs nothing until it is wanted.
 *
 * An empty `summary` draws the glyph-only form (see .bars > details.why), which needs
 * `label` to carry the accessible name a visible word would otherwise have provided.
 */
function whyBlock(summary, text, label) {
  return el('details', { class: 'why' },
    el('summary', { text: summary, 'aria-label': label || null, title: label || null }),
    el('p', { text: text }));
}

/**
 * SERIES_SPENT / SERIES_SAVED are the two categorical slots of the spent-against-saved
 * comparison, taken from the design system's fixed series order (--s1 first, --s3 third)
 * rather than picked by eye. Validated as a pair against both the light and the dark chart
 * surface: lightness band, chroma floor, deuteranopia separation (ΔE 19.6 light / 17.5
 * dark) and 3:1 contrast all pass.
 *
 * Blue against green is the one pair tritanopia struggles with (ΔE ~4), which is why every
 * bar below is DIRECTLY LABELLED with its own value and the legend is always present:
 * identity never rests on the hue alone.
 */
const SERIES_SPENT = SERIES[0];
const SERIES_SAVED = SERIES[2];

/**
 * pairedBars(host, rows) draws two bars per row against ONE shared scale.
 *
 * One scale, deliberately: both figures are dollars, and a second axis scaled to fit the
 * smaller series would make a day that saved a tenth of what it spent look like a day that
 * broke even. Both bars carry their own value as text, so the chart reads correctly with
 * the colours ignored entirely.
 *
 * rows: [{label, a, b, aDisplay, bDisplay, note, unknown}]
 *   a/b       the two magnitudes (a = spent, b = saved), signed
 *   unknown   true when the group has no priced rows — drawn as no bars and the word
 *             "unknown", never as a zero, which would read as "this cost nothing"
 */
function pairedBars(host, rows, opts = {}) {
  clear(host);
  if (!rows.length) { emptyState(host, opts.empty || 'Nothing to show yet', opts.emptyDetail || ''); return; }
  const max = Math.max(...rows.flatMap((r) => [Math.abs(r.a || 0), Math.abs(r.b || 0)]), Number.MIN_VALUE);
  const wrap = el('div', { class: 'bars' });
  const bar = (v, color) => el('div', { class: 'bar-track' }, el('div', {
    class: 'bar-fill' + (v < 0 ? ' neg' : ''),
    style: 'width:' + Math.min(100, (Math.abs(v) / max) * 100) + '%' + (v >= 0 ? ';background:' + color : ''),
  }));
  for (const r of rows) {
    wrap.appendChild(el('div', { class: 'bar-row' },
      el('div', { class: 'bar-label', text: r.label },
        r.note ? el('span', { class: 'bar-sub', text: r.note }) : null),
      r.unknown ? el('div', { class: 'bar-track' }) : el('div', { class: 'bar-pair' },
        bar(r.a || 0, SERIES_SPENT), bar(r.b || 0, SERIES_SAVED)),
      el('div', { class: 'bar-val' + (r.unknown ? ' na' : '') },
        el('div', { text: r.unknown ? 'unknown' : r.aDisplay }),
        r.unknown ? null : el('div', { class: 'bar-val-b', text: r.bDisplay }))));
  }
  host.appendChild(wrap);
  // A legend for two series is not optional: the swatch is what ties the second, thinner
  // bar in each row to the word "saved".
  host.appendChild(el('div', { class: 'legend' },
    el('span', {}, el('i', { style: 'background:' + SERIES_SPENT }), opts.aLabel || 'Spent'),
    el('span', {}, el('i', { style: 'background:' + SERIES_SAVED }), opts.bLabel || 'Saved')));
}

// ── overview ───────────────────────────────────────────────────────────────
function tile(key, label, value, sub, cls) {
  return el('div', { class: 'tile ' + (cls || ''), 'data-testid': 'tile-' + key },
    el('div', { class: 'k', text: label }),
    el('div', { class: 'v', 'data-testid': 'tile-' + key + '-value', text: value }),
    sub ? el('div', { class: 's', text: sub }) : null);
}

/** tileGroup renders one labelled band of tiles. */
function tileGroup(label, note, tiles, cls) {
  const frag = document.createDocumentFragment();
  if (label) {
    frag.appendChild(el('div', { class: 'section' },
      el('h2', { text: label }),
      note ? el('span', { class: 'section-note', text: note }) : null));
  }
  frag.appendChild(el('div', { class: 'tiles' + (cls ? ' ' + cls : '') }, ...tiles));
  return frag;
}

function renderTiles(o) {
  const host = clear($('#tiles'));
  const exact = (o.accounting && o.accounting.complete) || 0;
  const costKnown = exact > 0;

  // The headline row answers the only question someone opening this page has: did it
  // save money, did it save tokens, and over how much traffic. Everything else is the
  // evidence for those three, so it sits below them in labelled groups rather than
  // beside them at the same weight.
  // A tile's sub-line is kept only where it carries a FACT the label cannot: a formula, a
  // second number, or the distinguisher between two tiles that would otherwise read the
  // same ("gross" vs "unique"). Anything that only restated the label is gone — "Tokens
  // before / content tokens in" was a caption explaining a caption.
  host.appendChild(tileGroup(null, null, [
    // Our two savings, added: compaction's and the prefix components'. Both are ours and
    // the token sets are disjoint. The provider's own cache saving is much larger and is
    // NOT in here — it was, under the label "compaction + provider cache", and a headline
    // number that mostly measures somebody else's mechanism is not a headline number.
    tile('total-saved-usd', 'Total dollars avoided', costKnown ? usd(o.total_saved_usd) : 'unknown',
      'compaction + prefix cache', costKnown ? (o.total_saved_usd < 0 ? 'bad' : 'good') : ''),
    tile('saved-usd', 'Net dollars saved', costKnown ? usd(o.net_saved_usd) : 'unknown',
      'baseline − actual − our spend', costKnown ? (o.net_saved_usd < 0 ? 'bad' : 'good') : ''),
    tile('saved-unique', 'Tokens saved (unique)', compact(o.saved_unique),
      compact(o.replay_tokens) + ' re-earned on later turns', 'accent'),
    // Risk beside value, at the SAME altitude, because on this traffic it is 22x larger than
    // the savings two tiles to the left and it used to sit below the fold as a diagnostic
    // paragraph. It is subtracted from nothing (mutation is not randomly assigned; see the
    // note under the cache-miss chart) — but a page that shows a $7 saving in 48-point type
    // and a $157 exposure in a folded footnote is not honest about which number is bigger.
    tile('prefix-change-exposure', 'Prefix-change exposure',
      costKnown ? usd(o.prefix_change_cost_all_usd) : 'unknown',
      num(o.prefix_change_requests_all) + ' turns re-billed · not netted',
      o.prefix_change_cost_all_usd > 0 ? 'bad' : ''),
    tile('requests', 'Requests', num(o.requests), num(o.sessions) + ' sessions'),
  ], 'headline'));

  host.appendChild(tileGroup('Content tokens', 'three ways to count the same removal', [
    tile('tokens-before', 'Tokens before', compact(o.tokens_before)),
    tile('tokens-after', 'Tokens after', compact(o.tokens_after)),
    tile('saved-gross', 'Saved (gross)', compact(o.saved_gross), 'recounts re-sent history'),
    // The label has to name the UNIQUE calculation, which dominates this figure, and not
    // only the restore subtraction, which is usually zero: sitting between "Saved (gross)
    // 17k" and "Overcount 1.7×", "Saved (net of restores)" invited reading it as
    // gross-minus-restores and made a 10k number look like an arithmetic error.
    tile('saved-adjusted', 'Saved (unique − restores)', compact(o.saved_adjusted),
      compact(o.expand_tokens) + ' asked back', o.saved_adjusted < 0 ? 'bad' : ''),
    // Was "Overcount ratio · gross ÷ unique", i.e. the replay presented as a discount
    // against us. It is the opposite: a reduction frozen at turn N is still absent from every
    // later turn the agent re-sends, so it is EARNED again on each of them — and that replay
    // is already priced per turn into baseline_cost_usd and into every component's saved_usd,
    // at the tier each turn actually paid. Same arithmetic, correct direction.
    tile('overcount', 'Each reduction re-earned', o.overcount_ratio ? o.overcount_ratio.toFixed(1) + '×' : '—',
      'gross ÷ unique · already priced in'),
  ]));

  // The replay, and the ceiling on it. Both were absent: the realized figure was labelled as
  // an over-count, and how much replay the cache-safety freeze forgoes had no number at all.
  host.appendChild(tileGroup('Amortization', 'what the freeze forgoes', [
    tile('replay-realized', 'Replay realized', compact(o.replay_tokens),
      'reductions re-earned on later turns'),
    tile('replay-projected', 'Replay ceiling', compact(o.replay_projected_tokens),
      'if each survived to end of session'),
    tile('replay-pct', 'Of that ceiling', o.replay_projected_tokens
      ? pct(o.replay_realized_pct, 1) : '—',
      compact(o.frozen_tokens) + ' tok frozen for cache safety',
      o.replay_projected_tokens && o.replay_realized_pct < 25 ? 'bad' : ''),
  ]));

  host.appendChild(tileGroup('Cost', costKnown ? 'billed, and the counterfactual' : 'no priced requests in this window', [
    tile('cost-baseline', 'Baseline cost', costKnown ? usd(o.baseline_cost_usd) : 'unknown',
      costKnown ? 'without context-guru' : 'needs all four tiers'),
    tile('cost-actual', 'Actual cost', costKnown ? usd(o.cost_usd) : 'unknown',
      costKnown ? 'as billed' : 'needs all four tiers'),
    tile('cost-cg', 'Our own LLM cost', costKnown ? usd(o.cg_llm_cost_usd) : 'unknown',
      'extract_llm, summarize'),
    // The cache saving this project claims, and the only one. Three conditions per request:
    // the volatile-tail split ran, the provider then READ that prefix from cache, and it was
    // the session's FIRST request — the one that would have missed. And the amount is the
    // stable half the split moved, not the whole cache_read: the cachesplit-off control arm
    // still read 45,805 tokens, so only the 8,499-token difference was ever ours.
    //
    // What was here before was the provider's whole cache saving ("Prompt-cache savings")
    // plus the subset that merely co-occurred with our components. On the traffic that
    // measured this, 23x larger — and neither of them a thing we did.
    // Measured + the pre-instrumentation window, added, because the question "what has this
    // component earned" is about the whole history and the split of it into measured and
    // valued-on-read is an implementation detail of when we started recording. The
    // decomposition is one group down, and the historical half is absent (not zero) when it
    // could not be priced.
    tile('cachesplit-saved', 'Prefix-cache savings',
      costKnown ? usd(o.cachesplit_saved_usd + histUSD(o)) : 'unknown',
      num(o.split_credited) + ' of ' + num(o.split_tail_moved) + ' moved-snapshot turns'
        + (histRequests(o) ? ' + ' + num(histRequests(o)) + ' earlier' : ''),
      costKnown && (o.cachesplit_saved_usd + histUSD(o)) > 0 ? 'good' : ''),
  ]));

  // The bill, split by the tier it was charged on, and the only defensible denominator for a
  // compaction percentage. There were no cost columns here at all — only token counts — so
  // "0.28% of spend" was silently divided by a bill that is two-thirds OUTPUT tokens, which no
  // input-side transformation can reach. Absent, not zeroed, when the rates are unknown.
  if (o.tier_costs) {
    const t = o.tier_costs;
    const addrPct = t.total_usd > 0 ? (100 * t.addressable_usd) / t.total_usd : null;
    host.appendChild(tileGroup('Addressable spend', 'the part of the bill compaction can reach', [
      tile('addressable', 'Addressable spend', usd(t.addressable_usd),
        addrPct === null ? 'input side (derived)' : 'input side · ' + pct(addrPct, 0)
          + ' of the bill, derived', 'accent'),
      tile('saved-of-addressable', 'Saved, of addressable',
        t.addressable_usd > 0 ? pct((100 * o.total_saved_usd) / t.addressable_usd, 2) : '—',
        'not of the whole bill', o.total_saved_usd < 0 ? 'bad' : ''),
      tile('cost-output', 'Output tokens cost', usd(t.output_usd),
        'nothing here is reachable'),
      // The reconciliation. These four tiers are priced at TODAY's rates while cost_usd was
      // priced when each request was served, so the two disagree by whatever the rates moved
      // over the window. Showing both is the only way a reader can tell a rate change from an
      // arithmetic error, and hiding the derived total would have been the dishonest option.
      // The reconciliation, and it is load-bearing rather than decorative. These four tiers
      // are priced at TODAY's rates over tokens billed at whatever the rate was then, so a
      // gateway price change inside the window makes the split drift from the bill. Measured
      // on production: two distinct rate regimes per model, so the drift is real and it is
      // ~15%. A split that disagrees with the bill by more than a rounding error must SAY it
      // is an estimate; the alternative is a reader treating "8% of the bill is output" as a
      // measurement when it is a derivation.
      tile('tier-reconcile', 'At today\'s rates', usd(t.total_usd),
        'billed ' + usd(t.stored_usd)
          + (t.stored_usd > 0 && Math.abs(t.total_usd - t.stored_usd) / t.stored_usd > 0.05
            ? ' · ' + pct((100 * (t.total_usd - t.stored_usd)) / t.stored_usd, 0) + ' rate drift'
            : '')
          + (t.uncovered_requests ? ' · ' + num(t.uncovered_requests) + ' unpriced' : ''),
        t.stored_usd > 0 && Math.abs(t.total_usd - t.stored_usd) / t.stored_usd > 0.05 ? 'bad' : ''),
    ]));
  }

  // Why the figure above is the size it is. Without these three counts a small number is
  // indistinguishable from a broken component — which is exactly what happened: gated on the
  // session's first request it read ~$0, and the reason (1,105 of 1,127 session starts had no
  // cache to hit) was invisible.
  // The honest framing of a $0.03 figure. The credit fires only when the environment snapshot
  // MOVED, and on real Claude Code traffic it does not move within a session — one distinct
  // tail hash across the 257 turns of the largest measured session, because the snapshot is
  // captured once per process. So the mechanism is verified and its measured value on this
  // traffic is ~$0. The −34.1% that the A/B measured was CROSS-SESSION reuse, which this
  // per-request credit condition cannot see, and inventing a number for it here would be
  // worse than reporting the zero.
  host.appendChild(tileGroup('Prefix split',
    'mechanism verified; the credit only fires when the snapshot MOVES, and it does not move '
    + 'within a session — the A/B measured cross-session reuse, which this cannot see', [
    // "acted on", not "ran on": the component runs on every request and does nothing on most.
    // Three of this deployment's largest accounts have it running on 125, 1,035 and 1,972
    // requests and acting on NONE of them — their system prompts carry no volatile tail to
    // split. All zeros here with a nonzero run count on the Components tab is that case, and
    // it is a fact about their prompt, not a fault.
    tile('split-requests', 'Requests it acted on', num(o.split_requests),
      o.split_requests === 0 ? 'no volatile tail to split' : ''),
    // ONE definition of "moved", shared with the write path: differs from the last non-zero
    // tail hash recorded for the session, and a session with no earlier one counts as moved.
    // This tile used to compare against the previous ROW's hash including 0 — where 0 means
    // "nothing was split on that turn" — while the credit used a map that only remembered
    // non-zero hashes, so the page read 844 acted / 314 moved / 3 credited and looked broken.
    tile('split-tail-moved', 'Snapshot had moved', num(o.split_tail_moved),
      'the turns it can earn on'),
    tile('split-credited', 'Served from cache', num(o.split_credited),
      'the turns it did earn on'),
    // The reconciliation, and it is not decoration. A credited turn the recomputation does not
    // call moved was credited because the write-time map had FORGOTTEN a session it had seen
    // (proxy restart, or eviction) and read a first sighting where there was none. On the
    // stored corpus that is all three of them. The write path no longer does this, so a gap
    // here means pre-fix rows are still inside the window — not that the recount is wrong.
    tile('split-credited-moved', 'Credited and moved', num(o.split_credited_moved),
      o.split_credited > o.split_credited_moved
        ? num(o.split_credited - o.split_credited_moved) + ' credited before the write-path fix'
        : 'the two definitions agree',
      o.split_credited > o.split_credited_moved ? 'bad' : ''),
    tile('split-hit-rate', 'Of moved snapshots', o.split_tail_moved > 0
      ? pct((100 * o.split_credited) / o.split_tail_moved) : '—',
      'kept out of the write tier'),
    // The pre-instrumentation window, valued on read and never stored. Its own tile so the
    // measured figure above is never confused with an estimate — and "—" when it could not be
    // priced, because an unpriced number must not read as "saved nothing".
    tile('split-historical', 'Before we recorded it',
      o.cachesplit_historical ? usd(o.cachesplit_historical.usd) : '—',
      o.cachesplit_historical
        ? num(o.cachesplit_historical.requests) + ' session starts, valued on read'
        : 'no rates for these models'),
  ]));

  // Tokens AND what each tier cost. The cost half was missing entirely, which is why the one
  // fact that explains every small percentage on this page — most of the bill is output —
  // could only be inferred by dividing columns by hand.
  const tc = o.tier_costs;
  host.appendChild(tileGroup('Billed tokens', 'the four tiers the provider charges on', [
    tile('cache-read', 'Cache reads', compact(o.cache_read), tc ? usd(tc.cache_read_usd) : null),
    tile('cache-write', 'Cache writes', compact(o.cache_write),
      tc ? usd(tc.cache_write_usd) + ' · ~11.5× a read' : '~11.5× a read'),
    tile('fresh-input', 'Fresh input', compact(o.fresh_input), tc ? usd(tc.fresh_usd) : null),
    tile('output', 'Output tokens', compact(o.output_tokens), tc ? usd(tc.output_usd) : null),
  ]));

  host.appendChild(tileGroup('Latency and safety', 'the price of compaction', [
    tile('cg-latency', 'context-guru latency', ms(o.cg_latency_ms_avg), 'p95 ' + ms(o.cg_latency_ms_p95)),
    tile('upstream-latency', 'Upstream latency', ms(o.upstream_ms_avg), 'p95 ' + ms(o.upstream_ms_p95)),
    tile('expands', 'Restorations', num(o.expands),
      pct(o.expand_rate * 100) + ' of requests · ' + compact(o.expand_tokens) + ' tok',
      o.expands > 0 ? 'bad' : ''),
    tile('reverts', 'Reverts', num(o.reverts), 'never-worse guard'),
    tile('passthroughs', 'Not compacted', num(o.passthroughs)),
  ]));
}

function renderDenominators(o) {
  barRows($('#denominators'), (o.denominators || []).map((d) => ({
    label: d.label,
    value: d.available ? d.percent : 0,
    max: 100,
    display: d.available ? pct(d.percent, 2) : 'n/a',
    available: d.available,
    // The divisor is what makes these four bars four different measurements, so it is the
    // half that stays visible. The prose folds.
    formula: d.available
      ? `${compact(d.numerator)} ÷ ${compact(d.denominator)} tokens`
      : 'inputs unavailable in this window',
    desc: d.description,
    descSummary: 'Why this denominator',
  })), { emptyDetail: 'No requests match the filter.' });
}

function renderWaterfall(o) {
  const host = clear($('#waterfall'));
  const steps = o.waterfall || [];
  if (!steps.length || !o.baseline_cost_usd) {
    emptyState(host, 'No priced requests yet',
      'The waterfall needs provider usage data (all four token tiers) and a known model price.');
    return;
  }
  const max = Math.max(...steps.map((s) => Math.abs(s.delta_usd)), 0.0001);
  const wrap = el('div', { class: 'bars' });
  for (const s of steps) {
    const color = s.total ? SERIES[0] : s.delta_usd < 0 ? 'var(--good)' : 'var(--bad)';
    wrap.appendChild(el('div', { class: 'bar-row' },
      el('div', { class: 'bar-label', text: s.label }),
      el('div', { class: 'bar-track' }, el('div', {
        class: 'bar-fill', style: `width:${(Math.abs(s.delta_usd) / max) * 100}%;background:${color}`,
      })),
      el('div', { class: 'bar-val', text: (s.delta_usd < 0 ? '−' : s.total ? '' : '+') + usd(Math.abs(s.delta_usd)) })));
    wrap.appendChild(whyBlock('', s.description, 'Why: ' + s.label));
  }
  host.appendChild(wrap);
}

function renderDistribution(hostSel, map, labels, detail) {
  const host = clear($(hostSel));
  const entries = Object.entries(map || {}).filter(([, v]) => v > 0);
  if (!entries.length) { emptyState(host, 'No requests in this window', detail || ''); return; }
  entries.sort((a, b) => b[1] - a[1]);
  const total = entries.reduce((n, [, v]) => n + v, 0);
  barRows(host, entries.map(([k, v]) => ({
    label: (labels && labels[k]) || (k === '' ? 'compacted' : k),
    value: v, max: total,
    display: num(v) + '  (' + pct((v / total) * 100, 0) + ')',
  })));
}

function renderSafety(o) {
  const s = o.safety_cost || {};
  $('#safety-note').textContent = s.description || '';
  // These five rows are five different units (tokens, runs, ms, dollars), so the bar
  // lengths are a rough sense of scale within each row's own description and nothing
  // more. One hue throughout, because five hues would imply five plotted series;
  // the one row that is a genuine PENALTY rather than a price gets the status colour.
  barRows($('#safety'), [
    // The benefit half, in dollars. This row has always PROMISED it in prose — "its benefit is
    // the cache reads it preserved" — and never computed it, so 396.5M frozen tokens sat here
    // as a cost with no counterpart. Two figures, because one would have been ambiguous: what
    // the frozen prefix was billed as at the read rate, and what re-creating it at the write
    // rate instead would have added. The second is the freeze's actual purchase.
    { label: 'Frozen for cache safety', value: s.frozen_tokens || 0, display: compact(s.frozen_tokens) + ' tok',
      formula: s.priced
        ? 'billed ' + usd(s.frozen_read_usd) + ' as reads · avoided ' + usd(s.frozen_write_risk_usd)
          + ' of re-creation'
        : 'unpriced — no rates for these models',
      desc: 'Compaction we deliberately did NOT do on the already-cached prefix, because '
            + 'rewriting a cached prefix re-bills the whole suffix at the creation rate. '
            + (s.priced
              ? 'Those ' + compact(s.frozen_tokens) + ' tokens were billed as cache reads for '
                + usd(s.frozen_read_usd) + '; re-creating them instead would have added '
                + usd(s.frozen_write_risk_usd) + ', which is what the freeze bought. The cost '
                + 'is the compaction not done — see the amortization ceiling above.'
              : 'The benefit cannot be priced in this window: no rates for these models. '
                + 'Reported as unpriced rather than as zero.') },
    { label: 'Restored after offload', value: s.restored_tokens || 0, display: compact(s.restored_tokens) + ' tok',
      color: 'var(--bad)', formula: 'paid for twice',
      desc: 'Content we removed and the model asked back for — a premature offload, paid for twice.' },
    { label: 'Reverted component runs', value: s.reverted_runs || 0, display: num(s.reverted_runs) + ' runs',
      desc: 'The never-worse guard rolling a component back. Safety working, and its cost is the ' +
            'latency of the attempt.' },
    { label: "context-guru's own latency", value: s.cg_latency_ms_total || 0, display: dur(s.cg_latency_ms_total),
      desc: 'Total wall time context-guru itself added across the window.' },
    { label: "context-guru's own LLM spend", value: (s.cg_llm_cost_usd || 0) * 1000, display: usd(s.cg_llm_cost_usd),
      formula: 'paid out of the savings above',
      desc: "context-guru's own model calls (extract_llm, summarize), paid out of the savings above." },
  ]);
}

/**
 * renderPrefixChangeCost surfaces prefix_change_cost_usd, beside the prefix_change bucket it
 * is computed from — and says in the sentence itself that it is a DIAGNOSTIC that is
 * subtracted from nothing.
 *
 * It is not a tile and it is not summed into any saving: components act where there is
 * something to act on, which are also the long churny turns most likely to break a prefix on
 * their own, so mutation is not randomly assigned and this is observational. Netting it would
 * book a correlation as a debt. It is nonetheless larger than every saving on this page on
 * some corpora, so hiding it would be the dishonest option.
 */
function renderPrefixChangeCost(o) {
  const n = $('#prefix-change-note');
  if (!n) return;
  const v = o.prefix_change_cost_usd, all = o.prefix_change_cost_all_usd;
  // Shown whenever EITHER figure exists. It used to hide on the conditional one alone, so a
  // window with a large prefix-change bill and no mutation adjacent to it said nothing at all.
  n.hidden = !v && !all;
  if (n.hidden) return;
  clear(n);
  n.appendChild(el('strong', { text: 'Diagnostic, not netted: ' + usd(all)
    + ' over ' + num(o.prefix_change_requests_all) + ' turns' }));
  n.appendChild(document.createTextNode(
    ' re-billed a whole prompt on a changed prefix — the failure mode this project exists to '
    + 'avoid, and larger than every saving on this page. Of that, ' + usd(v) + ' over '
    + num(o.prefix_change_requests) + ' turns'
    + ' was billed on turns whose cache missed directly after a turn we '
    + 'mutated. That is where "we rewrote history and the next turn re-billed the whole '
    + 'prompt" is a live hypothesis — but components act on exactly the long, churny turns '
    + 'most likely to break a prefix by themselves, so this is a correlation. It is '
    + 'subtracted from no savings figure on this dashboard; settling it needs the A/B.'));
}

function renderLive() {
  const body = clear($('#live-body'));
  // The feed is the raw capture stream: it is not filtered, and with a filter bar right
  // above it that is the sort of disagreement a reader blames on the numbers.
  const note = $('#live-filter-note');
  const active = activeFilters();
  note.hidden = !active.length;
  if (active.length) {
    note.textContent = 'Not filtered — shows traffic outside ' + describeFilters() + ' too.';
  }
  if (!state.live.length) {
    tableMessage(body, 8, 'Waiting for traffic',
      'Requests appear here the moment they are captured.');
    return;
  }
  for (const e of state.live.slice(0, 25)) {
    body.appendChild(el('tr', { class: 'click', onclick: () => openRequest(e.id) },
      el('td', { text: when(e.ts) }),
      el('td', {}, el('span', { class: 'trunc', title: e.session_id, text: e.session_id || '—' })),
      el('td', { text: e.model || '—' }),
      el('td', { class: 'num', text: compact(e.tokens_before) }),
      el('td', { class: 'num', text: compact(e.tokens_after) }),
      el('td', { class: 'num', text: compact(e.tokens_before - e.tokens_after) }),
      el('td', { class: 'num', text: ms(e.cg_latency_ms) }),
      el('td', {}, el('span', { class: 'pill ' + e.token_accounting, text: e.token_accounting }))));
  }
}

async function loadOverview() {
  loadingState($('#tiles'), 4);
  try {
    const [o, s] = await Promise.all([api('stats'), api('series', { bucket: bucketFor() })]);
    state.overview = o;
    renderTiles(o);
    renderDenominators(o);
    renderWaterfall(o);
    renderSafety(o);
    renderDistribution('#cachemiss', o.cache_miss, {
      hit: 'cache hit', cold_start: 'cold start (not a failure)', ttl_expiry: 'TTL expiry',
      prefix_change: 'prefix change', unknown: 'unknown', '': 'no cache data',
    }, 'Every request carries a cache attribution once one has been captured in this window.');
    renderPrefixChangeCost(o);
    // "below every trigger" is gone. The label named components.Trigger, which is not
    // consulted in that decision at all — and it was believed: it was reported upward as
    // "$744.62 of spend was gated by the trigger" and cost an investigation to disprove. The
    // real cause is that every deployed component rewrites `role == tool` content and nothing
    // else, so a request with no tool output has nothing any of them can touch. The legacy
    // slug is mapped to the same words as its replacement so one history reads as one cause.
    renderDistribution('#reasons', o.uncompressed, {
      '': 'compacted', bypassed: 'bypassed by header',
      no_tool_output: 'no tool output to rewrite',
      no_candidate: 'no candidate in the eligible tail',
      below_trigger: 'no candidate in the eligible tail (pre-rename)',
      cache_frozen: 'frozen for cache safety', found_nothing: 'nothing to remove',
      reverted: 'all components reverted', no_messages: 'no messages',
    }, 'Nothing was skipped — every captured request in this window was compacted.');
    renderDistribution('#accounting', o.accounting, {
      complete: 'exact (all four tiers)', partial: 'estimated', missing: 'unmeasured',
    }, 'Fills in from the first captured request: every row is counted as exact, estimated or unmeasured.');
    renderSeries(s.buckets || []);
  } catch (err) {
    if (aborted(err)) return;
    errorState($('#tiles'), 'Could not load statistics', err);
  }
}

function bucketFor() {
  const [since, until] = rangeMs();
  if (since === 0) return 3600000;
  const span = (until || state.nowMs || Date.now()) - since;
  if (span <= 3600000) return 60000;
  if (span <= 86400000) return 300000;
  return 3600000;
}

function renderSeries(buckets) {
  if (!buckets.length) {
    for (const id of ['#chart-cost', '#chart-tokens', '#chart-cache', '#chart-latency', '#chart-volume']) {
      emptyState($(id), 'No data in this window', 'Send traffic through the proxy, or widen the time range.');
    }
    return;
  }
  // Cumulative NET SAVING, not the two cost lines it is the difference of.
  //
  // This used to draw baseline and actual and tell the reader "the area between the lines is
  // the money". On real traffic those lines are 0.28% apart, so there was no visible area and
  // the caption described something the chart could not show. The saving itself is the series
  // worth plotting: it starts at zero, so its own scale is the scale of the money, and it
  // crosses below zero exactly when our own spend overtakes what compaction avoided — which is
  // the one event this chart should make impossible to miss. The two cost totals are still on
  // the page, as tiles, where a number that differs in the third decimal belongs.
  let cumBase = 0, cumAct = 0;
  const saved = [];
  for (const b of buckets) {
    cumBase += b.baseline_cost_usd;
    cumAct += b.cost_usd + b.cg_llm_cost_usd;
    saved.push([b.ts, cumBase - cumAct]);
  }
  const anyCost = cumBase > 0 || cumAct > 0;
  if (anyCost) {
    lineChart($('#chart-cost'), [
      { name: 'Cumulative net saving (baseline − billed − our own spend)',
        color: SERIES[0], points: saved, area: true },
    ], { yFmt: usd, tipFmt: usd,
      label: 'cumulative net saving; below zero means context-guru cost more than it saved' });
  } else {
    emptyState($('#chart-cost'), 'No priced requests yet',
      'Cost needs provider usage data (all four token tiers) and a known model price. Token charts below still work.');
  }

  // Hue assignment is by ENTITY and holds across all five charts: blue is always the
  // with-context-guru / primary measure, orange always the baseline it is compared
  // against, aqua always the third measure.
  lineChart($('#chart-tokens'), [
    { name: 'Tokens before', color: SERIES[1], points: buckets.map((b) => [b.ts, b.tokens_before]) },
    { name: 'Tokens after', color: SERIES[0], points: buckets.map((b) => [b.ts, b.tokens_after]), area: true },
    { name: 'Saved (unique)', color: SERIES[2], points: buckets.map((b) => [b.ts, b.saved_unique]) },
  ], { label: 'content tokens over time' });

  lineChart($('#chart-cache'), [
    { name: 'Cache reads', color: SERIES[0], points: buckets.map((b) => [b.ts, b.cache_read]), area: true },
    { name: 'Cache writes', color: SERIES[1], points: buckets.map((b) => [b.ts, b.cache_write]) },
    { name: 'Fresh input', color: SERIES[2], points: buckets.map((b) => [b.ts, b.fresh_input]) },
  ], { label: 'cache reads versus writes over time' });

  lineChart($('#chart-latency'), [
    { name: 'context-guru added (avg)', color: SERIES[0], points: buckets.map((b) => [b.ts, b.cg_latency_ms_avg]) },
    { name: 'Upstream round-trip (avg)', color: SERIES[1], points: buckets.map((b) => [b.ts, b.upstream_ms_avg]), dashed: true },
  ], { yFmt: ms, tipFmt: ms, label: 'latency over time' });

  lineChart($('#chart-volume'), [
    { name: 'Requests', color: SERIES[0], points: buckets.map((b) => [b.ts, b.requests]), area: true },
    { name: 'Restorations (expands)', color: SERIES[1], points: buckets.map((b) => [b.ts, b.expands]) },
    { name: 'Cache misses', color: SERIES[2], points: buckets.map((b) => [b.ts, b.cache_misses]) },
  ], { yFmt: num, label: 'request volume and restorations' });
}

// ── components ─────────────────────────────────────────────────────────────
/**
 * verdict summarises whether a component earns its place, from what it saved
 * against what it cost. Order matters: a component that burned real wall time for
 * nothing is a worse finding than one that simply never fired, so the cost test
 * comes FIRST — otherwise extract_llm's 15 s of model calls for zero savings reads
 * as a bland "inert here".
 */
function verdict(c) {
  if (c.runs === 0) return ['—', 'neutral'];
  if (c.errors > 0) return ['errors', 'missing'];
  // DOLLARS FIRST, where there are any. A component that makes LLM calls is the only kind
  // that can be net-negative, and until the per-call records existed this function had no
  // money to reason with — so it judged the one component that can lose money on tokens and
  // latency, and could describe a $3 loss as "expensive for its yield".
  if (c.llm_calls > 0) {
    // c.net_usd is the SERVER's figure: saved_usd (summed per turn, so the amortization of
    // a frozen reduction replaying across a session is already in it) minus llm_cost_usd.
    // The browser used to compute this from a hardcoded rate table AND from calls-only
    // savings, which both mispriced it and threw away ~93% of the realized value.
    // The figure the TABLE shows, estimate included. Judging a component's spend against the
    // six rows of its saving that happen to postdate a column is not a verdict.
    const net = c.net_usd_with_estimate || 0;
    if (net < -0.01) return ['underwater ' + usd(net), 'missing'];
    if (net <= 0) return ['break-even', 'partial'];
    return ['net ' + usd(net), 'complete'];
  }
  // Spent >1s of hot-path time and returned nothing: paid for, unused.
  if (c.saved_unique === 0 && c.duration_ms_total > 1000) return ['costly and inert', 'missing'];
  if (c.mutated === 0) return ['inert here', 'partial'];
  if (c.saved_unique === 0) return ['mutates, saves no content', 'neutral'];
  // More than a millisecond of latency per 100 tokens saved.
  if (c.duration_ms_total > 1000 && c.duration_ms_total / c.saved_unique > 0.01) {
    return ['expensive for its yield', 'partial'];
  }
  // Both halves, or a component that only ever moves a cache breakpoint is "rarely fires"
  // on every workload — which is what cachesplit read, on 1,805 requests it mutated.
  if (c.act_rate + (c.act_rate_structural || 0) < 0.02) return ['rarely fires', 'partial'];
  return ['earning its place', 'complete'];
}

/** DAY_MS is dash.DayMs: per-day bars are the shared time series at a day-wide bucket. */
const DAY_MS = 86400000;

/** Labels for the breakdown dimensions the server offers. Unknown keys fall back to the
 *  raw name, so a dimension added server-side appears in the picker without a UI change. */
const DIM_LABELS = {
  model: 'model', provider: 'provider', agent: 'agent', preset: 'preset', mode: 'mode',
  reasoning_effort: 'reasoning effort', thinking_mode: 'thinking mode',
  stop_reason: 'stop reason', tool_choice: 'tool choice',
  cache_miss_reason: 'cache outcome', cache_breakpoints: 'cache_control breakpoints',
  stream: 'streaming',
};

async function loadUsage() {
  const dayHost = $('#chart-daily');
  const breakHost = $('#chart-breakdown');
  const body = clear($('#breakdown-body'));
  loadingRows(body, 10);
  try {
    // Per-day bars need no endpoint of their own: the series is bucketed in SQL from the
    // raw timestamp, so a day-wide bucket is a query parameter.
    const [{ buckets }, bd] = await Promise.all([
      api('series', { bucket: DAY_MS }),
      api('breakdown', { dim: state.dim }),
    ]);
    pairedBars(dayHost, (buckets || []).map((b) => ({
      label: new Date(b.ts).toISOString().slice(0, 10),
      note: num(b.requests) + ' requests · ' + compact(b.tokens_before) + ' → ' + compact(b.tokens_after) + ' tokens',
      a: b.cost_usd + b.cg_llm_cost_usd,
      b: b.saved_usd,
      aDisplay: usd(b.cost_usd + b.cg_llm_cost_usd),
      bDisplay: usd(b.saved_usd),
      unknown: b.baseline_cost_usd === 0 && b.cost_usd === 0,
    })), {
      empty: 'No days in this window',
      emptyDetail: 'Send traffic through the proxy, or widen the time range.',
    });

    syncDimPicker(bd.dimensions || []);
    $('#breakdown-key-head').textContent = DIM_LABELS[bd.dim] || bd.dim;
    const groups = bd.groups || [];
    clear(body);
    if (!groups.length) {
      tableMessage(body, 10, 'Nothing to break down', 'No requests match the current filters.');
      emptyState(breakHost, 'Nothing to break down', 'No requests match the current filters.');
      return;
    }
    pairedBars(breakHost, groups.map((g) => ({
      label: g.key || '(not set)',
      note: num(g.requests) + ' requests',
      a: g.spent_usd,
      b: g.saved_usd,
      aDisplay: usd(g.spent_usd),
      bDisplay: usd(g.saved_usd),
      // Every row unpriced means the money figures for this bar are unknown, not zero.
      unknown: g.incomplete_rows >= g.requests,
    })));
    for (const g of groups) {
      const unpriced = g.incomplete_rows >= g.requests;
      body.appendChild(el('tr', {},
        el('td', { text: g.key || '(not set)' }),
        el('td', { class: 'num', text: num(g.requests) }),
        el('td', { class: 'num', text: compact(g.tokens_before) }),
        el('td', { class: 'num', text: compact(g.tokens_after) }),
        el('td', { class: 'num', text: compact(g.saved_unique) }),
        el('td', { class: 'num' + (unpriced ? ' na' : ''), text: unpriced ? 'unknown' : usd(g.spent_usd) }),
        el('td', { class: 'num' + (unpriced ? ' na' : ''), text: unpriced ? 'unknown' : usd(g.saved_usd) }),
        // Ours, not the provider's: this column used to carry cache_saved_usd, which made
        // every model look like a large cache saving we had nothing to do with.
        el('td', { class: 'num' + (unpriced ? ' na' : ''), text: unpriced ? 'unknown' : usd(g.cachesplit_saved_usd) }),
        el('td', { class: 'num' + (unpriced ? ' na' : ''), text: unpriced ? 'unknown' : usd(g.total_saved_usd) }),
        el('td', { class: 'num', text: num(g.incomplete_rows) })));
    }
  } catch (err) {
    if (aborted(err)) return;
    clear(body);
    tableMessage(body, 10, 'Could not load the breakdown', err.message);
    emptyState(breakHost, 'Could not load the breakdown', err.message, { error: true });
    emptyState(dayHost, 'Could not load daily usage', err.message, { error: true });
  }
}

/** histUSD and histRequests read the pre-instrumentation split figure, which the API OMITS
 *  rather than zeroes when it cannot price it — so every read of it has to tolerate absence. */
function histUSD(o) { return (o.cachesplit_historical || {}).usd || 0; }
function histRequests(o) { return (o.cachesplit_historical || {}).requests || 0; }

/** syncDimPicker fills the dimension picker from what the SERVER says it can group by,
 *  so the options can never name a dimension the query would reject. */
function syncDimPicker(dims) {
  const sel = $('#f-dim');
  if (sel.options.length !== dims.length) {
    clear(sel);
    for (const d of dims) sel.appendChild(el('option', { value: d }, DIM_LABELS[d] || d));
  }
  sel.value = state.dim;
}

/**
 * gateSummary renders a component's gate counts: the reasons it turned candidates away,
 * commonest first.
 *
 * This is the answer to the question the components table could not answer before —
 * "act rate 0%, why?" — and it is the difference between a Bob user concluding
 * context-guru does nothing and reading `no_filter_match 15`, which says the heuristics
 * were written for another agent's tool-output shapes.
 *
 * Three states, all different: a populated map is the reasons; an EMPTY map is "this
 * component turned nothing away"; a MISSING map is "unknown" — on a request row that means
 * it was written before the column existed, and on an aggregate row that no row in the
 * window carried gate data at all.
 */
function gateSummary(gates) {
  if (!gates) return el('span', { class: 'na', text: 'unknown' });
  const all = Object.entries(gates).sort((a, b) => b[1] - a[1]);
  if (!all.length) return el('span', { class: 'na', text: '—' });
  const shown = all.slice(0, 2).map(([k, v]) => k + ' ' + num(v)).join(' · ');
  const rest = all.length > 2 ? ' +' + (all.length - 2) : '';
  return el('span', { title: all.map(([k, v]) => k + ' ' + num(v)).join('\n'), text: shown + rest });
}

/**
 * COMPONENT_SORT is one field per column of the components table, in column order. null is
 * a column with nothing orderable in it (the gate summary, the verdict prose).
 */
const COMPONENT_SORT = ['component', 'kind', 'runs', 'acted_tokens', 'acted_structural',
  'act_rate', 'reverted',
  'saved_unique', 'saved_gross', 'overcount_ratio', 'duration_ms_total', 'duration_ms_avg',
  'llm_calls', 'llm_cost_usd', 'saved_usd', 'net_usd_with_estimate', 'errors', null, null];

/** compSaved is a component's dollar saving over the window: the figure stored per request
 *  plus the read-time valuation of the rows written before that column existed. Added here
 *  rather than server-side so the two halves stay separable in the API — and the tooltip on
 *  every cell says how much of it is which. */
function compSaved(c) { return (c.saved_usd || 0) + (c.saved_usd_estimated || 0); }

async function loadComponents() {
  const body = clear($('#components-body'));
  loadingRows(body, 19);
  try {
    const { components: raw } = await api('components');
    // /api/components has no LIMIT — every component that ran in the window is in this
    // array — so sorting it here IS a global sort, not a sort of one page.
    const components = state.sort ? sortRows(raw, state.sort, state.dir) : raw;
    syncSortHeads('[data-testid=components-table]', COMPONENT_SORT);
    clear(body);
    if (!components.length) {
      tableMessage(body, 19, 'No component runs captured',
        'Run some traffic through the proxy with a non-empty pipeline.');
      emptyState($('#chart-comp'), 'No component data',
        'This chart fills in once a component has saved something.');
      return;
    }
    for (const c of components) {
      const [vtext, vcls] = verdict(c);
      body.appendChild(el('tr', { class: 'click', onclick: () => { setFilter('component', c.component, { quiet: true }); go('requests'); } },
        el('td', {}, el('code', { text: c.component })),
        el('td', { text: c.kind || '—' }),
        el('td', { class: 'num', text: num(c.runs) }),
        // Two columns, because "acted" meant "removed tokens" and a component whose whole job
        // is where a cache breakpoint goes therefore read 0 acted / 0.0% forever and painted
        // red. cachesplit mutated 1,805 requests on measured traffic and "acted" on none.
        el('td', { class: 'num', text: num(c.acted_tokens) }),
        el('td', {
          class: 'num', title: 'mutated the request without removing tokens — cache placement, '
            + 'markers, breakpoints. Not a failure to act.',
        }, num(c.acted_structural) || '—'),
        el('td', { class: 'num', text: pct(c.act_rate * 100, 1) }),
        el('td', { class: 'num', text: num(c.reverted) }),
        el('td', { class: 'num', text: compact(c.saved_unique) }),
        el('td', { class: 'num', text: compact(c.saved_gross) }),
        el('td', { class: 'num', text: c.overcount_ratio ? c.overcount_ratio.toFixed(1) + '×' : '—' }),
        el('td', { class: 'num', text: dur(c.duration_ms_total) }),
        el('td', { class: 'num', text: ms(c.duration_ms_avg) }),
        el('td', { class: 'num', text: c.llm_calls ? num(c.llm_calls) : '—' }),
        el('td', { class: 'num', text: c.llm_calls ? usd(c.llm_cost_usd) : '—' }),
        // A cost never travels alone. saved_usd is what this component's removals were
        // worth over the window — summed per turn, so a frozen reduction replaying across a
        // session is already amortized into it — and net_usd is the verdict. Both from the
        // server, priced at the model this deployment actually bills.
        // saved_usd is stored per request; saved_usd_estimated is the same arithmetic applied
        // on READ to the rows written before that column existed. Without the second half this
        // column showed $0.00 for every component over all history predating the last restart
        // — 6 populated rows out of 100,579 on production — and the most-read tab in the
        // dashboard therefore said the product was worthless. The dagger says how much of the
        // figure is valued rather than recorded; it is never silently merged.
        el('td', {
          class: 'num',
          title: c.saved_usd_estimated
            ? usd(c.saved_usd) + ' recorded + ' + usd(c.saved_usd_estimated) + ' valued on read '
              + 'over ' + num(c.saved_usd_estimated_rows) + ' rows written before the column existed'
              + (c.saved_usd_unpriced_rows
                ? ' · ' + num(c.saved_usd_unpriced_rows) + ' rows unpriceable' : '')
            : 'recorded per request',
        }, usd(compSaved(c)) + (c.saved_usd_estimated ? '†' : '')),
        el('td', {
          class: 'num' + (c.net_usd_with_estimate < 0 ? ' warn-text' : ''),
          title: 'saved ' + usd(compSaved(c)) + ' − own LLM cost ' + usd(c.llm_cost_usd || 0),
        }, usd(c.net_usd_with_estimate)),
        el('td', { class: 'num', text: num(c.errors) }),
        el('td', {}, gateSummary(c.gates)),
        el('td', {}, el('span', { class: 'pill ' + vcls, text: vtext }))));
    }
    // One measure (unique tokens saved) across up to twelve components: a magnitude
    // comparison, so ONE hue. Colouring bar N by N implied each component was a
    // different series and repainted them all whenever a filter changed the order.
    const top = components.filter((c) => c.saved_unique > 0).slice(0, 12);
    barRows($('#chart-comp'), top.map((c) => ({
      label: c.component, value: c.saved_unique, display: compact(c.saved_unique) + ' tok',
      // Figures, not prose, so they go in the always-visible slot rather than behind a
      // disclosure: eight collapsed glyphs would each hide four numbers and cost a line to
      // do it.
      formula: `${num(c.runs)} runs · acted ${pct(c.act_rate * 100, 1)} · own latency ${dur(c.duration_ms_total)} · ` +
            `overcount ${c.overcount_ratio ? c.overcount_ratio.toFixed(1) + '×' : 'n/a'}`,
    })), { emptyDetail: 'No component saved any content tokens in this window.' });
  } catch (err) {
    if (aborted(err)) return;
    tableMessage(body, 19, 'Could not load components', String(err.message || err), { error: true });
  }
}

// ── sessions ───────────────────────────────────────────────────────────────
/**
 * wideScope is "this list can contain more than one account", which is a manager with no
 * ?tenant= — the server's default. It is what decides whether the Account column is shown:
 * a single-account list does not need a column repeating the filter bar.
 */
function wideScope() { return isManager() && !state.filter.tenant; }
/** showScopeCol toggles the static Account <th> of one table. */
function showScopeCol(tableSel, wide) {
  for (const th of $$('thead th[data-scope-col]', $(tableSel))) th.hidden = !wide;
  return wide;
}

async function loadSessions() {
  const body = clear($('#sessions-body'));
  const wide = showScopeCol('[data-testid=sessions-table]', wideScope());
  const cols = wide ? 14 : 13;
  loadingRows(body, cols);
  try {
    const { sessions, total } = await api('sessions', { limit: 25, offset: state.sessOffset });
    clear(body);
    if (!sessions.length) {
      if (activeFilters().length) renderNoMatch(body, cols, 'sessions');
      else {
        tableMessage(body, cols, 'No sessions yet',
          'A session appears as soon as its first request is captured.');
      }
    }
    for (const s of sessions) {
      body.appendChild(el('tr', { class: 'click', onclick: () => { setFilter('session', s.session_id, { quiet: true }); go('requests'); } },
        el('td', {}, el('span', { class: 'trunc', title: s.session_id, text: s.session_id || '(none)' }),
          // A young session has paid for its extraction call and not yet collected the
          // replay, so its net reads underwater and stops doing so as the turns come in.
          // The pill says the amortization is unfinished instead of letting a half-collected
          // figure read as a verdict.
          s.in_flight
            ? el('span', {
              class: 'pill partial', 'data-testid': 'in-flight',
              title: 'This session\'s last request is still inside one provider cache TTL, so '
                   + 'the next turn may replay the same reduction. Its dollar figures are an '
                   + 'incomplete amortization, not a verdict.',
            }, 'in flight')
            : null),
        wide ? el('td', {}, el('span', { class: 'trunc', title: s.tenant_id, text: s.tenant_id || '—' })) : null,
        el('td', { text: firstOf(s.models) }),
        el('td', { text: firstOf(s.agents) }),
        el('td', { text: firstOf(s.presets) }),
        el('td', { class: 'num', text: num(s.turns) }),
        el('td', { class: 'num', text: compact(s.tokens_before) }),
        el('td', { class: 'num', text: compact(s.saved) }),
        // A dollar figure over a session whose turns are not all priced is a figure with
        // a different denominator from the token columns beside it: it covers the priced
        // turns only. The dagger says so rather than letting the two read as one total.
        el('td', { class: 'num' }, s.baseline_cost_usd ? usd(s.saved_usd) : '—',
          s.baseline_cost_usd && s.incomplete_rows > 0
            ? el('span', {
              class: 'na small',
              title: `${num(s.incomplete_rows)} of this session's ${num(s.turns)} turns have no usable ` +
                     'provider usage data, so this covers the ' + num(s.turns - s.incomplete_rows) +
                     ' priced turns only — the token columns cover all of them.',
            }, ' †')
            : null),
        el('td', { class: 'num', text: compact(s.cache_read) + ' / ' + compact(s.cache_write) }),
        el('td', { class: 'num', text: num(s.expands) }),
        el('td', { class: 'num', text: ms(s.cg_latency_ms_avg) }),
        el('td', { text: when(s.start) }),
        // stopPropagation: the row itself navigates to this session's requests, and a
        // click that does two things is a click that does the wrong one.
        el('td', {}, el('button', {
          class: 'ghost small', 'data-testid': 'session-diff',
          onclick: (ev) => { ev.stopPropagation(); openSessionDiff(s.session_id); },
        }, 'Diff'))));
    }
    const from = total ? state.sessOffset + 1 : 0;
    $('#sess-page').textContent = `${from}–${Math.min(state.sessOffset + 25, total)} of ${num(total)}`;
    $('#sess-prev').disabled = state.sessOffset === 0;
    $('#sess-next').disabled = state.sessOffset + 25 >= total;
  } catch (err) {
    if (aborted(err)) return;
    tableMessage(body, cols, 'Could not load sessions', String(err.message || err), { error: true });
  }
}

// ── requests ───────────────────────────────────────────────────────────────
async function loadRequests() {
  const body = clear($('#requests-body'));
  const wide = showScopeCol('[data-testid=requests-table]', wideScope());
  const cols = wide ? 14 : 13;
  loadingRows(body, cols, 6);
  try {
    const page = await api('requests', { limit: 50, before: state.reqCursor });
    clear(body);
    if (!page.requests.length) {
      renderNoMatch(body, cols, 'requests');
    }
    for (const e of page.requests) {
      body.appendChild(el('tr', { class: 'click', 'data-testid': 'request-row', onclick: () => openRequest(e.id) },
        el('td', { text: e.id }),
        el('td', { text: when(e.ts) }),
        el('td', {}, el('span', { class: 'trunc', title: e.session_id, text: e.session_id || '—' })),
        wide ? el('td', {}, el('span', { class: 'trunc', title: e.tenant_id, text: e.tenant_id || '—' })) : null,
        el('td', { text: e.model || '—' }),
        el('td', {}, el('span', { class: 'pill neutral', text: modeLabel(e.mode) })),
        el('td', { class: 'num', text: compact(e.tokens_before) }),
        el('td', { class: 'num', text: compact(e.tokens_after) }),
        el('td', { class: 'num', text: compact(e.tokens_before - e.tokens_after) }),
        el('td', { class: 'num', text: compact(e.cache_read) + ' / ' + compact(e.cache_write) }),
        el('td', { class: 'num', text: e.token_accounting === 'complete' ? usd(e.cost_usd) : '—' }),
        el('td', { class: 'num', text: ms(e.cg_latency_ms) }),
        el('td', {}, el('span', { class: 'pill ' + (e.cache_miss_reason || 'neutral'), text: e.cache_miss_reason || '—' })),
        el('td', {}, el('span', { class: 'pill ' + e.token_accounting, text: e.token_accounting }))));
    }
    $('#req-page').textContent = `${num(page.requests.length)} shown of ${num(page.total)} matching`;
    $('#req-next').disabled = !page.next_cursor;
    $('#req-prev').disabled = state.reqStack.length === 0;
    state.nextCursor = page.next_cursor;
  } catch (err) {
    if (aborted(err)) return;
    tableMessage(body, cols, 'Could not load requests', String(err.message || err), { error: true });
  }
}

// ── request detail + diff ──────────────────────────────────────────────────
/**
 * Myers-style LCS diff over lines, then rendered Git-style. This is the view both
 * reference implementations carry the data for and neither built: it answers
 * "what did context-guru actually remove or rewrite?" instead of asserting a
 * token count.
 */
function diffLines(a, b) {
  const n = a.length, m = b.length;
  // Trim the common head/tail first: agent transcripts share long identical
  // stretches, so this cuts the DP table to the part that actually differs.
  let head = 0;
  while (head < n && head < m && a[head] === b[head]) head++;
  let tail = 0;
  while (tail < n - head && tail < m - head && a[n - 1 - tail] === b[m - 1 - tail]) tail++;
  const as = a.slice(head, n - tail), bs = b.slice(head, m - tail);

  const out = [];
  for (let i = 0; i < head; i++) out.push({ op: ' ', text: a[i], ai: i + 1, bi: i + 1 });

  // Guard the quadratic table: a huge rewrite renders as a whole-block replace
  // rather than hanging the tab.
  // ponytail: LCS is O(n·m); the cap below is the ceiling. Switch to a real Myers
  // O(nd) if multi-megabyte single-message diffs ever matter.
  const LIMIT = 1500;
  if (as.length > LIMIT || bs.length > LIMIT) {
    if (as.length) out.push({ op: 'gap', text: `… ${as.length} lines replaced (too large to line-diff) …` });
    for (let i = 0; i < as.length; i++) out.push({ op: '-', text: as[i], ai: head + i + 1 });
    for (let j = 0; j < bs.length; j++) out.push({ op: '+', text: bs[j], bi: head + j + 1 });
  } else {
    const dp = Array.from({ length: as.length + 1 }, () => new Uint32Array(bs.length + 1));
    for (let i = as.length - 1; i >= 0; i--) {
      for (let j = bs.length - 1; j >= 0; j--) {
        dp[i][j] = as[i] === bs[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
      }
    }
    let i = 0, j = 0;
    while (i < as.length && j < bs.length) {
      if (as[i] === bs[j]) { out.push({ op: ' ', text: as[i], ai: head + i + 1, bi: head + j + 1 }); i++; j++; }
      else if (dp[i + 1][j] >= dp[i][j + 1]) { out.push({ op: '-', text: as[i], ai: head + i + 1 }); i++; }
      else { out.push({ op: '+', text: bs[j], bi: head + j + 1 }); j++; }
    }
    while (i < as.length) { out.push({ op: '-', text: as[i], ai: head + i + 1 }); i++; }
    while (j < bs.length) { out.push({ op: '+', text: bs[j], bi: head + j + 1 }); j++; }
  }
  for (let k = 0; k < tail; k++) {
    out.push({ op: ' ', text: a[n - tail + k], ai: n - tail + k + 1, bi: m - tail + k + 1 });
  }
  return out;
}

/** Collapse runs of unchanged lines to CTX lines of context, Git-style. */
function withHunks(rows, ctx = 3) {
  const keep = new Array(rows.length).fill(false);
  rows.forEach((r, i) => {
    if (r.op === ' ') return;
    for (let k = Math.max(0, i - ctx); k <= Math.min(rows.length - 1, i + ctx); k++) keep[k] = true;
  });
  const out = [];
  let skipped = 0;
  rows.forEach((r, i) => {
    if (keep[i]) {
      if (skipped) { out.push({ op: 'gap', text: `… ${skipped} unchanged lines …` }); skipped = 0; }
      out.push(r);
    } else skipped++;
  });
  if (skipped) out.push({ op: 'gap', text: `… ${skipped} unchanged lines …` });
  return out;
}

/**
 * DIFF_MODES are the four ways to look at one compaction, and they are four views of
 * ONE diff rather than four renderers: `orig` and `raw` are the before/after sides on
 * their own, `git` interleaves them, `side` puts them in aligned columns. Sharing the
 * LCS output is what keeps the line tints and the line numbers agreeing between modes.
 */
const DIFF_MODES = [
  ['orig', 'Before', 'diff-mode-orig'],
  ['raw', 'After', 'diff-mode-raw'],
  ['git', 'Inline diff', 'diff-mode-git'],
  ['side', 'Side by side', 'diff-mode-side'],
];

/** Render one whole side as plain text, with the changed lines still tinted. */
function renderOneSide(host, rows, side) {
  const keepOp = side === 'a' ? '-' : '+';
  // Focusable for the same reason as the other scrollers: keyboard users scroll it.
  const pre = el('pre', { tabindex: '0' });
  let any = false;
  for (const r of rows) {
    // The elision has to be visible here too. Dropping the gap markers made this view
    // claim to be the whole before/after text while quietly omitting every unchanged
    // run — two adjacent lines that are 200 lines apart in the real message.
    if (r.op === 'gap') { pre.appendChild(el('span', { class: 'sl pad' }, (r.text || '') + '\n')); continue; }
    if (r.op !== ' ' && r.op !== keepOp) continue;
    any = true;
    pre.appendChild(el('span', {
      class: 'sl' + (r.op === '-' ? ' del' : r.op === '+' ? ' add' : ''),
    }, (r.text || '') + '\n'));
  }
  if (!any) { emptyState(host, 'Nothing on this side', 'This message is empty in that version.'); return; }
  host.appendChild(el('div', { class: 'oneside' }, pre));
}

/**
 * renderSide lays the two versions out in aligned columns.
 *
 * The alignment is the point, and it is why this cannot be two independent <pre>
 * blocks. Two of them drift for two separate reasons, and padding by LINE COUNT — which
 * is what this used to do — fixes neither:
 *
 *   - the lines WRAP, so a 1,961-character deletion is 540px tall beside an 18px
 *     addition. Equal counts, unequal heights, 522px of drift measured on a real block,
 *     with SKU-0032 sitting opposite SKU-0023.
 *   - each <pre> was its own scroll box, so scrolling one desynchronised them even where
 *     the heights did agree.
 *
 * So the two sides are cells of ONE grid, a row per line pair: the taller cell sets its
 * row's height, both cells top-align inside it, and a single scroller wraps the whole
 * thing. Alignment is then a property of the layout rather than something this function
 * has to compute and keep true — which also makes the count-based padding unnecessary
 * rather than merely insufficient.
 */
function renderSide(host, rows) {
  const grid = el('div', { class: 'side', 'data-testid': 'side-grid' });
  // Headers are the grid's first row so they sit above their own column.
  grid.appendChild(el('div', { class: 'side-h side-l', text: 'Before compaction' }));
  grid.appendChild(el('div', { class: 'side-h side-r', text: 'After compaction' }));
  let dels = [], adds = [];
  const cell = (side, cls, text) => el('div', { class: 'side-' + side + ' sl ' + cls, text: text });
  const pair = (l, r) => { grid.appendChild(l); grid.appendChild(r); };
  const flush = () => {
    const n = Math.max(dels.length, adds.length);
    for (let i = 0; i < n; i++) {
      pair(cell('l', i < dels.length ? 'del' : 'pad', i < dels.length ? dels[i] : ''),
        cell('r', i < adds.length ? 'add' : 'pad', i < adds.length ? adds[i] : ''));
    }
    dels = []; adds = [];
  };
  for (const r of rows) {
    if (r.op === '-') { dels.push(r.text); continue; }
    if (r.op === '+') { adds.push(r.text); continue; }
    flush();
    const cls = r.op === 'gap' ? 'pad' : 'ctx';
    pair(cell('l', cls, r.text || ''), cell('r', cls, r.text || ''));
  }
  flush();
  // The single scroller is focusable so it can be scrolled from the keyboard: it is the
  // only way to reach the rest of a long diff without a pointer.
  host.appendChild(el('div', { class: 'side-scroll', tabindex: '0' }, grid));
}

function renderDiff(host, before, after, mode) {
  clear(host);
  const rows = withHunks(diffLines((before || '').split('\n'), (after || '').split('\n')));
  if (mode === 'orig') { renderOneSide(host, rows, 'a'); return; }
  if (mode === 'raw') { renderOneSide(host, rows, 'b'); return; }
  if (mode === 'side') { renderSide(host, rows); return; }
  if (!rows.length) { emptyState(host, 'Identical', 'This message was not changed.'); return; }
  const frag = document.createDocumentFragment();
  for (const r of rows) {
    if (r.op === 'gap') {
      frag.appendChild(el('div', { class: 'dl gap' },
        el('span', { class: 'ln' }), el('span', { class: 'ln' }), el('span', { class: 'tx', text: r.text })));
      continue;
    }
    const cls = r.op === '+' ? 'add' : r.op === '-' ? 'del' : 'ctx';
    frag.appendChild(el('div', { class: 'dl ' + cls },
      el('span', { class: 'ln', text: r.ai || '' }),
      el('span', { class: 'ln', text: r.bi || '' }),
      el('span', { class: 'tx', text: r.text })));
  }
  host.appendChild(frag);
}

/** Count added/removed lines, for the toolbar's tally. */
function diffTally(before, after) {
  let add = 0, del = 0;
  for (const r of diffLines((before || '').split('\n'), (after || '').split('\n'))) {
    if (r.op === '+') add++;
    else if (r.op === '-') del++;
  }
  return { add, del };
}

/**
 * MARKER matches the offload sentinel context-guru leaves behind. Its PRESENCE in the
 * after-text is what distinguishes a reversible offload from a lossy rewrite, which is
 * the only per-span signal the capture actually records.
 */
const MARKER = /<<cg:[A-Za-z0-9_-]{1,64}>>/;

/**
 * attribute answers "which components changed this message?" and says whether the
 * answer is RECORDED or INFERRED — a distinction the reader has to be able to see.
 *
 * Recorded: `c.components` is the list the pipeline wrote down, in the order the
 * components touched the message; reverted and panicked ones are already excluded. The
 * diff is their cumulative result, so all of them are named, not a single winner. One
 * entry is not a special case — it is a recorded answer that happens to have one name.
 *
 * Inferred: rows written before that field existed have no list, and an absent list
 * means UNKNOWN, not "nothing". For those, attribution falls back to the change's
 * shape — an after-text carrying a <<cg:HASH>> marker was produced by an offloader, one
 * without by a reformatter — narrowed to the components that ran and were not reverted.
 * Those chips stay dashed, so an inferred list never reads as a recorded one.
 */
function attribute(c, components) {
  if (c.components && c.components.length) {
    // kind per name, for the chip's colour dot only; an unlisted component gets a
    // neutral dot rather than a colour that would assert a kind we do not have.
    const kinds = new Map((components || []).map((x) => [x.component, x.kind || '']));
    return {
      recorded: true, names: c.components, kinds, marked: MARKER.test(c.after || ''),
      why: 'Recorded by the pipeline: the components that rewrote this message, in the order they touched it. The diff is their combined result.',
    };
  }
  const ran = (components || []).filter((x) => x.mutated && !x.reverted);
  const kind = MARKER.test(c.after || '') ? 'offload' : 'reformat';
  let cands = ran.filter((x) => (x.kind || '') === kind);
  let fallback = false;
  let why = kind === 'offload'
    ? 'The replacement carries a <<cg:HASH>> marker, so an offload component produced it — the original is recoverable with the expand tool.'
    : 'The replacement carries no marker, so a reformat component rewrote this text in place.';
  if (!cands.length) {
    // No component of the inferred kind ran. Fall back to everything that did rather
    // than claiming nothing produced a change that plainly happened.
    cands = ran;
    fallback = true;
    why = 'No component of the expected kind ran on this request, so this lists every component that did.';
  }
  return { recorded: false, kind, fallback, names: cands.map((x) => x.component), why };
}

const KIND_HUE = { offload: SERIES[0], reformat: SERIES[1] };

/**
 * attributionChips renders the attribution beside a change.
 *
 * Recorded lists are solid chips in order, separated by arrows: "Changed by, in order:
 * a → b" is a different statement from the inferred "Offloaded by one of a, b", and the
 * two must not read alike. Inferred lists keep dashed chips and say they are inferred.
 *
 * The KIND is carried by the verb rather than by a chip of its own — "Offloaded by" vs
 * "Rewritten by". A chip reading "offload" sat in the same row as the component chips
 * and read as a third candidate component, which is the opposite of what it meant.
 */
function attributionChips(c, components) {
  const a = attribute(c, components);
  if (!a.names.length) return null;

  if (a.recorded) {
    const box = el('div', { class: 'attrib' }, el('span', {
      class: 'small muted', title: a.why,
      text: a.names.length > 1 ? 'Changed by, in order:' : 'Changed by:',
    }));
    a.names.forEach((n, i) => {
      if (i) box.appendChild(el('span', { class: 'small muted', text: '→' }));
      box.appendChild(el('span', { class: 'chip', title: a.why },
        el('i', { style: 'background:' + (KIND_HUE[a.kinds.get(n)] || 'var(--fg-faint)') }), n));
    });
    // A marker in the after-text is an observation about the text, not an inference
    // about the author, so it still belongs here — the reader wants to know the
    // original can be got back.
    if (a.marked) {
      box.appendChild(el('span', {
        class: 'small muted', text: '· the original is recoverable from the marker',
        title: 'The replacement carries a <<cg:HASH>> marker, so the original text is recoverable with the expand tool.',
      }));
    }
    return box;
  }

  const hue = KIND_HUE[a.kind];
  // In the fallback the kind could NOT be inferred, so the verb stays neutral rather
  // than asserting "rewritten" over a list that may well be offloaders.
  const verb = a.fallback ? 'Changed by' : a.kind === 'offload' ? 'Offloaded by' : 'Rewritten by';
  const box = el('div', { class: 'attrib' }, el('span', {
    class: 'small muted', title: a.why, text: verb + (a.names.length > 1 ? ' one of' : ''),
  }));
  for (const n of a.names) {
    box.appendChild(el('span', { class: 'chip guess', title: a.why },
      el('i', { style: 'background:' + hue }), n));
  }
  box.appendChild(el('span', { class: 'small muted', title: a.why, text: a.fallback
    ? '· inferred: this request recorded no per-message attribution'
    : a.kind === 'offload'
      ? '· inferred from the marker; the original is recoverable from it'
      : '· inferred from the absence of a marker' }));
  return box;
}

function kv(k, v) { return el('div', {}, el('div', { class: 'k', text: k }), el('div', { class: 'v', text: v })); }

/** kvNode is kv() for a value that is a NODE rather than a string (the breakpoint chips). */
function kvNode(k, node) { return el('div', {}, el('div', { class: 'k', text: k }), el('div', { class: 'v' }, node)); }

/**
 * kvUnset prints a value the client never set as the WORD "unset", styled as absent.
 *
 * The distinction is the whole reason temperature and top_p are nullable columns:
 * `temperature: 0` means "be deterministic" and a missing temperature means "you choose".
 * An em dash was already honest, but italic "unset" cannot be misread as a dash-shaped
 * zero — and it never renders 0 for an absent value, which is the invariant.
 */
function kvMaybe(k, v, fmt) {
  if (v === null || v === undefined || v === '') {
    return el('div', {}, el('div', { class: 'k', text: k }),
      el('div', { class: 'v unset', text: 'unset' }));
  }
  return kv(k, fmt ? fmt(v) : String(v));
}

/** kvBand groups a set of pairs under a caption. */
function kvBand(title, testid, ...pairs) {
  return el('div', { class: 'kv-band' },
    el('h3', { text: title }),
    el('div', { class: 'kv', 'data-testid': testid }, ...pairs.flat().filter(Boolean)));
}

/**
 * diffBlock builds one collapsible before/after block for a rewritten message: the
 * summary line, the view-mode toolbar, the attribution chips and the diff itself.
 *
 * Shared by the single-request drawer and the whole-session view so the two cannot
 * drift into rendering the same data two different ways. `mode` seeds the view, and
 * the returned element exposes setMode so a session-wide toolbar can drive every
 * block at once.
 */
function diffBlock(c, components, opts = {}) {
  const saved = c.before_tokens - c.after_tokens;
  const t = diffTally(c.before, c.after);
  const det = el('details', { class: 'diff', 'data-testid': 'diff-block' });
  if (opts.open) det.open = true;
  det.appendChild(el('summary', {
    text: `${opts.prefix || ''}${c.path} — ${compact(c.before_tokens)} → ${compact(c.after_tokens)} tokens ` +
          (saved > 0 ? `(saved ${compact(saved)})` : '(rewritten, no token saving)'),
  }));

  // tabindex: this box scrolls, and a scroll region a keyboard user cannot focus is a
  // part of the diff they cannot reach at all (axe scrollable-region-focusable). Same
  // reason .tblwrap carries one.
  const bodyHost = el('div', { class: 'diffbody', tabindex: '0', role: 'group',
    'aria-label': 'Diff of ' + c.path });
  let mode = opts.mode || 'git';
  const buttons = [];
  const bar = el('div', { class: 'difftoolbar' });
  const setMode = (next) => {
    mode = next;
    for (const b of buttons) b.setAttribute('aria-pressed', String(b.dataset.mode === next));
    renderDiff(bodyHost, c.before, c.after, next);
  };
  for (const [m, label, testid] of DIFF_MODES) {
    const b = el('button', {
      class: 'ghost small', 'data-testid': testid, 'data-mode': m,
      'aria-pressed': String(m === mode), onclick: () => setMode(m),
    }, label);
    buttons.push(b);
    bar.appendChild(b);
  }
  bar.appendChild(el('span', { class: 'spacer' }));
  bar.appendChild(el('span', { class: 'tally' },
    el('span', { class: 'del', text: '−' + num(t.del) }), ' / ',
    el('span', { class: 'add', text: '+' + num(t.add) }), ' lines'));
  det.appendChild(bar);

  const chips = attributionChips(c, components);
  if (chips) det.appendChild(el('div', { class: 'difftoolbar' }, chips));

  det.appendChild(bodyHost);
  renderDiff(bodyHost, c.before, c.after, mode);
  det.setMode = setMode;
  return det;
}

/** openDrawer shows the side panel with a title and a body node. Factored out of
 *  openRequest so the archive view opens the same panel rather than inventing a second
 *  one that would need its own close handling and focus behaviour. */
function openDrawer(title, node) {
  const drawer = $('#drawer');
  // Remember where focus came from so closing returns it there rather than dumping the
  // caret at the top of the document — for a keyboard user that means losing their
  // place in a 500-row table every time they inspect a request.
  if (drawer.hidden) drawerReturnFocus = document.activeElement;
  drawer.hidden = false;
  $('#scrim').hidden = false;
  setBackgroundInert(true);
  $('#drawer-title').textContent = title;
  const body = clear($('#drawer-body'));
  if (node) body.appendChild(node);
  $('#drawer-close').focus();
  return body;
}
let drawerReturnFocus = null;

/**
 * trapTab keeps Tab inside the open drawer.
 *
 * The drawer is aria-modal, and the background is not inert, so without this a
 * keyboard user tabs straight out of the dialog into the table behind it — the focus
 * ring disappears under the scrim and the only way back is Shift+Tab thirteen times.
 * The list is recomputed per keypress because the drawer's contents are rebuilt on
 * every open and every diff-mode switch.
 */
function trapTab(ev) {
  if (ev.key !== 'Tab') return;
  const drawer = $('#drawer');
  if (drawer.hidden) return;
  const stops = $$('button, a[href], input, select, textarea, summary, [tabindex]:not([tabindex="-1"])', drawer)
    .filter((n) => !n.disabled && n.offsetParent !== null && n.id !== 'drawer-end');
  if (!stops.length) return;
  if (!drawer.contains(document.activeElement)) { ev.preventDefault(); stops[0].focus(); return; }
  // Backwards off the close button: there is nothing before it, so wrap to the end.
  if (ev.shiftKey && document.activeElement === stops[0]) {
    ev.preventDefault();
    stops[stops.length - 1].focus();
  }
}

async function openRequest(id, fromURL) {
  // The open drawer is part of the address (see urlFor): a request is linkable, and
  // Back dismisses the drawer instead of undoing the user's last filter change.
  if (!fromURL) { state.drawer = { req: Number(id) }; syncURL(false); }
  const body = openDrawer('Request #' + id, null);
  loadingState(body, 5);
  try {
    const res = await fetch('/api/requests/' + id);
    if (!res.ok) throw new Error(res.status + ' ' + res.statusText);
    // content_archived is the server's answer to "this session's text moved to cold
    // storage and I tried to fetch it": without reading it here, the branch below
    // referenced an undeclared `archived` and threw on every request that HAS content.
    // capture_blocked_by names WHICH party's gate is shut, so the empty-diff panel can
    // stop telling people to change a setting that is not theirs.
    const { request: e, content_visible: visible, content_captured: captured,
      content_archived: archived, capture_blocked_by: blockedBy } = await res.json();
    clear(body);

    // Four captioned bands rather than twenty-four pairs in one grid. Same data, same
    // testid on the first band (the checks and the docs screenshots read it), but "what
    // this request was" / "what it moved" / "what it cost" are three questions and the flat
    // wall answered them in one undifferentiated scan.
    const priced = e.token_accounting === 'complete';
    body.appendChild(kvBand('Request', 'detail-summary',
      kv('Session', e.session_id || '—'),
      kv('When', when(e.ts)),
      kv('Model', e.model || '—'),
      kv('Provider', e.provider || '—'),
      kv('Agent', e.agent || '—'),
      kv('Preset', e.preset || '—'),
      kv('Mode', modeLabel(e.mode)),
      kv('Upstream status', e.status || '—'),
      kv('Messages', num(e.messages))));

    body.appendChild(kvBand('Tokens', 'detail-tokens',
      kv('Before → after', compact(e.tokens_before) + ' → ' + compact(e.tokens_after)),
      kv('Saved (gross / unique)', compact(e.tokens_before - e.tokens_after) + ' / ' + compact(e.saved_unique)),
      kv('Attempted (eligible)', compact(e.attempted_tokens)),
      kv('Frozen for cache safety', compact(e.frozen_tokens)),
      kv('Fresh / read / write / out',
        [e.fresh_input, e.cache_read, e.cache_write, e.output_tokens].map(compact).join(' / ')),
      kv('Token accounting', e.token_accounting)));

    body.appendChild(kvBand('Cost and latency', 'detail-cost',
      kv('Cost (actual / baseline)', priced ? usd(e.cost_usd) + ' / ' + usd(e.baseline_cost_usd) : 'not priced'),
      kv('Our own LLM cost', priced ? usd(e.cg_llm_cost_usd) : '—'),
      // Directly beneath the cost, because the cost on its own is the figure that makes
      // readers conclude the product is worthless: baseline − actual − our own spend is
      // what this one turn was actually worth.
      kv('Net after our cost',
        priced ? usd(e.baseline_cost_usd - e.cost_usd - e.cg_llm_cost_usd) : '—'),
      // On a turn whose cache HIT this is usually the largest money figure on the row,
      // and it was not reported anywhere: the cache reads this request was billed for,
      // against the fresh rate they would have cost without the cache.
      kv('Prefix-cache saved (ours)', priced ? usd(e.cachesplit_saved_usd) : '—'),
      kv('Prefix half we moved', e.split_stable_tokens ? compact(e.split_stable_tokens) + ' tok' : '—'),
      kv('Provider cache saved', priced ? usd(e.cache_saved_usd) : '—'),
      kv('context-guru latency', ms(e.cg_latency_ms)),
      kv('Upstream latency', ms(e.upstream_ms)),
      kv('Restorations', num(e.expands) + ' (' + compact(e.expand_tokens) + ' tok)'),
      kv('Reverts', num(e.reverts)),
      kv('Cache attribution', e.cache_miss_reason || '—'),
      kv('Compaction outcome', e.uncompressed_reason || 'compacted')));

    // The request's own metadata: what the client ASKED for. Separate from the bands above,
    // which are what happened to it. kvMaybe() prints an absent value as "unset" and a zero
    // as 0 — the API sends null for "not set" and 0 for "be deterministic", and collapsing
    // the two would misreport every deterministic request.
    body.appendChild(el('h2', { text: 'What the request asked for' }));
    body.appendChild(el('div', { class: 'kv', 'data-testid': 'detail-meta' },
      kvMaybe('Reasoning effort', e.reasoning_effort),
      kv('Thinking', e.thinking_mode
        ? e.thinking_mode + (e.thinking_budget ? ' (' + compact(e.thinking_budget) + ' token budget)' : '')
        : 'unset'),
      kvMaybe('Temperature', e.temperature),
      kvMaybe('top_p', e.top_p),
      kvMaybe('max_tokens', e.max_tokens || null, num),
      kv('Streaming', e.stream ? 'yes' : 'no'),
      kvMaybe('Tool choice', e.tool_choice),
      kv('Tools declared', num(e.tools)),
      kv('System blocks', num(e.system_blocks)),
      kvMaybe('Stop reason', e.stop_reason)));

    // Placement, not just a count: tools and system render ahead of messages, so a
    // breakpoint's LOCATION decides how much of the prefix it protects. Four labelled slots
    // rather than one run-on string that wrapped mid-clause and read as a sentence.
    const bpTotal = e.cache_bp_system + e.cache_bp_tools + e.cache_bp_messages + e.cache_bp_blocks;
    body.appendChild(el('div', { class: 'kv' },
      // Full width: four labelled slots do not fit a 184px grid cell, and wrapped over
      // three lines they stopped reading as four separate measurements.
      el('div', { class: 'kv-wide' },
        el('div', { class: 'k', text: 'cache_control breakpoints · ' + num(bpTotal) + ' of the provider’s 4' }),
        el('div', { class: 'v' }, el('div', { class: 'bp', 'data-testid': 'detail-breakpoints' },
          el('span', { class: e.cache_bp_system ? 'on' : '' }, 'system ', el('b', { text: num(e.cache_bp_system) })),
          el('span', { class: e.cache_bp_tools ? 'on' : '' }, 'tools ', el('b', { text: num(e.cache_bp_tools) })),
          el('span', { class: e.cache_bp_messages ? 'on' : '' }, 'messages ', el('b', { text: num(e.cache_bp_messages) })),
          el('span', { class: e.cache_bp_blocks ? 'on' : '' }, 'blocks ', el('b', { text: num(e.cache_bp_blocks) }))))),
      kv('Tenant id', e.tenant_id || '— (single-tenant)')));

    // The log lines this request emitted. Text, not a link: Grafana binds loopback on the
    // box (see renderLogsHelp).
    if (e.session_id) body.appendChild(logQueryBlock(e.session_id, e.tenant_id));

    body.appendChild(el('h2', { text: 'Components, in the order they ran' }));
    if (!e.components || !e.components.length) {
      body.appendChild(el('div', { class: 'empty', text: 'No components ran on this request.' }));
    } else {
      const tbl = el('table', { class: 'tbl compact', 'data-testid': 'detail-components' },
        el('thead', {}, el('tr', {},
          el('th', { text: '#' }), el('th', { text: 'Component' }), el('th', { text: 'Kind' }),
          el('th', { class: 'num', text: 'Saved' }), el('th', { class: 'num', text: 'Unique' }),
          el('th', { class: 'num', text: 'Latency' }), el('th', { text: 'Outcome' }),
          el('th', { text: 'Why declined' }))));
      const tb = el('tbody');
      e.components.forEach((c, i) => {
        const outcome = c.reverted ? ['reverted', 'missing'] : c.skipped ? ['skipped', 'neutral']
          : c.acted ? ['acted', 'complete'] : ['mutated only', 'partial'];
        tb.appendChild(el('tr', {},
          el('td', { text: i + 1 }),
          el('td', {}, el('code', { text: c.component })),
          el('td', { text: c.kind || '—' }),
          el('td', { class: 'num', text: compact(c.saved_gross) }),
          el('td', { class: 'num', text: compact(c.saved_unique) }),
          el('td', { class: 'num', text: ms(c.duration_ms) }),
          el('td', {}, el('span', { class: 'pill ' + outcome[1], text: outcome[0] }),
            c.err ? el('div', { class: 's', text: c.err }) : null),
          el('td', {}, gateSummary(c.gates))));
      });
      tbl.appendChild(tb);
      body.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, tbl));
    }

    // Recorded model calls. This is the only place the cost of one extraction call is
    // visible: the request row carries a single rolled-up dollar figure, and the components
    // table has no dollars at all, so an expensive component could never be shown to be
    // underwater on a particular KIND of call.
    if (e.extractions && e.extractions.length) {
      body.appendChild(el('h2', { text: 'Compaction model calls' }));
      const perTok = savedPerTok(e);
      const net = perTok === null ? null
        : e.extractions.reduce((a, x) => a + netUSD(x, perTok), 0);
      const spent = e.extractions.reduce((a, x) => a + (x.cost_usd || 0), 0);
      body.appendChild(el('div', { class: 'note' },
        `${e.extractions.length} call(s), spent ${usd(spent)}, net `,
        el('strong', { class: net !== null && net < 0 ? 'warn-text' : '', text: usd(net) }),
        net === null ? ' — this request is not fully priced, so the calls cannot be valued.'
          : net < 0 ? ' — these calls cost more than the tokens they removed were worth.' : '.'));
      const xt = el('table', { class: 'tbl compact', 'data-testid': 'detail-extractions' },
        el('thead', {}, el('tr', {},
          el('th', { text: '#' }), el('th', { text: 'Component' }), el('th', { text: 'Target' }),
          el('th', { class: 'num', text: 'Candidate' }), el('th', { class: 'num', text: 'Saved' }),
          el('th', { class: 'num', text: 'Prompt' }), el('th', { class: 'num', text: 'Cost' }),
          el('th', { class: 'num', text: 'Net' }), el('th', { class: 'num', text: 'Latency' }),
          el('th', { text: 'Outcome' }))));
      const xb = el('tbody');
      e.extractions.forEach((x, i) => {
        const n = netUSD(x, perTok);
        xb.appendChild(el('tr', {},
          el('td', { text: i + 1 }),
          el('td', {}, el('code', { text: x.component }),
            x.cold ? el('span', { class: 'pill complete', text: 'cold sweep' }) : null,
            x.escalated ? el('span', { class: 'pill neutral', text: 'escalated' }) : null),
          el('td', { text: x.aggressiveness || '—' }),
          el('td', { class: 'num', text: compact(x.candidate_tokens) }),
          el('td', { class: 'num', text: compact(x.saved_tokens) }),
          el('td', { class: 'num', text: compact(x.prompt_tokens) }),
          el('td', { class: 'num', text: usd(x.cost_usd) }),
          el('td', { class: 'num ' + (n !== null && n < 0 ? 'warn-text' : ''), text: usd(n) }),
          el('td', { class: 'num', text: ms(x.latency_ms) }),
          el('td', {},
            el('span', {
              class: 'pill ' + (x.accepted ? 'complete' : 'neutral'),
              text: x.accepted ? 'accepted' : 'no reduction',
            }),
            x.summary ? el('div', { class: 's', text: x.summary }) : null,
            x.rejection ? el('div', { class: 's warn-text', text: x.rejection }) : null,
            x.gate_reason ? el('div', { class: 's', text: x.gate_reason }) : null)));
      });
      xt.appendChild(xb);
      body.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, xt));
      // The before/after of each call, where the account stores transcripts at all.
      e.extractions.forEach((x, i) => {
        if (!x.before && !x.after) return;
        const d = el('details', { class: 'field' },
          el('summary', { text: `Call ${i + 1}: what the model removed` }));
        const host = el('div', {});
        renderDiff(host, x.before || '', x.after || '', 'unified');
        d.appendChild(host);
        body.appendChild(d);
      });
    }

    body.appendChild(el('h2', { text: 'What context-guru changed' }));
    body.appendChild(el('div', { class: 'note' },
      'Whole-session view: ',
      el('button', {
        class: 'subtle', 'data-testid': 'open-session-diff',
        onclick: () => openSessionDiff(e.session_id),
      }, 'every change in this session')));
    // The order matters, and so does what each branch is allowed to conclude. Absent
    // content means "we do not have the text", NEVER "there was no change" — only the
    // component rows and the compaction outcome may decide that.
    const hasContent = !!(e.content && e.content.length);
    const rewritten = wasRewritten(e);
    if (!visible) {
      contentBlockedState(body, 'not_permitted');
    } else if (!hasContent && archived) {
      // The server tried the fetch for this one request and could not complete it. Say
      // which of the two it was rather than showing an empty diff.
      contentBlockedState(body, 'unreachable', { session: e.session_id });
    } else if (!hasContent && !captured) {
      contentBlockedState(body, 'not_captured', { blockedBy, rewritten });
    } else if (!hasContent && rewritten) {
      // Capture is on NOW and this request was compacted, so the text is missing for a
      // reason of its own: it ran before capture was enabled, or it exceeded the cap.
      contentBlockedState(body, 'not_stored', { rewritten });
    } else if (!hasContent) {
      // Into its own box: emptyState CLEARS the host it is given, and given `body` it
      // deleted the metrics and the component table it was talking about — leaving a
      // drawer whose only content was the claim, with none of the evidence for it.
      emptyState(body.appendChild(el('div')), 'Nothing was rewritten',
        'This request passed through unchanged' + (e.uncompressed_reason ? ' (' + e.uncompressed_reason + ')' : '') + '.');
    } else {
      // Biggest saving first, and open that one: the point of the view is "what did
      // context-guru actually remove?", so leading with an unchanged block (and
      // collapsing the 2k-token rewrite below it) buries the answer.
      const blocks = e.content.slice().sort(
        (a, b) => (b.before_tokens - b.after_tokens) - (a.before_tokens - a.after_tokens));
      blocks.forEach((c, idx) => {
        body.appendChild(diffBlock(c, e.components, {
          open: idx === 0 && c.before_tokens > c.after_tokens,
        }));
      });
    }
  } catch (err) {
    errorState(clear(body), 'Could not load this request', err);
  }
}

/**
 * contentBlockedState renders the "why is there no transcript here" panel.
 *
 * Four distinct answers, never one blank panel, because they are four different
 * situations and only two of them are anything the reader can do something about:
 * capture is a setting they own, cold storage is a button, permission is neither.
 */
/** The one sentence every "no transcript" panel owes a reader whose request WAS
 *  compacted: the change happened, only its text is missing. */
function compactedAnywayNote() {
  return el('span', { class: 'warn-text', text:
    'This request was compacted all the same — the component rows above are what ran and ' +
    'what each one removed. What is missing here is the stored before/after text, not the ' +
    'change itself.' });
}

function contentBlockedState(host, state, opts = {}) {
  const box = el('div', { class: 'state blocked' });
  const b = el('div', { class: 'state-body' });
  switch (state) {
    case 'not_permitted':
      box.className = 'state blocked';
      b.appendChild(el('strong', { text: 'Transcripts are not yours to read' }));
      b.appendChild(el('span', { text:
        'Metrics for this traffic are visible; its content is not. On a hosted deployment the ' +
        'owning account and the manager can read it. On a single-tenant proxy, content is ' +
        'served to loopback or a configured trusted CIDR only.' }));
      break;
    case 'not_captured':
      // WHOSE gate is shut decides what to say. Storing a transcript needs the operator's
      // service-wide gate AND the account's own consent, and the panel used to name only
      // the second — so a user whose consent was already on was told to switch on a
      // setting they had switched on, while the gate that was actually closed was one
      // they cannot reach.
      b.appendChild(el('strong', { text: 'Content was not captured' }));
      if (opts.blockedBy === 'operator') {
        b.appendChild(el('span', { text:
          'Storing transcripts is switched off service-wide on this proxy, so nothing was ' +
          'recorded for any account and your own consent setting makes no difference until ' +
          'that changes. Only whoever operates this proxy can turn it on ' +
          '(--dashboard-content); ask them. Capture is not retroactive, so even then this ' +
          'request stays empty.' }));
      } else if (opts.blockedBy === 'tenant') {
        b.appendChild(el('span', { text:
          'Storing transcripts is off by default and opt-in per account, so there is no ' +
          'before/after text to diff. Turn on "Store my transcripts for the diff view" in ' +
          'Settings to record it from the next request on. It is not retroactive.' }));
      } else {
        // capture_blocked_by is '' with content_captured false: more than one account is
        // in view, so there is no single consent setting to report — and guessing at one
        // would name the wrong party.
        b.appendChild(el('span', { text:
          'No transcripts are stored for this view. Storing them is decided per account, ' +
          'and this view spans more than one, so there is no single setting to report here. ' +
          "Open one account's traffic to see whether its transcripts are being stored." }));
      }
      if (opts.rewritten) b.appendChild(compactedAnywayNote());
      break;
    case 'not_stored':
      // Capture is on and this request still has no text. The component table above says
      // it WAS compacted, so the honest answer is about storage, not about change.
      b.appendChild(el('strong', { text: 'The text of this request was not stored' }));
      b.appendChild(el('span', { text:
        'Storing transcripts is on now, but it is not retroactive and it is size-capped: a ' +
        'request that ran before it was switched on, or whose messages were larger than the ' +
        'per-blob cap, has metrics and no text. There is nothing to recover for this one — ' +
        'requests from here on will have their before/after stored.' }));
      b.appendChild(compactedAnywayNote());
      break;
    case 'unknown_session':
      b.appendChild(el('strong', { text: 'No such session' }));
      b.appendChild(el('span', { text:
        'Nothing here has this session id: it was never captured, it has aged out of ' +
        'retention, or it belongs to another account. A link to a session that has since ' +
        'been pruned lands exactly here.' }));
      break;
    case 'capture_not_permitted':
      b.appendChild(el('strong', { text: 'Capture health is per-process, so it is manager-only' }));
      b.appendChild(el('span', { text:
        'The queue, drop and write counters cover every account this proxy serves, which ' +
        'makes them a read on other tenants\' traffic volume. One consequence is worth ' +
        'knowing: if the capture queue overflows, YOUR figures under-report and this page ' +
        'cannot tell you. Ask whoever operates the proxy — they see the counter.' }));
      break;
    case 'never_archived':
      b.appendChild(el('strong', { text: 'Never written to cold storage' }));
      b.appendChild(el('span', { text:
        'The index says this session has no archived object, so the transcripts were dropped by ' +
        'retention rather than moved. The metrics above are all that remains of it.' }));
      break;
    case 'unreachable':
      box.className = 'state failed';
      b.appendChild(el('strong', { text: 'Cold storage is unreachable' }));
      b.appendChild(el('span', { text:
        'The transcripts are safe where they are — this is a failure to reach the remote, not a ' +
        'loss. Try again shortly. ' + (opts.error || '') }));
      break;
    default:
      b.appendChild(el('strong', { text: 'No transcript' }));
  }
  box.appendChild(b);
  if (opts.action) box.appendChild(opts.action);
  host.appendChild(box);
  return box;
}

/** The page behind an open dialog is inert: not clickable, not tabbable, and not read
 *  out. Without it the drawer was aria-modal in name only — Tab walked straight into the
 *  table behind the scrim. */
function setBackgroundInert(on) {
  for (const sel of ['.topbar', '.filters', '#main', '.skip']) {
    const n = $(sel);
    if (n) n.inert = on;
  }
}

/** closeDrawer is what the ✕, Escape and the scrim call. It drops the drawer out of the
 *  URL by REPLACING the entry rather than going back one: a bookmark opened straight into
 *  a drawer has no previous entry of ours to return to, and history.back() there would
 *  leave the app. Back still closes the drawer — through popstate → syncDrawer. */
function closeDrawer() {
  if (state.drawer) { state.drawer = null; syncURL(true); }
  dismissDrawer();
}

function dismissDrawer() {
  setBackgroundInert(false);
  $('#drawer').hidden = true;
  $('#scrim').hidden = true;
  if (drawerReturnFocus && document.contains(drawerReturnFocus)) drawerReturnFocus.focus();
  drawerReturnFocus = null;
}

// ── session compaction diff ────────────────────────────────────────────────
//
// "Show me what compaction did to this conversation." The request drawer answers it for
// one turn; this answers it for the whole session, which is the level at which the
// question is actually asked.
//
// Two things it is careful about:
//
//   - It does NOT reconstruct a transcript. What was captured is the messages we
//     REWROTE, not the conversation around them, so presenting a stitched-together
//     "session before compaction" would be a fabrication wherever we touched nothing.
//     The before/after views are the before/after of every span we changed, in order,
//     and the heading says exactly that.
//   - Cold storage is never touched implicitly. The first load reads local data only
//     and reports state="cold"; the fetch happens on a button, once, per the user.

/** transcriptURL builds the route, carrying only the manager's tenant selector — never
 *  the view filters, which would silently drop turns out of the middle of a session. */
function transcriptURL(session, fetchCold) {
  const p = new URLSearchParams();
  if (state.filter.tenant) p.set('tenant', state.filter.tenant);
  if (fetchCold) p.set('fetch', '1');
  const q = p.toString();
  return '/api/sessions/' + encodeURIComponent(session) + '/transcript' + (q ? '?' + q : '');
}

async function openSessionDiff(session, fetchCold, fromURL) {
  // Linkable, like the request drawer: this view is the one people want to send someone.
  if (!fromURL) { state.drawer = { diff: session }; syncURL(false); }
  const body = openDrawer('Compaction diff · ' + (session || '(no session id)'), null);
  loadingState(body, 5);
  let out;
  try {
    const res = await fetch(transcriptURL(session, fetchCold), { headers: { accept: 'application/json' } });
    if (!res.ok) {
      let msg = res.status + ' ' + res.statusText;
      let j = null;
      try { j = await res.json(); } catch (_) { /* not json */ }
      // The 404 carries a state like every other answer, so a stale bookmark renders "no
      // such session" through the normal renderer instead of throwing an HTTP error at
      // someone who did nothing wrong.
      if (j && j.state) { renderSessionDiff(body, session, j); return; }
      const e = new Error(msg); e.status = res.status; throw e;
    }
    out = await res.json();
  } catch (err) {
    errorState(clear(body), 'Could not load this session', err);
    return;
  }
  renderSessionDiff(body, session, out);
}

function renderSessionDiff(body, session, out) {
  clear(body);
  if (out.state === 'unknown_session') {
    contentBlockedState(body, 'unknown_session');
    return;
  }
  const reqs = out.requests || [];

  // Totals from the rows themselves: an archived session has no local aggregate to ask
  // for, and recomputing here means the two paths agree.
  let before = 0, after = 0, unique = 0, changed = 0, cgMs = 0;
  const compTotals = new Map();
  for (const e of reqs) {
    before += e.tokens_before || 0;
    after += e.tokens_after || 0;
    unique += e.saved_unique || 0;
    cgMs += e.cg_latency_ms || 0;
    changed += (e.content || []).length;
    for (const c of e.components || []) {
      const t = compTotals.get(c.component) || { kind: c.kind, runs: 0, acted: 0, saved: 0, reverted: 0, ms: 0 };
      t.runs++;
      if (c.acted) t.acted++;
      if (c.reverted) t.reverted++;
      t.saved += c.saved_unique || 0;
      t.ms += c.duration_ms || 0;
      compTotals.set(c.component, t);
    }
  }

  // Three different populations, and the old summary called the first one by the second's
  // name: "Turns captured: 12" was the session's TOTAL turn count while only 4 turns had
  // any stored text, and "Messages rewritten" silently counted only those 4 turns'
  // messages even though components acted on the other 8.
  const rewrittenTurns = reqs.filter(wasRewritten).length;
  const storedTurns = reqs.filter((e) => (e.content || []).length).length;
  const partial = storedTurns < rewrittenTurns;

  body.appendChild(el('div', { class: 'kv', 'data-testid': 'session-diff-summary' },
    kv('Session', session || '(none)'),
    kv('Turns in this session', num(reqs.length)),
    kv('Turns rewritten', num(rewrittenTurns)),
    kv('Turns with stored text', storedTurns + ' of ' + rewrittenTurns + ' rewritten'),
    kv('Messages rewritten' + (partial ? ' (stored turns only)' : ''), num(changed)),
    kv('Tokens before → after', compact(before) + ' → ' + compact(after)),
    kv('Saved (gross / unique)', compact(before - after) + ' / ' + compact(unique)),
    kv('context-guru latency', dur(cgMs)),
    kv('Window', reqs.length
      ? when(reqs[0].ts) + ' → ' + when(reqs[reqs.length - 1].ts)
      : (out.archive ? when(out.archive.first_ts) + ' → ' + when(out.archive.last_ts) : '—'))));

  // The state strip decides whether there is anything to diff, and is the only place
  // the cold-storage fetch can be started from.
  const st = out.state;
  if (st === 'cold') {
    const btn = el('button', {
      class: 'primary', 'data-testid': 'fetch-transcript',
      onclick: async () => {
        btn.disabled = true;
        btn.textContent = 'fetching from cold storage…';
        // Re-enter through the same path so success and every failure mode render
        // through exactly one renderer.
        await openSessionDiff(session, true);
      },
    }, 'Fetch transcript');
    const box = el('div', { class: 'state cold', 'data-testid': 'state-cold' },
      el('div', { class: 'state-body' },
        el('strong', { text: 'Transcripts are in cold storage' }),
        el('span', { text:
          'The metrics above are local and complete. The before/after text moved to ' +
          (out.remote || 'the configured remote') + ' and is not read until you ask — it is a ' +
          'network round trip, so it never happens on page load or for a list. ' +
          (out.archive ? 'Archived ' + when(out.archive.archived_at) + ', ' +
            compact((out.archive.content_bytes || 0) + (out.archive.full_bytes || 0)) + 'B compressed.' : '') })),
      btn);
    body.appendChild(box);
  } else if (st === 'not_permitted' || st === 'not_captured' || st === 'never_archived') {
    contentBlockedState(body, st, { blockedBy: out.capture_blocked_by, rewritten: rewrittenTurns > 0 });
  } else if (st === 'unreachable') {
    const retry = el('button', {
      class: 'ghost', 'data-testid': 'retry-transcript',
      onclick: () => openSessionDiff(session, true),
    }, 'Try again');
    contentBlockedState(body, 'unreachable', { error: out.error, action: retry });
  } else if (st === 'fetched') {
    body.appendChild(el('div', { class: 'state', 'data-testid': 'state-fetched' },
      el('div', { class: 'state-body' },
        el('strong', { text: 'Fetched from cold storage' }),
        el('span', { text:
          'Read-only. Nothing was written back to local disk, so this does not re-trigger the ' +
          'eviction that archived it in the first place.' }))));
  }

  if (compTotals.size) {
    body.appendChild(el('h2', { text: 'Components, across the session' }));
    const rows = Array.from(compTotals.entries()).sort((a, b) => b[1].saved - a[1].saved);
    const tbl = el('table', { class: 'tbl compact', 'data-testid': 'session-diff-components' },
      el('thead', {}, el('tr', {},
        el('th', { text: 'Component' }), el('th', { text: 'Kind' }),
        el('th', { class: 'num', text: 'Runs' }), el('th', { class: 'num', text: 'Acted' }),
        el('th', { class: 'num', text: 'Reverted' }), el('th', { class: 'num', text: 'Unique saved' }),
        el('th', { class: 'num', text: 'Own latency' }))));
    const tb = el('tbody');
    for (const [name, t] of rows) {
      tb.appendChild(el('tr', {},
        el('td', {}, el('code', { text: name })),
        el('td', { text: t.kind || '—' }),
        el('td', { class: 'num', text: num(t.runs) }),
        el('td', { class: 'num', text: num(t.acted) }),
        el('td', { class: 'num', text: num(t.reverted) }),
        el('td', { class: 'num', text: compact(t.saved) }),
        el('td', { class: 'num', text: dur(t.ms) })));
    }
    tbl.appendChild(tb);
    body.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, tbl));
  }

  const withContent = reqs.filter((e) => (e.content || []).length);
  if (!withContent.length) {
    if (st === 'hot' || st === 'fetched' || st === 'nothing_changed') {
      body.appendChild(el('h2', { text: 'What compaction changed' }));
      // Same rule as the single request: only the component rows may say "nothing was
      // rewritten". With no stored text and turns that WERE rewritten, the honest answer
      // is about storage.
      if (rewrittenTurns) contentBlockedState(body, 'not_stored', { rewritten: true });
      else {
        // Own box: emptyState clears its host, and given `body` it wiped the session
        // summary and the component table above it.
        emptyState(body.appendChild(el('div')), 'Nothing was rewritten in this session',
          'Every turn passed through unchanged. The per-request rows say why under "compaction outcome".');
      }
    }
    return;
  }

  // Partial coverage is disclosed, not left to be inferred from a block list that starts
  // at "turn 9".
  if (partial) {
    body.appendChild(el('div', { class: 'state blocked', 'data-testid': 'partial-coverage' },
      el('div', { class: 'state-body' },
        el('strong', { text: 'Partial coverage: ' + (rewrittenTurns - storedTurns) + ' of ' +
          rewrittenTurns + ' rewritten turns have no stored text' }),
        el('span', { text:
          'The blocks below are the ' + storedTurns + ' turn(s) whose before/after was stored, so ' +
          '"messages rewritten" counts those only and undercounts the session. The missing ' +
          'turns ran before transcript storage was switched on, or exceeded its size cap — ' +
          'their metrics and component rows are complete either way.' }))));
  }

  body.appendChild(el('h2', { text: 'What compaction changed, oldest turn first' }));
  body.appendChild(el('p', { class: 'note', text:
    'Each block is one message context-guru rewrote. This is the set of spans we touched — ' +
    'not a reconstructed transcript: the messages we left alone were never captured, so ' +
    'stitching them into a whole conversation would be inventing the parts we did not store.' }));

  // One toolbar drives every block, which is what makes (a) "the session before
  // compaction" and (b) "after" single clicks rather than N expansions.
  const blocks = [];
  const bar = el('div', { class: 'difftoolbar', 'data-testid': 'session-diff-modes' },
    el('span', { text: 'All blocks:' }));
  const allButtons = [];
  for (const [m, label] of DIFF_MODES) {
    const b = el('button', {
      class: 'ghost small', 'data-mode': m, 'aria-pressed': String(m === 'git'),
      onclick: () => {
        for (const x of allButtons) x.setAttribute('aria-pressed', String(x.dataset.mode === m));
        for (const blk of blocks) { blk.open = true; blk.setMode(m); }
      },
    }, label);
    allButtons.push(b);
    bar.appendChild(b);
  }
  bar.appendChild(el('span', { class: 'spacer' }));
  bar.appendChild(el('button', {
    class: 'ghost small',
    onclick: () => { for (const blk of blocks) blk.open = !blk.open; },
  }, 'Expand / collapse all'));
  body.appendChild(bar);

  // Turn numbers come from the position in the FULL request list, not in the filtered
  // one: "turn 3" has to mean the third turn of the conversation, not the third turn
  // that happened to be rewritten.
  const turnOf = new Map(reqs.map((e, i) => [e, i + 1]));
  for (const e of withContent) {
    for (const c of e.content) {
      const blk = diffBlock(c, e.components, {
        prefix: 'turn ' + turnOf.get(e) + ' · ',
        open: blocks.length === 0,
      });
      blocks.push(blk);
      body.appendChild(blk);
    }
  }
}

// ── benchmarks ─────────────────────────────────────────────────────────────
async function loadBenchmarks() {
  const host = clear($('#bench-list'));
  loadingState(host, 3);
  try {
    const { runs } = await api('benchmarks');
    clear(host);
    if (!runs || !runs.length) {
      emptyState(host, 'No benchmark runs ingested',
        'Point --dash-bench-dirs at a harness jobs root (a directory of runs, each with summary.json and rows-*.json) and re-scan.');
      return;
    }
    // 42 ingested runs rendered flat is 40k pixels of table. Collapse each run and
    // open only the newest, so the view opens on the run you just finished.
    runs.forEach((run, runIdx) => {
      const sec = el('details', { class: 'panel diff', 'data-testid': 'bench-run' });
      if (runIdx === 0) sec.open = true;
      const armNames = (run.arms || []).map((a) => a.arm).join(', ');
      sec.appendChild(el('summary', {},
        el('strong', { text: run.name }),
        '  ' + [run.dataset, run.model, armNames && 'arms: ' + armNames,
          when(run.ts)].filter(Boolean).join(' · ')));
      const inner = el('div', { style: 'padding:12px 14px' });
      const tbl = el('table', { class: 'tbl' }, el('thead', {}, el('tr', {},
        el('th', { text: 'Arm' }), el('th', { class: 'num', text: 'Tasks' }),
        el('th', { class: 'num', text: 'Solved' }), el('th', { class: 'num', text: 'Solve rate' }),
        el('th', { class: 'num', text: 'Mean reward' }), el('th', { class: 'num', text: 'Mean steps' }),
        el('th', { class: 'num', text: 'Total cost' }), el('th', { class: 'num', text: 'Cost / task' }),
        el('th', { class: 'num', text: '$ per solve' }),
        el('th', { class: 'num', text: 'Cache hit' }), el('th', { class: 'num', text: 'Mean wall' }),
        el('th', { class: 'num', text: 'Exceptions' }))));
      const tb = el('tbody');
      for (const a of run.arms || []) {
        const perSolve = a.solved > 0 ? a.total_cost_usd / a.solved : null;
        tb.appendChild(el('tr', { class: 'click', onclick: () => toggleBenchTasks(inner, run.id, a.arm) },
          el('td', {}, el('code', { text: a.arm })),
          el('td', { class: 'num', text: num(a.tasks) }),
          el('td', { class: 'num', text: num(a.solved) }),
          el('td', { class: 'num', text: pct(a.solve_rate * 100) }),
          el('td', { class: 'num', text: a.mean_reward.toFixed(3) }),
          el('td', { class: 'num', text: a.mean_steps.toFixed(1) }),
          el('td', { class: 'num', text: usd(a.total_cost_usd) }),
          el('td', { class: 'num', text: usd(a.mean_cost_usd) }),
          el('td', { class: 'num', text: perSolve === null ? '—' : usd(perSolve) }),
          el('td', { class: 'num', text: pct(a.cache_hit_rate * 100, 2) }),
          el('td', { class: 'num', text: dur(a.mean_wall_s * 1000) }),
          el('td', { class: 'num', text: num(a.exceptions) })));
      }
      tbl.appendChild(tb);
      inner.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, tbl));
      inner.appendChild(el('p', { class: 'note', text: 'Cost per solve is the number that matters: an arm that spends less by solving fewer tasks has not saved anything. Click an arm for its per-task rows.' }));
      // Cost-vs-reward scatter: the visualization the issue asks for.
      inner.appendChild(el('h2', { text: 'Cost vs reward, by arm' }));
      const scatter = el('div', { class: 'chart', 'data-testid': 'bench-scatter' });
      inner.appendChild(scatter);
      sec.appendChild(inner);
      host.appendChild(sec);
      renderScatter(scatter, run.arms || []);
    });
  } catch (err) {
    if (aborted(err)) return;
    errorState(host, 'Could not load benchmarks', err);
  }
}

function renderScatter(host, arms) {
  clear(host);
  const pts = arms.filter((a) => a.tasks > 0);
  if (!pts.length) { emptyState(host, 'No arms to plot', 'An arm appears here once it has at least one task.'); return; }
  const { w, h, pad } = geom(host);
  const xMax = Math.max(...pts.map((a) => a.mean_cost_usd)) * 1.15 || 1;
  const svg = svgEl('svg', { viewBox: `0 0 ${w} ${h}`, role: 'img' });
  svg.setAttribute('aria-label', 'mean cost per task versus solve rate, by arm');
  const px = (v) => pad.l + (v / xMax) * (w - pad.l - pad.r);
  const py = (v) => h - pad.b - v * (h - pad.t - pad.b);
  for (const t of [0, 0.25, 0.5, 0.75, 1]) {
    svg.appendChild(svgEl('line', { class: 'gridline', x1: pad.l, x2: w - pad.r, y1: py(t), y2: py(t) }));
    const lab = svgEl('text', { class: 'axis-text', x: pad.l - 6, y: py(t) + 3, 'text-anchor': 'end' });
    lab.textContent = (t * 100).toFixed(0) + '%';
    svg.appendChild(lab);
  }
  svg.appendChild(svgEl('line', { class: 'axis', x1: pad.l, x2: w - pad.r, y1: h - pad.b, y2: h - pad.b }));
  // Snapped ticks here as well as on the line charts: dividing the range in half gave
  // "$0 / $2.38 / $4.76", three numbers nobody compares at a glance.
  for (const t of ticks(0, xMax, 4)) {
    if (t > xMax) continue;
    const lab = svgEl('text', { class: 'axis-text', x: px(t), y: h - pad.b + 14, 'text-anchor': 'middle' });
    lab.textContent = usd(t);
    svg.appendChild(lab);
  }
  // Direct labels are the only identity cue on this chart, so they must not overlap.
  // Two arms within a label's width of each other used to print on top of each other,
  // which at 620px turned "codesmart" and "off" into one unreadable smear.
  // Every MARK is an obstacle too: a label that clears its neighbours' labels can still
  // land under a neighbour's dot, which is what turned two arms into one smear at 620px.
  const placed = pts.map((a) => ({ x0: px(a.mean_cost_usd) - 8, x1: px(a.mean_cost_usd) + 8, y: py(a.solve_rate) + 4 }));
  pts.forEach((a, i) => {
    const cx = px(a.mean_cost_usd), cy = py(a.solve_rate);
    // Every arm is directly labelled, so identity never depends on the fill. Past the
    // four validated hues the marks go neutral rather than cycling back to blue and
    // making two different arms look like the same one.
    svg.appendChild(svgEl('circle', {
      cx, cy, r: 6, fill: i < SERIES.length ? SERIES[i] : 'var(--fg-muted)',
      stroke: 'var(--bg-raised)', 'stroke-width': 2,
    }));
    const wid = a.arm.length * 6.5;                 // ~6.5px per char at 11px
    const flip = cx + wid + 11 > w - pad.r;         // near the right edge: label to the left
    const x0 = flip ? cx - 11 - wid : cx + 11;
    let ly = cy + 4;
    // Nudge upward until this label's own box clears every box already placed. Comparing
    // the drawn interval, not the mark's x, is what makes it work for flipped labels too.
    while (placed.some((p) => x0 < p.x1 && p.x0 < x0 + wid && Math.abs(p.y - ly) < 13)) ly -= 13;
    placed.push({ x0, x1: x0 + wid, y: ly });
    const lab = svgEl('text', {
      class: 'mark-label', x: flip ? cx - 11 : cx + 11, y: ly,
      'text-anchor': flip ? 'end' : 'start',
    });
    lab.textContent = a.arm;
    svg.appendChild(lab);
  });
  host.appendChild(svg);
  host.appendChild(el('div', { class: 'legend' },
    el('span', { text: 'x: mean billed cost per task  ·  y: solve rate  ·  up and to the left is better' })));
}

async function toggleBenchTasks(sec, runID, arm) {
  const existing = sec.querySelector('[data-tasks="' + arm + '"]');
  if (existing) { existing.remove(); return; }
  const host = el('div', { class: 'tblwrap', tabindex: '0', 'data-tasks': arm, 'data-testid': 'bench-tasks' });
  sec.appendChild(host);
  loadingState(host, 2);
  try {
    const { tasks } = await api('benchmarks/' + runID + '/tasks', { arm });
    clear(host);
    const tbl = el('table', { class: 'tbl compact' }, el('thead', {}, el('tr', {},
      el('th', { text: 'Task' }), el('th', { class: 'num', text: 'Reward' }),
      el('th', { class: 'num', text: 'Steps' }), el('th', { class: 'num', text: 'Cache r/w' }),
      el('th', { class: 'num', text: 'Fresh' }), el('th', { class: 'num', text: 'Out' }),
      el('th', { class: 'num', text: 'Cost' }), el('th', { class: 'num', text: 'Wall' }), el('th', { text: '' }))));
    const tb = el('tbody');
    for (const t of tasks) {
      tb.appendChild(el('tr', {},
        el('td', {}, el('span', { class: 'trunc', title: t.task, text: t.task })),
        el('td', { class: 'num', text: t.reward.toFixed(2) }),
        el('td', { class: 'num', text: num(t.steps) }),
        el('td', { class: 'num', text: compact(t.cache_read) + ' / ' + compact(t.cache_write) }),
        el('td', { class: 'num', text: compact(t.fresh_input) }),
        el('td', { class: 'num', text: compact(t.completion_tokens) }),
        el('td', { class: 'num', text: usd(t.cost_usd) }),
        el('td', { class: 'num', text: dur(t.wall_s * 1000) }),
        el('td', {}, t.exception ? el('span', { class: 'pill missing', text: 'exception' })
          : t.reward >= 1 ? el('span', { class: 'pill complete', text: 'solved' })
            : el('span', { class: 'pill neutral', text: 'unsolved' }))));
    }
    tbl.appendChild(tb);
    host.appendChild(tbl);
  } catch (err) {
    errorState(host, 'Could not load tasks', err);
  }
}

// ── config ─────────────────────────────────────────────────────────────────
function renderTree(v, key) {
  if (v === null || v === undefined) return el('div', { class: 'v', text: '—' });
  if (Array.isArray(v)) {
    return el('div', {}, el('div', { class: 'k', text: key }),
      el('div', { class: 'v', text: v.map((x) => (typeof x === 'object' ? JSON.stringify(x) : String(x))).join(', ') || '(empty)' }));
  }
  if (typeof v === 'object') {
    const box = el('details', { class: 'diff', open: key === undefined ? 'open' : null },
      el('summary', { text: key === undefined ? 'effective configuration' : key }));
    const inner = el('div', { style: 'padding:10px 12px' });
    const grid = el('div', { class: 'kv' });
    for (const [k, val] of Object.entries(v)) {
      if (val !== null && typeof val === 'object' && !Array.isArray(val)) inner.appendChild(renderTree(val, k));
      else grid.appendChild(kv(k, Array.isArray(val) ? (val.join(', ') || '(empty)') : String(val)));
    }
    inner.insertBefore(grid, inner.firstChild);
    box.appendChild(inner);
    return box;
  }
  return el('div', {}, el('div', { class: 'k', text: key }), el('div', { class: 'v', text: String(v) }));
}

async function loadConfig() {
  const host = clear($('#config-body'));
  loadingState(host, 3);
  renderLogsHelp();
  try {
    // The payload is {scope, config, description} — the same envelope /api/capture uses.
    // Rendering the envelope as the tree printed "scope: server" and "description: …" as
    // if they were configuration keys, and buried the config a level down.
    const cfg = await api('config');
    clear(host);
    // The caveat that matters stays in the layout; the server's full explanation folds.
    host.appendChild(el('p', { class: 'note', 'data-testid': 'config-scope', text:
      'Scope: ' + (cfg.scope || 'server') + ' — what this PROXY runs, not necessarily what ' +
      'compacted your traffic.' }));
    if (cfg.description) host.appendChild(whyBlock('Why those can differ', cfg.description));
    host.appendChild(renderTree(cfg.config || cfg));
  } catch (err) {
    if (aborted(err)) return;
    emptyState(host, err.status === 403 ? 'Configuration is not visible from this address'
      : 'Could not load configuration', String(err.message || err), { error: err.status !== 403 });
  }
  const chost = clear($('#capture-body'));
  // The capture counters are PROCESS-WIDE, so hosted mode serves them to managers only
  // (they leak other tenants' traffic volume). Rendering them as zeros for everyone else
  // would assert "nothing was dropped", which is a claim this page cannot make.
  if (account.hosted && !isManager()) {
    contentBlockedState(chost, 'capture_not_permitted');
    return;
  }
  try {
    const { capture: c, description } = await api('capture');
    chost.appendChild(el('div', { class: 'kv' },
      kv('Captured', num(c.captured)), kv('Written', num(c.written)),
      kv('Dropped', num(c.dropped)), kv('Insert errors', num(c.errors)),
      kv('Queue', c.queued + ' / ' + c.queue_cap), kv('SSE clients', num(c.sse_clients)),
      kv('Database', c.db_path || '(in memory — history is lost on restart)'),
      kv('Database size', compact(c.db_bytes) + ' B')));
    // A non-zero drop count is the one thing worth saying out loud here, because it means
    // every number on the Overview under-reports. The definitions fold.
    if (c.dropped > 0) {
      chost.appendChild(el('p', { class: 'note warn-text', text:
        num(c.dropped) + ' events dropped — every figure on this dashboard under-reports by ' +
        'that much. Raise the queue size or lower the traffic before drawing conclusions.' }));
    }
    if (description) chost.appendChild(whyBlock('What these counters mean', description));
  } catch (err) {
    errorState(chost, 'Could not load capture health', err);
  }
}

// ── capture-drop + observe-mode banners ────────────────────────────────────
async function checkCapture() {
  try {
    const { capture: c } = await api('capture');
    const b = $('#capture-warning');
    if (c.dropped > 0) {
      b.textContent = `${num(c.dropped)} captured request(s) were dropped because the capture queue was full — ` +
        'the figures below under-report. Requests were never delayed; observability was. Raise the queue size.';
      b.hidden = false;
    } else b.hidden = true;

    // Observe mode has to be unmissable. Every request was forwarded UNTOUCHED, so
    // reading these figures as achieved savings is exactly the wrong conclusion — and it
    // is the conclusion a dashboard invites unless it says otherwise.
    const o = $('#observe-banner');
    if (c.mode === 'observe') {
      const q = c.observe_queue;
      let text = 'You are currently in OBSERVE mode. context-guru did not modify any request: ' +
        'every request above was forwarded to the provider untouched, and the pipeline ran ' +
        'off-path on a copy. Savings shown here are what compaction WOULD have achieved, ' +
        'not what it did.';
      if (q) {
        text += ` Off-path queue: ${num(q.processed)} measured, ${num(q.pending)} in flight`;
        // Drops matter more than depth: a dropped observation never happened, so the
        // projection understates. Say which direction the error runs.
        text += q.dropped > 0
          ? `, ${num(q.dropped)} DROPPED — the projection under-reports by whatever those would have saved.`
          : ', 0 dropped.';
      }
      o.textContent = text;
      o.hidden = false;
    } else o.hidden = true;
  } catch (_) { /* the banners are best-effort */ }
}

// ── views + filters ────────────────────────────────────────────────────────
const loaders = {
  overview: loadOverview, usage: loadUsage, components: loadComponents, sessions: loadSessions,
  requests: loadRequests, benchmarks: loadBenchmarks, config: loadConfig,
};

/**
 * DIMS is every filter dimension, and it is the single list the whole filter layer
 * reads: the URL, the chips, the facet dropdowns and the "why is this empty" copy.
 *
 * The third column is the control, where one exists. `session` has NO control — it is set
 * by drilling in from the Sessions table — and that
 * is exactly what the reported bug was: state.filter was rebuilt from the DOM on every
 * change, so a filter with no control could only be got rid of by pressing Clear, and
 * a filter with no control and no chip could not even be SEEN. Nothing here is
 * DOM-derived any more: state.filter is the truth, the controls and chips are its view.
 */
const DIMS = [
  ['q', 'search', '#f-q'],
  ['model', 'model', '#f-model'],
  ['provider', 'provider', '#f-provider'],
  ['agent', 'agent', '#f-agent'],
  ['preset', 'preset', '#f-preset'],
  ['mode', 'mode', '#f-mode'],
  ['component', 'component', '#f-component'],
  ['reason', 'outcome', '#f-reason'],
  ['accounting', 'accounting', '#f-accounting'],
  ['effort', 'effort', '#f-effort'],
  ['thinking', 'thinking', '#f-thinking'],
  ['stop_reason', 'stop reason', '#f-stop_reason'],
  ['session', 'session', null],
  ['tenant', 'tenant', '#f-tenant'],
];
/** The facet dimensions the server can enumerate; the rest are fixed option lists. */
const FACET_DIMS = ['model', 'provider', 'agent', 'preset', 'mode', 'component',
  'effort', 'thinking', 'stop_reason'];

/** activeFilters lists the set filters as [key, label, value], time range included. */
function activeFilters() {
  const out = DIMS.filter(([k]) => state.filter[k]).map(([k, label]) => [k, label, state.filter[k]]);
  if (hasRange()) out.push(['range', 'range', rangeLabel()]);
  return out;
}
/**
 * QUICK_RANGES are the relative windows offered in the popover. The label is also the chip
 * text and the summary text, so there is one wording per window rather than three.
 */
const QUICK_RANGES = [
  ['now-5m', 'Last 5 minutes'], ['now-15m', 'Last 15 minutes'], ['now-1h', 'Last hour'],
  ['now-6h', 'Last 6 hours'], ['now-12h', 'Last 12 hours'], ['now-24h', 'Last 24 hours'],
  ['now-2d', 'Last 2 days'], ['now-7d', 'Last 7 days'], ['now-30d', 'Last 30 days'],
  [0, 'All time'],
];
function rangeLabel() {
  if (state.to === 'now') {
    const q = QUICK_RANGES.find(([tok]) => tok === state.from);
    if (q) return q[1].toLowerCase();
    if (!state.from) return 'all time';
    return 'since ' + when(resolveTime(state.from, state.nowMs || Date.now()));
  }
  const [since, until] = rangeMs();
  if (!since) return 'up to ' + when(until);
  return when(since) + ' → ' + when(until);
}
/** describeFilters renders the active set the way a person would say it out loud. */
function describeFilters() {
  return activeFilters().map(([k, label, v]) => (k === 'range' ? 'in the ' + v : label + '=' + v)).join(' + ');
}

function go(view, push = true) {
  // Nothing is loadable while the gate is up: every loader would 401 and paint an error
  // over a login form. A typed #requests used to do exactly that.
  if (!$('#gate').hidden) return;
  if (!Object.prototype.hasOwnProperty.call(loaders, view)) view = 'overview';
  // A view whose tab this account is not entitled to is not reachable by typing its
  // hash either: its loader would 401/403 and paint an error nobody can act on.
  const tab = $(`.tab[data-view="${view}"]`);
  if (tab && tab.hidden) view = 'overview';
  state.view = view;
  for (const t of $$('.tab')) t.setAttribute('aria-selected', String(t.dataset.view === view));
  for (const s of $$('.view')) s.hidden = s.id !== 'view-' + view;
  // A filter bar over a view with nothing to filter is thirteen controls inviting clicks
  // that change nothing — and on Settings it sat directly above a form, so the two read as
  // one set of inputs. These four views read no filter at all (see refresh).
  $('.filters').hidden = UNFILTERED_VIEWS.has(view);
  syncURL(!push);
  refresh();
}

/** The views whose data is not scoped by the filter bar: account configuration, the tenant
 *  roster, cold storage and the process-wide configuration. */
const UNFILTERED_VIEWS = new Set(['setup', 'settings', 'tenants', 'archive', 'config']);

/**
 * setFilter is the ONE way a filter changes, wherever the change comes from: a
 * dropdown, the search box, a row click that drills into a session, or a removed chip.
 * Every caller therefore gets the same four consequences — state updated, control
 * synced, pagination reset, URL pushed — which is what makes "change a filter" a
 * single action instead of a sequence a caller can get half-right.
 */
function setFilter(key, value, opts = {}) {
  const v = (value || '').trim();
  // A no-op change is not a change: the search box fires `input` and then `search` for
  // one press of its clear affordance, and refetching twice for that means cancelling a
  // request that was about to answer the same question.
  if (v === (state.filter[key] || '') && !opts.force) return;
  if (v) state.filter[key] = v; else delete state.filter[key];
  syncControl(key);
  resetPaging();
  if (opts.quiet) return;
  syncURL();
  refresh();
}
/** setRange is the one way the window changes: a quick token, or an absolute pair. */
function setRange(from, to = 'now') {
  state.from = from || 0;
  state.to = to || 'now';
  syncRangeControl();
  resetPaging();
  syncURL();
  refresh();
}
function clearFilters() {
  state.filter = {};
  state.from = 0;
  state.to = 'now';
  for (const [k] of DIMS) syncControl(k);
  syncRangeControl();
  resetPaging();
  syncURL();
  refresh();
}
/**
 * initRange builds the quick-range buttons and wires the absolute pair. Called once.
 *
 * The two <input type="datetime-local"> are the platform's own picker: nothing here needs a
 * date library, and a native picker is the one a user already knows how to type into.
 */
function initRange() {
  const quick = clear($('#f-range-quick'));
  for (const [tok, label] of QUICK_RANGES) {
    quick.appendChild(el('button', {
      class: 'ghost small', 'data-testid': 'range-' + (tok || 'all'),
      onclick: () => { $('#f-range').open = false; setRange(tok); },
    }, label));
  }
  $('#f-range-apply').addEventListener('click', () => {
    const from = localToMs($('#f-from').value);
    const to = localToMs($('#f-to').value);
    // Either bound alone is a legitimate window ("everything before Friday"), so this does
    // not demand both. Both empty means the same as All time.
    if (!from && !to) { setRange(0); return; }
    $('#f-range').open = false;
    setRange(from || 0, to || 'now');
  });
  syncRangeControl();
}
/** localToMs reads a datetime-local value (no zone, so it is the VIEWER's local time). */
function localToMs(v) { const t = v ? Date.parse(v) : NaN; return Number.isFinite(t) ? t : 0; }
/** msToLocal writes one back, in the form the input accepts: YYYY-MM-DDTHH:mm, local. */
function msToLocal(ms) {
  if (!ms) return '';
  const d = new Date(ms - new Date(ms).getTimezoneOffset() * 60000);
  return d.toISOString().slice(0, 16);
}
/** syncRangeControl makes the popover show what state.from/to actually say. */
function syncRangeControl() {
  const label = $('#f-range-label');
  if (!label) return;
  label.textContent = rangeLabel().replace(/^./, (c) => c.toUpperCase());
  const [since, until] = rangeMs();
  // An absolute window fills the inputs; a relative one leaves them empty rather than
  // printing a resolved instant the window is not actually pinned to.
  $('#f-from').value = typeof state.from === 'number' && since ? msToLocal(since) : '';
  $('#f-to').value = state.to === 'now' ? '' : msToLocal(until);
}

// ── sortable columns ───────────────────────────────────────────────────────
/**
 * sortable turns a static <thead> into a sortable one. `keys` is one entry per column, in
 * column order; a null means that column is not sortable.
 *
 * Only wired where the sort can be honest — see sortRows. A <button> inside the <th>, not a
 * click handler on the <th>, because a header a mouse can activate and a keyboard cannot is
 * not a control. aria-sort goes on the <th>, which is where a screen reader looks for it.
 */
function sortable(tableSel, keys) {
  const ths = $$('thead th', $(tableSel));
  keys.forEach((key, i) => {
    const th = ths[i];
    if (!th || !key) return;
    const label = th.textContent;
    clear(th).appendChild(el('button', {
      class: 'sort', title: 'Sort by ' + label, onclick: () => toggleSort(key),
    }, label));
  });
  syncSortHeads(tableSel, keys);
}
/** syncSortHeads publishes the current sort on the headers. */
function syncSortHeads(tableSel, keys) {
  const ths = $$('thead th', $(tableSel));
  keys.forEach((key, i) => {
    const th = ths[i];
    if (!th || !key) return;
    th.setAttribute('aria-sort', key === state.sort
      ? (state.dir === 'asc' ? 'ascending' : 'descending') : 'none');
  });
}
/** toggleSort flips direction on the current column, or takes over a new one descending. */
function toggleSort(key) {
  state.dir = state.sort === key && state.dir === 'desc' ? 'asc' : 'desc';
  state.sort = key;
  resetPaging();
  syncURL();
  refresh();
}
/**
 * sortRows sorts a COMPLETE result set in place. Numbers compare numerically, everything
 * else by locale — one comparator, because a column is one type in every row.
 *
 * "Complete" is load-bearing: /api/components returns every row, so sorting it here is the
 * whole answer. Sessions and Requests are paginated server-side, so the same code applied
 * there would label a column "Net saved $" and show the top spender of an arbitrary page.
 * They are deliberately NOT wired — see docs/dashboard.md.
 */
function sortRows(rows, key, dir) {
  const sign = dir === 'asc' ? 1 : -1;
  return rows.slice().sort((a, b) => {
    const x = a[key], y = b[key];
    if (typeof x === 'number' || typeof y === 'number') return sign * ((x || 0) - (y || 0));
    return sign * String(x || '').localeCompare(String(y || ''));
  });
}

/** Push the state value into the control, adding the option if the facets dropped it. */
function syncControl(key) {
  const dim = DIMS.find(([k]) => k === key);
  const sel = dim && dim[2] ? $(dim[2]) : null;
  if (!sel) return;
  const want = state.filter[key] || '';
  if (sel.tagName === 'SELECT' && want && !Array.from(sel.options).some((o) => o.value === want)) {
    sel.appendChild(el('option', { value: want }, want + ' (no rows now)'));
  }
  if (sel.value !== want) sel.value = want;
}

function resetPaging() { state.reqCursor = 0; state.reqStack = []; state.sessOffset = 0; }

/** refresh aborts whatever the previous filter state was still fetching, then reloads
 *  the current view, the chips and the facet lists. */
function refresh() {
  // ONE clock reading for the whole repaint. Every relative token resolves against it, so
  // the tiles, the series and the breakdown describe the same window.
  state.nowMs = Date.now();
  if (state.ac) state.ac.abort();
  state.ac = new AbortController();
  renderChips();
  // The live feed is not filtered, and it says so — but only renderLive writes that
  // line, and the feed is otherwise only redrawn when an event arrives. A filter
  // changed with an idle proxy would leave the disclaimer out.
  if (state.view === 'overview') renderLive();
  loaders[state.view]();
  if (state.view !== 'setup' && state.view !== 'settings') loadFacets();
}

// ── URL state ──────────────────────────────────────────────────────────────
// Every filter lives in the URL, so a filtered view is a link, a reload keeps it, and
// Back undoes the last filter change rather than only the last tab change. Filter
// changes pushState; the initial normalisation replaces, so Back never lands on the
// unfiltered page the user never actually looked at.
function urlFor() {
  const p = new URLSearchParams();
  for (const [k] of DIMS) if (state.filter[k]) p.set(k, state.filter[k]);
  // from/to rather than one duration, and only when they are not the defaults. `to` is
  // omitted while it is 'now' so a shared link to a live window stays live for its reader.
  if (state.from) p.set('from', String(state.from));
  if (state.to !== 'now') p.set('to', String(state.to));
  // A sort is a VIEW of the same rows, so it belongs in the URL (a link reproduces what its
  // author was looking at) but gets no chip: it narrows nothing.
  if (state.sort && state.view === 'components') { p.set('sort', state.sort); p.set('dir', state.dir); }
  // The open drawer is state too, so a request and a session diff are both linkable and
  // Back dismisses the drawer rather than undoing the last filter change — which was the
  // one thing that made Back dangerous here. `diff` rather than `session` because
  // `session` is already a filter dimension and they are not the same thing.
  if (state.drawer && state.drawer.req) p.set('req', String(state.drawer.req));
  if (state.drawer && state.drawer.diff) p.set('diff', state.drawer.diff);
  // The manager's account editor is state too: "the account I mean" is a thing managers
  // send each other, and Back must close the panel rather than undo a filter change.
  if (state.drawer && state.drawer.acct) p.set('acct', state.drawer.acct);
  const q = p.toString();
  return location.pathname + '#' + state.view + (q ? '?' + q : '');
}
function syncURL(replace) {
  const url = urlFor();
  if (location.pathname + location.hash === url) return;
  if (replace) history.replaceState(null, '', url);
  else history.pushState(null, '', url);
}
function parseURL() {
  const [view, query] = (location.hash || '').replace(/^#/, '').split('?');
  const p = new URLSearchParams(query || '');
  const filter = {};
  for (const [k] of DIMS) if (p.get(k)) filter[k] = p.get(k);
  const req = Number(p.get('req')) || 0;
  const diff = p.get('diff') || '';
  const acct = p.get('acct') || '';
  // Legacy `range=<ms>` bookmarks: the same window, said the new way. Kept because links
  // into this dashboard are pasted into issues and they should not quietly widen to all time.
  const legacy = Number(p.get('range')) || 0;
  let from = p.get('from') || (legacy ? 'now-' + legacy + 'ms' : 0);
  if (legacy) from = legacyFrom(legacy);
  return {
    view: view || 'overview', filter,
    from: numish(from), to: numish(p.get('to') || 'now'),
    sort: p.get('sort') || '', dir: p.get('dir') === 'asc' ? 'asc' : 'desc',
    drawer: req ? { req } : diff ? { diff } : acct ? { acct } : null,
  };
}
/** numish keeps a relative token as a string and an absolute stamp as a number. */
function numish(v) {
  if (typeof v !== 'string' || /^now/.test(v)) return v || 0;
  const n = Number(v);
  return Number.isFinite(n) && n > 0 ? n : 0;
}
/** legacyFrom maps an old `range=<ms>` duration onto the nearest relative token. */
function legacyFrom(ms) {
  const unit = [['w', 604800000], ['d', 86400000], ['h', 3600000], ['m', 60000], ['s', 1000]]
    .find(([, u]) => ms % u === 0 && ms >= u);
  return unit ? 'now-' + ms / unit[1] + unit[0] : 'now-' + Math.round(ms / 1000) + 's';
}
/** applyURL makes the page match the address bar. Used on load, on Back/Forward, and
 *  when someone edits the hash by hand — one reader for all three. */
function applyURL() {
  const want = parseURL();
  state.filter = want.filter;
  state.from = want.from;
  state.to = want.to;
  state.sort = want.sort;
  state.dir = want.dir;
  // state.drawer is adopted BEFORE go(), because go() calls syncURL(replace) and would
  // otherwise rewrite the entry we just navigated to with the drawer we are leaving.
  const prev = state.drawer;
  state.drawer = want.drawer;
  for (const [k] of DIMS) syncControl(k);
  syncRangeControl();
  resetPaging();
  go(want.view, false);
  syncDrawer(prev);
}

/** syncDrawer makes the drawer match state.drawer, which the address bar has just set:
 *  Back closes it, Forward and a pasted link open it. Nothing here touches history. */
function syncDrawer(prev) {
  const want = state.drawer;
  if (!want) { if (!$('#drawer').hidden) dismissDrawer(); return; }
  const same = !!prev && prev.req === want.req && prev.diff === want.diff
    && prev.acct === want.acct;
  if (same && !$('#drawer').hidden) return;
  if (want.req) openRequest(want.req, true);
  else if (want.acct) openTenantEditor(want.acct, true);
  else openSessionDiff(want.diff, false, true);
}

// ── active-filter chips ────────────────────────────────────────────────────
/**
 * renderChips shows what is actually being filtered on, one removable chip each.
 *
 * This is the visible half of the bug fix: a session or tenant filter set by drilling
 * into a row had no control and no chip, so it was invisible AND unremovable except by
 * Clear — which is why filtering felt like it needed a Clear between every use.
 */
function renderChips() {
  const host = clear($('#f-active'));
  const active = activeFilters();
  host.hidden = !active.length;
  syncMoreCount(active);
  // Clear is the convenience, never the necessity: it says how many it will remove and
  // is disabled when there is nothing to remove, rather than sitting there implying the
  // list might be filtered when it is not.
  const btn = $('#f-clear');
  btn.disabled = !active.length;
  btn.textContent = active.length ? 'Clear ' + active.length : 'Clear';
  if (!active.length) {
    // The last chip removed itself out of existence; a disabled Clear cannot hold focus.
    if (chipFocus !== null) { $('#f-q').focus(); chipFocus = null; }
    return;
  }
  host.appendChild(el('span', { class: 'small muted', text: 'Filtering on' }));
  active.forEach(([key, label, value], i) => {
    host.appendChild(el('button', {
      class: 'chip removable', 'data-testid': 'chip-' + key,
      // The accessible name says what the button DOES; the visible text says what is
      // filtered. "agent: bob ✕" read aloud on its own does not mention removal.
      'aria-label': `Remove filter ${label} ${value}`,
      title: `Remove the ${label} filter`,
      onclick: () => { chipFocus = i; if (key === 'range') setRange(0); else setFilter(key, ''); },
    }, label + ': ' + value, el('span', { class: 'x', 'aria-hidden': 'true' }, '✕')));
  });
  // Removing a chip destroys the element focus was on, which for a keyboard user means
  // being dumped back at the top of the document. Land on whatever took its place.
  if (chipFocus !== null) {
    const chips = $$('.chip.removable', host);
    (chips[Math.min(chipFocus, chips.length - 1)] || $('#f-q')).focus();
    chipFocus = null;
  }
}
let chipFocus = null;

/** MORE_FILTERS is which dimensions live behind the "More filters" disclosure. */
const MORE_FILTERS = new Set(['reason', 'component', 'effort', 'thinking', 'stop_reason', 'accounting']);

/**
 * syncMoreCount badges the disclosure with how many of the filters inside it are set, and
 * opens it when one is.
 *
 * A collapsed control holding a value is a hidden filter, which is the exact bug the chip
 * row exists to prevent — so the count is the second guard: the summary itself says
 * something in there is narrowing the page, whether or not the reader looks at the chips.
 */
function syncMoreCount(active) {
  const badge = $('#f-more-count');
  if (!badge) return;
  const n = active.filter(([k]) => MORE_FILTERS.has(k)).length;
  badge.textContent = n ? String(n) : '';
  badge.hidden = !n;
  if (n) $('#f-more').open = true;
}

/**
 * renderNoMatch explains an empty list instead of just being empty, and offers the one
 * click that fixes it: drop the NARROWEST filter. Which filter that is cannot be
 * guessed from the values, so it is measured — one count per active filter, with that
 * filter left out — and only on this path, where the list is already empty and the
 * user is waiting for an answer rather than for rows.
 */
function renderNoMatch(body, cols, noun) {
  const active = activeFilters();
  if (!active.length) {
    tableMessage(body, cols, 'No ' + noun + ' captured yet',
      'Send traffic through the proxy and rows appear here.');
    return;
  }
  const cell = el('td', { colspan: String(cols) });
  clear(body).appendChild(el('tr', {}, cell));
  const box = emptyState(cell, 'No ' + noun + ' match ' + describeFilters(),
    'Every filter above is combined with AND, so one narrow value empties the list. ' +
    'Remove any single one from the chips in the filter bar, or:');
  const slot = el('div', { class: 'empty-action' }, el('span', { class: 'small muted' }, 'checking which filter is the narrowest…'));
  box.appendChild(slot);
  suggestDrop(active).then((best) => {
    clear(slot);
    if (!best) {
      slot.appendChild(el('span', { class: 'small muted' },
        'Dropping any one filter still matches nothing — the combination is empty from more than one side. '));
      slot.appendChild(el('button', { class: 'ghost', 'data-testid': 'nomatch-clear', onclick: clearFilters }, 'Clear all filters'));
      return;
    }
    slot.appendChild(el('button', {
      class: 'ghost', 'data-testid': 'nomatch-drop',
      onclick: () => (best.key === 'range' ? setRange(0) : setFilter(best.key, '')),
    }, `Drop ${best.label}${best.key === 'range' ? '' : '=' + best.value} — ${num(best.total)} requests match without it`));
  }).catch(() => { clear(slot); });
}

/** suggestDrop measures each filter's cost: the count with that one filter removed. */
async function suggestDrop(active) {
  const base = { ...state.filter };
  const counts = await Promise.all(active.map(async ([key]) => {
    const p = new URLSearchParams();
    for (const [k, v] of Object.entries(base)) if (v && k !== key) p.set(k, v);
    if (key !== 'range') writeRange(p);
    p.set('limit', '1');
    const res = await fetch('/api/requests?' + p.toString(),
      { headers: { accept: 'application/json' }, signal: state.ac ? state.ac.signal : undefined });
    if (!res.ok) return 0;
    return (await res.json()).total || 0;
  }));
  let best = null;
  active.forEach(([key, label, value], i) => {
    if (counts[i] > 0 && (!best || counts[i] > best.total)) best = { key, label, value, total: counts[i] };
  });
  return best;
}

/**
 * loadFacets fills the dropdowns, in TWO groups, and the second group is the point.
 *
 * /api/facets scopes its distinct-value lists by the whole active filter — including the
 * dimension being enumerated. So with agent=bob set, the agent list comes back as just
 * ["bob"], and the agent dropdown becomes a one-way door: the only way to look at codex
 * instead is to press Clear and start again. That is the other half of the reported bug,
 * and it is why this asks twice: once scoped (what has rows right now) and once with only
 * the tenant and the time range (what exists at all). Values in the first group are
 * offered plainly; the rest are offered under a group heading that says they currently
 * match nothing, so the list never CLAIMS a value has rows when it does not.
 *
 * The selected value is kept even when it has no rows left, marked as such: letting the
 * facets drop it left the control blank while state.filter still held it, so the filter
 * bar and the query disagreed about what was being filtered.
 *
 * ponytail: two requests per filter change. One would do if /api/facets enumerated each
 * dimension with its OWN value excluded — see the handoff; this is the UI-side fix.
 */
async function loadFacets() {
  // The universe: tenant scope and time range only. Tenant stays because it is a
  // permission boundary, not a view preference.
  const uni = new URLSearchParams();
  if (state.filter.tenant) uni.set('tenant', state.filter.tenant);
  writeRange(uni);
  try {
    const [scoped, all] = await Promise.all([
      api('facets'),
      fetch('/api/facets?' + uni.toString(), {
        headers: { accept: 'application/json' },
        signal: state.ac ? state.ac.signal : undefined,
      }).then((r) => (r.ok ? r.json() : {})),
    ]);
    for (const dim of FACET_DIMS) {
      const sel = $('#f-' + dim);
      while (sel.options.length > 1) sel.remove(1);
      // Modes are labelled in both vocabularies (see modeLabel): the recorded value is
      // what the filter sends, and the configured name is what the user set in Settings.
      const label = dim === 'mode' ? modeLabel : (v) => v;
      const here = scoped[dim] || [];
      for (const v of here) sel.appendChild(el('option', { value: v }, label(v)));
      const rest = (all[dim] || []).filter((v) => !here.includes(v));
      if (rest.length) {
        const grp = el('optgroup', { label: 'No rows under the current filters' });
        for (const v of rest) grp.appendChild(el('option', { value: v }, label(v)));
        sel.appendChild(grp);
      }
      syncControl(dim);
    }
  } catch (err) {
    if (!aborted(err)) { /* dropdowns degrade to "All" */ }
  }
}

// ── SSE ────────────────────────────────────────────────────────────────────
let lastEventID = 0;
function connectLive() {
  const src = new EventSource('/api/events' + (lastEventID ? '?last_event_id=' + lastEventID : ''));
  const label = $('#live-label'), box = $('.live');
  src.onopen = () => { box.className = 'live on'; label.textContent = 'live'; };
  src.onerror = () => { box.className = 'live off'; label.textContent = 'reconnecting…'; };
  src.addEventListener('request', (ev) => {
    let e;
    try { e = JSON.parse(ev.data); } catch (_) { return; }
    lastEventID = Math.max(lastEventID, e.id || 0);
    state.live.unshift(e);
    if (state.live.length > 60) state.live.length = 60;
    if (state.view === 'overview') renderLive();
  });
}

// ── boot ───────────────────────────────────────────────────────────────────
function initTheme() {
  const saved = localStorage.getItem('cg-theme');
  if (saved) document.documentElement.setAttribute('data-theme', saved);
  const btn = $('#theme');
  // The button SAYS which of the three states it is in. A single static glyph on a
  // three-way toggle means the only way to find out is to press it and watch.
  const label = () => {
    const cur = document.documentElement.getAttribute('data-theme') || 'auto';
    const face = cur === 'dark' ? 'Dark' : cur === 'light' ? 'Light' : 'Auto';
    btn.textContent = face;
    btn.setAttribute('aria-label', 'Colour theme: ' + face.toLowerCase() + '. Activate to change.');
    btn.title = btn.getAttribute('aria-label');
  };
  label();
  btn.addEventListener('click', () => {
    const cur = document.documentElement.getAttribute('data-theme');
    const next = cur === 'dark' ? 'light' : cur === 'light' ? 'auto' : 'dark';
    document.documentElement.setAttribute('data-theme', next);
    if (next === 'auto') localStorage.removeItem('cg-theme');
    else localStorage.setItem('cg-theme', next);
    label();
  });
}

function init() {
  initTheme();
  for (const t of $$('.tab')) t.addEventListener('click', () => go(t.dataset.view));
  // One control changes one filter. Nothing else in state.filter is touched, which is
  // what stops a filter with no control (session, tenant) from being wiped — or kept
  // invisibly — by a change to an unrelated dropdown.
  for (const [key, , ctl] of DIMS) {
    if (!ctl || ctl === '#f-q') continue;
    $(ctl).addEventListener('change', (ev) => setFilter(key, ev.currentTarget.value));
  }
  initRange();
  // Only the components table. Sessions and Requests are LIMIT 25 / LIMIT 50 server-side, so
  // a client-side sort there would sort ONE PAGE under a header that looks global — see
  // sortRows and docs/dashboard.md. They stay unsorted until ?sort=/?dir= reach the SQL.
  sortable('[data-testid=components-table]', COMPONENT_SORT);
  $('#f-dim').addEventListener('change', (ev) => { state.dim = ev.currentTarget.value; loadUsage(); });
  // Debounced, and Enter commits immediately rather than waiting out the delay. The
  // pending timer is dropped on submit so the same query is not sent twice.
  let deb;
  const searchNow = () => { clearTimeout(deb); setFilter('q', $('#f-q').value); };
  $('#f-q').addEventListener('input', () => { clearTimeout(deb); deb = setTimeout(searchNow, 250); });
  $('#f-q').addEventListener('keydown', (ev) => { if (ev.key === 'Enter') searchNow(); });
  $('#f-q').addEventListener('search', searchNow); // the input's own clear affordance
  $('#f-clear').addEventListener('click', clearFilters);
  $('#req-next').addEventListener('click', () => {
    if (!state.nextCursor) return;
    state.reqStack.push(state.reqCursor);
    state.reqCursor = state.nextCursor;
    loadRequests();
  });
  $('#req-prev').addEventListener('click', () => {
    state.reqCursor = state.reqStack.pop() || 0;
    loadRequests();
  });
  $('#sess-prev').addEventListener('click', () => { state.sessOffset = Math.max(0, state.sessOffset - 25); loadSessions(); });
  $('#sess-next').addEventListener('click', () => { state.sessOffset += 25; loadSessions(); });
  $('#bench-refresh').addEventListener('click', async () => {
    await fetch('/api/benchmarks?refresh=1');
    loadBenchmarks();
  });
  $('#drawer-close').addEventListener('click', closeDrawer);
  // Forward wrap: reaching the sentinel means the last real stop is behind us.
  $('#drawer-end').addEventListener('focus', () => { $('#drawer-close').focus(); });
  $('#scrim').addEventListener('click', closeDrawer);
  document.addEventListener('keydown', (ev) => { if (ev.key === 'Escape') closeDrawer(); });
  document.addEventListener('keydown', trapTab);

  // Back/Forward and a hand-edited hash both mean "make the page match the address
  // bar", and the address bar now carries every filter — so Back undoes a filter
  // change, not just a tab change. popstate covers the buttons, hashchange covers a
  // typed hash (pushState does not fire it).
  window.addEventListener('popstate', applyURL);
  window.addEventListener('hashchange', () => {
    if (location.pathname + location.hash !== urlFor()) applyURL();
  });

  initAccounts();

  renderLive(); // show the feed's empty state immediately, not only on the first event

  // Decide the world before loading anything. On a hosted deployment with no session,
  // every loader would otherwise fire a request that 401s and paint an error the user
  // cannot act on — the gate is the actionable thing to show instead.
  probeAccount().then((signedIn) => {
    if (account.hosted && !signedIn) return; // the gate is up; nothing to load
    // The whole state — view AND filters — comes from the URL, so a shared link opens
    // the view its author was looking at. An unknown view falls back inside go().
    applyURL();
    checkCapture();
    connectLive();
  });
  // Poll the aggregates: SSE carries individual rows, but a rollup must be
  // recomputed server-side, and 10 s is well under a human's patience.
  //
  // Both pollers stop while the gate is up. Without that check they kept firing behind
  // the login form — and after a sign-out — so a page left sitting on the gate produced
  // a 401 every ten seconds forever.
  const gated = () => !$('#gate').hidden;
  // A window whose `to` is absolute cannot gain rows, so repolling it is pure waste — and
  // on a wide manager scope it is a full-corpus aggregate every ten seconds for no new data.
  setInterval(() => {
    if (state.view === 'overview' && !gated() && state.to === 'now') loadOverview();
  }, 10000);
  setInterval(() => { if (!gated()) { loadFacets(); checkCapture(); } }, 30000);
}

document.addEventListener('DOMContentLoaded', init);

// ── accounts: the gate, setup, settings, tenants, archive ───────────────────
//
// This whole section is inert on a single-tenant proxy. /api/me only exists in hosted
// mode, so a 404 there is the signal to hide every account tab and behave exactly as
// the dashboard always has — the mode is DETECTED rather than configured into the page,
// because a build-time flag would be one more thing to keep in step with the server.

const account = {
  tenant: null, tokens: [], options: null, hosted: false, baseURL: '',
  // register is the operator's self-registration mode: closed | invite | open. Read
  // from /api/whoami, which answers it unauthenticated — which is the only moment it
  // matters. Absent (an older server) is treated as closed, matching the server's own
  // default: guessing "open" would draw a form that 403s on submit.
  register: 'closed',
};

/** ctl() calls a control-plane route. Throws with the server's message, which is
 *  written for the person reading it — passing it straight through beats inventing a
 *  generic one that says less. */
async function ctl(path, opts = {}) {
  const res = await fetch(path, {
    ...opts,
    headers: { 'content-type': 'application/json', ...(opts.headers || {}) },
  });
  let body = null;
  try { body = await res.json(); } catch { /* a 204 or a proxy error page */ }
  if (!res.ok) {
    const err = new Error((body && body.error) || `${res.status} ${res.statusText}`);
    err.status = res.status;
    throw err;
  }
  return body;
}

function gateError(msg) {
  const n = $('#gate-error');
  n.textContent = msg || '';
  n.hidden = !msg;
}

/**
 * applyRegisterMode makes the gate match what the server will actually accept.
 *
 * Three modes, three renderings, and the closed one is deliberately NOT a form:
 *
 *   closed — no form at all, just who to ask. Drawing an email box that answers 403 on
 *            submit wastes the user's time and reads as a broken deployment rather than
 *            a deliberate policy.
 *   invite — the form plus an invite-code field, because a code is checked.
 *   open   — the form, no code field, because no code is checked.
 *
 * The gate reflects the policy; it never decides it. Every one of these paths is still
 * enforced server-side, so a hand-edited page gains nothing.
 */
function applyRegisterMode() {
  const mode = account.register;
  const closed = mode === 'closed';
  const box = $('#gate-closed');
  const form = $('#gate-register');
  const registerTabSelected = $('#gate-tab-register').getAttribute('aria-selected') === 'true';

  $('#gate-invite-field').hidden = mode !== 'invite';
  // Only ever visible while the Register tab is the one showing.
  box.hidden = !(closed && registerTabSelected);
  if (closed && registerTabSelected) form.hidden = true;
  clear(box).appendChild(el('div', { class: 'state-body' },
    el('strong', { text: 'Registration is closed on this deployment' }),
    el('span', { text:
      'Accounts are created by whoever operates this proxy — ask them for one, then sign ' +
      'in above. Self-registration is normally open; this deployment has turned it off.' })));

  const hint = $('#gate-register-hint');
  if (hint) {
    hint.textContent = mode === 'invite'
      ? 'Registration is invite-only: you need the code from the operator.'
      : mode === 'open' ? 'Anyone with an allowed work email may register.' : '';
    hint.hidden = !hint.textContent;
  }
}

// ── two-phase verification ─────────────────────────────────────────────────
//
// Phase one (register or sign in) returns an absolute expiry; phase two posts the code.
// The countdown ticks against THAT timestamp rather than a five-minute timer this page
// starts, so a code that took forty seconds to arrive shows the time it really has left.
// The server is the only thing that enforces the deadline; this is just honest about it.

const verify = { email: '', expires: 0, tick: null };

/** Show the code form for an email, counting down to `expires` (epoch ms). */
function showVerify(email, expires, intro) {
  verify.email = email;
  verify.expires = expires || 0;
  hideGateForms();
  // The sign-in/register pair goes away for phase two: that choice is already made, and
  // leaving "Register" marked selected above a code box invited clicking it and losing the
  // code. "Start over" is the way back, and it restores them.
  $('.gate-tabs').hidden = true;
  $('#gate-verify').hidden = false;
  $('#gate-verify-intro').textContent = intro;
  $('#gate-verify-code').value = '';
  $('#gate-verify-code').focus();
  if (verify.tick) clearInterval(verify.tick);
  const paint = () => {
    // The remaining time is still (server expiry − now), never a countdown this page
    // started: a code that took forty seconds to arrive must show the time it really has.
    // Only the STYLING is derived from it — amber under a minute, red once gone.
    const left = Math.max(0, Math.round((verify.expires - Date.now()) / 1000));
    const timer = $('#gate-verify-timer');
    if (!verify.expires) { timer.textContent = ''; timer.className = ''; return; }
    timer.textContent = left > 0
      ? `Expires in ${Math.floor(left / 60)}:${String(left % 60).padStart(2, '0')}`
      : 'That code has expired — start over to get a new one.';
    timer.className = left <= 0 ? 'hint gone' : left <= 60 ? 'hint soon' : 'hint';
    if (left <= 0 && verify.tick) { clearInterval(verify.tick); verify.tick = null; }
  };
  paint();
  verify.tick = setInterval(paint, 1000);
}

/** Leave the code form, back to whichever tab is selected. */
function cancelVerify() {
  if (verify.tick) { clearInterval(verify.tick); verify.tick = null; }
  verify.email = '';
  $('#gate-verify').hidden = true;
  $('#gate-reset').hidden = true;
  $('#gate-reset-verify').hidden = true;
  $('.gate-tabs').hidden = false;
  const onRegister = $('#gate-tab-register').getAttribute('aria-selected') === 'true';
  $('#gate-signin').hidden = onRegister;
  $('#gate-register').hidden = !onRegister;
  if (onRegister) applyRegisterMode();
}

// ── password reset (the gate) ──────────────────────────────────────────────
//
// The flow for someone who cannot sign in, which is why it is on the gate and not behind
// it. Two steps: ask for a code, then spend it together with the new password. One step
// would mean holding a spent code while the user thinks of a password.
//
// The server answers phase one identically whether or not the address has an account, so
// this UI must promise only that a code was sent IF there was somewhere to send it. Saying
// "check your inbox" is true either way; saying "we found your account" would not be.

const reset = { email: '', expires: 0, tick: null };

/** hideGateForms hides every form the gate can show. One place, because two forms visible
 *  at once is the bug that appears when a fifth form is added to four ad-hoc toggles. */
function hideGateForms() {
  for (const id of ['#gate-signin', '#gate-register', '#gate-verify', '#gate-closed',
    '#gate-reset', '#gate-reset-verify']) {
    const n = $(id);
    if (n) n.hidden = true;
  }
}

/** gateNotice shows a non-error status on the gate — "your password is set, now sign in".
 *  Separate from gateError because a success rendered in the error box reads as a failure. */
function gateNotice(msg) {
  const n = $('#gate-notice');
  n.textContent = msg || '';
  n.hidden = !msg;
}

function showResetRequest() {
  cancelVerify();
  cancelReset();
  gateError('');
  gateNotice('');
  hideGateForms();
  $('.gate-tabs').hidden = true;
  $('#gate-reset').hidden = false;
  $('#gate-reset-email').value = $('#gate-signin-email').value.trim();
  $('#gate-reset-email').focus();
}

/** showResetVerify counts down against the server's absolute expiry, like the sign-in code
 *  form — never a five-minute timer this page started. */
function showResetVerify(email, expires) {
  reset.email = email;
  reset.expires = expires || 0;
  hideGateForms();
  $('.gate-tabs').hidden = true;
  $('#gate-reset-verify').hidden = false;
  $('#gate-reset-intro').textContent = 'If ' + email + ' has an account, a 6-digit code is '
    + 'on its way there. Enter it with the password you want.';
  $('#gate-reset-code').value = '';
  $('#gate-reset-password').value = '';
  $('#gate-reset-code').focus();
  if (reset.tick) clearInterval(reset.tick);
  const paint = () => {
    const left = Math.max(0, Math.round((reset.expires - Date.now()) / 1000));
    const timer = $('#gate-reset-timer');
    if (!reset.expires) { timer.textContent = ''; return; }
    timer.textContent = left > 0
      ? 'The code expires in ' + Math.floor(left / 60) + ':' + String(left % 60).padStart(2, '0')
      : 'That code has expired — start over to get a new one.';
    timer.className = left <= 0 ? 'hint gone' : left <= 60 ? 'hint soon' : 'hint';
    if (left <= 0 && reset.tick) { clearInterval(reset.tick); reset.tick = null; }
  };
  paint();
  reset.tick = setInterval(paint, 1000);
}

/** cancelReset abandons a pending reset and returns the gate to the sign-in form. */
function cancelReset(silent) {
  if (reset.tick) { clearInterval(reset.tick); reset.tick = null; }
  reset.email = '';
  if (silent) return;
  hideGateForms();
  $('.gate-tabs').hidden = false;
  $('#gate-signin').hidden = false;
  $('#gate-tab-signin').setAttribute('aria-selected', 'true');
  $('#gate-tab-register').setAttribute('aria-selected', 'false');
}

function initReset() {
  $('#gate-forgot').addEventListener('click', showResetRequest);
  $('#gate-reset-cancel').addEventListener('click', () => { gateError(''); cancelReset(); });
  $('#gate-reset-verify-cancel').addEventListener('click', () => { gateError(''); showResetRequest(); });

  $('#gate-reset').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    gateError('');
    const email = $('#gate-reset-email').value.trim();
    try {
      const out = await ctl('/api/password-reset', {
        method: 'POST', body: JSON.stringify({ email }),
      });
      showResetVerify(out.email || email, out.code_expires_at);
    } catch (e) { gateError(e.message); }
  });

  $('#gate-reset-verify').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    gateError('');
    try {
      await ctl('/api/password-reset/verify', {
        method: 'POST',
        body: JSON.stringify({
          email: reset.email,
          code: $('#gate-reset-code').value.trim(),
          new_password: $('#gate-reset-password').value,
        }),
      });
      // The password leaves the DOM the moment it has been spent.
      $('#gate-reset-password').value = '';
      cancelReset();
      // Deliberately NOT signed in: a reset proves the address, and signing in still wants
      // the password plus a fresh emailed code. One code must not buy two factors.
      gateNotice('Your password is set. Sign in with it — we will email a code as usual.');
      $('#gate-signin-email').value = reset.email || $('#gate-reset-email').value.trim();
      $('#gate-password').focus();
    } catch (e) {
      $('#gate-reset-password').value = '';
      gateError(e.message);
    }
  });
}

function showGate(show) {
  $('#gate').hidden = !show;
  // Hide the dashboard chrome as well, not just the views: a filter bar over a login
  // form invites clicking things that will 401 — and the TABS did exactly that, each
  // click firing a data fetch that answered 401 and logged a console error.
  $('#main').hidden = show;
  for (const sel of ['.filters', '.tabs', '.live']) {
    const n = $(sel);
    if (n) n.hidden = show;
  }
}

/** Reflect who is signed in, and which tabs that entitles them to. */
function applyAccount() {
  const t = account.tenant;
  $('#whoami').hidden = !t;
  $('#signout').hidden = !t;
  if (t) {
    $('#whoami').textContent = t.label ? `${t.email} · ${t.label}` : t.email;
    $('#whoami').title = t.role === 'manager' ? 'Manager' : 'User';
  }
  for (const el of $$('[data-account]')) el.hidden = !account.hosted || !t;
  // data-manager is "hosted managers only". data-local-ok marks the ones that are also
  // fine on a single-tenant proxy, where there is no principal and nothing to scope:
  // /api/benchmarks is manager-gated in hosted mode but open locally, and hiding the tab
  // there would break the local dev path.
  for (const el of $$('[data-manager]')) {
    el.hidden = account.hosted ? !(t && t.role === 'manager') : !el.hasAttribute('data-local-ok');
  }
  loadTenantOptions();
}
function isManager() { return !!(account.tenant && account.tenant.role === 'manager'); }

/**
 * loadTenantOptions fills the manager's scope select from the roster. Once per session: the
 * roster changes when an account is created, which is a page the manager is already on.
 *
 * The first two options are in the markup because they are not accounts: '' is the server's
 * own default (the whole service) and 'me' is the way back to own-only. A failure here leaves
 * those two, which is a usable control, so it is not reported.
 */
async function loadTenantOptions() {
  const sel = $('#f-tenant');
  if (!sel || !isManager() || sel.dataset.filled) return;
  sel.dataset.filled = '1';
  try {
    for (const t of (await ctl('/api/tenants')).tenants || []) {
      sel.appendChild(el('option', { value: t.id }, t.label ? t.email + ' · ' + t.label : t.email));
    }
    syncControl('tenant');
  } catch (_) { /* All accounts / Mine still work */ }
}

/**
 * probeAccount decides which of the three worlds this page is in: a single-tenant proxy,
 * a hosted one with nobody signed in, or a hosted one with a session.
 *
 * It asks /api/whoami, which answers 200 in every case — including "not signed in".
 * Probing by calling /api/me and reading its 401 also worked, and it put a red error in
 * the console of every user on every first load; a question with a legitimate negative
 * answer should not be asked with an error. whoami also returns the account and tokens
 * when signed in, so the probe and the first data fetch are one round trip.
 */
async function probeAccount() {
  try {
    const who = await ctl('/api/whoami');
    account.hosted = !!who.hosted;
    account.tenant = who.authenticated ? who.tenant : null;
    account.tokens = who.tokens || [];
    account.baseURL = who.base_url || location.origin;
    account.register = who.register || 'closed';
    applyRegisterMode();
    showGate(account.hosted && !account.tenant);
  } catch {
    // whoami is mounted whenever the dashboard is. Failing to reach it means the
    // dashboard itself is unreachable, so treat it as single-tenant and let the
    // individual views report their own errors rather than showing a login form the
    // user cannot complete.
    account.hosted = false;
    account.tenant = null;
    showGate(false);
  }
  applyAccount();
  return account.hosted && !!account.tenant;
}

async function loadOptions() {
  if (account.options || !account.hosted) return account.options;
  try { account.options = await ctl('/api/options'); } catch { account.options = null; }
  return account.options;
}

// ── setup ──────────────────────────────────────────────────────────────────
// TOKEN_SLOT is what stands in for a real token in every block on this page when we do
// not have the plaintext. A NAMED slot rather than the account's real prefix plus an
// ellipsis: these blocks exist to be pasted, and a credential fragment in one produces a
// line that silently cannot work.
const TOKEN_SLOT = 'cg_live_YOUR_TOKEN_HERE';

/** claudeSettings is the WHOLE env block a Claude Code user ends up with — not a diff.
 *  A beginner cannot apply a diff, and the two keys are the entire change. */
function claudeSettings(base, tok) {
  return [
    '{',
    '  "env": {',
    `    "ANTHROPIC_BASE_URL": "${base}/anthropic",`,
    `    "ANTHROPIC_CUSTOM_HEADERS": "x-context-guru-token: ${tok}"`,
    '  }',
    '}',
  ];
}

// AGENTS covers the agents that are NOT Claude Code — those two are still shell exports,
// because neither reads ~/.claude/settings.json. Claude Code has the numbered walkthrough
// above them instead.
const AGENTS = [
  {
    name: 'Bob (BobShell)',
    path: '/',
    // Bob's client builds every request header itself and offers no hook for another
    // one, so it cannot carry the token. Instead it is recognised by the sha256 of the
    // key it already sends — bound once on the Settings tab, never stored in plaintext.
    //
    // The variable name is version-dependent, and getting it wrong is silent: Bob simply
    // talks to its default gateway and nothing appears here. bobshell 2.x reads
    // BOB_GATEWAY_URL (checked against the 2.0.1 bundle, where CUSTOM_BASE_URL does not
    // appear at all); the older build read CUSTOM_BASE_URL. Both are listed, because a
    // spare export costs nothing and a missing one costs an afternoon.
    lines: (base, tok) => [
      `export BOB_GATEWAY_URL=${base}    # bobshell 2.x — check with: bob --version`,
      `export CUSTOM_BASE_URL=${base}    # older builds read this one instead`,
      '# Your Bob key stays your own (BOB_API_KEY; BOBSHELL_API_KEY still works).',
      '# Bob can send no header of ours, so bind that key once on the Settings tab:',
      '#   Settings → Bound agent keys → paste the key → Bind this key',
    ],
  },
  {
    name: 'OpenAI-dialect tools',
    path: '/openai/v1',
    lines: (base, tok) => [
      `export OPENAI_BASE_URL=${base}/openai/v1`,
      '# OPENAI_API_KEY stays your own provider key; send the token as a header:',
      `#   x-context-guru-token: ${tok}`,
    ],
  },
];

function copyButton(text) {
  return el('button', {
    class: 'ghost small', 'data-testid': 'copy',
    onclick: async (ev) => {
      const b = ev.currentTarget;
      try {
        await navigator.clipboard.writeText(text);
        b.textContent = 'copied';
      } catch {
        // Clipboard access can be refused (insecure origin, permissions). Say so
        // rather than appearing to succeed — the user needs to know to select manually.
        b.textContent = 'select manually';
      }
      setTimeout(() => { b.textContent = 'copy'; }, 1500);
    },
  }, 'copy');
}

// ── logs: Grafana over an SSH tunnel ───────────────────────────────────────
//
// Grafana and Loki bind LOOPBACK on the box, deliberately — the public path serves the
// proxy and this dashboard, nothing else. So the affordance here is copyable TEXT and
// never a hyperlink: an <a href> to 127.0.0.1:3000 is dead for every reader who has not
// already opened a tunnel, and a dead link is worse than instructions. Nothing is
// embedded either: an iframe would mean loosening a CSP that is worth more than the
// convenience.
//
// The address is written WITHOUT a scheme, on purpose as well as for brevity: the offline
// guarantee is enforced by a test that greps every served asset for URL schemes, and an
// asset that must reference no external origin should not start carrying URLs at all.
const GRAFANA_HOSTPORT = '127.0.0.1:3000';
const GRAFANA_LOGS_PATH = '/d/context-guru-logs/context-guru-logs';

/** copyBlock is a titled, copyable code block. Nothing new: it reuses the Setup tab's
 *  copy button and its <pre class="code">. */
function copyBlock(title, lines, testid) {
  const text = lines.join('\n');
  return el('div', { class: 'copyblock' },
    el('div', { class: 'setup-head' },
      el('h3', { text: title }),
      copyButton(text)),
    el('pre', { class: 'code', 'data-testid': testid }, text));
}

/** renderLogsHelp fills the Config tab's Logs panel: the tunnel, the address, and one
 *  LogQL query to paste into it. */
function renderLogsHelp() {
  const host = $('#logs-help');
  if (!host) return;
  clear(host);
  host.appendChild(copyBlock('1 · Open the tunnel from your machine',
    ['ssh -L 3000:' + GRAFANA_HOSTPORT + ' ' + (location.hostname || '<the host>')], 'logs-tunnel'));
  host.appendChild(copyBlock('2 · Open this address in a browser',
    [GRAFANA_HOSTPORT + GRAFANA_LOGS_PATH], 'logs-address'));
  host.appendChild(copyBlock('3 · Or query Loki directly, in Explore',
    ['{job="context-guru"} | json | level=~"WARN|ERROR"'], 'logs-query'));
  host.appendChild(el('p', { class: 'note', text:
    'Grafana and Loki bind loopback on the box, so this is text rather than a link — a ' +
    'link would be dead until the tunnel is up.' }));
}

/** logQueryBlock is the drawer's version: the LogQL that selects THIS session's lines. */
function logQueryBlock(session, tenant) {
  const sel = '{job="context-guru"' + (tenant ? ', tenant="' + tenant + '"' : '') + '}';
  return el('div', {},
    copyBlock('Logs for this session (Grafana → Explore → Loki)',
      [sel + ' | json | session="' + session + '"'], 'logs-session-query'));
}

/** step is one numbered instruction: the number, the sentence, and whatever it needs
 *  pasted underneath. Short on purpose — a step that needs a paragraph is two steps. */
function step(n, title, ...body) {
  return el('div', { class: 'setup-step', 'data-testid': 'setup-step-' + n },
    el('div', { class: 'setup-step-n' }, String(n)),
    el('div', { class: 'setup-step-body' }, el('h3', {}, title), ...body));
}

/** revealToken is the one-time reveal: the plaintext, big, with a copy button and a
 *  warning that cannot be scrolled past. Rendered only when we actually hold the
 *  plaintext, which is only ever the reply to registration or to minting. */
function revealToken(tok) {
  return el('div', { class: 'token-reveal', 'data-testid': 'token-reveal' },
    el('div', { class: 'setup-head' },
      el('h3', {}, 'Your context-guru token'),
      copyButton(tok)),
    el('pre', { class: 'code token-plain', 'data-testid': 'token-plain' }, tok),
    el('p', { class: 'warn-text', 'data-testid': 'token-once' },
      'Shown once. We store only its hash, so nobody — including us — can show it again. '
      + 'Copy it somewhere safe now; if you lose it, mint a new one on Settings.'));
}

function loadSetup() {
  const host = clear($('#setup-blocks'));
  const base = account.baseURL || location.origin;
  // The plaintext only exists at mint time, so a returning user gets the named slot.
  const tok = account.freshToken || TOKEN_SLOT;
  const settings = claudeSettings(base, tok);

  if (account.freshToken) host.appendChild(revealToken(account.freshToken));

  host.appendChild(el('div', { class: 'setup-steps' },
    step(1, 'Open your Claude Code settings file',
      el('pre', { class: 'code' }, '~/.claude/settings.json'),
      el('p', { class: 'hint' }, 'No such file? Create it — an empty file is fine.')),
    step(2, 'Put this in it',
      el('div', { class: 'setup-head' }, el('span', { class: 'hint' },
        'Your token is already filled in.'), copyButton(settings.join('\n'))),
      el('pre', { class: 'code', 'data-testid': 'setup-claude' }, settings.join('\n')),
      el('p', { class: 'hint' },
        'Already have an "env" block? Add just those two lines inside it. Leave every '
        + 'other key alone.')),
    step(3, 'Keep your own key where it is',
      el('p', { class: 'hint' },
        'ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN stays yours: we forward it, so your '
        + 'traffic is billed to you. Without it every request answers 401.')),
    step(4, 'Restart Claude Code, then check this dashboard',
      el('p', { class: 'hint' },
        'Ask it anything. Requests appear on Overview within a second.'))));

  host.appendChild(el('div', { class: 'banner warn', 'data-testid': 'setup-trap' },
    el('div', {}, el('strong', {}, 'Empty dashboard after exporting the variable? '),
      'An "env" block in ~/.claude/settings.json silently overrides an exported '
      + 'ANTHROPIC_BASE_URL. Claude Code answers normally and nothing reaches us. '
      + 'Put the block in that file — step 2 — rather than in your shell.')));

  const others = el('details', { class: 'setup-others' },
    el('summary', {}, 'Other agents (Bob, OpenAI-dialect tools)'));
  for (const a of AGENTS) {
    const lines = a.lines(base, tok);
    others.appendChild(el('div', { class: 'setup-block' },
      el('div', { class: 'setup-head' },
        el('h3', { text: a.name }),
        copyButton(lines.join('\n'))),
      el('pre', { class: 'code' }, lines.join('\n'))));
  }
  host.appendChild(others);

  // The banner above the blocks stays for the returning user's benefit — it is the only
  // thing on the page that explains why step 2 shows a placeholder instead of a token.
  const banner = $('#setup-token-banner');
  banner.hidden = !!account.freshToken;
  if (!account.freshToken) {
    banner.className = 'banner';
    banner.hidden = false;
    banner.textContent = 'Paste your own token over ' + TOKEN_SLOT + ' below. Tokens are '
      + 'shown once, at creation; mint a new one on Settings if you no longer have it.';
  }
}

// ── settings ───────────────────────────────────────────────────────────────
function componentPickers(pipeline, opts) {
  // The pipeline comes from the server as a resolved list of names (tenant.effective_config).
  // It used to be scraped out of the YAML text with a regex here, which only understood the
  // flow style and read an empty pipeline for a `preset:` account — so the grid showed
  // nothing running on the very configurations that ran the most.
  return { active: new Set(pipeline || []), all: (opts && opts.components) || [] };
}

function loadSettings() {
  const host = clear($('#settings-form'));
  const t = account.tenant;
  if (!t) { emptyState(host, 'Not signed in', 'Sign in to manage your configuration.'); return; }

  loadOptions().then((opts) => {
    clear(host);
    // Two states, and the page must not blur them. A tenant who has stored nothing
    // FOLLOWS the server default: config_yaml is empty, and drawing that would be a
    // blank form reading as "my configuration is gone". So the controls always show the
    // EFFECTIVE document — what the proxy actually runs — and when it is inherited they
    // are read-only and labelled as such, because it is not this tenant's choice yet.
    const inherited = !!t.config_inherited;
    const cfg = t.effective_config || {};
    const { active, all } = componentPickers(cfg.pipeline, opts);
    // The descriptors and the recommended prefill, both from /api/options. Nothing about a
    // field — its name, type, default, enum options, min or hint — is written in this file.
    const compFields = (opts && opts.component_fields) || {};
    const recommended = (opts && opts.recommended) || {};
    // Seeded with ONLY the keys the stored document states. See cfgState — an absent key is
    // not a zero. Secrets are dropped on the way in as well as on the way out: the server
    // does not read one back (config.readBlocks skips them), and if some future payload did,
    // holding it here would put a credential in a form's state and post it straight back.
    cfgState = {};
    for (const [cname, vals] of Object.entries(cfg.components || {})) {
      const secret = new Set((compFields[cname] || []).filter((fd) => fd.secret).map((fd) => fd.key));
      cfgState[cname] = Object.fromEntries(Object.entries(vals).filter(([k]) => !secret.has(k)));
    }
    // A best-effort read of a document the server could not fully load is drawn but never
    // editable: posting it back would write the fallback's guess over the real thing. The
    // server refuses that too (409), which is the backstop rather than the only guard.
    const cfgDisabled = inherited || !!cfg.parse_error;

    // One <details> per component that HAS knobs, drawn from the descriptors. Redrawn in
    // place when a component is enabled or disabled, or when the recommended values are
    // taken — cfgState survives a redraw, which is what makes the round trip work.
    const compHost = el('div', { 'data-testid': 'comp-fields' });
    const enabledNow = (cname) => {
      const cb = $('#comp-' + cname);
      return cb ? cb.checked : active.has(cname);
    };
    const drawComps = () => {
      clear(compHost);
      for (const cname of all) {
        const fields = compFields[cname] || [];
        if (!fields.length) continue; // takes no configuration (cachesplit)
        if (!cfgState[cname]) cfgState[cname] = {};
        const off = !enabledNow(cname);
        compHost.appendChild(renderComponentFields(cname, fields, cfgState[cname],
          cfgDisabled || off,
          { recommended: recommended[cname], redraw: drawComps, opts, off }));
      }
    };
    // Still the document itself, for the two things that are ABOUT the document rather than
    // about its contents: Customise stores a byte-identical copy of the default (comments and
    // all), and "identical to the current default" is a text comparison.
    const effective = t.effective_config_yaml || t.config_yaml || '';

    // Spend, first: it is what a shared box gets asked about most. Reported only —
    // your traffic runs on YOUR provider credential, so there is nothing to cap.
    host.appendChild(el('div', { class: 'spend' },
      el('div', { class: 'spend-label' }, `Spend this month: ${usd(t.spent_usd)}`),
      el('p', { class: 'hint' }, 'Billed to your own provider account, not to us.')));

    // Agent keys. Only relevant to agents that cannot send x-context-guru-token.
    //
    // The paste field exists because the alternative was a curl line carrying the
    // cg_dash cookie, and every part of that went wrong in practice: the cookie is not
    // displayed anywhere, so it meant a devtools detour, and the natural guess — pasting
    // the cg_live_ token into the cookie — fails with "no context-guru token", which
    // names a header this route does not even read. The browser already holds the
    // cookie. So the key is pasted HERE and sent in the Authorization slot by the same
    // fetch, which is the only step the person could not do for themselves.
    const keyIn = el('input', {
      type: 'password', id: 'agent-key', autocomplete: 'off', spellcheck: 'false',
      placeholder: 'paste your Bob API key', 'data-testid': 'agent-key-input',
    });
    const keyMsg = el('p', { class: 'hint', role: 'status', 'data-testid': 'agent-key-status' });
    host.appendChild(el('div', { class: 'field' },
      el('label', { for: 'agent-key' }, 'Bound agent keys'),
      el('div', { 'data-testid': 'agent-keys' },
        t.agent_keys > 0
          ? `${t.agent_keys} provider key${t.agent_keys === 1 ? '' : 's'} bound to this account.`
          : 'None bound.'),
      whyBlock('Why an agent needs one',
        'For agents that cannot send a custom header (Bob/BobShell): the proxy recognises ' +
        'them by the sha256 of the provider key they already send. Only the digest is ' +
        'stored — never the key, and it is not sent on anywhere. Keys under 20 characters ' +
        'are refused — the digest is the identity, so a short key would be a guessable ' +
        'account. A key already bound to another account is refused too, never moved: ' +
        'its owner unbinds it first.'),
      keyIn,
      el('div', { class: 'actions' }, el('button', {
        class: 'primary small', 'data-testid': 'agent-key-bind',
        onclick: async () => {
          const key = keyIn.value.trim();
          keyMsg.className = 'hint';
          if (!key) { keyMsg.textContent = 'Paste the key your agent sends first.'; return; }
          keyMsg.textContent = 'binding…';
          try {
            await ctl('/api/me/agent-key', {
              method: 'POST', headers: { authorization: `Bearer ${key}` },
            });
            // Cleared on success, so the key does not sit in a form field afterwards.
            keyIn.value = '';
            keyMsg.className = 'hint ok';
            keyMsg.textContent = 'Bound. Your agent is recognised by this key from now on.';
            await probeAccount();
            loadSettings();
          } catch (e) {
            keyMsg.className = 'hint warn-text';
            keyMsg.textContent = e.message;
          }
        },
      }, 'Bind this key')),
      keyMsg,
      t.agent_keys > 0
        ? el('button', {
          class: 'ghost small', 'data-testid': 'agent-keys-clear',
          onclick: async () => {
            if (!confirm('Unbind every provider key? Agents that rely on key ' +
              'recognition stop being identified until you bind again.')) return;
            try { await ctl('/api/me/agent-key', { method: 'DELETE' }); await probeAccount(); loadSettings(); } catch (e) { alert(e.message); }
          },
        }, 'Unbind all')
        : null));

    // Who may shape the compaction itself. A plain account keeps its own settings — the
    // upstreams it sends to, its capture consent, its tokens — but the pipeline is the
    // manager's to set, on this page and in PUT /api/me. Drawing a component grid that
    // the server answers 403 to would be a form that lies.
    const mgr = isManager();
    if (!mgr) {
      host.appendChild(el('div', { class: 'cfg-state', 'data-testid': 'cfg-state-managed' },
        el('div', {},
          el('strong', {}, 'Your manager sets the compaction.'),
          ' Ask them for a change; everything else on this page is yours.')));
    }

    // Which configuration is in force, and how to change that.
    if (mgr) host.appendChild(inherited
      ? el('div', { class: 'cfg-state', 'data-testid': 'cfg-state-inherited' },
        el('div', {},
          el('strong', {}, 'Following the server default.'),
          ' The pipeline below is what your traffic runs now. It changes when the ' +
          'operator changes the default.'),
        el('button', {
          class: 'ghost small', 'data-testid': 'cfg-customise',
          onclick: () => setStoredConfig(effective),
        }, 'Customise'))
      : el('div', { class: 'cfg-state', 'data-testid': 'cfg-state-own' },
        el('div', {},
          el('strong', {}, 'Using your own configuration.'),
          ' Changes to the server default do not reach you.',
          // Worth saying: this is the state the old registration bug left every account
          // in, and a saved copy of the default is the one case where following it
          // costs nothing.
          (opts && opts.default_config && effective.trim() === opts.default_config.trim())
            ? ' It is identical to the current default.' : ''),
        el('button', {
          class: 'ghost small', 'data-testid': 'cfg-follow-default',
          onclick: () => {
            if (!confirm('Discard your configuration and follow the server default? '
              + 'Recorded in the audit log; you can customise again at any time.')) return;
            setStoredConfig('');
          },
        }, 'Follow the server default')));

    // Mode.
    // The option text names the value a captured request will SHOW, because these are the
    // same dimension under two names: the config says sync, the request rows say active.
    const modeSel = el('select', { id: 'set-mode', 'data-testid': 'set-mode' },
      el('option', { value: 'sync' }, 'sync — compaction is applied (requests show Mode "active")'),
      el('option', { value: 'observe' }, 'observe — measure only, requests untouched (Mode "observe")'));
    modeSel.value = cfg.mode === 'observe' ? 'observe' : 'sync';
    modeSel.disabled = inherited;
    if (mgr) {
      host.appendChild(el('div', { class: 'field' },
        el('label', { for: 'set-mode' }, 'Mode'), modeSel,
        el('p', { class: 'hint' },
          'observe is the safe way to try a configuration: nothing is rewritten.')));
    }

    // Upstreams, one per dialect, from the operator's allow-list.
    const ups = (opts && opts.upstreams) || [];
    for (const [key, label] of [['up_anthropic', 'Anthropic-dialect upstream'],
      ['up_openai', 'OpenAI-dialect upstream'], ['up_bob', 'Bob upstream']]) {
      const sel = el('select', { id: 'set-' + key, 'data-testid': 'set-' + key },
        el('option', { value: '' }, '— none —'),
        ...ups.map((u) => el('option', { value: u.name }, `${u.name} (${u.dialect})`)));
      sel.value = t[key] || '';
      host.appendChild(el('div', { class: 'field' }, el('label', { for: 'set-' + key }, label), sel));
    }

    // Components.
    // The id is load-bearing: saveSettings reads the checkboxes back through
    // '#comp-grid input', and without it every save wrote `pipeline: []`.
    const grid = el('div', { id: 'comp-grid', class: 'comp-grid', 'data-testid': 'comp-grid' });
    for (const name of all) {
      const id = 'comp-' + name;
      const cb = el('input', { type: 'checkbox', id, 'data-comp': name, 'data-testid': id });
      cb.checked = active.has(name);
      cb.disabled = cfgDisabled;
      // Enablement is pipeline membership, so this checkbox is what makes a component's
      // fields editable — and unticking it is what clears them on save.
      cb.addEventListener('change', () => drawComps());
      const warn = name === 'extract_llm'
        ? ' — calls a compaction model on the request path (+117ms typical, up to ~945ms on file reads) and bills to the shared credential'
        : '';
      grid.appendChild(el('label', { class: 'comp', for: id }, cb,
        el('span', { class: 'comp-name' }, name),
        warn ? el('span', { class: 'comp-warn' }, warn) : null));
    }
    if (mgr) host.appendChild(el('div', { class: 'field' },
      el('label', {}, 'Pipeline components'), grid,
      el('p', { class: 'hint' }, 'What runs, in the order shown.'),
      whyBlock('What saving changes',
        'A newly enabled component is appended at the end of the pipeline. Ticking one here ' +
        'is what makes its fields below editable, and unticking one CLEARS the keys it has ' +
        'in your document — a block is configuration, not enablement, so leaving a block ' +
        'behind for a component that does not run is the state nobody can read back. ' +
        'Saving rebuilds your pipeline and discards frozen compaction decisions, so the ' +
        'next turn will not be cache-warm.')));

    // Idle keep-alive consent. Its own consent control, beside the transcript one, for the
    // same reason: it is a thing done with the user's property that they have to agree to.
    // Here it is their MONEY rather than their code, and the copy says what it buys, what it
    // costs, and where to see both — a mechanism that spends on its own initiative is only
    // acceptable if the person paying can read the ledger.
    const ka = el('input', { type: 'checkbox', id: 'set-keepalive', 'data-testid': 'set-keepalive' });
    ka.checked = !!(cfg.cache && cfg.cache.keepalive);
    // Editable under the same condition as the rest of the configuration document, because
    // that is where it is STORED: PUT /api/me answers 403 to a non-manager sending `config`,
    // and drawing an enabled box whose value the server discards is worse than not drawing
    // it. The ledger on Overview is visible to everyone regardless, which is the half that
    // matters for someone whose key is being spent.
    ka.disabled = inherited || !mgr;
    if (mgr) host.appendChild(el('div', { class: 'field' },
      el('label', { class: 'comp', for: 'set-keepalive' }, ka,
        el('span', { class: 'comp-name' },
          'Keep my prompt cache warm while I am away')),
      el('p', { class: 'hint' },
        'Spends a small amount to avoid a much larger cache-recreation charge. After '
        + (((cfg.cache && cfg.cache.keepalive_idle_seconds) || 280)) + 's idle, up to '
        + (((cfg.cache && cfg.cache.keepalive_max_pings) || 2)) + ' minimal requests re-read '
        + 'your cached prompt so the provider refreshes its 5-minute lifetime for free. '
        + 'Only on sessions with a large enough cached prompt to be worth it, and never on a '
        + 'session\u2019s first request. Billed to your own key.'),
      whyBlock('What it costs and what it saves',
        'A cache read costs 0.1x base input; re-creating a lapsed prefix costs 1.25x, so one '
        + 'ping buys back about 11.5 of itself. On this service\u2019s traffic, requests that '
        + 'resumed after the 5-minute window cost 8.5x a request that hit, and they were 23.6% '
        + 'of all spend. It is off by default because it is your money and nobody asked for it '
        + 'to be spent. Every ping is a row on your Requests tab marked keep-alive with its own '
        + 'cost, and the Overview ledger shows pings, ping cost, misses avoided and the net. '
        + 'Be aware of the shape: it is a small tax on most of the sessions it touches, funding '
        + 'a large rebate on a few. Measured over 5 days of this service\u2019s traffic, 34 of '
        + '119 pinged sessions came out ahead and the worst single session paid $2.42 for '
        + 'nothing. Watch your own ledger and switch it off if you are not one of the winners.')));

    // Content capture consent.
    const cap = el('input', {
      type: 'checkbox', id: 'set-capture', 'data-testid': 'set-capture',
    });
    cap.checked = !!t.capture_content;
    host.appendChild(el('div', { class: 'field' },
      el('label', { class: 'comp', for: 'set-capture' }, cap,
        el('span', { class: 'comp-name' }, 'Store my transcripts for the diff view')),
      el('p', { class: 'hint warn-text' },
        'Writes your agent output to disk. The manager can read what is stored. The ' +
        'redactor is best-effort, not a guarantee.'),
      whyBlock('What "best-effort" means here',
        'Source code and tool results are stored behind a redactor whose own review found ' +
        '11 of 22 realistic credential shapes passing through it. The manager can read ' +
        'whatever this stores. Off by default.')));

    // A stored document the server could not fully load. The fields below are a best-effort
    // read of it, and saving from them would post whatever that read happened to see — over
    // an already-broken document, on a page with no YAML box. So say so, and let the server's
    // 409 be the backstop rather than the only guard.
    if (mgr && cfg.parse_error) {
      host.appendChild(el('div', { class: 'state blocked', 'data-testid': 'cfg-unreadable' },
        el('div', { class: 'state-body' },
          el('strong', {}, 'Your stored configuration does not load.'),
          el('span', {}, cfg.parse_error),
          el('span', {}, 'The controls below are a guess at it and saving them is refused. '
            + 'A manager can repair the document on this account\u2019s page under Accounts.'))));
    }

    // Every component's configuration, one <details> each, drawn from /api/options.
    //
    // There is no YAML editor here any more. It was not a convenience, it was the failure:
    // the page rewrote the document with regular expressions, which corrupted any config
    // whose pipeline was written as a block sequence and produced "did not find expected
    // key" on every save, with no way out from the UI. Fields post fields; the server owns
    // the document.
    //
    // And the fields themselves are not written here either. The hand-written version of
    // this form covered 18 keys of 97 and one component of fourteen, and every field on it
    // was a second copy of a fact the server already stated — including a strategy list the
    // engine had outgrown, which silently rewrote a stored value. So the descriptors decide
    // what is drawn, and a knob added to a component appears here with no change to this
    // file.
    if (mgr) {
      host.appendChild(el('div', { class: 'field' },
        el('label', {}, 'Component configuration'),
        el('p', { class: 'hint' },
          'Every key each component reads, from the server\u2019s own declarations. A field '
          + 'left empty is UNSET: the key is removed from your document and the component\u2019s '
          + 'default decides, which is not the same thing as writing that default down.'),
        whyBlock('What happens when you save',
          'These fields are applied to your configuration on the server with a YAML library '
          + 'and the result is built once before it is stored, so a value that would not work '
          + 'is a refusal naming the field rather than a surprise on your next turn. A key you '
          + 'leave empty is REMOVED from the document and the component\u2019s own default '
          + 'takes over. Saving rebuilds your pipeline and discards frozen compaction '
          + 'decisions, so the next turn will not be cache-warm.')));
      host.appendChild(compHost);
      drawComps();
    }

    // The whole document, read-only. The fields above are the knobs worth turning from a
    // page; they are not every key a configuration can hold, and an operator asking "what
    // am I actually running" deserves the answer rather than an inference from a form. Not
    // editable: a textarea here is the regex-rewriting save path this page was rebuilt to
    // remove. Anything the fields do not cover is a manager edit on the account page.
    if (effective) {
      host.appendChild(el('details', { class: 'field', 'data-testid': 'full-config' },
        el('summary', {}, 'Full configuration (read-only)'),
        el('p', { class: 'hint' },
          inherited
            ? 'The server default, which your traffic follows because you have stored no '
              + 'configuration of your own.'
            : 'Your stored document, exactly as the proxy builds it. The fields above write '
              + 'into it; every other key here was set by a manager and is left untouched.'),
        el('pre', { class: 'code', 'data-testid': 'full-config-yaml' }, effective)));
    }

    // Save covers the upstreams and the capture consent in both states; it leaves the
    // configuration alone while it is inherited (see saveSettings), so saving one of
    // those does not quietly turn a tracking account into a frozen copy.
    host.appendChild(el('div', { class: 'actions' },
      el('button', { class: 'primary', 'data-testid': 'settings-save', onclick: saveSettings }, 'Save')));
  });

  loadTokens();
  loadPassword();
  loadMachines();
  loadAudit();
}

/**
 * The password card. Two shapes, because two different things are true:
 *
 *   has_password  — a change form, and the CURRENT password is required. A stolen session
 *                   cookie must not be enough to take the account over and lock its owner
 *                   out of a credential they still know.
 *   no password   — an account older than passwords. There is nothing to check a new one
 *                   against, so the honest answer is the emailed reset, not a form with an
 *                   "old password" box it cannot fill.
 */
function loadPassword() {
  const host = clear($('#password-form'));
  const t = account.tenant;
  if (!t) return;
  const status = $('#password-status');
  status.textContent = '';
  if (!t.has_password) {
    emptyState(host, 'No password on this account',
      'It was created before passwords existed and signs in with a token. Use “Forgot your '
      + 'password?” on the sign-in page to set one by email — that proves the address, '
      + 'which is the only thing there is to prove here.');
    return;
  }
  const old = el('input', {
    type: 'password', id: 'pw-old', autocomplete: 'current-password', 'data-testid': 'pw-old',
  });
  const next = el('input', {
    type: 'password', id: 'pw-new', autocomplete: 'new-password', minlength: '8',
    'data-testid': 'pw-new',
  });
  const err = el('p', { class: 'hint warn-text', role: 'alert', 'data-testid': 'pw-error' });
  host.appendChild(el('div', { class: 'field' },
    el('label', { for: 'pw-old' }, 'Current password'), old));
  host.appendChild(el('div', { class: 'field' },
    el('label', { for: 'pw-new' }, 'New password'), next,
    el('p', { class: 'hint' }, 'At least 8 characters. Your other signed-in machines are '
      + 'signed out; this browser stays in.')));
  host.appendChild(err);
  host.appendChild(el('div', { class: 'actions' },
    el('button', {
      class: 'primary', 'data-testid': 'pw-save',
      onclick: async () => {
        err.textContent = '';
        status.textContent = 'saving…';
        try {
          const out = await ctl('/api/me/password', {
            method: 'POST',
            body: JSON.stringify({ old_password: old.value, new_password: next.value }),
          });
          status.textContent = 'changed';
          err.className = 'hint ok';
          err.textContent = out.note || '';
          loadMachines(); // the other machines are gone; the list must say so
        } catch (e) {
          status.textContent = '';
          err.className = 'hint warn-text';
          err.textContent = e.message;
        } finally {
          // Neither value stays in the DOM after being spent, whatever happened.
          old.value = '';
          next.value = '';
        }
      },
    }, 'Change password')));
}

/**
 * The machines this account is signed in on, each revocable on its own.
 *
 * Named loadMachines, not loadSessions: there was already a top-level loadSessions (the
 * Sessions TAB), and two function declarations with one name means the later one wins for
 * every caller — so `loaders.sessions` and both pager buttons were calling this, painting
 * "Could not list your sessions" into the Settings card and leaving the Sessions table on
 * its loading skeleton forever.
 */
async function loadMachines() {
  const host = clear($('#session-list'));
  let rows = [];
  try {
    rows = (await ctl('/api/me/sessions')).sessions || [];
  } catch (e) { errorState(host, 'Could not list your sessions', e); return; }
  if (!rows.length) { emptyState(host, 'Not signed in anywhere', 'This browser is the only session.'); return; }
  const tbl = el('table', { class: 'grid' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Machine'), el('th', {}, 'Browser'), el('th', {}, 'Address'),
      el('th', {}, 'Signed in'), el('th', {}, 'Last active'),
      el('th', {}, el('span', { class: 'vh' }, 'Row actions')))));
  const body = el('tbody');
  for (const s of rows) {
    body.appendChild(el('tr', {},
      el('td', {}, s.label || '—', s.current ? el('span', { class: 'muted' }, ' (this browser)') : ''),
      el('td', {}, el('span', { class: 'clip' }, s.user_agent || '—')),
      el('td', {}, el('code', {}, s.ip || '—')),
      el('td', {}, when(s.created_at)),
      el('td', {}, when(s.last_seen_at)),
      el('td', {}, el('button', {
        class: 'ghost small', 'data-testid': 'revoke-session-' + s.id,
        onclick: async () => {
          // Revoking the current browser IS signing out, so say so rather than doing it
          // silently and leaving a dashboard that 401s on its next fetch.
          if (!confirm(s.current
            ? 'This is the browser you are using — revoking it signs you out here.'
            : 'Sign this machine out? It will need to sign in again.')) return;
          try {
            await ctl('/api/me/sessions/' + s.id, { method: 'DELETE' });
            if (s.current) { location.reload(); return; }
            loadMachines();
          } catch (e) { alert(e.message); }
        },
      }, s.current ? 'Sign out here' : 'Revoke'))));
  }
  tbl.appendChild(body);
  host.appendChild(tbl);
}

/** Store this configuration as the tenant's own, or '' to go back to following the
 *  server default. One call for both directions: the only difference between owning a
 *  configuration and inheriting one is whether a document is stored. */
async function setStoredConfig(yaml) {
  const status = $('#settings-saved');
  status.textContent = 'saving…';
  try {
    const out = await ctl('/api/me', { method: 'PUT', body: JSON.stringify({ config_yaml: yaml }) });
    account.tenant = out.tenant;
    status.textContent = 'saved';
    loadSettings();
  } catch (e) {
    status.textContent = '';
    alert('Not saved: ' + e.message);
  }
  setTimeout(() => { status.textContent = ''; }, 3000);
}

/**
 * cfgState is what Save posts as `components`: component name → DOTTED key → value, holding
 * ONLY the keys the stored document actually states plus the ones edited on this page.
 *
 * The distinction is the whole contract. An absent key means "the component's own default",
 * which is a DIFFERENT thing from a value — so a key this object does not carry is deleted
 * from the block on save and the component decides again. Inventing a value for a key the
 * document never stated is how a save wrote 20 over a deliberate `llm_max_per_session: 0`.
 * Every control below therefore renders an unstated key as EMPTY with its default as the
 * placeholder, and only writes into this object when somebody changes it.
 *
 * One entry per component with knobs, created on load and kept even when it empties: a name
 * present in `components` is what tells the server to CLEAR that block, so dropping an
 * emptied entry would silently ignore a field the operator just cleared.
 */
let cfgState = null;

/** openComps remembers which component sections are unfolded, so redrawing one (the
 *  recommended button) does not collapse the page under the cursor. */
const openComps = new Set();

/** fieldDefault is what an ABSENT key means: the descriptor's `default`, and when that is
 *  omitted the type's zero — the server omits it exactly when it is the zero value. */
function fieldDefault(fd) {
  if (fd.default !== undefined && fd.default !== null) return fd.default;
  switch (fd.type) {
    case 'bool': return false;
    case 'int': case 'float': return 0;
    case 'strings': return [];
    default: return '';
  }
}

/** fieldText renders a value for a text input. */
function fieldText(fd, v) {
  return fd.type === 'strings' ? (Array.isArray(v) ? v.join(', ') : String(v || '')) : String(v);
}

/** XLLM_SWITCHES are the two passes extract_llm can do. Its constructor REFUSES both off
 *  ("nothing to do"), so the form must not be able to post that combination — see
 *  config.applyExtractLLMCoupling, which takes the component out of the pipeline instead. */
const XLLM_SWITCHES = ['per_output', 'cold_cache.enabled'];

/**
 * renderComponentFields draws ONE component's whole configuration from the descriptors the
 * server serves at /api/options — no field names, types, defaults, enum options or hints in
 * this file at all.
 *
 * That is the point. The hand-written version reached 18 keys of 97, one component of
 * fourteen, and had already drifted: it offered four strategies where the engine parses
 * five, so a stored `deterministic` was not recognised and got rewritten to `code`, quietly
 * turning an LLM-free configuration into one that makes model calls. Anything hand-copied
 * here is a second source of truth for the same fact, which is the bug.
 *
 *   name      component name, used for the ids and for the one coupling below
 *   fields    the descriptors, in declaration order
 *   values    this component's entry in cfgState — MUTATED in place as controls change
 *   disabled  read-only: an inherited configuration, an unreadable one, or a component
 *             that is not in the pipeline
 *   ctx       { recommended, redraw, opts }
 */
function renderComponentFields(name, fields, values, disabled, ctx) {
  const { recommended, redraw, opts } = ctx || {};
  const tid = (key) => 'x-' + name + '-' + key.replace(/\./g, '-');
  const stated = (key) => Object.prototype.hasOwnProperty.call(values, key);
  const set = (key, v) => { values[key] = v; };
  // Deleting, not zeroing: "unset" hands the key back to the component's default, and the
  // server deletes exactly the declared leaf from the document.
  const unset = (key) => { delete values[key]; };
  // Every hint gains what an absent key does, phrased so it stays true whatever the control
  // currently holds — a live "currently unset" note goes stale the moment somebody types.
  const hintOf = (fd) => {
    const def = fieldDefault(fd);
    const shown = fd.type === 'strings' ? (def.length ? fieldText(fd, def) : 'nothing') : String(def);
    return (fd.hint ? fd.hint + ' ' : '') + `Unset means ${shown === '' ? 'empty' : shown}.`;
  };
  const field = (fd, ctl, ...extra) => el('div', { class: 'field cfg-field' },
    el('label', { for: tid(fd.key) }, el('code', {}, fd.key)),
    ctl, el('p', { class: 'hint' }, hintOf(fd)), ...extra);

  // The one place a combination is refused client-side, and it is refused because the
  // component's constructor refuses it. Rather than redraw, the sibling switch is flipped
  // in place and the note says what happened and what to do instead.
  const boxes = {};
  const note = el('p', { class: 'hint warn-text', role: 'status', 'data-testid': 'cfg-note-' + name });
  const effective = (key) => {
    const fd = fields.find((f) => f.key === key);
    return stated(key) ? values[key] : (fd ? fieldDefault(fd) : false);
  };

  const sw = (fd) => {
    const cb = el('input', { type: 'checkbox', id: tid(fd.key), 'data-testid': tid(fd.key) });
    // The default's own value while unstated: a checkbox cannot draw "absent", but leaving
    // it alone still posts nothing, so an untouched form adds no key.
    cb.checked = !!(stated(fd.key) ? values[fd.key] : fieldDefault(fd));
    cb.disabled = disabled;
    boxes[fd.key] = cb;
    cb.addEventListener('change', () => {
      set(fd.key, cb.checked);
      note.textContent = '';
      if (name !== 'extract_llm' || !XLLM_SWITCHES.includes(fd.key) || cb.checked) return;
      if (XLLM_SWITCHES.some((k) => effective(k))) return;
      const other = XLLM_SWITCHES.find((k) => k !== fd.key);
      set(other, true);
      if (boxes[other]) boxes[other].checked = true;
      note.textContent = 'extract_llm with both passes off is a component with nothing to '
        + 'do — its own constructor refuses that, so ' + other + ' was switched back on. To '
        + 'stop it running at all, untick extract_llm in Pipeline components above.';
    });
    return el('div', { class: 'field cfg-field' },
      el('label', { class: 'comp', for: tid(fd.key) }, cb, el('span', { class: 'comp-name' }, fd.key)),
      el('p', { class: 'hint' }, hintOf(fd)));
  };

  const numField = (fd) => {
    // min is semantics, not decoration: 0 on a CAP means unlimited and is a legitimate
    // choice, while 0 on a size threshold is not a setting, it is a removed brake — and the
    // server answers 400 for it. Carrying the right min is what stops a user earning that.
    const min = fd.min || 0;
    const inp = el('input', {
      type: 'number', min: String(min), step: fd.type === 'float' ? 'any' : '1',
      id: tid(fd.key), 'data-testid': tid(fd.key), placeholder: String(fieldDefault(fd)),
    });
    inp.value = stated(fd.key) ? String(values[fd.key]) : '';
    inp.disabled = disabled;
    const err = el('p', { class: 'hint warn-text', role: 'status', 'data-testid': tid(fd.key) + '-err' });
    const restore = () => { inp.value = stated(fd.key) ? String(values[fd.key]) : ''; };
    inp.addEventListener('change', () => {
      err.textContent = '';
      const raw = inp.value.trim();
      if (raw === '') { unset(fd.key); return; }
      const v = fd.type === 'float' ? Number(raw) : parseInt(raw, 10);
      if (!Number.isFinite(v)) {
        err.textContent = 'Not a number. Left as it was; clear the box to use the default.';
        restore();
        return;
      }
      if (v < min) {
        err.textContent = min > 0
          ? `Must be at least ${min}: ${v} here is not a setting, it removes the brake — `
            + 'every candidate clears a floor of 0. Left as it was.'
          : 'Cannot be negative. 0 is allowed here and means unlimited.';
        restore();
        return;
      }
      set(fd.key, v);
    });
    return field(fd, inp, err);
  };

  const textField = (fd) => {
    const inp = el('input', {
      type: fd.secret ? 'password' : 'text', id: tid(fd.key), 'data-testid': tid(fd.key),
      autocomplete: 'off', spellcheck: 'false',
      placeholder: fd.secret
        ? 'stored credential kept — type to replace it'
        : (fd.type === 'strings' ? 'comma separated' : String(fieldDefault(fd))),
    });
    // A secret is WRITE-ONLY. The server never reads it back into the form, and this never
    // puts one in the DOM: a value here would be a credential in every screenshot of this
    // page. Empty therefore means "leave the stored credential alone" — never "clear it" —
    // which is the same reading the server has (an absent secret is not a deletion).
    inp.value = fd.secret || !stated(fd.key) ? '' : fieldText(fd, values[fd.key]);
    inp.disabled = disabled;
    inp.addEventListener('change', () => {
      const raw = inp.value.trim();
      if (raw === '') { unset(fd.key); return; }
      set(fd.key, fd.type === 'strings' ? raw.split(',').map((s) => s.trim()).filter(Boolean) : raw);
    });
    return field(fd, inp);
  };

  const pick = (fd) => {
    // The empty option is how an enum gets back to "unset". The server reads an empty enum
    // as the component's default too, but deleting the key is the honest version of it.
    const sel = el('select', { id: tid(fd.key), 'data-testid': tid(fd.key) },
      el('option', { value: '' }, `— default (${fieldDefault(fd)}) —`),
      ...(fd.options || []).map((v) => el('option', { value: v }, v)));
    sel.value = stated(fd.key) ? String(values[fd.key]) : '';
    sel.disabled = disabled;
    sel.addEventListener('change', () => {
      if (sel.value === '') unset(fd.key); else set(fd.key, sel.value);
    });
    // Whether `source: config` can resolve to anything on THIS deployment. It cannot on the
    // hosted service, and a page that offers the choice silently is how an account watched
    // this component run 251 times and make zero model calls.
    const noStatic = fd.key === 'model.source' && opts && opts.compaction_model === false;
    return field(fd, sel, noStatic
      ? el('p', { class: 'hint warn-text' },
        'THIS DEPLOYMENT HAS NO CONFIGURED COMPACTION MODEL — it deliberately does not spend '
        + 'the operator’s credential on your traffic. So "config" here means the '
        + 'component has no model and never makes a call, however else it is configured.')
      : null);
  };

  const control = (fd) => {
    switch (fd.type) {
      case 'bool': return sw(fd);
      case 'int': case 'float': return numField(fd);
      case 'enum': return pick(fd);
      case 'string': case 'strings': return textField(fd);
      // A type this bundle does not know — the server is newer than the cached page. Shown
      // with no control rather than as a text box: a string posted where the server wants a
      // number is a 400, and inventing a control for an unknown type is guessing.
      default: return el('div', { class: 'field cfg-field' },
        el('label', {}, el('code', {}, fd.key)),
        el('p', { class: 'hint warn-text' },
          `This page does not know the field type “${fd.type}”, so it cannot draw a control `
          + 'for it. Reload; if it persists, the server is newer than this dashboard and a '
          + 'manager can set the key on the account page.'),
        el('p', { class: 'hint' }, hintOf(fd)));
    }
  };

  // A component that reaches a model is the one that can cost more than it saves, and that
  // is read off the descriptors (it has a model block) rather than from a list here.
  const callsModel = fields.some((fd) => fd.key === 'model.source');
  const setCount = fields.filter((fd) => stated(fd.key)).length;
  const det = el('details', { class: 'field comp-fields', 'data-testid': 'cfg-' + name });
  det.open = openComps.has(name);
  det.addEventListener('toggle', () => {
    if (det.open) openComps.add(name); else openComps.delete(name);
  });
  det.appendChild(el('summary', {},
    el('span', { class: 'comp-name' }, name),
    el('span', { class: 'muted' },
      ` — ${setCount} of ${fields.length} key${fields.length === 1 ? '' : 's'} set`
      + (disabled ? ', read-only' : ''))));
  // Why it is locked, when the reason is this component rather than the whole page (an
  // inherited or unreadable configuration is stated once, above, not fourteen times).
  if (ctx && ctx.off) {
    det.appendChild(el('p', { class: 'hint', 'data-testid': 'cfg-off-' + name },
      'Not editable here. Tick it under Pipeline components to configure it — a component '
      + 'that is not in the pipeline does not run, whatever its block says, and switching it '
      + 'off clears the keys below.'));
  }
  if (callsModel) {
    det.appendChild(el('p', { class: 'hint warn-text' },
      'This component calls a model to save tokens, so it can be net negative. Every call is '
      + 'recorded with its cost and its saving — open any request to see them.'));
  }
  det.appendChild(note);
  if (recommended) {
    det.appendChild(el('div', { class: 'actions' },
      el('button', {
        class: 'ghost small', 'data-testid': 'cfg-rec-' + name, disabled: disabled || null,
        onclick: () => { Object.assign(values, recommended); if (redraw) redraw(); },
      }, 'Use recommended values')));
    det.appendChild(el('p', { class: 'hint' },
      'Recommended is not the same thing as default: the defaults below are what an unset '
      + 'key means to the component, this is what we suggest you spend, from our own '
      + 'measurements. It fills the fields in; nothing is saved until you press Save.'));
  }
  for (const fd of fields) det.appendChild(control(fd));
  return det;
}

async function saveSettings() {
  const status = $('#settings-saved');
  status.textContent = 'saving…';
  const inherited = !!account.tenant.config_inherited;
  const prev = (account.tenant.effective_config || {}).pipeline || [];
  const picked = new Set($$('#comp-grid input[type=checkbox]')
    .filter((c) => c.checked).map((c) => c.dataset.comp));
  // Keep the configured run order for everything still enabled; a newly ticked component is
  // appended, never inserted, because this grid does not know where in the pipeline it
  // belongs.
  const ordered = prev.filter((n) => picked.has(n))
    .concat([...picked].filter((n) => !prev.includes(n)));
  const body = {
    capture_content: $('#set-capture').checked,
    up_anthropic: $('#set-up_anthropic').value,
    up_openai: $('#set-up_openai').value,
    up_bob: $('#set-up_bob').value,
  };
  // Omitted while the configuration is inherited: sending it would store a copy of
  // today's default, which is exactly the freeze this page exists to undo. Customise is
  // the deliberate way to start owning one.
  // Never sent by a plain account: the pipeline is the manager's field, and PUT /api/me
  // answers 403 to anyone else — sending it would fail the whole save, upstreams and
  // capture consent included.
  // Never posted from a form the server told us was a guess: the fields would carry the
  // fallback's reading of a document nobody can see. The server refuses this too (409).
  const unreadable = !!(account.tenant.effective_config || {}).parse_error;
  if (!inherited && isManager() && !unreadable) {
    // components carries, per component, ONLY the dotted keys the form actually holds a
    // value for. A key that is not here is DELETED from the block on the server and the
    // component's own default takes over, which is the same thing an absent key means — so
    // the page must not invent one. A component present with an empty object is how a
    // cleared block is expressed; a component absent from it entirely is left untouched.
    // cache carries the account's own consent, so it is sent whenever the form drew the
    // control — the tuning fields are omitted, which leaves the document's own values (or the
    // defaults) alone rather than freezing today's numbers into every saved document.
    body.config = {
      pipeline: ordered, mode: $('#set-mode').value, components: cfgState || {},
      cache: {
        keepalive: !!($('#set-keepalive') || {}).checked,
        head_ttl_1h: !!((account.tenant.effective_config || {}).cache || {}).head_ttl_1h,
      },
    };
  }
  try {
    const out = await ctl('/api/me', { method: 'PUT', body: JSON.stringify(body) });
    account.tenant = out.tenant;
    status.textContent = 'saved';
    loadSettings();
  } catch (e) {
    status.textContent = '';
    // The server's message names the offending key; showing it beats "invalid config".
    alert('Not saved: ' + e.message);
  }
  setTimeout(() => { status.textContent = ''; }, 3000);
}

async function loadTokens() {
  const host = clear($('#token-list'));
  try {
    const me = await ctl('/api/me');
    account.tokens = me.tokens || [];
  } catch { /* the gate will handle it */ }
  if (!account.tokens.length) { emptyState(host, 'No tokens', 'Mint one to get started.'); return; }
  const tbl = el('table', { class: 'grid' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Token'), el('th', {}, 'Label'),
      el('th', {}, 'Created'), el('th', {}, 'Last used'), el('th', {}, el('span', { class: 'vh' }, 'Row actions')))));
  const body = el('tbody');
  for (const t of account.tokens) {
    const revoked = t.revoked_at > 0;
    body.appendChild(el('tr', { class: revoked ? 'revoked' : '' },
      el('td', {}, el('code', {}, 'cg_live_' + t.prefix + '…')),
      el('td', {}, t.label || '—'),
      el('td', {}, when(t.created_at)),
      // "never" is load-bearing: a token that has never been used is one to suspect
      // was never delivered, or was pasted somewhere it does not belong.
      el('td', {}, t.last_used_at ? when(t.last_used_at) : el('span', { class: 'muted' }, 'never')),
      el('td', {}, revoked
        ? el('span', { class: 'muted' }, 'revoked ' + when(t.revoked_at))
        : el('button', {
          class: 'ghost small', 'data-testid': 'revoke-' + t.prefix,
          onclick: async () => {
            if (!confirm('Revoke this token? Any agent using it stops working immediately.')) return;
            try { await ctl('/api/me/tokens/' + t.prefix, { method: 'DELETE' }); loadTokens(); } catch (e) { alert(e.message); }
          },
        }, 'Revoke'))));
  }
  tbl.appendChild(body);
  host.appendChild(tbl);
}

async function loadAudit() {
  const host = clear($('#audit-list'));
  try {
    const out = await ctl('/api/me/audit');
    const rows = out.audit || [];
    if (!rows.length) { emptyState(host, 'No changes yet', 'Configuration edits are recorded here.'); return; }
    const tbl = el('table', { class: 'grid' },
      el('thead', {}, el('tr', {},
        el('th', {}, 'When'), el('th', {}, 'Field'), el('th', {}, 'From'), el('th', {}, 'To'))));
    const body = el('tbody');
    for (const e of rows.slice(0, 50)) {
      body.appendChild(el('tr', {},
        el('td', {}, when(e.at)), el('td', {}, el('code', {}, e.field)),
        el('td', { class: 'clip' }, e.before || '—'),
        el('td', { class: 'clip' }, e.after || '—')));
    }
    tbl.appendChild(body);
    host.appendChild(tbl);
  } catch (e) { errorState(host, 'Could not read the audit log', e); }
}

// ── tenants (manager) ──────────────────────────────────────────────────────

/** captureCell renders the EFFECTIVE transcript state for one account: consent AND the
 *  operator's service-wide gate. Consent alone is not retention. */
function captureCell(consented, operatorGate) {
  if (!consented) return el('span', { class: 'pill neutral', text: 'not consented' });
  if (operatorGate === false) {
    return el('div', {},
      el('span', { class: 'pill missing', text: 'none stored' }),
      el('div', { class: 'muted small', text: 'consented, but capture is off service-wide' }));
  }
  if (operatorGate === null) {
    return el('div', {},
      el('span', { class: 'pill partial', text: 'consented' }),
      el('div', { class: 'muted small', text: 'service-wide gate unknown from here' }));
  }
  return el('span', { class: 'pill complete', text: 'stored' });
}

async function loadTenants() {
  const host = clear($('#tenants-list'));
  loadingState(host);
  try {
    const out = await ctl('/api/tenants');
    const rows = out.tenants || [];
    // Storing a transcript needs BOTH gates: the operator's service-wide one and the
    // account's own consent. A column showing only the second reads as "transcripts are
    // being retained for this account" while the operator's gate is shut and none exist.
    // The server's own configuration is the only place the gate is published.
    let operatorGate = null; // null = could not read it; do not guess either way
    try {
      const cfg = await api('config');
      const dash = (cfg.config || {}).dashboard || {};
      if (typeof dash.capture_content === 'boolean') operatorGate = dash.capture_content;
    } catch (_) { /* stays null: the column says "unknown" rather than inventing a state */ }
    $('#tenants-count').textContent = `${rows.length} account${rows.length === 1 ? '' : 's'}`;
    clear(host);
    if (operatorGate === false) {
      host.appendChild(el('div', { class: 'state blocked', 'data-testid': 'tenants-capture-gate' },
        el('div', { class: 'state-body' },
          el('strong', { text: 'No transcripts are being stored on this proxy' }),
          el('span', { text:
            'Transcript storage is off service-wide (--dashboard-content), which overrides every ' +
            'account\'s own consent below: an account marked "consented" has nothing stored and ' +
            'no diff view. Turning it on is yours to do, not theirs.' }))));
    }
    const tbl = el('table', { class: 'grid' },
      el('thead', {}, el('tr', {},
        el('th', {}, 'Account'), el('th', {}, 'Role'), el('th', {}, 'Variant'), el('th', {}, 'Spend'),
        el('th', {}, 'Last seen'), el('th', {}, 'Transcripts'), el('th', {}, 'Configuration'),
        el('th', {}, el('span', { class: 'vh' }, 'Row actions')))));
    const body = el('tbody');
    for (const t of rows) {
      body.appendChild(el('tr', { class: t.disabled ? 'revoked' : '' },
        el('td', {}, el('div', {}, t.email),
          el('div', { class: 'muted small' }, t.label),
          // Why an account is off, printed where the manager will read it — the same
          // sentence its owner is shown when their agent is refused.
          t.disabled
            ? el('div', { class: 'small warn-text' },
              t.disabled_reason ? 'disabled: ' + t.disabled_reason : 'disabled (no reason recorded)')
            : null),
        el('td', {}, t.role),
        el('td', {}, t.variant
          ? el('span', { class: 'ab-name' }, t.variant)
          : el('span', { class: 'muted' }, '—')),
        el('td', {}, usd(t.spent_usd)),
        el('td', {}, t.last_seen_at ? when(t.last_seen_at) : el('span', { class: 'muted' }, 'never')),
        // The EFFECTIVE answer to "are this account's transcripts being kept", which is
        // the AND of both gates — never the account's flag on its own.
        el('td', {}, captureCell(t.capture_content, operatorGate)),
        // The EFFECTIVE pipeline, plus whose it is: an account that follows the default
        // stores nothing, and an empty cell would read as a broken account.
        //
        // Not the document's first line, which is what this cell used to show. Since the
        // settings form writes the document through a YAML marshaller its keys come out
        // alphabetically, so line one is the word `components:` for every account that has
        // ever saved — a cell that reads as "configured with no components" on exactly the
        // accounts that are configured. The pipeline is the fact anyone opens this column
        // for anyway.
        el('td', {},
          el('code', { class: 'clip' },
            ((t.effective_config || {}).pipeline || []).join(' → ')
            || (t.effective_config_yaml ? 'no components' : '—')),
          t.config_inherited ? el('div', { class: 'muted small' }, 'server default') : null),
        // Two buttons, not five. Everything that CHANGES this account — its configuration,
        // its variant, disable, a reissued token, a reset, purge, delete — is in the editor,
        // where the account's own facts are on screen next to the control. A row of small
        // ghost buttons that each act on a different account by id is how the wrong row
        // gets clicked.
        el('td', {}, el('div', { class: 'row-actions' },
          el('button', {
            class: 'ghost small', onclick: () => showTenantMetrics(t.id),
          }, 'Metrics'),
          el('button', {
            class: 'ghost small', 'data-testid': 'manage-' + t.id,
            onclick: () => openTenantEditor(t.id),
          }, 'Manage')))));
    }
    tbl.appendChild(body);
    host.appendChild(tbl);
  } catch (e) {
    clear(host);
    errorState(host, 'Could not list tenants', e);
  }
  // The A/B card lives in this view, above the roster: a variant is assigned here, so the
  // comparison belongs next to the assignment rather than behind another tab.
  loadVariants();
}

/** Jump to this tenant's traffic. Managers get ?tenant= on every read route, so the
 *  existing views work unchanged once the filter is set. */
function showTenantMetrics(id) {
  // quiet: go() below pushes the URL and refetches once. Setting it loudly here would
  // fetch the current view with the new scope and then immediately fetch Overview.
  setFilter('tenant', id, { quiet: true });
  go('overview');
}

// ── the account editor (manager) ───────────────────────────────────────────
//
// One panel per account, opened from the roster and linkable (#tenants?acct=<id>), because
// "the account I mean" is a thing managers send each other. It reuses the request drawer:
// same focus trap, same Escape, same Back-closes-it behaviour — a second panel would need
// all of that again and would get some of it wrong.
//
// Everything that mutates an account is in here rather than in its row, so the facts and
// the controls are on screen together. The alternative — a row of small buttons that each
// act on a different account by id — is how the wrong row gets clicked.

/** openTenantEditor shows one account. fromURL suppresses the history push. */
async function openTenantEditor(id, fromURL) {
  if (!fromURL) { state.drawer = { acct: id }; syncURL(false); }
  const body = openDrawer('Account', null);
  loadingState(body, 4);
  let rows = [];
  try {
    rows = (await ctl('/api/tenants')).tenants || [];
  } catch (e) { errorState(body, 'Could not read the roster', e); return; }
  const t = rows.find((x) => x.id === id);
  if (!t) {
    emptyState(clear(body), 'No such account', 'It may have just been deleted.');
    return;
  }
  await loadOptions(); // the upstream allow-list, so the selects offer only what is accepted
  renderTenantEditor(clear(body), t);
}

/** reloadTenantEditor repaints the panel and the roster behind it after a change, so the
 *  two can never disagree about what was just saved. */
function reloadTenantEditor(id) {
  loadTenants();
  openTenantEditor(id, true);
}

function renderTenantEditor(host, t) {
  $('#drawer-title').textContent = t.email;
  const status = el('span', { class: 'muted small', 'data-testid': 'acct-status' });
  const say = (msg, bad) => {
    status.textContent = msg || '';
    status.className = 'small ' + (bad ? 'warn-text' : 'muted');
  };

  // What this account IS, before anything about changing it — the same fact band the
  // request drawer uses.
  host.appendChild(el('div', { class: 'kv-band' },
    el('div', { class: 'kv' },
      kv('Role', t.role),
      kv('Variant', t.variant || 'none'),
      kv('Month to date', usd(t.spent_usd)),
      kv('Last seen', t.last_seen_at ? when(t.last_seen_at) : 'never'),
      kv('Registered', when(t.created_at)),
      kv('Provider keys bound', num(t.agent_keys)),
      kv('Row quota', t.max_rows ? num(t.max_rows) : 'server default'),
      kv('Password', t.has_password ? 'set' : 'never set'),
      kv('Status', t.disabled ? 'disabled' : 'active'))));

  // Says where the transcripts are rather than leaving a manager to hunt: the drawer and
  // diff viewer are the tenant's own, reached by pointing the account selector here.
  host.appendChild(el('div', { class: 'state blocked' },
    el('div', { class: 'state-body' },
      el('strong', { text: 'You can read this account’s transcripts' }),
      el('span', {
        text: 'Pick this account in the selector, then open any request or session diff. '
          + 'Only what they consented to capture exists to read.',
      }))));

  const fields = el('div');
  host.appendChild(fields);

  const label = textField(fields, 'acct-label', 'Machine label', t.label);
  const variant = textField(fields, 'acct-variant', 'A/B variant', t.variant,
    'A label only — it changes nothing about their pipeline. Letters, digits, dot, dash '
    + 'or underscore. Empty means not in a test.');
  const role = el('select', { id: 'acct-role', 'data-testid': 'acct-role' },
    el('option', { value: 'user' }, 'user'),
    el('option', { value: 'manager' }, 'manager — can administer every account'));
  role.value = t.role;
  fields.appendChild(el('div', { class: 'field' },
    el('label', { for: 'acct-role' }, 'Role'), role));

  const rowsCap = el('input', {
    type: 'number', min: '0', step: '1000', id: 'acct-rows', 'data-testid': 'acct-rows',
  });
  rowsCap.value = String(t.max_rows || 0);
  fields.appendChild(el('div', { class: 'field' },
    el('label', { for: 'acct-rows' }, 'Retained request rows'), rowsCap,
    el('p', { class: 'hint' }, '0 follows the server default. Over the cap their oldest '
      + 'sessions are archived or dropped — theirs, not everyone’s.')));

  const ups = (account.options && account.options.upstreams) || [];
  const upSel = {};
  for (const [key, text] of [['up_anthropic', 'Anthropic-dialect upstream'],
    ['up_openai', 'OpenAI-dialect upstream'], ['up_bob', 'Bob upstream']]) {
    const sel = el('select', { id: 'acct-' + key, 'data-testid': 'acct-' + key },
      el('option', { value: '' }, '— none —'),
      ...ups.map((u) => el('option', { value: u.name }, u.name + ' (' + u.dialect + ')')));
    sel.value = t[key] || '';
    upSel[key] = sel;
    fields.appendChild(el('div', { class: 'field' }, el('label', { for: 'acct-' + key }, text), sel));
  }

  const cap = el('input', { type: 'checkbox', id: 'acct-capture', 'data-testid': 'acct-capture' });
  cap.checked = !!t.capture_content;
  fields.appendChild(el('div', { class: 'field' },
    el('label', { class: 'comp', for: 'acct-capture' }, cap,
      el('span', { class: 'comp-name' }, 'Store their transcripts')),
    el('p', { class: 'hint' }, 'Their consent, which you can withdraw but never read '
      + 'through. Turning it off stops new capture; it deletes nothing.')));

  // Whether the configuration is theirs or the server default is the first thing to say,
  // because saving a tracking account's form must not silently freeze a copy of today's
  // default onto it — the same trap the user's own settings page avoids.
  const effective = t.effective_config_yaml || '';
  const inherited = !!t.config_inherited;
  fields.appendChild(el('div', { class: 'cfg-state' },
    el('div', {}, inherited
      ? el('span', {}, el('strong', {}, 'Following the server default.'),
        ' Saving this form leaves that alone unless you edit the YAML below.')
      : el('span', {}, el('strong', {}, 'Has its own configuration.'),
        ' Changes to the server default do not reach them.'))));
  const yaml = el('textarea', {
    id: 'acct-yaml', rows: 12, spellcheck: 'false', 'data-testid': 'acct-yaml',
    'aria-label': 'Their full configuration, YAML',
  });
  yaml.value = effective;
  fields.appendChild(el('details', { class: 'field' },
    el('summary', {}, 'Full configuration (YAML)'), yaml,
    el('p', { class: 'hint' }, 'Pipeline, per-component settings and mode. Validated on '
      + 'save by the same loader the proxy builds with, so a typo is a refusal naming the '
      + 'key rather than a surprise at request time. Saving rebuilds their pipeline, which '
      + 'discards their frozen compaction decisions — their next turn will not be cache-warm.')));

  fields.appendChild(el('div', { class: 'actions' },
    el('button', {
      class: 'primary', 'data-testid': 'acct-save',
      onclick: async () => {
        say('saving…');
        const patch = {
          label: label.value.trim(),
          variant: variant.value.trim(),
          role: role.value,
          max_rows: Number(rowsCap.value) || 0,
          capture_content: cap.checked,
          up_anthropic: upSel.up_anthropic.value,
          up_openai: upSel.up_openai.value,
          up_bob: upSel.up_bob.value,
        };
        // Omitted while it is inherited AND untouched: sending it would store a copy of
        // today's default and quietly end their tracking of it.
        if (!inherited || yaml.value.trim() !== effective.trim()) patch.config_yaml = yaml.value;
        try {
          await ctl('/api/tenants/' + t.id, { method: 'PATCH', body: JSON.stringify(patch) });
          reloadTenantEditor(t.id);
        } catch (e) { say('not saved: ' + e.message, true); }
      },
    }, 'Save'),
    status));

  // Availability and recovery. Disabling asks for a reason because the reason is what the
  // account's owner is shown when their agent is refused — without it, "disabled" is
  // indistinguishable from the proxy being broken.
  host.appendChild(el('h2', {}, 'Availability'));
  const reason = el('input', {
    type: 'text', id: 'acct-reason', maxlength: '200', 'data-testid': 'acct-reason',
    placeholder: 'e.g. paused pending the finance review',
  });
  reason.value = t.disabled_reason || '';
  if (!t.disabled) {
    host.appendChild(el('div', { class: 'field' },
      el('label', { for: 'acct-reason' }, 'Reason (shown to them)'), reason,
      el('p', { class: 'hint' }, 'Returned in the 403 their agent receives and in the '
        + 'refusal at sign-in. They are who reads this; write it for them.')));
  }
  host.appendChild(el('div', { class: 'actions' },
    el('button', {
      class: 'ghost', 'data-testid': 'acct-toggle',
      onclick: async () => {
        const patch = t.disabled
          ? { disabled: false }
          : { disabled: true, disabled_reason: reason.value.trim() };
        if (!t.disabled && !confirm('Disable ' + t.email + '? Their agents stop immediately '
          + 'and they are signed out of the dashboard.')) return;
        try {
          await ctl('/api/tenants/' + t.id, { method: 'PATCH', body: JSON.stringify(patch) });
          reloadTenantEditor(t.id);
        } catch (e) { say(e.message, true); }
      },
    }, t.disabled ? 'Enable' : 'Disable'),
    el('button', {
      class: 'ghost', 'data-testid': 'acct-reset',
      onclick: async () => {
        if (!confirm('Email ' + t.email + ' a password reset code?\n\nYou will not see the '
          + 'code and cannot set their password. Their current password keeps working '
          + 'until they finish.')) return;
        try {
          const out = await ctl('/api/tenants/' + t.id + '/password-reset', { method: 'POST' });
          say(out.note || 'reset code mailed');
        } catch (e) { say(e.message, true); }
      },
    }, 'Email a password reset'),
    el('button', {
      class: 'ghost',
      onclick: async () => {
        if (!confirm('Mint a replacement token for ' + t.email + '? Hand it over on a '
          + 'channel you trust; it is shown once.')) return;
        try {
          const out = await ctl('/api/tenants/' + t.id + '/tokens',
            { method: 'POST', body: JSON.stringify({ label: 'reissued' }) });
          prompt('Copy this token now — it cannot be recovered:', out.token);
        } catch (e) { say(e.message, true); }
      },
    }, 'Reissue token')));

  host.appendChild(dangerZone(t, say));
}

/** kv renders one label/value pair of the drawer's fact band. */
function kv(k, v) {
  return el('div', {}, el('div', { class: 'k' }, k), el('div', { class: 'v' }, String(v)));
}

/** textField appends a labelled text input and returns it. */
function textField(host, id, labelText, value, hint) {
  const input = el('input', { type: 'text', id, 'data-testid': id });
  input.value = value || '';
  host.appendChild(el('div', { class: 'field' },
    el('label', { for: id }, labelText), input,
    hint ? el('p', { class: 'hint' }, hint) : null));
  return input;
}

/**
 * dangerZone builds the purge and delete controls.
 *
 * Both are irreversible and both act on somebody else's data BY ID, so the mistake worth
 * engineering against is acting on the wrong account. The buttons are therefore inert until
 * that account's own address has been typed into the box beside them — the server demands
 * the same string, so this is the visible half of a check that is enforced anyway.
 *
 * Folded away behind a summary, and never a bare one-click button.
 */
function dangerZone(t, say) {
  const confirmBox = el('input', {
    type: 'text', id: 'acct-confirm', 'data-testid': 'acct-confirm',
    autocomplete: 'off', spellcheck: 'false', placeholder: t.email,
  });
  const purge = el('button', { class: 'ghost small', disabled: true, 'data-testid': 'acct-purge' },
    'Purge their stored data');
  const del = el('button', { class: 'destructive small', disabled: true, 'data-testid': 'acct-delete' },
    'Delete this account and its data');
  const matches = () => confirmBox.value.trim().toLowerCase() === t.email.toLowerCase();
  const sync = () => { purge.disabled = !matches(); del.disabled = !matches(); };
  confirmBox.addEventListener('input', sync);

  // What it removed, reported afterwards: a destructive action that answers "ok" tells the
  // person who pressed it nothing about whether it did anything.
  const report = el('p', { class: 'hint', 'data-testid': 'acct-purge-report' });
  const run = async (path, method, verb) => {
    if (!matches()) return;
    if (!confirm(verb + '.\n\nThis cannot be undone. Continue?')) return;
    say('working…');
    try {
      const out = await ctl(path, {
        method, body: JSON.stringify({ confirm: confirmBox.value.trim() }),
      });
      const p = out.purged || {};
      say('');
      report.className = 'hint ok';
      report.textContent = out.status + ': ' + num(p.requests) + ' requests, '
        + num(p.components) + ' component rows, ' + num(p.content) + ' stored transcripts, '
        + num(p.archives) + ' archived sessions (' + num(p.objects)
        + ' objects deleted from cold storage).';
      loadTenants();
      if (method === 'DELETE') {
        // The account is gone, so the panel describing it must not stay open showing a form
        // whose every button would now 404.
        setTimeout(() => { dismissDrawer(); state.drawer = null; syncURL(true); }, 1500);
      } else {
        confirmBox.value = '';
        sync();
      }
    } catch (e) {
      say('');
      report.className = 'hint warn-text';
      report.textContent = e.message;
    }
  };
  purge.addEventListener('click', () => run('/api/tenants/' + t.id + '/purge', 'POST',
    'Purge every stored request, component row and transcript for ' + t.email
    + ', including its archives in cold storage. The account itself keeps working'));
  del.addEventListener('click', () => run('/api/tenants/' + t.id, 'DELETE',
    'Delete ' + t.email + ': the account, its tokens, its sessions and all of its stored '
    + 'data in both databases and in cold storage'));

  return el('details', { class: 'danger', 'data-testid': 'acct-danger' },
    el('summary', {}, 'Danger zone'),
    el('p', { class: 'hint' },
      'Purge clears their history and leaves the account working. Delete removes the '
      + 'account too. Both reach the metrics database and cold storage, and neither can be '
      + 'undone — the audit record of having done it is all that is kept.'),
    el('div', { class: 'field' },
      el('label', { for: 'acct-confirm' }, 'Type ' + t.email + ' to enable these'),
      confirmBox),
    el('div', { class: 'danger-act' }, purge, del),
    report);
}

// ── A/B variants (manager) ─────────────────────────────────────────────────
//
// A variant is a name a manager put on a set of accounts; this panel groups the metrics
// that already exist by it. There is deliberately no significance test and no winner
// highlighting: assignment is not random and the workloads are not comparable, so a test
// statistic here would look like evidence without being any.
//
// A TABLE rather than a chart, on purpose. This is a handful of groups across eight
// measures whose DENOMINATORS have to sit beside the figures — a table's job. A bar chart
// of "spend per variant" would be precisely the misleading artefact the note above it warns
// about: two bars, no sample size, no confounds.

async function loadVariants() {
  const host = clear($('#ab-list'));
  const caveatHost = clear($('#ab-caveats'));
  loadingState(host, 2);
  const range = Number($('#ab-range').value) || 0;
  const q = range > 0 ? '?since=' + (Date.now() - range) : '';
  let out;
  try {
    out = await ctl('/api/variants' + q);
  } catch (e) { clear(host); errorState(host, 'Could not compare variants', e); return; }
  const rows = out.variants || [];
  clear(host);
  if (!rows.some((v) => v.variant)) {
    emptyState(host, 'No variants assigned yet',
      'Open an account from the roster below and give it a variant name. Two names over two '
      + 'groups is an A/B test; accounts with no name are grouped as unassigned.');
    return;
  }
  const tbl = el('table', { class: 'grid' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Variant'), el('th', {}, 'Accounts'), el('th', {}, 'Requests'),
      el('th', {}, 'Tokens in → out'), el('th', {}, 'Saved'), el('th', {}, 'Spent'),
      el('th', {}, 'Spent / request'), el('th', {}, 'Saved (est.)'),
      el('th', {}, 'Prefix split'), el('th', {}, 'Provider cache'),
      el('th', {}, 'Unpriced'))));
  const body = el('tbody');
  for (const v of rows) {
    const perReq = v.requests > 0 ? v.spent_usd / v.requests : null;
    const savedPct = v.tokens_before > 0 ? (100 * v.saved) / v.tokens_before : null;
    body.appendChild(el('tr', {},
      el('td', {},
        v.variant ? el('span', { class: 'ab-name' }, v.variant)
          : el('span', { class: 'muted' }, 'unassigned'),
        // Several configurations inside one variant means it is not one treatment. Said on
        // the row, because it invalidates that row's comparison specifically.
        (v.configs || []).length > 1
          ? el('span', { class: 'denom warn-text' }, (v.configs || []).length + ' different configs')
          : null),
      el('td', {}, num(v.tenants),
        el('span', { class: 'denom' }, num(v.reporting) + ' with traffic')),
      el('td', {}, num(v.requests),
        el('span', { class: 'denom' }, num(v.sessions) + (v.sessions === 1 ? ' session' : ' sessions'))),
      el('td', {}, compact(v.tokens_before) + ' → ' + compact(v.tokens_after)),
      el('td', {}, compact(v.saved),
        el('span', { class: 'denom' }, savedPct === null ? 'no traffic' : pct(savedPct) + ' of input')),
      el('td', {}, usd(v.spent_usd)),
      // The only normalisation offered, and the one this project has already been misled
      // by: it says nothing about cost per TASK, because a variant that needs more turns
      // can be cheaper per request and dearer per job.
      el('td', {}, perReq === null ? '—' : usd(perReq),
        el('span', { class: 'denom' }, 'not per task')),
      el('td', {}, usd(v.saved_usd),
        el('span', { class: 'denom' }, 'counterfactual')),
      // Beside it, because an arm that compacts deeper can win on the column above while
      // losing more than that on the cache it destroyed. Both figures, because they answer
      // different questions: ours is what the prefix split earned in this arm, the provider's
      // is the control variable — the thing a deep pipeline silently spends.
      el('td', {}, usd(v.cachesplit_saved_usd),
        el('span', { class: 'denom' }, 'prefix split, ours')),
      el('td', {}, usd(v.cache_saved_usd),
        el('span', { class: 'denom' }, 'provider cache')),
      // Rows the provider priced for nobody. Where this approaches the request count, the
      // money columns on this row are unknown rather than small.
      el('td', {}, num(v.incomplete_rows),
        v.requests > 0 && v.incomplete_rows >= v.requests
          ? el('span', { class: 'denom warn-text' }, 'money unknown')
          : null)));
    // Which component did the work, folded across the variant's accounts — the row that
    // turns "arm B is cheaper" into something anyone can act on.
    const comps = v.components || [];
    if (comps.length) {
      const inner = el('table', { class: 'grid' },
        el('thead', {}, el('tr', {}, el('th', {}, 'Component'), el('th', {}, 'Ran'),
          el('th', {}, 'Acted'), el('th', {}, 'Reverted'), el('th', {}, 'Saved'))));
      const ibody = el('tbody');
      for (const c of comps.slice(0, 20)) {
        ibody.appendChild(el('tr', {},
          el('td', {}, el('code', {}, c.component)),
          el('td', {}, num(c.runs)),
          el('td', {}, num(c.acted) + ' (' + pct(100 * (c.act_rate || 0)) + ')'),
          el('td', {}, c.reverted ? el('span', { class: 'warn-text' }, num(c.reverted)) : '0'),
          el('td', {}, compact(c.saved_unique))));
      }
      inner.appendChild(ibody);
      body.appendChild(el('tr', {}, el('td', { colspan: '9' },
        el('details', {}, el('summary', { class: 'hint' },
          'Components in ' + (v.variant || 'unassigned')), inner))));
    }
  }
  tbl.appendChild(body);
  host.appendChild(tbl);

  // The full caveat list comes from the server rather than being written twice: the API
  // decides what this comparison cannot show, and a second copy in the page would drift.
  const list = el('ul');
  for (const c of out.caveats || []) list.appendChild(el('li', {}, c));
  caveatHost.appendChild(el('details', { class: 'why' },
    el('summary', {}, 'What this comparison cannot tell you'),
    el('p', { class: 'hint' }, out.description || ''), list));
}

// ── archive ────────────────────────────────────────────────────────────────
async function loadArchive() {
  const host = clear($('#archive-list'));
  loadingState(host);
  try {
    const out = await ctl('/api/archive');
    const rows = out.archived || [];
    // Rows carry the remote they were written to. Saying "not configured" while listing
    // archived sessions is a contradiction the reader has to resolve; naming the remote
    // the objects are actually in, and that it is not set up here, is the true version.
    const rowRemote = rows.length ? rows[0].remote : '';
    $('#archive-remote').textContent = out.remote ? 'stored in ' + out.remote
      : rowRemote ? 'stored in ' + rowRemote + ' — not configured on this deployment now'
        : 'cold storage is not configured on this deployment';
    clear(host);
    if (!rows.length) {
      emptyState(host, 'Nothing archived yet',
        out.remote
          ? 'Sessions move here once they have been idle long enough.'
          : 'Without cold storage, old sessions are deleted rather than archived.');
      return;
    }
    const tbl = el('table', { class: 'grid' },
      el('thead', {}, el('tr', {},
        el('th', {}, 'Session'), el('th', {}, 'Requests'), el('th', {}, 'Active'),
        el('th', {}, 'Archived'), el('th', {}, 'Size'), el('th', {}, 'What moved'), el('th', {}, el('span', { class: 'vh' }, 'Row actions')))));
    const body = el('tbody');
    for (const a of rows) {
      const kind = a.full_path ? 'whole session' : 'transcripts only';
      body.appendChild(el('tr', {},
        el('td', {}, el('code', { class: 'clip' }, a.session_id)),
        el('td', {}, num(a.requests)),
        el('td', {}, `${when(a.first_ts)} → ${when(a.last_ts)}`),
        el('td', {}, when(a.archived_at)),
        el('td', {}, compact((a.full_bytes || 0) + (a.content_bytes || 0)) + 'B'),
        el('td', {}, kind),
        el('td', {}, el('button', {
          class: 'ghost small', 'data-testid': 'open-archive',
          // Opens the drawer on LOCAL data only: the metrics, plus the explicit
          // "fetch transcript" affordance. Clicking a row must never be the thing
          // that starts an rclone round trip.
          onclick: () => openSessionDiff(a.session_id),
        }, 'Open'))));
    }
    tbl.appendChild(body);
    host.appendChild(tbl);
  } catch (e) {
    clear(host);
    errorState(host, 'Could not list the archive', e);
  }
}

// Opening an archived session goes through openSessionDiff, the same drawer the
// Sessions view uses. There used to be a second reader here that fetched immediately
// and reported both failure modes through alert(); one renderer means the cold /
// unreachable / never-archived states are rendered identically wherever they are
// reached, and a modal alert is not a state a user can read a session id out of.

// ── wiring ─────────────────────────────────────────────────────────────────
Object.assign(loaders, {
  setup: loadSetup, settings: loadSettings, tenants: loadTenants, archive: loadArchive,
});

function initAccounts() {
  $('#gate-tab-signin').addEventListener('click', () => {
    cancelVerify(); // switching tabs abandons a pending code rather than hiding it
    cancelReset(true);
    hideGateForms();
    $('#gate-signin').hidden = false;
    $('#gate-tab-signin').setAttribute('aria-selected', 'true');
    $('#gate-tab-register').setAttribute('aria-selected', 'false');
    gateError('');
    gateNotice('');
  });
  $('#gate-tab-register').addEventListener('click', () => {
    cancelVerify();
    cancelReset(true);
    hideGateForms();
    gateNotice('');
    $('#gate-tab-signin').setAttribute('aria-selected', 'false');
    $('#gate-tab-register').setAttribute('aria-selected', 'true');
    // The form appears only if this deployment would accept it; applyRegisterMode puts
    // the closed explanation up instead.
    $('#gate-register').hidden = account.register === 'closed';
    applyRegisterMode();
    gateError('');
  });

  $('#gate-signin').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    gateError('');
    const token = $('#gate-token').value.trim();
    try {
      if (token) {
        // The legacy path: a token, no second factor, for an account with no password.
        await ctl('/api/login', { method: 'POST', body: JSON.stringify({ token }) });
        $('#gate-token').value = ''; // not in the DOM one moment longer than needed
        await probeAccount();
        go('overview'); // refresh() inside go() loads the facets for the new principal
        connectLive();
        return;
      }
      const email = $('#gate-signin-email').value.trim();
      const out = await ctl('/api/login', {
        method: 'POST',
        body: JSON.stringify({ email, password: $('#gate-password').value }),
      });
      // The password leaves the DOM the moment it has been spent, whatever happens next.
      $('#gate-password').value = '';
      showVerify(out.email || email, out.code_expires_at,
        `We emailed a 6-digit code to ${out.email || email}.`);
    } catch (e) { $('#gate-password').value = ''; gateError(e.message); }
  });

  $('#gate-register').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    gateError('');
    const email = $('#gate-email').value.trim();
    try {
      const out = await ctl('/api/register', {
        method: 'POST',
        body: JSON.stringify({
          email,
          password: $('#gate-new-password').value,
          label: $('#gate-label').value.trim(),
          // Sent only in invite mode, where the server compares it in constant time.
          // Always present as a key so the server sees "" rather than a missing field.
          code: account.register === 'invite' ? $('#gate-code').value : '',
        }),
      });
      // Neither the password nor the invite code stays in the DOM after being spent.
      $('#gate-new-password').value = '';
      $('#gate-code').value = '';
      // No token and no session yet: the account is inert until the mailed code comes
      // back. That is the whole point of phase two, so the UI must not pretend otherwise.
      showVerify(out.email || email, out.code_expires_at,
        `We emailed a 6-digit code to ${out.email || email} to confirm the address.`);
    } catch (e) { $('#gate-new-password').value = ''; gateError(e.message); }
  });

  $('#gate-verify').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    gateError('');
    try {
      const out = await ctl('/api/verify', {
        method: 'POST',
        body: JSON.stringify({ email: verify.email, code: $('#gate-verify-code').value.trim() }),
      });
      cancelVerify();
      await probeAccount();
      if (out.token) {
        // Registration's reply carries the ONLY copy of the first token, so go straight
        // to Setup with it substituted into the snippets.
        account.freshToken = out.token;
        go('setup');
      } else {
        go('overview');
      }
      connectLive();
    } catch (e) { gateError(e.message); }
  });

  $('#gate-verify-cancel').addEventListener('click', () => { gateError(''); cancelVerify(); });

  $('#signout').addEventListener('click', async () => {
    try { await ctl('/api/logout', { method: 'POST' }); } catch { /* clearing locally regardless */ }
    account.tenant = null;
    account.freshToken = null;
    showGate(true);
    applyAccount();
  });

  initReset();
  // The A/B window is local to that card: the Tenants view hides the global filter bar
  // (it is not a traffic view), so the comparison carries its own range.
  $('#ab-range').addEventListener('change', loadVariants);

  $('#mint-token').addEventListener('click', async () => {
    const label = prompt('Name this token (e.g. laptop, ci):', 'new-token');
    if (label === null) return;
    try {
      const out = await ctl('/api/me/tokens', { method: 'POST', body: JSON.stringify({ label }) });
      account.freshToken = out.token;
      prompt('Copy this token now — it is shown once and cannot be recovered:', out.token);
      loadTokens();
    } catch (e) { alert(e.message); }
  });
}

// ── feedback ───────────────────────────────────────────────────────────────
//
// One view, two audiences. Everybody gets the form; the manager additionally gets the
// aggregate and every answer, which are the three [data-manager] cards in index.html —
// so the entitlement is declared in the markup and applied by applyAccount(), exactly
// like the Tenants tab, rather than by a second check here that could disagree with it.
//
// The server is the authority on all of it: it re-checks the 50-character rule, it
// refuses a rating it did not ask for, and it answers 403 to a plain account that asks
// to read. What this file does is make the rules visible before the round trip.

/** STAR_WORDS name each step, so a rating is never just a count of shapes. */
const STAR_WORDS = ['bad', 'poor', 'okay', 'good', 'excellent'];

/**
 * The questions and the agent selector come from the SERVER, keys AND wording:
 * tenant.FeedbackQuestions and tenant.FeedbackAgents. A key invented here would be
 * refused with a 422, and wording invented here would label a row with a different
 * question from the one the manager's email reports — so neither is written down twice.
 *
 * Filled from /api/me for the form, and from /api/feedback for the manager's view, which
 * is also what lets that view label a key it is only reading.
 */
const feedbackForm = { questions: [], agents: [] };

const labelOf = (list, key) => (list.find((x) => x.key === key) || {}).label || key;
/** dimLabel prints a question key the way the form asked it. */
const dimLabel = (key) => labelOf(feedbackForm.questions, key);
const agentLabel = (key) => (key ? labelOf(feedbackForm.agents, key) : 'not stated');

/**
 * meaningfulLen counts the characters a reader would see, collapsing every run of
 * whitespace to one — the same rule tenant.meaningfulLen applies server-side.
 *
 * Both halves count it the same way on purpose: a counter that says 62 next to a server
 * that says "not 50 yet" is a form the user cannot satisfy, and 50 spaces must not be a
 * way past a mandatory field.
 */
function meaningfulLen(s) { return (s || '').trim().split(/\s+/).filter(Boolean).join(' ').length; }

/** starSVG draws one star. currentColor, so CSS decides filled or empty. */
function starSVG() {
  const svg = svgEl('svg', { viewBox: '0 0 20 20', 'aria-hidden': 'true', class: 'star-icon' });
  svg.appendChild(svgEl('path', {
    d: 'M10 1.6l2.6 5.3 5.8.8-4.2 4.1 1 5.8-5.2-2.8-5.2 2.8 1-5.8L1.6 7.7l5.8-.8z',
    fill: 'currentColor',
  }));
  return svg;
}

/**
 * starField builds one question: a fieldset, five radios, five star labels.
 *
 * Native radios, deliberately. They come with keyboard support (arrows move within the
 * group, Tab leaves it), a name/value pair the browser submits, screen-reader semantics
 * and a focus ring — all of which a div-with-click-handlers has to reimplement and
 * usually gets wrong. The stars are the LABELS; the .on class is paint over the radio's
 * state, never the state itself.
 */
function starField(key, question, help) {
  const name = 'fb-' + key;
  const read = el('span', { class: 'stars-read', 'aria-live': 'polite' });
  const row = el('div', { class: 'stars', role: 'radiogroup', 'aria-label': question });
  const paint = () => {
    const chosen = row.querySelector('input:checked');
    const v = chosen ? Number(chosen.value) : 0;
    $$('.star', row).forEach((s, i) => s.classList.toggle('on', i < v));
    read.textContent = v ? `${v} of 5 — ${STAR_WORDS[v - 1]}` : '';
  };
  for (let v = 1; v <= 5; v++) {
    const id = name + '-' + v;
    row.appendChild(el('input', {
      type: 'radio', name, id, value: String(v), class: 'star-input',
      'data-testid': 'star-' + key + '-' + v, onchange: paint,
    }));
    // The label's accessible name is the WORD, not the position: "4 — good" is a rating,
    // "star 4" is a coordinate.
    row.appendChild(el('label', { for: id, class: 'star', title: `${v} — ${STAR_WORDS[v - 1]}` },
      el('span', { class: 'vh', text: `${v} of 5 — ${STAR_WORDS[v - 1]}` }), starSVG()));
  }
  return el('fieldset', { class: 'stars-field', 'data-dim': key },
    el('legend', {}, question),
    help ? el('p', { class: 'hint', text: help }) : null,
    el('div', { class: 'stars-line' }, row, read),
    el('p', { class: 'field-error', role: 'alert', hidden: true, 'data-testid': 'err-' + key }));
}

/** Show or clear the error under one field. role=alert, so it is announced. */
function fieldError(node, msg) {
  const p = node.querySelector('.field-error');
  if (!p) return;
  p.textContent = msg || '';
  p.hidden = !msg;
}

/**
 * loadFeedback draws the form, and — for a manager — the aggregate below it.
 *
 * The questions and the two agents come from /api/me, so this file never guesses at a key
 * the server validates or at wording the server's email prints.
 */
async function loadFeedback() {
  const form = $('#feedback-form');
  if (!form.dataset.built) {
    try {
      const me = await ctl('/api/me');
      feedbackForm.questions = me.feedback_questions || [];
      feedbackForm.agents = me.feedback_agents || [];
    } catch (e) {
      // No questions means no form: drawing an empty one would collect nothing the server
      // would accept. The loader must not reject, so this is reported in place.
      errorState(clear(form), 'Could not load the feedback form', e);
      return;
    }
    buildFeedbackForm(form);
    form.dataset.built = '1';
  }
  if (isManager()) loadFeedbackAdmin();
}

function buildFeedbackForm(form) {
  clear(form);

  // Which agent first: the same seven questions follow either way, and the answer is
  // stored so the manager can read Claude Code and Bob apart. A native select, so the
  // keyboard, the screen reader and the mobile picker all come for free.
  const agent = el('select', { id: 'fb-agent', 'data-testid': 'fb-agent' },
    el('option', { value: '' }, 'Choose one…'),
    ...feedbackForm.agents.map((a) => el('option', { value: a.key }, a.label)));
  const agentField = el('div', { class: 'field' },
    el('label', { for: 'fb-agent' }, 'Which agent is this about? (required)'),
    agent,
    el('p', { class: 'field-error', role: 'alert', hidden: true, 'data-testid': 'err-agent' }));
  form.appendChild(agentField);

  for (const q of feedbackForm.questions) form.appendChild(starField(q.key, q.label));

  const comment = el('textarea', {
    id: 'fb-comment', rows: '6', maxlength: '4000', required: 'required',
    'aria-describedby': 'fb-comment-count', 'data-testid': 'fb-comment',
    placeholder: 'How it feels, what to add or improve, any bugs.',
  });
  // ONE element carries both the live count and the validation message for this field.
  //
  // It is deliberately not a separate error paragraph that appears, and this is a bug
  // fixed rather than a preference: an element that materialises on blur moves the submit
  // button DOWN between mousedown and mouseup, so the browser sees no click on it and the
  // first press of "Send feedback" does nothing at all. Found in the browser, not in a
  // test — the count line is always present, so nothing below it can move.
  //
  // aria-live so the count and the message are both announced as they change, and
  // aria-describedby on the textarea so a screen reader reads the requirement on focus.
  const count = el('p', {
    class: 'hint', id: 'fb-comment-count', 'data-testid': 'fb-count', 'aria-live': 'polite',
  });
  const commentField = el('div', { class: 'field' },
    el('label', { for: 'fb-comment' }, 'General feeling, things to add or improve, bugs (required)'),
    el('p', { class: 'hint' }, 'At least 50 characters of real text; whitespace does not count.'),
    comment, count);
  form.appendChild(commentField);

  // tally is the field's whole validation surface: it runs on every keystroke, so the
  // requirement is never a surprise at submit time. `demand` is the submit-time voice.
  const tally = (demand) => {
    const n = meaningfulLen(comment.value);
    count.textContent = n >= 50 ? `${n} characters — long enough.`
      : demand ? `Please write at least 50 characters of real text; this is ${n}.`
        : `${n} of 50 characters.`;
    count.classList.toggle('short', n < 50);
    return n;
  };
  comment.addEventListener('input', () => tally(false));
  tally(false);

  const status = el('p', { class: 'field-error', role: 'alert', hidden: true, 'data-testid': 'fb-error' });
  const submit = el('button', { type: 'submit', class: 'primary', 'data-testid': 'fb-submit' },
    'Send feedback');
  form.appendChild(el('div', { class: 'actions' }, submit, status));

  // Assigned rather than added: "Send more feedback" rebuilds this form on the same
  // element, and a second addEventListener there would post the next submission twice.
  form.onsubmit = async (ev) => {
    ev.preventDefault();
    status.hidden = true;
    const scores = {};
    let firstBad = null;
    if (!agent.value) {
      fieldError(agentField, 'Please say which agent this is about.');
      firstBad = agentField;
    } else {
      fieldError(agentField, '');
    }
    for (const fs of $$('.stars-field', form)) {
      const chosen = fs.querySelector('input:checked');
      if (!chosen) {
        fieldError(fs, 'Please give this a rating.');
        if (!firstBad) firstBad = fs;
        continue;
      }
      fieldError(fs, '');
      scores[fs.dataset.dim] = Number(chosen.value);
    }
    if (tally(true) < 50 && !firstBad) firstBad = commentField;
    if (firstBad) {
      // Move to the first problem rather than reporting all of them at the bottom.
      const focusable = firstBad.querySelector('input,textarea,select');
      if (focusable) focusable.focus();
      firstBad.scrollIntoView({ block: 'center', behavior: 'smooth' });
      return;
    }

    submit.disabled = true;
    submit.textContent = 'Sending…';
    try {
      await ctl('/api/feedback', {
        method: 'POST',
        body: JSON.stringify({ agent: agent.value, scores, comment: comment.value }),
      });
      feedbackThanks(form);
      if (isManager()) loadFeedbackAdmin();
    } catch (e) {
      // The server's message names the rule it enforced; passing it through beats a
      // generic "something went wrong" that says less than the thing it replaced.
      status.textContent = e.message;
      status.hidden = false;
      submit.disabled = false;
      submit.textContent = 'Send feedback';
    }
  };
}

/** After a successful submit the form is replaced by what happened, and one way back. */
function feedbackThanks(form) {
  clear(form);
  form.dataset.built = '';
  form.appendChild(el('div', { class: 'banner ok', 'data-testid': 'fb-thanks', role: 'status' },
    'Thank you — that is stored here and on its way to the manager by email.'));
  form.appendChild(el('div', { class: 'actions' },
    el('button', {
      type: 'button', class: 'ghost', 'data-testid': 'fb-again',
      onclick: () => loadFeedback(),
    }, 'Send more feedback')));
}

// ── the manager's aggregate ────────────────────────────────────────────────

/**
 * distBars is a five-bucket histogram of one question's answers, one hue.
 *
 * One measure (how many people) across five ordered buckets is ONE colour: five colours
 * would imply five different things are being plotted. aria-hidden because the same
 * numbers are in the five columns beside it — the picture is for scanning, the columns
 * are the accessible reading.
 */
function distBars(dist) {
  const max = Math.max(...dist, 1);
  return el('div', { class: 'dist', 'aria-hidden': 'true' },
    ...dist.map((n, i) => el('span', {
      class: 'dist-col', title: `${i + 1}★: ${n}`,
    }, el('i', { style: 'height:' + Math.max(2, Math.round((n / max) * 100)) + '%' }))));
}

/** meanBar is a 0–5 track with the value beside it, so the number is never colour-only. */
function meanBar(mean) {
  return el('div', { class: 'mean-cell' },
    el('div', { class: 'bar-track' },
      el('div', { class: 'bar-fill', style: 'width:' + (mean / 5) * 100 + '%' })),
    el('span', { class: 'mean-val', text: mean.toFixed(2) }));
}

/**
 * npsBar renders the recommend question as its three states.
 *
 * Status colours, not series colours: promoter / passive / detractor is a judgement
 * about a state, which is exactly what --good / --warn / --bad are reserved for. Each
 * segment is also labelled in words below, because a status must never be colour alone.
 */
function npsBar(host, nps) {
  clear(host);
  // nps.n, not nps.N: the wire is JSON, and Go's tag lowercases it. This read the
  // uppercase field at first, so the panel said "nobody has answered" beside a table
  // reporting eight answers.
  if (!nps || !nps.n) {
    emptyState(host, 'Nobody has answered the recommend question yet', '');
    return;
  }
  const parts = [
    ['promoters', 'Promoters (5★)', nps.promoters, 'good'],
    ['passives', 'Passives (4★)', nps.passives, 'warn'],
    ['detractors', 'Detractors (1–3★)', nps.detractors, 'bad'],
  ];
  host.appendChild(el('div', { class: 'nps-head' },
    el('span', { class: 'nps-score', text: (nps.score > 0 ? '+' : '') + nps.score.toFixed(0) }),
    el('span', { class: 'muted small', text: `NPS from ${nps.n} answer${nps.n === 1 ? '' : 's'}` })));
  host.appendChild(el('div', { class: 'nps-bar' }, ...parts
    .filter(([, , n]) => n > 0)
    .map(([key, , n, cls]) => el('div', {
      class: 'nps-seg ' + cls, style: 'flex:' + n, 'data-testid': 'nps-' + key,
    }))));
  host.appendChild(el('div', { class: 'nps-legend' }, ...parts.map(([, label, n, cls]) =>
    el('span', {}, el('i', { class: 'sw ' + cls }), `${label}: ${n}`))));
}

/** starText renders a stored rating as filled and empty stars plus its number. */
function starText(v) {
  return el('span', { class: 'star-read', title: `${v} of 5` },
    el('span', { class: 'on', text: '★'.repeat(v) }),
    el('span', { class: 'off', text: '★'.repeat(5 - v) }),
    el('span', { class: 'vh', text: ` ${v} of 5` }));
}

/**
 * loadFeedbackAdmin fills the manager's three areas.
 *
 * Carries the tenant filter when one is set, so drilling in from the Tenants roster
 * narrows this view too. The server answers 403 to anyone who is not a manager whatever
 * this sends, so the parameter is a filter and never a permission.
 */
async function loadFeedbackAdmin() {
  const body = $('#feedback-dims-body');
  const answers = $('#feedback-answers');
  loadingRows(body, 9);
  loadingState(answers);
  try {
    const q = state.filter.tenant ? '?tenant=' + encodeURIComponent(state.filter.tenant) : '';
    const out = await ctl('/api/feedback' + q);
    // The wording for a stored key comes with the data, so this view labels a question the
    // way it was asked even before anybody has opened the form.
    feedbackForm.questions = out.questions || feedbackForm.questions;
    feedbackForm.agents = out.agents || feedbackForm.agents;
    const sum = out.summary || {};
    renderFeedbackTiles(sum);
    renderFeedbackAgents(sum);
    $('#feedback-count').textContent = `${sum.n || 0} submission${sum.n === 1 ? '' : 's'}` +
      (state.filter.tenant ? ' from the selected account' : '');

    clear(body);
    const dims = sum.dimensions || [];
    if (!dims.length) {
      tableMessage(body, 9, 'No feedback yet',
        'The form above is what fills this in. Nothing is seeded.');
    }
    for (const d of dims) {
      body.appendChild(el('tr', { 'data-testid': 'dim-' + d.dimension },
        el('td', {}, dimLabel(d.dimension)),
        el('td', {}, meanBar(d.mean)),
        el('td', {}, distBars(d.dist)),
        ...d.dist.map((n) => el('td', { class: 'num', text: String(n) })),
        el('td', { class: 'num', text: String(d.n) })));
    }

    npsBar($('#feedback-nps'), sum.nps);
    lineChart($('#chart-feedback'), [{
      name: 'Mean overall', color: SERIES[0], area: true,
      points: (sum.trend || []).map((p) => [p.day, p.mean]),
    }], { yFmt: (v) => v.toFixed(1), yMax: 5, label: 'mean overall stars per day' });

    renderFeedbackAnswers(answers, out.submissions || []);
  } catch (e) {
    clear(body);
    tableMessage(body, 9, 'Could not read the feedback', String(e.message || e), { error: true });
    errorState(answers, 'Could not read the feedback', e);
  }
}

function renderFeedbackTiles(sum) {
  const host = clear($('#feedback-tiles'));
  const overall = (sum.dimensions || []).find((d) => d.dimension === 'overall');
  const nps = sum.nps || {};
  host.appendChild(tileGroup(null, null, [
    tile('fb-count', 'Submissions', num(sum.n || 0), 'stars plus written answers'),
    tile('fb-overall', 'Mean overall', overall ? overall.mean.toFixed(2) + ' / 5' : '—',
      overall ? `${overall.n} answered` : 'nobody has answered yet',
      overall ? (overall.mean >= 4 ? 'good' : overall.mean < 3 ? 'bad' : '') : ''),
    tile('fb-nps', 'NPS', nps.n ? (nps.score > 0 ? '+' : '') + nps.score.toFixed(0) : '—',
      nps.n ? `${nps.promoters} promoters, ${nps.detractors} detractors` : 'no answers yet',
      nps.n ? (nps.score > 0 ? 'good' : nps.score < 0 ? 'bad' : '') : ''),
    // A relay that stopped working is otherwise only visible in the server log.
    tile('fb-unmailed', 'Not emailed', num(sum.unmailed || 0),
      sum.unmailed ? 'stored here, never left the relay' : 'every copy was delivered',
      sum.unmailed ? 'bad' : ''),
  ], 'headline'));
}

/**
 * renderFeedbackAgents is the reason the form asks which agent it is about: the same
 * headline numbers, read per agent, so "compaction is fine" and "compaction is not fine"
 * do not average each other away.
 *
 * The declared agents first, then anything else stored (rows from before the selector
 * existed carry no agent at all), so the order does not move as the numbers do.
 */
function renderFeedbackAgents(sum) {
  const body = clear($('#feedback-agents-body'));
  const by = sum.by_agent || {};
  const keys = feedbackForm.agents.map((a) => a.key).filter((k) => by[k])
    .concat(Object.keys(by).filter((k) => !feedbackForm.agents.some((a) => a.key === k)).sort());
  if (!keys.length) {
    tableMessage(body, 4, 'No feedback yet', 'The form above is what fills this in.');
    return;
  }
  for (const k of keys) {
    const s = by[k];
    const overall = (s.dimensions || []).find((d) => d.dimension === 'overall');
    const nps = s.nps || {};
    body.appendChild(el('tr', { 'data-testid': 'agent-' + (k || 'none') },
      el('td', {}, agentLabel(k)),
      el('td', { class: 'num', text: String(s.n || 0) }),
      el('td', {}, overall ? meanBar(overall.mean) : el('span', { class: 'muted', text: '—' })),
      el('td', {
        class: 'num',
        text: nps.n ? (nps.score > 0 ? '+' : '') + nps.score.toFixed(0) : '—',
      })));
  }
}

/**
 * renderFeedbackAnswers lists every submission verbatim.
 *
 * Every string here was typed by a user, so every one of them lands through el() and
 * textContent. Nothing on this page concatenates markup — el() throws on raw html, which
 * is what makes that a property of the page rather than a habit.
 */
function renderFeedbackAnswers(host, rows) {
  clear(host);
  if (!rows.length) {
    emptyState(host, 'No written feedback yet',
      'The form above is the only thing that writes here.');
    return;
  }
  for (const fb of rows) {
    const scores = fb.scores || {};
    // Asked-order, not alphabetical: the chips read like the form somebody filled in.
    const chips = feedbackForm.questions.filter((q) => scores[q.key])
      .map((q) => el('span', { class: 'score-chip' },
        el('span', { class: 'score-dim', text: q.label }), starText(scores[q.key])));
    host.appendChild(el('article', { class: 'answer', 'data-testid': 'answer-' + fb.id },
      el('header', { class: 'answer-head' },
        el('strong', { text: fb.email || 'unknown account' }),
        fb.label ? el('span', { class: 'muted small', text: fb.label }) : null,
        el('span', { class: 'pill', 'data-testid': 'answer-agent-' + fb.id },
          agentLabel(fb.agent)),
        el('span', { class: 'muted small', text: when(fb.created_at) }),
        fb.mailed_at
          ? el('span', { class: 'pill complete' }, 'emailed')
          : el('span', { class: 'pill missing' }, 'not emailed')),
      el('div', { class: 'score-chips' }, ...chips),
      el('div', { class: 'answer-block' },
        el('h4', {}, 'Comment'), el('p', { text: fb.comment }))));
  }
}

// Registered here rather than in the shared Object.assign above, so this whole feature
// is one appended block and the view table above stays untouched.
Object.assign(loaders, { feedback: loadFeedback });
