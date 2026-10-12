// Pure models behind the five-page dashboard (ui/v2model.js). node --test dash/v2.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import fs from 'node:fs';
const V2 = createRequire(import.meta.url)('./ui/v2model.js');

const stats = { accounting: { complete: 90 }, requests: 100, cost_usd: 900, cg_llm_cost_usd: 4, keepalive_ping_usd: 6,
  net_saved_usd: 40, keepalive_net_usd: 30, cachesplit_saved_usd: 0, total_saved_usd: 75, cache_read: 80, cache_write: 15, fresh_input: 5,
  cache_miss: { ttl_expiry: 7, prefix_change: 3, cold_start: 2, unknown: 1 }, prefix_change_cost_all_usd: 50, prefix_change_requests_all: 3, cache_saved_usd: 9000 };

test('money, percent and token formats are consistent', () => {
  assert.equal(V2.money(0), '$0'); assert.equal(V2.money(4.386), '$4.39'); assert.equal(V2.money(1163.3), '$1,163'); assert.equal(V2.money(-12), '−$12');
  assert.equal(V2.money(0.001), '<$0.01'); assert.equal(V2.money(null), '—');
  assert.equal(V2.pct(0.44), '0.4%'); assert.equal(V2.pct(92.3), '92%');
  assert.equal(V2.tokens(12674394972), '13B'); assert.equal(V2.tokens(34691878), '35M'); assert.equal(V2.tokens(82000), '82k');
});

test('saved: keep-alive is split out of the realised figure and never added back', () => {
  const sv = V2.saved(stats);
  assert.equal(sv.realised, 45); assert.equal(sv.ka, 30);
  assert.equal(sv.bill, 910); assert.equal(sv.would, 955);
  assert.ok(Math.abs(sv.share - (100 * 45) / 955) < 1e-9);
  // The parts reconcile to the realised figure: trimmed + own model calls = compaction net; the remainder is "other".
  const sum = sv.parts.reduce((x, p) => x + p.usd, 0);
  assert.ok(Math.abs(sum - sv.realised) < 1e-9, 'parts sum to ' + sum);
});

test('saved: an unpriced window is unknown, not zero', () => {
  const sv = V2.saved({ ...stats, accounting: { complete: 0 } });
  assert.equal(sv.known, false); assert.equal(sv.share, null);
});

test('cache: hit share and miss counts', () => {
  const c = V2.cache(stats);
  assert.equal(c.hit, 80); assert.equal(c.misses.expired, 7); assert.equal(c.changedUsd, 50);
  assert.equal(V2.cache({}).hit, null);
});

const tools = { totals: { priced: true, unused_usd: 100, unused_tokens: 900, declared_tokens: 1000 }, tools: [{}, {}],
  servers: [{ server: 'a', tools: 5, tools_used: 0, unused_usd: 60, sessions_declared: 9 }, { server: 'b c', tools: 2, tools_used: 0, unused_usd: 30, sessions_declared: 4 },
    { server: 'used', tools: 3, tools_used: 1, unused_usd: 99, sessions_declared: 4 }, { server: 'zero', tools: 1, tools_used: 0, unused_usd: 0, sessions_declared: 1 }] };

test('recommend: ranked by the low bound; a ceiling never outranks an estimate', () => {
  const recs = V2.recommend({ tools: V2.tools(tools), rec: { n: 10, lo_usd: 40, hi_usd: 400 }, cache: V2.cache(stats) });
  assert.deepEqual(recs.map((r) => r.id), ['mcp-a', 'keepalive', 'mcp-b c', 'prefix']);
  assert.equal(recs.at(-1).kind, 'ceiling'); assert.equal(recs.at(-1).low, 0);
  // Only a server with NO tool used is offered for removal; a client-controlled name is never put in a command.
  assert.ok(!recs.some((r) => r.id === 'mcp-used' || r.id === 'mcp-zero'));
  assert.equal(recs[0].action.cmd, 'claude mcp remove a');
  assert.equal(recs.find((r) => r.id === 'mcp-b c').action.type, 'link');
});

test('recommend: keep-alive is not suggested when it is already on, or has no upside', () => {
  assert.ok(!V2.recommend({ rec: { n: 10, lo_usd: 40, hi_usd: 400 }, keepaliveOn: true }).some((r) => r.id === 'keepalive'));
  assert.ok(!V2.recommend({ rec: { n: 10, lo_usd: -5, hi_usd: 0 } }).some((r) => r.id === 'keepalive'));
  assert.deepEqual(V2.recommend({}), []);
});

test('sessions: shares are against the whole-window bill, not the returned page', () => {
  const m = V2.sessions([{ cost_usd: 10 }, { cost_usd: 30 }], 500, 100);
  assert.equal(m.count, 500); assert.equal(m.bill, 100); assert.equal(m.topShare, 40); assert.equal(m.list[0].cost_usd, 30);
});

test('period: all time has no bounds; 30 days ends on an hour', () => {
  const now = Date.UTC(2026, 9, 11, 10, 20);
  assert.deepEqual([V2.period('all', now).since, V2.period('all', now).until], [0, 0]);
  const p = V2.period('30d', now); assert.equal(p.until % 3600000, 0); assert.equal(p.until - p.since, 30 * 86400000);
  assert.equal(V2.period('nonsense', now).key, '30d');
});

test('daily: saved is net of our own model calls', () => {
  const [d] = V2.daily([{ ts: 1, cost_usd: 10, cg_llm_cost_usd: 1, saved_usd: 5, cachesplit_saved_usd: 0.5 }]);
  assert.equal(d.saved, 4.5); assert.equal(d.bill, 11);
});

test('shortId never returns the whole id', () => {
  assert.equal(V2.shortId('tenant:3887ca6c-e309-4683-820d-8479ccf51339'), '3887ca6c');
});

// Text budgets. The pages are built from these strings, so the banned words are checked at the source.
test('no filler words in the dashboard source', () => {
  for (const f of ['ui/v2.js', 'ui/v2model.js', 'ui/home.html']) {
    const t = fs.readFileSync(new URL('./' + f, import.meta.url), 'utf8');
    assert.doesNotMatch(t, /\b(leverag\w*|robust\w*|comprehensive\w*|seamless\w*|utili[sz]e\w*|delve\w*)\b/i, f);
  }
});

test('recommend: an unpriced inventory still ranks never-called servers, by tokens, with no dollars', () => {
  const t = V2.tools({ totals: { priced: false, unused_tokens: 90, declared_tokens: 100 }, servers: [{ server: 'a', tools: 3, tools_used: 0, tokens: 40, unused_usd: 0 }, { server: 'b', tools: 3, tools_used: 0, tokens: 60, unused_usd: 0 }] });
  const recs = V2.recommend({ tools: t });
  assert.deepEqual(recs.map((r) => r.id), ['mcp-b', 'mcp-a']); assert.equal(recs[0].low, 0); assert.equal(recs[0].alt, 'price unknown');
});

test('old classic bookmarks are recognised; the five page names and an empty hash are not', () => {
  for (const h of ['#/overview', '#/savings/usage', '#savings/usage', '#/behaviour/keepalive?x=1']) assert.equal(V2.isClassicHash(h), true, h);
  for (const h of ['', '#', '#home', '#savings', '#cache?p=7d', '#tools', '#sessions']) assert.equal(V2.isClassicHash(h), false, h);
});
