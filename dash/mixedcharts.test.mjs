// A view whose costs are in more than one unit (dash/currency.go says "mixed") has no money total, so a
// money chart must not draw one: Bobcoins and dollars added into one line is a wrong number (#1127
// review). Each money chart shows an empty state instead, and no text calls a mixed sum "dollars".
//
//   node --test dash/mixedcharts.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const app = readFileSync(new URL('./ui/app.js', import.meta.url), 'utf8');
const charts = readFileSync(new URL('./ui/charts.js', import.meta.url), 'utf8');
// A function's source, at any indent: from its header to the first closing brace at the same indent.
const body = (src, name) => {
  const m = src.match(new RegExp('\\n( *)(?:async )?function ' + name + '\\([^)]*\\) \\{[\\s\\S]*?\\n\\1\\}'));
  assert.ok(m, 'function ' + name + ' not found');
  return m[0];
};
const decl = (name) => {
  const m = app.match(new RegExp('(?:^|\\n)((?:let|const) ' + name + ' = [^\\n]*;)'));
  assert.ok(m, name + ' not found in app.js');
  return m[1];
};

const empties = [];
const { setMoneyUnit, moneyWord, noMoneyChart } = new Function('emptyState',
  decl('moneyUnit') + '\n' + decl('MONEY_UNIT_RE') + '\n' + body(app, 'setMoneyUnit') + '\n' +
  body(app, 'moneyWord') + '\n' + body(app, 'noMoneyChart') + '\nreturn { setMoneyUnit, moneyWord, noMoneyChart };')(
  (host, title, detail) => empties.push([host, title, detail]));

test('a mixed view is never called dollars', () => {
  setMoneyUnit('mixed');
  assert.notEqual(moneyWord(), 'dollars');
  setMoneyUnit('USD');
  assert.equal(moneyWord(), 'dollars');
  setMoneyUnit('Bobcoins');
  assert.equal(moneyWord(), 'Bobcoins');
});

test('a money chart in a mixed view is an empty state with the reason', () => {
  empties.length = 0;
  setMoneyUnit('mixed');
  assert.equal(noMoneyChart('host'), true);
  assert.equal(empties.length, 1);
  assert.match(empties[0][2], /more than one unit, so money is not charted\. Token figures are correct\./);
  for (const u of ['USD', 'Bobcoins']) {
    setMoneyUnit(u);
    assert.equal(noMoneyChart('host'), false, u + ' draws its chart');
  }
  assert.equal(empties.length, 1);
});

test('every money chart asks noMoneyChart first', () => {
  for (const [src, name] of [[charts, 'netSaving'], [charts, 'savingsBars'], [app, 'dailyColumns']]) {
    assert.match(body(src, name), /noMoneyChart\(/, name + ' draws money without checking the unit');
  }
  // The keep-alive tab's cost line. On a subscription it is in tokens, so it is drawn.
  assert.match(app, /sub \|\| !noMoneyChart\(\$\('#ka-chart-usd'\)\)\) lineChart\(\$\('#ka-chart-usd'\)/, '#ka-chart-usd draws money without checking the unit');
});
