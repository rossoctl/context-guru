// UI smoke test: every tab loads in a real browser with no JavaScript errors. Run by
// TestUISmokeEveryTab (ui_smoke_test.go) against a fixture server; usage:
//   node ui_smoke.mjs <baseUrl> <chromePath>
// playwright-core is resolved through NODE_PATH.
import { createRequire } from 'node:module';
const require = createRequire(import.meta.url);
const { chromium } = require('playwright-core');

const [, , base, chrome] = process.argv;
// Views the dash package serves end to end: their data must load, so an error state is a failure.
const DATA_VIEWS = ['overview', 'keepalive', 'tools', 'usage', 'sessions', 'requests', 'components', 'kvcache'];
// Views backed by the hosted control plane (not mounted by this fixture server): they must still
// open, show their heading and raise no JavaScript error; their data calls may legitimately fail.
const SHELL_VIEWS = ['config', 'strategies', 'tenants', 'setup', 'settings', 'archive', 'feedback', 'campaigns'];
const REMOVED_OVERVIEW_TEXT = [/Prefix-change exposure/i, /Addressable spend/i, /Removed by you/i, /Our own LLM cost/i,
  /Caching insights/i, /Cache reuse/i, /Your sessions/i, /Token accounting/i];

const browser = await chromium.launch({ executablePath: chrome, args: ['--no-sandbox'] });
const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
const page = await ctx.newPage();
const failures = [];
let problems = [];
page.on('pageerror', (e) => problems.push('pageerror: ' + String(e).slice(0, 200)));
page.on('console', (m) => {
  // A failed network call is logged by the browser as a console error; the control-plane views
  // are expected to have some. Everything else (a thrown handler, a missing function) is not.
  if (m.type() === 'error' && !/Failed to load resource|status of 4\d\d|status of 5\d\d/.test(m.text())) problems.push('console: ' + m.text().slice(0, 200));
});

async function visit(view) {
  problems = [];
  await page.goto(`${base}/dashboard/index.html#/${view}`);
  await page.reload();
  await page.waitForSelector('#view-' + view + ':not([hidden])', { timeout: 30000 }).catch(() => {});
  await page.waitForLoadState('networkidle', { timeout: 30000 }).catch(() => {});
  await page.waitForTimeout(300);
  const info = await page.evaluate((v) => {
    const panel = document.getElementById('view-' + v);
    const head = document.querySelector(`.page-head[data-view="${v}"]`);
    return {
      visible: !!panel && !panel.hidden,
      title: head && !head.hidden ? (head.querySelector('h1') || {}).textContent : '',
      intro: head && !head.hidden ? (head.querySelector('.tab-intro') || {}).textContent : '',
      errors: panel ? [...panel.querySelectorAll('.empty.error, [role=alert]')].map((e) => e.textContent.trim().slice(0, 120)) : [],
      text: panel ? panel.innerText : '',
      headings: panel ? [...panel.querySelectorAll('h1,h2,h3,summary,.lv-name h3,dt')].map((e) => e.textContent) : [],
    };
  }, view);
  if (!info.visible) failures.push(`${view}: its panel did not open`);
  if (!info.title) failures.push(`${view}: no heading`);
  if (!info.intro || info.intro.length < 20) failures.push(`${view}: no one-line intro`);
  for (const p of problems) failures.push(`${view}: ${p}`);
  if (DATA_VIEWS.includes(view) && info.errors.length) failures.push(`${view}: error state shown: ${info.errors.join(' | ')}`);
  return info;
}

for (const v of [...DATA_VIEWS, ...SHELL_VIEWS]) {
  const info = await visit(v);
  if (v === 'overview') {
    for (const id of ['saved-cache', 'saved-compaction', 'saved-filter']) {
      if (!(await page.$(`[data-testid="${id}"]`))) failures.push(`overview: missing the ${id} row`);
    }
    const total = await page.$('[data-testid="saved-total"]');
    if (!total) failures.push('overview: the "Together" line under the three savings is missing');
    else {
      const t = await total.innerText();
      if (!/estimated saved/.test(t) || !/(up to \$|cost more than it saved)/.test(t)) failures.push('overview: the Together line must show the estimate and the cache-strategy upper bound apart: ' + t);
    }
    const cacheRow = await (await page.$('[data-testid="saved-cache"]')).innerText();
    if (!/Cache strategy/.test(cacheRow)) failures.push('overview: the cache row does not use the user term "Cache strategy": ' + cacheRow.slice(0, 80));
    if (!(await page.$('#overview-diagnostics:not([open])'))) failures.push('overview: Detailed diagnostics must exist and start closed');
    const rows = await page.$$('#tab-map dd');
    if (rows.length < 5) failures.push(`overview: About the other tabs lists ${rows.length} tabs`);
    // Section titles and row labels must be gone; the four metric names must not appear anywhere.
    for (const re of REMOVED_OVERVIEW_TEXT) {
      const metric = /Prefix-change|Addressable|Removed by you|Our own LLM/i.test(re.source);
      if (info.headings.some((h) => re.test(h)) || (metric && re.test(info.text))) failures.push(`overview: still shows ${re}`);
    }
    // Detailed diagnostics is the last thing on the page, and latency lives inside it.
    const order = await page.evaluate(() => {
      const d = document.getElementById('overview-diagnostics');
      const last = document.getElementById('view-overview').lastElementChild;
      return { last: last === d, latencyInside: !!d.querySelector('#chart-latency') };
    });
    if (!order.last) failures.push('overview: Detailed diagnostics is not at the bottom');
    if (!order.latencyInside) failures.push('overview: latency is outside Detailed diagnostics');
  }
}

// The Benchmarks tab is gone: no nav entry, no panel, and its address falls back to Overview.
await page.goto(`${base}/dashboard/index.html#/benchmarks`);
await page.reload();
await page.waitForTimeout(500);
if (await page.$('[data-testid="tab-benchmarks"], #view-benchmarks, #tpl-tab-benchmarks')) failures.push('benchmarks: the tab is still in the page');
const res = await page.evaluate(async () => (await fetch('/api/benchmarks')).status);
if (res !== 404) failures.push(`benchmarks: /api/benchmarks answered ${res}, want 404`);

// Links to the retired five-page home still land somewhere useful.
for (const [hash, view] of [['#home', 'overview'], ['#cache', 'keepalive'], ['#sessions', 'sessions'], ['#tools', 'tools']]) {
  await page.goto(`${base}/dashboard/${hash}`);
  await page.reload();
  await page.waitForTimeout(500);
  const open = await page.evaluate(() => [...document.querySelectorAll('.view')].find((v) => !v.hidden)?.id);
  if (open !== 'view-' + view) failures.push(`${hash} opened ${open}, want view-${view}`);
}
const moved = await page.evaluate(async () => (await fetch('/dashboard/home.html')).url);
if (!moved.endsWith('/dashboard/index.html')) failures.push(`/dashboard/home.html resolved to ${moved}, want the dashboard`);

await browser.close();
if (failures.length) { console.error(failures.join('\n')); process.exit(1); }
console.log(`ok: ${DATA_VIEWS.length + SHELL_VIEWS.length} tabs loaded`);
