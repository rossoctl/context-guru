// Headless Chrome via the DevTools protocol: screenshot, DOM values, JS errors, overflow, contrast.
// No npm packages: Node's built-in WebSocket and fetch.
import { writeFileSync, readFileSync, mkdirSync } from 'node:fs';

const [, , base, expectedPath, outDir, tier] = process.argv;
const exp = JSON.parse(readFileSync(expectedPath, 'utf8'));
mkdirSync(`${outDir}/snapshots`, { recursive: true });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const tab = await (await fetch('http://127.0.0.1:9222/json/new?about:blank', { method: 'PUT' })).json();
const ws = new WebSocket(tab.webSocketDebuggerUrl);
const urls = new Map(); let id = 0; const pending = new Map(); let errors = [];
const send = (method, params = {}) => new Promise((res) => { const i = ++id; pending.set(i, res); ws.send(JSON.stringify({ id: i, method, params })); });
ws.onmessage = (m) => {
  const msg = JSON.parse(m.data);
  if (msg.id && pending.has(msg.id)) { pending.get(msg.id)(msg.result); pending.delete(msg.id); }
  const m_ = msg.method, pr = msg.params;
  if (m_ === 'Network.requestWillBeSent') urls.set(pr.requestId, pr.request.url);
  if (m_ === 'Network.responseReceived' && pr.response.status >= 400 && pr.response.url.startsWith(base)) errors.push(`http ${pr.response.status} ${pr.response.url}`);
  if (m_ === 'Network.loadingFailed' && !pr.canceled && (urls.get(pr.requestId) || '').startsWith(base)) errors.push(`net ${pr.errorText} ${urls.get(pr.requestId)}`);
  if (m_ === 'Log.entryAdded' && pr.entry.level === 'error') errors.push(`log.error: ${pr.entry.text} ${pr.entry.url || ''}`.trim());
  if (msg.method === 'Runtime.exceptionThrown') errors.push(msg.params.exceptionDetails.exception?.description || msg.params.exceptionDetails.text);
  if (msg.method === 'Runtime.consoleAPICalled' && msg.params.type === 'error') errors.push('console.error: ' + msg.params.args.map((a) => a.value ?? a.description).join(' '));
};
await new Promise((r) => (ws.onopen = r));
await send('Runtime.enable'); await send('Page.enable'); await send('Network.enable'); await send('Log.enable');
const evalJS = async (expr) => (await send('Runtime.evaluate', { expression: expr, returnByValue: true, awaitPromise: true })).result?.value;

// Poll until expr is truthy; false on timeout (the caller records the error, never throws). Counted in polls,
// not Date.now(): a host sleep steps the VM clock forward on resume, which would time out a page that never ran.
async function waitFor(expr, ms = 30000) {
  for (let i = 0; i * 150 < ms; i++) { if (await evalJS(expr)) return true; await sleep(150); }
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
  // Visible text only: body.innerText omits hidden nodes. A bare undefined/NaN/null is a template reading a missing field.
  const txt = document.body.innerText, bad = [], re = /\\b(undefined|NaN|null)\\b|\\[object Object\\]/g;
  for (let m; (m = re.exec(txt)) && bad.length < 10; )
    bad.push(txt.slice(Math.max(0, m.index - 30), m.index + m[0].length + 30).replace(/\\s+/g, ' '));
  out.values.bad_tokens = bad;
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
  // Open every <details> in the page bodies (not the header filter popovers) so hidden text/overflow is checked, and the PNG shows what was checked.
  await evalJS(`document.querySelectorAll('#history-body details, #insights-body details').forEach((d) => { d.open = true; })`);
  await evalJS(`new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)))`);
  await sleep(300);
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
    const body = '#insights-body', h2 = '#insights-body .panel h2'; // "Loading..." has no panel; a real render does
    let ok = await waitFor(`!!document.querySelector('${h2}')`);
    const timedOut = ok ? [] : [`render timeout: ${h2}`];
    if (ok && root) {
      const ready = await waitFor(`!!document.querySelector('[data-testid=insights-project]')`);
      if (!ready) timedOut.push('render timeout: [data-testid=insights-project]');
      else {
        // Tag the current h2 nodes, change the project, wait until a NEW (untagged) h2 exists: the view re-rendered.
        await evalJS(`(() => { document.querySelectorAll('${h2}').forEach((e) => { e.__old = true; });
          const s = document.querySelector('[data-testid=insights-project]');
          s.value = ${JSON.stringify(root)}; s.dispatchEvent(new Event('change', { bubbles: true })); })()`);
        const re = await waitFor(`(() => { const s = document.querySelector('[data-testid=insights-project]');
          const h = document.querySelector('${h2}');
          return !!s && s.value === ${JSON.stringify(root)} && !!h && !h.__old; })()`);
        if (!re) timedOut.push(`render timeout: ${h2} after project change`);
      }
    }
    await shoot(`cleanup-${pname}-${theme}-${wname}`, timedOut);
  }
}
writeFileSync(`${outDir}/actual-ui.json`, JSON.stringify({ pages }, null, 1));
ws.close(); process.exit(0);
