// The Overview's savings as plain numbers: no DOM, so dash/savings.test.mjs can check that they
// add up to the server's own total_saved_usd.
//
//   cache       keep-alive's net (an upper bound) plus the cache-reorder credit (what moving the stable part
//               of the prompt first earned, in the window and on earlier requests)
//   compaction  net_saved_usd, already after our own model calls
//   filter      tools and skills left out of the prompt
'use strict';

function savedParts(o) {
  const keepalive = o.keepalive_net_usd || 0;
  const reorder = (o.cachesplit_saved_usd || 0) + ((o.cachesplit_historical && o.cachesplit_historical.usd) || 0);
  const cache = keepalive + reorder;
  const compaction = o.net_saved_usd || 0;
  const filter = o.decl_filter_usd || 0;
  // The user's own Ping presses. saved_usd is already net of what the pings cost, and the server adds it to
  // total_saved_usd as its own addend, outside the cache strategy.
  const manual = (o.manual_pings && o.manual_pings.saved_usd) || 0;
  return { cache, keepalive, reorder, compaction, filter, manual, total: cache + compaction + filter + manual, serverTotal: o.total_saved_usd || 0 };
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
// '' when there is nothing to say. `fmt` formats dollars.
function capNote(o, fmt) {
  if (o.replay_cap_not_applied) {
    return 'Cap not applied: the replay cap could not be computed in time, so this figure may be too high.';
  }
  const c = o.replay_cap_correction_usd || 0;
  if (c < 0) {
    return 'Capped: ' + fmt(-c) + ' of replay credit earned after the client would have compacted is not counted.';
  }
  return '';
}

if (typeof module !== 'undefined') module.exports = { savedParts, capNote, overviewTotals, writesPrevented, strategyName };
