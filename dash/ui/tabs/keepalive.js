// context-guru dashboard: the Cache strategy (keep-alive) tab.
// Moved out of app.js (#1089). app.js loads first and holds the shared DOM helpers,
// formatting, state and router that this file uses. See the file list in index.html.
'use strict';

// ── keep-alive ─────────────────────────────────────────────────────────────
//
// The tab that answers "is this thing costing me or saving me, and what should I set?".
//
// Three rules run through every renderer below, and each one is a defect this project has
// already shipped once:
//
//   1. NO ARITHMETIC HERE. Every dollar figure is computed on the server, in Go, in one
//      place. The browser has twice duplicated a table the server owns and drifted from it,
//      and money is the worst case of that class. This file posts inputs and renders answers.
//   2. EVERY HEADLINE IS A RANGE WITH ITS n, and the recommendation is REFUSED where the
//      history is too thin. No account in our own corpus can pin its own figure closer than
//      about a factor of two.
//   3. THE SAVING IS A CEILING. The provider's cache is keyed on content, so another session
//      sending a byte-identical prefix would have refreshed the same entry for nothing. Every
//      tile carrying it says so.
//
// Forms: stat tiles, barRows, lineChart, the status bar, tables. No new chart library, no new
// hue, no pie, no gauge — see the SERIES comment at the top of this file for why a hue belongs
// to an entity and never to a rank.

/** kaState holds the tab's loaded payloads, so a control redraw need not refetch. */
const kaState = { ledger: null, recommend: null,
  // sub is true on a Claude subscription (#1022): the measured figures show in tokens, and the
  // forecast keeps dollars with an "at API-price weights" label, or "at learned 5-hour weights" when the
  // fit passed its gate (#1047, forecastView). False for a manager, an API key,
  // Bedrock, Vertex and hosted mode (`{}`), which keep dollars.
  sub: false, behaviour: null, sessions: [], calc: null, armed: [], live: null,
  x: 280, k: 2,
  // canArm is false on a single-tenant deployment, which mounts no control plane at all — so
  // there is nothing to POST an arm to. Drawing the button anyway would put an affordance on the
  // page whose only possible outcome is a 404, which is the same defect as a removal command
  // rendered in the one place nobody should act on.
  canArm: true };

/** kaMuted is the de-emphasis gray for "outside the current coverage". Not a fifth series. */
const KA_MUTED = 'var(--s-mute)';

/** kaSubscription says whether this tab shows tokens: a subscription, and not a manager (whose
 *  view spans other accounts, while the limits are the signed-in account's own). Any failure keeps dollars. */
async function kaSubscription() {
  if (!readsLimits(account && account.tenant)) return false;
  try { return isSubscription(await ctl('/api/me/limits'), Date.now()); } catch (e) { return false; }
}

async function loadKeepAlive() {
  // Panels 1 and 2 first and from ONE request, so the verdict is on screen before any
  // behavioural query runs. A slow window function must never delay the answer to
  // "is this costing me money".
  const host = clear($('#ka-tiles'));
  loadingState(host, 2);
  // The ledger and the recommendation start together; the other panels start as soon as the ledger is
  // back. The recommendation (about nine seconds) used to wait behind the ledger, which put it on the
  // critical path; starting EVERYTHING at once was measured worse still, because six heavy reads
  // contending for the database slowed every one of them (the ledger took twice as long). Each panel
  // still fills from its own request, so one slow query never blanks another.
  const ledger = keepAliveLedger();
  const subP = kaSubscription();
  loadKARecommend(subP);
  const rest = () => { loadKASessions(); loadKALive(); loadKABehaviour(); loadKACalc(); loadKAArmed(); };
  try {
    // The limits read is fast; the ledger and the flag land together, so the tiles never flip units.
    const [l, sub] = await Promise.all([ledger, subP]);
    kaState.sub = sub;
    kaState.ledger = l;
    rest();
    renderKAVerdict(kaState.ledger);
    // The decision row paints immediately with the ledger half, and again when the
    // counterfactual lands. The ledger must not wait on a window function; the row must not
    // wait on the ledger either, or the two halves flash in a different order every load.
    renderKADecision(kaState.ledger, kaState.recommend);
    renderKADownside(kaState.ledger);
  } catch (e) {
    rest();
    if (aborted(e)) return;
    errorState(host, 'Could not read the cache strategy ledger', e);
  }
}

/**
 * kaCoverage renders the three states of "is a zero here a measurement or an absence?".
 *
 * keepalive_pings and keepalive_saved_usd arrived as ADDITIVE columns defaulting to 0, so on a
 * row written before them a zero means NOT RECORDED. Rendering that as "saved nothing" is a
 * fabricated measurement, which is the failure this dashboard has hit repeatedly — hence three
 * distinct states and never a blend of them.
 */
function renderKACoverage(o) {
  const host = clear($('#ka-coverage'));
  if (!o.keepalive_recorded_from) {
    host.appendChild(el('div', { class: 'banner', 'data-testid': 'ka-never-ran' },
      el('strong', {}, 'Your cache strategy has not run on this account in this window. '),
      'The figures below are what it would have had to work with, not a measurement of it. ' +
      'Nothing here is a $0 saving — it is an absence.'));
    return;
  }
  if (o.keepalive_recorded_rows < o.requests) {
    host.appendChild(el('div', { class: 'banner warn', 'data-testid': 'ka-partial' },
      `Recorded on ${num(o.keepalive_recorded_rows)} of ${num(o.requests)} requests in this ` +
      `window — it started running ${when(o.keepalive_recorded_from)}. Rows before that carry ` +
      'no cache-strategy columns at all, so they are absent from these figures rather than zero.'));
  }
}

/**
 * renderKADecision reconciles the THREE verdicts this tab used to give on one window, all of
 * them reachable without touching a filter, two of them right, none of them labelled as the
 * decision:
 *
 *   - the `ka-net` tile:      +$99.79, green, "Winner"
 *   - the K ladder, rung 2:   −$83.33, and all four rungs negative
 *   - "What should I set?":   "this would COST you money … entirely below zero"
 *
 * They are not contradictory measurements. They are three different questions and the tab never
 * said so:
 *
 *   the LEDGER is retrospective and SELECTED — what the shipped policy did on the 36 sessions
 *   it actually fired on, and its saving half is explicitly a CEILING (content-keyed cache);
 *   the LADDER and the RECOMMENDATION are counterfactual over the WHOLE window — every ping the
 *   gated policy would have sent across all 173 sessions with an expiry, 3,323 of them at K=2
 *   against the 544 really sent.
 *
 * The decision "should this stay on across my traffic" is the counterfactual one, so that is the
 * decision number, it is named as such, and the ledger is subordinate to it — still at full
 * size, because a negative net is never demoted (DESIGN §3.7) and neither is an inconvenient
 * positive one.
 *
 * It renders in the Verdict panel, ADJACENT to the ledger tiles, not 2,900 px below them: cost
 * belongs beside its benefit. The counterfactual arrives from a slower endpoint, so the row is
 * built with a placeholder and filled when that lands — the ledger must never wait on it
 * (see loadKeepAlive).
 */
function renderKADecision(o, recIn) {
  const host = clear($('#ka-decision'));
  // On a subscription with learned 5-hour weights, the forecast is the learned one (#1047).
  const fv = forecastView(recIn, kaState.sub), rec = fv.rec;
  const ledgerNet = o && o.keepalive_recorded_from ? o.net_usd : null;
  if (!rec && ledgerNet === null) { host.hidden = true; return; }
  host.hidden = false;
  // One verdict, one number, one sentence, then the setting. The forecast (replay over every
  // session in the window) decides; the measured figure is a labelled upper bound beneath it.
  let verdict, big, cls, sentence, setting;
  if (!rec) {
    verdict = 'Working it out'; big = '\u2026'; cls = '';
    sentence = 'Replaying your past idle gaps to see whether a cache strategy would have paid.';
  } else if (rec.refused) {
    const wouldCost = rec.refused.indexOf('COST') >= 0;
    verdict = wouldCost ? 'No, it would cost you money' : 'Too early to say';
    big = wouldCost ? 'Below $0' : 'Not enough data';
    cls = wouldCost ? 'bad' : '';
    sentence = rec.refused + '.';
    setting = wouldCost ? 'Leave your cache strategy off. If you turn one on anyway, use the default: 240 s idle, 2 pings.'
      : 'Use the default for now: 240 s idle, 2 pings.';
  } else {
    const mid = (rec.lo_usd + rec.hi_usd) / 2;
    verdict = mid < 0 ? 'No, it would cost you money' : 'Yes, it should pay for itself';
    big = usd(rec.lo_usd) + ' to ' + usd(rec.hi_usd);
    cls = mid < 0 ? 'bad' : 'good';
    sentence = 'Expected over a window like this one, from ' + num(rec.n) + ' times a cache ran out in '
      + num(rec.sessions) + ' sessions (90% range).';
    setting = mid < 0 ? 'Leave your cache strategy off.'
      : 'Setting to use: ping after ' + rec.idle_seconds + ' s idle, at most ' + rec.max_pings + ' pings in a row.';
  }
  // On a subscription the measured figures are tokens, but the forecast stays a dollar replay: a
  // yes/no in tokens would need a rule to weigh a saved write against paid reads (#1022).
  const tk = kaState.sub && ledgerNet !== null ? kaTokenLines(o, compact) : null;
  host.appendChild(el('div', { class: 'ka-hero', 'data-testid': 'ka-decision-panel' },
    el('div', { class: 'ka-kicker' }, 'Forecast for all your traffic'),
    fv.label ? el('p', { class: 'ka-sentence', 'data-testid': 'ka-forecast-weights' }, fv.label) : null,
    el('div', { class: 'ka-verdict' + (cls ? ' ' + cls : ''), 'data-testid': 'ka-verdict' }, verdict),
    el('div', { class: 'ka-big ' + cls, 'data-testid': 'ka-forecast' }, big),
    el('p', { class: 'ka-sentence' }, sentence),
    setting ? el('p', { class: 'ka-setting', 'data-testid': 'ka-setting' }, setting) : null,
    tk ? el('p', { class: 'ka-measured', 'data-testid': 'ka-measured-tokens' },
      el('strong', {}, tk.measuredText),
      el('br'), el('strong', {}, 'Pings: ' + tk.pings),
      el('br'), 'On ' + num(o.sessions_touched) + ' sessions. The saving is an ',
      term('upper bound', 'The most it could have saved. The provider keeps one cache per identical prompt start, so another session may have kept the same cache warm for free.'),
      ', so it is not the number to decide on.')
    : ledgerNet !== null ? el('p', { class: 'ka-measured', 'data-testid': 'ka-measured' },
      el('strong', {}, 'Measured on the sessions where it ran: ' + usd(ledgerNet)),
      ' on ' + num(o.sessions_touched) + ' sessions. This is an ',
      term('upper bound', 'The most it could have saved. The provider keeps one cache per identical prompt start, so another session may have kept the same cache warm for free.'),
      ', so it is not the number to decide on.') : null));
}

/** renderKAVerdict is panel 1: a handful of headline numbers, as tiles. Not a bar chart of four
 *  numbers — four values with different units are four figures, not a comparison. */
function renderKAVerdict(o) {
  renderKACoverage(o);
  const host = clear($('#ka-tiles'));
  const ran = !!o.keepalive_recorded_from;
  // "Winner" was decided by the LEDGER's sign alone, so this tab called a policy a winner on a
  // window where its own ladder said every rung loses money and its own recommendation refused
  // it outright. The position now states which population it describes, and the judgement — the
  // --good/--bad colour — is deferred to the decision row above, which has the counterfactual.
  const position = !ran || !o.pings ? 'No pings yet' : (o.net_usd < 0 ? 'Behind' : 'Ahead');
  // On a subscription the saving and the pings are tokens, on two lines that are never netted (#1022).
  const tk = kaState.sub ? kaTokenLines(o, compact) : null;
  const moreTiles = tileGroup(null, null, [
    // The words are beside the colour, always: a status must never rest on hue alone.
    tile('ka-position', 'Where it stands so far', position,
      ran ? (tk && o.pings ? 'Ahead or behind at API-price weights · ' : '') + num(o.sessions_touched) + ' sessions touched — see the decision row above'
        : 'nothing to judge yet', ran ? 'accent' : ''),
    tk ? tile('ka-net-tok', 'Cache strategy, measured', ran ? tk.rescue : '—',
      ran ? 'pings: ' + tk.pings + ' — on the sessions it fired on, an upper bound, not the decision' : 'none')
      : tile('ka-net', 'Cache strategy net, measured', ran ? usd(o.net_usd) : '—',
        'avoided − spent, on the sessions it fired on — an upper bound, not the decision',
        ran ? (o.net_usd < 0 ? 'bad' : '') : ''),
    tile('ka-pings', 'Cache-strategy requests sent', ran ? num(o.pings) : '—',
      ran ? (tk ? tk.pings + ' of your limit, not counting the pings\u2019 output tokens' : usd(o.ping_usd) + ' spent on your key') : 'none'),
    tile('ka-saved', 'Cache rewrites avoided (upper bound)', ran ? (tk ? tk.rescue : usd(o.saved_usd)) : '—',
      ran ? num(o.misses_avoided) + ' requests rescued · a CEILING' : 'none',
      ran && o.saved_usd > 0 ? 'good' : ''),
    tk ? tile('ka-addressable', 'Expiries still worth acting on', num(o.addressable_misses),
      'cache expiries worth acting on', 'accent')
      : tile('ka-addressable', 'Expiries still worth acting on', usd(o.addressable_usd),
        num(o.addressable_misses) + ' cache expiries worth acting on', 'accent'),
    tile('ka-rowgrowth', 'Log growth from cache-strategy requests', o.pings_per_day > 0
      ? compact(o.bytes_per_day) + 'B/day' : '—',
      o.pings_per_day > 0 ? o.pings_per_day.toFixed(0) + ' requests/day × ~400 B' : 'no cache-strategy requests'),
    // Absent, not zero, below the sample-size gate — kvcache.MeasureLatency's own rule: a
    // difference of two means from a handful of rows is not a measurement.
    o.latency && o.latency.known
      ? tile('ka-latency', 'Large requests: cache hit vs. rewrite',
        Math.round(o.latency.per_miss_ms) + ' ms faster per request',
        `n=${num(o.latency.hit_n)} hits vs n=${num(o.latency.miss_n)} writes this window`, 'good')
      : null,
  ]);
  const rows = [];
  const row = (label, value, note) => rows.push(el('div', { class: 'ka-fact' },
    el('div', { class: 'ka-fl' }, label), el('div', { class: 'ka-fv' }, value), el('div', { class: 'ka-fn' }, note)));
  if (ran) {
    row('Cache-strategy requests sent', num(o.pings) + (tk ? ' · ' + tk.pings : ' for ' + usd(o.ping_usd)),
      ['Each ', term('ping', 'A tiny request sent just before the cache would be forgotten, to keep it warm.'),
        tk ? ' counts against your limit.' : ' is billed to your own key.']);
    row('Cache rewrites avoided', tk ? tk.avoidedText : 'up to ' + usd(o.saved_usd),
      [num(o.misses_avoided) + ' requests rescued from a full rewrite. An upper bound.']);
  }
  if (tk && o.tokens_unmeasured_requests > 0) {
    row('Not measured in tokens', num(o.tokens_unmeasured_requests) + ' requests',
      'Requests from before the token count, or that could not be priced. That is not "saved nothing".');
  }
  if (o.latency && o.latency.known) {
    row('Time saved per avoided rewrite', 'about ' + Math.round(o.latency.per_miss_ms) + ' ms',
      'Large requests answered from a warm cache are faster than ones that rewrite it.');
  }
  if (o.pings_per_day > 0) {
    row('Disk used by cache-strategy logs', 'about ' + compact(o.bytes_per_day) + 'B a day',
      'Cache-strategy requests add about ' + compact(o.bytes_per_day) + ' B of logs a day.');
  }
  if (rows.length) host.appendChild(el('div', { class: 'ka-facts', 'data-testid': 'ka-facts' }, ...rows));
  host.appendChild(el('details', { class: 'why', 'data-testid': 'ka-more' },
    el('summary', {}, 'More figures'), moreTiles));
}

/**
 * renderKADownside is panel 2, and it renders UNCONDITIONALLY — including on a positive net.
 *
 * The status bar reuses the same three-state component the recommend question uses, for the
 * same reason: winner/loser is a JUDGEMENT about a state, which is what --good/--bad are
 * reserved for, and every segment is labelled in words below it.
 */
function renderKADownside(o) {
  const host = clear($('#ka-split'));
  const worst = clear($('#ka-worst'));
  const panel = $('#ka-downside-panel');
  panel.classList.remove('bad');
  // The headers follow the units now: the sessions fetch can land later, or fail (#1065 review).
  subLabels(panel, kaState.sub);
  if (!o.sessions_touched) {
    emptyState(host, 'This mechanism has not touched any of your sessions yet',
      'The service-wide split above is what to expect if you switch it on.');
    return;
  }
  const parts = [
    ['winners', 'Came out ahead', o.winners, 'good'],
    ['losers', 'Paid for nothing', o.losers, 'bad'],
  ];
  // On a subscription the head is the two token lines, and the split is by the dollar net: labelled so.
  const tk = kaState.sub ? kaTokenLines(o, compact) : null;
  const across = `${o.sessions_touched} touched session${o.sessions_touched === 1 ? '' : 's'}`;
  host.appendChild(tk
    ? el('div', { class: 'nps-head', 'data-testid': 'ka-split-tokens' },
      el('span', { class: 'nps-score', text: tk.rescue }),
      el('span', { class: 'muted small', text: 'pings: ' + tk.pings + ' · across ' + across
        + '. Ahead or behind is at API-price weights.' }))
    : el('div', { class: 'nps-head' },
      el('span', { class: 'nps-score' + (o.net_usd < 0 ? ' bad' : ''), text: usd(o.net_usd) }),
      el('span', { class: 'muted small', text: 'your own net across ' + across })));
  host.appendChild(el('div', { class: 'nps-bar' }, ...parts
    .filter(([, , n]) => n > 0)
    .map(([key, label, n, c]) => el('div', {
      class: 'nps-seg ' + c, style: 'flex:' + n, 'data-testid': 'ka-' + key,
      title: label + ': ' + n + ' sessions',
    }))));
  host.appendChild(el('div', { class: 'nps-legend' }, ...parts.map(([, label, n, c]) =>
    el('span', {}, el('i', { class: 'sw ' + c }), `${label}: ${n}`))));
  if (o.worst_net_usd < 0) {
    worst.appendChild(tk
      ? el('p', { class: 'hint', 'data-testid': 'ka-worst-session' },
        'Your worst session paid more for pings than it got back, at API-price weights.')
      : el('p', { class: 'hint', 'data-testid': 'ka-worst-session' },
        'Your worst session paid ', el('strong', {}, usd(-o.worst_net_usd)),
        ' and got nothing back for it.'));
  }
  // A negative own net turns the panel into the refusal it should be, with the way out on it.
  if (o.net_usd < 0) {
    worst.appendChild(el('div', { class: 'banner bad', 'data-testid': 'ka-turn-off' },
      tk ? el('div', {}, el('strong', {}, 'At API-price weights this costs more than it saves. '),
        'Over this window the pings: ' + tk.pings + '. ' + tk.rescuesText)
        : el('div', {}, el('strong', {}, 'On your traffic this is costing you money. '),
          'Over this window the pings cost ' + usd(o.ping_usd) + ' and avoided ' +
          usd(o.saved_usd) + '.'),
      el('button', {
        class: 'ghost small', 'data-testid': 'ka-turn-off-btn',
        onclick: async () => {
          if (!confirm('Switch your cache strategy off for this account? Every armed session is ' +
            'disarmed and anything held for you is released immediately.')) return;
          try {
            await ctl('/api/me/keepalive', { method: 'DELETE' });
            alert('Switched off. Nothing further is held or pinged for this account.');
            loadKeepAlive();
          } catch (e) { alert(e.message); }
        },
      }, 'Turn your cache strategy off')));
  }
}

/** loadKASessions fills the concentration table, which doubles as the arm list. */
async function loadKASessions() {
  const body = $('#ka-sessions-body');
  loadingRows(body, 11);
  try {
    const out = await api('keepalive/sessions');
    kaState.sessions = out.sessions || [];
  } catch (e) {
    if (aborted(e)) return;
    tableMessage(body, 11, 'Could not read your sessions', e.message, { error: true });
    return;
  }
  renderKASessions();
}

/** renderKASessions draws the concentration table. Separate from the fetch so it can be redrawn
 *  once the control plane's absence is known, without the arm buttons. */
function renderKASessions() {
  const body = clear($('#ka-sessions-body'));
  const wide = wideScope();
  showScopeCol('[data-testid="ka-sessions-table"]', wide);
  // On a subscription the dollar columns show tokens (#1048): kaSessionTokenCells.
  const sub = kaState.sub;
  subLabels($('#ka-downside-panel'), sub);
  if (!kaState.sessions.length) {
    tableMessage(body, 11, 'No cache expiries worth acting on in this window',
      'Nothing here has anything to keep warm.');
    $('#ka-sessions-note').textContent = '';
    return;
  }
  for (const s of kaState.sessions) {
    const net = s.net_usd;
    const c = sub ? kaSessionTokenCells(s, compact) : null;
    body.appendChild(el('tr', {},
      el('td', {}, el('code', { class: 'clip', text: s.session_id })),
      wide ? el('td', {}, s.tenant_id || '—') : el('td', { hidden: 'hidden' }),
      el('td', { class: 'num' }, num(s.turns)),
      el('td', { class: 'num' }, num(s.expiries)),
      c ? el('td', { class: 'num', 'data-testid': 'ka-expiry-tok' }, c.expiry || '—')
        : el('td', { class: 'num' }, usd(s.expiry_usd)),
      el('td', { class: 'num' }, s.pings ? num(s.pings) : '—'),
      // The pings' own column: what they paid is never added into the rescue beside it.
      c ? el('td', { class: 'num', 'data-testid': 'ka-pings-tok' }, c.pings || '—')
        : el('td', { class: 'num' }, s.pings ? usd(s.ping_usd) : '—'),
      c ? el('td', { class: 'num', 'data-testid': 'ka-rescue-tok' }, c.ran ? c.rescue : '—')
        : el('td', { class: 'num' }, s.saved_usd ? usd(s.saved_usd) : '—'),
      // Tokens of different kinds have no common unit, so a subscription sees no net: only ahead or
      // behind, decided by the dollar net at API-price weights, as the ledger's "Where it stands" (#1042).
      c ? el('td', { class: 'num ' + (net < 0 ? 'bad-text' : net > 0 ? 'good-text' : ''), 'data-testid': 'ka-net-weights',
        title: 'Decided at API-price weights. A subscription does not bill per token.' },
      s.pings || s.saved_usd ? (net < 0 ? 'Behind' : net > 0 ? 'Ahead' : 'Even') : '—')
        : el('td', { class: 'num ' + (net < 0 ? 'bad-text' : net > 0 ? 'good-text' : '') },
          s.pings || s.saved_usd ? usd(net) : '—'),
      // 0 here means the last request reported no usage at all, which is an absence rather than
      // a prompt of no size — and it is also the reason such a session cannot be priced.
      el('td', { class: 'num' }, s.last_prefix > 0 ? compact(s.last_prefix) : '—'),
      // Not drawn on another tenant's session. Arming keys on the AUTHENTICATED principal by
      // design, so an override armed here against a foreign session id lands under the manager's
      // own key and can never match a request — the route returns a cheerful 200 and nothing is
      // kept warm. The affordance whose only outcome is a no-op does not belong on the page.
      el('td', {}, kaState.canArm && mySession(s)
        ? el('button', {
          class: 'ghost small', 'data-testid': 'ka-arm-' + s.session_id,
          onclick: () => armSession(s),
        }, 'Keep warm')
        : null)));
  }
  // The concentration, and the fact that it does not transfer. Both, in one sentence: a table
  // sorted by cost invites exactly the inference the split-half test refutes.
  // The concentration is only a finding when there is something to concentrate: "the top 7 of
  // these hold $81.40 of $81.40" is a tautology, and it is what the first live render said.
  // The denominator is the ACCOUNT's addressable total, not the sum of the rows this page
  // happens to be showing. The table is a page of 20, so summing it made the concentration read
  // 66% where it is 58% and moved the figure whenever `limit` did — the same defect as the
  // "$81.40 of $81.40" tautology, one step less degenerate: the numerator was fixed and the
  // denominator was not. The account's total is already on the page, in "Still on the table".
  const all = (kaState.ledger || {}).addressable_usd || 0;
  // On a subscription the concentration is counted in expiries, not dollars: the expiries' tokens
  // are by tier and do not add up. The order is still the dollar cost, at API-price weights.
  const allN = (kaState.ledger || {}).addressable_misses || 0;
  const lead = sub
    ? (kaState.sessions.length > 8
      ? `The top 8 of these hold ${num(kaState.sessions.slice(0, 8).reduce((a, s) => a + s.expiries, 0))}`
        + ` of ${num(allN)} cache expiries. That concentration is real and it does NOT transfer forward: `
      : `${num(allN)} cache expiries across ${kaState.sessions.length} of your sessions. `
        + 'Where it concentrates does NOT transfer forward: ')
    : kaState.sessions.length > 8
      ? `The top 8 of these hold ${usd(kaState.sessions.slice(0, 8).reduce((a, s) => a + s.expiry_usd, 0))}`
        + ` of ${usd(all)}. That concentration is real and it does NOT transfer forward: `
      : `${usd(all)} of cache expiries across ${kaState.sessions.length} of your sessions. `
        + 'Where it concentrates does NOT transfer forward: ';
  $('#ka-sessions-note').textContent = lead +
    'sessions are ephemeral (median lifetime about zero hours), so this is a description of the ' +
    'past and not a list to act on next week.' +
    (sub ? ' The rows are in order of what the expiries cost at API-price weights.' : '');
}

/**
 * armSession opens the arm dialog: the parameters, and the two numbers being authorized.
 *
 * A prompt() rather than a drawer, deliberately — the smallest thing that can state the
 * expiry, take a confirmation and refuse a bad value. What it must NOT do is hide either
 * number: how long this service may hold the credential, and what the pings can cost at worst.
 */
async function armSession(s) {
  // The credential hold is stated BEFORE the act, not after it. (K+1)×X is arithmetic on the two
  // integers the reader just chose — not money, so computing it here breaks no rule — and it is
  // the number the Settings copy promises ("up to about 12 minutes" on a 5-minute cache), which an
  // override changes. A 1-hour thread follows the account's 1-hour schedule instead; see below.
  // The worst-case SPEND comes back from the server, which owns every dollar on this page.
  const hold = ((kaState.k + 1) * kaState.x) / 60;
  const hours = prompt('Keep this session warm for how many hours?\n\n'
    + `For Anthropic, ${kaState.x}s idle and ${kaState.k} pings can hold your credential for `
    + `up to ${hold.toFixed(0)} minutes at a time ((K+1) × X) on a 5-minute cache; a session on a `
    + `1-hour cache is pinged 59 minutes apart, at most ${Math.min(kaState.k, 3)} time(s) (your ping count, capped at 3), `
    + `so it is held for up to ${(Math.min(kaState.k, 3) + 1) * 59} minutes. `
    + 'For GPT-5.6+ Responses, '
    + 'the account OpenAI cadence applies (at most 28 minutes), pings are capped to keep ' +
      'credential hold within 60 minutes. Between 0.25 and 12 hours, and it '
    + 'is cleared if the service restarts.', '1');
  if (hours === null) return;
  const h = parseFloat(hours);
  if (!isFinite(h) || h <= 0 || h > 12) { alert('Between 0.25 and 12 hours.'); return; }
  const body = {
    session: s.session_id, idle_seconds: kaState.x, max_pings: kaState.k,
    until: Date.now() + Math.round(h * 3600000),
  };
  try {
    const out = await ctl('/api/me/keepalive/sessions',
      { method: 'POST', body: JSON.stringify(body) });
    const cost = out.priced
      ? `at worst ${num(out.worst_case_pings)} pings costing ${usd(out.worst_case_usd)}`
      : `at worst ${num(out.worst_case_pings)} pings — this model is not on the price list, ` +
        'so the cost cannot be stated';
    // Both guards in force, stated. The per-ping ceiling resolves to the service default on an
    // account that configured none, and the prefix floor is whatever this override set — which
    // may be lower than the account's, and 0 means every request on the session is pingable.
    const guards = `No single ping may cost more than ${usd(out.max_usd_per_ping)}, and ` +
      (out.min_prefix_tokens > 0
        ? `only requests with a cached prompt over ${compact(out.min_prefix_tokens)} ` +
          'tokens are pinged'
        : 'there is NO prefix floor on this override, so every request on the session is pingable');
    alert(`Armed until ${when(out.until)}.\n\n` +
      `For Anthropic, your credential may be held for up to ${out.hold_minutes.toFixed(0)} ` +
      `minutes at a time ((K+1) × X). For GPT-5.6+ Responses, the account OpenAI ` +
      `cadence applies and credential hold is capped at 60 minutes. This authorization is ${cost}.\n\n` +
      guards + '.\n\n' +
      'It is cleared if the service restarts. The arm is recorded in your audit log.');
    loadKAArmed();
  } catch (e) { alert('Not armed: ' + e.message); }
}

/**
 * loadKALive fills "Your sessions right now": one row per session whose provider cache entry
 * has not lapsed yet.
 *
 * Its own request, and re-fetched rather than counted down in the browser, because the clock
 * that matters is the SERVER'S. A countdown driven by the client's own clock reads "2 minutes
 * left" on an entry that expired ten minutes ago whenever the two disagree, and the whole point
 * of the panel is the number of seconds left.
 */
async function loadKALive() {
  const body = $('#ka-live-body');
  loadingRows(body, 11);
  try {
    kaState.live = await api('keepalive/live', { x: kaState.x, k: kaState.k });
  } catch (e) {
    if (aborted(e)) return;
    tableMessage(body, 11, 'Could not read your live sessions', e.message, { error: true });
    return;
  }
  renderKALive();
}

/**
 * renderKALive draws the live table, the expiry warning above it, and the potential-saving
 * sentence below it.
 *
 * Separate from the fetch for the reason renderKASessions is: the arm column has to be redrawn
 * once the control plane's absence is known, without buttons that can only 404.
 */
function renderKALive() {
  const live = kaState.live;
  if (!live) return;
  const body = clear($('#ka-live-body'));
  const warn = clear($('#ka-live-warning'));
  const wide = wideScope();
  showScopeCol('[data-testid="ka-live-table"]', wide);
  // On a subscription the measured column shows tokens (#1048). One warm-up, a lapse and the
  // break-even are a price what-if, with no token form yet: they keep dollars, labelled so.
  const sub = kaState.sub;
  subLabels($('#ka-live-panel'), sub);
  const rows = live.rows || [];
  if (!rows.length) {
    tableMessage(body, 11, 'No session of yours has a live cache entry',
      'Either every session in the selected window has been idle longer than its own prompt-cache '
      + 'lifetime, or the window itself ends more than an hour ago — this panel is about NOW, and '
      + 'the filter above still applies to it. Both are an ABSENCE, not a zero saving.');
    $('#ka-live-hint').textContent = '';
    return;
  }
  // The expiry warning. Every figure in it is the SERVER's, counted against the server's clock:
  // the money at risk is the write premium those sessions would pay to re-create what they are
  // about to lose, and this page does not add up dollars.
  if (live.soon) {
    warn.appendChild(el('div', { class: 'banner warn', 'data-testid': 'ka-live-expiring' },
      el('strong', {}, `${num(live.soon)} of your ${num(rows.length)} live `
        + `session${rows.length === 1 ? '' : 's'} expire${live.soon === 1 ? 's' : ''} within `
        // The threshold is 330 s, which is 5.5 minutes: Math.round made the banner say "within
        // 6 minutes" about a 5.5-minute rule, which is 30 seconds of overclaim in the direction
        // of "you have longer than you do".
        + `${(live.soon_seconds / 60).toFixed(1).replace(/\.0$/, '')} minutes.`),
      ' ',
      live.soon_usd > 0
        ? `Between them they would pay ${usd(live.soon_usd)}${sub ? ' at API-price weights' : ''} to re-create what they are about to lose`
          + (live.soon_unpriced
            ? `, excluding ${num(live.soon_unpriced)} with no rate on the price list`
            : '')
          + '. That is a CEILING on what arming them could save: it is only spent if the session '
          + 'actually resumes after the entry has gone, and the provider’s cache is keyed on '
          + 'content, so another session sending the same prefix would refresh it for nothing.'
        : 'None of them has a rate on the operator’s price list, so what they would pay '
          + 'cannot be stated — it is unknown, not zero.'));
  }
  for (const r of rows) {
    const left = r.remaining_seconds;
    const urgent = left <= live.soon_seconds;
    body.appendChild(el('tr', {},
      el('td', {}, el('code', { class: 'clip', text: r.session_id })),
      wide ? el('td', {}, r.tenant_id || '—') : el('td', { hidden: 'hidden' }),
      el('td', { class: 'num' }, num(r.turns)),
      el('td', { class: 'num' }, r.prefix_tokens > 0 ? compact(r.prefix_tokens) : '—'),
      // The tier the provider BILLED, which is the only honest source for what is in force.
      el('td', {}, r.ttl_seconds >= 3600 ? '1 hour' : '5 minutes'),
      el('td', { class: 'num ' + (urgent ? 'bad-text' : '') }, dur(left * 1000)),
      el('td', { class: 'num' }, r.priced ? usd(r.ping_usd_each) : '—'),
      el('td', { class: 'num' }, r.priced ? usd(r.miss_usd) : '—'),
      // The number the whole panel exists to state: how many pings one lapse pays for.
      el('td', { class: 'num' }, r.priced
        ? el('span', { title: `${num(r.breakeven_pings)} pings at ${kaState.x}s bridge `
            + `${r.breakeven_minutes.toFixed(0)} minutes of idle. With a 1-hour entry one lapse `
            + `would pay for ${num(r.breakeven_1h_pings)}, bridging `
            + `${r.breakeven_1h_minutes.toFixed(0)} minutes.` },
          `${num(r.breakeven_pings)} · ${r.breakeven_minutes.toFixed(0)}m`)
        : '—'),
      sub ? kaLiveTokens(r) : el('td', { class: 'num' }, r.saved_usd || r.ping_usd
        ? usd(r.saved_usd - r.ping_usd)
        : '—'),
      el('td', {}, kaState.canArm && mySession(r)
        ? el('button', {
          class: 'ghost small', 'data-testid': 'ka-live-arm-' + r.session_id,
          onclick: () => armSession(r),
        }, 'Keep warm')
        : null)));
  }
  // Realised against potential, both from figures already on this page, and the difference
  // between them named for what it is.
  const led = kaState.ledger || {};
  if (sub) {
    const tk = kaTokenLines(led, compact);
    $('#ka-live-hint').textContent =
      `So far in this window${tk.measured ? ' (measured)' : ''}: ${tk.rescuesText} ${PING_LABEL}: ${tk.pings}. `
      + `Still open: these ${num(rows.length)} sessions hold ${usd(live.potential_usd)} of rewrite `
      + 'cost a lapse would charge, at API-price weights. That is a CEILING, not a measurement: it is only earned by a '
      + 'session that resumes AFTER its cache would have gone. PINGS PER LAPSE is the one to decide '
      + 'on: below it, pinging is cheaper than the lapse it prevents.';
    return;
  }
  $('#ka-live-hint').textContent =
    `So far in this window: ${usd(led.saved_usd || 0)} of rewrites avoided against `
    + `${usd(led.ping_usd || 0)} of pings, a net of ${usd(led.net_usd || 0)} (measured). `
    + `Still open: these ${num(rows.length)} sessions hold ${usd(live.potential_usd)} of rewrite `
    + 'cost a lapse would charge. That is a CEILING, not a measurement: it is only earned by a '
    + 'session that resumes AFTER its cache would have gone. PINGS PER LAPSE is the one to decide '
    + 'on: below it, pinging is cheaper than the lapse it prevents.';
}

/** The live table's REALISED cell in tokens, on a subscription (#1048): the rescue on one line, and
 *  what the pings paid on a line of its own, never added into it. `—` when the strategy did not run. */
function kaLiveTokens(r) {
  const c = kaSessionTokenCells(r, compact);
  if (!c.ran) return el('td', { class: 'num' }, '—');
  return el('td', {
    class: 'num', 'data-testid': 'ka-live-realised-tok',
    title: 'The first line is the cache rewrites the pings avoided (a ceiling). The second is what the pings paid: it is on its own line, not in any total.',
  },
  el('div', { text: c.rescue }),
  c.pings ? el('div', { class: 'small', 'data-testid': 'ka-live-pings-tok', text: PING_LABEL + ': ' + c.pings }) : null);
}

/**
 * mySession answers "can the signed-in principal actually arm this row?".
 *
 * An override is stored under the ARMING principal's own tenant, which is the isolation the
 * design wants and TestOverrideForAnotherTenantsSessionIdAddressesNothing proves. The
 * consequence is that arming somebody else's session silently does nothing, so a manager's
 * service-wide view must not offer it. On a single-tenant page every row is the caller's own.
 */
function mySession(r) {
  const mine = (account.tenant || {}).id;
  return !mine || !r.tenant_id || r.tenant_id === mine;
}

/** loadKAArmed lists what is armed RIGHT NOW — the live policy, not a stored intention. */
async function loadKAArmed() {
  const host = clear($('#ka-armed'));
  let out;
  try {
    out = await ctl('/api/me/keepalive/sessions');
  } catch (e) {
    // A single-tenant deployment has no control plane at all; that is not an error to show — but
    // it does mean the arm buttons in the table above cannot work, so the whole panel goes and
    // the table is redrawn without them.
    if (e.status === 404) {
      kaState.canArm = false;
      $('#view-keepalive').querySelectorAll('[data-ka-arm-panel]').forEach((n) => { n.hidden = true; });
      host.hidden = true;
      if (kaState.sessions.length) renderKASessions();
      renderKALive();
      return;
    }
    errorState(host, 'Could not read what is armed', e);
    return;
  }
  host.hidden = false;
  kaState.armed = out.armed || [];
  host.appendChild(el('p', { class: 'hint' },
    `At most ${out.max_hours} hours per session, and ${out.max_hold_minutes} minutes is the `
    + 'ceiling on how long your credential may be held between pings on a 5-minute cache. '
    + `A 1-hour cache keeps the account's 59-minute timing; the override's ping count applies to it `
    + `(at most 3), so it is held for up to ${out.max_hold_minutes_1h} minutes. `,
    el('strong', {}, 'Armed sessions are cleared if the service restarts'),
    ' — an authorization to spend that silently outlived a restart would be worse than one '
    + 'that does not. The arm itself is in your audit log permanently.'));
  if (!kaState.armed.length) {
    emptyState(host, 'Nothing is armed', 'Use "Keep warm" on a session above.');
    return;
  }
  const tbl = el('table', { class: 'grid' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Session'), el('th', {}, 'Idle'), el('th', {}, 'Pings'),
      el('th', {}, 'Credential hold'), el('th', {}, 'Armed until'),
      el('th', {}, el('span', { class: 'vh' }, 'Row actions')))));
  const tb = el('tbody');
  for (const a of kaState.armed) {
    tb.appendChild(el('tr', {},
      el('td', {}, el('code', { class: 'clip' }, a.session)),
      el('td', {}, a.idle_seconds + 's'),
      el('td', {}, String(a.max_pings)),
      el('td', {}, '≤ ' + a.hold_minutes.toFixed(0) + ' min'),
      el('td', {}, when(a.until)),
      el('td', {}, el('button', {
        class: 'ghost small', 'data-testid': 'ka-disarm-' + a.session,
        onclick: async () => {
          try {
            await ctl('/api/me/keepalive/sessions/' + encodeURIComponent(a.session),
              { method: 'DELETE' });
            loadKAArmed();
          } catch (e) { alert(e.message); }
        },
      }, 'Disarm'))));
  }
  tbl.appendChild(tb);
  host.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, tbl));
}

/** loadKABehaviour fills panels 3a–3e. */
async function loadKABehaviour() {
  const misses = $('#ka-chart-misses');
  loadingState(misses, 2);
  let b;
  try {
    b = await api('keepalive/behaviour', { x: kaState.x, k: kaState.k });
  } catch (e) {
    if (aborted(e)) return;
    errorState(misses, 'Could not read your expiry history', e);
    return;
  }
  kaState.behaviour = b;
  // On a subscription the expiries' cost shows as the tokens they billed, by tier (#1048).
  const sub = kaState.sub;
  subLabels($('#ka-behaviour-panel'), sub);
  const cost = (g) => (sub ? bandTokens(g, compact) : usd(g.usd));
  const days = (b.daily || []).map((d) => [d.ts, d.misses]);
  const usdPts = (b.daily || []).map((d) => [d.ts, d.usd]);
  // TWO charts. A count and a dollar figure share no scale, and a second y-axis is the single
  // most common way a chart implies a relationship it has not measured.
  lineChart(misses, [{ name: 'Cache expiries', color: SERIES[0], points: days, area: true }],
    { yFmt: num, xFmt: whenAxis });
  // In tokens: one line per tier, on one token scale. The tiers are never added into one line.
  // On a subscription the line is in tokens, so a mixed view still draws it.
  if (sub || !noMoneyChart($('#ka-chart-usd'))) lineChart($('#ka-chart-usd'), sub
    ? tierSeries(b.daily).map((t, i) => ({ name: t.name, color: SERIES[i], points: t.points }))
    : [{ name: 'What they cost', color: SERIES[0], points: usdPts, area: true }],
  { yFmt: sub ? compact : usd, xFmt: whenAxis });

  // 3b: the gap bands, in EDGE ORDER — the order is the meaning, so they are never sorted by
  // size — with everything beyond the current coverage in the de-emphasis gray.
  $('#ka-gap-note').textContent =
    'Each bar is a range of idle time before the cache ran out; longer bars mean more expiries. ' +
    `Half the gaps were under ${b.gap_p50.toFixed(0)}s (10% under ${b.gap_p10.toFixed(0)}s, 90% under ${b.gap_p90.toFixed(0)}s). ` +
    `At ${kaState.x}s idle and ${kaState.k} ping${kaState.k === 1 ? '' : 's'} the cache stays ` +
    `alive for ${b.coverage_seconds.toFixed(0)}s, so grey bars are expiries this ` +
    `setting cannot reach. ${num(b.phantom_ttl_rows)} further ttl_expiry rows are left out: they wrote nothing, so there was nothing to protect.`;
  barRows($('#ka-gap-bands'), (b.gap_bands || []).map((g) => ({
    label: g.label + (g.beyond ? ' (beyond coverage)' : ''),
    value: g.n, display: num(g.n) + ' · ' + cost(g),
    color: g.beyond ? KA_MUTED : SERIES[0],
    title: `${g.n} expiries ${sub ? 'billing' : 'costing'} ${cost(g)}`,
    onclick: () => { setFilter('reason', 'ttl_expiry'); go('requests'); },
  })), { emptyDetail: 'No cache expiries in this window.' });
  kaTable('#ka-gap-bands', 'Idle gap', b.gap_bands, sub);

  // 3e: prefix size, and the measured reason the 20k gate is nearly free.
  const above = b.addressable_misses > 0
    ? pct(100 * b.prefix_above_20k / b.addressable_misses, 0) : '—';
  $('#ka-prefix-note').textContent =
    'Each bar is a range of cached-prompt sizes at the moment the cache ran out. ' +
    `Half were under ${compact(b.prefix_p50)} tokens (10% under ${compact(b.prefix_p10)}, 90% under ${compact(b.prefix_p90)}). ` +
    `${above} were already past 20k tokens, so the 20k floor costs almost nothing. ` +
    'A main thread that waits for a person or an unknown cause needs 80k.';
  barRows($('#ka-prefix-bands'), (b.prefix_bands || []).map((g) => ({
    label: g.label + ' tokens', value: g.n, display: num(g.n) + ' · ' + cost(g),
    color: SERIES[0], title: `${g.n} expiries ${sub ? 'billing' : 'costing'} ${cost(g)}`,
  })), { emptyDetail: 'No cache expiries in this window.' });
  kaTable('#ka-prefix-bands', 'Cached prompt size', b.prefix_bands, sub);

  // 3c: 24 bins, in the VIEWER's own timezone with the offset stated. "What times of day" is
  // a human question and a UTC-only axis answers a different one.
  const offMin = -new Date().getTimezoneOffset();
  const offLabel = 'UTC' + (offMin < 0 ? '−' : '+') +
    String(Math.floor(Math.abs(offMin) / 60)).padStart(2, '0') +
    ':' + String(Math.abs(offMin) % 60).padStart(2, '0');
  $('#ka-hour-note').textContent =
    `Each bar is one hour of the day when the cache ran out, in your timezone (${offLabel}). ` +
    'On this service the busiest hour has about eight times the expiries of the quietest.';
  const bins = (b.hour_bins || []).map((h) => {
    const start = (Number(h.label) * 60 + offMin + 1440) % 1440;
    const hh = String(Math.floor(start / 60)).padStart(2, '0');
    const mm = String(start % 60).padStart(2, '0');
    return { label: `${hh}:${mm}`, value: h.n, display: num(h.n) + ' · ' + cost(h),
      color: SERIES[0], title: `${h.n} expiries ${sub ? 'billing' : 'costing'} ${cost(h)}`, sort: start };
  }).sort((a, c) => a.sort - c.sort);
  barRows($('#ka-hour-bins'), bins, { emptyDetail: 'No cache expiries in this window.' });

  // 3d: per account, mean/median/max, never a single blended figure.
  const gaps = clear($('#ka-account-gaps'));
  if (!(b.account_gaps || []).length) {
    emptyState(gaps, 'Not enough expiries to measure a gap between them',
      'A gap needs two expiries on the same account.');
  } else {
    const tbl = el('table', { class: 'grid' },
      el('thead', {}, el('tr', {}, el('th', {}, 'Account'), el('th', {}, 'Gaps'),
        el('th', {}, 'Mean'), el('th', {}, 'Median'), el('th', {}, 'Longest'))));
    const tb = el('tbody');
    for (const g of b.account_gaps) {
      tb.appendChild(el('tr', {}, el('td', {}, g.tenant || 'this account'),
        el('td', {}, num(g.n)), el('td', {}, g.mean_hours.toFixed(2) + ' h'),
        el('td', {}, g.median_hours.toFixed(2) + ' h'), el('td', {}, g.max_hours.toFixed(2) + ' h')));
    }
    tbl.appendChild(tb);
    gaps.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, tbl));
  }
}

/** kaTable adds the table view of a band chart, as the existing panels do. `sub`: the cost column
 *  shows the tokens the expiries billed, by tier (#1048). */
function kaTable(sel, dim, bands, sub) {
  const host = $(sel);
  if (!bands || !bands.length) return;
  const rows = bands.map((b) => el('tr', {}, el('td', {}, b.label),
    el('td', { class: 'num' }, num(b.n)), el('td', { class: 'num' }, sub ? bandTokens(b, compact) : usd(b.usd))));
  host.appendChild(el('details', { class: 'why' },
    el('summary', {}, 'Table view'),
    el('table', { class: 'grid' },
      el('thead', {}, el('tr', {}, el('th', {}, dim), el('th', { class: 'num' }, 'Expiries'),
        el('th', { class: 'num' }, sub ? 'Tokens billed' : 'Cost'))),
      el('tbody', {}, ...rows))));
}

/** renderKACalcControls draws X, K and the prefix. Every value is sent to the server, which
 *  owns the arithmetic; nothing below multiplies a dollar by anything. */
function renderKACalcControls() {
  const host = clear($('#ka-calc-controls'));
  const x = el('input', { type: 'number', id: 'ka-x', min: '60', max: '290', step: '10',
    value: String(kaState.x), 'data-testid': 'ka-x' });
  const k = el('input', { type: 'number', id: 'ka-k', min: '1', max: '4', step: '1',
    value: String(kaState.k), 'data-testid': 'ka-k' });
  const apply = el('button', { class: 'ghost', 'data-testid': 'ka-calc-apply',
    onclick: () => {
      kaState.x = Math.max(60, Math.min(290, parseInt(x.value, 10) || 280));
      kaState.k = Math.max(1, Math.min(4, parseInt(k.value, 10) || 2));
      loadKACalc();
      loadKABehaviour(); // the coverage rule on the gap bands moves with the policy
      loadKALive();      // and so do the live panel's own reach figures
    } }, 'Recalculate');
  host.appendChild(el('div', { class: 'field-row' },
    el('label', { for: 'ka-x' }, 'Idle seconds (X)'), x,
    el('label', { for: 'ka-k' }, 'Max pings (K)'), k, apply));
}

/** kaRungName names a ladder rung by its model: the live plan, or a fixed count. */
function kaRungName(r) {
  const name = r.plan === 'wait_plan' ? 'Live plan, base ' + r.max_pings : 'Fixed ' + r.max_pings;
  return name + (r.current ? ' (current)' : '');
}

/** kaRungCoverage says how far a rung reaches. The plan's reach depends on the wait, so it has no single figure. */
function kaRungCoverage(r) {
  const pc = kaState.calc && kaState.calc.plan_counts;
  return r.plan === 'wait_plan'
    ? (pc ? `by wait: a tool up to ${pc.tool_wait_max}, a person base+${pc.human_extra}` : 'by wait')
    : r.coverage_seconds.toFixed(0) + 's = ' + r.max_pings + '×' + kaState.x + ' + 300';
}

/** loadKACalc renders the K ladder. */
async function loadKACalc() {
  renderKACalcControls();
  const host = clear($('#ka-calc'));
  loadingState(host, 3);
  let c;
  try {
    c = await api('keepalive/calc', { x: kaState.x, k: kaState.k });
  } catch (e) {
    if (aborted(e)) return;
    errorState(host, 'Could not compute the ladder', e);
    return;
  }
  kaState.calc = c;
  clear(host);
  if (!c.prefix_tokens) {
    emptyState(host, 'No cached prompt to compute against',
      'This window holds no cache expiry of yours, so there is no real prefix to price a ping ' +
      'on. A service-wide average would be misleading: per-ping cost runs from $0.0004 at the ' +
      'median to $0.2275 at the 99th percentile.');
    return;
  }
  // The ladder is a replay priced at one prefix: a price what-if, with no token form yet. On a
  // subscription it keeps dollars and says they are at API-price weights (#1048), as the forecast does.
  if (kaState.sub) {
    host.appendChild(el('p', { class: 'ka-sentence', 'data-testid': 'ka-calc-weights' },
      'Dollars at API-price weights. A subscription does not bill per token.'));
  }
  const src = { session: 'the selected session’s last cached prompt',
    account_median: 'your own median cached prompt at a lapse',
    given: 'the size you gave' }[c.prefix_source] || 'a real cached prompt';
  host.appendChild(el('p', { class: 'note' },
    `Priced on ${compact(c.prefix_tokens)} tokens — ${src} — for `,
    el('code', {}, c.model || 'an unnamed model'), '. ',
    c.priced
      ? `One ping costs ${usd(c.ping_usd_each)}; one rescued expiry avoids ` +
        `${usd(c.avoided_usd_each)}, which is the WRITE PREMIUM and not the whole miss.`
      : 'This model is not on the operator’s price list, so the counts below stand and ' +
        'the dollar columns are blank. They are not zero.'));
  const tbl = el('table', { class: 'grid', 'data-testid': 'ka-ladder' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Policy (K)'), el('th', {}, 'Cache stays alive'),
      el('th', { class: 'num' }, 'Your expiries inside it'),
      el('th', { class: 'num' }, 'Their cost'),
      // The value is convertible_usd / addressable_usd — a share of the DOLLARS, not of the
      // expiry count, and the old label ("share of what is reachable") named neither.
      el('th', { class: 'num', title: 'Their cost as a share of every addressable expiry’s cost '
        + 'in this window' }, 'Share of those dollars'),
      el('th', { class: 'num' }, 'Pings'), el('th', { class: 'num' }, 'Ping cost'),
      el('th', { class: 'num' }, 'Net'))));
  const tb = el('tbody');
  for (const r of c.rows) {
    // Emphasis by weight, not by a new hue: the current row is the accent, the rest recede.
    tb.appendChild(el('tr', { class: r.current ? 'is-current' : 'muted-row',
      'data-testid': r.plan === 'wait_plan' ? 'ka-ladder-plan' : 'ka-ladder-k' + r.max_pings },
      el('td', {}, kaRungName(r)),
      el('td', {}, kaRungCoverage(r)),
      el('td', { class: 'num' }, num(r.convertible_misses)),
      el('td', { class: 'num' }, usd(r.convertible_usd)),
      el('td', { class: 'num' }, pct(r.share_of_addressable_pct, 1)),
      el('td', { class: 'num' }, num(r.pings)),
      el('td', { class: 'num' }, c.priced ? usd(r.ping_usd) : 'not priced'),
      el('td', { class: 'num ' + (c.priced && r.net_usd < 0 ? 'bad-text' : '') },
        c.priced ? usd(r.net_usd) : 'not priced')));
  }
  tbl.appendChild(tb);
  host.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, tbl));
  // Reach and cost side by side, so the flattening is visible: on our own traffic K=1→K=2 takes
  // reach from 30% to 51% of the recoverable dollars and K=3, K=4 add 6 and 4 points, while
  // ping count keeps climbing.
  //
  // Its OWN container: barRows clears the host it is given, and passing the panel's host wiped
  // the ladder table above it. The page looked plausible — a bar per rung, all the numbers
  // present — and the table it replaced was simply absent, which is precisely why this was found
  // by opening the page and not by reading the code.
  const share = el('div');
  host.appendChild(share);
  barRows(share, c.rows.map((r) => ({
    label: kaRungName(r),
    value: r.share_of_addressable_pct, max: 100,
    display: pct(r.share_of_addressable_pct, 1) + ' · ' + num(r.pings) + ' pings',
    color: r.current ? 'var(--accent)' : KA_MUTED,
    title: `${r.convertible_misses} of ${c.addressable_misses} expiries reachable`,
  })), {});
  host.appendChild(el('p', { class: 'hint' },
    'Each row is one policy. Reading across: how many of your expiries it would have caught, ' +
    'what they cost, and what the pings cost. A replay of your past gaps, not a forecast. ' +
    (c.plan === 'wait_plan'
      ? 'The first row is the live plan: it picks the count from what each thread waits for ' +
        (c.plan_counts
          ? `(up to ${c.plan_counts.tool_wait_max} for a tool wait, the base plus ${c.plan_counts.background_extra} for background work, ` +
            `the base plus ${c.plan_counts.human_extra} for a person, `
          : '(') +
        'none before the thread has called a tool). The fixed rows send the same count for every wait, ' +
        'for comparison. It shows your account setting after campaigns and manager strategies, not a per-session override. '
      : 'Your account has the per-wait plan switched off, so every row is a fixed count and the row ' +
        'with your count is the current one. ') +
    'Catching grows sharply from one ping to two, then flattens while ping cost keeps climbing.'));
  host.appendChild(el('details', { class: 'why' }, el('summary', {}, 'Technical details'),
    el('p', {},
    'PINGS is every ping the policy would have sent over this window, gated as the ' +
    'request path gates (past the session\u2019s first request, cached prompt over 20k tokens, or 80k for a main thread that waits for a person or an unknown cause) and ' +
    'including the pings a session\u2019s last request attracts, because a live policy cannot know a ' +
    'session has ended and sends its full K anyway. It counts only expiries that were ' +
    'addressable (the provider wrote a prefix that a ping could have refreshed). The live-plan row ' +
    'reads the stop reason and wait cause the proxy stored for the request before each idle gap, and ' +
    'whether the thread had called a tool by then. It uses the same rule the proxy uses.')));
}

/**
 * loadKARecommend renders a RANGE with its n, or a refusal with the count that caused it.
 *
 * Never a hero number and never a gauge. Every account in our own corpus has a 90% interval
 * whose relative half-width is at least 62%, and it crosses zero for 5 of 12 — so the honest
 * sentence is "the direction is resolvable, the size is not", and it is on the page.
 */
async function loadKARecommend(subP) {
  const host = clear($('#ka-recommend'));
  loadingState(host, 2);
  let raw, sub;
  try {
    [raw, sub] = await Promise.all([api('keepalive/recommend'), subP || kaState.sub]);
  } catch (e) {
    if (aborted(e)) return;
    errorState(host, 'Could not work out a recommendation', e);
    return;
  }
  kaState.recommend = raw;
  // The decision row's forecast (#1047), so the two panels never disagree.
  const fv = forecastView(raw, sub), rec = fv.rec;
  // Same payload, two places: the decision row above (which is what a reader acts on) and the
  // detail below. Rendering it only here is how the refusal came to sit 2,900 px under a green
  // "Winner" tile that contradicted it.
  // If the ledger has not landed yet, its own handler renders the decision row with this payload.
  if (kaState.ledger) renderKADecision(kaState.ledger, raw);
  clear(host);
  if (fv.label) host.appendChild(el('p', { class: 'ka-sentence', 'data-testid': 'ka-rec-weights' }, fv.label));
  if (rec.refused) {
    // Two different refusals, and only one of them is about thin history: "your own gaps are
    // too long for this to reach" is a finding, not a shortage of data, and it is a `bad` banner
    // rather than a `warn` one because it is telling somebody not to switch this on.
    const wouldCost = rec.refused.indexOf('COST') >= 0;
    host.appendChild(el('div', { class: 'banner ' + (wouldCost ? 'bad' : 'warn'),
      'data-testid': 'ka-refused' },
      el('div', {}, el('strong', {},
        wouldCost ? 'This would not pay for itself on your traffic: ' : 'Not enough history to recommend: '),
      rec.refused, '.'),
      el('div', { class: 'small' },
        `For scale: measured over 357 cache expiries across this whole service, the 90% `
        + `interval on this policy is ${usd(rec.service_lo_usd)} to ${usd(rec.service_hi_usd)}. `
        + 'Use the default (240 s idle, 2 pings) or arm a single session above and read your '
        + 'own ledger.')));
    return;
  }
  host.appendChild(tileGroup(null, null, [
    // A RANGE as the tile's value. There is no point estimate on the wire to render.
    tile('ka-rec-range', 'Expected over a window like this one',
      usd(rec.lo_usd) + ' – ' + usd(rec.hi_usd),
      `90% interval from ${num(rec.n)} expiries in ${num(rec.sessions)} sessions`,
      rec.lo_usd > 0 ? 'good' : 'bad'),
    tile('ka-rec-policy', 'Suggested', rec.idle_seconds + 's / ' + rec.max_pings + ' pings'
      + (rec.plan === 'wait_plan' ? ' (base)' : ''),
      'past a session\u2019s first request, prompt over 20k (80k if a person or an unknown cause)', 'accent'),
  ]));
  if (rec.plan === 'wait_plan' && rec.fixed_k) {
    host.appendChild(el('p', { class: 'note', 'data-testid': 'ka-rec-fixed' },
      `The live plan picks the count from each wait. For comparison, ${rec.max_pings} pings for every wait ` +
      `would have given ${usd(rec.fixed_k.lo_usd)} – ${usd(rec.fixed_k.hi_usd)} over the same window.`));
  }
  host.appendChild(el('p', { class: 'note' },
    'The direction is resolvable; the size is not — a window like this one cannot pin it ',
    el('strong', {}, 'closer than about a factor of two'),
    '. Watch your own ledger for a week before changing it.',
    rec.alt_max_pings
      ? ` ${rec.alt_max_pings} pings clears this account's own noise and is worth trying.`
      : ' 2 pings and 3 pings are a measured tie on this history, so 2 is suggested for the ' +
        'lower request volume rather than for a dollar reason.'));
  host.appendChild(el('div', { class: 'actions' }, el('button', {
    class: 'ghost', 'data-testid': 'ka-rec-apply',
    onclick: () => {
      // Fills the fields in and leaves them UNSAVED, the same pattern the component
      // recommendations use. A recommendation whose own interval is 62% wide is not something
      // a service should apply on somebody's behalf.
      go('settings');
      setTimeout(() => {
        const idle = $('#cfg-cache-keepalive_idle_seconds');
        const pings = $('#cfg-cache-keepalive_max_pings');
        if (idle) idle.value = rec.idle_seconds;
        if (pings) pings.value = rec.max_pings;
        alert('Filled in on the Settings tab. Nothing is saved until you press Save there.');
      }, 200);
    },
  }, 'Use these values in Settings')));
  host.appendChild(el('p', { class: 'hint' },
    'Nothing is applied for you. One ping is never suggested: it covers 9.7 minutes of idle time where two ' +
    'cover 14.3. One ping is not a loss, just a smaller win. Compare the rows above on your own gaps.'));
}

Object.assign(loaders, { keepalive: loadKeepAlive });
