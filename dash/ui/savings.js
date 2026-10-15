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

if (typeof module !== 'undefined') module.exports = { savedParts };
