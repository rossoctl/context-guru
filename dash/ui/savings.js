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

// The cache strategy's plugin name (the proxy's strategy table, config/strategy.go), read back from
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

// One token figure, with the one rule for zero against "not measured" that every screen uses.
// `flow` is the figure ({saved, caused}); `scope` is the stats object it came from, which carries
// `tokens` and `tokens_unmeasured_requests` (the Overview's /api/stats, a ledger, a session row).
//   - a flow with tokens: its line, measured;
//   - an all-zero flow and nothing unpriced: `saved 0 tokens`, measured. The server leaves a
//     feature with no tokens out of by_feature, so a missing feature on a priced scope is a zero;
//   - an all-zero flow and some requests unpriced, or a server from before #999 (no `tokens`):
//     `not measured`. That is not "saved nothing".
function tokenFigure(flow, scope, fmt) {
  const line = tokenLine(flow, fmt);
  if (line) return { text: line, measured: true };
  const priced = !!(scope && scope.tokens) && !((scope.tokens_unmeasured_requests || 0) > 0);
  return priced ? { text: 'saved 0 tokens', measured: true } : { text: 'not measured', measured: false };
}

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
const PING_LABEL = 'Keep-alive pings (not in the total)';
function pingLine(o, fmt) {
  const t = tierText(o && o.keepalive_ping_tokens, fmt);
  return t ? 'cost ' + t : '';
}

// The Overview headline on a subscription, or null for the dollar view. `o` is the Overview's own
// stats, `w` the 5-hour window's (for the estimate), `fmt` formats a count. The estimate needs an
// open 5-hour window; the token line does not. The estimate is for the whole account, so it is
// hidden while a page filter (a session, a time range, a model...) narrows the figures: `filtered`.
function subscriptionHeadline(limits, o, w, nowMs, fmt, filtered) {
  if (!isSubscription(limits, nowMs)) return null;
  const main = tokenFigure(totalTokens(o), o, fmt).text;
  const ping = pingLine(o, fmt);
  const pings = ping ? PING_LABEL + ': ' + ping : '';
  const f = liveFiveHour(limits, nowMs);
  const p = f && !filtered ? fiveHourSavedPct(f, w) : null;
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
//   keepalive  the rescue (keepalive_tokens) through the same shared rule, and the pings on a line
//              of their own (pingLine), never added into the rescue's cost. The caller shows `n/a`
//              when neither has tokens: the strategy did not run.
// `saved` follows the shared rule for zero against "not measured" (tokenFigure), scoped to the
// session row: an all-zero figure with any unpriced request of the session, or a row with no
// `tokens`, reads "not measured". `measured` is that rule's answer. `unmeasured` counts the session's
// requests with no token figure. The other fields are '' when empty.
function sessionTokenCells(s, fmt) {
  const saved = tokenFigure(s && s.tokens, s, fmt);
  return {
    cost: tierText(s && s.used_tokens, fmt),
    saved: saved.text,
    measured: saved.measured,
    keepalive: tokenFigure(s && s.keepalive_tokens, s, fmt).text,
    keepaliveRan: hasTokens(s && s.keepalive_tokens),
    pings: pingLine(s, fmt),
    unmeasured: (s && s.tokens_unmeasured_requests) || 0,
  };
}

// The Cache strategy tab in tokens (#1022), from the keep-alive ledger's `tokens` (the rescue) and
// `keepalive_ping_tokens` (what the pings paid, read by pingLine). Two lines, never netted: the pings
// are not added into the rescue's cost, and the tiers have no common unit. `fmt` formats a count.
// The rescue follows the one rule for zero against "not measured" (tokenFigure): `measured` is
// false for an absence. The sentences that carry the rescue branch on it, so an absence never
// reads as a figure ("up to not measured"):
//   measuredText  the line under the forecast
//   avoidedText   the "Cache rewrites avoided" fact: "up to 85.0k wr · cost 40.0k rd"
//   rescuesText   the "costs more than it saves" banner
function kaTokenLines(o, fmt) {
  const { text: rescue, measured } = tokenFigure(o && o.tokens, o, fmt);
  return {
    measured, rescue, pings: pingLine(o, fmt) || 'cost 0 tokens',
    measuredText: measured ? 'Measured on the sessions where it ran: ' + rescue
      : 'Not measured in tokens on the sessions where it ran',
    avoidedText: measured ? 'up to ' + rescue.replace(/^saved /, '') : 'not measured',
    rescuesText: measured ? 'The rescues: ' + rescue + '.' : 'The rescues are not measured in tokens.',
  };
}

// One row of the Cache strategy tab's session tables in tokens, on a subscription (#1048), from
// /api/keepalive/sessions or /api/keepalive/live. The fields are the Sessions tab's own (#1023).
//   expiry  what the session's expiries billed, by tier (expiry_tokens): the token form of the
//           "cost of running out". '' on a row with none (a live row has none).
//   rescue  the rescue (keepalive_tokens) through the shared rule for zero (tokenFigure), scoped to
//           the row: all zero with any unpriced request of the session, or no `keepalive_tokens`
//           (a server from before #1048), reads "not measured". `measured` is that rule's answer.
//   pings   what the pings paid (pingLine), never added into the rescue's cost. A row with pings
//           and no ping tokens reads `cost 0 tokens`, as kaTokenLines does. '' with no pings.
//   ran     the strategy ran on the session: it sent a ping, or the rescue has tokens. The caller
//           shows `—` when it did not, as the dollar cells do.
function kaSessionTokenCells(s, fmt) {
  const ka = s && s.keepalive_tokens;
  const rescue = tokenFigure(ka, ka ? { tokens: ka, tokens_unmeasured_requests: s.tokens_unmeasured_requests } : null, fmt);
  const ping = pingLine(s, fmt);
  const pinged = !!(s && s.pings > 0) || ping !== '';
  return {
    expiry: tierText(s && s.expiry_tokens, fmt),
    rescue: rescue.text,
    measured: rescue.measured,
    pings: ping || (pinged ? 'cost 0 tokens' : ''),
    ran: pinged || hasTokens(ka),
  };
}

// The expiries' billed tokens in a band of the behaviour panel (#1048): `1.2M wr · 30.0k rd`. A band
// with no expiries reads `0 tokens`; a band from a server before #1048 (no `tokens`) `not measured`.
function bandTokens(b, fmt) {
  if (!b || !b.tokens) return 'not measured';
  return tierText(b.tokens, fmt) || '0 tokens';
}

// The behaviour panel's daily chart in tokens (#1048): one series per tier that is not 0 on any day,
// in the display order. The tiers are never added into one line. `rows` is /api/keepalive/behaviour
// `daily`; each point is [ts, tokens of that tier].
const TIER_NAMES = { wr: 'Cache write (wr)', wr1h: '1-hour cache write (wr1h)', rd: 'Cache read (rd)', in: 'Fresh input (in)' };
function tierSeries(rows) {
  const rs = rows || [];
  return TOKEN_TIERS.filter(([k]) => rs.some((r) => r.tokens && r.tokens[k]))
    .map(([k, l]) => ({ tier: l, name: TIER_NAMES[l], points: rs.map((r) => [r.ts, (r.tokens && r.tokens[k]) || 0]) }));
}

if (typeof module !== 'undefined') module.exports = { savedParts, capNote, overviewTotals, writesPrevented, strategyName,
  liveFiveHour, fiveHourStartMs, fiveHourSavedPct, readsLimits, isSubscription, subscriptionHeadline,
  tierText, addTiers, tokenLine, hasTokens, tokenFigure, featureTokens, totalTokens, pingLine, PING_LABEL, sessionTokenCells, kaTokenLines,
  kaSessionTokenCells, bandTokens, tierSeries };
