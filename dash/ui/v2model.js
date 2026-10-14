// Pure models behind the five-page dashboard (ui/v2.js). No DOM, no fetch: every function takes
// an API payload and returns plain data, so dash/v2.test.mjs runs them under node.
//
// Rules the models enforce, so the pages cannot drift from them:
//   - realised savings (a measured or priced-after-the-fact figure) and opportunity (a forecast)
//     are different kinds of number and are never added together;
//   - an unpriced window says "unknown", never "$0";
//   - recommendations rank by a LOW bound, so a ceiling cannot outrank an estimate.
'use strict';

const V2 = (() => {
  const nf = new Intl.NumberFormat('en-US');
  const DAY = 86400000;

  /** Dollars: two decimals under $10, whole dollars above, so a column of them lines up. */
  const money = (v) => {
    if (v === null || v === undefined || Number.isNaN(v)) return '—';
    const a = Math.abs(v);
    const s = a === 0 ? '$0' : a < 0.01 ? '<$0.01' : a < 10 ? '$' + a.toFixed(2) : '$' + nf.format(Math.round(a));
    return v < 0 ? '−' + s : s;
  };
  /** Percent: one decimal under 10, whole above. */
  const pct = (v) => (v === null || v === undefined || Number.isNaN(v) ? '—' : (Math.abs(v) < 10 ? v.toFixed(1) : String(Math.round(v))) + '%');
  const num = (v) => (v === null || v === undefined ? '—' : nf.format(Math.round(v)));
  /** Token counts: 1.2B, 34.7M, 82k. */
  const tokens = (v) => {
    if (v === null || v === undefined) return '—';
    const a = Math.abs(v);
    if (a >= 1e9) return (v / 1e9).toFixed(a >= 1e10 ? 0 : 1) + 'B';
    if (a >= 1e6) return (v / 1e6).toFixed(a >= 1e7 ? 0 : 1) + 'M';
    if (a >= 1e3) return Math.round(v / 1e3) + 'k';
    return String(Math.round(v));
  };

  const PERIODS = {
    '7d': { label: 'Last 7 days', days: 7 },
    '30d': { label: 'Last 30 days', days: 30 },
    all: { label: 'All time', days: 0 },
  };
  /** The query window for a period. `until` is rounded to the hour so repeated loads share a cache entry. */
  function period(key, now) {
    const p = PERIODS[key] || PERIODS['30d'];
    const until = Math.ceil(now / 3600000) * 3600000;
    return { key: PERIODS[key] ? key : '30d', label: p.label, since: p.days ? until - p.days * DAY : 0, until: p.days ? until : 0, bucket: DAY };
  }

  /** The realised-savings model: ONE number (what was saved, priced after the fact) with its parts.
   *  total_saved_usd already contains the keep-alive net, which is an upper bound, so it is split
   *  out and kept apart from the realised figure. */
  function saved(s) {
    const priced = (s.accounting && s.accounting.complete) || 0;
    const known = priced > 0;
    const ka = s.keepalive_net_usd || 0;
    const split = s.cachesplit_saved_usd || 0;
    const own = s.cg_llm_cost_usd || 0;
    const net = s.net_saved_usd || 0; // compaction after our own model spend
    const realised = (s.total_saved_usd || 0) - ka;
    const bill = (s.cost_usd || 0) + own + (s.keepalive_ping_usd || 0);
    const would = bill + realised; // what the window would have cost with context-guru off
    const other = realised - net - split;
    const parts = [
      { label: 'Trimmed prompts', usd: net + own, tip: 'Old tool output and repeated text we removed before it reached the model, priced at what it would have cost.' },
      { label: 'Our own model calls', usd: -own, tip: 'The summaries context-guru asked a model to write. This is a real charge, so it is subtracted.' },
    ];
    if (Math.abs(split) >= 0.005) parts.push({ label: 'Cache split', usd: split, tip: 'Cache reads saved by moving the part of the prompt that changes to the end.' });
    if (Math.abs(other) >= 0.005) parts.push({ label: 'Other savings', usd: other, tip: 'Tool definitions left out of the prompt, and cache-split savings from before they were tracked per request.' });
    return {
      known, realised, ka, bill, would, parts,
      share: known && would > 0 ? (100 * realised) / would : null,
      partial: known && priced < (s.requests || 0),
      // Said only when some requests could not be priced: a full count is noise above the fold.
      coverage: known && priced < (s.requests || 0) ? 'dollar figures cover ' + num(priced) + ' of ' + num(s.requests || 0) + ' requests' : '',
    };
  }

  /** The cache model: how much input the provider's cache served, and what the misses were. */
  function cache(s) {
    const read = s.cache_read || 0, write = s.cache_write || 0, fresh = s.fresh_input || 0;
    const input = read + write + fresh;
    const m = s.cache_miss || {};
    return {
      hit: input > 0 ? (100 * read) / input : null,
      input, read, write, fresh,
      requests: s.requests || 0,
      misses: { expired: m.ttl_expiry || 0, changed: m.prefix_change || 0, cold: m.cold_start || 0, unknown: m.unknown || 0 },
      changedUsd: s.prefix_change_cost_all_usd || 0,
      changedN: s.prefix_change_requests_all || 0,
      providerSavedUsd: s.cache_saved_usd || 0,
    };
  }

  /** Unused-tool inventory: servers with no tool ever called, biggest carrying cost first. */
  function tools(rep) {
    const t = (rep && rep.totals) || {};
    const priced = !!t.priced;
    const servers = ((rep && rep.servers) || []).slice().sort((a, b) => b.unused_usd - a.unused_usd);
    return {
      priced,
      unusedUsd: t.unused_usd || 0,
      unusedTokens: t.unused_tokens || 0,
      declaredTokens: t.declared_tokens || 0,
      unusedPct: t.declared_tokens > 0 ? (100 * t.unused_tokens) / t.declared_tokens : null,
      servers,
      removable: servers.filter((s) => s.tools_used === 0 && (s.unused_usd > 0 || (!priced && s.tokens > 0))).sort((a, b) => b.unused_usd - a.unused_usd || b.tokens - a.tokens),
      toolCount: ((rep && rep.tools) || []).length,
    };
  }

  /** A shell command is offered only for a plain identifier: server names come from the client. */
  const safeName = (n) => /^[A-Za-z0-9_.-]+$/.test(n || '');

  const KEEPALIVE_YAML = 'cache:\n  keepalive: true'; // the shipped defaults are the recommended policy

  /** The ranked list. Every item: {id, low, high, text, action, kind}. `low` ranks; `high` is shown as "up to". */
  function recommend({ tools: t, rec, cache: c, keepaliveOn }) {
    const out = [];
    if (t) {
      for (const s of t.removable.slice(0, 3)) {
        out.push({
          id: 'mcp-' + s.server, kind: 'estimate', low: t.priced ? s.unused_usd : 0, high: null, alt: t.priced ? null : 'price unknown',
          text: 'Remove the ' + s.server + ' tool server. None of its ' + s.tools + ' tools was ever called.', short: 'Remove the ' + s.server + ' tool server, never called.',
          action: safeName(s.server) ? { type: 'cmd', label: 'Copy command', cmd: 'claude mcp remove ' + s.server } : { type: 'link', label: 'See tools', href: '#tools' },
        });
      }
    }
    if (!keepaliveOn && rec && rec.n > 0 && rec.hi_usd > 0) {
      out.push({
        id: 'keepalive', kind: 'estimate', low: Math.max(rec.lo_usd, 0), high: rec.hi_usd,
        text: 'Turn on keep-alive so idle gaps stop costing a full rewrite.',
        action: { type: 'cmd', label: 'Copy setting', cmd: KEEPALIVE_YAML },
      });
    }
    if (c && c.changedUsd > 0) {
      out.push({
        id: 'prefix', kind: 'ceiling', low: 0, high: c.changedUsd,
        text: num(c.changedN) + ' turns were billed in full because the start of the prompt changed. Find what edits it.', short: 'Find what edits the start of your prompt between turns.',
        action: { type: 'link', label: 'See cache detail', href: '#cache' },
      });
    }
    return out.sort((a, b) => b.low - a.low || (b.high || 0) - (a.high || 0));
  }

  /** Sessions: where the money went. `count` and `bill` cover every session in the window; the
   *  list is only the costliest ones the server returned, so shares use the whole-window bill. */
  function sessions(rows, count, bill) {
    const list = (rows || []).slice().sort((a, b) => b.cost_usd - a.cost_usd);
    const costs = list.map((r) => r.cost_usd).sort((a, b) => a - b);
    const typical = costs.length ? costs[Math.floor(costs.length / 2)] : 0;
    const top = list.slice(0, 10).reduce((x, r) => x + r.cost_usd, 0);
    return { typical, list, count: count == null ? list.length : count, bill: bill == null ? list.reduce((x, r) => x + r.cost_usd, 0) : bill, topShare: bill > 0 ? (100 * top) / bill : null };
  }

  /** Daily buckets for the charts. Saved = compaction after our own model spend, plus cache split. */
  function daily(buckets) {
    return (buckets || []).map((b) => ({
      ts: b.ts, requests: b.requests,
      bill: (b.cost_usd || 0) + (b.cg_llm_cost_usd || 0),
      saved: (b.saved_usd || 0) - (b.cg_llm_cost_usd || 0) + (b.cachesplit_saved_usd || 0),
      read: b.cache_read || 0, write: b.cache_write || 0, fresh: b.fresh_input || 0,
    }));
  }

  /** Short session label: never the full id. */
  const shortId = (id) => { const s = String(id || ''); const i = s.lastIndexOf(':'); return (i >= 0 ? s.slice(i + 1) : s).slice(0, 8); };

  const ago = (ms, now) => {
    const d = Math.max(0, now - ms);
    if (d < 90e3) return 'just now';
    if (d < 5400e3) return Math.round(d / 60e3) + ' min ago';
    if (d < 36 * 3600e3) return Math.round(d / 3600e3) + ' h ago';
    return Math.round(d / DAY) + ' days ago';
  };
  const utc = (ms) => new Date(ms).toISOString().slice(0, 16).replace('T', ' ') + ' UTC';

  /** True for a hash the classic dashboard owns (#/overview, #/savings/usage), so old bookmarks keep working. */
  const PAGE_IDS = ['home', 'savings', 'cache', 'tools', 'sessions'];
  const isClassicHash = (hash) => { const path = String(hash || '').replace(/^#/, '').split('?')[0]; return path !== '' && !PAGE_IDS.includes(path); };

  return { isClassicHash, money, pct, num, tokens, period, PERIODS, saved, cache, tools, recommend, sessions, daily, safeName, shortId, ago, utc, KEEPALIVE_YAML };
})();

if (typeof module !== 'undefined') module.exports = V2;
