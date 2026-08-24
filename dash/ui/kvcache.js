// The KV-cache TTL view: how long a conversation actually stays idle, what its cached prefix
// costs to hold, and what a different TTL strategy would have cost on exactly this history.
//
// It is one appended file and it mounts itself — the tab, the section, the stylesheet and the
// loader registration all happen here — so this feature is one line in the shared page. Every
// helper it uses is app.js's (el, clear, num, compact, usd, pct, dur, when, tile, tileGroup,
// barRows, lineChart, emptyState, tableMessage, api, go, setFilter, wideScope), because a
// second design system on one page is how a page stops looking like one product.
//
// THE FOUR RULES THIS FILE IS STRUCTURED AROUND. Each is a way this exact page could look
// completely fine while lying:
//
//   1. NO ARITHMETIC HERE. Every dollar, every percentage and every idle average is computed
//      on the server, in Go, in one place — and so are the cost FORMULAS, which arrive as data
//      and are printed rather than retyped. The browser has twice duplicated a table the
//      server owns and drifted from it; a hardcoded rate in a UI component is the specific
//      version of that mistake this page would make, so there is not one number in this file
//      that could be a price.
//   2. A REQUEST WITH NO NEXT REQUEST HAS NO IDLE TIME. Not zero. Every average on this page
//      is over the requests that have a successor, the rest are counted beside it, and the
//      table renders "no next request" rather than a duration.
//   3. A ZERO MAY BE AN ABSENCE. cache_ttl arrived as an additive column defaulting to '', so
//      on older rows a blank tier means NOT RECORDED. The coverage banner says which of the
//      three states the window is in, unconditionally.
//   4. NEGATIVE SAVINGS ARE THE POINT. A strategy that costs more is shown costing more, in
//      red, with its percentage. Clamping that to zero would remove the only result on this
//      page that can stop somebody making a change.
'use strict';

// Its own stylesheet, linked from here rather than from index.html so mounting this feature is
// ONE line in the shared page. Same-origin, so `style-src 'self'` allows it.
document.head.appendChild(el('link', { rel: 'stylesheet', href: 'kvcache.css' }));

// ── mount ──────────────────────────────────────────────────────────────────
// Next to Keep-alive: both are about what a cached prefix costs to hold, and this one is the
// measurement the other one's calculator is an application of.
(function mountKVCacheTab() {
  const tabs = $('.tabs');
  const tab = el('button', {
    role: 'tab', class: 'tab', 'data-view': 'kvcache', 'data-testid': 'tab-kvcache',
    'aria-selected': 'false',
  }, 'KV-cache');
  const after = $('.tab[data-view="keepalive"]', tabs);
  tabs.insertBefore(tab, after ? after.nextSibling : null);
})();

const kvView = el('section', { class: 'view', id: 'view-kvcache', hidden: 'hidden' });
$('#main').appendChild(kvView);

// ── local state ────────────────────────────────────────────────────────────
// Its own state object, NOT app.js's: the shared one is mirrored into the URL for the views
// that own those keys, so sharing it would make a sort here silently reorder another table.
//
// The four filters here are exactly the ones the shared filter bar above does not carry. Model,
// conversation, agent and the time range come from that bar and are sent by qs() on every
// request, which is why they are absent from this object.
const kvc = {
  analysis: null,
  sim: null,
  prices: null,
  page: null,
  // The page's own filters.
  user: '',
  bucket: '',
  ttl: '',
  hasNext: '',
  // The detail table's paging and sort, both server-side: the server orders the WHOLE dataset
  // and returns one page of it, so a sorted column shows the real top rows rather than the top
  // of an arbitrary page. (That is the same distinction app.js draws between Components, which
  // it sorts client-side over a complete result, and Sessions, which it refuses to sort.)
  sort: 'ts',
  dir: 'desc',
  offset: 0,
  limit: 50,
  // The simulation's inputs. Every one of them travels in the query string, so a result is a
  // URL somebody else can open.
  arms: null,
  // '' means "whatever the server's registry says is the honest denominator". Not a literal
  // name: prompt caching is already on in production and already saving ~53%, so scoring
  // against no-cache reports a saving no decision can act on. The domain flags its own
  // baseline arm and the page follows it — a second answer here would be a second definition
  // of the number every percentage is divided by.
  baseline: '',
  // rates holds EDITED rates only, per model, in USD per MILLION tokens exactly as typed. An
  // absent key means "not edited", which is why this is a sparse object rather than a copy of
  // the price list: a copy would turn every configured rate into an override the moment
  // anything was touched.
  rates: {},
  mult: { read: '', w5m: '', w1h: '' },
  ping: { in: '', out: '' },
  sem: { hit: true, ping: true, zero: false },
  sched: { x: '', x1h: '', k: '' },
  custom: { p5m: '', p1h: '', min: '', always: false },
};

/**
 * kvArms is every arm the SERVER says exists, in its order.
 *
 * There is deliberately no list of arm names in this file. There was one — six names, matching
 * a closed list the API also carried — and by the time anyone looked the domain had gained four
 * more (two keep-alive arms, an extend-to-1h arm, and the exact cost ceiling). All four were
 * shipped, tested, and unreachable from the dashboard, and nothing on this page could have
 * shown that. The picker is now built from the payload, so an arm added server-side appears the
 * day it lands.
 */
function kvArms() { return (kvc.sim && kvc.sim.arms) || []; }

/** kvArmNames is the arms a caller may ask the server to run. */
function kvArmNames() { return kvArms().filter((a) => a.selectable).map((a) => a.name); }

/**
 * kvBaselineArms is the arms that may be a DENOMINATOR.
 *
 * An unreachable arm is excluded, and that is not tidiness: `optimal` reads the true
 * next-request time, so every percentage measured against it would be a share of a number no
 * policy can reach. It is a ceiling to compare against, never a floor to divide by.
 */
function kvBaselineArms() { return kvArms().filter((a) => a.selectable && !a.unreachable); }

/** The detail table's columns, in header order. '' for a header that does not sort. */
const KV_COLS = ['ts', 'user', 'conversation', 'model', 'input', 'output', 'read', 'write',
  'cached', 'ttl', 'hit', 'idle', '', '', 'cost', ''];

// ── what the numbers mean ──────────────────────────────────────────────────
//
// Into app.js's TILE_INFO rather than a registry of its own, so all the explanations on this
// dashboard can be read side by side and checked for one voice. Each entry follows the three
// rules that table states: what it is before how it is computed, the CATCH named out loud, and
// never a description of a number the code does not compute.
Object.assign(TILE_INFO, {
  'kv-requests': {
    what: 'How many requests are in this analysis.',
    how: 'Every request matching the filters above, EXCLUDING keep-alive pings that '
      + 'context-guru sent on your behalf.',
    catch: 'Pings are excluded deliberately: counted, one of them would split a real idle '
      + 'gap into two short ones and make every reuse figure on this page look better than '
      + 'it is. If the analysis was capped, the banner above says so and this is the number '
      + 'actually read, not the number that matched.',
  },
  'kv-conversations': {
    what: 'How many distinct trajectories those requests belong to.',
    how: 'A conversation is one account plus one session id, counted as a pair.',
    catch: 'Never the session id alone: it is chosen by the client, so two accounts can '
      + 'present the same one, and keying on it would splice two people’s requests into '
      + 'one trajectory. Conversations with a single request in this window contribute no '
      + 'idle gap at all — the coverage line says how many.',
  },
  'kv-median-idle': {
    what: 'The middle idle time between one request and the next in the same conversation.',
    how: 'The exact median over every request that HAS a next request, in this window.',
    catch: 'Requests with no next request are NOT in it. They have no idle time, and counting '
      + 'them as zero would drag this number toward "returns instantly". The card beside this '
      + 'one says how many were left out.',
  },
  'kv-mean-idle': {
    what: 'The average idle time until the next request in the same conversation.',
    how: 'The arithmetic mean over the same population as the median.',
    catch: 'Idle gaps are heavily skewed — on this service the median is about 15 seconds '
      + 'and the 99th percentile is about 40 minutes — so the mean is well above the '
      + 'typical gap. Read the median for "usually" and this for "what the tail costs".',
  },
  'kv-reuse-5m': {
    what: 'How often a conversation comes back within five minutes — the provider’s '
      + 'default cache lifetime.',
    how: 'Requests whose next request arrived no more than five minutes later, over the '
      + 'requests that have a next request at all.',
    catch: 'This is the ceiling on what a plain five-minute TTL can hit, not a hit rate: a '
      + 'prefix that CHANGED will miss inside five minutes too. The denominator excludes '
      + 'requests with no successor.',
  },
  'kv-reuse-1h': {
    what: 'How often a conversation comes back within an hour — the long tier.',
    how: 'The same measurement at the one-hour horizon.',
    catch: 'The gap between this and the five-minute figure is the most a one-hour TTL could '
      + 'convert, before paying for it. A one-hour write costs 2.0x base input where a '
      + 'five-minute one costs 1.25x, so a small gap here is easily worth less than the '
      + 'premium.',
  },
  'kv-hit-rate': {
    what: 'How often the provider actually served this prefix from its cache.',
    how: 'Requests the provider reported as a cache hit, over all requests in the analysis.',
    catch: 'This is a MEASUREMENT of what happened, not a simulation. It moves for reasons no '
      + 'TTL controls — a changed prefix, a cold start — which is why the simulator '
      + 'below reports those separately as misses no strategy could have rescued.',
  },
  'kv-cost': {
    what: 'What these requests were actually billed.',
    how: 'The sum of each request’s own cost, priced when it was recorded from the rates '
      + 'that were in force at the time.',
    catch: 'Requests whose token accounting was incomplete are NOT in it — their cost is '
      + 'unknown, not zero, and the coverage line counts them. This is the real bill; every '
      + 'figure in the simulator below is a re-pricing of the same traffic under a different '
      + 'policy and at the rates in the pricing panel.',
  },
  'kv-final': {
    what: 'How many requests have no next request in the same conversation.',
    how: 'The last request of every conversation in this window.',
    catch: 'These have NO idle time and are excluded from every average and every percentage '
      + 'above. Their successor may simply be outside the window or not have happened yet, '
      + 'which is why they are counted rather than dropped.',
  },
  'kv-prefix': {
    what: 'The middle size of the cached prompt, in tokens.',
    how: 'The median of cache-read plus cache-write tokens per request — the billed '
      + 'prefix.',
    catch: 'Every cost in the pricing panel is this number times a rate, which is why it is '
      + 'the median and not the mean: per-request cache cost is heavily skewed, so a mean '
      + 'would describe no request. It is not the message text size, which runs about 3.4x '
      + 'lower.',
  },
});

// ── shared bits ────────────────────────────────────────────────────────────

/**
 * kvMoney renders a dollar figure, or says the model had no rates.
 *
 * Rule 1 in one function: usd(0) renders "$0", which is a claim that something was free, and
 * an unpriced model's cost is unknown. Every dollar cell on this page goes through here.
 */
function kvMoney(v, known) {
  if (known === false) {
    return el('span', {
      class: 'pill missing',
      title: 'This ran on a model with no known rates. Its tokens are counted; its cost is not.',
    }, 'unpriced');
  }
  return document.createTextNode(usd(v));
}

/**
 * kvSigned renders a difference against the baseline, IN WORDS as well as by sign, and never
 * clamps it.
 *
 * The word is the whole point. A column of signed dollars under a heading like "saving" was
 * read as a saving when it was the opposite: -$2,117.80 against the baseline is the arm costing
 * two thousand dollars MORE, and a reader scanning for the biggest number picks the worst
 * option. Sign plus colour is not enough either — colour must never carry a judgement alone,
 * which is the rule the rest of this dashboard already keeps.
 */
function kvSigned(v, known) {
  if (known === false) return el('span', { class: 'pill missing' }, 'unpriced');
  if (!v) return el('span', { class: 'muted' }, usd(0));
  const cheaper = v > 0;
  return el('span', {
    class: cheaper ? 'good-text' : 'bad-text',
    title: cheaper ? 'Cheaper than the baseline by this much.'
      : 'This arm costs MORE than the baseline by this much.',
  }, usd(Math.abs(v)), el('span', { class: 'kv-dir' }, cheaper ? ' cheaper' : ' MORE'));
}

/**
 * kvPremium renders the incremental cache premium.
 *
 * Its sign colouring is INVERTED against every other money cell on this page, and that is not
 * a slip: the premium is what the caching machinery itself cost, so a NEGATIVE one means the
 * cache paid for itself. The value is printed exactly as the server sent it — nothing here
 * negates it to make the colour work, because a money figure this page computed would be a
 * money figure nothing tests.
 */
function kvPremium(v, known) {
  if (known === false) return el('span', { class: 'pill missing' }, 'unpriced');
  return el('span', {
    class: v < 0 ? 'good-text' : v > 0 ? 'bad-text' : '',
    title: v < 0 ? 'Negative: caching this traffic cost less than not caching it.'
      : v > 0 ? 'Positive: the cache cost more than sending every prompt uncached.'
        : 'Nothing was cached, so there is no premium either way.',
    text: usd(v),
  });
}

/**
 * kvIdle renders one row's idle time — or says it has none.
 *
 * Rule 2 at the cell level. dur() renders 0 as "—", which would be indistinguishable from
 * "no successor", so a genuine zero-length gap (tied timestamps: 9 of 12,635 consecutive
 * pairs on this service) is spelled out.
 */
function kvIdle(r) {
  if (!r.has_next) {
    return el('span', {
      class: 'pill missing',
      title: 'The last request of this conversation in this window. It has no idle time — not '
        + 'a zero one — so it is excluded from every average on this page.',
    }, 'no next request');
  }
  // === 0, not !idle_ms. A genuine zero-length gap (tied timestamps: 9 of 12,635 consecutive
  // pairs on this service) is a real measurement and reads as "0 s"; an idle time that is ABSENT
  // while has_next is true is a broken contract and must not be rendered as one. `!x` catches
  // both and would print a fabricated zero for the second.
  if (r.idle_ms === null || r.idle_ms === undefined) {
    return el('span', {
      class: 'pill missing',
      title: 'This request has a successor but carries no idle time, which should not be '
        + 'possible. Something upstream of this page is wrong; the gap is not zero.',
    }, 'not recorded');
  }
  if (r.idle_ms === 0) return document.createTextNode('0 s');
  return document.createTextNode(dur(r.idle_ms));
}

/** kvTTLPill names the tier, and how well the tier is known. */
const KV_TTL_LABEL = {
  ephemeral_5m: '5m', ephemeral_1h: '1h', '': 'none', none: 'none',
  // The fourth group is an ABSENCE, not a tier, and it is labelled as one. Those rows are
  // replayed as five minutes so a simulation has something to run, but calling them "5m" in the
  // grouped table said "N of your requests used the 5-minute tier" about rows the coverage banner
  // directly above was reporting as not recorded.
  unrecorded: 'not recorded',
};
const KV_SOURCE_NOTE = {
  configured: 'The tier this request asked for, as recorded.',
  observed: 'Not recorded on the row, but deduced from what the provider billed.',
  unknown: 'Not recorded and not deducible. Taken as the provider default of five minutes so '
    + 'the simulation has something to replay, and counted as uncovered above.',
};
function kvTTLPill(r) {
  // A row whose tier was never recorded says SO, in the label.
  //
  // It used to render the assumed tier — a pink "5m" pill with the explanation in a tooltip —
  // and that is the same defect the grouped table had: the visible text asserted a tier nobody
  // configured, on a page whose own coverage banner was reporting that row as not recorded. A
  // colour and a hover are not where a contradiction gets resolved.
  const unknown = r.ttl_source === 'unknown';
  const label = unknown ? KV_TTL_LABEL.unrecorded
    : KV_TTL_LABEL[r.ttl] !== undefined ? KV_TTL_LABEL[r.ttl] : r.ttl;
  const cls = unknown ? 'pill missing' : r.ttl === 'ephemeral_1h' ? 'pill neutral' : 'pill';
  return el('span', { class: cls, title: KV_SOURCE_NOTE[r.ttl_source] || '' }, label);
}

/** kvHitPill is the provider's own verdict on this request, with its reason. */
function kvHitPill(r) {
  return el('span', { class: 'pill ' + (r.miss_reason || 'unknown'), title: r.miss_reason },
    r.hit ? 'hit' : r.miss_reason || 'miss');
}

/** A number cell, and a compacted one for the token counts that run to hundreds of thousands. */
function kvNum(v) { return el('td', { class: 'num', text: num(v) }); }
function kvTok(v) {
  return el('td', { class: 'num', title: num(v) + ' tokens', text: compact(v) });
}

/**
 * kvParams is the whole page state as query parameters.
 *
 * Everything the simulation depends on is in here, which is what makes a result a URL. Note
 * the string coercions: qs() drops any extra whose value is '', 0 or undefined, so a boolean
 * that must be sent as false travels as the STRING '0' and a numeric zero is simply never
 * sent (the server then applies its own default, which is what an absent parameter means).
 */
function kvParams(extra) {
  const p = {
    // `tenant` is the shared scoping parameter, and the server decides what a caller may do
    // with it: a manager may narrow to one account, and anyone else's is ignored. So this is
    // the user filter, with no second mechanism to keep in step.
    tenant: kvc.user || undefined,
    bucket: kvc.bucket || undefined,
    ttl: kvc.ttl || undefined,
    has_next: kvc.hasNext || undefined,
    mult_read: kvc.mult.read || undefined,
    mult_w5m: kvc.mult.w5m || undefined,
    mult_w1h: kvc.mult.w1h || undefined,
    ping_in: kvc.ping.in || undefined,
    ping_out: kvc.ping.out || undefined,
    x: kvc.sched.x || undefined,
    x1h: kvc.sched.x1h || undefined,
    k: kvc.sched.k || undefined,
    p5m: kvc.custom.p5m || undefined,
    p1h: kvc.custom.p1h || undefined,
    min_prefix: kvc.custom.min || undefined,
    always_ping: kvc.custom.always ? '1' : undefined,
    hit_refresh: kvc.sem.hit ? undefined : '0',
    ping_refresh: kvc.sem.ping ? undefined : '0',
    zero_gen: kvc.sem.zero ? '1' : undefined,
  };
  Object.assign(p, extra || {});
  return p;
}

/** kvRateParams is the edited rates, one `rate=` per model, as the server parses them. */
function kvRateSpecs() {
  return Object.keys(kvc.rates).sort().map((model) => {
    const r = kvc.rates[model] || {};
    return [model, r.in || '', r.out || '', r.read || '', r.w5m || '', r.w1h || ''].join(':');
  });
}

/**
 * kvFetch is api() plus the repeated `rate` parameter, which URLSearchParams can carry more
 * than once and qs()'s object form cannot.
 */
async function kvFetch(path, extra) {
  const specs = kvRateSpecs();
  let url = path + '';
  const q = qs(kvParams(extra));
  const sep = q ? '&' : '?';
  const tail = specs.map((s) => 'rate=' + encodeURIComponent(s)).join('&');
  const res = await fetch('/api/' + url + q + (tail ? sep + tail : ''), {
    headers: { accept: 'application/json' },
    signal: state.ac ? state.ac.signal : undefined,
  });
  if (!res.ok) {
    let msg = res.status + ' ' + res.statusText;
    try { const j = await res.json(); if (j.error) msg = j.error; } catch (_) { /* not json */ }
    const e = new Error(msg); e.status = res.status; throw e;
  }
  return res.json();
}

// ── the page skeleton, built once ──────────────────────────────────────────
//
// Stable hosts, created at mount and only ever cleared. Rebuilding the whole view on every
// load would throw away the pricing inputs mid-edit and move focus out from under whoever was
// typing in them.

/** kvPanel is one titled panel with a note and a set of hosts. */
function kvPanel(title, note, ...kids) {
  const p = el('div', { class: 'panel' }, el('h2', {}, title));
  if (note) p.appendChild(el('p', { class: 'note' }, note));
  for (const k of kids) if (k) p.appendChild(k);
  kvView.appendChild(p);
  return p;
}

/** kvHost is an empty div with an id and a test id. */
function kvHost(id) { return el('div', { id: id, 'data-testid': id }); }

// 1. What is being measured, and how well. The coverage statement comes FIRST and renders
//    unconditionally: a reader has to know whether a blank tier means "not cached" or "not
//    recorded" before any figure below is worth reading.
kvPanel('What happens after a request',
  'For every historical request, how long until the same conversation came back — and what '
  + 'that means for a cached prompt with a five-minute or a one-hour lifetime. Model, '
  + 'conversation, agent and the time range come from the filter bar at the top of the page; '
  + 'the four filters here are the ones it does not carry.',
  kvHost('kv-coverage'), kvHost('kv-controls'), kvHost('kv-tiles'));

// 2. The distribution itself: the histogram, then the same data as "has it come back yet".
const kvDistPanel = kvPanel('How long until the next request',
  'The idle gap between one request and the next in the same conversation. Requests with no '
  + 'next request are not in these charts — they have no idle time.');
kvDistPanel.appendChild(el('h3', {}, 'Idle time, by band'));
kvDistPanel.appendChild(el('p', { class: 'note', id: 'kv-idle-note' }));
kvDistPanel.appendChild(kvHost('kv-idle-bands'));
kvDistPanel.appendChild(el('h3', {}, 'Has it come back yet?'));
kvDistPanel.appendChild(el('p', { class: 'note' },
  'The share of requests whose conversation had returned by each elapsed time. The two rungs '
  + 'that matter are on the curve rather than between them: five minutes is the provider’s '
  + 'default lifetime, an hour is the long tier.'));
kvDistPanel.appendChild(el('div', { class: 'chart', id: 'kv-survival', 'data-testid': 'kv-survival' }));

// 3. Grouped views. By tier first, because that is the dimension the whole page is about.
const kvGroupPanel = kvPanel('Who waits how long',
  'The same measurement grouped four ways. A group’s reuse percentages are over ITS OWN '
  + 'requests that have a successor, so a group of final requests shows no percentage rather '
  + 'than a zero.');
kvGroupPanel.appendChild(el('h3', {}, 'By observed TTL'));
kvGroupPanel.appendChild(kvHost('kv-by-ttl'));
kvGroupPanel.appendChild(el('h3', {}, 'By user'));
kvGroupPanel.appendChild(kvHost('kv-by-user'));
kvGroupPanel.appendChild(el('h3', {}, 'By model'));
kvGroupPanel.appendChild(kvHost('kv-by-model'));
kvGroupPanel.appendChild(el('h3', {}, 'By time of day (UTC)'));
kvGroupPanel.appendChild(kvHost('kv-by-bucket'));

// 4. The prices. Before the simulator, deliberately: every figure below is these rates times a
//    token count, and a reader who has not seen them cannot judge the result.
kvPanel('Cache prices',
  'The rates every figure below is computed from. They come from this deployment’s own price '
  + 'list; edit any of them to see what a different rate would do. Rates are USD per million '
  + 'tokens, which is the unit every vendor’s price page uses.',
  kvHost('kv-pricing'));

// 5. The simulator.
kvPanel('What a different TTL strategy would have cost',
  'Each strategy is replayed against exactly this history. At every request it is given only '
  + 'what was knowable at that moment — never how long the gap turned out to be — and the real '
  + 'next-request time is used only to score the decision afterwards.',
  kvHost('kv-arms'), kvHost('kv-sim'));

// 6. The dataset itself.
const kvTablePanel = kvPanel('Every request in the analysis',
  'The derived dataset, one row per request, sorted and paged on the server so a sorted '
  + 'column shows the real top rows rather than the top of one page.',
  kvHost('kv-table'), kvHost('kv-pager'));

// 7. The assumptions, last and always present.
kvPanel('Formulas and assumptions',
  'Every expression below is the one the server actually evaluated — they are sent with the '
  + 'data rather than written into this page, so they cannot drift from the code.',
  kvHost('kv-assumptions'));

// ── controls ───────────────────────────────────────────────────────────────

/** kvSelect builds one labelled select and wires it to a state key. */
function kvSelect(label, value, options, onChange, testid) {
  const sel = el('select', { 'data-testid': testid, onchange: (ev) => onChange(ev.target.value) });
  for (const [v, text] of options) {
    const opt = el('option', { value: v }, text);
    if (v === value) opt.setAttribute('selected', 'selected');
    sel.appendChild(opt);
  }
  return el('label', {}, label, sel);
}

/** renderKVControls draws the four page-local filters. */
function renderKVControls() {
  const host = clear($('#kv-controls'));
  const a = kvc.analysis;
  const users = [['', 'All users']];
  if (a) for (const g of a.by_user) users.push([g.key, (g.key || '(single-tenant)') + ' · ' + num(g.requests)]);
  const row = el('div', { class: 'kv-controls' },
    // The user filter is only offered where the caller can actually use it: the server
    // ignores a narrowing from anyone but a manager, so drawing it for everybody else would
    // be a control whose only possible outcome is no change.
    wideScope() ? kvSelect('User', kvc.user, users, (v) => { kvc.user = v; kvReload(); },
      'kv-filter-user') : null,
    kvSelect('Time of day (UTC)', kvc.bucket, [['', 'All'], ['night', 'Night 00–06'],
      ['morning', 'Morning 06–12'], ['afternoon', 'Afternoon 12–18'], ['evening', 'Evening 18–24']],
    (v) => { kvc.bucket = v; kvReload(); }, 'kv-filter-bucket'),
    kvSelect('Observed TTL', kvc.ttl, [['', 'All'], ['ephemeral_5m', '5 minutes'],
      ['ephemeral_1h', '1 hour'], ['none', 'Not cached'],
      // The fourth group is reachable by clicking its row in the by-TTL table, so it needs an
      // option here too: without one kvSelect matches nothing and the browser falls back to
      // showing the first, so the page read "All" while filtered to the not-recorded rows.
      ['unrecorded', 'Not recorded']],
    (v) => { kvc.ttl = v; kvReload(); }, 'kv-filter-ttl'),
    kvSelect('Next request', kvc.hasNext, [['', 'All'], ['yes', 'Has one'], ['no', 'None (final)']],
      (v) => { kvc.hasNext = v; kvReload(); }, 'kv-filter-hasnext'));
  host.appendChild(row);
}

/**
 * renderKVCoverage is rule 3: whether a zero on this page is a measurement or an absence.
 *
 * Unconditional. It renders on a fully-instrumented window too, because a reader deciding
 * whether to act on these figures has to know how much of the window recorded its own tier —
 * and on a database with any history at all, some of it did not.
 */
function renderKVCoverage() {
  const host = clear($('#kv-coverage'));
  const a = kvc.analysis;
  if (!a) return;
  const c = a.coverage;
  if (!a.total) {
    emptyState(host, 'No requests match these filters',
      'Widen the time range at the top of the page, or clear a filter.');
    return;
  }
  if (a.truncated) {
    host.appendChild(el('div', { class: 'banner warn', 'data-testid': 'kv-truncated' },
      el('strong', {}, 'This analysis read ' + num(a.scanned) + ' of ' + num(a.total)
        + ' matching requests. '),
      'The newest ones were kept. Every figure on this page is over the '
      + num(a.scanned) + ' that were read, not over the whole window — narrow the time range '
      + 'to analyse all of it.'));
  }
  if (c.ttl_unknown > 0) {
    host.appendChild(el('div', { class: 'banner', 'data-testid': 'kv-ttl-coverage' },
      el('strong', {}, num(c.ttl_unknown) + ' of ' + num(a.scanned)
        + ' requests did not record which cache lifetime they asked for. '),
      'Those rows predate that column, so a blank there means NOT RECORDED, not "no cache". '
      + 'They are taken as the provider default of five minutes so the simulation has '
      + 'something to replay, and the observed-policy strategy below reports how much of '
      + 'itself rested on that assumption. ' + num(c.ttl_configured) + ' recorded their own '
      + 'tier and ' + num(c.ttl_observed) + ' could be deduced from what the provider billed.'));
  }
  if (c.cost_unknown > 0) {
    host.appendChild(el('div', { class: 'banner warn', 'data-testid': 'kv-cost-coverage' },
      el('strong', {}, num(c.cost_unknown) + ' requests have an unknown cost. '),
      'Their token accounting was incomplete, so they are counted everywhere and valued '
      + 'nowhere. An unpriced request is not a free one.'));
  }
  if (c.single_request_conversations > 0) {
    host.appendChild(el('p', { class: 'hint', 'data-testid': 'kv-single-note' },
      num(c.single_request_conversations) + ' of ' + num(a.cards.conversations)
      + ' conversations have a single request in this window, so they contribute a final '
      + 'request and no idle gap at all.'));
  }
}

/** renderKVTiles is the summary band. */
function renderKVTiles() {
  const host = clear($('#kv-tiles'));
  const a = kvc.analysis;
  if (!a || !a.total) return;
  const c = a.cards;
  const costKnown = c.cost_unknown < c.requests;
  host.appendChild(tileGroup(null, null, [
    tile('kv-requests', 'Requests', num(c.requests),
      num(c.scanned || a.scanned) + ' read of ' + num(a.total) + ' matched'),
    tile('kv-conversations', 'Conversations', num(c.conversations),
      num(c.users) + ' user' + (c.users === 1 ? '' : 's') + ' · ' + num(c.models) + ' models'),
    tile('kv-median-idle', 'Median idle', c.with_next ? dur(c.median_idle_ms) : '—',
      c.with_next ? 'over ' + num(c.with_next) + ' gaps' : 'no gaps in this window'),
    tile('kv-mean-idle', 'Mean idle', c.with_next ? dur(c.mean_idle_ms) : '—',
      c.with_next ? 'p90 ' + dur(c.p90_idle_ms) : 'no gaps'),
    tile('kv-reuse-5m', 'Back within 5m', c.with_next ? pct(c.within_5m_pct) : '—',
      c.with_next ? num(c.within_5m) + ' of ' + num(c.with_next) : 'no gaps', 'accent'),
    tile('kv-reuse-1h', 'Back within 1h', c.with_next ? pct(c.within_1h_pct) : '—',
      c.with_next ? num(c.within_1h) + ' of ' + num(c.with_next) : 'no gaps', 'accent'),
    tile('kv-hit-rate', 'Cache-hit rate', pct(c.hit_rate_pct),
      num(c.hits) + ' of ' + num(c.requests) + ' as the provider reported it'),
    tile('kv-cost', 'Cost as billed', costKnown ? usd(c.cost_usd) : 'unknown',
      costKnown ? num(c.requests - c.cost_unknown) + ' priced requests' : 'nothing priced'),
    tile('kv-final', 'No next request', num(c.final_requests),
      'excluded from every average above'),
    tile('kv-prefix', 'Median cached prompt', compact(c.cached_context_p50) + ' tok',
      'what every cost below is proportional to'),
  ]));
}

/** renderKVIdleBands draws the histogram. */
function renderKVIdleBands() {
  const host = clear($('#kv-idle-bands'));
  const a = kvc.analysis;
  if (!a) return;
  const bands = a.idle_bands || [];
  const total = bands.reduce((s, b) => s + b.n, 0);
  $('#kv-idle-note').textContent = total
    ? 'Bands beyond five minutes are drawn in gray: a default cached prefix is already gone '
      + 'by then. ' + num(total) + ' gaps.'
    : '';
  barRows(host, bands.map((b) => ({
    label: b.label,
    value: b.n,
    display: num(b.n) + (total ? '  ' + pct(100 * b.n / total, 1) : ''),
    color: b.beyond ? 'var(--s-mute)' : undefined,
    title: b.n + ' gaps in this band' + (b.usd ? ', ' + usd(b.usd) + ' of billed cost' : ''),
  })), { emptyDetail: 'No request in this window is followed by another in the same conversation.' });
}

/** renderKVSurvival draws the "has it come back yet" curve. */
function renderKVSurvival() {
  const host = $('#kv-survival');
  const a = kvc.analysis;
  if (!a) return;
  const ladder = a.survival || [];
  if (!ladder.length) {
    emptyState(clear(host), 'No idle gaps in this window',
      'Every request here is the last of its conversation.');
    return;
  }
  // Plotted against the RUNG INDEX, not against the seconds.
  //
  // The ladder spans 5 s to a day, and on real traffic the entire shape is inside the first
  // five minutes — so a linear seconds axis draws a vertical line at the left edge and a flat
  // line across the rest, which is a chart that hides exactly what it exists to show. The rungs
  // are chosen because they are the decision points, so spacing them evenly is the honest
  // reading: this is a ladder, not a timeline. The labels carry the real elapsed times.
  const pts = ladder.map((p, i) => [i, p.arrived_pct]);
  lineChart(host, [{ name: 'Returned by', color: SERIES[0], points: pts, area: true }], {
    yFmt: (v) => v.toFixed(0) + '%',
    // ROUNDED, because lineChart labels the midpoint of the x range and that lands between two
    // rungs on an even-length ladder. Without this the axis printed the raw index's neighbour
    // as a bare number — "43202.5" — which is not an elapsed time anybody can read.
    xFmt: (v) => (ladder[Math.round(v)] || {}).label || '',
    tipFmt: (v) => v.toFixed(1) + '%',
    yMax: 100,
    label: 'share of conversations that had returned by each elapsed time',
  });
}

/**
 * kvGroupTable draws one grouped view.
 *
 * `onPick` makes a row a drill-down where one exists; a group with no successor in it shows a
 * dash rather than 0% for its reuse columns, which is rule 2 applied to a group.
 */
function kvGroupTable(hostSel, groups, keyLabel, onPick, fmtKey) {
  const host = clear($(hostSel));
  if (!groups || !groups.length) {
    emptyState(host, 'Nothing to group', 'No requests match these filters.');
    return;
  }
  const table = el('table', { class: 'tbl compact', 'data-testid': hostSel.slice(1) + '-table' },
    el('thead', {}, el('tr', {},
      el('th', {}, keyLabel),
      el('th', { class: 'num' }, 'Requests'),
      el('th', { class: 'num' }, 'Conversations'),
      el('th', { class: 'num' }, 'With a next'),
      el('th', { class: 'num' }, 'Final'),
      el('th', { class: 'num' }, 'Median idle'),
      el('th', { class: 'num' }, 'Mean idle'),
      el('th', { class: 'num' }, 'Back in 5m'),
      el('th', { class: 'num' }, 'Back in 1h'),
      el('th', { class: 'num' }, 'Hit rate'),
      el('th', { class: 'num' }, 'Cost'))));
  const body = el('tbody');
  table.appendChild(body);
  for (const g of groups) {
    const row = el('tr', onPick ? { class: 'click', onclick: () => onPick(g) } : {},
      el('td', {}, el('span', { class: 'trunc', title: g.key },
        (fmtKey ? fmtKey(g) : g.key) || '—')),
      kvNum(g.requests), kvNum(g.conversations), kvNum(g.with_next), kvNum(g.final_requests),
      el('td', { class: 'num' }, g.with_next ? dur(g.median_idle_ms) : '—'),
      el('td', { class: 'num' }, g.with_next ? dur(g.mean_idle_ms) : '—'),
      el('td', { class: 'num' }, g.with_next ? pct(g.within_5m_pct) : '—'),
      el('td', { class: 'num' }, g.with_next ? pct(g.within_1h_pct) : '—'),
      el('td', { class: 'num' }, pct(g.hit_rate_pct)),
      el('td', { class: 'num' }, kvMoney(g.cost_usd, g.cost_unknown < g.requests)));
    body.appendChild(row);
  }
  host.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, table));
}

/** renderKVGroups draws all four grouped views. */
function renderKVGroups() {
  const a = kvc.analysis;
  if (!a) return;
  // The group key IS the filter value, including for the not-recorded group, so clicking a row
  // narrows to exactly the rows behind it rather than to something that looks similar.
  kvGroupTable('#kv-by-ttl', a.by_ttl, 'Observed TTL',
    (g) => { kvc.ttl = g.key; kvReload(); }, (g) => KV_TTL_LABEL[g.key] || g.key);
  kvGroupTable('#kv-by-user', a.by_user, 'User',
    wideScope() ? (g) => { kvc.user = g.key; kvReload(); } : null);
  kvGroupTable('#kv-by-model', a.by_model, 'Model', (g) => setFilter('model', g.key));
  kvGroupTable('#kv-by-bucket', a.by_bucket, 'Time of day (UTC)',
    (g) => { kvc.bucket = g.key; kvReload(); });
}

// ── pricing ────────────────────────────────────────────────────────────────

/** The five editable rates, in table order: state key, label, price-list field. */
const KV_RATE_FIELDS = [
  ['in', 'Input', 'input'],
  ['out', 'Output', 'output'],
  ['read', 'Cache read', 'cache_read'],
  ['w5m', 'Write 5m', 'write_5m'],
  ['w1h', 'Write 1h', 'write_1h'],
];

/** perMTok converts a per-token rate to the per-million unit the inputs are in. */
function perMTok(v) { return v === null || v === undefined ? '' : (v * 1e6).toFixed(4); }

/**
 * renderKVPricing draws the editable rate table and what those rates come to.
 *
 * The two halves are deliberate: the RATE is what a reader can change, the COST is what they
 * are deciding about, and a per-token rate on its own is not a number anybody can act on.
 */
function renderKVPricing() {
  const host = clear($('#kv-pricing'));
  const v = kvc.prices;
  if (!v) { loadingState(host, 2); return; }
  // No prefix means no size to apply a rate to, so every cost below would be $0.00 — which is
  // not a price. The server states this in one field rather than leaving the page to infer it
  // from a zero, which is the inference a reader of a table of dollar signs will not make.
  if (v.prefix_known === false) {
    host.appendChild(el('div', { class: 'banner warn', 'data-testid': 'kv-no-prefix' },
      el('strong', {}, 'No request in this window cached anything. '),
      'There is no cached prompt to price, so the cost columns below are omitted rather than '
      + 'shown as $0.00. The rates themselves are still the configured ones and are still '
      + 'editable.'));
  }
  const list = v.pricing || { models: [] };
  if (!list.models.length) {
    emptyState(host, 'No models in this window', 'Nothing to price.');
    return;
  }
  host.appendChild(el('p', { class: 'hint' },
    'Costs are on this window’s own median cached prompt of ',
    el('strong', {}, compact(v.prefix_tokens) + ' tokens'),
    '. “Hold” is a write plus ' + v.assumptions.schedule.max_pings
    + ' keep-alive refreshes — the whole cost of carrying that prompt through one idle span.'));

  const table = el('table', { class: 'tbl compact', 'data-testid': 'kv-pricing-table' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Model'),
      ...KV_RATE_FIELDS.map(([, label]) => el('th', { class: 'num' }, label + ' $/MTok')),
      el('th', { class: 'num' }, 'Uncached'),
      el('th', { class: 'num' }, 'One read'),
      el('th', { class: 'num' }, 'Write 5m'),
      el('th', { class: 'num' }, 'Write 1h'),
      el('th', { class: 'num' }, 'One ping'),
      el('th', { class: 'num' }, 'Late ping'),
      el('th', { class: 'num' }, 'Hold 5m'),
      el('th', { class: 'num' }, 'Hold 1h'),
      el('th', {}, 'Source'))));
  const body = el('tbody');
  table.appendChild(body);
  const costs = {};
  for (const c of v.costs || []) costs[c.model] = c;
  for (const m of list.models) {
    const c = costs[m.model] || {};
    const edited = kvc.rates[m.model] || {};
    body.appendChild(el('tr', {},
      el('td', {}, el('span', { class: 'trunc', title: m.model }, m.model || '—')),
      ...KV_RATE_FIELDS.map(([key, label, field]) => el('td', { class: 'num' },
        el('input', {
          type: 'number', step: 'any', min: '0', class: 'kv-rate' + (edited[key] ? ' kv-edited' : ''),
          'data-testid': 'kv-rate-' + key,
          'aria-label': label + ' rate for ' + m.model + ', USD per million tokens',
          value: edited[key] !== undefined && edited[key] !== '' ? edited[key] : perMTok(m[field]),
          onchange: (ev) => kvEditRate(m.model, key, ev.target.value, perMTok(m[field])),
        }))),
      el('td', { class: 'num' }, kvMoney(c.uncached_usd, m.known)),
      el('td', { class: 'num' }, kvMoney(c.read_usd, m.known)),
      el('td', { class: 'num' }, kvMoney(c.write_5m_usd, m.known)),
      el('td', { class: 'num' }, kvMoney(c.write_1h_usd, m.known)),
      el('td', { class: 'num' }, kvMoney(c.keep_alive_usd, m.known)),
      el('td', { class: 'num', title: 'A keep-alive that arrived after the entry lapsed: it '
        + 're-creates the prefix, so it is billed as a write.' },
      kvMoney(c.late_5m_usd, m.known)),
      el('td', { class: 'num' }, kvMoney(c.hold_5m_usd, m.known)),
      el('td', { class: 'num' }, kvMoney(c.hold_1h_usd, m.known)),
      el('td', {}, el('span', {
        class: m.source === 'override' ? 'pill neutral' : m.known ? 'pill' : 'pill missing',
        title: m.source === 'override' ? 'You typed this.'
          : m.known ? 'From this deployment’s configured price list.'
            : 'This model has no rates on the price list. Type one to price it.',
      }, m.source || 'unpriced'))));
  }
  host.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, table));

  // The three multipliers and the ping overhead: deployment-wide facts rather than per-model
  // rates, so they get their own row of controls.
  const mult = v.assumptions.multipliers || {};
  const sched = v.assumptions.schedule || {};
  host.appendChild(el('div', { class: 'kv-controls', 'data-testid': 'kv-price-knobs' },
    kvNumberInput('Cache-read × input', kvc.mult.read, mult.cache_read,
      (val) => { kvc.mult.read = val; kvReload(); }, 'kv-mult-read'),
    kvNumberInput('5m write × input', kvc.mult.w5m, mult.write_5m,
      (val) => { kvc.mult.w5m = val; kvReload(); }, 'kv-mult-w5m'),
    kvNumberInput('1h write × input', kvc.mult.w1h, mult.write_1h,
      (val) => { kvc.mult.w1h = val; kvReload(); }, 'kv-mult-w1h'),
    kvNumberInput('Ping input tokens', kvc.ping.in, 1,
      (val) => { kvc.ping.in = val; kvReload(); }, 'kv-ping-in'),
    kvNumberInput('Ping output tokens', kvc.ping.out, 1,
      (val) => { kvc.ping.out = val; kvReload(); }, 'kv-ping-out'),
    kvNumberInput('Ping every (s)', kvc.sched.x, sched.idle_seconds,
      (val) => { kvc.sched.x = val; kvReload(); }, 'kv-sched-x'),
    kvNumberInput('1h ping every (s)', kvc.sched.x1h, sched.idle_seconds_1h,
      (val) => { kvc.sched.x1h = val; kvReload(); }, 'kv-sched-x1h'),
    kvNumberInput('Max pings per span', kvc.sched.k, sched.max_pings,
      (val) => { kvc.sched.k = val; kvReload(); }, 'kv-sched-k')));
  host.appendChild(el('p', { class: 'hint' },
    'A one-hour write rate is the one number no gateway publishes, so it is derived from the '
    + 'multiplier above unless you type a rate for it. A keep-alive costs the same per ping at '
    + 'either tier — it is a cache read either way — so what separates a five-minute hold from '
    + 'a one-hour one is the write that put the entry there and how often it has to be '
    + 'refreshed.'));

  // The provider semantics, which change every result and are therefore switches rather than
  // assumptions.
  const sem = v.assumptions.semantics || {};
  host.appendChild(el('div', { class: 'kv-controls', 'data-testid': 'kv-semantics' },
    kvCheck('A cache hit refreshes the lifetime', sem.hit_refreshes_ttl,
      (on) => { kvc.sem.hit = on; kvReload(); }, 'kv-sem-hit',
      'Anthropic documents this: using a cached prefix refreshes it for no extra cost. Turn it '
      + 'off for a provider where a hit does not extend the deadline.'),
    kvCheck('A keep-alive refreshes the lifetime', sem.ping_refreshes_ttl,
      (on) => { kvc.sem.ping = on; kvReload(); }, 'kv-sem-ping',
      'A keep-alive is just a use of the cache, so it refreshes the entry for its own tier’s '
      + 'lifetime.'),
    kvCheck('Zero-generation pings allowed', sem.zero_generation,
      (on) => { kvc.sem.zero = on; kvReload(); }, 'kv-sem-zero',
      'Anthropic’s Messages API requires max_tokens ≥ 1, so a ping cannot generate nothing and '
      + 'its one output token is priced. Turn this on for a provider that accepts a '
      + 'zero-generation request.')));
}

/** kvEditRate records one typed rate, or clears it back to the configured one. */
function kvEditRate(model, key, value, configured) {
  const v = String(value === undefined || value === null ? '' : value).trim();
  const row = kvc.rates[model] || {};
  // Typing the configured value back is the way OUT of an override, so an accidental edit can
  // be undone without a reset button.
  if (v === '' || v === configured) delete row[key];
  else row[key] = v;
  if (Object.keys(row).length) kvc.rates[model] = row;
  else delete kvc.rates[model];
  kvReload();
}

/** kvNumberInput is one labelled numeric control with the server's value as its placeholder. */
function kvNumberInput(label, value, fallback, onChange, testid) {
  return el('label', {}, label,
    el('input', {
      type: 'number', step: 'any', min: '0', 'data-testid': testid,
      value: value || '',
      placeholder: fallback === undefined || fallback === null ? '' : String(fallback),
      onchange: (ev) => onChange(ev.target.value.trim()),
    }));
}

/** kvCheck is one labelled checkbox. */
function kvCheck(label, on, onChange, testid, title) {
  const box = el('input', { type: 'checkbox', 'data-testid': testid,
    onchange: (ev) => onChange(ev.target.checked) });
  if (on) box.setAttribute('checked', 'checked');
  return el('label', { class: 'small', title: title || '' }, box, ' ' + label);
}

// ── the simulator ──────────────────────────────────────────────────────────

/** renderKVArms draws the strategy picker and the baseline selector. */
function renderKVArms() {
  const host = clear($('#kv-arms'));
  const sim = kvc.sim;
  const specs = kvArms();
  if (!specs.length) { loadingState(host, 2); return; }
  const described = {};
  if (sim) for (const r of sim.results) described[r.strategy] = r.description;
  // What is ticked: the caller's own selection, or — on first load — exactly what the server
  // ran, so the picker and the table below it never disagree about which arms are on screen.
  const on = new Set(kvc.arms || (sim ? sim.results.map((r) => r.strategy) : []));
  host.appendChild(el('div', { class: 'kv-arms', 'data-testid': 'kv-arm-picker' },
    ...specs.map((a) => {
      const box = el('input', {
        type: 'checkbox', 'data-testid': 'kv-arm-' + a.name,
        disabled: a.selectable ? null : 'disabled',
        onchange: (ev) => {
          const set = new Set(kvc.arms || on);
          if (ev.target.checked) set.add(a.name); else set.delete(a.name);
          kvc.arms = kvArmNames().filter((n) => set.has(n));
          kvLoadSim();
        },
      });
      if (on.has(a.name)) box.setAttribute('checked', 'checked');
      return el('div', { class: 'kv-arm' + (a.unreachable ? ' kv-arm-ceiling' : ''), }, box,
        el('div', {},
          el('div', { class: 'kv-arm-name' }, a.name,
            a.unreachable ? kvCeilingPill() : null,
            a.selectable ? null : el('span', {
              class: 'pill missing',
              title: 'This arm is scored from an action list supplied in the process rather '
                + 'than from a query, so it cannot be selected here. It exists so an offline '
                + 'model\u2019s answers can be scored against the same baseline.',
            }, 'not selectable')),
          el('div', { class: 'kv-arm-desc', text: described[a.name] || a.description || '' })));
    })));
  host.appendChild(el('div', { class: 'kv-controls' },
    kvSelect('Baseline', (sim && sim.baseline) || kvc.baseline,
      kvBaselineArms().map((a) => [a.name, a.name]),
      (v) => { kvc.baseline = v; kvLoadSim(); }, 'kv-baseline'),
    kvNumberInput('P(back in 5m) \u2265', kvc.custom.p5m, 0.5,
      (v) => { kvc.custom.p5m = v; kvLoadSim(); }, 'kv-custom-p5m'),
    kvNumberInput('P(back in 1h) \u2265', kvc.custom.p1h, 0.5,
      (v) => { kvc.custom.p1h = v; kvLoadSim(); }, 'kv-custom-p1h'),
    kvNumberInput('Never cache under (tokens)', kvc.custom.min, 20000,
      (v) => { kvc.custom.min = v; kvLoadSim(); }, 'kv-custom-min'),
    kvCheck('Custom arm always pings', kvc.custom.always,
      (on2) => { kvc.custom.always = on2; kvLoadSim(); }, 'kv-custom-always',
      'Force the long hold onto the keep-alive path instead of taking whichever of a 1h write '
      + 'and a 5m write plus refreshes is cheaper on that prefix.')));
  host.appendChild(el('p', { class: 'hint' },
    'The three thresholds configure the ', el('strong', {}, 'custom'), ' arm. A learned '
    + 'predictor plugs into the same arm server-side without changing anything on this page.'));
}

/**
 * kvCeilingPill marks an arm that reads the future.
 *
 * It is a bound on what any policy could achieve, never a result — so it gets its own word, its
 * own colour, and it is not offered as a baseline. A ceiling rendered beside real arms in the
 * same style is a promise the product cannot keep.
 */
function kvCeilingPill() {
  return el('span', {
    class: 'pill missing', 'data-testid': 'kv-ceiling-pill',
    title: 'Unreachable. This arm is told the real next-request time, so it is the cheapest plan '
      + 'that exists for this history \u2014 the headroom, not an option. No policy can reach it, '
      + 'because no policy knows the future.',
  }, 'ceiling');
}

/** renderKVSim draws the comparison table and the per-user / per-model breakdowns. */
function renderKVSim() {
  const host = clear($('#kv-sim'));
  const sim = kvc.sim;
  if (!sim) { loadingState(host, 3); return; }
  if (sim.unknown && sim.unknown.length) {
    host.appendChild(el('div', { class: 'banner warn' },
      el('strong', {}, 'Not a strategy: ' + sim.unknown.join(', ') + '. '),
      'It was skipped rather than replaced with something else.'));
  }
  if (!sim.results.length) {
    emptyState(host, 'No strategy selected', 'Tick at least one arm above.');
    return;
  }
  // A window where NOTHING could be priced makes every total zero and the ordering between arms
  // meaningless — including the ceiling's, which then wears the styling of "the cheapest plan
  // that exists" over a plan that is simply unpriced. The server states that in one field per
  // arm rather than leaving it to be inferred from Unpriced === Requests, which is exactly the
  // inference a consumer forgets to make.
  if (sim.results.every((r) => r.valued === false)) {
    host.appendChild(el('div', { class: 'banner warn', 'data-testid': 'kv-unvalued' },
      el('strong', {}, 'None of these requests could be priced. '),
      'Every model in this window is missing from the price list, so all of the totals below are '
      + 'zero for want of a rate — not because nothing was spent, and not because one arm is '
      + 'cheaper than another. Every strategy that decides by comparing costs, the ceiling '
      + 'included, had nothing to decide with, so its actions and its hit rate are an artefact '
      + 'rather than a policy. Add the models to the price list above, or to MODEL_PRICES, and '
      + 'the comparison becomes meaningful.'));
  }
  const savings = {};
  for (const s of sim.savings) savings[s.strategy] = s;

  const table = el('table', { class: 'tbl', 'data-testid': 'kv-sim-table' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Strategy'),
      el('th', { class: 'num' }, 'Total cost'),
      el('th', { class: 'num' }, 'vs baseline'),
      el('th', { class: 'num' }, '% vs baseline'),
      el('th', { class: 'num' }, 'Hit rate'),
      el('th', { class: 'num' }, 'Miss rate'),
      el('th', { class: 'num' }, '5m writes'),
      el('th', { class: 'num' }, '1h writes'),
      el('th', { class: 'num' }, 'Pings'),
      el('th', { class: 'num' }, 'Ping cost'),
      el('th', { class: 'num' }, 'Recomputes avoided'),
      el('th', { class: 'num' }, 'Cache held'),
      el('th', { class: 'num' }, 'Latency avoided'))));
  const body = el('tbody');
  table.appendChild(body);
  const ceiling = {};
  for (const a of kvArms()) ceiling[a.name] = a.unreachable;
  for (const r of sim.results) {
    const s = savings[r.strategy] || {};
    const isBase = r.strategy === sim.baseline;
    const isCeiling = !!ceiling[r.strategy];
    // The server's own field, not the same test spelled a second time: `valued` IS
    // unpriced !== requests, and two spellings of one predicate is how they come to
    // disagree the day one of them gains a caveat.
    const priced = r.valued;
    body.appendChild(el('tr', {
      class: (isBase ? 'kv-baseline' : '') + (isCeiling ? ' kv-ceiling' : ''),
      'data-testid': 'kv-sim-row-' + r.strategy },
    // The name only, with the description on hover. The description is already on the picker
    // above, and repeating it here made this column wide enough to push Total cost and
    // vs baseline — the two figures the page exists to show — off the right edge at a normal
    // viewport width, behind a horizontal scroll nobody would know to use.
    el('td', { title: r.description || '' },
      el('div', { class: 'kv-sim-name' }, r.strategy, isCeiling ? kvCeilingPill() : null)),
    el('td', { class: 'num' }, kvMoney(r.total_usd, priced)),
    el('td', { class: 'num' }, isBase ? document.createTextNode('—')
      // Gated on `priced` ALONE. percent_known is false only when the baseline total is zero,
      // and the absolute difference is perfectly well defined then — it is simply −strategy_usd.
      // Gating this cell on it would print "unpriced" over a figure that is known, which is an
      // absence shown where there is a measurement: the inverse of the rule this page keeps
      // everywhere else.
      : kvSigned(s.absolute_usd, priced)),
    el('td', { class: 'num' }, isBase ? document.createTextNode('—')
      : s.percent_known ? el('span', {
        class: s.percent_usd < 0 ? 'bad-text' : 'good-text',
        // The sign is kept here, unlike the column beside it, and it needs saying which way it
        // points: this is a percentage OF THE BASELINE'S COST, so a negative one is an arm that
        // costs more, not an arm that saved a negative amount.
        title: s.percent_usd < 0 ? 'Negative: this arm costs this much MORE than the baseline.'
          : 'Positive: this arm costs this much less than the baseline.',
      }, pct(s.percent_usd, 2))
        : el('span', { class: 'pill missing', title: 'The baseline cost nothing, so a '
          + 'percentage of it is undefined rather than 0%.' }, 'undefined')),
    el('td', { class: 'num' }, pct(r.hit_rate_pct)),
    el('td', { class: 'num' }, pct(r.miss_rate_pct)),
    kvNum(r.writes_5m), kvNum(r.writes_1h),
    el('td', { class: 'num', title: kvPingNote(r) },
      r.pings ? num(r.pings) + (r.pings_that_rewrote ? ' \u26a0' : '') : '—'),
    el('td', { class: 'num' }, r.pings ? kvMoney(r.ping_usd, priced) : '—'),
    el('td', { class: 'num', title: compact(r.avoided_tokens) + ' prefix tokens served from '
      + 'cache instead of re-created' }, num(r.avoided_recomputations)),
    el('td', { class: 'num' }, r.retained_ms ? dur(r.retained_ms) : '—'),
    el('td', { class: 'num' }, s.latency_known
      ? dur(Math.abs(s.latency_avoided_ms)) + (s.latency_avoided_ms < 0 ? ' worse' : '')
      : el('span', { class: 'pill missing', title: 'Needs at least 20 real hits and 20 real '
        + 'misses in this window to measure a difference.' }, 'not measured'))));
  }
  host.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, table));

  // The three things a reader has to see beside that table, or a number in it means less than
  // it looks like it does.
  // Reduced across the arms, like the line below it, rather than read off the first one.
  // ForcedMisses is strategy-independent BY CONSTRUCTION — it counts rows whose recorded reason is
  // prefix_change or cold_start, which no TTL can change — but that is an invariant of the
  // simulator, and this page has no way to check it. Taking the max is the same answer while the
  // invariant holds and the honest one if it ever stops holding.
  const forced = sim.results.reduce((m, r) => Math.max(m, r.forced_misses || 0), 0);
  const open = sim.results.reduce((m, r) => Math.max(m, r.pings_on_open_spans || 0), 0);
  // Hit rate is on the table because an operator asks for it, NOT because it is the objective —
  // and on this service's own traffic the two point opposite ways. Said in words, next to the
  // number, because a reader scanning a table for the best column will otherwise pick it.
  host.appendChild(el('div', { class: 'banner', 'data-testid': 'kv-hitrate-warning' },
    el('strong', {}, 'Hit rate is not the objective. '),
    'The cheapest arm is often not the one that hits most: holding every prefix for an hour '
    + 'raises the hit rate and pays 2.0\u00d7 input to protect prompts that a 1.25\u00d7 write '
    + 'already covered. Read the ', el('strong', {}, 'vs baseline'), ' column, not this one.'));
  host.appendChild(el('p', { class: 'hint', 'data-testid': 'kv-sim-caveats' },
    forced ? num(forced) + ' of these requests missed for a reason no TTL can fix (the prefix '
      + 'changed, or there was no entry at all). Every arm misses on them, which is the '
      + 'ceiling on all of it. ' : '',
    open ? 'Up to ' + num(open) + ' pings are charged into idle spans whose end is not in this '
      + 'window; they are bounded by the window and counted apart. ' : '',
    'The cache premium below each total is what the caching machinery itself cost: a negative '
    + 'one means the cache paid for itself.'));

  // The premium, and the statistics coverage, as a small second table: these are properties of
  // the simulation rather than of the comparison, and putting them in the table above would
  // add four columns nobody reads across.
  const detail = el('table', { class: 'tbl compact', 'data-testid': 'kv-sim-detail' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Strategy'),
      el('th', { class: 'num' }, 'Uncached equivalent'),
      el('th', { class: 'num' }, 'Cache premium'),
      el('th', { class: 'num' }, 'Fresh input'),
      el('th', { class: 'num' }, 'Cache read'),
      el('th', { class: 'num' }, 'Cache write'),
      el('th', { class: 'num' }, 'Output'),
      el('th', { class: 'num' }, 'Unpriced'),
      el('th', {}, 'Decisions'),
      el('th', {}, 'Own history used')))); 
  const dbody = el('tbody');
  detail.appendChild(dbody);
  for (const r of sim.results) {
    // The server's own field, not the same test spelled a second time: `valued` IS
    // unpriced !== requests, and two spellings of one predicate is how they come to
    // disagree the day one of them gains a caveat.
    const priced = r.valued;
    dbody.appendChild(el('tr', {},
      el('td', {}, r.strategy),
      el('td', { class: 'num' }, kvMoney(r.uncached_usd, priced)),
      el('td', { class: 'num' }, kvPremium(r.cache_premium_usd, priced)),
      el('td', { class: 'num' }, kvMoney(r.fresh_input_usd, priced)),
      el('td', { class: 'num' }, kvMoney(r.cache_read_usd, priced)),
      el('td', { class: 'num' }, kvMoney(r.cache_write_usd, priced)),
      el('td', { class: 'num' }, kvMoney(r.output_usd, priced)),
      el('td', { class: 'num' }, r.unpriced ? num(r.unpriced) : '—'),
      el('td', { class: 'small' }, kvDecisions(r.decisions)),
      el('td', { class: 'small' }, kvLevels(r))));
  }
  host.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, detail));

  // And the breakdowns, by user and by model, for the arms on screen.
  host.appendChild(el('h3', {}, 'By user'));
  host.appendChild(kvSimGroups(sim, 'by_user', 'User'));
  host.appendChild(el('h3', {}, 'By model'));
  host.appendChild(kvSimGroups(sim, 'by_model', 'Model'));
}

/**
 * kvPingNote separates the two ways a keep-alive can be more than a refresh.
 *
 * An UPGRADE is a policy buying a hold it chose — the entry was moved to the one-hour tier
 * because this arm decided the conversation was worth holding that long. A REWRITE is a schedule
 * repairing damage it caused: the ping arrived after the entry had already lapsed, so the
 * "refresh" re-created the prefix at 12.5\u00d7 a read. One is the mechanism working and the
 * other is it misconfigured, and a single "pings that cost extra" number would make them look
 * identical.
 */
function kvPingNote(r) {
  const parts = [];
  if (r.pings_that_upgraded) {
    parts.push(num(r.pings_that_upgraded) + ' extended the entry to an hour with a 1h write '
      + '(the policy choosing a longer hold)');
  }
  if (r.pings_that_rewrote) {
    parts.push(num(r.pings_that_rewrote) + ' arrived after the entry had lapsed and re-created '
      + 'it at the write rate (the schedule interval is longer than the lifetime it protects)');
  }
  if (r.pings_on_open_spans) {
    parts.push(num(r.pings_on_open_spans) + ' fall in idle spans whose end is outside this '
      + 'window, so their number depends on where the window ends');
  }
  return parts.join('\n');
}

/** kvDecisions renders one arm's action counts as text. */
function kvDecisions(d) {
  if (!d) return '—';
  const parts = Object.keys(d).sort().filter((k) => d[k]).map((k) => k + ' ' + num(d[k]));
  return parts.length ? parts.join(' · ') : '—';
}

/**
 * kvLevels says how much of an arm was decided on the account's OWN history rather than on the
 * service-wide average.
 *
 * "62% within five minutes over 340 of this user's gaps" and "62%, actually the service-wide
 * average because this user is new" are different facts, and an arm that cannot tell them apart
 * is acting confidently on nothing.
 */
function kvLevels(r) {
  const l = r.stats_levels || {};
  const total = Object.keys(l).reduce((s, k) => s + l[k], 0);
  if (!total) return '—';
  const own = (l['user+model+bucket'] || 0) + (l['user+model'] || 0) + (l.user || 0);
  return el('span', {
    title: Object.keys(l).sort().map((k) => k + ': ' + num(l[k])).join('\n'),
  }, pct(100 * own / total, 0) + ' own history');
}

/** kvSimGroups draws one breakdown dimension for every arm on screen. */
function kvSimGroups(sim, field, label) {
  const table = el('table', { class: 'tbl compact', 'data-testid': 'kv-sim-' + field },
    el('thead', {}, el('tr', {},
      el('th', {}, label), el('th', {}, 'Strategy'),
      el('th', { class: 'num' }, 'Requests'),
      el('th', { class: 'num' }, 'Total cost'),
      el('th', { class: 'num' }, 'Hit rate'),
      el('th', { class: 'num' }, 'Pings'),
      el('th', { class: 'num' }, 'Ping cost'),
      el('th', { class: 'num' }, '5m writes'),
      el('th', { class: 'num' }, '1h writes'))));
  const body = el('tbody');
  table.appendChild(body);
  // Keyed by group, so a reader compares arms within one user rather than across the page.
  const keys = [];
  const seen = new Set();
  for (const r of sim.results) {
    for (const g of r[field] || []) if (!seen.has(g.key)) { seen.add(g.key); keys.push(g.key); }
  }
  if (!keys.length) {
    tableMessage(body, 9, 'Nothing to break down', 'No requests in this window.');
  }
  for (const key of keys) {
    for (const r of sim.results) {
      const g = (r[field] || []).find((x) => x.key === key);
      if (!g) continue;
      body.appendChild(el('tr', {},
        el('td', {}, el('span', { class: 'trunc', title: key }, key || '—')),
        el('td', {}, r.strategy),
        kvNum(g.requests),
        el('td', { class: 'num' }, kvMoney(g.total_usd, g.valued)),
        el('td', { class: 'num' }, pct(g.hit_rate_pct)),
        el('td', { class: 'num' }, g.pings ? num(g.pings) : '—'),
        el('td', { class: 'num' }, g.pings ? kvMoney(g.ping_usd, g.valued) : '—'),
        kvNum(g.writes_5m), kvNum(g.writes_1h)));
    }
  }
  return el('div', { class: 'tblwrap', tabindex: '0' }, table);
}

// ── the detail table ───────────────────────────────────────────────────────

/** kvSortable wires the header buttons to a server-side re-sort. */
function kvSortable(table) {
  $$('thead th', table).forEach((th, i) => {
    const key = KV_COLS[i];
    if (!key) return;
    const label = th.textContent;
    th.setAttribute('aria-sort', key === kvc.sort
      ? (kvc.dir === 'asc' ? 'ascending' : 'descending') : 'none');
    clear(th).appendChild(el('button', {
      class: 'sort', title: 'Sort by ' + label,
      onclick: () => {
        kvc.dir = kvc.sort === key && kvc.dir === 'desc' ? 'asc' : 'desc';
        kvc.sort = key;
        kvc.offset = 0;
        kvLoadRows();
      },
    }, label));
  });
}

/** renderKVTable draws one page of the derived dataset. */
function renderKVTable() {
  const host = clear($('#kv-table'));
  const table = el('table', { class: 'tbl compact', 'data-testid': 'kv-rows-table' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'When'), el('th', {}, 'User'), el('th', {}, 'Conversation'),
      el('th', {}, 'Model'),
      el('th', { class: 'num' }, 'Input'), el('th', { class: 'num' }, 'Output'),
      el('th', { class: 'num' }, 'Cache read'), el('th', { class: 'num' }, 'Cache write'),
      el('th', { class: 'num' }, 'Cached prompt'),
      el('th', {}, 'TTL'), el('th', {}, 'Cache'),
      el('th', { class: 'num' }, 'Idle to next'),
      el('th', {}, 'In 5m'), el('th', {}, 'In 1h'),
      el('th', { class: 'num' }, 'Cost'),
      el('th', {}, 'Next at'))));
  const body = el('tbody', { id: 'kv-rows-body' });
  table.appendChild(body);
  host.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, table));
  kvSortable(table);

  const page = kvc.page;
  if (!page) { loadingRows(body, 16); return; }
  if (!page.rows.length) {
    tableMessage(body, 16, 'No requests match these filters',
      'Widen the time range at the top of the page, or clear a filter.');
    renderKVPager();
    return;
  }
  const wide = wideScope();
  for (const r of page.rows) {
    body.appendChild(el('tr', {},
      el('td', {}, el('a', { href: r.request_url, class: 'kv-link',
        title: 'Open this request' }, when(r.ts))),
      el('td', {}, wide ? el('span', { class: 'trunc', title: r.user }, r.user || '—') : '—'),
      el('td', {}, el('a', { href: r.conversation_url, class: 'kv-link clip',
        title: 'Open this conversation' }, r.conversation_id || '—')),
      el('td', {}, el('span', { class: 'trunc', title: r.model }, r.model || '—')),
      kvTok(r.input_tokens), kvTok(r.output_tokens),
      kvTok(r.cache_read_tokens), kvTok(r.cache_write_tokens),
      kvTok(r.cached_context_tokens),
      el('td', {}, kvTTLPill(r)),
      el('td', {}, kvHitPill(r)),
      el('td', { class: 'num' }, kvIdle(r)),
      el('td', {}, r.has_next ? (r.within_5m ? 'yes' : 'no') : '—'),
      el('td', {}, r.has_next ? (r.within_1h ? 'yes' : 'no') : '—'),
      el('td', { class: 'num' }, kvMoney(r.cost_usd, r.cost_known)),
      el('td', {}, r.has_next ? when(r.next_ts) : '—')));
  }
  renderKVPager();
}

/** renderKVPager draws the offset pager. */
function renderKVPager() {
  const host = clear($('#kv-pager'));
  const page = kvc.page;
  if (!page) return;
  const from = page.rows.length ? kvc.offset + 1 : 0;
  const to = kvc.offset + page.rows.length;
  host.appendChild(el('div', { class: 'pager' },
    el('button', {
      class: 'ghost', 'data-testid': 'kv-prev', disabled: kvc.offset <= 0 ? 'disabled' : null,
      onclick: () => { kvc.offset = Math.max(0, kvc.offset - kvc.limit); kvLoadRows(); },
    }, 'Previous'),
    el('span', { class: 'muted small', 'data-testid': 'kv-pager-label' },
      num(from) + '–' + num(to) + ' of ' + num(page.total)
      + (page.truncated ? ' (analysis capped)' : '')),
    el('button', {
      class: 'ghost', 'data-testid': 'kv-next', disabled: to >= page.total ? 'disabled' : null,
      onclick: () => { kvc.offset += kvc.limit; kvLoadRows(); },
    }, 'Next')));
}

/**
 * renderKVAssumptions prints the server's own statement of every formula and caveat.
 *
 * Printed, not written here. These strings arrive with the data and are checked against the
 * functions that implement them, so the page cannot describe arithmetic the code does not do.
 */
function renderKVAssumptions() {
  const host = clear($('#kv-assumptions'));
  const a = (kvc.analysis && kvc.analysis.assumptions)
    || (kvc.prices && kvc.prices.assumptions);
  if (!a) { loadingState(host, 2); return; }
  host.appendChild(el('p', { class: 'hint', 'data-testid': 'kv-units' },
    'Times are in ', el('strong', {}, a.time_zone), '; every duration is measured in ',
    a.time_unit, '. Reuse horizons: ', num(a.horizon_5m_seconds), ' s and ',
    num(a.horizon_1h_seconds), ' s.'));
  for (const f of a.formulas || []) {
    host.appendChild(el('div', { class: 'kv-formula' },
      el('div', { class: 'kv-formula-name', text: f.name }),
      el('code', { text: f.formula }),
      el('div', { class: 'kv-formula-note', text: f.note })));
  }
  const notes = el('ul', { 'data-testid': 'kv-notes' });
  for (const n of a.notes || []) notes.appendChild(el('li', { class: 'small', text: n }));
  host.appendChild(el('details', { class: 'why' },
    el('summary', {}, 'What is assumed, and what is missing'), notes));
}

// ── loading ────────────────────────────────────────────────────────────────

/** kvReload re-reads everything that depends on the filters or the prices. */
function kvReload() {
  kvc.offset = 0;
  loadKVCache();
}

async function kvLoadAnalysis() {
  try {
    kvc.analysis = await kvFetch('kvcache');
  } catch (err) {
    if (aborted(err)) return;
    errorState(clear($('#kv-tiles')), 'Could not read the KV-cache analysis', err);
    return;
  }
  renderKVCoverage();
  renderKVControls();
  renderKVTiles();
  renderKVIdleBands();
  renderKVSurvival();
  renderKVGroups();
  renderKVAssumptions();
}

async function kvLoadPricing() {
  try {
    kvc.prices = await kvFetch('kvcache/pricing');
  } catch (err) {
    if (aborted(err)) return;
    errorState(clear($('#kv-pricing')), 'Could not read the price list', err);
    return;
  }
  renderKVPricing();
  renderKVAssumptions();
}

async function kvLoadSim() {
  const host = $('#kv-sim');
  loadingState(clear(host), 3);
  try {
    kvc.sim = await kvFetch('kvcache/simulate', {
      // Both OMITTED on the first load, so the server picks its own default arms and its own
      // baseline. That is the point of driving the picker from the registry: the page does not
      // know the roster until the server has told it, and guessing one here is what put a
      // six-name list in this file in the first place.
      baseline: kvc.baseline || undefined,
      strategy: kvc.arms && kvc.arms.length ? kvc.arms.join(',') : undefined,
    });
  } catch (err) {
    if (aborted(err)) return;
    errorState(host, 'Could not replay the strategies', err);
    return;
  }
  renderKVArms();
  renderKVSim();
}

async function kvLoadRows() {
  try {
    kvc.page = await kvFetch('kvcache/rows',
      { limit: kvc.limit, offset: kvc.offset, sort: kvc.sort, dir: kvc.dir });
  } catch (err) {
    if (aborted(err)) return;
    kvc.page = null;
    errorState(clear($('#kv-table')), 'Could not read the request rows', err);
    return;
  }
  renderKVTable();
}

/**
 * loadKVCache fetches the four payloads, each from its own request.
 *
 * Four requests and not one: the analysis is the measurement and must be on screen before any
 * simulation runs, and one slow replay must not blank the histogram beside it. The same
 * arrangement the keep-alive tab uses, for the same reason.
 */
async function loadKVCache() {
  renderKVControls();
  renderKVArms();
  kvLoadAnalysis();
  kvLoadPricing();
  kvLoadSim();
  kvLoadRows();
}

// Registered here, so mounting this whole view is one line in the shared page.
Object.assign(loaders, { kvcache: loadKVCache });
