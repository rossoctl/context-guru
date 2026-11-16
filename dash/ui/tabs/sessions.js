// context-guru dashboard: the Sessions tab.
// Moved out of app.js (#1089). app.js loads first and holds the shared DOM helpers,
// formatting, state and router that this file uses. See the file list in index.html.
'use strict';

// ── sessions ───────────────────────────────────────────────────────────────
/**
 * wideScope is "this list can contain more than one account", which is a manager with no
 * ?tenant= — the server's default. It is what decides whether the Account column is shown:
 * a single-account list does not need a column repeating the filter bar.
 */
function wideScope() { return isManager() && !state.filter.tenant; }
/** showScopeCol toggles the static Account <th> of one table. */
function showScopeCol(tableSel, wide) {
  for (const th of $$('thead th[data-scope-col]', $(tableSel))) th.hidden = !wide;
  return wide;
}

/**
 * GET /api/me/limits, one request for the Overview and the Sessions tab. A request in flight is
 * always shared. A good answer is reused while it is at most `maxAgeMs` old: the Overview passes 0
 * (it always asks for a fresh answer, as before), the Sessions tab a minute. A failed answer is not
 * kept, so the next caller asks again.
 */
function meLimits(maxAgeMs) {
  const now = Date.now();
  const reuse = state.limitsP && (state.limitsAt === null || now - state.limitsAt <= maxAgeMs);
  if (!reuse) {
    state.limitsAt = null; // null: in flight
    const p = ctl('/api/me/limits');
    state.limitsP = p;
    p.then(() => { if (state.limitsP === p) state.limitsAt = Date.now(); },
      () => { if (state.limitsP === p) state.limitsP = null; });
  }
  return state.limitsP;
}

/**
 * True on a Claude subscription (GET /api/me/limits, isSubscription): the Sessions tab then shows
 * tokens, not dollars. A manager (readsLimits), an API key and hosted mode (#997: `{}`) keep the
 * dollars, and so does any failure.
 */
function onSubscription() {
  if (!readsLimits(account && account.tenant)) return Promise.resolve(false);
  return meLimits(60000).then((l) => isSubscription(l, Date.now()), () => false);
}

/** The elements under `host` that name dollars take their token label (data-sub-label, and
 *  data-sub-title when set) on a subscription, and their own content back otherwise. The dollar
 *  content is kept as nodes, so its markup (an em, a br) comes back as it was. */
function subLabels(host, sub) {
  if (!host) return;
  for (const n of $$('[data-sub-label]', host)) {
    if (!n.usdNodes) { n.usdNodes = [...n.childNodes].map((c) => c.cloneNode(true)); n.dataset.usdTitle = n.title; }
    if (sub) n.textContent = n.dataset.subLabel;
    else n.replaceChildren(...n.usdNodes.map((c) => c.cloneNode(true)));
    n.title = sub && n.dataset.subTitle !== undefined ? n.dataset.subTitle : n.dataset.usdTitle;
  }
}

/** The Sessions headers that name dollars take their token label (data-sub-label) on a subscription. */
function sessionHeaders(sub) {
  subLabels($('[data-testid=sessions-table]'), sub);
  // "Costliest" still orders by the dollar cost: one token order would add the tiers. The dollar
  // order weights them at API prices, as the 5-hour estimate does.
  const opt = $('#sess-sort option[value=cost]');
  if (opt) opt.textContent = sub ? 'Costliest first (at API prices)' : 'Costliest first';
}

async function loadSessions() {
  const body = clear($('#sessions-body'));
  const wide = showScopeCol('[data-testid=sessions-table]', wideScope());
  const cols = wide ? 15 : 14;
  loadingRows(body, cols);
  // Each load takes a number: a limits answer that lands after a newer load must not repaint it.
  const seq = (state.sessSeq = (state.sessSeq || 0) + 1);
  const subP = onSubscription(); // asked beside the sessions, never before them
  try {
    const { sessions, total } = await api('sessions', { limit: 25, offset: state.sessOffset, sort: state.sessSort === 'cost' ? 'cost' : '' });
    // The table paints on the sessions answer, in the last known view. A slow or failed
    // /api/me/limits never holds it back: when the answer differs, the table paints again.
    const paint = (sub) => {
      state.sessSub = sub;
      clear(body);
      sessionHeaders(sub);
      if (!sessions.length) {
        if (activeFilters().length) renderNoMatch(body, cols, 'sessions');
        else {
          tableMessage(body, cols, 'No sessions yet',
            'A session appears as soon as its first request is captured.');
        }
      }
      for (const s of sessions) {
        body.appendChild(el('tr', { class: 'click', onclick: () => { setFilter('session', s.session_id, { quiet: true }); go('requests'); } },
          el('td', {}, el('span', { class: 'trunc', title: s.session_id, text: s.session_id || '(none)' }),
            // A young session has paid for its extraction call and not yet collected the
            // replay, so its net reads underwater and stops doing so as the turns come in.
            // The pill says the amortization is unfinished instead of letting a half-collected
            // figure read as a verdict.
            s.in_flight
              ? el('span', {
                class: 'pill partial', 'data-testid': 'in-flight',
                title: 'This session\'s last request is still inside one provider cache TTL, so '
                     + 'the next turn may replay the same reduction. Its dollar figures are an '
                     + 'incomplete amortization, not a verdict.',
              }, 'in flight')
              : null),
          wide ? el('td', { class: 'narrow-hide' }, el('span', { title: s.tenant_id, text: s.tenant_id ? s.tenant_id.slice(0, 6) : '—' })) : null,
          el('td', { class: 'narrow-hide', text: whenShort(s.start) }),
          el('td', { class: 'num', text: num(s.turns) }),
          sub ? sessionTokenCost(s) : el('td', { class: 'num' },
            usdOrNA(s.cost_usd + s.cg_llm_cost_usd, !!s.baseline_cost_usd, 'no turn in this session reported the provider usage a cost needs')),
          // A dollar figure over a session whose turns are not all priced is a figure with
          // a different denominator from the token columns beside it: it covers the priced
          // turns only. The dagger says so rather than letting the two read as one total.
          // `—` was standing in for an uncomputable value, which DESIGN §3.6 rule 3 rules out the
          // same way it rules out `$0`: neither carries a reason, and a dash reads as "nothing
          // here" rather than "we cannot say". Routed through the same helper as the Components
          // dollar cells so there is one answer to this in the codebase.
          sub ? sessionTokenSaved(s) : el('td', { class: 'num' },
            usdOrNA(s.saved_usd, !!s.baseline_cost_usd,
              'no turn in this session reported the provider usage a baseline cost needs, so there '
              + 'is nothing to subtract an actual cost from'),
            s.baseline_cost_usd && s.incomplete_rows > 0
              ? el('span', {
                class: 'na small',
                title: `${num(s.incomplete_rows)} of this session's ${num(s.turns)} turns have no usable ` +
                       'provider usage data, so this covers the ' + num(s.turns - s.incomplete_rows) +
                       ' priced turns only — the token columns cover all of them.',
              }, ' †')
              : null),
          el('td', { class: 'more', title: s.models || '', text: s.primary_model || firstOf(s.models) }),
          el('td', { class: 'more', text: firstOf(s.agents) }),
          el('td', { class: 'more', text: firstOf(s.presets) }),
          el('td', { class: 'num more', text: compact(s.tokens_before) }),
          // BOTH denominators, in one cell rather than a 16th column, and in the order the
          // <details> above the table explains them. Gross alone was the only savings figure
          // on this row and it overstates by 27.7x overall / up to 572.9x on one session.
          el('td', {
            class: 'num more',
            title: 'gross ' + num(s.saved) + ' tokens: a removed span counted once per turn it '
              + 'was absent from. unique ' + num(s.saved_unique) + ' tokens: the same text '
              + 'counted once. Gross is the size of every request we sent; unique is how much '
              + 'distinct text was dropped.',
          }, compact(s.saved) + ' / ' + compact(s.saved_unique)),
          el('td', { class: 'num more', text: compact(s.cache_read) + ' / ' + compact(s.cache_write) }),
          // The keep-alive's net for this session, from its own ping rows. `n/a` rather than $0
          // where it never ran — a zero would read as "it broke even here", which it did not do —
          // and rather than the `—` that was here, which carried the reason only in a title.
          sub ? sessionTokenKeepAlive(s) : el('td', {
            class: 'num more ' + (s.keepalive_net_usd < 0 ? 'bad-text' : s.keepalive_net_usd > 0 ? 'good-text' : ''),
            title: s.keepalive_pings || s.keepalive_saved_usd
              ? `${num(s.keepalive_pings)} pings cost ${usd(s.keepalive_ping_usd)}; `
                + `${usd(s.keepalive_saved_usd)} of re-creations avoided (a ceiling)`
              : '',
          }, usdOrNA(s.keepalive_net_usd,
            !!(s.keepalive_pings || s.keepalive_saved_usd),
            'your cache strategy did not run on this session, so it neither spent nor avoided anything '
            + 'here — this is an absence, not a break-even')),
          el('td', { class: 'num more', text: num(s.expands) }),
          el('td', { class: 'num more', text: ms(s.cg_latency_ms_avg) }),
          // stopPropagation: the row itself navigates to this session's requests, and a
          // click that does two things is a click that does the wrong one.
          el('td', {}, el('button', {
            class: 'ghost small', 'data-testid': 'session-diff',
            onclick: (ev) => { ev.stopPropagation(); openSessionDiff(s.session_id); },
          }, 'Diff'))));
      }
      const from = total ? state.sessOffset + 1 : 0;
      $('#sess-page').textContent = `${from}–${Math.min(state.sessOffset + 25, total)} of ${num(total)}`;
      $('#sess-prev').disabled = state.sessOffset === 0;
      $('#sess-next').disabled = state.sessOffset + 25 >= total;
    };
    paint(!!state.sessSub);
    subP.then((sub) => { if (seq === state.sessSeq && sub !== state.sessSub) paint(sub); });
  } catch (err) {
    if (aborted(err)) return;
    tableMessage(body, cols, 'Could not load sessions', String(err.message || err), { error: true });
  }
}

// The three Sessions cells that show dollars, in tokens on a subscription (#1023, sessionTokenCells).
function sessionTokenCost(s) {
  const c = sessionTokenCells(s, compact);
  return el('td', {
    class: 'num', 'data-testid': 'session-used-tok',
    title: 'Input tokens this session was billed, by kind: wr cache write, wr1h 1-hour cache write, rd cache read, in fresh input. '
      + num(s.output_tokens) + ' output tokens are not in it. context-guru’s own model calls have no token figure here.',
  }, s.baseline_cost_usd ? (c.cost || '0')
    : usdOrNA(0, false, 'no turn in this session reported the provider usage a cost needs'));
}
function sessionTokenSaved(s) {
  const c = sessionTokenCells(s, compact);
  return el('td', {
    class: 'num', 'data-testid': 'session-saved-tok',
    title: 'Tokens context-guru saved, by kind. "cost" is what the savings made the session pay. '
      + 'Kinds are never added: Anthropic does not say how the limit weights them. The keep-alive is in its own column.',
  }, c.saved,
  c.unmeasured > 0 && c.measured ? el('span', {
    class: 'na small', 'data-testid': 'session-unmeasured',
    title: num(c.unmeasured) + (c.unmeasured === 1 ? ' request' : ' requests') + ' of this session not measured in tokens: '
      + 'from before the token count, or not priced. That is not "saved nothing".',
  }, ' †') : null);
}
function sessionTokenKeepAlive(s) {
  const c = sessionTokenCells(s, compact);
  if (!c.keepaliveRan && !c.pings) {
    return el('td', { class: 'num more' }, el('span', {
      class: 'na', text: 'n/a',
      title: 'your cache strategy did not run on this session, so it neither spent nor avoided anything here',
    }));
  }
  return el('td', {
    class: 'num more', 'data-testid': 'session-keepalive-tok',
    title: num(s.keepalive_pings) + ' pings. The first line is the cache rewrites the pings avoided (a ceiling). '
      + 'The second is what the pings paid: it is on its own line, not in any cost.',
  },
  el('div', { text: c.keepalive }),
  c.pings ? el('div', { class: 'small', 'data-testid': 'session-pings-tok', text: PING_LABEL + ': ' + c.pings }) : null);
}

Object.assign(loaders, { sessions: loadSessions });
