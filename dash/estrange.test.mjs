// Review of #1001, point 2: a held estimate whose floor is a COST must show it.
//
// estRange (dash/ui/app.js) shows "$low to $high" only when low > 0. When the floor is zero or negative it falls back
// to the ceiling alone, so a request whose true saving may be nothing, or a cost, reads as "≈ $high". The negative floor
// is new data (a row written before the column has exactly 0), so a negative one is never ambiguous.
//
//   node --test dash/estrange.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const src = readFileSync(new URL('./ui/app.js', import.meta.url), 'utf8');
// The three helpers are plain functions at the top level of a classic script: cut them out by name and run them.
const cut = (name) => {
  const m = src.match(new RegExp('(?:^|\\n)(function ' + name + '\\([^)]*\\) \\{[\\s\\S]*?\\n\\})', 'm'))
    || src.match(new RegExp('(?:^|\\n)(function ' + name + '\\([^)]*\\) \\{[^\\n]*\\})', 'm'));
  assert.ok(m, 'function ' + name + ' not found in app.js');
  return m[1];
};
const nf = new Intl.NumberFormat('en-US');
// usd() reads the page's money unit (dash/moneyunit.test.mjs); these figures are dollars.
const { usd, estRange } = new Function('nf', "let moneyUnit = 'USD';\n" + cut('usd') + '\n' + cut('estUsd') + '\n' + cut('estRange') +
  '\nreturn { usd, estRange };')(nf);

test('a floor that is a cost is shown beside the ceiling', () => {
  const out = estRange(-0.0017, 0.0075);
  assert.ok(out.includes(usd(-0.0017)) && out.includes(usd(0.0075)),
    'got "' + out + '": the range hides that the true saving may be a cost');
});

test('a positive floor still reads as a range', () => {
  assert.equal(estRange(0.0008, 0.01), usd(0.0008) + ' to ' + usd(0.01));
});
