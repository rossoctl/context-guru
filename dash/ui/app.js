// context-guru dashboard. No framework, no build step, no CDN — the page is a few
// plain files in a Go binary. This file is the shell: the DOM helpers, formatting, state,
// charts, the router and the account gate. Each tab lives in its own file under tabs/,
// which index.html loads after this one (#1089). State is a plain object, rendering is
// direct DOM writes, and charts are hand-drawn SVG. Everything that reads provider- or
// agent-supplied text goes through textContent or el(), never innerHTML, because a tool
// output in a transcript is attacker-influenced content (gateway interpolates it; we do not).
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
/* neverCalled is the server's own test of "never used" (dash: SessionsUsed == 0 && Calls == 0), the one
 * rule every "unused" filter, row class and pill on the dashboard uses. sessions_used alone is not enough:
 * a tool called in a session that did not declare it has sessions_used 0 and calls > 0. */
function neverCalled(t) { return !t.sessions_used && !t.calls; }
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

/*
 * moneyUnit is the unit of every money figure on the page. A view has one unit, and the API says
 * which in `currency` (dash/currency.go): USD, the name a price file gives (Bob's says Bobcoins), or
 * 'mixed' when the view sums more than one unit and so has no money total.
 */
let moneyUnit = 'USD';
const MONEY_UNIT_RE = /^[A-Za-z]{1,16}$/;
// The name comes from a price file. The server lets only letters through; a name that is not plain
// letters is refused here as well and read as mixed, so it hides money rather than reach the page.
function setMoneyUnit(u) {
  moneyUnit = typeof u === 'string' && MONEY_UNIT_RE.test(u) ? u : 'mixed';
}
function usd(v) {
  if (v === null || v === undefined) return '—';
  if (moneyUnit === 'mixed') return '—';
  const a = Math.abs(v);
  if (moneyUnit !== 'USD') {
    const n = a === 0 ? '0' : a < 0.01 ? a.toFixed(4) : a < 1000 ? a.toFixed(2) : nf.format(Math.round(a));
    return (v < 0 ? '-' : '') + n + ' ' + moneyUnit;
  }
  if (a === 0) return '$0';
  if (a < 0.01) return (v < 0 ? '-' : '') + '$' + a.toFixed(4);
  if (a < 1000) return (v < 0 ? '-' : '') + '$' + a.toFixed(2);
  return (v < 0 ? '-' : '') + '$' + nf.format(Math.round(a));
}
// moneySym is the unit for a column head: `$` for dollars, else the unit's name.
function moneySym() { return moneyUnit === 'USD' || moneyUnit === 'mixed' ? '$' : moneyUnit; }
// moneyWord is the unit in prose: "dollars", the unit's name, or "mixed units", never "dollars" for a
// view that adds two units.
function moneyWord() { return moneyUnit === 'USD' ? 'dollars' : moneyUnit === 'mixed' ? 'mixed units' : moneyUnit; }
// noMoneyChart draws the empty state in place of a money chart when the view has more than one unit,
// and says whether it did. Such a chart would plot Bobcoins and dollars added into one line.
function noMoneyChart(host) {
  if (moneyUnit !== 'mixed') return false;
  emptyState(host, 'Money is not charted',
    'Costs are in more than one unit, so money is not charted. Token figures are correct.');
  return true;
}
const MIXED_UNIT_NOTE = 'Costs are in more than one unit (for example Bobcoins and dollars), so money '
  + 'totals are not shown. Token figures are correct.';
// applyMoneyUnit sets the page's unit from an API reply, shows the note a mixed view needs, and
// renames the column heads that name the unit.
function applyMoneyUnit(u) {
  setMoneyUnit(u);
  const n = $('#money-unit-note');
  if (n) { n.textContent = MIXED_UNIT_NOTE; n.hidden = moneyUnit !== 'mixed'; }
  for (const s of document.querySelectorAll('.money-sym')) s.textContent = moneySym();
}
// coinsIcon is lucide's Coins icon (ISC licence), the icon Bob IDE shows beside a cost.
function coinsIcon() {
  const s = svgEl('svg', { class: 'coins-icon', viewBox: '0 0 24 24', width: '1em', height: '1em', fill: 'none',
    stroke: 'currentColor', 'stroke-width': 2, 'stroke-linecap': 'round', 'stroke-linejoin': 'round',
    'aria-hidden': 'true' });
  s.appendChild(svgEl('circle', { cx: 8, cy: 8, r: 6 }));
  for (const d of ['M18.09 10.37A6 6 0 1 1 10.34 18', 'M7 6h1v4', 'm16.71 13.88.7.71-2.82 2.82']) {
    s.appendChild(svgEl('path', { d }));
  }
  return s;
}
// moneyIcon is the coins icon for a figure in a unit other than dollars, else null. The figure stays a
// text node and the icon is a node of its own, so a unit name is never parsed as markup.
function moneyIcon(text) {
  const other = moneyUnit !== 'USD' && moneyUnit !== 'mixed';
  return other && typeof text === 'string' && text.endsWith(' ' + moneyUnit) ? coinsIcon() : null;
}
// estUsd renders a held-out estimate: a negative figure is a cost, so it reads as one.
function estUsd(v) { return v < 0 ? 'a cost of ' + usd(-v) : usd(v); }
// estRange shows the held estimate as the range it is: the true saving lies between the floor
// (every removed token at the read rate) and the ceiling (every one at the write rate). A floor below
// zero is shown: the true saving may be a cost, and the ceiling alone would hide that. A floor of
// exactly 0 is also what a row from before the column holds, so it is not a floor and falls back to the
// ceiling alone, as does a floor that is not below the ceiling.
function estRange(low, high) {
  return (low !== 0 && high > low) ? usd(low) + ' to ' + usd(high) : estUsd(high);
}
function pct(v, digits = 1) { return v === null || v === undefined ? '—' : v.toFixed(digits) + '%'; }
/**
 * usdOrNA renders a dollar figure, or `n/a` WITH ITS REASON when the figure is not a
 * measurement.
 *
 * It exists because usd(0) returns '$0' and a $0 is a CLAIM (BRIEF conventions, DESIGN §3.6).
 * searchfold removed 1.76M tokens on this corpus with every one of its 488 acted rows
 * unpriceable, and the cell read `$0` in the ordinary colour with a tooltip that said
 * "recorded per request" — a confident zero standing in for an unknown, which is the single
 * thing this dashboard exists to refuse. cachesplit is the same shape inverted: 8,906
 * structural mutations, no content removed, and a `$0` that reads as "saved nothing" while
 * /api/stats credits it separately.
 *
 * `known` is the CALLER's assertion that the number was computed from something, and it is a
 * required argument rather than a defaulted one so that a new dollar cell cannot inherit the
 * lie by omitting it. `reason` is required for the same purpose: an unreasoned `n/a` is
 * indistinguishable from a bug and will be filed as one (DESIGN §3.6 rule 4).
 *
 * Returns a Node, not a string, because the reason has to reach a screen reader as well as a
 * pointer.
 */
/**
 * term wraps a word that needs a plain-words definition: dotted underline, definition on hover and
 * on keyboard focus. Use it the first time a piece of jargon appears in a section, and write the
 * definition for someone who has never used the product.
 */
function term(word, definition) {
  return el('span', { class: 'term', tabindex: '0' }, word,
    el('span', { class: 'tip', role: 'tooltip', text: definition }));
}
function usdOrNA(v, known, reason) {
  if (known) return document.createTextNode(usd(v));
  if (!reason) throw new Error('usdOrNA(): an n/a without a reason is indistinguishable from a bug');
  return el('span', { class: 'na', title: 'not available: ' + reason,
    'aria-label': 'not available: ' + reason }, 'n/a');
}
function ms(v) {
  if (!v) return '0 ms';
  return v >= 1000 ? (v / 1000).toFixed(2) + ' s' : v.toFixed(v < 10 ? 1 : 0) + ' ms';
}
// Timestamps are epoch ms on the wire and formatted here, in the VIEWER's locale.
// The server never stores or sends a formatted date — a locale string cannot be
// range-queried, sorted, or bucketed.
// One short form for table cells: today shows the time, older rows "Oct 13 11:21".
function whenShort(tsMs) {
  if (!tsMs) return '—';
  const d = new Date(tsMs);
  if (d.toDateString() === new Date().toDateString()) return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  return d.toLocaleDateString([], { month: 'short', day: 'numeric' }) + ' ' + d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false });
}
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
  // The open nav group, derived from `view` by go(). Held rather than recomputed because
  // both nav levels read it on every switch.
  group: 'overview',
  filter: {},
  // loadedAt is when the rollups on screen were fetched, and dirty is whether the server
  // has captured a request since. Together they are the whole freshness contract of a page
  // that no longer repaints itself every ten seconds: the reader can always see how old
  // what they are looking at is, and whether refreshing would change it. See initRefresh.
  loadedAt: 0,
  // lastTryAt is when a load last FINISHED, succeeded or not, and loadingOverview is whether one is
  // in flight. They exist because loadedAt alone cannot pace the timer: it only advances on
  // success, so once a load started failing the timer's own backoff guards — which both measure
  // `now - loadedAt` — were permanently satisfied and it re-fired on every 5s tick instead of every
  // 5 minutes. Worse, the repaint error path calls markDirty(), which set dirty and so ALSO removed
  // the staleness cap that was the second brake. A dashboard left open therefore hammered two
  // expensive endpoints 12x a minute for as long as they kept failing, which is how a slow query
  // became a service-wide outage.
  //
  // They are separate from loadedAt rather than a redefinition of it because loadedAt has a second
  // job: paintFreshness() reports it to the reader as "Updated 3m ago", and that line must keep
  // meaning "when the numbers on screen were fetched". Pacing and freshness are two different
  // questions and one timestamp cannot answer both honestly.
  lastTryAt: 0,
  loadingOverview: false,
  dirty: false,
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
    // Round `now` down to 30 s so a relative range gives the same since for 30 s and the server cache can hit.
    // The server keeps since exact (issue #916), so this rounding is the UI's own choice.
    if (m) return Math.floor(nowMs / 30000) * 30000 - Number(m[1]) * RANGE_UNIT_MS[m[2]];
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
/** pageFiltered is whether a filter or the time range narrows the page. The tenant pick is not one:
 *  it chooses whose figures, not a part of them. */
function pageFiltered() { return hasRange() || Object.entries(state.filter).some(([k, v]) => v && k !== 'tenant'); }

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
  // The server may answer from a cache that is up to ten minutes old (stale-while-revalidate) and
  // says how old in the Age header. Keep it per endpoint so a page can show the age of its DATA.
  const age = Number(res.headers.get('Age')) || 0;
  (state.ageMs ||= {})[path] = age * 1000;
  return readMoneyUnit(await res.json());
}

/**
 * readMoneyUnit applies the unit a money reply states (dash/currency.go) and returns the reply. Every
 * fetch of a money route goes through it, before the caller renders. A reply read with a filter that
 * is not the page's (a count with one filter dropped) must not use it.
 */
function readMoneyUnit(body) {
  if (body && typeof body === 'object' && 'currency' in body) applyMoneyUnit(body.currency);
  return body;
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
    // title is the hover layer every bar on this dashboard should have had: an HTML chart IS
    // interactive, and a bar whose exact value is only in the label column is a bar the reader
    // has to trace across the panel. onclick makes a bar a drill-down where one exists.
    const row = el('div', {
      class: 'bar-row' + (r.onclick ? ' clickable' : ''), title: r.title || null,
      onclick: r.onclick || null,
    },
      el('div', { class: 'bar-label' }, r.term ? term(r.label, r.term) : r.label),
      // `ratio` drops the fill: a figure over 100% is not a share, and this track is a 100%
      // scale, so any bar drawn for it is a bar that disagrees with its own label. The empty
      // track keeps the three-column grid intact.
      el('div', { class: 'bar-track' + (r.ratio ? ' none' : '') }, r.ratio ? null : el('div', {
        class: 'bar-fill' + (r.value < 0 ? ' neg' : '') + (r.value === 0 ? ' zero' : ''),
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
  // The key is what lets an OPEN explanation survive a repaint. Every disclosure on the
  // page is built inside a host that gets cleared and rebuilt, so without this an open
  // <details> closes under the reader on every refresh. The accessible name is the natural
  // key: it is stable across rebuilds and unique per disclosure, and it needs no call site
  // to invent an id. See openWhy / rememberDisclosures.
  const key = 'why:' + (label || summary || text.slice(0, 40));
  return el('details', {
    class: 'why', 'data-why': key, open: openWhy.has(key) ? 'open' : null,
    ontoggle: (ev) => { if (ev.currentTarget.open) openWhy.add(key); else openWhy.delete(key); },
  },
  el('summary', { text: summary, 'aria-label': label || null, title: label || null }),
  el('p', { text: text }));
}

/**
 * openWhy remembers which explanations the reader has opened, by key, for the lifetime of
 * the page.
 *
 * It is a Set rather than a per-host snapshot because the disclosures are rebuilt by six
 * different renderers into five different hosts, and a reader who opens "Why this
 * denominator" and then changes a filter is still reading it. Restoring `open` at build
 * time (above) rather than after the fact means the element is never briefly collapsed,
 * so nothing reflows.
 */
const openWhy = new Set();

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

/**
 * stackedShare draws ONE horizontal bar divided into its parts, to scale.
 *
 * Chosen against the form heuristic rather than by habit: the data's job here is
 * part-to-whole over a handful of named categories, and the heuristic's answer for that is a
 * stacked bar — horizontal, because the category names are long ("messages", "ephemeral_1h",
 * "mcp__plugin_context7_context7"). A pie was rejected for the usual reason (angles cannot be
 * compared, and it needs a legend to say anything); a treemap was rejected because it earns
 * its keep only past a dozen-odd parts and reads worse than a bar below that.
 *
 * Colours are the design system's fixed categorical order (SERIES), which is already validated
 * for the lightness band, chroma floor, adjacent CVD separation and surface contrast in both
 * themes. Four steps is the whole set, so a caller with more than four parts must fold the
 * tail into "other" — passing a fifth colour is not an option this function offers, because a
 * fifth invented hue is how a validated palette stops being one.
 *
 * rows: [{label, value, note}] — already ordered and already folded to <= 4.
 */
function stackedShare(host, rows, opts = {}) {
  clear(host);
  const total = rows.reduce((n, r) => n + Math.max(0, r.value), 0);
  if (!total) {
    emptyState(host, opts.empty || 'Nothing to show yet', opts.emptyDetail || '');
    return;
  }
  const fmt = opts.format || compact;
  // A row may name its OWN fill, and one use of that is load-bearing rather than cosmetic:
  // the segment a reader must not act on takes the de-emphasis gray instead of a categorical
  // slot, so hue means "this is a choice" and gray means "this is context". Without it the
  // only way to draw five parts was to cycle SERIES, which asserts that part 5 is the same
  // thing as part 1.
  const hue = (r, i) => r.color || SERIES[i % SERIES.length];
  const track = el('div', { class: 'share-track', 'data-testid': opts.testid || null });
  rows.forEach((r, i) => {
    const share = (100 * Math.max(0, r.value)) / total;
    if (share <= 0) return;
    const seg = el('span', {
      class: 'share-seg',
      style: 'width:' + share.toFixed(3) + '%;background:' + hue(r, i),
      // The hover layer is not optional on an HTML chart: a 0.4%-wide segment is unreadable
      // and unlabellable, and the tooltip is the only way it can still be identified.
      title: r.label + ': ' + fmt(r.value) + ' (' + pct(share, 1) + ')'
        + (r.note ? ' — ' + r.note : ''),
    });
    track.appendChild(seg);
  });
  host.appendChild(track);
  // A legend is always present for two or more parts, and each entry carries its own value —
  // so identity never rests on hue alone, which is the accessibility floor for this chart.
  const legend = el('div', { class: 'legend share-legend' });
  rows.forEach((r, i) => {
    if (r.value <= 0) return;
    legend.appendChild(el('span', {},
      el('i', { style: 'background:' + hue(r, i) }),
      r.label + ' ',
      el('b', { text: fmt(r.value) }),
      el('span', { class: 'share-pct', text: ' ' + pct((100 * r.value) / total, 1) })));
  });
  host.appendChild(legend);
  if (opts.note) host.appendChild(el('p', { class: 'hint', text: opts.note }));
}

/**
 * renderCacheDetail is the Usage tab's prompt-cache panel: the tier split, WHERE the
 * breakpoints sit, which TTL tier was asked for, and why the cache missed.
 *
 * Placement is the point, and it is the half that was missing. Four columns have recorded it
 * per request since the placement work landed, and they were readable one request at a time in
 * a drawer — so "which of my prompts protect the right prefix" could not be asked at all. The
 * provider caps a request at four breakpoints and its automatic caching consumes one slot, so
 * two prompts with the same COUNT and different locations are not remotely the same prompt.
 */
function renderCacheDetail(o) {
  const host = $('#cache-detail');
  if (!host) return;
  clear(host);

  // 1. The tier split — the one number that explains every small percentage on this dashboard.
  host.appendChild(el('h3', {}, 'What the provider billed, by price tier'));
  const inputTotal = o.cache_read + o.cache_write + o.fresh_input;
  stackedShare(host.appendChild(el('div')), [
    { label: 'Cache reads', value: o.cache_read, note: 'about a tenth of full rate' },
    { label: 'Cache writes', value: o.cache_write, note: 'about 12.5x a read' },
    { label: 'Fresh input', value: o.fresh_input, note: 'full rate' },
  ], { testid: 'cache-tier-share',
    note: inputTotal > 0
      ? 'Cache reads are ' + pct((100 * o.cache_read) / inputTotal, 1) + ' of all input. That '
        + 'is why component savings are small: a token removed from an already-cached region '
        + 'is worth the read rate, and almost everything we could remove is in one.'
      : '' });

  // 2. Placement BY LOCATION. Aggregated for the first time.
  const bp = o.breakpoints || {};
  const bpTotal = (bp.system || 0) + (bp.tools || 0) + (bp.messages || 0) + (bp.blocks || 0) + (bp.root || 0);
  host.appendChild(el('h3', {}, 'Where the cache markers sit in your prompts'));
  if (!bpTotal) {
    emptyState(host.appendChild(el('div')), 'No breakpoints recorded in this window',
      'These requests carried no prompt-cache breakpoints, or predate the placement capture.');
  } else {
    stackedShare(host.appendChild(el('div')), [
      { label: 'system', value: bp.system || 0, note: 'protects the whole prompt after it' },
      { label: 'tools', value: bp.tools || 0, note: 'protects the tool declarations' },
      { label: 'messages', value: bp.messages || 0, note: 'protects transcript history' },
      { label: 'blocks', value: bp.blocks || 0, note: 'inside a single content block' },
      { label: 'top-level', value: bp.root || 0, note: 'automatic caching: the provider marks the last block' },
    ], { testid: 'cache-bp-share', format: num,
      note: 'Counted over ' + num(bp.requests) + ' requests that carried at least one. '
        + 'The provider caps a request at FOUR breakpoints and its own automatic caching '
        + 'consumes one of those slots, so where they sit decides how much prefix is '
        + 'protected — a count alone cannot answer that.' });
    if (bp.modal) {
      host.appendChild(el('p', { class: 'hint', 'data-testid': 'cache-bp-modal' },
        'Most common placement: ' + bp.modal + ' — on ' + num(bp.modal_requests) + ' requests ('
        + pct((100 * bp.modal_requests) / Math.max(1, o.requests), 0) + '). Reported as the '
        + 'most common rather than an average, because an average across locations describes '
        + 'no real prompt: 1.5 breakpoints in a system block is not a thing any request did.'));
    }
  }

  // 3. The TTL tier. Newly captured, so coverage is stated before any share is.
  host.appendChild(el('h3', {}, 'How long the cache was asked to keep your prompt'));
  const ttl = o.cache_ttl || {};
  if (!o.cache_ttl_recorded) {
    emptyState(host.appendChild(el('div')), 'Not recorded for these requests yet',
      'The prompt-cache TTL tier is newly captured. The pipeline always computed it — it '
      + 'decides when a cache is treated as cold — but it was never stored, so nothing could '
      + 'group by it and a 1-hour prompt was priced at the 5-minute write rate. These fill in '
      + 'from the next request through the proxy. Showing a 100% share of "no caching" over '
      + 'history would be reporting the absence of a measurement as a measurement.');
  } else {
    stackedShare(host.appendChild(el('div')), [
      { label: '5-minute cache', value: ttl.ephemeral_5m || 0, note: 'the default, kept for about 5 minutes' },
      { label: '1-hour cache', value: ttl.ephemeral_1h || 0, note: 'kept for an hour, costs more to write' },
      { label: 'no caching asked for', value: ttl[''] || 0, note: 'body carried no cache_control' },
    ], { testid: 'cache-ttl-share', format: num,
      note: 'Recorded on ' + num(o.cache_ttl_recorded) + ' of ' + num(o.requests)
        + ' requests in this window. "No caching asked for" is a third answer and is never '
        + 'folded into the 5-minute tier: an uncached request is not a 5-minute one.' });
  }

  // 4. Why the cache missed, and what each miss cost. The distribution already exists on
  // Overview; what is new here is that a miss is priced.
  host.appendChild(el('h3', {}, 'Why the cache sometimes missed'));
  renderDistribution('#cache-miss-detail', o.cache_miss, {
    hit: 'hit', cold_start: 'first request (not a failure)', ttl_expiry: 'cache expired while idle',
    prefix_change: 'start of prompt changed', unknown: 'unknown', '': 'no cache data',
  }, 'Every request carries an attribution once one has been captured.');
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
        'off-path on a copy. Savings shown here are what your preset WOULD have achieved, ' +
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
// Each tab file under tabs/ adds its own entry at its end, with Object.assign(loaders, ...).
// This file loads first, so a literal here cannot name a tab's loader: that function does not
// exist yet when this line runs.
const loaders = {
  // Registered by insights.js, which mounts its own tab; named here because `loaders` is the
  // single map the router consults and a view absent from it renders an empty section forever.
  insights: () => (typeof loadInsights === 'function' ? loadInsights() : undefined),
};

// ── nav: two levels, five groups ───────────────────────────────────────────
//
// Everything from here to the end of `applyURL` is the SHELL + ROUTER — the nav, the hash
// contract and the view switch. It depends on `loaders`, `DIMS`, `state` and the DOM helpers.
// The tab files under tabs/ use its internals only through `go`, `mountTab` and `syncNav`.
//
/**
 * GROUPS is the nav, and the ONLY place the two levels' shape is written down: the group
 * buttons in index.html mirror it, the per-group tablists mirror it, and the hash's first
 * path segment is a group name from it.
 *
 * It deliberately lists only the views index.html authors. Inventory, KV-cache and
 * Campaigns are absent because they mount themselves (mountTab) — hardcoding them here
 * would put back exactly the coupling that self-mounting exists to avoid.
 */
const GROUPS = [
  ['overview', ['overview']],
  ['savings', ['keepalive', 'usage']],
  ['traffic', ['sessions', 'requests']],
  ['admin', ['config', 'strategies', 'tenants', 'setup', 'settings', 'archive', 'feedback']],
  // Operator and research views: real, and not what a user opens this page for.
  ['advanced', ['components']],
];
/**
 * TAB_INFO is the one place that says, in plain words, what each tab is for. It feeds three
 * things: the heading and one-line intro at the top of every tab (ensureIntro), the "About the
 * other tabs" list on Overview (renderTabMap), and nothing else, so the wording cannot drift.
 * Written for someone who has never seen this dashboard: no component names, no jargon.
 */
const TAB_INFO = {
  overview: { short: 'Savings and what to remove', title: 'Overview', what: 'How much context-guru saved you, in dollars and tokens, and what to remove next.' },
  keepalive: { short: 'Does your cache strategy pay off', title: 'Cache strategy', what: 'Whether your cache strategy (keeping the prompt cache warm while you think) paid for itself, and which one to use.',
    see: 'The verdict first, then who gained and who lost, how long your sessions sit idle, and a calculator. (Also called keep-alive.)' },
  tools: { short: 'Tools and skills you never use', title: 'Inventory', what: 'Every tool server and skill your requests carry, and the ones that were never used.',
    see: 'Ranked cards with the cost of carrying each one and the command to remove it.' },
  usage: { short: 'Spend by day and by model', title: 'Usage', what: 'What you spent each day and on which models, and how much the provider cache covered.',
    see: 'A day-by-day chart, a split by model or agent, and how often requests found their prompt already cached.' },
  sessions: { short: 'One row per working session', title: 'Sessions', what: 'One row per working session: what it cost, how long it ran and what was saved.',
    see: 'A sortable table. Open a row to see its requests.' },
  requests: { short: 'Every single request', title: 'Requests', what: 'Every single request, newest first.',
    see: 'Open one to see what it contained, what was removed from it and what the provider billed.' },
  components: { short: 'How each step of your preset did', title: 'Preset steps', what: 'How each step of your preset performed: what it removed, what it cost and whether it paid off.',
    see: 'One row per step with its net result, plus a table of the ways it chose to do nothing.' },
  kvcache: { short: 'How long your cache stays warm', title: 'Cache detail', what: 'How long your prompt cache stays warm between requests, and what other timings would cost. (Also called the KV-cache view.)',
    see: 'Idle-gap charts, a price table and a replay of different cache strategies on your own history.' },
  campaigns: { short: 'Cache strategies in bulk', title: 'Strategy campaigns', what: 'Create cache strategies for many sessions at once, then check them on data they have not seen.',
    see: 'A form to create rules and a table of the ones you made.' },
  config: { short: 'How this proxy is set up', title: 'Config', what: 'The settings this proxy is running with, and whether it is recording requests properly.',
    see: 'The effective configuration, recording health and recent log lines.' },
  strategies: { short: 'Saved cache strategies', title: 'Cache strategies', what: 'The cache strategies you saved and what each has done so far.',
    see: 'A form to add a rule and a table of existing ones.' },
  tenants: { short: 'Accounts and A/B groups', title: 'Tenants', what: 'The accounts on this proxy, and the A/B groups they are split into.',
    see: 'Account list, spend per account and the A/B comparison.' },
  setup: { short: 'Connect Claude Code to it', title: 'Setup', what: 'How to point Claude Code or Codex at this proxy.',
    see: 'Copy-paste settings for your token.' },
  settings: { short: 'Your account and tokens', title: 'Settings', what: 'Your account: preset settings, access tokens, password and machines.',
    see: 'Forms you can change, and an audit trail of past changes.' },
  archive: { short: 'Old sessions in storage', title: 'Archive', what: 'Old sessions moved to cold storage to save disk.',
    see: 'A list of archived sessions; open one to read it.' },
  feedback: { short: 'Tell us what to fix', title: 'Feedback', what: 'Tell us what is working and what is not.',
    see: 'A short form, and for managers a summary of what people said.' },
};

/** ensureIntro gives a view its heading and one-line "what this tab shows", once. Overview ships
 *  its own heading in the markup; every other view gets one prepended here. */
function ensureIntro(view) {
  const panel = document.getElementById('view-' + view);
  const info = TAB_INFO[view];
  if (!panel || !info) return;
  let head = document.querySelector(`.page-head[data-view="${view}"]`);
  if (!head) {
    head = el('div', { class: 'page-head', 'data-view': view },
      el('h1', { class: 'page-title', text: info.title }),
      el('p', { class: 'tab-intro', text: info.what }));
    // A sibling ABOVE the panel (and above its bottom-line banner, if one is already there),
    // not a child: Inventory and KV-cache rebuild their whole panel on every load.
    const prev = panel.previousElementSibling;
    (prev && prev.classList.contains('bl') ? prev : panel).before(head);
  }
}

/** The "About the other tabs" list: one entry per tab this viewer can open, in nav order. */
function renderTabMap() {
  const host = $('#tab-map');
  if (!host) return;
  clear(host);
  // Grouped exactly like the nav, in the nav's own names. Walk the live nav rather than GROUPS:
  // Inventory, KV-cache and Campaigns mount themselves.
  const groups = new Map();
  for (const tab of $$('.viewtabs .tab')) {
    const info = TAB_INFO[tab.dataset.view];
    if (!info || !reachable(tab)) continue;
    const g = tab.closest('.viewtabs').dataset.group;
    if (!groups.has(g)) groups.set(g, []);
    groups.get(g).push([tab.dataset.view, info]);
  }
  for (const [g, items] of groups) {
    const label = ($(`.groups [data-group="${g}"]`) || {}).textContent || g;
    host.appendChild(el('div', { class: 'tab-group' }, el('dt', { text: label }),
      ...items.map(([view, info]) => el('dd', {}, el('a', { href: '#/' + view, text: info.title }), el('span', { text: info.short })))));
  }
}

/** GROUP_OF maps a view onto its group. mountTab adds to it, which is how a self-mounted
 *  view gets a group without this file naming it. */
const GROUP_OF = new Map(GROUPS.flatMap(([g, views]) => views.map((v) => [v, g])));

/** navTab is a view's tab button, in whichever level owns it: Overview's tab is a group
 *  button (it is a group AND a view), every other view's tab is in a group's tablist. */
function navTab(view) { return $(`[data-view="${view}"]`); }
/** reachable is "this account can open it now" — the permission gate hides, the local-mode
 *  lock only disables, and neither is navigable. */
function reachable(tab) { return !tab.hidden && tab.getAttribute('aria-disabled') !== 'true'; }
/** firstView is what a group opens on: its first REACHABLE tab, so Admin on a single-tenant
 *  proxy lands on Config rather than on a locked Tenants. null = nothing in it is open to
 *  this viewer, and the group button hides. */
function firstView(group) {
  if (group === 'overview') return 'overview';
  const t = $$(`.viewtabs[data-group="${group}"] .tab`).find(reachable);
  return t ? t.dataset.view : null;
}

/**
 * mountTab adds one tab and its panel, and is the single place outside this section that
 * knows the nav's DOM shape — tools.js, kvcache.js and campaigns.js each call it once.
 *
 * The tab button exists as soon as the caller runs, BEFORE the view's body is built, so
 * lazy-loading those bodies later is a change to the caller and not to the nav.
 *
 * It returns the empty `<section class="view">`, already appended to #main and already
 * wired as the tab's tabpanel, which is what each caller then fills.
 */
function mountTab({ group, after, view, label, manager }) {
  const list = $(`.viewtabs[data-group="${group}"]`);
  if (!list) throw new Error('mountTab: no such group: ' + group);
  const tab = el('button', {
    role: 'tab', class: 'tab', id: 'tab-' + view, 'data-view': view,
    'data-testid': 'tab-' + view, 'aria-controls': 'view-' + view,
    'aria-selected': 'false', tabindex: '-1',
    // `manager` is the only gate any self-mounting view has needed. data-local-ok and
    // data-account are markup-only for that reason; add a flag the day a caller wants one.
    'data-manager': manager ? '' : null,
    // A gated tab mounts hidden and applyAccount reveals it, exactly like the ones in the
    // markup: a manager tab that is visible for the moment before /api/whoami answers is a
    // manager tab a non-manager can click.
    hidden: manager ? 'hidden' : null,
  }, label);
  const sib = after && $(`.tab[data-view="${after}"]`, list);
  list.insertBefore(tab, sib ? sib.nextSibling : null);
  GROUP_OF.set(view, group);
  const panel = el('section', {
    class: 'view', id: 'view-' + view, role: 'tabpanel',
    'aria-labelledby': 'tab-' + view, hidden: 'hidden',
  });
  $('#main').appendChild(panel);
  return panel;
}

/**
 * syncNav makes both nav levels match state: which group is open, which of its tabs is
 * selected, which groups this viewer has anything in, and ONE tab stop per level.
 *
 * That last part is the fix for a measured defect: seventeen `role="tab"` buttons were
 * seventeen Tab stops, so reaching the filter bar by keyboard meant seventeen presses. The
 * WAI-ARIA tabs pattern is one stop per tablist with the arrows moving inside it — navKeys.
 */
function syncNav() {
  const group = state.group || 'overview';
  for (const b of $$('.groups .tab')) {
    b.hidden = !firstView(b.dataset.group);
    const on = b.dataset.group === group;
    b.setAttribute('aria-selected', String(on));
    b.tabIndex = on ? 0 : -1;
    // Level 1 scrolls sideways below 900px rather than wrapping to a second 44px row, so
    // the open group can be half off the edge. A no-op when it is already fully visible.
    if (on) b.scrollIntoView({ block: 'nearest', inline: 'nearest' });
  }
  const panel = $(`#subnav .grouppanel[data-group="${group}"]`);
  for (const p of $$('#subnav .grouppanel')) p.hidden = p !== panel;
  // Overview is a group with one view, so it has no second level at all rather than a
  // sub-nav row holding a single tab that repeats the label above it. And nothing in the
  // nav means anything while the gate is up — a stray sub-nav bar over a login form is
  // worse than no nav, which is why showGate calls back into here.
  $('#subnav').hidden = !panel || gated();
  for (const t of $$('.viewtabs .tab')) {
    const on = t.dataset.view === state.view;
    t.setAttribute('aria-selected', String(on));
    t.tabIndex = on ? 0 : -1;
  }
  // A tablist whose selected tab is not in it (mid-switch) would have no tab stop at all.
  if (panel && !$('.tab[tabindex="0"]', panel)) {
    const t = $$('.tab', panel).find(reachable) || $('.tab', panel);
    if (t) t.tabIndex = 0;
  }
  renderTabNote(panel);
}

/**
 * lockTab marks a tab present-but-not-now instead of hiding it. NN/g `empty-nav-state`: an
 * unavailable destination should say why, and on a default single-tenant proxy only nine of
 * the seventeen tabs are open, so silently hiding the rest makes the product look half its
 * size and reads as a broken build. A locked tab keeps its place and stays focusable.
 */
function lockTab(tab, why) {
  if (why) { tab.setAttribute('aria-disabled', 'true'); tab.dataset.why = why; } else { tab.removeAttribute('aria-disabled'); delete tab.dataset.why; }
}

/** renderTabNote is the one line under a group's tabs naming what is locked and why. All
 *  current locks share one reason, so the note states it once; give it a per-reason split
 *  the day a second reason exists. */
function renderTabNote(panel) {
  const note = $('#tab-note');
  const locked = panel ? $$('.tab[aria-disabled="true"]', panel) : [];
  note.hidden = !locked.length;
  if (!locked.length) return;
  const names = locked.map((t) => t.textContent);
  const list = names.length > 1
    ? names.slice(0, -1).join(', ') + ' and ' + names[names.length - 1] + ' need'
    : names[0] + ' needs';
  note.textContent = list + ' ' + locked[0].dataset.why + '.';
}

/**
 * navKeys is arrow-key movement inside a tablist, with MANUAL activation: the arrows move
 * focus and Enter/Space (the button's own behaviour) opens it. Automatic activation is the
 * pattern's default, but every tab here fires a data fetch, so scrubbing across Admin's
 * seven tabs would issue seven queries nobody asked for.
 */
function navKeys(ev) {
  const from = ev.target.closest('[role="tab"]');
  if (!from) return;
  const tabs = $$('[role="tab"]', ev.currentTarget).filter((t) => !t.hidden);
  const i = tabs.indexOf(from);
  const step = { ArrowRight: 1, ArrowDown: 1, ArrowLeft: -1, ArrowUp: -1 }[ev.key];
  let j = -1;
  if (step) j = (i + step + tabs.length) % tabs.length;
  else if (ev.key === 'Home') j = 0;
  else if (ev.key === 'End') j = tabs.length - 1;
  if (j < 0 || j === i) return;
  ev.preventDefault();
  tabs[j].focus();
}

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
  // hash either: its loader would 401/403 and paint an error nobody can act on. That
  // covers a LOCKED tab as well as a hidden one, and — for a manager-only view never
  // revealed from its <template> (see revealManagerTemplates) — a MISSING one too.
  const tab = navTab(view);
  if (!tab || !reachable(tab)) view = 'overview';
  state.view = view;
  state.group = GROUP_OF.get(view) || 'overview';
  syncNav();
  for (const s of $$('.view')) s.hidden = s.id !== 'view-' + view;
  ensureIntro(view);
  for (const h of $$('.page-head[data-view]')) h.hidden = h.dataset.view !== view;
  if (view === 'overview') renderTabMap();
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
      class: 'sort', title: 'Sort by ' + label + (th.title ? '. ' + th.title : ''), onclick: () => toggleSort(key),
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
  // The per-tab bottom-line banners are retired: each tab now opens with its own heading and intro,
  // and a second headline above it contradicted the tab's own verdict.
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
  return location.pathname + '#/' + navPath(state.view) + (q ? '?' + q : '');
}
/**
 * navPath is the canonical hash path for a view: `<group>/<view>`, collapsed to just the
 * name when a group is also a view (Overview). So `#/savings/usage`, and `#/overview`.
 *
 * THE RULE, stated once: the LAST path segment is the view; anything before it is a group
 * hint and is ignored whenever the view is known. That is what makes the old one-level
 * `#usage` and the new `#/savings/usage` the same link, and it is decidable without a
 * lookahead because no view name contains a slash. The leading slash is written, not
 * required — `#savings/usage` resolves identically and is rewritten.
 */
function navPath(view) {
  const g = GROUP_OF.get(view) || 'overview';
  return g === view ? view : g + '/' + view;
}
/**
 * resolveNav turns a hash path into a view name. Every link this dashboard has ever
 * written is one segment, including the two the SERVER writes (dash/kvcache.go:510-511),
 * so that branch is not a compatibility shim to be removed later — it is half the traffic.
 */
const FIVE_PAGE_ALIAS = { home: 'overview', cache: 'keepalive' }; // #savings already opens the Savings group
// Views that were removed, and the tab that now answers the same question. Matched as the LAST segment at
// any depth, so both `#history` and `#/savings/history` land there instead of on the group's first tab.
const REMOVED_VIEW = { history: 'insights' };
function resolveNav(path) {
  const seg = String(path || '').split('/').filter(Boolean);
  const view = seg[seg.length - 1] || '';
  if (GROUP_OF.has(view)) return view;
  if (REMOVED_VIEW[view]) return REMOVED_VIEW[view];
  // Links to the short-lived five-page home (#home, #cache; #savings, #tools and #sessions already resolve) land on the nearest tab.
  if (seg.length === 1 && FIVE_PAGE_ALIAS[seg[0]]) return FIVE_PAGE_ALIAS[seg[0]];
  // Only a group name survived (`#/admin`, or `#/savings/typo`): open its first tab.
  const g = GROUPS.find(([n]) => n === seg[0]);
  return (g && firstView(g[0])) || 'overview';
}
function syncURL(replace) {
  const url = urlFor();
  if (location.pathname + location.hash === url) return;
  if (replace) history.replaceState(null, '', url);
  else history.pushState(null, '', url);
}
function parseURL() {
  const [path, query] = (location.hash || '').replace(/^#\/?/, '').split('?');
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
    view: resolveNav(path), filter,
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
  src.onopen = () => {
    box.className = 'live on';
    label.textContent = 'live';
    // A reconnect means we were away, and the hub's replay ring is bounded — so anything that
    // happened beyond it is gone and no `request` event will ever arrive for it. Having held a
    // connection before is exactly the signal that a gap may exist.
    if (lastEventID) markDirty();
  };
  src.onerror = () => { box.className = 'live off'; label.textContent = 'reconnecting…'; };
  src.addEventListener('request', (ev) => {
    let e;
    try { e = JSON.parse(ev.data); } catch (_) { return; }
    lastEventID = Math.max(lastEventID, e.id || 0);
    state.live.unshift(e);
    if (state.live.length > 60) state.live.length = 60;
    // The feed itself is append-only and cheap, so it always paints. The ROLLUPS are what
    // cost a query and a full repaint, so this only records that they are now behind.
    if (state.view === 'overview') renderLive();
    markDirty();
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
    btn.textContent = 'Theme: ' + face;
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
  $('#overview-diagnostics').addEventListener('toggle', (ev) => { if (ev.target.open && (state.diagStale || !state.diagDrawn)) { state.diagDrawn = true; renderDiagnostics(); } });
  $('#filters-toggle').addEventListener('click', (ev) => {
    const open = $('#filters').classList.toggle('open');
    ev.currentTarget.setAttribute('aria-expanded', String(open));
  });
  // Delegated, not one listener per tab: the three self-mounting views add their tabs
  // while this file is still being parsed, so a per-tab loop here would either miss them
  // or have to be re-run. A group button opens that group's first usable tab.
  $('.groups').addEventListener('click', (ev) => {
    const b = ev.target.closest('[data-group]');
    if (b) go(firstView(b.dataset.group) || 'overview');
  });
  $('#subnav').addEventListener('click', (ev) => {
    const t = ev.target.closest('.tab');
    // A locked tab is inert; #tab-note beside it already says why, so a click that
    // bounced to Overview would be a worse answer than no click at all.
    if (t && reachable(t)) go(t.dataset.view);
  });
  $('.groups').addEventListener('keydown', navKeys);
  $('#subnav').addEventListener('keydown', navKeys);
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
  // Components is manager-only and its markup is now a <template> until revealed, so this
  // is wired from wireManagerView instead of here — see revealManagerTemplates.
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
  $('#break-more').addEventListener('change', (e) => $('[data-testid=breakdown-table]').classList.toggle('show-more', e.target.checked));
  document.addEventListener('change', (e) => { if (e.target.id === 'comp-more') $('[data-testid=components-table]').classList.toggle('show-more', e.target.checked); });
  $('#req-more').addEventListener('change', (e) => $('[data-testid=requests-table]').classList.toggle('show-more', e.target.checked));
  $('#sess-more').addEventListener('change', (e) => $('[data-testid=sessions-table]').classList.toggle('show-more', e.target.checked));
  $('#sess-sort').addEventListener('change', (ev) => { state.sessSort = ev.target.value; state.sessOffset = 0; loadSessions(); });
  $('#sess-prev').addEventListener('click', () => { state.sessOffset = Math.max(0, state.sessOffset - 25); loadSessions(); });
  $('#sess-next').addEventListener('click', () => { state.sessOffset += 25; loadSessions(); });
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
  initRefresh();
  setInterval(() => { if (!gated() && !busyReading()) { loadFacets(); checkCapture(); } }, 30000);
}

// ── refresh: manual, event-driven, and out of the reader's way ──────────────
//
// This replaced a bare `setInterval(loadOverview, 10000)`, which repainted the headline
// every ten seconds whether or not anything had changed. Every figure on Overview is a
// server-side rollup, so a repaint clears and rebuilds ~40 tiles, six bar panels and five
// SVG charts — collapsing open explanations, dropping a hovered tooltip and clamping the
// scroll offset while somebody was reading. Three separate changes fix that, and the first
// one is the important one:
//
//  1. REFRESH ONLY WHEN THE DATA CHANGED. The page already holds an EventSource on
//     /api/events that fires per captured request (connectLive). That stream IS the
//     "something changed" hint, so it costs nothing to use: an event sets `state.dirty`,
//     and the timer is a no-op unless it is set. An idle deployment now issues zero
//     aggregate queries instead of 8,640 a day.
//  2. A LONGER, CONFIGURABLE INTERVAL with the choice remembered, because "how fresh do I
//     need this" is a property of the reader's task, not of the page.
//  3. A PAUSE WHILE READING, because a refresh the reader did not ask for is only
//     acceptable when they are not mid-sentence.
//
// The manual button is what makes all three safe: any automatic policy can be wrong, and
// the reader always has an unambiguous way to say "now".

/** REFRESH_CHOICES are the offered intervals, in ms. 0 is manual-only. */
const REFRESH_CHOICES = [['0', 'Manual only'], ['60000', 'Every minute'],
  ['300000', 'Every 5 minutes'], ['900000', 'Every 15 minutes']];
const REFRESH_DEFAULT = 300000;

/** refreshMs is the reader's remembered choice. 5 minutes by default, not 10 seconds. */
function refreshMs() {
  // The stored value is read as a STRING first and only then converted, because Number(null)
  // is 0, not NaN — so treating an absent preference as a number made "never stored" and
  // "explicitly set to manual" the same state, and every first-time reader got manual-only.
  const raw = localStorage.getItem('cg-refresh');
  if (raw === null) return REFRESH_DEFAULT;
  const v = Number(raw);
  return Number.isFinite(v) && v >= 0 ? v : REFRESH_DEFAULT;
}

const gated = () => !$('#gate').hidden;

/**
 * busyReading is the interaction guard: true when a repaint would interrupt somebody.
 *
 * Five states, all cheap to test and all of them cases where the old poller actively
 * destroyed work: a focused control (the filter bar, the search box), an open explanation,
 * a live text selection, an open drawer, and a window that is not even frontmost. The
 * checks are scoped to #main so focus in the header's theme button does not freeze the
 * page forever.
 */
function busyReading() {
  // document.hasFocus() is deliberately NOT here. It was, as "the reader is not looking" — but a
  // dashboard on a second monitor is exactly the case the old poller served, and gating on focus
  // made it permanently stale while still printing an age. Nothing is being interrupted when
  // nobody is interacting, which is the only thing this guard is for.
  const a = document.activeElement;
  if (a && a !== document.body && $('#main').contains(a)) return true;
  if ($('#view-overview details[open]')) return true;
  if (!$('#drawer').hidden) return true;
  const sel = window.getSelection();
  if (sel && !sel.isCollapsed && String(sel).trim()) return true;
  return false;
}

/** markDirty records that the server has data the page has not shown yet. */
function markDirty() {
  state.dirty = true;
  const b = $('#refresh-now');
  if (b) b.classList.add('has-new');
  paintFreshness();
}

/**
 * paintFreshness writes the "last updated" line. It ticks every second so the reader can
 * see the number is a live age rather than a timestamp that might be hours stale — which
 * is the actual question a manual-refresh page has to answer, and the one the old
 * always-repainting page never had to.
 */
function paintFreshness() {
  const n = $('#refresh-age');
  if (!n) return;
  if (!state.loadedAt) { n.textContent = ''; return; }
  const secs = Math.max(0, Math.round((Date.now() - state.loadedAt) / 1000));
  const age = secs < 60 ? secs + 's ago'
    : secs < 3600 ? Math.round(secs / 60) + 'm ago'
      : Math.round(secs / 3600) + 'h ago';
  n.textContent = 'Updated ' + age + (state.dirty ? ' · new data available' : '');
  n.classList.toggle('has-new', state.dirty);
}

function initRefresh() {
  const sel = $('#refresh-every');
  for (const [v, label] of REFRESH_CHOICES) {
    sel.appendChild(el('option', { value: v, text: label,
      selected: String(refreshMs()) === v ? 'selected' : null }));
  }
  sel.addEventListener('change', (ev) => {
    localStorage.setItem('cg-refresh', ev.currentTarget.value);
    paintFreshness();
  });
  $('#refresh-now').addEventListener('click', () => {
    $('#refresh-now').classList.remove('has-new');
    loadOverview();
  });
  setInterval(paintFreshness, 1000);
  // One timer at a fixed short tick, testing the reader's interval itself — so changing
  // the interval takes effect immediately instead of after the old one elapses.
  setInterval(() => {
    const every = refreshMs();
    if (!every || gated() || state.view !== 'overview' || state.to !== 'now') return;
    // A window whose `to` is absolute cannot gain rows, so repolling it is pure waste.
    // `dirty` is the same argument applied to a window that CAN: no captured request means
    // no changed rollup, and the SSE stream already tells us.
    if (busyReading()) return; // try again on the next tick; the button is always available
    // Never stack. A load slower than this tick would otherwise have another started on top of it
    // every 5s, and since a failing load is also a SLOW one, the failure case stacked hardest: an
    // abandoned request still costs the server a connection and a full query, so a pileup on the
    // client is a pileup on the server.
    if (state.loadingOverview) return;
    // Paced on the last ATTEMPT, not the last success. Using loadedAt here is what let a failing
    // dashboard poll every 5s forever: it only advances when a load works, so `age` grew without
    // bound and both of these guards stopped guarding. lastTryAt advances in loadOverview's
    // `finally`, so a run of failures backs off to the reader's chosen interval exactly like a run
    // of successes.
    const age = Date.now() - Math.max(state.loadedAt, state.lastTryAt);
    if (age < every) return;
    // The staleness cap, and it is the honest half of this design.
    //
    // `dirty` is an OPTIMISATION, not a source of truth, and it can be missed three ways: the
    // event hub's ring is capped so a client away long enough loses the gap; /api/events may be
    // unavailable entirely (an older proxy, a reverse proxy that buffers SSE, a 403 on the
    // scoped feed); and a request captured between issuing a fetch and clearing the flag is
    // dropped. In all three the page would sit reading "Updated 3h ago" with no "new data"
    // hint — which is not silence, it is a positive claim that nothing has changed.
    //
    // So past four intervals we refresh regardless. The idle-deployment claim survives intact:
    // a genuinely idle deployment still does nothing until the cap, and then one query.
    if (!state.dirty && age < every * 4) return;
    loadOverview();
  }, 5000);
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
  // preset is the preset the local proxy runs now, from /api/whoami. '' on a hosted proxy.
  preset: '',
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
  for (const sel of ['.filters', '.groups', '.live']) {
    const n = $(sel);
    if (n) n.hidden = show;
  }
  // The second level is syncNav's, because whether it is shown at all depends on the open
  // group as well as on the gate.
  syncNav();
}

/**
 * Reflect who is signed in, and which tabs that entitles them to.
 *
 * async, and a caller that is about to navigate — probeAccount, on the very first load —
 * awaits it. Without that, the old <script defer> tag guaranteed campaigns.js ran (and had
 * mounted its own #tab-campaigns) before applyURL() ever could; this function now fetches
 * that script itself, on demand, so it has to await that fetch BEFORE the [data-manager]
 * hidden-toggle loop below — otherwise a manager who deep-links or refreshes on
 * #/savings/campaigns would hit go('campaigns') while #tab-campaigns either does not exist
 * yet or exists but was never unhidden (mountTab mounts it hidden on purpose; see its own
 * comment), and get silently bounced to Overview.
 */
async function applyAccount() {
  const t = account.tenant;
  $('#whoami').hidden = !t;
  $('#signout').hidden = !t;
  if (t) {
    $('#whoami').textContent = t.label ? `${t.email} · ${t.label}` : t.email;
    $('#whoami').title = t.role === 'manager' ? 'Manager' : 'User';
  }
  for (const el of $$('[data-account]')) el.hidden = !account.hosted || !t;
  // Build the manager-only tab/panel DOM (and fetch campaigns.js) only once the role that
  // entitles a viewer to them is actually known — see revealManagerTemplates and
  // maybeLoadManagerScript. A plain hosted account must never have this markup exist at
  // all, not just be CSS-hidden.
  revealManagerTemplates();
  // Awaited before the loop below runs: campaigns.js's own mountTab call (which creates
  // #tab-campaigns, initially hidden) has to have already happened, or this loop would
  // run over a tab that does not exist yet and never get a second pass to unhide it.
  await maybeLoadManagerScript();
  // data-manager is "hosted managers only". data-local-ok marks the ones that are also
  // fine on a single-tenant proxy, where there is no principal and nothing to scope:
  // The Components and Config tabs are manager-gated in hosted mode but open locally, and
  // hiding them there would break the local dev path.
  //
  // In HOSTED mode a non-manager still sees nothing: that is a permissions boundary over
  // somebody else's data. In single-tenant local mode there is no other tenant to protect,
  // so the manager-only tabs are LOCKED rather than hidden — see lockTab.
  for (const b of $$('[data-manager]')) {
    const ok = account.hosted ? !!(t && t.role === 'manager') : b.hasAttribute('data-local-ok');
    b.hidden = account.hosted && !ok;
    lockTab(b, ok || account.hosted ? '' : 'a manager sign-in');
  }
  // data-account tabs stay silently hidden. A signed-out viewer has no use for a tab they
  // cannot enable from here, which is the whole difference from the manager case above.
  syncNav();
  loadTenantOptions();
}
function isManager() { return !!(account.tenant && account.tenant.role === 'manager'); }

// Manager-only tab + panel pairs that ship in index.html as inert <template>s (never part
// of the live DOM, so a plain hosted account's browser never builds them) rather than as
// hidden elements. `localOk` mirrors data-local-ok: true for the views a single-tenant
// proxy — which has no principal to protect — may use even signed out.
const MANAGER_ONLY_VIEWS = [
  ['config', true], ['components', true],
  ['strategies', false], ['tenants', false],
];
/** Clone each manager-only template into the live DOM once its viewer is entitled to it. */
function revealManagerTemplates() {
  for (const [view, localOk] of MANAGER_ONLY_VIEWS) {
    if (!(account.hosted ? isManager() : localOk)) continue;
    const tabTpl = document.getElementById('tpl-tab-' + view);
    const panelTpl = document.getElementById('tpl-view-' + view);
    if (!tabTpl && !panelTpl) continue; // already revealed by an earlier call
    if (tabTpl) tabTpl.replaceWith(tabTpl.content);
    if (panelTpl) panelTpl.replaceWith(panelTpl.content);
    wireManagerView(view);
  }
}
// Each view's init()-time-only bindings (sort headers, one-off click/change listeners),
// deferred here because their targets did not exist in the DOM until just now.
function wireManagerView(view) {
  if (view === 'components') sortable('[data-testid=components-table]', COMPONENT_SORT);
  else if (view === 'tenants') $('#ab-range').addEventListener('change', loadVariants);
}
// campaigns.js is manager-only (see its own header) with no local-ok exemption, so it is
// not even <script>-tagged in index.html — fetching it at all would let a plain account's
// network tab see a manager-only feature's code. Load it the one time a hosted manager
// signs in, and resolve only once it has actually run — see applyAccount's own comment on
// why a caller that is about to navigate needs to await this.
let campaignsScriptPromise = null;
function maybeLoadManagerScript() {
  if (!(account.hosted && isManager())) return Promise.resolve();
  if (!campaignsScriptPromise) {
    campaignsScriptPromise = new Promise((resolve) => {
      const s = el('script', { src: 'campaigns.js', onload: resolve, onerror: resolve });
      document.body.appendChild(s);
    });
  }
  return campaignsScriptPromise;
}

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
    account.preset = who.preset || '';
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
  // Awaited: init()'s caller navigates (applyURL -> go) right after this resolves, and a
  // manager deep-linked or refreshed on a campaigns.js view needs loaders.campaigns to
  // already be registered before that happens — see applyAccount's own comment.
  await applyAccount();
  return account.hosted && !!account.tenant;
}

async function loadOptions() {
  if (account.options || !account.hosted) return account.options;
  try { account.options = await ctl('/api/options'); } catch { account.options = null; }
  return account.options;
}

// ── account gate wiring ────────────────────────────────────────────────────

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
  // (it is not a traffic view), so the comparison carries its own range. Tenants is
  // manager-only and its markup is now a <template> until revealed, so #ab-range is wired
  // from wireManagerView instead of here — see revealManagerTemplates.

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
