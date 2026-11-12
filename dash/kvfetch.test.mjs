// The Cache detail tab reads its money routes with kvFetch (dash/ui/kvcache.js), not api(). It must
// apply the reply's `currency` the same way, or the tab shows Bobcoins with a `$` sign (#1127 review).
//
//   node --test dash/kvfetch.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const app = readFileSync(new URL('./ui/app.js', import.meta.url), 'utf8');
const kv = readFileSync(new URL('./ui/kvcache.js', import.meta.url), 'utf8');
const cut = (src, name) => {
  const m = src.match(new RegExp('(?:^|\\n)((?:async )?function ' + name + '\\([^)]*\\) \\{[\\s\\S]*?\\n\\})', 'm'));
  assert.ok(m, 'function ' + name + ' not found');
  return m[1];
};

test('kvFetch applies the unit its reply states', async () => {
  const applied = [];
  const stubs = {
    applyMoneyUnit: (u) => applied.push(u),
    kvRateSpecs: () => [], kvParams: (x) => x || {}, qs: () => '', state: {},
    fetch: async () => ({ ok: true, json: async () => ({ currency: 'Bobcoins', rows: [] }) }),
  };
  const kvFetch = new Function(...Object.keys(stubs),
    cut(app, 'readMoneyUnit') + '\n' + cut(kv, 'kvFetch') + '\nreturn kvFetch;')(...Object.values(stubs));
  const body = await kvFetch('kvcache');
  assert.deepEqual(body, { currency: 'Bobcoins', rows: [] });
  assert.deepEqual(applied, ['Bobcoins']);
});
