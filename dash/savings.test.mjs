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
