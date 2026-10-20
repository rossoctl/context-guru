// Headless Chrome via the DevTools protocol: screenshot, DOM values, JS errors, overflow, contrast.
// No npm packages: Node's built-in WebSocket and fetch.
import { writeFileSync, readFileSync, mkdirSync } from 'node:fs';

const [, , base, expectedPath, outDir, tier] = process.argv;
const exp = JSON.parse(readFileSync(expectedPath, 'utf8'));
mkdirSync(`${outDir}/snapshots`, { recursive: true });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const tab = await (await fetch('http://127.0.0.1:9222/json/new?about:blank', { method: 'PUT' })).json();
const ws = new WebSocket(tab.webSocketDebuggerUrl);
let id = 0; const pending = new Map(); let errors = [];
const send = (method, params = {}) => new Promise((res) => { const i = ++id; pending.set(i, res); ws.send(JSON.stringify({ id: i, method, params })); });
ws.onmessage = (m) => {
  const msg = JSON.parse(m.data);
  if (msg.id && pending.has(msg.id)) { pending.get(msg.id)(msg.result); pending.delete(msg.id); }
  if (msg.method === 'Runtime.exceptionThrown') errors.push(msg.params.exceptionDetails.exception?.description || msg.params.exceptionDetails.text);
  if (msg.method === 'Runtime.consoleAPICalled' && msg.params.type === 'error') errors.push('console.error: ' + msg.params.args.map((a) => a.value ?? a.description).join(' '));
};
await new Promise((r) => (ws.onopen = r));
await send('Runtime.enable'); await send('Page.enable');
const evalJS = async (expr) => (await send('Runtime.evaluate', { expression: expr, returnByValue: true, awaitPromise: true })).result?.value;

// Poll until expr is truthy; false on timeout (the caller records the error, never throws).
async function waitFor(expr, ms = 10000) {
  const t0 = Date.now();
  while (Date.now() - t0 < ms) { if (await evalJS(expr)) return true; await sleep(150); }
  return false;
}

// Read what a reader would: the headline count, the picker, overflow, and contrast. Absent elements are skipped.
const READ = `(() => {
  const out = { values: {}, overflow: [], contrast: {} };
  const head = document.querySelector('#history-body .ins-total strong');
  if (head) { const m = head.innerText.match(/^(\\d+)/); if (m) out.values.headline_never = +m[1]; }
  const sel = document.querySelector('[data-testid=insights-project]');
  if (sel) out.values.picker = [...sel.options].map((o) => o.value).filter(Boolean);
  for (const el of document.querySelectorAll('.view:not([hidden]) *')) {
    if (el.scrollWidth > el.clientWidth + 1 && getComputedStyle(el).overflowX === 'visible' && el.clientWidth > 0 && el.checkVisibility())
      out.overflow.push((el.className || el.tagName) + ' [' + el.scrollWidth + '>' + el.clientWidth + ' h=' + el.clientHeight + ' vis=' + (el.checkVisibility ? el.checkVisibility() : '?') + ']: ' + (el.textContent || '').trim().slice(0, 40));
  }
  const lum = (c) => { const v = c.match(/\\d+(\\.\\d+)?/g).slice(0, 3).map((x) => { x = x / 255; return x <= 0.03928 ? x / 12.92 : ((x + 0.055) / 1.055) ** 2.4; }); return 0.2126 * v[0] + 0.7152 * v[1] + 0.0722 * v[2]; };
  const bgOf = (el) => { while (el) { const b = getComputedStyle(el).backgroundColor; if (b && !/rgba\\(0, 0, 0, 0\\)|transparent/.test(b)) return b; el = el.parentElement; } return 'rgb(255,255,255)'; };
  for (const [label, q] of [['headline', '.ins-total strong'], ['tile', '.tile .v'], ['h2', '.view:not([hidden]) h2']]) {
    const el = document.querySelector(q); if (!el) continue;
    const a = lum(getComputedStyle(el).color), b = lum(bgOf(el));
    out.contrast[label] = Math.round(((Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05)) * 100) / 100;
  }
  out.overflow = out.overflow.slice(0, 10);
  return out;
})()`;

const themes = tier === 'smoke' ? ['light'] : ['light', 'dark'];
const widths = tier === 'smoke' ? [['desktop', 1280]] : [['desktop', 1280], ['phone', 400]];
const projects = [['pooled', '']];
const roots = exp.cleanup.picker;
for (const r of tier === 'smoke' ? roots.slice(0, 1) : roots) projects.push([r.split('/').pop(), r]);
const pages = {};

// Console errors / exceptions raised before the first page (tab open) are kept under a synthetic "boot" page.
if (errors.length) pages.boot = { errors, values: {}, overflow: [], contrast: {} };
errors = [];

async function shoot(name, timedOut) {
  await sleep(1500); // charts animate in
  const shot = await send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: true });
  writeFileSync(`${outDir}/snapshots/${name}.png`, Buffer.from(shot.data, 'base64'));
  const read = await evalJS(READ);
  pages[name] = { errors: [...errors, ...timedOut], values: {}, overflow: [], contrast: {}, ...read };
  errors = [];
}

// Load a route from a blank page so every page is a fresh render and its errors are its own.
async function open(route) {
  await send('Page.navigate', { url: 'about:blank' });
  errors = [];
  await send('Page.navigate', { url: `${base}/dashboard/#${route}` });
}

for (const theme of themes) for (const [wname, w] of widths) {
  await send('Emulation.setDeviceMetricsOverride', { width: w, height: 1600, deviceScaleFactor: 1, mobile: w < 600 });
  await send('Emulation.setEmulatedMedia', { features: [{ name: 'prefers-color-scheme', value: theme }] });
  await open('behaviour/history');
  const hSel = '#history-body .ins-total';
  const hOk = await waitFor(`!!document.querySelector(${JSON.stringify(hSel)})`);
  await shoot(`history-pooled-${theme}-${wname}`, hOk ? [] : [`render timeout: ${hSel}`]);
  for (const [pname, root] of projects) {
    await open('behaviour/insights');
    const body = '#insights-body';
    let ok = await waitFor(`(document.querySelector('${body}')?.innerText || '').trim().length > 0`);
    const timedOut = ok ? [] : [`render timeout: ${body}`];
    if (ok && root) {
      const ready = await waitFor(`!!document.querySelector('[data-testid=insights-project]')`);
      if (!ready) timedOut.push('render timeout: [data-testid=insights-project]');
      else {
        // Mark the current content, change the project, wait for the body to be re-rendered (mark gone).
        await evalJS(`(() => { const b = document.querySelector('${body}'); b.__snapMark = b.firstElementChild; b.__snapText = b.innerText;
          const s = document.querySelector('[data-testid=insights-project]');
          s.value = ${JSON.stringify(root)}; s.dispatchEvent(new Event('change', { bubbles: true })); })()`);
        const re = await waitFor(`(() => { const b = document.querySelector('${body}'); const s = document.querySelector('[data-testid=insights-project]');
          return s && s.value === ${JSON.stringify(root)} && (b.firstElementChild !== b.__snapMark || b.innerText !== b.__snapText); })()`);
        if (!re) timedOut.push(`render timeout: ${body} after project change`);
      }
    }
    await shoot(`cleanup-${pname}-${theme}-${wname}`, timedOut);
  }
}
writeFileSync(`${outDir}/actual-ui.json`, JSON.stringify({ pages }, null, 1));
ws.close(); process.exit(0);
