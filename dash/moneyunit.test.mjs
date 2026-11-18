// A dashboard view has one money unit, from the API's `currency` (dash/currency.go). usd() (dash/ui/app.js)
// keeps today's `$` output for USD, writes another unit's name after the number (Bob's price file says
// Bobcoins), and shows no money figure for a mixed view.
//
//   node --test dash/moneyunit.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const src = readFileSync(new URL('./ui/app.js', import.meta.url), 'utf8');
const cut = (name) => {
  const m = src.match(new RegExp('(?:^|\\n)(function ' + name + '\\([^)]*\\) \\{[\\s\\S]*?\\n\\})', 'm'));
  assert.ok(m, 'function ' + name + ' not found in app.js');
  return m[1];
};
const decl = (name) => {
  const m = src.match(new RegExp('(?:^|\\n)((?:let|const) ' + name + ' = [^\\n]*;)'));
  assert.ok(m, name + ' not found in app.js');
  return m[1];
};
const nf = new Intl.NumberFormat('en-US');
const { usd, setMoneyUnit } = new Function('nf', decl('moneyUnit') + '\n' + decl('MONEY_UNIT_RE') + '\n' +
  cut('setMoneyUnit') + '\n' + cut('usd') + '\nreturn { usd, setMoneyUnit };')(nf);

// The values and their output before the unit existed: USD must not change by one character.
const usdBefore = [[null, '—'], [undefined, '—'], [0, '$0'], [0.0042, '$0.0042'], [-0.0042, '-$0.0042'],
  [1.75, '$1.75'], [-1.75, '-$1.75'], [999.994, '$999.99'], [12345.6, '$12,346']];

test('USD reads exactly as before', () => {
  setMoneyUnit('USD');
  for (const [v, want] of usdBefore) assert.equal(usd(v), want, 'usd(' + v + ')');
});

test('Bobcoins write the unit name after the number', () => {
  setMoneyUnit('Bobcoins');
  assert.equal(usd(1.75), '1.75 Bobcoins');
  assert.equal(usd(0.0042), '0.0042 Bobcoins');
  assert.equal(usd(-1.75), '-1.75 Bobcoins');
  assert.equal(usd(0), '0 Bobcoins');
  assert.equal(usd(12345.6), '12,346 Bobcoins');
  assert.equal(usd(null), '—');
});

test('a mixed view shows no money figure', () => {
  setMoneyUnit('mixed');
  assert.equal(usd(1.75), '—');
  assert.equal(usd(0), '—');
});

test('a unit name that is not plain letters never reaches the page', () => {
  for (const bad of ['<img src=x onerror=alert(1)>', 'Bob coins', '$', '', 42, undefined]) {
    setMoneyUnit(bad);
    assert.equal(usd(1.75), '—', 'unit ' + JSON.stringify(bad));
  }
});
