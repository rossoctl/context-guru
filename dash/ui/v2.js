// Five-page dashboard: Home, Savings, Cache, Tools & context, Sessions.
// Every page: bottom line, ranked recommendations, one chart, details behind a click.
// All figures come from the models in v2model.js; this file only fetches and draws.
'use strict';
(() => {
  const M = V2;
  const NS = 'http://www.w3.org/2000/svg';
  const $ = (id) => document.getElementById(id);

  function h(tag, attrs, ...kids) {
    const e = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (v === null || v === undefined || v === false) continue;
      if (k === 'text') e.textContent = v;
      else if (k.startsWith('on')) e.addEventListener(k.slice(2), v);
      else e.setAttribute(k, v === true ? '' : v);
    }
    for (const k of kids.flat()) if (k !== null && k !== undefined && k !== false) e.append(k);
    return e;
  }
  function s(tag, attrs, ...kids) {
    const e = document.createElementNS(NS, tag);
    for (const [k, v] of Object.entries(attrs || {})) e.setAttribute(k, v);
    for (const k of kids.flat()) if (k) e.append(k);
    return e;
  }
  /** A jargon word with its plain-words meaning, readable on hover and on keyboard focus. */
  const term = (word, tip) => h('span', { class: 'term', tabindex: '0' }, word, h('span', { class: 'tip', role: 'tooltip', text: tip }));
  const BADGE = { observed: 'Measured', saved: 'Saved', estimated: 'Estimated', forecast: 'Estimated', ceiling: 'Estimated' };
  const badge = (kind, label) => h('span', { class: 'badge ' + kind, text: label || BADGE[kind] || kind });

  const PAGES = [
    ['home', 'Home'], ['savings', 'Savings'], ['cache', 'Cache'], ['tools', 'Tools & context'], ['sessions', 'Sessions'],
  ];
  const CLASSIC = 'index.html';
  const state = { page: 'home', period: '30d', ac: null, seq: 0 };

  // ── data ──────────────────────────────────────────────────────────────────
  const memo = new Map();
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  /** GET /api/<path>. A cold read of a big history can answer 503 while it finishes in the
   *  background, so retry a few times, one second apart, before giving up. */
  async function api(path, params, tries = 5) {
    const u = new URL('/api/' + path, location.origin);
    for (const [k, v] of Object.entries(params || {})) if (v) u.searchParams.set(k, v);
    for (let i = 0; ; i++) {
      const res = await fetch(u, { headers: { accept: 'application/json' } });
      if (res.ok) return res.json();
      if ((res.status === 503 || res.status === 500) && i < tries) { await sleep(1000); continue; }
      const e = new Error(res.status === 401 ? 'Sign in required' : 'The server answered ' + res.status); e.status = res.status; throw e;
    }
  }
  function cached(name, params, path) {
    const key = name + '|' + JSON.stringify(params);
    if (!memo.has(key)) memo.set(key, api(path || name, params).catch((e) => { memo.delete(key); throw e; }));
    return memo.get(key);
  }
  const win = () => { const p = M.period(state.period, Date.now()); return { since: p.since, until: p.until }; };
  const data = {
    stats: () => cached('stats', { ...win(), lean: 1 }, 'stats'),
    series: () => cached('series', { ...win(), bucket: M.period(state.period, 0).bucket }, 'series'),
    tools: () => cached('tools', win()).then((x) => { if (!x || !x.totals) { memo.clear(); throw new Error('The tool history is not ready yet'); } return x; }),
    rec: () => cached('rec', win(), 'keepalive/recommend'),
    ledger: () => cached('ledger', win(), 'keepalive'),
    sessions: () => cached('sessions', { ...win(), limit: 100, sort: 'cost' }),
  };

  // ── charts ────────────────────────────────────────────────────────────────
  const dayLabel = (ts) => new Date(ts).toISOString().slice(5, 10);
  function hatch() {
    return s('defs', {}, s('pattern', { id: 'hatch-saved', width: 6, height: 6, patternUnits: 'userSpaceOnUse', patternTransform: 'rotate(45)' },
      s('rect', { width: 6, height: 6, fill: 'none' }), s('line', { x1: 0, y1: 0, x2: 0, y2: 6, class: 'hatch-line' })));
  }
  /** One series of daily bars. kind 'est' is hatched (priced after the fact), 'obs' is solid. */
  function bars(points, key, kind, label) {
    const W = 640, H = 200, L = 44, B = 22, T = 8;
    const max = Math.max(...points.map((p) => p[key]), 0) || 1;
    const bw = (W - L) / Math.max(points.length, 1);
    const svg = s('svg', { viewBox: `0 0 ${W} ${H}`, role: 'img', 'aria-label': label }, hatch());
    [0, 0.5, 1].forEach((f) => {
      const y = T + (H - B - T) * (1 - f);
      svg.append(s('line', { x1: L, x2: W, y1: y, y2: y, class: 'grid' }));
      const t = s('text', { x: L - 6, y: y + 4, 'text-anchor': 'end' }); t.textContent = M.money(max * f); svg.append(t);
    });
    points.forEach((p, i) => {
      const bh = ((H - B - T) * p[key]) / max;
      const r = s('rect', { x: L + i * bw + 1, y: H - B - Math.max(bh, p[key] > 0 ? 1 : 0), width: Math.max(bw - 2, 1), height: Math.max(bh, p[key] > 0 ? 1 : 0), class: kind === 'est' ? 'bar-est' : 'bar-obs' });
      const t = s('title'); t.textContent = dayLabel(p.ts) + ': ' + M.money(p[key]); r.append(t); svg.append(r);
    });
    [0, Math.floor((points.length - 1) / 2), points.length - 1].forEach((i, n) => {
      if (!points[i]) return;
      const t = s('text', { x: L + i * bw + bw / 2, y: H - 6, 'text-anchor': n === 0 ? 'start' : n === 2 ? 'end' : 'middle' }); t.textContent = dayLabel(points[i].ts); svg.append(t);
    });
    return svg;
  }
  /** 100% stacked daily bars: cache read, cache write, fresh input. */
  function tokenMix(points) {
    const W = 640, H = 200, L = 44, B = 22, T = 8;
    const bw = (W - L) / Math.max(points.length, 1);
    const svg = s('svg', { viewBox: `0 0 ${W} ${H}`, role: 'img', 'aria-label': 'Share of input tokens per day that came from the cache, were written to the cache, or were new.' });
    [0, 0.5, 1].forEach((f) => {
      const y = T + (H - B - T) * (1 - f);
      svg.append(s('line', { x1: L, x2: W, y1: y, y2: y, class: 'grid' }));
      const t = s('text', { x: L - 6, y: y + 4, 'text-anchor': 'end' }); t.textContent = Math.round(f * 100) + '%'; svg.append(t);
    });
    points.forEach((p, i) => {
      const tot = p.read + p.write + p.fresh; if (!tot) return;
      let y = H - B;
      [['read', 'seg-read', 'from cache'], ['write', 'seg-write', 'written to cache'], ['fresh', 'seg-fresh', 'new']].forEach(([k, c, name]) => {
        const hh = ((H - B - T) * p[k]) / tot; y -= hh;
        const r = s('rect', { x: L + i * bw + 1, y, width: Math.max(bw - 2, 1), height: hh, class: c });
        const t = s('title'); t.textContent = dayLabel(p.ts) + ': ' + M.pct((100 * p[k]) / tot) + ' ' + name; r.append(t); svg.append(r);
      });
    });
    [0, Math.floor((points.length - 1) / 2), points.length - 1].forEach((i, n) => {
      if (!points[i]) return;
      const t = s('text', { x: L + i * bw + bw / 2, y: H - 6, 'text-anchor': n === 0 ? 'start' : n === 2 ? 'end' : 'middle' }); t.textContent = dayLabel(points[i].ts); svg.append(t);
    });
    return svg;
  }
  function hbars(rows, fmt, cls) {
    const max = Math.max(...rows.map((r) => r.v), 0) || 1;
    return h('div', { role: 'list' }, rows.map((r) => {
      const fill = h('span', { class: 'fill ' + (cls || '') }); fill.style.width = Math.max((100 * r.v) / max, 1) + '%';
      return h('div', { class: 'hbar', role: 'listitem' }, h('span', { text: r.name }), h('span', { class: 'track' }, fill), h('span', { class: 'num', text: fmt(r.v) }));
    }));
  }
  const chartBox = (title, sub, body, legend) => h('section', { class: 'chart' }, h('h3', { text: title }), h('p', { class: 'sub', text: sub }), body, legend);
  const legendItem = (cls, text) => { const i = h('i'); i.className = cls; return h('span', {}, i, text); };

  // ── shared pieces ─────────────────────────────────────────────────────────
  const skeleton = (n = 3) => h('div', { 'aria-busy': 'true', 'aria-label': 'Loading' }, Array.from({ length: n }, () => { const e = h('div', { class: 'skeleton' }); e.style.margin = '12px 0'; e.style.height = '28px'; return e; }));
  const failure = (e) => h('div', { class: 'notice', role: 'alert' }, 'This part did not load: ' + e.message + '. ', h('a', { href: '#', text: 'Try again', onclick: (ev) => { ev.preventDefault(); route(); } }));

  /** The bottom line: one number, one sentence, one provenance badge. */
  function bottomLine({ number, sentence, kind, meta, tone, realised }) {
    const card = h('section', { class: 'bl card' + (realised ? ' realised' : ''), 'data-tone': tone || '', 'data-testid': 'bottom-line', 'aria-label': 'Bottom line' },
      h('div', { class: 'bl-num', 'data-testid': 'bottom-line-number', text: number }),
      h('div', {}, h('p', { class: 'bl-sentence', 'data-testid': 'bottom-line-sentence' }, sentence), h('p', { class: 'bl-meta' }, badge(kind), ' ', meta || '')));
    return card;
  }
  /** The forecast card: dashed, amber, never summed with the realised number. */
  const forecastCard = (title, value, note, kind = 'forecast', action) => h('section', { class: 'card forecast', 'data-testid': 'forecast' },
    h('h3', {}, title, ' ', badge(kind)), h('div', { class: 'big', text: value }), h('p', { text: note }),
    action ? h('div', { class: 'card-act' }, action.type === 'cmd' ? copyBtn(action.cmd, action.label) : h('a', { class: 'btn', href: action.href, text: action.label })) : null);
  const topGrid = (a, b) => { const g = h('div', { class: 'top-grid' }, a, b); return g; };
  const statCard = (label, value, note, kind) => h('div', { class: 'card' }, h('h3', {}, label), h('div', { class: 'big', text: value }), h('p', {}, kind ? [badge(kind), ' '] : '', note));

  function copyBtn(cmd, label) {
    const b = h('button', { type: 'button', class: 'btn', text: label || 'Copy' });
    b.addEventListener('click', async () => {
      try { await navigator.clipboard.writeText(cmd); b.textContent = 'Copied'; } catch (_) { b.textContent = 'Select and copy'; }
    });
    return b;
  }
  /** "What we recommend": ranked, each with expected dollars and one action. */
  function recList(recs, empty, loading) {
    const box = h('section', { 'aria-labelledby': 'recs-h' }, h('h2', { id: 'recs-h', text: 'What we recommend' }));
    if (!recs.length) { box.append(h('p', { class: 'empty', text: loading ? 'Checking your tool servers and idle gaps…' : empty || 'Nothing to fix in this period.' })); return box; }
    const item = (r) => {
      const usd = r.alt ? r.alt : r.kind === 'ceiling' ? 'up to ' + M.money(r.high) : M.money(r.low);
      const act = r.action.type === 'cmd' ? [copyBtn(r.action.cmd)] : [h('a', { class: 'btn ghost', href: r.action.href, text: r.action.label })];
      return h('li', { class: 'rec' },
        h('p', { class: 'rec-text', text: r.text }),
        h('div', { class: 'rec-act' }, h('span', { class: 'rec-usd' }, usd, ' ', badge(r.kind === 'ceiling' ? 'ceiling' : 'estimated')), act),
        r.action.type === 'cmd' ? h('pre', { class: 'cmd rec-cmd', text: r.action.cmd }) : null);
    };
    box.append(h('ol', { class: 'recs', 'data-testid': 'recs' }, item(recs[0])));
    if (recs.length > 1) box.append(detail('More recommendations (' + (recs.length - 1) + ')', h('ol', { class: 'recs', start: '2' }, recs.slice(1).map(item))));
    if (loading) box.append(h('p', { class: 'muted small', text: 'Still reading your tool history. More may appear.' }));
    return box;
  }
  const detail = (title, body) => h('details', {}, h('summary', { text: title }), h('div', {}, body));
  const table = (head, rows, rightFrom = 1) => h('table', {}, h('thead', {}, h('tr', {}, head.map((c, i) => h('th', { scope: 'col', class: i >= rightFrom ? 'r' : '', text: c })))),
    h('tbody', {}, rows.map((r) => h('tr', {}, r.map((c, i) => (i === 0 ? h('th', { scope: 'row' }, c) : h('td', { class: i >= rightFrom ? 'r' : '' }, c)))))));

  /** Recommendations arrive in two steps: the fast ones (cache, keep-alive) paint at once, and the
   *  tool-server ones are added when the slow tool history has been read. */
  /** True when this one account already has keep-alive on. For a manager's all-accounts view it is never true. */
  const kaOn = () => !!(me && me.tenant && me.tenant.role !== 'manager' && me.tenant.effective_config && me.tenant.effective_config.cache && me.tenant.effective_config.cache.keepalive);
  async function liveRecs(filter, paint) {
    const keep = filter || (() => true);
    const [st, rec] = await Promise.all([data.stats(), data.rec().catch(() => null)]);
    const quick = M.recommend({ rec, cache: M.cache(st), keepaliveOn: kaOn() }).filter(keep);
    paint(quick, true);
    try {
      const tl = await data.tools();
      paint(M.recommend({ tools: M.tools(tl), rec, cache: M.cache(st), keepaliveOn: kaOn() }).filter(keep), false);
    } catch (_) { paint(quick, false); }
  }

  // ── pages ─────────────────────────────────────────────────────────────────
  const pages = {};

  pages.home = async (root) => {
    const st = await data.stats();
    root.replaceChildren();
    const sv = M.saved(st), c = M.cache(st);
    const per = M.PERIODS[state.period].label.toLowerCase();
    const sentence = !sv.known ? 'Not enough priced requests in this period to put a dollar figure on it.'
      : sv.realised >= 0 ? ['context-guru saved an estimated ', M.money(sv.realised), ' ', per.startsWith('all') ? 'so far' : 'in this period', ', ', M.pct(sv.share), ' off what you would have paid.']
        : ['context-guru cost ', M.money(-sv.realised), ' more than it saved ', per.startsWith('all') ? 'so far' : 'in this period', '.'];
    const meta = [sv.coverage ? sv.coverage + ' · ' : '', h('a', { href: '#savings', text: 'How this is worked out' })];
    const fc = h('div', { id: 'home-fc' }, forecastCard('Best next step', '…', 'Looking at your tool use and idle gaps.'));
    root.append(topGrid(bottomLine({ number: sv.known ? M.money(sv.realised) : 'Unknown', sentence, kind: sv.known ? 'saved' : 'observed', meta, realised: true, tone: sv.known ? (sv.realised >= 0 ? 'good' : 'bad') : '' }), fc));
    root.append(h('div', { class: 'stats' },
      statCard('You paid', M.money(sv.bill), 'to the model provider', 'observed'),
      statCard('Served from cache', M.pct(c.hit), ['of your input, at a ', term('tenth of the price', 'The provider re-uses a prompt it has already read and charges about a tenth for it. This is the provider’s feature.'), '.'], 'observed')));
    const recBox = h('div', {}, skeleton(2)); root.append(recBox);
    const chartBox_ = h('div', {}, skeleton(1)); root.append(chartBox_);
    liveRecs(null, (recs, loading) => {
      const top = recs[0];
      fc.replaceChildren(top ? forecastCard('Best next step', top.high && top.low === 0 ? 'up to ' + M.money(top.high) : M.money(top.low), top.short || top.text, top.kind === 'ceiling' ? 'ceiling' : 'forecast', top.action) : forecastCard('Best next step', loading ? '…' : '$0', loading ? 'Checking your tool servers and idle gaps.' : 'Nothing to fix in this period.'));
      recBox.replaceChildren(recList(recs.slice(0, 3), null, loading));
    }).catch(() => {});
    const ser = await data.series().catch((e) => e);
    chartBox_.replaceChildren(ser instanceof Error ? failure(ser) : chartBox('What you paid each day', 'Dollars billed by the model provider, per day (UTC).', bars(M.daily(ser.buckets), 'bill', 'obs', 'Bar chart of dollars paid per day.'), h('div', { class: 'legend' }, legendItem('bar-obs-key', 'Observed: what the provider billed'))));
  };

  pages.savings = async (root) => {
    const st = await data.stats();
    root.replaceChildren();
    const sv = M.saved(st);
    const per = M.PERIODS[state.period].label.toLowerCase();
    root.append(topGrid(
      bottomLine({
        number: sv.known ? M.money(sv.realised) : 'Unknown',
        sentence: sv.known ? ['Without context-guru this period would have cost ', M.money(sv.would), '. You paid ', M.money(sv.bill), '.'] : 'Not enough priced requests to put a dollar figure on it.',
        kind: sv.known ? 'saved' : 'observed', meta: [sv.coverage], realised: true, tone: sv.known && sv.realised >= 0 ? 'good' : sv.known ? 'bad' : '',
      }),
      sv.ka > 0 ? forecastCard('Keep-alive, counted apart', 'up to ' + M.money(sv.ka), 'The most it could have saved. Not in the green number.', 'ceiling')
        : forecastCard('Keep-alive', 'Not on yet', 'Turn it on and its savings show here, apart from the green number.', 'forecast')));
    const recBox = h('div', {}, skeleton(2)); root.append(recBox);
    const chartHost = h('div', {}, skeleton(1)); root.append(chartHost);
    root.append(detail('How the number is built', table(['Source', 'Dollars', 'Kind'],
      [...sv.parts.map((p) => [term(p.label, p.tip), M.money(p.usd), badge('estimated')]),
        [h('strong', { text: 'Saved (estimated)' }), h('strong', { text: M.money(sv.realised) }), ''],
        [term('Keep-alive (at most)', 'Cache rewrites that pings may have avoided, minus the pings paid for. Other sessions can fill the same cache, so this is a ceiling.'), M.money(sv.ka), badge('ceiling')],
        [term('Provider cache discount (not counted)', 'The provider charges a tenth for cached input with or without us, so we do not claim it.'), M.money(M.cache(st).providerSavedUsd), badge('observed')]], 1)));
    liveRecs((r) => r.id === 'keepalive' || r.id.startsWith('mcp'), (recs, loading) => recBox.replaceChildren(recList(recs.slice(0, 3), null, loading))).catch(() => {});
    const ser = await data.series().catch((e) => e);
    chartHost.replaceChildren(ser instanceof Error ? failure(ser) : chartBox('Saved each day', 'Estimated dollars saved per day by trimming prompts, after our own model calls (UTC).', bars(M.daily(ser.buckets), 'saved', 'est', 'Bar chart of estimated dollars saved per day.'), h('div', { class: 'legend' }, legendItem('bar-est-key', 'Estimated: priced against what would have been billed'))));
  };

  pages.cache = async (root) => {
    const [st, led] = await Promise.all([data.stats(), data.ledger().catch(() => null)]);
    root.replaceChildren();
    const c = M.cache(st);
    const lost = led && led.addressable_usd > 0;
    const hero = lost
      ? bottomLine({ number: M.money(led.addressable_usd), sentence: ['You paid ', M.money(led.addressable_usd), ' extra when a session sat idle and the ', term('cache', 'The provider re-uses a prompt it has already read, at a tenth of the normal price.'), ' expired.'], kind: 'estimated', meta: [M.num(led.addressable_misses), ' times; ', M.pct(c.hit), ' of input still came from cache.'], tone: '' })
      : bottomLine({ number: M.pct(c.hit), sentence: ['The provider’s ', term('cache', 'The provider re-uses a prompt it has already read, at a tenth of the normal price.'), ' served ', M.pct(c.hit), ' of the input you sent.'], kind: 'observed', meta: ['No idle-timeout losses found in this period.'], tone: '' });
    root.append(topGrid(hero, forecastCard('Keep-alive, expected', '…', 'Replay of your own idle gaps.')));
    const recBox = h('div', {}, skeleton(2)); root.append(recBox);
    const chartHost = h('div', {}, skeleton(1)); root.append(chartHost);
    root.append(detail('Why turns missed the cache', table(['Reason', 'Turns'], [
      [term('Timer ran out', 'The cache forgets a prompt after 5 idle minutes. The next turn pays full price to write it again.'), M.num(c.misses.expired)],
      [term('Start of prompt changed', 'Something near the top of the prompt differed from last turn, so none of it matched.'), M.num(c.misses.changed)],
      [term('First turn', 'The very first request of a session has nothing cached yet.'), M.num(c.misses.cold)],
      [term('Not classified', 'The log did not say why. Counted so the table adds up.'), M.num(c.misses.unknown)]])));
    liveRecs((r) => r.id === 'keepalive' || r.id === 'prefix', (recs) => recBox.replaceChildren(recList(recs))).catch(() => {});
    const [ser, rec] = await Promise.all([data.series().catch((e) => e), data.rec().catch(() => null)]);
    const ka = rec && rec.n > 0 && rec.hi_usd > 0;
    document.querySelector('.top-grid > .forecast').replaceWith(ka ? forecastCard('Keep-alive, expected', M.money(rec.lo_usd) + ' to ' + M.money(rec.hi_usd), 'Over a period like this one, replayed on ' + M.num(rec.n) + ' of your own idle gaps.') : forecastCard('Keep-alive, expected', 'Unknown', 'Not enough idle gaps in this period to say.'));
    chartHost.replaceChildren(ser instanceof Error ? failure(ser) : chartBox('Where your input went', 'Share of input tokens each day (UTC). More green is cheaper.', tokenMix(M.daily(ser.buckets)),
      h('div', { class: 'legend' }, legendItem('seg-read', 'Read from cache (cheap)'), legendItem('seg-write', 'Written to cache'), legendItem('seg-fresh', 'New, full price'))));
  };

  pages.tools = async (root) => {
    const [st, raw] = await Promise.all([data.stats(), data.tools()]);
    root.replaceChildren();
    const t = M.tools(raw), per = M.PERIODS[state.period].label.toLowerCase();
    const avg = st.requests ? st.tokens_before / st.requests : null;
    root.append(bottomLine(t.priced ? {
        number: M.money(t.unusedUsd),
        sentence: ['You paid about ', M.money(t.unusedUsd), ' to carry ', M.tokens(t.unusedTokens), ' tokens of ', term('tool definitions', 'Each request lists every tool the agent may call. The list is re-read, and billed, on every turn.'), ' that were never called.'],
        kind: 'estimated', meta: [], tone: '',
      } : {
        number: M.pct(t.unusedPct),
        sentence: ['of the ', term('tool definitions', 'Each request lists every tool the agent may call. The list is re-read, and billed, on every turn.'), ' you send were never called. Dollars are missing because prices for these models are not loaded.'],
        kind: 'observed', meta: [], tone: '',
      }));
    const recs = M.recommend({ tools: t }).slice(0, 5);
    root.append(recList(recs, t.servers.length ? 'Every tool server was called at least once.' : 'No tool servers were declared in this period.'));
    root.append(chartBox('Unused tools by server', t.priced ? 'Estimated dollars spent carrying tools that were never called, by server.' : 'Tool-definition tokens sent every turn, by server.',
      t.priced ? hbars(t.servers.filter((x) => x.unused_usd > 0).slice(0, 8).map((x) => ({ name: x.server, v: x.unused_usd })), M.money, 'opp') : hbars(t.servers.filter((x) => x.tokens > 0).sort((a, b) => b.tokens - a.tokens).slice(0, 8).map((x) => ({ name: x.server, v: x.tokens })), M.tokens, 'opp')));
    root.append(detail('Prompt size', table(['Measure', 'Value'], [['Average prompt per request', M.tokens(avg) + ' tokens'], ['Share of prompt we removed', M.pct(st.tokens_before ? (100 * st.saved_gross) / st.tokens_before : null)], ['Tool definitions never called', M.tokens(t.unusedTokens) + ' tokens a turn']])));
    root.append(detail('All tool servers', table(['Server', 'Tools', 'Tools used', 'Carrying cost'], t.servers.map((x) => [x.server, M.num(x.tools), M.num(x.tools_used), M.money(x.unused_usd)]), 1)));
    root.append(h('p', { class: 'muted small' }, 'Per-tool removal commands: ', h('a', { href: 'index.html#/behaviour/inventory', text: 'classic Inventory' }), '.'));
  };

  let openDrawer = null;
  function drawer(row) {
    if (openDrawer) openDrawer.remove();
    const mix = row.cache_read + row.cache_write + row.fresh_input;
    const d = h('aside', { class: 'drawer', role: 'dialog', 'aria-label': 'Session detail' },
      h('button', { type: 'button', class: 'btn ghost', text: 'Close', onclick: () => { d.remove(); openDrawer = null; } }),
      h('h2', { text: 'Session ' + M.shortId(row.session_id) }),
      h('dl', {},
        ...[['Started', M.utc(row.start)], ['Agent', row.agents || '—'], ['Main model', row.primary_model || '—'], ['Turns', M.num(row.turns)], ['Cost', M.money(row.cost_usd)],
          ['Saved (estimate)', M.money(row.saved_usd - (row.cg_llm_cost_usd || 0))], ['Served from cache', M.pct(mix ? (100 * row.cache_read) / mix : null)], ['Output tokens', M.tokens(row.output_tokens)]]
          .flatMap(([k, v]) => [h('dt', { text: k }), h('dd', { text: v })])),
      h('a', { href: 'index.html#/traffic/sessions', text: 'Open the transcript in the classic dashboard' }));
    document.body.append(d); openDrawer = d; d.querySelector('button').focus();
  }
  pages.sessions = async (root) => {
    const [raw, st] = await Promise.all([data.sessions(), data.stats()]);
    root.replaceChildren();
    const m = M.sessions(raw.sessions, raw.total, st.cost_usd), per = M.PERIODS[state.period].label.toLowerCase();
    root.append(bottomLine({
      number: M.money(m.bill),
      sentence: m.count ? [M.num(m.count), ' sessions cost ', M.money(m.bill), ' in requests, before pings. The top ten made up ', M.pct(m.topShare), '.'] : 'No sessions in this period.',
      kind: 'observed', meta: [],
    }));
    const recs = m.list.slice(0, 1).map((r) => {
      return { low: r.cost_usd, kind: 'estimate', text: 'Open session ' + M.shortId(r.session_id) + ' first: it cost ' + (m.typical > 0 ? (r.cost_usd / m.typical).toFixed(1) + 'x the typical session' : 'the most') + ' over ' + M.num(r.turns) + ' turns.', action: { type: 'link', label: 'Open', href: '#sessions' }, session: r };
    });
    root.append(h('section', {}, h('h2', { text: 'What we recommend' }), 
      h('ol', { class: 'recs' }, recs.map((r) => h('li', { class: 'rec' }, h('p', { class: 'rec-text', text: r.text }), h('div', { class: 'rec-act' }, h('span', { class: 'rec-usd' }, M.money(r.low), ' ', badge('observed')), h('button', { type: 'button', class: 'btn ghost', text: 'Open', onclick: () => drawer(r.session) })))))));
    root.append(chartBox('Ten costliest sessions', 'Dollars billed per session in the ' + per + '.', hbars(m.list.slice(0, 10).map((r) => ({ name: M.shortId(r.session_id), v: r.cost_usd })), M.money)));
    root.append(detail('The ' + m.list.length + ' costliest sessions', table(['Started (UTC)', 'Agent', 'Turns', 'Cost', 'Saved', 'From cache'], m.list.map((r) => {
      const mix = r.cache_read + r.cache_write + r.fresh_input;
      const tr = [M.utc(r.start), r.agents || '—', M.num(r.turns), M.money(r.cost_usd), M.money(r.saved_usd - (r.cg_llm_cost_usd || 0)), M.pct(mix ? (100 * r.cache_read) / mix : null)];
      return tr;
    }), 2)));
  };

  // ── shell ─────────────────────────────────────────────────────────────────
  function parseHash() {
    // A bookmark to a classic view (#/savings/usage, #/overview) opens the classic dashboard.
    if (M.isClassicHash(location.hash)) { location.replace(CLASSIC + location.hash); return { page: 'home', period: '30d' }; }
    const [path, q] = location.hash.replace(/^#\/?/, '').split('?');
    const page = PAGES.some((p) => p[0] === path) ? path : 'home';
    const per = new URLSearchParams(q || '').get('p');
    return { page, period: M.PERIODS[per] ? per : '30d' };
  }
  const link = (page) => '#' + page + (state.period === '30d' ? '' : '?p=' + state.period);

  function chrome() {
    $('nav').replaceChildren(...PAGES.map(([id, label]) => h('a', { href: link(id), 'aria-current': id === state.page ? 'page' : null, text: label })));
    $('period').replaceChildren(...Object.entries(M.PERIODS).map(([k, v]) => h('button', { type: 'button', 'aria-pressed': String(k === state.period), text: v.label.replace('Last ', ''), onclick: () => { location.hash = state.page + (k === '30d' ? '' : '?p=' + k); } })));
    document.title = (PAGES.find((p) => p[0] === state.page) || PAGES[0])[1] + ' · context-guru';
  }
  async function scope(me) {
    const who = me.tenant ? (me.tenant.role === 'manager' ? 'All accounts' : me.tenant.label || 'Your account') : 'This proxy';
    $('scope').replaceChildren('Scope: ', h('strong', { text: who }), ' · Period: ', h('strong', { text: M.PERIODS[state.period].label }), ' · ', h('span', { id: 'fresh', text: 'checking data age…' }));
    try {
      const ser = await data.series();
      const last = [...ser.buckets].reverse().find((b) => b.requests > 0);
      $('fresh').textContent = last ? 'Data through ' + M.utc(last.ts + ser.bucket_ms - 1).slice(0, 10) + ', loaded ' + M.utc(Date.now()).slice(11) : 'No requests in this period';
    } catch (_) { $('fresh').textContent = 'Data age unknown'; }
  }

  let me = null;
  async function route() {
    const r = parseHash(); state.page = r.page; state.period = r.period;
    const seq = ++state.seq;
    if (openDrawer) { openDrawer.remove(); openDrawer = null; }
    chrome();
    const root = $('app');
    root.replaceChildren(h('h1', { text: (PAGES.find((p) => p[0] === state.page) || PAGES[0])[1] }), skeleton(3));
    try {
      if (!me) me = await fetch('/api/whoami').then((x) => x.json());
      if (me.hosted && !me.authenticated) { root.replaceChildren(h('h1', { text: 'Sign in' }), h('p', {}, h('a', { class: 'btn', href: CLASSIC, text: 'Sign in to context-guru' }))); return; }
      scope(me);
      const body = h('div', {}, skeleton(3));
      root.replaceChildren(h('h1', { text: (PAGES.find((p) => p[0] === state.page) || PAGES[0])[1] }), body);
      await pages[state.page](body);
    } catch (e) {
      if (seq === state.seq) root.replaceChildren(h('h1', { text: 'Could not load this page' }), failure(e));
    }
  }

  // Theme: auto, light, dark. Same storage key as the classic dashboard.
  function theme() {
    const btn = $('theme');
    const get = () => { try { return localStorage.getItem('cg-theme') || 'auto'; } catch (_) { return 'auto'; } };
    const apply = (t) => {
      if (t === 'auto') document.documentElement.removeAttribute('data-theme'); else document.documentElement.setAttribute('data-theme', t);
      btn.textContent = 'Theme: ' + t;
    };
    apply(get());
    btn.addEventListener('click', () => {
      const next = { auto: 'light', light: 'dark', dark: 'auto' }[get()];
      try { if (next === 'auto') localStorage.removeItem('cg-theme'); else localStorage.setItem('cg-theme', next); } catch (_) { /* private mode */ }
      apply(next);
    });
  }
  theme();
  window.addEventListener('hashchange', route);
  route();
})();
