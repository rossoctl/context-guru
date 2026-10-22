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
