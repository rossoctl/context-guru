// The Overview's three savings add up to the server's total. node --test dash/savings.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import { readFileSync } from 'node:fs';
const { savedParts } = createRequire(import.meta.url)('./ui/savings.js');

test('cache + compaction + filter equal total_saved_usd', () => {
  const o = { net_saved_usd: 89.82, keepalive_net_usd: 150.66, cachesplit_saved_usd: 3.5,
    cachesplit_historical: { usd: 1.25 }, decl_filter_usd: 7.1, total_saved_usd: 89.82 + 150.66 + 3.5 + 1.25 + 7.1 };
  const p = savedParts(o);
  assert.ok(Math.abs(p.total - p.serverTotal) < 1e-9, `${p.total} vs ${p.serverTotal}`);
  assert.equal(p.reorder, 4.75); assert.ok(Math.abs(p.cache - 155.41) < 1e-9);
});

test('missing parts count as zero, not as NaN', () => {
  const p = savedParts({ net_saved_usd: 5, total_saved_usd: 5 });
  assert.equal(p.total, 5); assert.equal(p.cache, 0);
});

test('the capped note says what was not counted, or that the cap was not applied', async () => {
  const { capNote } = createRequire(import.meta.url)('./ui/savings.js');
  const fmt = (v) => '$' + v.toFixed(2);
  assert.equal(capNote({}, fmt), '');
  assert.equal(capNote({ replay_cap_correction_usd: 0 }, fmt), '');
  assert.match(capNote({ replay_cap_correction_usd: -1.5 }, fmt), /Capped: \$1\.50 of replay credit/);
  assert.match(capNote({ replay_cap_not_applied: true, replay_cap_correction_usd: -1.5 }, fmt), /Cap not applied/);
});

test('manual pings are their own part, and the parts still add up to total_saved_usd', () => {
  const o = { net_saved_usd: 10, keepalive_net_usd: 2, cachesplit_saved_usd: 1, decl_filter_usd: 0.5,
    manual_pings: { count: 3, ping_usd: 0.04, saved_usd: 0.56 }, total_saved_usd: 10 + 2 + 1 + 0.5 + 0.56 };
  const p = savedParts(o);
  assert.equal(p.manual, 0.56);
  assert.ok(Math.abs(p.total - p.serverTotal) < 1e-9, `${p.total} vs ${p.serverTotal}`);
  assert.equal(p.cache, 3); // the cache strategy's own part does not include the button
});

test('a server from before the Ping button has no manual_pings: that is zero, not NaN', () => {
  const p = savedParts({ net_saved_usd: 5, total_saved_usd: 5 });
  assert.equal(p.manual, 0); assert.equal(p.total, 5);
});

test('the headline adds the parts, and its percent is of what the traffic would have cost without us', () => {
  const { overviewTotals } = createRequire(import.meta.url)('./ui/savings.js');
  const o = { net_saved_usd: 30, keepalive_net_usd: 10, decl_filter_usd: 5, manual_pings: { ping_usd: 1, saved_usd: 5 },
    cost_usd: 140, cg_llm_cost_usd: 6, keepalive_ping_usd: 3, total_saved_usd: 50 };
  const t = overviewTotals(o);
  assert.equal(t.saved, 50); assert.equal(t.spent, 150); assert.equal(t.without, 200);
  assert.equal(t.pct, 25);
  // the parts' percents add up to the total's
  const sum = t.share(t.parts.cache + t.parts.manual) + t.share(t.parts.compaction) + t.share(t.parts.filter);
  assert.ok(Math.abs(sum - t.pct) < 1e-9);
});

test('an empty window has no percent, not NaN', () => {
  const { overviewTotals } = createRequire(import.meta.url)('./ui/savings.js');
  const t = overviewTotals({});
  assert.equal(t.pct, 0); assert.equal(t.without, 0);
});

test('cache writes prevented count real requests; manual pings are kept apart, not added', () => {
  const { writesPrevented } = createRequire(import.meta.url)('./ui/savings.js');
  assert.deepEqual(writesPrevented({ keepalive_misses_avoided: 7, manual_pings: { writes_avoided: 2 } }), { requests: 7, manual: 2 });
  assert.deepEqual(writesPrevented({}), { requests: 0, manual: 0 });
});

test('the cache strategy name is read back from the settings, and only for a named strategy', () => {
  const { strategyName } = createRequire(import.meta.url)('./ui/savings.js');
  assert.equal(strategyName({ keepalive: true }), 'auto'); // the proxy defaults are auto's numbers
  assert.equal(strategyName({ keepalive: true, keepalive_idle_seconds: 240, keepalive_max_pings: 2, keepalive_max_pings_1h: 1 }), 'auto');
  assert.equal(strategyName({ keepalive: true, keepalive_max_pings: 1 }), '1-ping');
  assert.equal(strategyName({ keepalive: true, keepalive_max_pings: 2, keepalive_max_pings_1h: 2 }), '2-ping');
  assert.equal(strategyName({ head_ttl_1h: true }), '1-hour-head');
  assert.equal(strategyName({}), 'none');
  assert.equal(strategyName({ keepalive: true, keepalive_idle_seconds: 120 }), ''); // hand-tuned: no name
  assert.equal(strategyName(null), '');
});

test('tool deferral is its own part, and the parts still add up to total_saved_usd', () => {
  const o = { net_saved_usd: 10, keepalive_net_usd: 2, decl_filter_usd: 0.5, defer_net_usd: 0.75, total_saved_usd: 13.25 };
  const p = savedParts(o);
  assert.equal(p.defer, 0.75);
  assert.ok(Math.abs(p.total - p.serverTotal) < 1e-9, `${p.total} vs ${p.serverTotal}`);
  assert.equal(savedParts({ net_saved_usd: 5, total_saved_usd: 5 }).defer, 0); // no deferral: zero, not NaN
});

// A Claude subscription: GET /api/me/limits is mocked here as data, the shape of its contract.
test('a live 5-hour window turns the headline into saved tokens, with the estimate below', () => {
  const { subscriptionHeadline, fiveHourStartMs } = createRequire(import.meta.url)('./ui/savings.js');
  const fmt = (v) => (v >= 1e6 ? (v / 1e6).toFixed(1) + 'M' : v >= 1e3 ? Math.round(v / 1e3) + 'k' : String(v));
  const now = 1794233089000;
  const limits = { subscription: true, at_ms: now, five_hour: { utilization: 0.54, reset_s: 1794249600, status: 'allowed' } };
  const o = { total_saved_usd: 40, tokens: { saved: { cache_write_5m: 85000, cache_write_1h: 0, cache_read: 1200000, input: 0 },
    caused: { cache_read: 40000 } } };
  const win = { cost_usd: 90, total_saved_usd: 5 }; // 54% x 5 / 90 = 3%
  const h = subscriptionHeadline(limits, o, win, now, fmt);
  assert.equal(h.main, 'saved 85k wr · 1.2M rd · cost 40k rd');
  assert.equal(h.estimate, '≈ 3% of the 5-hour limit (estimate at API-price weights)');
  assert.ok(!/\$/.test(h.main + h.estimate), 'no dollars on a subscription');
  assert.equal(fiveHourStartMs(limits.five_hour), (1794249600 - 18000) * 1000);
});

test('the estimate waits for $0.50 of spend and 2% used; the tokens show without it', () => {
  const { subscriptionHeadline, fiveHourSavedPct } = createRequire(import.meta.url)('./ui/savings.js');
  const now = 1000 * 1000;
  const f = (u) => ({ five_hour: { utilization: u, reset_s: 2000 } });
  const o = { tokens: { saved: { cache_write_5m: 2, cache_read: 10 } } };
  assert.equal(subscriptionHeadline(f(0.54), o, { cost_usd: 0.49, total_saved_usd: 1 }, now, String).estimate, '');
  assert.equal(subscriptionHeadline(f(0.01), o, { cost_usd: 9, total_saved_usd: 1 }, now, String).estimate, '');
  assert.equal(subscriptionHeadline(f(0.54), o, null, now, String).estimate, ''); // the window stats failed
  assert.equal(subscriptionHeadline(f(0.54), o, { cost_usd: 9, total_saved_usd: 0 }, now, String).estimate, '');
  assert.equal(subscriptionHeadline(f(0.54), o, null, now, String).main, 'saved 2 wr · 10 rd');
  assert.deepEqual(fiveHourSavedPct({ utilization: 0.02 }, { cost_usd: 0.5, total_saved_usd: 0.5 }), { pct: 2, hit: false });
  assert.match(subscriptionHeadline(f(0.02), o, { cost_usd: 100, total_saved_usd: 1 }, now, String).estimate, /^≈ < 1% /);
});

test('{}, no answer, or a window that is over with no subscription flag keeps the dollar view', () => {
  const { subscriptionHeadline, liveFiveHour } = createRequire(import.meta.url)('./ui/savings.js');
  const now = 1794233089000;
  const o = { tokens: { saved: { cache_read: 5 } } };
  assert.equal(subscriptionHeadline({}, o, null, now, String), null);
  assert.equal(subscriptionHeadline(null, o, null, now, String), null);
  assert.equal(subscriptionHeadline({ seven_day: { utilization: 0.1, reset_s: 1794348000 } }, o, null, now, String), null);
  assert.equal(liveFiveHour({ five_hour: { utilization: 0.5, reset_s: now / 1000 - 1 } }, now), null); // reset in the past
  assert.equal(liveFiveHour({ five_hour: { utilization: 0.5, reset_s: now / 1000 } }, now), null); // resets this moment
});

test('the share saved is capped at 100 - used: past it, the line says the limit would have been hit', () => {
  const { subscriptionHeadline, fiveHourSavedPct } = createRequire(import.meta.url)('./ui/savings.js');
  const now = 1000 * 1000;
  const lim = { five_hour: { utilization: 0.45, reset_s: 2000 } };
  const o = { tokens: { saved: { cache_write_5m: 2, cache_read: 10 } } };
  // used 45, cost $15, saved $45: raw 135%, but only 55% was left.
  assert.deepEqual(fiveHourSavedPct(lim.five_hour, { cost_usd: 15, total_saved_usd: 45 }), { pct: 55, hit: true });
  assert.equal(subscriptionHeadline(lim, o, { cost_usd: 15, total_saved_usd: 45 }, now, String).estimate,
    '≈ without context-guru you would have hit the 5-hour limit (estimate at API-price weights)');
  // Just below the cap: used 45, cost $15, saved $18 is 54%, under the 55% left.
  const below = fiveHourSavedPct(lim.five_hour, { cost_usd: 15, total_saved_usd: 18 });
  assert.equal(below.hit, false); assert.ok(Math.abs(below.pct - 54) < 1e-9);
  assert.equal(subscriptionHeadline(lim, o, { cost_usd: 15, total_saved_usd: 18 }, now, String).estimate,
    '≈ 54% of the 5-hour limit (estimate at API-price weights)');
  // Exactly at the cap counts as a hit.
  assert.equal(fiveHourSavedPct(lim.five_hour, { cost_usd: 15, total_saved_usd: 15 * 55 / 45 }).hit, true);
});

test('a manager keeps the dollar view and the page does not ask for the limits; everyone else asks', () => {
  const { readsLimits } = createRequire(import.meta.url)('./ui/savings.js');
  assert.equal(readsLimits({ role: 'manager' }), false);
  assert.equal(readsLimits({ role: 'member' }), true);
  assert.equal(readsLimits(null), true); // a local proxy has no tenant
});

// The shared display rule for tokens (#998): tiers are never added; wr, wr1h, rd, in; cost after.
test('a token line shows the tiers that are not 0, in order, and the cost after', () => {
  const { tokenLine, tierText } = createRequire(import.meta.url)('./ui/savings.js');
  const k = (v) => (v / 1e3).toFixed(1) + 'k';
  assert.equal(tokenLine({ saved: { input: 12000, cache_write_5m: 85000 } }, k), 'saved 85.0k wr · 12.0k in');
  assert.equal(tokenLine({ saved: { cache_write_5m: 85000 }, caused: { cache_read: 40000 } }, k), 'saved 85.0k wr · cost 40.0k rd');
  assert.equal(tokenLine({ saved: { input: 1000, cache_read: 2000, cache_write_1h: 3000, cache_write_5m: 4000 } }, k),
    'saved 4.0k wr · 3.0k wr1h · 2.0k rd · 1.0k in');
  assert.equal(tokenLine({ caused: { cache_read: 1000 } }, k), 'saved 0 · cost 1.0k rd');
  assert.equal(tokenLine({ saved: {}, caused: {} }, k), '');
  assert.equal(tokenLine(undefined, k), '');
  assert.equal(tierText({ input: 0, cache_read: 0 }, k), '');
});

test('a subscription with no open 5-hour window still shows tokens, with no estimate', () => {
  const { subscriptionHeadline, isSubscription } = createRequire(import.meta.url)('./ui/savings.js');
  const now = 1794233089000;
  const o = { tokens: { saved: { cache_write_5m: 7 } } };
  assert.equal(isSubscription({ subscription: true }, now), true);
  const h = subscriptionHeadline({ subscription: true, five_hour: { utilization: 0.5, reset_s: now / 1000 - 1 } }, o, null, now, String);
  assert.deepEqual(h, { main: 'saved 7 wr', pings: '', estimate: '' });
  assert.equal(subscriptionHeadline({ subscription: true }, { tokens: {} }, null, now, String).main, 'saved 0 tokens');
  assert.equal(isSubscription({ subscription: false }, now), false);
});

test('the cards split tokens.by_feature: cache, filter, deferral, preset; the pings stay apart', () => {
  const { featureTokens, totalTokens, tokenLine, pingLine, subscriptionHeadline } = createRequire(import.meta.url)('./ui/savings.js');
  const f = (saved, caused) => ({ saved, caused });
  const o = {
    tokens: {
      saved: { input: 0, cache_read: 600, cache_write_5m: 6500, cache_write_1h: 300 },
      caused: { input: 0, cache_read: 4000, cache_write_5m: 600, cache_write_1h: 0 },
      by_feature: {
        mask: f({ cache_read: 600, cache_write_5m: 2000 }, { cache_write_5m: 600 }),
        ended_change: f({ cache_write_5m: 100 }, {}),
        keepalive: f({ cache_write_5m: 4000 }, { cache_read: 4000 }),
        cachesplit: f({ cache_write_5m: 200 }, {}),
        ttl: f({ cache_write_1h: 300 }, {}),
        toolfilter: f({ cache_write_5m: 150 }, {}),
        tooldefer: f({ cache_write_5m: 50 }, {}),
      },
    },
    keepalive_ping_tokens: { input: 3, cache_read: 9000, cache_write_5m: 20, cache_write_1h: 10 },
  };
  const t = featureTokens(o);
  // The rescue's own cost only: the pings are not added into it.
  assert.equal(tokenLine(t.keepalive, String), 'saved 4000 wr · cost 4000 rd');
  assert.equal(tokenLine(t.cache, String), 'saved 4200 wr · 300 wr1h · cost 4000 rd');
  assert.equal(tokenLine(t.compaction, String), 'saved 2100 wr · 600 rd · cost 600 wr');
  assert.equal(tokenLine(t.filter, String), 'saved 150 wr');
  assert.equal(tokenLine(t.defer, String), 'saved 50 wr');
  // The cards add up to the total, tier by tier. The total is tokens.saved and tokens.caused: no pings.
  const tot = totalTokens(o);
  assert.deepEqual(tot.caused, o.tokens.caused);
  for (const k of ['input', 'cache_read', 'cache_write_5m', 'cache_write_1h']) {
    assert.equal(t.cache.saved[k] + t.compaction.saved[k] + t.filter.saved[k] + t.defer.saved[k], tot.saved[k], 'saved ' + k);
    assert.equal(t.cache.caused[k] + t.compaction.caused[k] + t.filter.caused[k] + t.defer.caused[k], tot.caused[k], 'caused ' + k);
  }
  // The pings are a line of their own, on the card and under the total.
  assert.equal(pingLine(o, String), 'cost 20 wr · 10 wr1h · 9000 rd · 3 in');
  const h = subscriptionHeadline({ subscription: true }, o, null, 0, String);
  assert.equal(h.main, 'saved 6500 wr · 300 wr1h · 600 rd · cost 600 wr · 4000 rd');
  assert.equal(h.pings, 'Keep-alive pings (not in the total): cost 20 wr · 10 wr1h · 9000 rd · 3 in');
  assert.equal(subscriptionHeadline({ subscription: true }, { tokens: o.tokens }, null, 0, String).pings, ''); // no pings: no line
  assert.equal(pingLine({ keepalive_ping_tokens: { input: 0, cache_read: 0 } }, String), '');
  // A read with no tokens (a server from before #999) gives empty lines, not NaN.
  assert.equal(tokenLine(featureTokens({}).cache, String), '');
  assert.equal(tokenLine(totalTokens({}), String), '');
});

test('the capped note leaves the dollars out on a subscription', () => {
  const { capNote } = createRequire(import.meta.url)('./ui/savings.js');
  assert.equal(capNote({ replay_cap_correction_usd: -1.5 }, null),
    'Capped: replay credit earned after the client would have compacted is not counted.');
});

// The Cache strategy tab on a subscription (#1022): the measured rescue and the pings, two lines.
test('the Cache strategy tab shows the rescue and the pings in tokens, never netted', () => {
  const { kaTokenLines } = createRequire(import.meta.url)('./ui/savings.js');
  const k = (v) => (v / 1e3).toFixed(1) + 'k';
  const l = {
    net_usd: -1.5,
    tokens: { saved: { cache_write_5m: 85000 }, caused: { cache_read: 40000 } },
    keepalive_ping_tokens: { input: 0, cache_read: 410000, cache_write_5m: 900, cache_write_1h: 0 },
  };
  const t = kaTokenLines(l, k);
  assert.deepEqual(t, { measured: true, rescue: 'saved 85.0k wr · cost 40.0k rd', pings: 'cost 0.9k wr · 410.0k rd',
    measuredText: 'Measured on the sessions where it ran: saved 85.0k wr · cost 40.0k rd',
    avoidedText: 'up to 85.0k wr · cost 40.0k rd',
    rescuesText: 'The rescues: saved 85.0k wr · cost 40.0k rd.' });
  assert.ok(!/\$/.test(t.rescue + t.pings), 'no dollars');
  // The pings are not in the rescue's cost.
  assert.ok(!/410/.test(t.rescue));
  // A ledger with no tokens yet, or an old server without the fields.
  // A ledger from before #999 (no `tokens`), or none: not measured (tokenFigure).
  for (const o of [{}, null]) {
    const z = kaTokenLines(o, k);
    assert.equal(z.measured, false);
    assert.equal(z.rescue, 'not measured');
    assert.equal(z.pings, 'cost 0 tokens');
  }
});

// A zero is a measurement only when the requests were priced (#1042 review).
test('an all-zero rescue with unpriced requests says not measured; a priced zero says saved 0', () => {
  const { kaTokenLines } = createRequire(import.meta.url)('./ui/savings.js');
  const zero = { input: 0, cache_read: 0, cache_write_5m: 0, cache_write_1h: 0 };
  // The golden fixture's ledger (dash/testdata/golden/keepalive.json): no row priced.
  const golden = JSON.parse(readFileSync(new URL('./testdata/golden/keepalive.json', import.meta.url), 'utf8'));
  assert.equal(golden.tokens_unmeasured_requests, 591);
  const g = kaTokenLines(golden, String);
  assert.equal(g.measured, false);
  assert.equal(g.rescue, 'not measured');
  assert.equal(g.pings, 'cost 720540 rd · 150 in');
  // The sentences that carry the rescue say the absence in words, never as a figure.
  assert.equal(g.measuredText, 'Not measured in tokens on the sessions where it ran');
  assert.equal(g.avoidedText, 'not measured');
  assert.equal(g.rescuesText, 'The rescues are not measured in tokens.');
  // Every request priced, and nothing rescued: a true zero.
  const p = kaTokenLines({ tokens: { saved: zero, caused: zero }, tokens_unmeasured_requests: 0 }, String);
  assert.equal(p.measured, true);
  assert.equal(p.rescue, 'saved 0 tokens');
  assert.equal(p.measuredText, 'Measured on the sessions where it ran: saved 0 tokens');
  assert.equal(p.avoidedText, 'up to 0 tokens');
  // Some requests unpriced, but a rescue measured: the measured line shows.
  const m = kaTokenLines({ tokens: { saved: { cache_write_5m: 7 } }, tokens_unmeasured_requests: 3 }, String);
  assert.equal(m.measured, true);
  assert.equal(m.rescue, 'saved 7 wr');
});

// One rule for zero against "not measured" (tokenFigure), for every screen (#988 round 2).
test('a priced zero reads saved 0 tokens; an unpriced zero or a server with no tokens reads not measured', () => {
  const { tokenFigure, featureTokens, totalTokens } = createRequire(import.meta.url)('./ui/savings.js');
  const zero = { input: 0, cache_read: 0, cache_write_5m: 0, cache_write_1h: 0 };
  // A full read with 0 unmeasured: a feature left out of by_feature is a measured zero.
  const priced = { tokens: { saved: { cache_write_5m: 9 }, caused: zero, by_feature: { mask: { saved: { cache_write_5m: 9 } } } },
    tokens_unmeasured_requests: 0 };
  assert.deepEqual(tokenFigure(featureTokens(priced).filter, priced, String), { text: 'saved 0 tokens', measured: true });
  assert.deepEqual(tokenFigure(featureTokens(priced).compaction, priced, String), { text: 'saved 9 wr', measured: true });
  // Some requests unpriced and an all-zero figure: not measured, not "saved nothing".
  const some = { ...priced, tokens_unmeasured_requests: 3 };
  assert.deepEqual(tokenFigure(featureTokens(some).filter, some, String), { text: 'not measured', measured: false });
  // Some unpriced, but the figure has tokens: it is measured.
  assert.equal(tokenFigure(featureTokens(some).compaction, some, String).measured, true);
  // A server from before #999: no tokens at all.
  assert.deepEqual(tokenFigure(totalTokens({}), {}, String), { text: 'not measured', measured: false });
  assert.deepEqual(tokenFigure(undefined, null, String), { text: 'not measured', measured: false });
});

test('the headline uses the same rule, and a page filter hides the account-wide estimate', () => {
  const { subscriptionHeadline } = createRequire(import.meta.url)('./ui/savings.js');
  const now = 1000 * 1000;
  const lim = { subscription: true, five_hour: { utilization: 0.54, reset_s: 2000 } };
  const win = { cost_usd: 90, total_saved_usd: 5 };
  const o = { tokens: { saved: { cache_write_5m: 2 } } };
  assert.equal(subscriptionHeadline(lim, o, win, now, String).estimate, '≈ 3% of the 5-hour limit (estimate at API-price weights)');
  assert.equal(subscriptionHeadline(lim, o, win, now, String, true).estimate, '');
  assert.equal(subscriptionHeadline(lim, o, win, now, String, true).main, 'saved 2 wr');
  assert.equal(subscriptionHeadline(lim, { tokens: { saved: {} }, tokens_unmeasured_requests: 0 }, null, now, String).main, 'saved 0 tokens');
  assert.equal(subscriptionHeadline(lim, { tokens: { saved: {} }, tokens_unmeasured_requests: 4 }, null, now, String).main, 'not measured');
  assert.equal(subscriptionHeadline(lim, {}, null, now, String).main, 'not measured'); // a server from before #999
});
