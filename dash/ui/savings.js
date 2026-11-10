// The Overview's savings as plain numbers: no DOM, so dash/savings.test.mjs can check that they
// add up to the server's own total_saved_usd.
//
//   cache       keep-alive's net (an upper bound) plus the cache-reorder credit (what moving the stable part
//               of the prompt first earned, in the window and on earlier requests)
//   compaction  net_saved_usd, already after our own model calls
//   filter      tools and skills left out of the prompt
//   defer       tool deferral's net saving (ultrahigh); 0 on an account that never deferred
'use strict';

function savedParts(o) {
  const keepalive = o.keepalive_net_usd || 0;
  const reorder = (o.cachesplit_saved_usd || 0) + ((o.cachesplit_historical && o.cachesplit_historical.usd) || 0);
  const cache = keepalive + reorder;
  const compaction = o.net_saved_usd || 0;
  const filter = o.decl_filter_usd || 0;
  const defer = o.defer_net_usd || 0;
  // The user's own Ping presses. saved_usd is already net of what the pings cost, and the server adds it to
  // total_saved_usd as its own addend, outside the cache strategy.
  const manual = (o.manual_pings && o.manual_pings.saved_usd) || 0;
  return { cache, keepalive, reorder, compaction, filter, defer, manual, total: cache + compaction + filter + defer + manual, serverTotal: o.total_saved_usd || 0 };
}

// The headline on the Overview: the parts added up, against what the same traffic would have cost
// without context-guru. Every saving is already net of what context-guru spent to make it (our own
// model calls, the pings), so "without" is simply what was spent plus what was saved. The percent of
// each part uses the same "without", so the parts' percents add up to the total's.
function overviewTotals(o) {
  const p = savedParts(o);
  const spent = (o.cost_usd || 0) + (o.cg_llm_cost_usd || 0) + (o.keepalive_ping_usd || 0)
    + ((o.manual_pings && o.manual_pings.ping_usd) || 0);
  const without = spent + p.total;
  const share = (v) => (without > 0 ? (100 * v) / without : 0);
  return { saved: p.total, spent, without, pct: share(p.total), share, parts: p };
}

// The cache writes context-guru prevented. `requests` is real requests that resumed after the cache
// would have expired and read it anyway: a measured count, and what the waiting time multiplies.
// `manual` is the user's own pings that found the cache warm. A ping is not a request anyone waited
// on, and a later ping can find the same entry warm, so it is an upper bound and kept apart.
function writesPrevented(o) {
  return { requests: o.keepalive_misses_avoided || 0, manual: (o.manual_pings && o.manual_pings.writes_avoided) || 0 };
}

// The cache strategy's plugin name (context-guru-plugin/scripts/settings.py STRATEGIES), read back from
// the account's cache settings: they hold numbers, not the name. '' when the settings match no named
// strategy, so a hand-tuned account is never given a name it did not pick. Missing fields take the
// proxy defaults (240 s; 2 pings; 1 ping on a 1-hour cache). The hosted settings form has no 1-hour
// ping count, so there 2 pings reads as `auto`, the plugin's default, and not as `2-ping`.
function strategyName(c) {
  if (!c) return '';
  if (!c.keepalive) return c.head_ttl_1h ? '1-hour-head' : 'none';
  if ((c.keepalive_idle_seconds || 240) !== 240) return '';
  const k = c.keepalive_max_pings || 2, k1h = c.keepalive_max_pings_1h || 1;
  if (k === 1 && k1h === 1) return '1-ping';
  if (k === 2 && k1h === 1) return 'auto';
  if (k === 2 && k1h === 2) return '2-ping';
  return '';
}

// The note under "Your preset" when the replay cap changed the figure, or could not be applied.
// '' when there is nothing to say. `fmt` formats dollars; null leaves the dollars out (a subscription).
function capNote(o, fmt) {
  if (o.replay_cap_not_applied) {
    return 'Cap not applied: the replay cap could not be computed in time, so this figure may be too high.';
  }
  const c = o.replay_cap_correction_usd || 0;
  if (c < 0) {
    return 'Capped: ' + (fmt ? fmt(-c) + ' of ' : '') + 'replay credit earned after the client would have compacted is not counted.';
  }
  return '';
}

// ── Claude subscription (GET /api/me/limits) ──
//
// A subscription does not bill per token, so its Overview shows every saving in tokens, not dollars.
// /api/me/limits holds the auth kind (`subscription`) and the latest anthropic-ratelimit-unified-*
// headers the proxy saw for the account: `{}` for an API key, Bedrock or Vertex. For the estimate,
// only a 5-hour window that is still open counts.

// The open 5-hour window, or null: no answer, `{}`, or a window whose reset is in the past.
function liveFiveHour(limits, nowMs) {
  const f = limits && limits.five_hour;
  if (!f || !(f.reset_s > 0) || f.reset_s * 1000 <= nowMs) return null;
  return f;
}

// When the 5-hour window started, in unix ms: its reset less 5 hours.
function fiveHourStartMs(f) { return (f.reset_s - 5 * 3600) * 1000; }

// The share of the 5-hour limit context-guru saved in the window, in percent, or null when the
// window has too little data to say (under $0.50 of spend, or under 2% used). Anthropic does not
// publish how the limit weights token kinds, so this weights them at API prices: an estimate.
// `w` is /api/stats?lean=1&since=<window start>.
//
// The provider stops a user at 100%, so the share saved can never be more than 100 - used. At that
// cap, `hit` is true: without context-guru the user would have hit the limit.
function fiveHourSavedPct(f, w) {
  const used = 100 * ((f && f.utilization) || 0);
  const cost = (w && w.cost_usd) || 0;
  const saved = (w && w.total_saved_usd) || 0;
  if (cost < 0.5 || used < 2 || !(saved > 0)) return null;
  const raw = (used * saved) / cost;
  const room = Math.max(0, 100 - used);
  return { pct: Math.min(raw, room), hit: raw >= room };
}

// A manager's Overview spans other accounts, and the limits are the signed-in account's own, so a
// manager keeps the dollar view and the page does not ask. A local proxy has no tenant: it asks.
function readsLimits(tenant) { return !(tenant && tenant.role === 'manager'); }

// A Claude subscription: the auth kind says so (`subscription`), or a 5-hour window is open.
function isSubscription(limits, nowMs) {
  return !!(limits && (limits.subscription === true || liveFiveHour(limits, nowMs)));
}

// ── Savings in tokens (/api/stats `tokens`, #998) ──
//
// Every saving the accountant measured, by tier. The tiers are never added into one number: the
// provider does not say how a usage limit weights them. A feature can save in one tier and cost in
// another, so a line shows both: `saved 85.0k wr · 12.0k in · cost 40.0k rd`.

// The tiers in display order, with their short labels.
const TOKEN_TIERS = [['cache_write_5m', 'wr'], ['cache_write_1h', 'wr1h'], ['cache_read', 'rd'], ['input', 'in']];

// The tiers that are not 0, as `85.0k wr · 12.0k in`. '' when all are 0. `fmt` formats a count.
function tierText(t, fmt) {
  return TOKEN_TIERS.filter(([k]) => t && t[k]).map(([k, l]) => fmt(t[k]) + ' ' + l).join(' · ');
}

// The sum of tier figures, tier by tier (never across tiers). A missing figure counts as 0.
function addTiers(...ts) {
  const out = { input: 0, cache_read: 0, cache_write_5m: 0, cache_write_1h: 0 };
  for (const t of ts) if (t) for (const k of Object.keys(out)) out[k] += t[k] || 0;
  return out;
}

// One saving in tokens: `saved 85.0k wr · cost 40.0k rd`. '' when the flow has no tokens at all.
function tokenLine(flow, fmt) {
  const s = tierText(flow && flow.saved, fmt), c = tierText(flow && flow.caused, fmt);
  if (!s && !c) return '';
  return 'saved ' + (s || '0') + (c ? ' · cost ' + c : '');
}

// True when the flow has a token in any tier.
function hasTokens(flow) { return tokenLine(flow, String) !== ''; }

// The request-level cache effects (`cachesplit` is one entry: the split plus its component's own).
const CACHE_FEATURES = ['keepalive', 'cachesplit', 'ttl'];

// The Overview cards in tokens, from tokens.by_feature (full read only):
//   keepalive   the rescue (what the pings paid is not in it: see pingLine)
//   cachesplit, ttl
//   cache       the three above together: the Cache card
//   filter      toolfilter;   defer  tooldefer
//   compaction  every other entry (the preset's components and ended_change)
// The cards add up to the total (totalTokens), tier by tier.
function featureTokens(o) {
  const bf = (o && o.tokens && o.tokens.by_feature) || {};
  const flow = (...fs) => ({ saved: addTiers(...fs.map((f) => f && f.saved)), caused: addTiers(...fs.map((f) => f && f.caused)) });
  const rest = Object.keys(bf).filter((k) => !CACHE_FEATURES.includes(k) && k !== 'toolfilter' && k !== 'tooldefer');
  return {
    keepalive: flow(bf.keepalive),
    cachesplit: flow(bf.cachesplit),
    ttl: flow(bf.ttl),
    cache: flow(bf.keepalive, bf.cachesplit, bf.ttl),
    filter: flow(bf.toolfilter),
    defer: flow(bf.tooldefer),
    compaction: flow(...rest.map((k) => bf[k])),
  };
}

// The total in tokens: tokens.saved and tokens.caused. The pings are not in it (pingLine).
function totalTokens(o) {
  const t = (o && o.tokens) || {};
  return { saved: addTiers(t.saved), caused: addTiers(t.caused) };
}

// What the cache strategy's keep-alive pings paid, on a line of its own: `cost 900 wr · 410.0k rd`,
// or '' when they paid nothing. A ping is not agent traffic, so it is not in the total or in any
// card's cost. Every screen follows this rule, because the band's lean read has no ping tokens.
// The keep-alive's net is its rescue line less this line.
const PING_LABEL = 'Keep-alive pings (not in the total)';
function pingLine(o, fmt) {
  const t = tierText(o && o.keepalive_ping_tokens, fmt);
  return t ? 'cost ' + t : '';
}

// The Overview headline on a subscription, or null for the dollar view. `o` is the Overview's own
// stats, `w` the 5-hour window's (for the estimate), `fmt` formats a count. The estimate needs an
// open 5-hour window; the token line does not.
function subscriptionHeadline(limits, o, w, nowMs, fmt) {
  if (!isSubscription(limits, nowMs)) return null;
  const main = tokenLine(totalTokens(o), fmt) || 'saved 0 tokens';
  const ping = pingLine(o, fmt);
  const pings = ping ? PING_LABEL + ': ' + ping : '';
  const f = liveFiveHour(limits, nowMs);
  const p = f ? fiveHourSavedPct(f, w) : null;
  const tail = ' (estimate at API-price weights)';
  const estimate = p === null ? ''
    : p.hit ? '≈ without context-guru you would have hit the 5-hour limit' + tail
      : '≈ ' + (p.pct < 1 ? '< 1' : String(Math.round(p.pct))) + '% of the 5-hour limit' + tail;
  return { main, pings, estimate };
}

// One Sessions row in tokens, on a subscription (#1023), from /api/sessions' token fields. The
// columns that show dollars elsewhere:
//   cost       the session's billed input tokens (used_tokens). context-guru's own model calls
//              (cg_llm_cost_usd) have no token form, so they are not in it.
//   saved      the accountant's figure less the keep-alive rescue (tokens): `saved … · cost …`.
//   keepalive  the rescue (keepalive_tokens), and the pings on a line of their own (pingLine),
//              never added into the rescue's cost.
// `unmeasured` counts the session's requests with no token figure. Each field is '' when empty.
function sessionTokenCells(s, fmt) {
  const unmeasured = (s && s.tokens_unmeasured_requests) || 0;
  const all = unmeasured > 0 && unmeasured >= ((s && s.turns) || 0);
  return {
    cost: tierText(s && s.used_tokens, fmt),
    saved: tokenLine(s && s.tokens, fmt) || (all ? 'not measured' : 'saved 0'),
    keepalive: tokenLine(s && s.keepalive_tokens, fmt),
    pings: pingLine(s, fmt),
    unmeasured,
  };
}

if (typeof module !== 'undefined') module.exports = { savedParts, capNote, overviewTotals, writesPrevented, strategyName,
  liveFiveHour, fiveHourStartMs, fiveHourSavedPct, readsLimits, isSubscription, subscriptionHeadline,
  tierText, addTiers, tokenLine, hasTokens, featureTokens, totalTokens, pingLine, PING_LABEL, sessionTokenCells };
