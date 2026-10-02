// The Keep-alive PAGE: a per-session drill-down beside the keep-alive tab's own account-wide
// ledger. One conversation's real timeline, what every available policy would have decided at
// each of its real decision points, the feature values behind those decisions, and what each
// policy would have spent and saved on this session and on the account it belongs to.
//
// One appended file, self-mounting exactly the way kvcache.js documents at its own top: the
// tab, the section and the loader registration all happen here, so this feature is one line in
// the shared page. Every helper it uses is app.js's (el, clear, num, usd, pct, when, dur, tile,
// tileGroup, emptyState, loadingState, tableMessage, api, mountTab) — no second design system,
// and NO stylesheet of its own: every class used here (.tbl, .pill, .banner, .tile) already
// exists in style.css.
//
// THE RULE THIS PAGE EXISTS TO SHOW, NOT JUST COMPUTE: every predictor decision and every
// feature value on this page was built by the server from ONLY the rows up to and including
// the moment being shown — see dash/keepalivepage.go's own top-of-file comment for how that is
// enforced. This file's job is to make that boundary visible: the timeline draws it, the
// feature inspector labels it, and the economics panel never lets a hindsight figure (optimal)
// wear the same styling as a reachable one.
'use strict';

// ── mount ──────────────────────────────────────────────────────────────────
const kapView = mountTab({
  group: 'behaviour', after: 'kvcache', view: 'kapage', label: 'Keep-alive session',
});

// ── local state ────────────────────────────────────────────────────────────
const kap = {
  session: '',
  page: null,     // last GET /api/keepalive/page/session payload
  sessions: null, // last GET /api/keepalive/page/sessions payload
  open: -1,       // index of the expanded decision row, -1 = none
};

/** kapUnreachable is the same ceiling test kvcache.js's own kvBaselineArms uses, against
 *  whichever arm list the current payload carries. */
function kapUnreachable(name, arms) {
  const a = (arms || []).find((x) => x.name === name);
  return !!(a && a.unreachable);
}

/** kapPill renders a small labelled pill, reusing the classes every other tab already has. */
function kapPill(cls, text, title) {
  return el('span', { class: 'pill ' + cls, title: title || null }, text);
}

/** kapCeilingPill marks a hindsight/unreachable figure — the same wording kvcache.js uses for
 *  the identical concept, so a reader who has seen one page recognises the other. */
function kapCeilingPill() {
  return kapPill('missing', 'ceiling — hindsight',
    'Unreachable. This arm is told the real next-request time, so it is the headroom this '
    + 'history has, not an option — no policy can reach it, because no policy knows the future.');
}

// ── the page shell ─────────────────────────────────────────────────────────
function kapRenderShell() {
  clear(kapView);
  kapView.appendChild(el('div', { class: 'section' },
    el('h2', {}, 'Keep-alive session'),
    el('span', { class: 'section-note' },
      'One conversation’s real timeline, what every available policy would have decided '
      + 'at each of its real decision points, and what each would have spent and saved.')));
  kapView.appendChild(el('div', { class: 'kv-controls', id: 'kap-picker' }));
  kapView.appendChild(el('div', { id: 'kap-body' }));
  kapView.appendChild(el('div', { class: 'section' }, el('h2', {}, 'Sessions in this cohort')));
  kapView.appendChild(el('div', { id: 'kap-sessions' }));
}
kapRenderShell();

// ── the session picker ─────────────────────────────────────────────────────
function kapRenderPicker() {
  const host = clear($('#kap-picker'));
  const input = el('input', {
    type: 'text', placeholder: 'session id', value: kap.session,
    'data-testid': 'kap-session-input',
    onkeydown: (ev) => { if (ev.key === 'Enter') { kap.session = input.value.trim(); kapLoadSession(); } },
  });
  host.appendChild(el('label', { class: 'field' }, 'Session ', input));
  host.appendChild(el('button', {
    class: 'btn', 'data-testid': 'kap-session-go',
    onclick: () => { kap.session = input.value.trim(); kapLoadSession(); },
  }, 'Open'));
  if (kap.session) {
    host.appendChild(el('button', {
      class: 'ghost', onclick: () => { kap.session = ''; input.value = ''; kapRenderBody(); },
    }, 'Clear'));
  }
}

// ── the timeline + detail ──────────────────────────────────────────────────

/** kapRenderBody draws everything below the picker: the summary line, the timeline, the
 *  expanded decision's predictors/features, the economics and the ledger-vs-replay panel. */
function kapRenderBody() {
  const host = clear($('#kap-body'));
  if (!kap.session) {
    emptyState(host, 'No session open', 'Type a session id above, or pick one from the cohort list below.');
    return;
  }
  const p = kap.page;
  if (!p) { loadingState(host, 4); return; }
  if (!p.requests) {
    emptyState(host, 'Nothing here', `No requests match "${kap.session}" under the current filters.`);
    return;
  }
  host.appendChild(kapSummary(p));
  host.appendChild(el('div', { class: 'section' }, el('h3', {}, 'Timeline')));
  host.appendChild(kapTimeline(p));
  if (kap.open >= 0 && p.decisions[kap.open]) {
    host.appendChild(kapDetail(p.decisions[kap.open]));
  }
  host.appendChild(el('div', { class: 'section' }, el('h3', {}, 'What each policy would have spent and saved')));
  host.appendChild(kapEconomicsTable('On this session', p.economics && p.economics.session));
  host.appendChild(kapEconomicsTable('On this cohort', p.economics && p.economics.cohort));
  host.appendChild(kapLedgerVsReplay(p.ledger_vs_replay));
}

function kapSummary(p) {
  const tiles = [
    tile('kap-requests', 'Requests', num(p.requests), 'observed', null),
    tile('kap-pings', 'Pings', num(p.pings), 'observed', null),
    tile('kap-models', 'Models', (p.models || []).join(', ') || '—',
      p.multi_model ? 'this session switches model — see the timeline' : 'observed', p.multi_model ? 'warn' : null),
  ];
  const frag = document.createDocumentFragment();
  frag.appendChild(tileGroup('', null, tiles));
  frag.appendChild(el('div', { class: 'banner warn', 'data-testid': 'kap-stats-note' },
    p.session_stats_note));
  return frag;
}

/** kapTimeline draws one row per real decision point: what happened, and — the two boundaries
 *  every predictor and this whole mechanism is judged against — the 280s idle trigger and the
 *  policy's own coverage (K*X + the provider's 5-minute TTL), made visually obvious as a plain
 *  labelled column rather than left for the reader to compute from a raw idle number. */
function kapTimeline(p) {
  const table = el('table', { class: 'tbl', 'data-testid': 'kap-timeline' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'When'), el('th', {}, 'Model'), el('th', {}, 'Stop reason'),
      el('th', { class: 'num' }, 'Since last'), el('th', {}, 'TTL entering'),
      el('th', {}, 'Outcome'), el('th', { class: 'num' }, 'Cost'),
      el('th', {}, 'Credited'), el('th', {}, 'Pings'), el('th', {}, ''))));
  const body = el('tbody');
  table.appendChild(body);
  (p.decisions || []).forEach((d, i) => {
    const idleTrigger = d.since_last_ms / 1000 >= d.idle_trigger_seconds;
    const coverageBreach = d.since_last_ms / 1000 >= d.coverage_seconds;
    body.appendChild(el('tr', {
      class: 'clickable' + (i === kap.open ? ' kap-open' : ''),
      'data-testid': 'kap-row-' + i,
      onclick: () => { kap.open = kap.open === i ? -1 : i; kapRenderBody(); },
    },
      el('td', {}, when(d.ts), d.model_switch ? kapPill('warn', 'model switch',
        'This is the first request on a different model — the entry above cannot transfer to it.') : null),
      el('td', {}, d.model, ' ', el('span', { class: 'section-note' }, 'turn ' + d.turn)),
      el('td', {}, d.stop_reason || '(unset)', ' ', kapPill(
        d.stop_cluster === 'actually_done' ? 'good' : d.stop_cluster === 'still_working' ? 'bad' : 'neutral',
        (d.stop_cluster || '').replace(/_/g, ' '))),
      el('td', { class: 'num' + (idleTrigger ? ' warn-text' : '') },
        d.turn > 1 ? dur(d.since_last_ms) : 'first turn',
        idleTrigger ? ' ⚠' : ''),
      el('td', {}, d.ttl_in_force === 'none' ? 'none' : d.ttl_in_force,
        d.remaining_seconds_at_arrival ? el('div', { class: 'section-note' },
          dur(d.remaining_seconds_at_arrival * 1000) + ' left') : null,
        coverageBreach ? kapPill('warn', 'past coverage',
          'This gap exceeded K×X + the provider’s TTL — no ping schedule at the current '
          + 'policy could have reached it.') : null),
      el('td', {}, d.hit ? kapPill('good', 'hit') : kapPill(d.addressable ? 'bad' : 'neutral', d.miss_reason || 'miss'),
        d.addressable ? kapPill('warn', 'addressable') : null),
      el('td', { class: 'num' }, usd(d.cost_usd)),
      el('td', {}, d.saved_usd_credited > 0
        ? kapPill('good', usd(d.saved_usd_credited))
        : (d.credit_reachable === false ? kapPill('bad', 'unreachable',
          'A credit was recorded on this row but no qualifying ping backs it — see kaSaved’s own note.') : '—')),
      el('td', {}, (d.pings || []).length
        ? kapPill(d.pings.some((pg) => pg.wrote) ? 'warn' : 'good', d.pings.length + ' ping' + (d.pings.length > 1 ? 's' : ''),
          d.pings.map((pg) => (pg.wrote ? 'rewrote (arrived after lapse)' : pg.read_nothing ? 'read nothing' : 'refreshed')
            + ' — ' + usd(pg.cost_usd)).join('; '))
        : ''),
      el('td', {}, i === kap.open ? '▴ hide' : '▾ features & predictors')));
  });
  return table;
}

/** kapDetail is the feature inspector + side-by-side predictors for one decision point. */
function kapDetail(d) {
  const frag = document.createDocumentFragment();
  frag.appendChild(el('div', { class: 'section' }, el('h3', {}, 'What every predictor would have decided'),
    el('span', { class: 'section-note' }, 'At ' + when(d.ts) + ' — using only what was knowable then.')));
  const pt = el('table', { class: 'tbl', 'data-testid': 'kap-predictors' },
    el('thead', {}, el('tr', {}, el('th', {}, 'Policy'), el('th', {}, 'Would do'), el('th', {}, ''))));
  const pb = el('tbody');
  pt.appendChild(pb);
  (d.predictors || []).forEach((pr) => {
    pb.appendChild(el('tr', { class: pr.unreachable ? 'kv-ceiling' : '' },
      el('td', {}, pr.name, pr.unreachable ? ' ' : null),
      el('td', {}, (pr.action || '').replace(/_/g, ' ') || 'error'),
      el('td', {}, pr.unreachable ? kapCeilingPill() : null,
        pr.description ? el('span', { class: 'section-note', title: pr.description }, ' ℹ') : null)));
  });
  frag.appendChild(pt);

  frag.appendChild(el('div', { class: 'section' }, el('h3', {}, 'Feature inspector'),
    el('span', { class: 'section-note' },
      'Every feature a predictor could use, whether it was available here, and where it comes from.')));
  const ft = el('table', { class: 'tbl', 'data-testid': 'kap-features' },
    el('thead', {}, el('tr', {}, el('th', {}, 'Feature'), el('th', {}, 'Value'), el('th', {}, 'Availability'),
      el('th', {}, 'Source'), el('th', {}, 'Explanation'))));
  const fb = el('tbody');
  ft.appendChild(fb);
  (d.features || []).forEach((f) => {
    fb.appendChild(el('tr', { class: f.missing ? 'kap-missing-feature' : '' },
      el('td', {}, f.name),
      el('td', {}, f.missing
        ? kapPill('bad', 'absent', f.missing_reason)
        : (f.value || '—')),
      el('td', {}, kapPill(
        f.availability === 'live' ? 'good' : f.availability === 'offline-derivable' ? 'neutral' : 'warn',
        f.availability)),
      el('td', {}, f.source),
      el('td', {}, f.explanation)));
  });
  frag.appendChild(ft);
  return frag;
}

// ── economics ──────────────────────────────────────────────────────────────

/** kapEconomicsTable reuses the KV-cache tab's own simulation payload shape (Arms/Results/
 *  Savings) — see dash/keepalivepage.go's own comment for why this is the same struct and not
 *  a reshaped copy of it. */
function kapEconomicsTable(label, sim) {
  const wrap = el('div', {});
  wrap.appendChild(el('h4', {}, label));
  if (!sim || !sim.results || !sim.results.length) {
    emptyState(wrap, 'Nothing to compare', 'No priced traffic in scope.');
    return wrap;
  }
  const savings = {};
  (sim.savings || []).forEach((s) => { savings[s.strategy] = s; });
  const table = el('table', { class: 'tbl' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Policy'), el('th', { class: 'num' }, 'Total cost'),
      el('th', { class: 'num' }, 'vs baseline'), el('th', { class: 'num' }, '% vs baseline'),
      el('th', { class: 'num' }, 'Pings'), el('th', {}, ''))));
  const body = el('tbody');
  table.appendChild(body);
  sim.results.forEach((r) => {
    const s = savings[r.strategy] || {};
    const ceiling = kapUnreachable(r.strategy, sim.arms);
    body.appendChild(el('tr', { class: (r.strategy === sim.baseline ? 'kv-baseline' : '') + (ceiling ? ' kv-ceiling' : '') },
      el('td', {}, r.strategy, r.strategy === sim.baseline ? ' (baseline)' : ''),
      el('td', { class: 'num' }, usdOrNA(r.total_usd, r.valued, 'nothing in scope was priced')),
      el('td', { class: 'num' }, usdOrNA(s.absolute_usd, !!s.percent_known, 'no baseline to compare against')),
      el('td', { class: 'num' }, s.percent_known ? pct(s.percent_usd) : '—'),
      el('td', { class: 'num' }, num(r.pings)),
      el('td', {}, ceiling ? kapCeilingPill() : null)));
  });
  wrap.appendChild(table);
  wrap.appendChild(el('p', { class: 'hint' }, 'replayed — an exact replay of this history under each policy, not a forecast.'));
  return wrap;
}

function kapLedgerVsReplay(lvr) {
  if (!lvr) return document.createDocumentFragment();
  const frag = document.createDocumentFragment();
  frag.appendChild(el('div', { class: 'section' }, el('h3', {}, 'Ledger vs. replay'),
    el('span', { class: 'section-note' }, 'Two different, both honest, ways to ask "did keep-alive pay off here".')));
  frag.appendChild(tileGroup('', null, [
    tile('kap-ledger-net', 'Real ledger net', usd(lvr.ledger_net_usd),
      'observed — ' + num(lvr.ledger_pings) + ' real pings', lvr.ledger_net_usd < 0 ? 'bad' : 'good'),
    tile('kap-replay-net', 'Replay net (' + lvr.replay_arm + ')',
      lvr.replay_known ? usd(lvr.replay_net_usd) : 'not priced',
      'replayed', lvr.replay_known && lvr.replay_net_usd < 0 ? 'bad' : 'good'),
  ]));
  if (lvr.disagree) {
    frag.appendChild(el('div', { class: 'banner warn', 'data-testid': 'kap-ledger-disagree' },
      el('strong', {}, 'These disagree. '), lvr.note));
  } else {
    frag.appendChild(el('p', { class: 'hint' }, lvr.note));
  }
  return frag;
}

// ── the cohort session list ────────────────────────────────────────────────
function kapRenderSessions() {
  const host = clear($('#kap-sessions'));
  const rows = kap.sessions;
  if (!rows) { loadingState(host, 3); return; }
  if (!rows.length) { emptyState(host, 'No sessions', 'Nothing matches the current filters.'); return; }
  const table = el('table', { class: 'tbl', 'data-testid': 'kap-session-list' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Session'), el('th', {}, 'Tenant'), el('th', {}, 'Model'), el('th', {}, 'Agent'),
      el('th', {}, 'Stop reason'), el('th', { class: 'num' }, 'Requests'),
      el('th', { class: 'num' }, 'Addressable'), el('th', { class: 'num' }, 'Addressable $'))));
  const body = el('tbody');
  table.appendChild(body);
  rows.forEach((s) => {
    body.appendChild(el('tr', {
      class: 'clickable', onclick: () => { kap.session = s.session_id; kapLoadSession(); kapRenderPicker(); },
    },
      el('td', {}, s.session_id), el('td', {}, s.tenant_id), el('td', {}, s.model),
      el('td', {}, s.agent), el('td', {}, s.stop_reason || '(unset)'),
      el('td', { class: 'num' }, num(s.requests)),
      el('td', { class: 'num' }, num(s.addressable_misses)),
      el('td', { class: 'num' }, usd(s.addressable_usd))));
  });
  host.appendChild(table);
}

// ── loading ────────────────────────────────────────────────────────────────
async function kapLoadSession() {
  kap.open = -1;
  if (!kap.session) { kapRenderBody(); return; }
  kapRenderBody();
  try {
    kap.page = await api('keepalive/page/session', { session: kap.session });
  } catch (err) {
    if (aborted(err)) return;
    errorState($('#kap-body'), 'Could not load this session', err);
    return;
  }
  kapRenderBody();
}

async function kapLoadSessions() {
  kapRenderSessions();
  try {
    kap.sessions = (await api('keepalive/page/sessions')).sessions;
  } catch (err) {
    if (aborted(err)) return;
    errorState($('#kap-sessions'), 'Could not load the cohort list', err);
    return;
  }
  kapRenderSessions();
}

async function loadKAPage() {
  kapRenderPicker();
  kapLoadSessions();
  kapLoadSession();
}

// Registered here, so mounting this whole view is one line in the shared page.
Object.assign(loaders, { kapage: loadKAPage });
