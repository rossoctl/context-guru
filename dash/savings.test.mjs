// The Overview's three savings add up to the server's total. node --test dash/savings.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
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
  const o = { replay_tokens: 1200000, saved_unique: 85000, total_saved_usd: 40 };
  const win = { cost_usd: 90, total_saved_usd: 5 }; // 54% x 5 / 90 = 3%
  const h = subscriptionHeadline(limits, o, win, now, fmt);
  assert.equal(h.main, 'saved 1.2M rd / 85k wr');
  assert.equal(h.estimate, '≈ 3% of the 5-hour limit (estimate at API-price weights)');
  assert.ok(!/\$/.test(h.main + h.estimate), 'no dollars on a subscription');
  assert.equal(fiveHourStartMs(limits.five_hour), (1794249600 - 18000) * 1000);
});

test('the estimate waits for $0.50 of spend and 2% used; the tokens show without it', () => {
  const { subscriptionHeadline, fiveHourSavedPct } = createRequire(import.meta.url)('./ui/savings.js');
  const now = 1000 * 1000;
  const f = (u) => ({ five_hour: { utilization: u, reset_s: 2000 } });
  const o = { replay_tokens: 10, saved_unique: 2 };
  assert.equal(subscriptionHeadline(f(0.54), o, { cost_usd: 0.49, total_saved_usd: 1 }, now, String).estimate, '');
  assert.equal(subscriptionHeadline(f(0.01), o, { cost_usd: 9, total_saved_usd: 1 }, now, String).estimate, '');
  assert.equal(subscriptionHeadline(f(0.54), o, null, now, String).estimate, ''); // the window stats failed
  assert.equal(subscriptionHeadline(f(0.54), o, { cost_usd: 9, total_saved_usd: 0 }, now, String).estimate, '');
  assert.equal(subscriptionHeadline(f(0.54), o, null, now, String).main, 'saved 10 rd / 2 wr');
  assert.equal(fiveHourSavedPct({ utilization: 0.02 }, { cost_usd: 0.5, total_saved_usd: 0.5 }), 2);
  assert.match(subscriptionHeadline(f(0.02), o, { cost_usd: 100, total_saved_usd: 1 }, now, String).estimate, /^≈ < 1% /);
});

test('{}, no answer, or a window that is over keeps the dollar view', () => {
  const { subscriptionHeadline, liveFiveHour } = createRequire(import.meta.url)('./ui/savings.js');
  const now = 1794233089000;
  const o = { replay_tokens: 5, saved_unique: 1 };
  assert.equal(subscriptionHeadline({}, o, null, now, String), null);
  assert.equal(subscriptionHeadline(null, o, null, now, String), null);
  assert.equal(subscriptionHeadline({ seven_day: { utilization: 0.1, reset_s: 1794348000 } }, o, null, now, String), null);
  assert.equal(liveFiveHour({ five_hour: { utilization: 0.5, reset_s: now / 1000 - 1 } }, now), null); // reset in the past
  assert.equal(liveFiveHour({ five_hour: { utilization: 0.5, reset_s: now / 1000 } }, now), null); // resets this moment
});
