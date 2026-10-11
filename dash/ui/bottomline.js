// The bottom line at the top of every page: one sentence, one number, a provenance badge,
// and a short ranked "What we recommend" list. Detail stays below, unchanged.
//
// No new backend field: every figure comes from an endpoint the page already reads.
// The models (ovModel, kaModel, invModel, ...) are PURE so dash/bottomline.test.mjs can run
// them under node; the render half below uses app.js's globals (api, el, $, clear).
'use strict';

const BL = (() => {
  const nf = new Intl.NumberFormat('en-US');
  const money = (v) => {
    const a = Math.abs(v);
    const s = a === 0 ? '$0' : a < 0.01 ? '$' + a.toFixed(4) : a < 1000 ? '$' + a.toFixed(2) : '$' + nf.format(Math.round(a));
    return v < 0 ? '-' + s : s;
  };
  const pc = (v) => (Math.abs(v) < 10 ? v.toFixed(1) : Math.round(v)) + '%';

  /** Overview: ONE net figure and the receipt that adds up to it. The parts are the terms of
   *  the server's own total_saved_usd (dash/savings.go savingsArithmetic + the declaration
   *  credit), so the receipt reconciles by construction; `other` carries any remainder. */
  function ovModel(o) {
    const priced = (o.accounting && o.accounting.complete) || 0;
    const known = priced > 0;
    const total = o.total_saved_usd || 0;
    const bill = (o.cost_usd || 0) + (o.cg_llm_cost_usd || 0) + (o.keepalive_ping_usd || 0);
    const net = o.net_saved_usd || 0, ka = o.keepalive_net_usd || 0, split = o.cachesplit_saved_usd || 0;
    const other = total - net - ka - split;
    const rows = [
      { label: 'Compaction, before our own model spend', usd: net + (o.cg_llm_cost_usd || 0), badge: 'Estimated',
        note: 'what the removed content would have cost, minus what was billed' },
      { label: 'context-guru’s own model spend', usd: -(o.cg_llm_cost_usd || 0), badge: 'Observed',
        note: 'summarizer and extractor calls we paid for' },
      { label: 'Keep-alive, net', usd: ka, badge: 'Estimated',
        note: 'cache re-creations avoided, minus ' + money(o.keepalive_ping_usd || 0) + ' of pings (Observed)' },
      { label: 'Prefix split', usd: split, badge: 'Estimated', note: 'cache reads saved by splitting the volatile tail' },
    ];
    if (Math.abs(other) >= 0.005) rows.push({ label: 'Declarations dropped', usd: other, badge: 'Estimated', note: 'tool and skill schemas filtered out' });
    const partial = known && priced < (o.requests || 0);
    let sentence;
    if (!known) sentence = 'Not enough priced requests in this window to put a dollar figure on it yet.';
    else if (total >= 0) sentence = 'context-guru saved you ' + money(total) + ' net in this window' + (bill > 0 ? ', ' + pc((100 * total) / bill) + ' of your ' + money(bill) + ' bill.' : '.');
    else sentence = 'context-guru cost you ' + money(-total) + ' more than it saved in this window.';
    return {
      known, total, bill, rows, sentence, number: known ? money(total) : 'unknown',
      badge: known ? 'Estimated' : 'Unpriced',
      coverage: known ? 'priced on ' + nf.format(priced) + ' of ' + nf.format(o.requests || 0) + ' requests' + (partial ? ' (rest unpriced)' : '') : '',
      tone: !known ? '' : total < 0 ? 'bad' : 'good',
    };
  }

  /** Inventory: unused MCP servers, biggest carrying cost first. Only servers with NO tool used
   *  are offered for removal as a whole (the command removes the whole server). */
  function invModel(rep) {
    const t = (rep && rep.totals) || {};
    const priced = !!t.priced;
    const recs = ((rep && rep.servers) || [])
      .filter((s) => s.tools_used === 0 && s.unused_usd > 0)
      .sort((a, b) => b.unused_usd - a.unused_usd).slice(0, 3)
      .map((s) => ({ usd: s.unused_usd, text: 'Remove the ' + s.server + ' MCP server: ' + s.tools + ' tools, never called in ' + nf.format(s.sessions_declared) + ' sessions, about ' + money(s.unused_usd) + ' to carry in this window.', command: 'claude mcp remove ' + s.server, badge: 'Estimated' }));
    const unusedPct = t.declared_tokens > 0 ? (100 * t.unused_tokens) / t.declared_tokens : 0;
    return {
      sentence: priced ? 'You are paying to carry ' + nf.format(t.unused_tokens || 0) + ' tokens of tools you never used: about ' + money(t.unused_usd || 0) + ' in this window.'
        : 'Tool declarations were captured but could not be priced yet.',
      number: priced ? money(t.unused_usd || 0) : 'unknown', badge: priced ? 'Estimated' : 'Unpriced',
      coverage: t.declared_tokens ? pc(unusedPct) + ' of declared tokens never used' : '', tone: priced && t.unused_usd > 0 ? 'bad' : '', recs, rows: [],
    };
  }

  /** Keep-alive: the decision number first (the replay over your own idle gaps), what it has
   *  actually netted so far second, and the one setting we suggest. */
  function kaModel(rec, ka) {
    const lo = rec && rec.lo_usd, hi = rec && rec.hi_usd;
    const have = rec && rec.n > 0 && lo !== undefined;
    const net = ka ? ka.net_usd : 0;
    let verdict;
    if (!have) verdict = 'Not enough idle gaps in this window to recommend a setting.';
    else if (lo > 0) verdict = 'Keep-alive is expected to save ' + money(lo) + ' to ' + money(hi) + ' over a window like this one.';
    else if (hi <= 0) verdict = 'Keep-alive is not expected to pay for itself on this traffic (' + money(lo) + ' to ' + money(hi) + ').';
    else verdict = 'Keep-alive could go either way on this traffic (' + money(lo) + ' to ' + money(hi) + ').';
    const recs = have && hi > 0 ? [{ usd: hi, text: 'Use ' + rec.idle_seconds + ' s idle and at most ' + rec.max_pings + ' pings.', href: '#/admin/settings', hrefLabel: 'Open Settings', badge: 'Estimated' }] : [];
    return {
      sentence: verdict, number: have ? money(lo) + ' to ' + money(hi) : 'unknown', badge: have ? 'Estimated' : 'Unpriced',
      coverage: have ? 'replay of ' + nf.format(rec.n) + ' of your own expiries' : '',
      observed: ka ? 'Measured so far: ' + money(net) + ' net after ' + money(ka.ping_usd || 0) + ' of pings, an upper bound (Observed).' : '',
      tone: have && lo > 0 ? 'good' : have && hi <= 0 ? 'bad' : '', recs, rows: [],
    };
  }

  /** Usage: what you spent, and how much of the input the provider's cache already served. */
  function usageModel(o) {
    const bill = (o.cost_usd || 0) + (o.cg_llm_cost_usd || 0) + (o.keepalive_ping_usd || 0);
    const input = (o.cache_read || 0) + (o.cache_write || 0) + (o.fresh_input || 0);
    const hit = input > 0 ? (100 * o.cache_read) / input : null;
    const recs = [];
    if (o.prefix_change_requests_all > 0 && o.prefix_change_cost_all_usd > 0) {
      recs.push({ usd: o.prefix_change_cost_all_usd, text: nf.format(o.prefix_change_requests_all) + ' turns were re-billed because the start of the prompt changed (' + money(o.prefix_change_cost_all_usd) + '). Find what edits the start of your prompt between turns.', href: '#/savings/usage', hrefLabel: 'See the cache section', badge: 'Observed' });
    }
    return {
      sentence: 'You spent ' + money(bill) + ' in this window' + (hit === null ? '.' : '; the provider’s prompt cache served ' + pc(hit) + ' of your input.'),
      number: money(bill), badge: 'Observed', coverage: '', tone: '', recs, rows: [],
    };
  }

  /** Sessions: where the money went. */
  function sessionsModel(rows) {
    const list = (rows || []).slice().sort((a, b) => b.cost_usd - a.cost_usd);
    const total = list.reduce((s, r) => s + r.cost_usd, 0);
    const top = list[0];
    return {
      sentence: list.length ? nf.format(list.length) + ' sessions cost ' + money(total) + '; the costliest alone was ' + money(top.cost_usd) + ' (' + pc((100 * top.cost_usd) / total) + ').' : 'No sessions in this window.',
      number: money(total), badge: 'Observed', coverage: '', tone: '', recs: [], rows: [],
    };
  }

  /** Requests. */
  function requestsModel(o) {
    const n = o.requests || 0, cost = (o.cost_usd || 0);
    return {
      sentence: nf.format(n) + ' requests in this window' + (n ? ', ' + money(cost / n) + ' each on average.' : '.'),
      number: nf.format(n), badge: 'Observed', coverage: '', tone: '', recs: [], rows: [],
    };
  }

  /** Overview recommendations, ranked by dollars. Each source is optional. */
  function rank(...lists) {
    return [].concat(...lists).filter(Boolean).sort((a, b) => b.usd - a.usd).slice(0, 3);
  }

  return { money, ovModel, invModel, kaModel, usageModel, sessionsModel, requestsModel, rank };
})();

if (typeof module !== 'undefined') module.exports = BL;

// ── render half (browser only) ──────────────────────────────────────────────
if (typeof document !== 'undefined' && typeof el === 'function') {
  const ADVANCED = new Set(['components', 'kvcache', 'campaigns', 'benchmarks']);
  const mounts = new Map();

  function host(view) {
    for (const [v, m] of mounts) m.hidden = v !== view;
    const panel = document.getElementById('view-' + view);
    if (!panel) return null;
    let h = mounts.get(view);
    if (h) h.hidden = false;
    if (!h || !h.isConnected) {
      h = el('section', { class: 'bl', 'data-testid': 'bottom-line', 'aria-label': 'Bottom line' });
      // A sibling BEFORE the panel, not a child: Inventory and KV-cache rebuild their whole
      // section on every load and would wipe a banner placed inside it.
      panel.before(h);
      mounts.set(view, h);
    }
    return h;
  }

  function badge(kind) { return el('span', { class: 'badge ' + kind.toLowerCase(), text: kind }); }

  function receipt(m) {
    const t = el('table', { class: 'bl-receipt-table' },
      el('caption', { text: 'How this number is made' }),
      ...m.rows.map((r) => el('tr', {},
        el('th', { scope: 'row', text: r.label }),
        el('td', { class: 'num ' + (r.usd < 0 ? 'neg' : ''), text: BL.money(r.usd) }),
        el('td', {}, badge(r.badge)),
        el('td', { class: 'note', text: r.note }))),
      el('tr', { class: 'sum' }, el('th', { scope: 'row', text: 'Net' }), el('td', { class: 'num', text: m.number }), el('td'), el('td', { class: 'note', text: 'Provider prompt-cache savings are not counted: that is the provider’s mechanism, not ours.' })));
    return el('span', { class: 'bl-receipt' },
      el('button', { type: 'button', class: 'bl-receipt-btn', 'aria-label': 'Show how this number is made' }, 'How is this made?'),
      el('div', { class: 'bl-receipt-pop', role: 'region', 'aria-label': 'Receipt' }, t));
  }

  function recList(recs) {
    if (!recs || !recs.length) return null;
    return el('div', { class: 'bl-recs' }, el('h2', { text: 'What we recommend' }),
      el('ol', {}, ...recs.map((r) => el('li', {},
        el('span', { class: 'bl-rec-text', text: r.text }), ' ', badge(r.badge || 'Estimated'),
        r.command ? el('code', { class: 'bl-cmd', text: r.command }) : null,
        r.command ? el('button', { type: 'button', class: 'btn small', text: 'Copy', onclick: (ev) => {
          try { navigator.clipboard.writeText(r.command); ev.currentTarget.textContent = 'Copied'; } catch (_) { /* clipboard needs a secure context */ }
        } }) : null,
        r.href ? el('a', { class: 'btn small', href: r.href, text: r.hrefLabel }) : null))));
  }

  function paint(view, m, extra) {
    const h = host(view);
    if (!h) return;
    clear(h);
    h.dataset.tone = m.tone || '';
    h.appendChild(el('div', { class: 'bl-main' },
      el('div', { class: 'bl-num', 'data-testid': 'bottom-line-number', text: m.number }),
      el('div', { class: 'bl-text' },
        el('p', { class: 'bl-sentence', 'data-testid': 'bottom-line-sentence', text: m.sentence }),
        el('p', { class: 'bl-meta' }, badge(m.badge), m.coverage ? ' ' + m.coverage : '', m.rows && m.rows.length ? ' ' : '', m.rows && m.rows.length ? receipt(m) : null),
        m.observed ? el('p', { class: 'bl-meta', text: m.observed }) : null)));
    const rl = recList(extra || m.recs);
    if (rl) h.appendChild(rl);
  }

  async function overview(o) {
    const m = BL.ovModel(o);
    paint('overview', m);
    try {
      const [rep, rec] = await Promise.all([api('tools'), api('keepalive/recommend')]);
      const ka = BL.kaModel(rec, null);
      const kaRec = ka.recs.map((r) => ({ ...r, text: 'Keep-alive: ' + r.text.charAt(0).toLowerCase() + r.text.slice(1) + ' Expected ' + ka.number + '.', href: '#/savings/keepalive', hrefLabel: 'Open Keep-alive' }));
      const pre = BL.usageModel(o).recs;
      paint('overview', m, BL.rank(BL.invModel(rep).recs, kaRec, pre));
    } catch (e) { if (typeof aborted === 'function' && aborted(e)) return; /* recommendations are best-effort */ }
  }

  async function render(view) {
    try {
      host(view); // shows this view's banner and hides the others
      if (view === 'overview') return; // loadOverview calls overview(o) with the stats it fetched
      if (ADVANCED.has(view)) {
        const h = host(view);
        if (h && !h.firstChild) {
          h.dataset.tone = 'note';
          h.appendChild(el('p', { class: 'bl-advanced' }, el('strong', { text: 'Advanced view. ' }),
            'This is an operator and research page. For the plain answer to “is it saving me money?” open the ',
            el('a', { href: '#/overview', text: 'Overview' }), '.'));
        }
        return;
      }
      if (view === 'keepalive') { const [rec, ka] = await Promise.all([api('keepalive/recommend'), api('keepalive')]); paint(view, BL.kaModel(rec, ka)); }
      else if (view === 'tools') paint(view, BL.invModel(await api('tools')));
      else if (view === 'usage') paint(view, BL.usageModel(await api('stats')));
      else if (view === 'requests') paint(view, BL.requestsModel(await api('stats')));
      else if (view === 'sessions') paint(view, BL.sessionsModel((await api('sessions', { limit: 500 })).sessions));
    } catch (e) { /* aborted by a newer filter state, or best-effort */ }
  }

  window.CGBL = { overview, render };
  if (typeof state !== 'undefined' && state.view) render(state.view);
}
