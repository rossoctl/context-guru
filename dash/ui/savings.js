// The Overview's three savings as plain numbers: no DOM, so dash/savings.test.mjs can check that they
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
  return { cache, keepalive, reorder, compaction, filter, total: cache + compaction + filter, serverTotal: o.total_saved_usd || 0 };
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

if (typeof module !== 'undefined') module.exports = { savedParts, capNote };
