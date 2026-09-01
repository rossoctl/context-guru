// The dashboard's URL contract, tested against the REAL resolver in app.js.
//
// Every filtered view of this dashboard is a link, and those links are pasted into issues,
// runbooks and commit messages — and two of the shapes are written by the SERVER
// (dash/kvcache.go:510-511), so they cannot be found and updated. Adding a second nav level
// changed the canonical hash from `#<view>` to `#/<group>/<view>`, which is precisely the
// kind of change that silently breaks every link ever written. Hence a table rather than a
// hand check.
//
// There is no bundler and no test framework in this project, on purpose: `node --test` and
// `node:assert` ship with node. app.js is a classic script, so it is loaded the way the
// browser loads it — as source, into a function scope, over a stub DOM small enough to read.
// Nothing here is a second implementation of the resolver; a change to app.js changes what
// this file tests.
//
//   node --test dash/navhash.test.mjs        (or: go test ./dash/ -run NavHash)
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

// Beside dash/, not inside dash/ui/, because `//go:embed ui` (dash/ui.go:29) would
// otherwise ship this file inside the proxy binary and serve it at /dashboard/.
const read = (f) => readFileSync(new URL('./ui/' + f, import.meta.url), 'utf8');

// ── the stub DOM ───────────────────────────────────────────────────────────
// The nav is the only DOM the resolver touches, and it reads it through exactly one
// selector shape. So the tab list is parsed out of index.html and the three self-mounting
// views' own mountTab() calls, which makes this a test of the real nav rather than of a
// hand-written copy of it: a tab moved between groups in the markup moves here too.
function navFromSource() {
  const groups = new Map();
  const mounted = [];
  const html = read('index.html');
  for (const [, group, body] of html.matchAll(
    /<div class="viewtabs"[^>]*data-group="([a-z]+)"[^>]*>([\s\S]*?)<\/div>/g)) {
    groups.set(group, [...body.matchAll(/data-view="([a-z]+)"/g)].map((m) => m[1]));
  }
  // The self-mounters declare their own group and position; splice them in the same way
  // mountTab does, so `after:` is exercised rather than assumed.
  for (const f of ['tools.js', 'kvcache.js', 'campaigns.js']) {
    const call = read(f).match(/mountTab\(\{([\s\S]*?)\}\)/);
    assert.ok(call, `${f} no longer calls mountTab()`);
    const field = (k) => (call[1].match(new RegExp(k + ":\\s*'([a-z]+)'")) || [])[1];
    const [group, view, after] = [field('group'), field('view'), field('after')];
    const list = groups.get(group);
    assert.ok(list, `${f} mounts into group ${group}, which index.html does not define`);
    const at = list.indexOf(after);
    assert.ok(at >= 0, `${f} mounts after ${after}, which is not in group ${group}`);
    list.splice(at + 1, 0, view);
    mounted.push([view, group]);
  }
  return { groups, mounted };
}
const { groups: NAV, mounted: MOUNTED } = navFromSource();

/** loadApp evaluates app.js over the stub and hands back the internals under test. */
function loadApp() {
  const loc = { pathname: '/dashboard/', hash: '' };
  // Only the one selector shape the nav reads, and a tab object with only the three
  // properties reachable() and firstView() look at. Anything else returns empty, which is
  // what a null-DOM would do anyway.
  const querySelectorAll = (sel) => {
    const m = /^\.viewtabs\[data-group="([a-z]+)"\] \.tab$/.exec(sel);
    return (m ? NAV.get(m[1]) || [] : []).map((v) => ({
      hidden: false, dataset: { view: v }, getAttribute: () => null,
    }));
  };
  const noop = () => {};
  const stub = {
    document: {
      addEventListener: noop, querySelector: () => null, querySelectorAll,
      documentElement: { getAttribute: noop, setAttribute: noop }, head: { appendChild: noop },
      createElement: () => ({ setAttribute: noop, appendChild: noop, classList: {}, style: {} }),
    },
    window: { addEventListener: noop },
    localStorage: { getItem: () => null, setItem: noop, removeItem: noop },
    location: loc,
    EventSource: function EventSource() {}, setInterval: noop, setTimeout: noop, fetch: noop,
  };
  const names = Object.keys(stub);
  const api = new Function(...names,
    read('app.js') + '\n;return { parseURL, urlFor, resolveNav, navPath, state, DIMS, GROUP_OF };')(
    ...names.map((n) => stub[n]));
  api.at = (hash) => { loc.hash = hash; return api.parseURL(); };
  // urlFor reads `state`, so a round-trip is: parse a hash, adopt it, write it back.
  api.roundTrip = (hash) => {
    const w = api.at(hash);
    Object.assign(api.state, w);
    return api.urlFor().replace('/dashboard/', '');
  };
  return api;
}
const app = loadApp();
// mountTab() does exactly this to GROUP_OF when a self-mounting view loads. Doing it here is
// what makes the three appended views' hashes testable without a browser; the groups and the
// positions come from their own mountTab() calls, not from a list in this file.
for (const [view, group] of MOUNTED) app.GROUP_OF.set(view, group);

const VIEWS = [...NAV.values()].flat().concat('overview');

// ── 1. every view, by its bare legacy name and in canonical form ───────────
test('all 17 views resolve from a bare #<view> and canonicalise to #/<group>/<view>', () => {
  assert.equal(VIEWS.length, 17, 'the nav is no longer 17 views; update this table');
  for (const v of VIEWS) {
    assert.equal(app.at('#' + v).view, v, `#${v} must still resolve to ${v}`);
    const canon = app.roundTrip('#' + v);
    assert.equal(app.at(canon).view, v, `${canon} must resolve back to ${v}`);
    assert.match(canon, /^#\//, `${canon} is not in canonical #/... form`);
    // Overview is a group AND a view, so its canonical path is the one that is not doubled.
    const want = v === 'overview' ? '#/overview' : '#/' + app.navPath(v);
    assert.equal(canon, want);
    assert.equal(app.at(want).view, v);
  }
});

// ── 2. all 14 filter dimensions survive a round trip ──────────────────────
test('all 14 filter dimensions are read and written', () => {
  const dims = app.DIMS.map(([k]) => k);
  assert.deepEqual(dims, ['q', 'model', 'provider', 'agent', 'preset', 'mode', 'component',
    'reason', 'accounting', 'effort', 'thinking', 'stop_reason', 'session', 'tenant']);
  for (const k of dims) {
    const got = app.at(`#requests?${k}=v1`);
    assert.equal(got.filter[k], 'v1', `${k} is not parsed out of the hash`);
    assert.match(app.roundTrip(`#requests?${k}=v1`), new RegExp(`[?&]${k}=v1`),
      `${k} is parsed but not written back`);
  }
  // And together, so no dimension is dropped when another is set.
  const all = dims.map((k) => `${k}=${k}v`).join('&');
  const got = app.at('#requests?' + all);
  for (const k of dims) assert.equal(got.filter[k], k + 'v');
});

// ── 3. the time window ────────────────────────────────────────────────────
test('from/to take a relative token or absolute ms, and `to` is omitted while now', () => {
  assert.equal(app.at('#usage?from=now-24h').from, 'now-24h');
  assert.equal(app.at('#usage?from=now-24h').to, 'now');
  assert.equal(app.at('#usage?from=1700000000000').from, 1700000000000);
  assert.equal(app.at('#usage?from=1700000000000&to=1700000900000').to, 1700000900000);
  assert.equal(app.at('#usage?from=now-2d&to=now-1d').to, 'now-1d');
  assert.ok(!app.roundTrip('#usage?from=now-6h').includes('to='),
    'a live window must stay live for whoever the link is sent to');
  assert.ok(app.roundTrip('#usage?from=now-2d&to=now-1d').includes('to=now-1d'));
  // Junk is all-time rather than NaN, which would send `since=NaN` to the API.
  assert.equal(app.at('#usage?from=tomorrow').from, 0);
  assert.equal(app.at('#usage?from=-5').from, 0);
});

// ── 4. LEGACY range=<ms>. Read, never written. docs/dashboard.md documents it. ──
test('legacy range=<ms> still maps onto the equivalent relative window', () => {
  for (const [ms, want] of [[86400000, 'now-1d'], [3600000, 'now-1h'], [604800000, 'now-1w'],
    [300000, 'now-5m'], [1000, 'now-1s'], [1500, 'now-2s']]) {
    assert.equal(app.at(`#usage?range=${ms}`).from, want, `range=${ms}`);
  }
  // The documented example, end to end: docs/dashboard.md.
  assert.equal(app.roundTrip('#usage?range=86400000'), '#/savings/usage?from=now-1d');
  assert.ok(!app.roundTrip('#usage?range=86400000').includes('range='),
    'range= is read for compatibility and never written');
});

// ── 5. sort/dir — written only on components, parsed anywhere ──────────────
test('sort and dir parse on any view and are written only on components', () => {
  assert.equal(app.at('#components?sort=saved&dir=asc').sort, 'saved');
  assert.equal(app.at('#components?sort=saved&dir=asc').dir, 'asc');
  assert.equal(app.at('#components?sort=saved').dir, 'desc', 'dir defaults to desc');
  assert.equal(app.at('#requests?sort=saved').sort, 'saved', 'sort parses on every view');
  assert.ok(app.roundTrip('#components?sort=saved&dir=asc').includes('sort=saved&dir=asc'));
  assert.ok(!app.roundTrip('#requests?sort=saved').includes('sort='),
    'a sort of another view’s table is not this view’s state');
});

// ── 6. the drawer: req | diff | acct, mutually exclusive ──────────────────
test('the drawer keys are mutually exclusive and round-trip', () => {
  assert.deepEqual(app.at('#requests?req=9').drawer, { req: 9 });
  assert.deepEqual(app.at('#sessions?diff=s-1').drawer, { diff: 's-1' });
  assert.deepEqual(app.at('#tenants?acct=a-1').drawer, { acct: 'a-1' });
  assert.deepEqual(app.at('#requests?req=9&diff=s-1').drawer, { req: 9 }, 'req wins');
  assert.equal(app.at('#requests').drawer, null);
  assert.equal(app.at('#requests?req=0').drawer, null, 'req=0 is not a request');
  assert.equal(app.roundTrip('#requests?req=9'), '#/traffic/requests?req=9');
  assert.equal(app.roundTrip('#sessions?diff=s-1'), '#/traffic/sessions?diff=s-1');
});

// ── 7. THE TWO SHAPES THE SERVER WRITES ───────────────────────────────────
// dash/kvcache.go:510-511 builds these, asserted in dash/kvcache_test.go:380,383, and
// dash/uikvcache_test.go:267 asserts the UI must NOT build them — so the server stays their
// sole author and these strings cannot be found by grepping the front end.
test('the hashes dash/kvcache.go writes still resolve', () => {
  const req = app.at('#requests?req=1234');
  assert.equal(req.view, 'requests');
  assert.deepEqual(req.drawer, { req: 1234 });
  // A session id is client-supplied, so the server percent-escapes it; decoding is
  // URLSearchParams' job and a `/` in the id must not read as a path separator.
  const diff = app.at('#sessions?diff=sess%2Fwith%20space');
  assert.equal(diff.view, 'sessions');
  assert.deepEqual(diff.drawer, { diff: 'sess/with space' });
});

// ── 8. two-level paths ────────────────────────────────────────────────────
test('#/group/view, #/group alone, and a stale group segment', () => {
  assert.equal(app.at('#/behaviour/kvcache').view, 'kvcache');
  assert.equal(app.at('#/savings/campaigns').view, 'campaigns');
  // The leading slash is written but not required, which is what settles the ambiguity the
  // one-level form left: the LAST segment is the view, so `savings/campaigns` cannot be
  // read as a view literally named "savings/campaigns".
  assert.equal(app.at('#savings/campaigns').view, 'campaigns');
  assert.equal(app.roundTrip('#savings/campaigns'), '#/savings/campaigns');
  // A group on its own opens its first tab.
  assert.equal(app.at('#/savings').view, 'usage');
  assert.equal(app.at('#/behaviour').view, 'components');
  assert.equal(app.at('#/traffic').view, 'sessions');
  assert.equal(app.at('#/admin').view, 'config');
  assert.equal(app.at('#/overview').view, 'overview');
  // A view that moved group is still found; the segment before it is only a hint.
  assert.equal(app.at('#/admin/campaigns').view, 'campaigns');
  assert.equal(app.roundTrip('#/admin/campaigns'), '#/savings/campaigns');
  // A group that survived and a view name that did not.
  assert.equal(app.at('#/savings/typo').view, 'usage');
  // Filters ride along on both forms.
  assert.equal(app.at('#/savings/usage?model=m1').filter.model, 'm1');
});

// ── 9. junk resolves to Overview rather than to a blank page ───────────────
test('an unknown, empty or mis-cased hash resolves to overview', () => {
  for (const h of ['', '#', '#/', '#nonsense', '#OVERVIEW', '#Usage', '#/no/such/thing',
    '#view-usage', '#/nonsense/nonsense']) {
    assert.equal(app.at(h).view, 'overview', `${JSON.stringify(h)} should fall back`);
  }
  // Case-sensitive on purpose: #OVERVIEW is not a near-miss to be corrected, it is a name
  // that does not exist, and go() rewrites the address bar so it does not stay wrong.
  assert.equal(app.roundTrip('#OVERVIEW'), '#/overview');
  assert.equal(app.roundTrip('#nonsense'), '#/overview');
  // Empty parameters are dropped rather than becoming empty filters.
  const got = app.at('#requests?req=9&&&');
  assert.equal(got.view, 'requests');
  assert.deepEqual(got.drawer, { req: 9 });
  assert.equal(app.roundTrip('#requests?req=9&&&'), '#/traffic/requests?req=9');
  assert.deepEqual(app.at('#requests?model=').filter, {}, 'an empty value is not a filter');
});

// ── 10. every view in the nav is a real view, and vice versa ──────────────
test('the nav, the loaders and the view sections agree', () => {
  const html = read('index.html');
  for (const v of VIEWS) {
    assert.ok(app.GROUP_OF.has(v) || ['tools', 'kvcache', 'campaigns'].includes(v),
      `${v} has no group`);
  }
  // A tab whose section is missing is a tab that switches to a blank page. The three
  // self-mounted sections are built in JS, so they are checked in their own files.
  for (const v of VIEWS) {
    if (['tools', 'kvcache', 'campaigns'].includes(v)) continue;
    assert.ok(html.includes(`id="view-${v}"`), `no section for ${v}`);
    assert.ok(html.includes(`aria-labelledby="tab-${v}"`), `${v}'s panel names no tab`);
    assert.ok(html.includes(`aria-controls="view-${v}"`), `${v}'s tab controls no panel`);
  }
});
