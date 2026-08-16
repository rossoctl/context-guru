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
  range: 0,
  reqCursor: 0,
  reqStack: [],
  sessOffset: 0,
  live: [],
  overview: null,
  // What the drawer is showing, and part of the URL: {req: id} | {diff: session} | null.
  drawer: null,
  // ac aborts every fetch belonging to the PREVIOUS filter state. Without it a slow
  // response for "agent=bob" could land after a fast one for "agent=bob + preset=x"
  // and repaint the table with rows that do not match the filter bar.
  ac: null,
};

function qs(extra) {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(state.filter)) if (v) p.set(k, v);
  if (state.range > 0) p.set('since', String(Date.now() - state.range));
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
  const yMin = Math.min(0, ...ys), yMax = Math.max(...ys) || 1;
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

/** barRows(host, rows) — rows: [{label, value, display, max, negative, desc}] */
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
    wrap.appendChild(row);
    if (r.desc) wrap.appendChild(el('div', { class: 'bar-desc', text: r.desc }));
  }
  host.appendChild(wrap);
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
  host.appendChild(tileGroup(null, null, [
    tile('saved-usd', 'Net dollars saved', costKnown ? usd(o.net_saved_usd) : 'unknown',
      'baseline − actual − our own spend', costKnown ? (o.net_saved_usd < 0 ? 'bad' : 'good') : ''),
    tile('saved-unique', 'Tokens saved (unique)', compact(o.saved_unique),
      'each compaction counted once', 'accent'),
    tile('requests', 'Requests', num(o.requests), num(o.sessions) + ' sessions'),
  ], 'headline'));

  host.appendChild(tileGroup('Content tokens', 'what the pipeline removed, three ways of counting it', [
    tile('tokens-before', 'Tokens before', compact(o.tokens_before), 'content tokens in'),
    tile('tokens-after', 'Tokens after', compact(o.tokens_after), 'content tokens out'),
    tile('saved-gross', 'Saved (gross)', compact(o.saved_gross), 'recounts re-sent history'),
    // The label has to name the UNIQUE calculation, which dominates this figure, and not
    // only the restore subtraction, which is usually zero: sitting between "Saved (gross)
    // 17k" and "Overcount 1.7×", "Saved (net of restores)" invited reading it as
    // gross-minus-restores and made a 10k number look like an arithmetic error.
    tile('saved-adjusted', 'Saved (unique, less restores)', compact(o.saved_adjusted),
      'unique − ' + compact(o.expand_tokens) + ' restored back', o.saved_adjusted < 0 ? 'bad' : ''),
    tile('overcount', 'Overcount ratio', o.overcount_ratio ? o.overcount_ratio.toFixed(1) + '×' : '—',
      'gross ÷ unique'),
  ]));

  host.appendChild(tileGroup('Cost', costKnown ? 'billed, and the counterfactual' : 'no priced requests in this window', [
    tile('cost-baseline', 'Baseline cost', costKnown ? usd(o.baseline_cost_usd) : 'unknown',
      costKnown ? 'without context-guru' : 'needs all four token tiers'),
    tile('cost-actual', 'Actual cost', costKnown ? usd(o.cost_usd) : 'unknown',
      costKnown ? 'as billed' : 'needs all four token tiers'),
    tile('cost-cg', "context-guru's own LLM", costKnown ? usd(o.cg_llm_cost_usd) : 'unknown',
      'our components’ model spend'),
  ]));

  host.appendChild(tileGroup('Billed tokens', 'the four tiers the provider charges on', [
    tile('cache-read', 'Cache reads', compact(o.cache_read), 'billed at the read rate'),
    tile('cache-write', 'Cache writes', compact(o.cache_write), '~11.5× a read'),
    tile('fresh-input', 'Fresh input', compact(o.fresh_input), 'uncached new tokens'),
    tile('output', 'Output tokens', compact(o.output_tokens), 'completions'),
  ]));

  host.appendChild(tileGroup('Latency and safety', 'what compaction cost to get', [
    tile('cg-latency', 'context-guru latency', ms(o.cg_latency_ms_avg), 'p95 ' + ms(o.cg_latency_ms_p95)),
    tile('upstream-latency', 'Upstream latency', ms(o.upstream_ms_avg), 'p95 ' + ms(o.upstream_ms_p95)),
    tile('expands', 'Restorations', num(o.expands),
      pct(o.expand_rate * 100) + ' of requests · ' + compact(o.expand_tokens) + ' tok',
      o.expands > 0 ? 'bad' : ''),
    tile('reverts', 'Reverts', num(o.reverts), 'never-worse guard fired'),
    tile('passthroughs', 'Not compacted', num(o.passthroughs), 'see reason buckets below'),
  ]));
}

function renderDenominators(o) {
  barRows($('#denominators'), (o.denominators || []).map((d) => ({
    label: d.label,
    value: d.available ? d.percent : 0,
    max: 100,
    display: d.available ? pct(d.percent, 2) : 'n/a',
    available: d.available,
    desc: d.description + (d.available ? `  (${compact(d.numerator)} ÷ ${compact(d.denominator)} tokens)` : ''),
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
    wrap.appendChild(el('div', { class: 'bar-desc', text: s.description }));
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
    { label: 'Frozen for cache safety', value: s.frozen_tokens || 0, display: compact(s.frozen_tokens) + ' tok',
      desc: 'Compaction we deliberately did NOT do on the already-cached prefix. The benefit ' +
            'is the ' + compact(o.cache_read) + ' cache-read tokens that stayed cheap; the cost is this.' },
    { label: 'Restored after offload', value: s.restored_tokens || 0, display: compact(s.restored_tokens) + ' tok',
      color: 'var(--bad)',
      desc: 'Content we removed and the model asked back for — a premature offload, paid for twice.' },
    { label: 'Reverted component runs', value: s.reverted_runs || 0, display: num(s.reverted_runs) + ' runs',
      desc: 'The never-worse guard rolling a component back. Safety working, and its cost is the ' +
            'latency of the attempt.' },
    { label: "context-guru's own latency", value: s.cg_latency_ms_total || 0, display: dur(s.cg_latency_ms_total),
      desc: 'Total wall time context-guru itself added across the window.' },
    { label: "context-guru's own LLM spend", value: (s.cg_llm_cost_usd || 0) * 1000, display: usd(s.cg_llm_cost_usd),
      desc: 'Paid out of the savings above.' },
  ]);
}

function renderLive() {
  const body = clear($('#live-body'));
  // The feed is the raw capture stream: it is not filtered, and with a filter bar right
  // above it that is the sort of disagreement a reader blames on the numbers.
  const note = $('#live-filter-note');
  const active = activeFilters();
  note.hidden = !active.length;
  if (active.length) {
    note.textContent = 'Not filtered: this feed shows every request captured, including ones ' +
      'outside ' + describeFilters() + '. Everything else on this page is filtered.';
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
    renderDistribution('#reasons', o.uncompressed, {
      '': 'compacted', bypassed: 'bypassed by header', below_trigger: 'below every trigger',
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
  if (state.range === 0) return 3600000;
  if (state.range <= 3600000) return 60000;
  if (state.range <= 86400000) return 300000;
  return 3600000;
}

function renderSeries(buckets) {
  if (!buckets.length) {
    for (const id of ['#chart-cost', '#chart-tokens', '#chart-cache', '#chart-latency', '#chart-volume']) {
      emptyState($(id), 'No data in this window', 'Send traffic through the proxy, or widen the time range.');
    }
    return;
  }
  // Cumulative cost: the headline chart. The area between the lines is the money.
  let cumBase = 0, cumAct = 0;
  const base = [], act = [];
  for (const b of buckets) {
    cumBase += b.baseline_cost_usd;
    cumAct += b.cost_usd + b.cg_llm_cost_usd;
    base.push([b.ts, cumBase]);
    act.push([b.ts, cumAct]);
  }
  const anyCost = cumBase > 0 || cumAct > 0;
  if (anyCost) {
    lineChart($('#chart-cost'), [
      { name: 'Without context-guru (cumulative)', color: SERIES[1], points: base },
      { name: 'With context-guru (incl. our own spend)', color: SERIES[0], points: act, area: true },
    ], { band: true, yFmt: usd, tipFmt: usd, label: 'cumulative cost with and without context-guru' });
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
  // Spent >1s of hot-path time and returned nothing: paid for, unused.
  if (c.saved_unique === 0 && c.duration_ms_total > 1000) return ['costly and inert', 'missing'];
  if (c.mutated === 0) return ['inert here', 'partial'];
  if (c.saved_unique === 0) return ['mutates, saves no content', 'neutral'];
  // More than a millisecond of latency per 100 tokens saved.
  if (c.duration_ms_total > 1000 && c.duration_ms_total / c.saved_unique > 0.01) {
    return ['expensive for its yield', 'partial'];
  }
  if (c.act_rate < 0.02) return ['rarely fires', 'partial'];
  return ['earning its place', 'complete'];
}

async function loadComponents() {
  const body = clear($('#components-body'));
  loadingRows(body, 13);
  try {
    const { components } = await api('components');
    clear(body);
    if (!components.length) {
      tableMessage(body, 13, 'No component runs captured',
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
        el('td', { class: 'num', text: num(c.acted) }),
        el('td', { class: 'num', text: pct(c.act_rate * 100, 1) }),
        el('td', { class: 'num', text: num(c.reverted) }),
        el('td', { class: 'num', text: compact(c.saved_unique) }),
        el('td', { class: 'num', text: compact(c.saved_gross) }),
        el('td', { class: 'num', text: c.overcount_ratio ? c.overcount_ratio.toFixed(1) + '×' : '—' }),
        el('td', { class: 'num', text: dur(c.duration_ms_total) }),
        el('td', { class: 'num', text: ms(c.duration_ms_avg) }),
        el('td', { class: 'num', text: num(c.errors) }),
        el('td', {}, el('span', { class: 'pill ' + vcls, text: vtext }))));
    }
    // One measure (unique tokens saved) across up to twelve components: a magnitude
    // comparison, so ONE hue. Colouring bar N by N implied each component was a
    // different series and repainted them all whenever a filter changed the order.
    const top = components.filter((c) => c.saved_unique > 0).slice(0, 12);
    barRows($('#chart-comp'), top.map((c) => ({
      label: c.component, value: c.saved_unique, display: compact(c.saved_unique) + ' tok',
      desc: `${num(c.runs)} runs, acted on ${pct(c.act_rate * 100, 1)}, own latency ${dur(c.duration_ms_total)}, ` +
            `overcount ${c.overcount_ratio ? c.overcount_ratio.toFixed(1) + '×' : 'n/a'}`,
    })), { emptyDetail: 'No component saved any content tokens in this window.' });
  } catch (err) {
    if (aborted(err)) return;
    tableMessage(body, 13, 'Could not load components', String(err.message || err), { error: true });
  }
}

// ── sessions ───────────────────────────────────────────────────────────────
async function loadSessions() {
  const body = clear($('#sessions-body'));
  loadingRows(body, 13);
  try {
    const { sessions, total } = await api('sessions', { limit: 25, offset: state.sessOffset });
    clear(body);
    if (!sessions.length) {
      if (activeFilters().length) renderNoMatch(body, 13, 'sessions');
      else {
        tableMessage(body, 13, 'No sessions yet',
          'A session appears as soon as its first request is captured.');
      }
    }
    for (const s of sessions) {
      body.appendChild(el('tr', { class: 'click', onclick: () => { setFilter('session', s.session_id, { quiet: true }); go('requests'); } },
        el('td', {}, el('span', { class: 'trunc', title: s.session_id, text: s.session_id || '(none)' })),
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
    tableMessage(body, 13, 'Could not load sessions', String(err.message || err), { error: true });
  }
}

// ── requests ───────────────────────────────────────────────────────────────
async function loadRequests() {
  const body = clear($('#requests-body'));
  loadingRows(body, 13, 6);
  try {
    const page = await api('requests', { limit: 50, before: state.reqCursor });
    clear(body);
    if (!page.requests.length) {
      renderNoMatch(body, 13, 'requests');
    }
    for (const e of page.requests) {
      body.appendChild(el('tr', { class: 'click', 'data-testid': 'request-row', onclick: () => openRequest(e.id) },
        el('td', { text: e.id }),
        el('td', { text: when(e.ts) }),
        el('td', {}, el('span', { class: 'trunc', title: e.session_id, text: e.session_id || '—' })),
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
    tableMessage(body, 13, 'Could not load requests', String(err.message || err), { error: true });
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

    body.appendChild(el('div', { class: 'kv', 'data-testid': 'detail-summary' },
      kv('Session', e.session_id || '—'),
      kv('When', when(e.ts)),
      kv('Model', e.model || '—'),
      kv('Provider', e.provider || '—'),
      kv('Agent', e.agent || '—'),
      kv('Preset', e.preset || '—'),
      kv('Mode', modeLabel(e.mode)),
      kv('Upstream status', e.status || '—'),
      kv('Messages', num(e.messages)),
      kv('Tokens before → after', compact(e.tokens_before) + ' → ' + compact(e.tokens_after)),
      kv('Saved (gross / unique)', compact(e.tokens_before - e.tokens_after) + ' / ' + compact(e.saved_unique)),
      kv('Attempted (eligible)', compact(e.attempted_tokens)),
      kv('Frozen for cache safety', compact(e.frozen_tokens)),
      kv('Fresh / read / write / out',
        [e.fresh_input, e.cache_read, e.cache_write, e.output_tokens].map(compact).join(' / ')),
      kv('Cost (actual / baseline)', e.token_accounting === 'complete'
        ? usd(e.cost_usd) + ' / ' + usd(e.baseline_cost_usd) : 'not priced'),
      kv("context-guru's own LLM", e.token_accounting === 'complete' ? usd(e.cg_llm_cost_usd) : '—'),
      kv('context-guru latency', ms(e.cg_latency_ms)),
      kv('Upstream latency', ms(e.upstream_ms)),
      kv('Restorations', num(e.expands) + ' (' + compact(e.expand_tokens) + ' tok)'),
      kv('Reverts', num(e.reverts)),
      kv('Cache attribution', e.cache_miss_reason || '—'),
      kv('Token accounting', e.token_accounting),
      kv('Compaction outcome', e.uncompressed_reason || 'compacted')));

    body.appendChild(el('h2', { text: 'Components, in the order they ran' }));
    if (!e.components || !e.components.length) {
      body.appendChild(el('div', { class: 'empty', text: 'No components ran on this request.' }));
    } else {
      const tbl = el('table', { class: 'tbl compact', 'data-testid': 'detail-components' },
        el('thead', {}, el('tr', {},
          el('th', { text: '#' }), el('th', { text: 'Component' }), el('th', { text: 'Kind' }),
          el('th', { class: 'num', text: 'Saved' }), el('th', { class: 'num', text: 'Unique' }),
          el('th', { class: 'num', text: 'Latency' }), el('th', { text: 'Outcome' }))));
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
            c.err ? el('div', { class: 's', text: c.err }) : null)));
      });
      tbl.appendChild(tb);
      body.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, tbl));
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
        'Metrics for this traffic are visible; its content is not. On a hosted deployment only ' +
        'the owning account can read its own transcripts — a manager cannot, because reading ' +
        "someone else's source code is not an administrative need. On a single-tenant proxy, " +
        'content is served to loopback or a configured trusted CIDR only.' }));
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
  try {
    // The payload is {scope, config, description} — the same envelope /api/capture uses.
    // Rendering the envelope as the tree printed "scope: server" and "description: …" as
    // if they were configuration keys, and buried the config a level down.
    const cfg = await api('config');
    clear(host);
    host.appendChild(el('p', { class: 'note', 'data-testid': 'config-scope', text:
      'Scope: ' + (cfg.scope || 'server') + ' — this is the configuration THIS PROXY is ' +
      'running, which is not necessarily what compacted your traffic: on a hosted ' +
      'deployment each account may run its own pipeline (see Settings). ' +
      (cfg.description || '') }));
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
    chost.appendChild(el('p', { class: 'note', text: description }));
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
  overview: loadOverview, components: loadComponents, sessions: loadSessions,
  requests: loadRequests, benchmarks: loadBenchmarks, config: loadConfig,
};

/**
 * DIMS is every filter dimension, and it is the single list the whole filter layer
 * reads: the URL, the chips, the facet dropdowns and the "why is this empty" copy.
 *
 * The third column is the control, where one exists. `session` and `tenant` have NO
 * control — they are set by drilling in from the Sessions or Tenants table — and that
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
  ['session', 'session', null],
  ['tenant', 'tenant', null],
];
/** The facet dimensions the server can enumerate; the rest are fixed option lists. */
const FACET_DIMS = ['model', 'provider', 'agent', 'preset', 'mode', 'component'];

/** activeFilters lists the set filters as [key, label, value], time range included. */
function activeFilters() {
  const out = DIMS.filter(([k]) => state.filter[k]).map(([k, label]) => [k, label, state.filter[k]]);
  if (state.range > 0) out.push(['range', 'range', rangeLabel()]);
  return out;
}
function rangeLabel() {
  const opt = $('#f-range').selectedOptions[0];
  return (opt ? opt.textContent : String(state.range)).toLowerCase();
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
  syncURL(!push);
  refresh();
}

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
function setRange(msWindow) {
  state.range = Number(msWindow) || 0;
  $('#f-range').value = String(state.range);
  resetPaging();
  syncURL();
  refresh();
}
function clearFilters() {
  state.filter = {};
  state.range = 0;
  for (const [k] of DIMS) syncControl(k);
  $('#f-range').value = '0';
  resetPaging();
  syncURL();
  refresh();
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
  if (state.range > 0) p.set('range', String(state.range));
  // The open drawer is state too, so a request and a session diff are both linkable and
  // Back dismisses the drawer rather than undoing the last filter change — which was the
  // one thing that made Back dangerous here. `diff` rather than `session` because
  // `session` is already a filter dimension and they are not the same thing.
  if (state.drawer && state.drawer.req) p.set('req', String(state.drawer.req));
  if (state.drawer && state.drawer.diff) p.set('diff', state.drawer.diff);
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
  return {
    view: view || 'overview', filter, range: Number(p.get('range')) || 0,
    drawer: req ? { req } : diff ? { diff } : null,
  };
}
/** applyURL makes the page match the address bar. Used on load, on Back/Forward, and
 *  when someone edits the hash by hand — one reader for all three. */
function applyURL() {
  const want = parseURL();
  state.filter = want.filter;
  state.range = want.range;
  // state.drawer is adopted BEFORE go(), because go() calls syncURL(replace) and would
  // otherwise rewrite the entry we just navigated to with the drawer we are leaving.
  const prev = state.drawer;
  state.drawer = want.drawer;
  for (const [k] of DIMS) syncControl(k);
  $('#f-range').value = String(state.range);
  resetPaging();
  go(want.view, false);
  syncDrawer(prev);
}

/** syncDrawer makes the drawer match state.drawer, which the address bar has just set:
 *  Back closes it, Forward and a pasted link open it. Nothing here touches history. */
function syncDrawer(prev) {
  const want = state.drawer;
  if (!want) { if (!$('#drawer').hidden) dismissDrawer(); return; }
  const same = !!prev && prev.req === want.req && prev.diff === want.diff;
  if (same && !$('#drawer').hidden) return;
  if (want.req) openRequest(want.req, true);
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
  const baseRange = state.range;
  const counts = await Promise.all(active.map(async ([key]) => {
    const p = new URLSearchParams();
    for (const [k, v] of Object.entries(base)) if (v && k !== key) p.set(k, v);
    if (baseRange > 0 && key !== 'range') p.set('since', String(Date.now() - baseRange));
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
  if (state.range > 0) uni.set('since', String(Date.now() - state.range));
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
  $('#f-range').addEventListener('change', (ev) => setRange(ev.currentTarget.value));
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
  setInterval(() => { if (state.view === 'overview' && !gated()) loadOverview(); }, 10000);
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
  $('#gate-signin').hidden = true;
  $('#gate-register').hidden = true;
  $('#gate-closed').hidden = true;
  $('#gate-verify').hidden = false;
  $('#gate-verify-intro').textContent = intro;
  $('#gate-verify-code').value = '';
  $('#gate-verify-code').focus();
  if (verify.tick) clearInterval(verify.tick);
  const paint = () => {
    const left = Math.max(0, Math.round((verify.expires - Date.now()) / 1000));
    const timer = $('#gate-verify-timer');
    if (!verify.expires) { timer.textContent = ''; return; }
    timer.textContent = left > 0
      ? `Expires in ${Math.floor(left / 60)}:${String(left % 60).padStart(2, '0')}`
      : 'That code has expired — start over to get a new one.';
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
  const onRegister = $('#gate-tab-register').getAttribute('aria-selected') === 'true';
  $('#gate-signin').hidden = onRegister;
  $('#gate-register').hidden = !onRegister;
  if (onRegister) applyRegisterMode();
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
}
function isManager() { return !!(account.tenant && account.tenant.role === 'manager'); }

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
const AGENTS = [
  {
    name: 'Claude Code',
    path: '/anthropic',
    lines: (base, tok) => [
      `export ANTHROPIC_BASE_URL=${base}/anthropic`,
      `export ANTHROPIC_AUTH_TOKEN=${tok}`,
    ],
  },
  {
    name: 'Bob (BobShell)',
    path: '/',
    lines: (base, tok) => [
      `export CUSTOM_BASE_URL=${base}`,
      `export BOBSHELL_API_KEY=${tok}`,
    ],
  },
  {
    name: 'OpenAI-dialect tools',
    path: '/openai/v1',
    lines: (base, tok) => [
      `export OPENAI_BASE_URL=${base}/openai/v1`,
      `export OPENAI_API_KEY=${tok}`,
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

function loadSetup() {
  const host = clear($('#setup-blocks'));
  const base = account.baseURL || location.origin;
  // The plaintext only exists at mint time, so a returning user gets a placeholder. It is
  // a NAMED slot rather than their real token's prefix plus an ellipsis: that version put
  // a credential fragment in a block whose whole purpose is being pasted and shared, and
  // copying it produced an export line that could not work.
  const tok = account.freshToken || 'cg_live_YOUR_TOKEN';
  for (const a of AGENTS) {
    const lines = a.lines(base, tok);
    const block = el('div', { class: 'setup-block' },
      el('div', { class: 'setup-head' },
        el('h3', { text: a.name }),
        copyButton(lines.join('\n'))),
      el('pre', { class: 'code' }, lines.join('\n')));
    host.appendChild(block);
  }
  const banner = $('#setup-token-banner');
  if (account.freshToken) {
    banner.hidden = false;
    banner.textContent = 'Your new token is filled in below. It is shown once and cannot ' +
      'be recovered — copy it somewhere safe now.';
  } else {
    banner.hidden = true;
  }
}

// ── settings ───────────────────────────────────────────────────────────────
function componentPickers(cfgText, opts) {
  // Parse the pipeline line out of the YAML rather than shipping a YAML parser: the one
  // field the checkboxes drive is `pipeline: [a, b, c]`, and a full parser for one line
  // would be a lot of bytes in a page that has no build step.
  const m = /^pipeline:\s*\[(.*?)\]\s*$/m.exec(cfgText || '');
  const active = new Set((m ? m[1] : '').split(',').map((s) => s.trim()).filter(Boolean));
  const all = (opts && opts.components) || [];
  return { active, all };
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
    const effective = t.effective_config_yaml || t.config_yaml || '';
    const { active, all } = componentPickers(effective, opts);

    // Spend, first: it is the thing that stops traffic, so it belongs above the knobs.
    if (t.monthly_cap_usd > 0) {
      const frac = Math.min(1, (t.spent_usd || 0) / t.monthly_cap_usd);
      host.appendChild(el('div', { class: 'spend' },
        el('div', { class: 'spend-label' },
          `Spend this month: ${usd(t.spent_usd)} of ${usd(t.monthly_cap_usd)}`),
        el('div', { class: 'meter' },
          el('i', { style: `width:${(frac * 100).toFixed(1)}%`, class: frac > 0.9 ? 'hot' : '' })),
        el('p', { class: 'hint' },
          'Requests are refused once the cap is reached. A manager can raise it.')));
    }

    // Which configuration is in force, and how to change that.
    host.appendChild(inherited
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
    modeSel.value = /^mode:\s*observe/m.test(effective) ? 'observe' : 'sync';
    modeSel.disabled = inherited;
    host.appendChild(el('div', { class: 'field' },
      el('label', { for: 'set-mode' }, 'Mode'), modeSel,
      el('p', { class: 'hint' },
        'observe forwards every request byte-for-byte and only records what compaction ' +
        'would have saved. The safe way to try a configuration.')));

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
      cb.disabled = inherited;
      const warn = name === 'extract_llm'
        ? ' — calls a compaction model on the request path (+117ms typical, up to ~945ms on file reads) and bills to the shared credential'
        : '';
      grid.appendChild(el('label', { class: 'comp', for: id }, cb,
        el('span', { class: 'comp-name' }, name),
        warn ? el('span', { class: 'comp-warn' }, warn) : null));
    }
    host.appendChild(el('div', { class: 'field' },
      el('label', {}, 'Pipeline components'), grid,
      el('p', { class: 'hint' },
        'These toggles decide what runs; the ORDER is the one in your configuration, ' +
        'which this list keeps. A newly enabled component is appended at the end — move ' +
        'it in the YAML below if it belongs earlier. Saving rebuilds your pipeline and ' +
        'discards frozen compaction decisions, so the next turn will not be cache-warm.')));

    // Content capture consent.
    const cap = el('input', {
      type: 'checkbox', id: 'set-capture', 'data-testid': 'set-capture',
    });
    cap.checked = !!t.capture_content;
    host.appendChild(el('div', { class: 'field' },
      el('label', { class: 'comp', for: 'set-capture' }, cap,
        el('span', { class: 'comp-name' }, 'Store my transcripts for the diff view')),
      el('p', { class: 'hint warn-text' },
        'Off by default. This writes your agent output — source code, tool results — to ' +
        'disk behind a best-effort redactor whose own review found 11 of 22 realistic ' +
        'credential shapes passing through it. Only you can read them; a manager cannot.')));

    // Raw YAML, for anything the toggles do not cover.
    const ta = el('textarea', {
      id: 'set-yaml', rows: 10, spellcheck: 'false', 'data-testid': 'set-yaml',
      'aria-label': 'Full configuration, YAML',
    });
    ta.value = effective;
    ta.disabled = inherited;
    host.appendChild(el('details', { class: 'field' },
      el('summary', {}, 'Full configuration (YAML)'), ta,
      el('p', { class: 'hint' }, inherited
        ? 'The server default, read-only. Customise above to edit it as your own.'
        : 'Edited here, this wins over the toggles above. Rejected on save if it does not ' +
          'build, with the offending key named.')));

    // Save covers the upstreams and the capture consent in both states; it leaves the
    // configuration alone while it is inherited (see saveSettings), so saving one of
    // those does not quietly turn a tracking account into a frozen copy.
    host.appendChild(el('div', { class: 'actions' },
      el('button', { class: 'primary', 'data-testid': 'settings-save', onclick: saveSettings }, 'Save')));
  });

  loadTokens();
  loadSessions();
  loadAudit();
}

/** The machines this account is signed in on, each revocable on its own. */
async function loadSessions() {
  const host = clear($('#session-list'));
  let rows = [];
  try {
    rows = (await ctl('/api/me/sessions')).sessions || [];
  } catch (e) { errorState(host, 'Could not list your sessions', e); return; }
  if (!rows.length) { emptyState(host, 'No sessions', 'Nothing is signed in.'); return; }
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
            loadSessions();
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

async function saveSettings() {
  const status = $('#settings-saved');
  status.textContent = 'saving…';
  // The textarea wins when the user edited it; otherwise rebuild the pipeline line from
  // the checkboxes. Two sources for one field, so the precedence has to be explicit —
  // and "what you typed beats what you clicked" is the order that never surprises.
  let yaml = $('#set-yaml').value;
  const inherited = !!account.tenant.config_inherited;
  const original = account.tenant.effective_config_yaml || '';
  if (yaml.trim() === original.trim()) {
    const picked = new Set($$('#comp-grid input[type=checkbox]')
      .filter((c) => c.checked).map((c) => c.dataset.comp));
    // Keep the configured run order for everything still enabled; a newly ticked
    // component is appended, never inserted, because this grid does not know where in
    // the pipeline it belongs.
    const prev = (/^pipeline:\s*\[(.*?)\]\s*$/m.exec(original) || ['', ''])[1]
      .split(',').map((x) => x.trim()).filter(Boolean);
    const ordered = prev.filter((n) => picked.has(n))
      .concat([...picked].filter((n) => !prev.includes(n)));
    const mode = $('#set-mode').value;
    yaml = yaml.replace(/^pipeline:.*$/m, `pipeline: [${ordered.join(', ')}]`);
    if (!/^pipeline:/m.test(yaml)) yaml = `pipeline: [${ordered.join(', ')}]\n` + yaml;
    yaml = /^mode:/m.test(yaml) ? yaml.replace(/^mode:.*$/m, `mode: ${mode}`) : yaml + `\nmode: ${mode}\n`;
  }
  const body = {
    capture_content: $('#set-capture').checked,
    up_anthropic: $('#set-up_anthropic').value,
    up_openai: $('#set-up_openai').value,
    up_bob: $('#set-up_bob').value,
  };
  // Omitted while the configuration is inherited: sending it would store a copy of
  // today's default, which is exactly the freeze this page exists to undo. Customise is
  // the deliberate way to start owning one.
  if (!inherited) body.config_yaml = yaml;
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
        el('th', {}, 'Account'), el('th', {}, 'Role'), el('th', {}, 'Spend / cap'),
        el('th', {}, 'Last seen'), el('th', {}, 'Transcripts'), el('th', {}, 'Configuration'),
        el('th', {}, el('span', { class: 'vh' }, 'Row actions')))));
    const body = el('tbody');
    for (const t of rows) {
      const over = t.monthly_cap_usd > 0 && t.spent_usd >= t.monthly_cap_usd;
      body.appendChild(el('tr', { class: t.disabled ? 'revoked' : '' },
        el('td', {}, el('div', {}, t.email), el('div', { class: 'muted small' }, t.label)),
        el('td', {}, t.role),
        el('td', { class: over ? 'warn-text' : '' },
          t.monthly_cap_usd > 0 ? `${usd(t.spent_usd)} / ${usd(t.monthly_cap_usd)}` : usd(t.spent_usd)),
        el('td', {}, t.last_seen_at ? when(t.last_seen_at) : el('span', { class: 'muted' }, 'never')),
        // The EFFECTIVE answer to "are this account's transcripts being kept", which is
        // the AND of both gates — never the account's flag on its own.
        el('td', {}, captureCell(t.capture_content, operatorGate)),
        // The EFFECTIVE first line, plus whose it is: an account that follows the
        // default stores nothing, and an empty cell would read as a broken account.
        el('td', {},
          el('code', { class: 'clip' }, (t.effective_config_yaml || '').split('\n')[0] || '—'),
          t.config_inherited ? el('div', { class: 'muted small' }, 'server default') : null),
        el('td', {}, el('div', { class: 'row-actions' },
          el('button', {
            class: 'ghost small', onclick: () => showTenantMetrics(t.id),
          }, 'Metrics'),
          el('button', {
            class: 'ghost small', 'data-testid': 'cap-' + t.id,
            onclick: async () => {
              const v = prompt(`Monthly cap in USD for ${t.email} (0 = uncapped):`, String(t.monthly_cap_usd));
              if (v === null) return;
              const n = Number(v);
              if (!Number.isFinite(n) || n < 0) { alert('Not a valid amount.'); return; }
              try { await ctl('/api/tenants/' + t.id, { method: 'PATCH', body: JSON.stringify({ monthly_cap_usd: n }) }); loadTenants(); } catch (e) { alert(e.message); }
            },
          }, 'Set cap'),
          el('button', {
            class: 'ghost small', 'data-testid': 'toggle-' + t.id,
            onclick: async () => {
              const msg = t.disabled
                ? `Re-enable ${t.email}?`
                : `Disable ${t.email}? Their agents stop working immediately and they are signed out.`;
              if (!confirm(msg)) return;
              try { await ctl('/api/tenants/' + t.id, { method: 'PATCH', body: JSON.stringify({ disabled: !t.disabled }) }); loadTenants(); } catch (e) { alert(e.message); }
            },
          }, t.disabled ? 'Enable' : 'Disable'),
          el('button', {
            class: 'ghost small',
            onclick: async () => {
              if (!confirm(`Mint a replacement token for ${t.email}? Hand it over on a channel you trust.`)) return;
              try {
                const out = await ctl('/api/tenants/' + t.id + '/tokens', { method: 'POST', body: JSON.stringify({ label: 'reissued' }) });
                // Shown once, so it goes in a place the manager must acknowledge.
                prompt('Copy this token now — it cannot be recovered:', out.token);
              } catch (e) { alert(e.message); }
            },
          }, 'Reissue token')))));
    }
    tbl.appendChild(body);
    host.appendChild(tbl);
  } catch (e) {
    clear(host);
    errorState(host, 'Could not list tenants', e);
  }
}

/** Jump to this tenant's traffic. Managers get ?tenant= on every read route, so the
 *  existing views work unchanged once the filter is set. */
function showTenantMetrics(id) {
  // quiet: go() below pushes the URL and refetches once. Setting it loudly here would
  // fetch the current view with the new scope and then immediately fetch Overview.
  setFilter('tenant', id, { quiet: true });
  go('overview');
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
    $('#gate-signin').hidden = false; $('#gate-register').hidden = true;
    $('#gate-closed').hidden = true;
    $('#gate-tab-signin').setAttribute('aria-selected', 'true');
    $('#gate-tab-register').setAttribute('aria-selected', 'false');
    gateError('');
  });
  $('#gate-tab-register').addEventListener('click', () => {
    cancelVerify();
    $('#gate-signin').hidden = true;
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
