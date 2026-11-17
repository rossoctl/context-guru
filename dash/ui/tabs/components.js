// context-guru dashboard: the Preset steps tab (per-component table and compaction episodes).
// Moved out of app.js (#1089). app.js loads first and holds the shared DOM helpers,
// formatting, state and router that this file uses. See the file list in index.html.
'use strict';

// ── components ─────────────────────────────────────────────────────────────
/**
 * ERROR_RATE_BAD is the rate at which a component's own failures outrank its economics.
 *
 * There used to be no threshold at all: `if (c.errors > 0)` sat above every economic test, and
 * because fail-open logs an error for each reverted run, EVERY component in a real window has
 * some. On the 15,900-request corpus the worst rate is 0.39% (toolschema, 9 of 2,314) and all
 * 14 rows rendered the red `errors` pill — so on a deployment saving $174 no component could
 * ever read "earning its place", and extract_llm's $2.15 loss was indistinguishable from
 * dedup's $66 gain. 5% is where "this component is broken" becomes the headline; below it the
 * rate is noise and belongs in the Errors column, with its denominator, not in the verdict.
 */
const ERROR_RATE_BAD = 0.05;

/** errRate is errors per run — the only form in which an error count is a finding. */
function errRate(c) { return c.runs > 0 ? (c.errors || 0) / c.runs : 0; }

/**
 * actedStructuralOnly is a component that mutated requests without removing any content.
 *
 * cachesplit is the whole reason this predicate exists: its job is WHERE the cache breakpoint
 * goes, so `acted_tokens` and `saved_unique` are 0 by construction and every content-shaped
 * test below reads it as inert. Its dollars are recorded per REQUEST (`cachesplit_saved_usd`,
 * $60.33 on this window from /api/stats) and never reach /api/components at all, so this
 * column cannot judge it and must not try.
 */
function actedStructuralOnly(c) {
  return (c.acted_structural || 0) > 0 && (c.acted_tokens || 0) === 0;
}

/**
 * verdict summarises whether a component earns its place, from what it saved against what it
 * cost. The precedence, in order, and every step of it was a defect once:
 *
 *   1. never ran            — nothing to judge.
 *   2. error RATE over 5%   — a component failing on one request in twenty is worse news than
 *                             any dollar figure. Thresholded, and stated WITH the rate; an
 *                             unthresholded bare `errors` here defeated steps 3-5 for all 14
 *                             components.
 *   3. DOLLARS              — a component that makes LLM calls is the only kind that can be
 *                             net-negative, and that is the most valuable thing this column
 *                             can say. It comes before latency so extract_llm's 1h 2m of
 *                             hot-path time for $0.09 of savings reads as `underwater -$2.15`
 *                             rather than as a bland "inert here".
 *   4. STRUCTURAL           — before every content test, because "removed no tokens" is
 *                             cachesplit's normal state, not a failure. Reversed, this branch
 *                             was unreachable and the third-largest saving on the deployment
 *                             read `costly and inert`.
 *   5. content economics    — latency against tokens, then act rate.
 */
function verdict(c) {
  if (c.runs === 0) return ['—', 'neutral'];
  if (errRate(c) >= ERROR_RATE_BAD) {
    return ['errors on ' + pct(errRate(c) * 100, 1) + ' of runs', 'missing'];
  }
  // DOLLARS FIRST, where there are any. A component that makes LLM calls is the only kind
  // that can be net-negative, and until the per-call records existed this function had no
  // money to reason with — so it judged the one component that can lose money on tokens and
  // latency, and could describe a $3 loss as "expensive for its yield".
  if (c.llm_calls > 0) {
    // c.net_usd is the SERVER's figure: saved_usd (summed per turn, so the amortization of
    // a frozen reduction replaying across a session is already in it) minus llm_cost_usd.
    // The browser used to compute this from a hardcoded rate table AND from calls-only
    // savings, which both mispriced it and threw away ~93% of the realized value.
    // The figure the TABLE shows, estimate included. Judging a component's spend against the
    // six rows of its saving that happen to postdate a column is not a verdict.
    const net = c.net_usd_with_estimate || 0;
    if (net < -0.01) return ['underwater ' + usd(net), 'missing'];
    if (net <= 0) return ['break-even', 'partial'];
    return ['net ' + usd(net), 'complete'];
  }
  // Consulted BEFORE the latency test below, which is what "costly and inert" is: that test
  // asks "spent time, removed nothing", and removing nothing is this component's job.
  if (actedStructuralOnly(c)) {
    return [num(c.acted_structural) + ' structural mutations', 'partial'];
  }
  // Spent >1s of hot-path time and returned nothing: paid for, unused.
  if (c.saved_unique === 0 && c.duration_ms_total > 1000) return ['costly and inert', 'missing'];
  if (c.mutated === 0) return ['inert here', 'partial'];
  if (c.saved_unique === 0) return ['mutates, saves no content', 'neutral'];
  // More than a millisecond of latency per 100 tokens saved.
  if (c.duration_ms_total > 1000 && c.duration_ms_total / c.saved_unique > 0.01) {
    return ['expensive for its yield', 'partial'];
  }
  // Both halves, or a component that only ever moves a cache breakpoint is "rarely fires"
  // on every workload — which is what cachesplit read, on 1,805 requests it mutated.
  if (c.act_rate + (c.act_rate_structural || 0) < 0.02) return ['rarely fires', 'partial'];
  return ['earning its place', 'complete'];
}

/**
 * activitySummary renders what a component DID above what it turned away.
 *
 * Both, because this cell used to show declines only and was therefore blind in proportion to how
 * well a component was working: the names a component raises when it SUCCEEDS are the ones that live
 * in `events`. Observed on real traffic — a turn that adjudicated twelve outputs and removed twelve
 * showed an empty cell, while a turn that refused everything showed a full one.
 *
 * Events lead, because "what happened" is the question a reader opens this cell with; declines
 * answer "why did nothing happen", which is only interesting once the first is empty.
 *
 * Kept in ONE cell rather than adding a column: the column is intentionally unsortable (null in
 * COMPONENT_SORT), and a new one would have to be threaded through that array and both header rows
 * for no gain a reader can use. They stay labelled and on separate lines, so a success is never
 * mistaken for a refusal — which is the whole point of having split them.
 */
/**
 * The sweep's quality and money, beside its activity (#404). Only the sweep's row carries the fields.
 * The FALSE-DROP RATE is the number to pull the sweep from xhigh on: drops a later turn used without
 * expanding them, plus drops the agent expanded back, over the drops applied. The expand charge is
 * already taken off the credit and the net; the answer cost is its own figure and is NOT taken off a
 * second time (the net already carries the model spend).
 */
function sweepQualityLine(c) {
  if (!c || !c.sweep_applied) return null;
  const rate = (c.sweep_false_drop_rate || 0) * 100;
  return el('div', {
    class: rate >= 15 ? 'warn-text' : 'did',
    title: 'Drops applied: ' + num(c.sweep_applied) + '. Needed again: ' + num(c.sweep_false_drops)
      + ' (a later turn used them, or the agent expanded them back). Expands: ' + num(c.sweep_expands)
      + ' (' + compact(c.sweep_expand_tokens) + ' tok), charged to the sweep: ' + usd(c.sweep_expand_charge_usd || 0)
      + ', already off the saving. Answer cost: ' + usd(c.sweep_answer_usd || 0)
      + ', booked once on the ping row the question replaced.',
    text: 'false drops ' + rate.toFixed(1) + '% (' + num(c.sweep_false_drops) + ' of ' + num(c.sweep_applied) + ')',
  });
}

function activitySummary(gates, events) {
  const parts = [];
  const add = (label, m, cls) => {
    if (!m) return;
    const all = Object.entries(m).sort((a, b) => b[1] - a[1]);
    if (!all.length) return;
    const shown = all.slice(0, 2).map(([k, v]) => reasonText(k) + ' ' + num(v)).join(' · ');
    const rest = all.length > 2 ? ' +' + (all.length - 2) : '';
    parts.push(el('div', {
      class: cls,
      title: label + '\n' + all.map(([k, v]) => reasonText(k) + ' ' + num(v) + ' (' + k + ')').join('\n'),
      text: shown + rest,
    }));
  };
  add('did', events, 'did');
  add('skipped', gates, 's');
  // Absent BOTH maps means the row predates the columns — "unknown", not "nothing happened". An
  // empty pair means the component genuinely recorded neither, which is a dash. Conflating those
  // two is the mistake the gates column's own comment records having made once.
  if (!parts.length) {
    const known = gates || events;
    return el('span', { class: 'na', text: known ? '—' : 'unknown' });
  }
  return el('span', {}, ...parts);
}

/**
 * STEP_INFO: plain name and one-clause job per component; the raw name stays in a title.
 * REASON_TEXT: plain phrases for gate/event codes. An unknown code prints with spaces, never dropped.
 */
const STEP_INFO = {
  cachesplit: ['Cache placement', 'moves the cache breakpoint so more of the prompt is reused'],
  cacheinject: ['Cache marker', 'adds cache markers to the request'],
  cmdfilter: ['Command output trimmer', 'cuts noise out of shell command output'],
  dedup: ['Duplicate remover', 'drops text the prompt already contains'],
  extract: ['Content extractor', 'keeps the relevant part of long tool output'],
  extract_llm: ['Model-based extractor', 'asks a cheap model to keep only what matters in long output'],
  extract_llm_sweep: ['Old-output sweep', 'asks the model which old outputs are spent, and removes them on the next request after the cache went cold'],
  failed_run: ['Failed-run trimmer', 'shortens output from commands that failed'],
  format: ['Format compactor', 'rewrites JSON and similar text in a shorter form'],
  linecap: ['Line limiter', 'caps very long lines and outputs'],
  searchfold: ['Search folder', 'folds repeated search results into one'],
  summarize: ['Chat summarizer', 'replaces old turns with a summary'],
  textclean: ['Text cleaner', 'removes blank space, colour codes and other clutter'],
  toolfilter: ['Tool list filter', 'drops tool definitions the agent is unlikely to use'],
  toolschema: ['Tool schema trimmer', 'shortens tool definitions'],
  toon: ['Compact data format', 'rewrites tabular JSON in a denser format'],
  agent_compaction: ['Agent summary', "the agent's own summary of its earlier turns"],
};
function stepInfo(name) {
  // An unknown step shows its name and no description: an empty hint reads better than "no description recorded".
  return STEP_INFO[name] || [String(name).replace(/_/g, ' '), ''];
}
const REASON_TEXT = {
  below_min_size: 'text too short to bother', below_min_tokens: 'text too short to bother',
  below_max_tokens: 'prompt still small enough', below_output_floor: 'output too short to bother',
  below_request_trigger: 'prompt not big enough yet', request_trigger_not_fired: 'trigger not reached',
  not_frozen: 'was not in the cached part', already_set: 'already set',
  already_clean: 'already clean', already_compact: 'already compact', already_dense: 'already dense',
  offloaded: 'moved out to storage', break_even: 'would not pay for itself',
  economic_gate: 'not worth the cost', economic_gate_advisory: 'not worth the cost',
  no_filter_match: 'no rule matched', filter_matched_no_change: 'rule matched, nothing to cut',
  no_model: 'no model known', not_json_shaped: 'not JSON', marker_present: 'already marked',
  empty: 'nothing there', cached_prefix: 'inside the cached part', reapplied_same_session: 'applied again in the same session',
  sweep_dropped: 'old output dropped', sweep_kept: 'old output kept', sweep_offered: 'offered for removal',
  sweep_applied_cold: 'dropped on a cold request', sweep_applied_changed_prefix: 'dropped after a changed prefix',
  sweep_applied_warm: 'dropped on a warm request (gate passed)', sweep_false_ref_warm: 'warm drop, then used by a later turn',
  sweep_warm_gate_accept: 'warm gate: applied', sweep_warm_removed_tokens: 'warm sweep: tokens removed (billed)', sweep_warm_rewrite_tokens: 'warm sweep: tokens rewritten (billed)', sweep_warm_expected_turns: 'warm sweep: warm turns the gate expected', sweep_warm_deep: 'warm sweep: drop deeper than the look-back (whole conversation rewritten)', sweep_warm_refused_removed_tokens: 'warm sweep refused: tokens it would remove (billed)', sweep_warm_refused_rewrite_tokens: 'warm sweep refused: tokens it would rewrite (billed)', sweep_warm_refused_expected_turns: 'warm sweep refused: warm turns the gate expected', sweep_warm_refused_deep: 'warm sweep refused: drop deeper than the look-back', sweep_warm_too_new: 'too new for a warm question',
  sweep_false_ref: 'dropped, then used by a later turn', sweep_apply_declined: 'drop refused (kept in full)',
  sweep_adjudicated: 'judged by the model', char_window: 'cut to a character window',
  over_per_request_cap: 'over the per-request limit', over_per_session_cap: 'over the per-session limit',
  non_text_blocks: 'not plain text', no_repeated_prefix: 'no repeated start',
};
function reasonText(k) {
  return REASON_TEXT[k] || String(k).replace(/_/g, ' ');
}

/**
 * COMPONENT_SORT is one field per column of the components table, in column order. null is
 * a column with nothing orderable in it (the gate summary, the verdict prose).
 */
const COMPONENT_SORT = ['component', 'saved_usd', 'llm_cost_usd', 'net_usd_with_estimate',
  'kind', 'runs', 'acted_tokens', 'acted_structural',
  'act_rate', null, 'reverted',
  'saved_unique', 'saved_gross', 'overcount_ratio', 'duration_ms_total', 'duration_ms_avg',
  'llm_calls',
  'net_usd_first_removal', 'replay_multiple', 'errors', null];

/**
 * renderNetReconcile prints the two verdicts side by side, and the one step between them.
 *
 * It exists because the same component is green here and red in an operator's terminal. The
 * panel names both figures, names which question each answers, and — the part that makes it
 * a reconciliation rather than a disclaimer — prints the CROSS-CHECK: the decomposition is
 * computed by different code from the same rows than the stored figure is, so if the two
 * disagree by more than rounding, one of them is broken and this says so instead of picking
 * a winner.
 */
function renderNetReconcile(components) {
  const host = $('#net-reconcile');
  if (!host) return;
  // Only the components that actually spend money can flip sign, and they are the only ones
  // the reconciliation is interesting for. With none of them present the panel is noise.
  const spenders = components.filter((c) => (c.llm_cost_usd || 0) > 0);
  const priced = components.filter((c) => c.saved_usd_decomposed > 0);
  host.hidden = !priced.length;
  if (host.hidden) return;
  clear(host);
  host.appendChild(el('h2', {}, 'What did each step save, net of what it cost?'));

  const decomp = priced.reduce((n, c) => n + c.saved_usd_decomposed, 0);
  const first = priced.reduce((n, c) => n + c.saved_usd_first_removal, 0);
  const replay = priced.reduce((n, c) => n + c.saved_usd_replay, 0);
  const llm = components.reduce((n, c) => n + (c.llm_cost_usd || 0), 0);

  host.appendChild(el('p', { class: 'note' },
    'Dollars saved by all steps, minus what the steps themselves spent on model calls. '
    + 'The top bar counts each removed piece of text once (the cautious figure). The bottom bar also '
    + 'counts the later turns it stayed removed for. Both are real; they answer different questions.'));
  const tech = el('details', { class: 'why', 'data-testid': 'net-technical' }, el('summary', {}, 'Technical details'));

  // The part of the "credited once" figure that is not a dedup measurement at all.
  //
  // MarkUnique returns a component's saving in full when it reports no content fingerprint, and
  // only Offload components ever report one — so for a reformatter `unique` equals `gross` on
  // every turn by construction. That puts its whole saving at the cache-WRITE rate with a
  // replay multiple of exactly 1.00, and on measured traffic it is 77% of the bar below. It is
  // not repriced here, because "these are surely replays" is an inference and not a
  // measurement either; it is disclosed, which is what the page can honestly do about it.
  const unkeyed = priced.filter((c) => uniqueState(c) === 'fabricated');
  const unkeyedUSD = unkeyed.reduce((n, c) => n + c.saved_usd_first_removal, 0);
  // The states that are not confident either way. Reported separately and not added to the
  // figure above: "we cannot tell" is a different claim from "this is not a measurement", and
  // over a narrow window (a single session, an hour) it is the usual answer.
  const unsure = priced.filter((c) => uniqueState(c) === 'unknown');
  const unsureUSD = unsure.reduce((n, c) => n + c.saved_usd_first_removal, 0);
  barRows($('#net-reconcile-bars') || host.appendChild(el('div', { id: 'net-reconcile-bars' })), [
    { label: 'Counting each removal once', value: first, max: Math.max(decomp, 0.0001),
      display: usd(first),
      formula: 'minus ' + usd(llm) + ' of our own LLM spend = ' + usd(first - llm) + ' net',
      term: 'Each piece of removed content valued ONCE, at the cache-write rate it would have '
        + 'entered the prompt at. This answers "did the work of removing it pay for itself?" '
        + "It is the figure the proxy's own /stats endpoint reports, and it is the "
        + 'conservative one.' },
    { label: 'Counting every turn it stayed removed', value: decomp, max: Math.max(decomp, 0.0001),
      color: SERIES_SAVED, display: usd(decomp),
      formula: 'minus ' + usd(llm) + ' of our own LLM spend = ' + usd(decomp - llm) + ' net',
      term: 'The same removals, plus ' + usd(replay) + ' for every later turn they were still '
        + 'absent from. The agent re-sends its whole transcript each turn, so content removed '
        + 'at turn 5 is genuinely still absent at turns 6, 7, 8… and each of those turns was '
        + 'billed less because of it. This answers "has this component paid for itself across '
        + 'the sessions it ran in?" It is the figure the tiles on this page show.' },
  ]);

  if (unkeyedUSD > 0) {
    tech.appendChild(el('div', { class: 'state blocked', 'data-testid': 'net-unkeyed' },
      el('div', { class: 'state-body' },
        el('strong', {}, usd(unkeyedUSD) + ' of the "credited once" figure — '
          + pct((100 * unkeyedUSD) / Math.max(first, 1e-12), 0)
          + ' — is not a deduplicated measurement'),
        el('span', {}, unkeyed.map((c) => c.component).join(', ')
          + ' report no content fingerprint, so nothing can be deduplicated against and their '
          + '"credited once" figure is simply their whole saving, re-counted every turn.'),
        el('span', {}, 'That places all of it at the cache-write rate — 12.5× a cache read — '
          + 'and gives them a replay multiple of exactly 1.00. Since the agent re-sends the '
          + 'original bytes each turn, most of it is very probably replay that belongs at the '
          + 'read rate, which would make these figures SMALLER.'),
        el('span', {}, 'It is shown rather than adjusted: repricing on an inference would be '
          + 'the same mistake pointing the other way. These components need a '
          + 'content-derived fingerprint before either number is a measurement.'))));
  }

  if (unsureUSD > 0) {
    tech.appendChild(el('div', { class: 'state', 'data-testid': 'net-unsure' },
      el('div', { class: 'state-body' },
        el('strong', {}, usd(unsureUSD) + ' more cannot be judged in this window'),
        el('span', {}, unsure.map((c) => c.component + ' (' + num(c.unique_diff_rows) + '/'
          + num(c.unique_rows) + ' rows deduped)').join(', ')),
        el('span', {}, 'Too few rows to tell either way. Widen the time range to resolve it — '
          + 'on a single session or a single hour this is the normal answer, and it is reported '
          + 'rather than rounded to "measured" or to "fabricated".'))));
  }

  // The sign flip, named, for every component where it happens. This is the whole point.
  const flips = spenders.filter((c) => (c.net_usd_with_estimate > 0) !== (c.net_usd_first_removal > 0));
  if (flips.length) {
    const box = el('div', { class: 'state', 'data-testid': 'net-flip' },
      el('div', { class: 'state-body' },
        el('strong', {}, flips.length === 1 ? 'One component changes verdict between the two'
          : flips.length + ' components change verdict between the two')));
    for (const c of flips) {
      box.querySelector('.state-body').appendChild(el('span', {},
        c.component + ': ' + usd(c.net_usd_with_estimate) + ' amortized, but '
        + usd(c.net_usd_first_removal) + ' counted once — it spends ' + usd(c.llm_cost_usd)
        + ' and its first removals are only worth ' + usd(c.saved_usd_first_removal)
        + '. It is only ahead because those removals kept paying off on '
        + c.replay_multiple.toFixed(1) + '× as much later traffic. Both are true. Which one '
        + 'you should act on depends on whether that later traffic is going to keep happening.'));
    }
    tech.appendChild(box);
  }

  // The cross-check, and an honest account of how much of the figure it actually checks.
  //
  // Only rows carrying a STORED saved_usd are checked at all. For a row whose saved_usd is 0
  // the stored side is supplied by the read-time estimator, which runs the IDENTICAL formula as
  // the decomposition — so those rows agree by arithmetic, not by corroboration. Presenting the
  // whole comparison as "evidence, not a tautology" was wrong: on production almost every row
  // is in the estimated bucket, so the sentence was close to always false where it mattered.
  const stored = components.reduce((n, c) => n + (c.saved_usd || 0), 0);
  const stDecomp = priced.reduce((n, c) => n + (c.saved_usd_decomposed_stored || 0), 0);
  const covered = decomp > 0 ? stDecomp / decomp : 0;
  const drift = stored > 0 ? Math.abs(stDecomp - stored) / stored : 0;
  const coverage = 'Genuinely cross-checked: ' + usd(stDecomp) + ' of ' + usd(decomp) + ' ('
    + pct(covered * 100, 0) + '). The rest is valued on read by the same formula the '
    + 'decomposition uses, so it agrees by arithmetic and corroborates nothing.';
  host.appendChild(tech);
  tech.appendChild(el('p', { class: 'hint' + (drift > 0.02 ? ' warn-text' : '') },
    !stored
      ? 'Nothing here can be cross-checked: no row in this window carries a recorded '
        + 'per-component dollar figure, so every figure above is valued on read.'
      : drift > 0.02
        // A gap is most often a RATE CHANGE, not an arithmetic error — the stored side was
        // priced when each request ran and the decomposition prices on read. The sibling
        // tier-reconcile tile has always said so; this one used to declare a bug instead.
        ? 'Cross-check DISAGREES by ' + pct(drift * 100, 1) + ': ' + usd(stDecomp)
          + ' decomposed against ' + usd(stored) + ' recorded, on the same rows. The likeliest '
          + 'cause is a RATE CHANGE inside this window — the recorded figure was priced when '
          + 'each request ran, this one is priced at today\'s rates — which is a real event on '
          + 'this deployment, not an error. Check the "At today\'s rates" tile on Overview '
          + 'first; only if that reads clean is one of these two computations wrong. ' + coverage
        : 'Cross-check: ' + usd(stDecomp) + ' decomposed against ' + usd(stored)
          + ' recorded on the same rows — different code, so their agreement is evidence. '
          + coverage));
}

/**
 * llmCostGap is the fractional disagreement between the two prices recorded for the same
 * model calls: the gateway's rate list, and the component's own. Zero when only one of them
 * exists. 2% is the threshold for marking the cell — below that it is rounding.
 */
function llmCostGap(c) {
  const g = c.llm_cost_gateway_usd || 0, r = c.llm_cost_reported_usd || 0;
  if (g <= 0 || r <= 0) return 0;
  return Math.abs(g - r) / g > 0.02 ? (r - g) / g : 0;
}

function llmCostTitle(c) {
  const gap = llmCostGap(c);
  const base = 'Priced from the gateway rate list this deployment bills against — the same '
    + 'source as "Our own LLM cost" on Overview, so the two tabs agree.';
  if (!gap) return base;
  return base + ' NOTE: ' + c.component + ' priced these same calls at '
    + usd(c.llm_cost_reported_usd) + ' itself, which is ' + pct(Math.abs(gap) * 100, 0)
    + (gap > 0 ? ' HIGHER' : ' LOWER') + ' than the gateway\'s ' + usd(c.llm_cost_gateway_usd)
    + '. The gateway figure is shown because it is what the invoice is denominated in.';
}

/**
 * uniqueState is the three-way answer to "is this component's `unique` a measurement?".
 *
 * A boolean could not say it: below the row floor "nothing deduped" is not a finding, and 98.2%
 * of per-session windows are below it — so a session drawer rendered a fabricated figure exactly
 * like a real one. That third state is "we cannot tell yet", and it is the common answer on any
 * narrow window.
 *
 * There is deliberately NO "partly measured" state keyed on the PROPORTION of rows that deduped.
 * A row where unique equals gross is the normal first sighting of new content, so every healthy
 * component has some — `differ < rows` is true of all of them, and treating it as partial
 * fabrication would put a permanent caveat on correct figures. Any dedup at all means the
 * component reports content keys, which is the thing being tested.
 */
function uniqueState(c) {
  const rows = c.unique_rows || 0;
  if (!rows) return 'none';
  if ((c.unique_diff_rows || 0) > 0) return 'measured';
  return c.unique_unkeyed ? 'fabricated' : 'unknown';
}

const UNIQUE_NOTE = {
  fabricated: 'NOT a deduplicated figure: no row in this window deduped, so every turn\'s saving '
    + 'is counted as new. Equal to gross by construction.',
  unknown: 'Too few rows to tell. Nothing here deduped, but that is not yet evidence either '
    + 'way — widen the range to find out.',
  measured: 'content that genuinely never reached the provider, deduped by content key',
  none: '',
};
const UNIQUE_MARK = { fabricated: '*', unknown: '?', measured: '', none: '' };

/** compSaved is a component's dollar saving over the window: the figure stored per request
 *  plus the read-time valuation of the rows written before that column existed. Added here
 *  rather than server-side so the two halves stay separable in the API — and the tooltip on
 *  every cell says how much of it is which. */
function compSaved(c) { return (c.saved_usd || 0) + (c.saved_usd_estimated || 0); }

/**
 * savedKnown is whether this component's dollar saving is a MEASUREMENT rather than an
 * unpriceable or inapplicable zero. Three cases, and only the last is a real $0:
 *
 *   - a non-zero figure                  → measured.
 *   - zero with unpriceable rows         → the rows exist and could not be valued. searchfold
 *                                          removed 1.76M tokens over 488 acted rows, every one
 *                                          of them on a model with no price, and the cell read
 *                                          `$0`.
 *   - zero on a structural-only component→ this column does not measure what it does at all
 *                                          (see actedStructuralOnly). `$0` there asserts
 *                                          "saved nothing" about the third-largest saving on
 *                                          the deployment.
 *   - zero, priced, and it did act        → a genuine $0.
 */
function savedKnown(c) {
  if (compSaved(c) !== 0) return true;
  if (c.saved_usd_unpriced_rows > 0) return false;
  return !actedStructuralOnly(c);
}

/** savedNAReason is why the saving is not a number, in the words the reader needs. */
function savedNAReason(c) {
  if (actedStructuralOnly(c)) {
    return c.component + ' removes no content, so this column cannot value it. It mutated '
      + num(c.acted_structural) + ' requests structurally — cache placement — and its saving is '
      + 'recorded per request instead, as the prefix-cache figure on Overview.';
  }
  return num(c.saved_usd_unpriced_rows) + ' of this component\u2019s acted rows ran on a model '
    + 'with no entry in the operator\u2019s price list, or on a request the accountant could not price '
    + '(see acc_fallback in the proxy log), so their saving could not be valued. '
    + 'It is missing from this figure, not zero.';
}

/**
 * savedTitle names which parts of the figure are recorded, valued on read, and unpriceable.
 *
 * The unpriced clause used to be NESTED inside the `saved_usd_estimated` branch, and that field
 * is 0 across this entire corpus — so the one disclosure that says "some of these rows could
 * not be priced at all" was unreachable on every row that needed it. It is now a clause of its
 * own, appended whenever there are unpriced rows, whether or not anything was estimated.
 */
function savedTitle(c) {
  const parts = [c.saved_usd_estimated
    ? usd(c.saved_usd) + ' recorded + ' + usd(c.saved_usd_estimated) + ' valued on read over '
      + num(c.saved_usd_estimated_rows) + ' rows written before the column existed'
    : usd(c.saved_usd) + ' recorded per request'];
  if (c.saved_usd_unpriced_rows) {
    parts.push(num(c.saved_usd_unpriced_rows) + ' row'
      + (c.saved_usd_unpriced_rows === 1 ? '' : 's') + ' could not be priced at all — no price for the model they '
      + 'ran on, or the request fell back (see acc_fallback in the proxy log) — so their saving is MISSING from '
      + 'this figure, not zero');
  }
  return parts.join(' · ');
}

/** partly lists components with their unpriced row count, for the note above the table. */
function partly(cs) {
  return cs.map((c) => c.component + ' (' + num(c.saved_usd_unpriced_rows) + ' of '
    + num(c.acted_tokens) + ' acted rows)').join(', ');
}

/**
 * renderUnpricedNote is the ONE explanation a column of `n/a` is allowed to have (DESIGN §3.6
 * rule 5) — the per-cell reasons are for detail, not for the fact that unpriced rows exist.
 *
 * It is level-1 visible rather than behind a `<details>`, because a missing denominator is a
 * CAVEAT and disclosure hides derivation, never caveats (DESIGN §3.3).
 */
function renderUnpricedNote(components) {
  const host = $('#components-unpriced');
  if (!host) return;
  clear(host);
  const unpriced = components.filter((c) => c.saved_usd_unpriced_rows > 0);
  const structural = components.filter((c) => actedStructuralOnly(c));
  if (!unpriced.length && !structural.length) { host.hidden = true; return; }
  host.hidden = false;
  if (unpriced.length) {
    const rows = unpriced.reduce((n, c) => n + c.saved_usd_unpriced_rows, 0);
    // Two different outcomes and they must not be conflated. A component with SOME unpriceable
    // rows still shows a figure — it is a lower bound, and calling it n/a would throw away a
    // real measurement. A component with no priceable row at all has no figure to show.
    const partial = unpriced.filter((c) => savedKnown(c));
    const total = unpriced.filter((c) => !savedKnown(c));
    host.appendChild(el('div', {},
      el('strong', {}, num(rows) + ' acted rows across ' + unpriced.length + ' component'
        + (unpriced.length === 1 ? '' : 's') + ' could not be priced'),
      ' — the model they ran on has no entry in the operator’s price list (set MODEL_PRICES to value them), or the '
      + 'accountant could not price the request (the proxy logs acc_fallback with the reason). Either way their '
      + 'saving is absent from the dollar columns rather than zero.'));
    if (partial.length) {
      host.appendChild(el('div', {},
        'Partly unpriced, so the dollar figure shown is a ', el('strong', {}, 'lower bound'),
        ': ' + partly(partial) + '.'));
    }
    if (total.length) {
      host.appendChild(el('div', {},
        'No priceable row at all, so the dollar cells read ', el('span', { class: 'na' }, 'n/a'),
        ' rather than $0: ' + partly(total) + '.'));
    }
  }
  if (structural.length) {
    host.appendChild(el('div', {},
      el('strong', {}, structural.map((c) => c.component).join(', ')
        + (structural.length === 1 ? ' removes' : ' remove') + ' no content'),
      ' — ' + (structural.length === 1 ? 'its' : 'their') + ' work is where the cache breakpoint '
      + 'goes, which this table measures in the Rearranged column and cannot value in dollars. '
      + 'The saving is recorded per request and appears on Overview as the prefix-cache figure.'));
  }
}

/** The fold for steps this page has no description for, kept next to the Preset steps table. */
function renderUndescribedSteps(rows) {
  const table = $('[data-testid=components-table]');
  if (!table) return;
  const wrap = table.closest('.tblwrap') || table;
  let host = $('#components-undescribed');
  if (!host) {
    host = el('details', { class: 'why', id: 'components-undescribed', 'data-testid': 'components-undescribed' });
    wrap.after(host);
  }
  clear(host);
  host.hidden = !rows.length;
  if (!rows.length) return;
  host.appendChild(el('summary', { text: 'Other steps in the history (' + rows.length + '), without a description here' }));
  host.appendChild(el('ul', { class: 'plain-list' }, rows.map((c) => el('li', {},
    el('strong', { text: String(c.component).replace(/_/g, ' ') }), ' ran ' + num(c.runs) + ' times and removed ' + compact(c.saved_unique) + ' tokens'))));
}

async function loadComponents() {
  const body = clear($('#components-body'));
  loadingRows(body, COMPONENT_SORT.length);
  try {
    const { components: raw } = await api('components');
    // /api/components has no LIMIT — every component that ran in the window is in this
    // array — so sorting it here IS a global sort, not a sort of one page.
    const all = state.sort ? sortRows(raw, state.sort, state.dir) : raw;
    // Steps the page can describe are listed; any other name in the history (a step that has since left the
    // product, say) goes into a fold under the table so no recorded figure is dropped, and the table is not
    // padded with rows nobody can explain.
    const components = all.filter((c) => STEP_INFO[c.component]);
    const undescribed = all.filter((c) => !STEP_INFO[c.component]);
    renderUndescribedSteps(undescribed);
    syncSortHeads('[data-testid=components-table]', COMPONENT_SORT);
    clear(body);
    if (!components.length) {
      tableMessage(body, COMPONENT_SORT.length, 'No component runs captured',
        'Run some traffic through the proxy with a non-empty pipeline.');
      emptyState($('#chart-comp'), 'No component data',
        'This chart fills in once a component has saved something.');
      return;
    }
    for (const c of components) {
      const [vtext, vcls] = verdict(c);
      const tr = el('tr', { class: 'click', onclick: () => { setFilter('component', c.component, { quiet: true }); go('requests'); } },
        el('td', { title: c.component }, el('strong', { text: stepInfo(c.component)[0] }),
          stepInfo(c.component)[1] ? el('div', { class: 'hint', text: stepInfo(c.component)[1] }) : null),
        // A cost never travels alone. saved_usd is what this component's removals were
        // worth over the window — summed per turn, so a frozen reduction replaying across a
        // session is already amortized into it — and net_usd is the verdict. Both from the
        // server, priced at the model this deployment actually bills.
        // saved_usd is stored per request; saved_usd_estimated is the same arithmetic applied
        // on READ to the rows written before that column existed. Without the second half this
        // column showed $0.00 for every component over all history predating the last restart
        // — 6 populated rows out of 100,579 on production — and the most-read tab in the
        // dashboard therefore said the product was worthless. The dagger says how much of the
        // figure is valued rather than recorded; it is never silently merged.
        el('td', { class: 'num', title: savedKnown(c) ? savedTitle(c) : '' },
          usdOrNA(compSaved(c), savedKnown(c), savedNAReason(c)),
          savedKnown(c) && c.saved_usd_estimated ? '†' : null),
        // ONE price for our own model calls, on every tab. This cell used to show the
        // component's self-reported figure while Overview showed the proxy's gateway-priced
        // one — 31.6% apart on measured traffic, with nothing saying which to believe. The
        // gateway figure is what the invoice is denominated in, so it is what shows, and the
        // gap is named in the tooltip rather than hidden by the choice.
        el('td', {
          class: 'num',
          title: !c.llm_calls ? 'this component makes no model calls of its own' : llmCostTitle(c),
        }, c.llm_calls ? usd(c.llm_cost_usd) + (llmCostGap(c) ? '‡' : '') : '—'),
        // A net whose benefit half is unknown is unknown too: subtracting a real cost from an
        // unpriceable saving produced `$0` for searchfold and `-$0.00` for anything with a
        // sliver of LLM cost, both of which read as measurements.
        el('td', {
          class: 'num' + (savedKnown(c) && c.net_usd_with_estimate < 0 ? ' warn-text' : ''),
          title: !savedKnown(c) ? '' : 'AMORTIZED verdict: saved ' + usd(compSaved(c))
            + ' across every turn those removals stayed removed for, − own LLM cost '
            + usd(c.llm_cost_usd || 0)
            + '. The per-turn column beside this one is the same verdict without the repeat '
            + 'business, and it can have the opposite sign.',
        }, usdOrNA(c.net_usd_with_estimate, savedKnown(c),
          'the saving half of this net is not a number — ' + savedNAReason(c))),
        el('td', { text: c.kind || '—' }),
        el('td', { class: 'num', text: num(c.runs) }),
        // Two columns, because "acted" meant "removed tokens" and a component whose whole job
        // is where a cache breakpoint goes therefore read 0 acted / 0.0% forever and painted
        // red. cachesplit mutated 1,805 requests on measured traffic and "acted" on none.
        el('td', { class: 'num', text: num(c.acted_tokens) }),
        el('td', {
          class: 'num', title: 'mutated the request without removing tokens — cache placement, '
            + 'markers, breakpoints. Not a failure to act.',
        }, num(c.acted_structural) || '—'),
        el('td', { class: 'num', text: pct(c.act_rate * 100, 1) }),
        // Beside the act rate, not twenty columns to its right. This is the cell that makes a
        // 0.0% act rate falsifiable (docs/dashboard.md), and at 1600px wide it sat at x=2087
        // in a 2628px table — off-screen for the reader who needs it most.
        el('td', {}, activitySummary(c.gates, c.events), sweepQualityLine(c)),
        el('td', { class: 'num', text: num(c.reverted) }),
        // An unkeyed component's `unique` is identically its `gross`, so the cell is marked:
        // without it the column reads as "this component never repeated a removal", which is a
        // measurement it did not make.
        el('td', {
          class: 'num' + (uniqueState(c) === 'measured' || uniqueState(c) === 'none'
            ? '' : ' warn-text'),
          title: UNIQUE_NOTE[uniqueState(c)]
            + (c.unique_rows ? ' (' + num(c.unique_diff_rows) + ' of '
              + num(c.unique_rows) + ' rows deduped)' : ''),
        }, compact(c.saved_unique) + UNIQUE_MARK[uniqueState(c)]),
        el('td', { class: 'num', text: compact(c.saved_gross) }),
        el('td', {
          class: 'num',
          title: uniqueState(c) === 'fabricated'
            ? 'Exactly 1.0 by construction, not by measurement — see the unique column.' : '',
        }, c.overcount_ratio ? c.overcount_ratio.toFixed(1) + '×' : '—'),
        el('td', { class: 'num', text: dur(c.duration_ms_total) }),
        el('td', { class: 'num', text: ms(c.duration_ms_avg) }),
        el('td', { class: 'num', text: c.llm_calls ? num(c.llm_calls) : '—' }),
        // The same verdict with replay excluded — the number /stats reports. It is here
        // BESIDE the amortized one, not instead of it, because the two answer different
        // questions and a component can be genuinely positive on one and negative on the
        // other. Showing only whichever is flattering is the failure mode this column exists
        // to prevent.
        el('td', {
          class: 'num' + (savedKnown(c) && c.net_usd_first_removal < 0 ? ' warn-text' : ''),
          title: !savedKnown(c) ? '' : 'PER-TURN verdict: each piece of content credited ONCE at '
            + 'the rate it would have entered the prompt at ('
            + usd(c.saved_usd_first_removal || 0) + ')'
            + (c.saved_usd_costs ? ' + the costs the component caused (breaks, added tokens) ' + usd(c.saved_usd_costs) : '')
            + ' − own LLM cost ' + usd(c.llm_cost_usd || 0) + '. This is what the proxy\'s /stats endpoint reports.',
        }, c.saved_gross
          ? usdOrNA(c.net_usd_first_removal, savedKnown(c),
            'the saving half of this net is not a number — ' + savedNAReason(c))
          : '—'),
        el('td', {
          class: 'num',
          title: c.replay_multiple > 1
            ? usd(c.saved_usd_replay || 0) + ' of the amortized figure is repeat business, at '
              + 'the cache-read rate. Compare the Overcount column: far more tokens are '
              + 're-removed than this multiple suggests, because each repeat is worth about a '
              + 'tenth of a first removal.'
            : 'Every removal this component made was of content it had not removed before, so '
              + 'there is no repeat business to amortize.',
        }, c.replay_multiple ? c.replay_multiple.toFixed(1) + '×' : '—'),
        // The count AND its rate. The verdict column no longer shouts `errors` below the 5%
        // threshold (see ERROR_RATE_BAD), so this is where a sub-threshold rate stays visible —
        // and a bare count over an unstated denominator was never a finding anyway: 41 errors
        // means nothing until you know it is 41 of 14,057.
        el('td', {
          class: 'num' + (errRate(c) >= ERROR_RATE_BAD ? ' warn-text' : ''),
          title: c.errors
            ? num(c.errors) + ' of ' + num(c.runs) + ' runs = ' + pct(errRate(c) * 100, 2)
              + '. Fail-open reverts the component and logs one of these per affected run, so a '
              + 'low rate is the mechanism working, not a defect. The verdict column names '
              + 'errors only above ' + pct(ERROR_RATE_BAD * 100, 0) + '.'
            : 'no run of this component failed in this window',
        }, c.errors ? num(c.errors) + ' (' + pct(errRate(c) * 100, 2) + ')' : '0'),
        el('td', {}, el('span', { class: 'pill ' + vcls, text: vtext })));
      [...tr.children].slice(4).forEach((td) => td.classList.add('more'));
      body.appendChild(tr);
    }
    renderUnpricedNote(components);
    renderNetReconcile(components);
    // One measure (unique tokens saved) across up to twelve components: a magnitude
    // comparison, so ONE hue. Colouring bar N by N implied each component was a
    // different series and repainted them all whenever a filter changed the order.
    const top = components.filter((c) => c.saved_unique > 0).slice(0, 12);
    barRows($('#chart-comp'), top.map((c) => ({
      label: stepInfo(c.component)[0], value: c.saved_unique, display: compact(c.saved_unique) + ' tok',
      // Figures, not prose, so they go in the always-visible slot rather than behind a
      // disclosure: eight collapsed glyphs would each hide four numbers and cost a line to
      // do it.
      formula: `${num(c.runs)} runs · removed text in ${pct(c.act_rate * 100, 1)} · time added ${dur(c.duration_ms_total)} · ` +
            `${c.overcount_ratio ? c.overcount_ratio.toFixed(1) + '× recounted' : ''}`,
    })), { emptyDetail: 'No component saved any content tokens in this window.' });
  } catch (err) {
    if (aborted(err)) return;
    tableMessage(body, COMPONENT_SORT.length, 'Could not load components', String(err.message || err), { error: true });
  }
}

// ── compaction episodes ────────────────────────────────────────────────
const EPISODE_COLS = 11;

/**
 * Provenance is TWO CLAIMS, not one series, so each row says which it is in words rather than
 * relying on the reader to know that "inferred" is weaker.
 */
const PROVENANCE_NOTE = {
  recorded: 'the component recorded a fresh summary on this turn',
  inferred: 'an older row that acted and carried no replay marker, so it must have paid — '
    + 'deduced, not recorded, and never added to the recorded figures',
};

/**
 * loadCompactionEpisodes renders what each summary earned.
 *
 * Every dollar goes through usdOrNA with the group's own priced flag, because an unpriced model
 * is not a free one and a $0 here would be a claim about a mechanism we are asking the reader to
 * trust. The coverage line is rendered even when it is unflattering — particularly then: a panel
 * that only reports the sessions the trigger fired on is survivorship, and always positive.
 */
async function loadCompactionEpisodes() {
  const body = clear($('#episodes-body'));
  loadingRows(body, EPISODE_COLS);
  const cov = $('#episodes-coverage');
  const asm = $('#episodes-assumptions');
  try {
    const out = await api('components/compaction-episodes');
    const groups = out.by_provenance || [];
    clear(body);
    if (!groups.length) {
      tableMessage(body, EPISODE_COLS, 'No preset-step episodes yet',
        'An episode needs a summary followed by 10% more of the window being spent. Nothing in '
        + 'this time range has both.');
    }
    for (const g of groups) {
      const priced = g.episodes > g.unpriced_episodes;
      const reason = 'every episode in this group ran on a model with no known rates';
      const ourCost = g.invalidation_debit_usd + g.summarizer_cost_usd;
      body.appendChild(el('tr', {},
        el('td', {}, el('span', { class: 'pill', title: PROVENANCE_NOTE[g.provenance] || '' },
          g.provenance)),
        el('td', { class: 'num', text: num(g.episodes) }),
        el('td', { class: 'num', text: num(g.closed) }),
        // Open and voided are shown even at zero: they are the reasons a total is smaller than
        // the episode count, and a blank cell would leave that unexplained.
        el('td', {
          class: 'num' + (g.open ? ' muted' : ''),
          title: 'spans that have not finished inside this time range; counted, not totalled',
        }, num(g.open)),
        el('td', {
          class: 'num' + (g.voided ? ' warn-text' : ''),
          title: 'the client compacted its own transcript part-way through the span, so the rest '
            + 'is not comparable; counted, not totalled',
        }, num(g.voided)),
        el('td', { class: 'num', text: num(g.turns) }),
        el('td', { class: 'num' }, usdOrNA(g.cold_credit_usd, priced, reason)),
        el('td', { class: 'num' }, usdOrNA(g.read_credit_usd, priced, reason)),
        el('td', { class: 'num' }, usdOrNA(g.other_credit_usd, priced, reason)),
        el('td', {
          class: 'num',
          title: 'the summary\u2019s own model call (' + usd(g.summarizer_cost_usd) + ') plus the '
            + 'cache write our rewrite caused (' + usd(g.invalidation_debit_usd) + ', an upper bound)',
        }, usdOrNA(ourCost, priced, reason)),
        // NOT GREEN WHEN NOTHING HAS SETTLED. With no closed span the settled net is $0.00 by
        // definition, and rendering that in good-text read as "this component broke even" on
        // exactly the sessions where it has spent money and not yet recouped it — the position is
        // in open_net_usd, which nothing rendered at all. A review found the false green.
        g.closed
          ? el('td', { class: 'num ' + (g.net_usd >= 0 ? 'good-text' : 'bad-text') },
            usdOrNA(g.net_usd, priced, reason))
          : el('td', {
            class: 'num muted',
            title: 'no span in this group has finished, so nothing has settled. The money already '
              + 'committed is in the exposure line below the table.',
          }, '\u2014')));
    }
    renderEpisodeExposure($('#episodes-exposure'), groups);
    renderEpisodeCoverage(cov, out.coverage, out.assumptions);
    renderEpisodeAssumptions(asm, out.assumptions);
  } catch (err) {
    if (aborted(err)) return;
    cov.hidden = true;
    asm.hidden = true;
    $('#episodes-exposure').hidden = true;
    tableMessage(body, EPISODE_COLS, 'Could not load preset-step episodes',
      String(err.message || err), { error: true });
  }
}

/**
 * renderEpisodeExposure reports money already COMMITTED whose payoff has not arrived.
 *
 * WITHOUT THIS THE PANEL IS FALSE-GREEN IN ITS NORMAL STATE. A summarized session adds new content
 * slowly by construction — five turns after a summary on a live run had accrued 3,486 of a 20,000
 * target — so "open" is where a healthy episode spends most of its life. The server computes
 * open_net_usd, open_turns and voided_net_usd for exactly this reason, and nothing rendered any of
 * them: the table showed a settled total of $0.00 in good-text green while the real position was
 * negative. A review found it, and an earlier commit of mine had documented the state in prose
 * rather than fixing it, which is the wrong end of the problem.
 *
 * Voided is reported apart and never blended: the client compacted its own transcript mid-span, so
 * the remainder is not comparable to a world where we had not compacted. It is counted because
 * dropping it would be survivorship, and separated because averaging it in would be a claim.
 */
function renderEpisodeExposure(node, groups) {
  if (!node) return;
  const open = groups.reduce((a, g) => a + (g.open || 0), 0);
  const voided = groups.reduce((a, g) => a + (g.voided || 0), 0);
  node.hidden = !(open || voided);
  if (node.hidden) return;
  clear(node);
  // Only priced groups contribute a figure, for the same reason every cell above goes through
  // usdOrNA: an unpriced model is not a free one.
  const priced = groups.filter((g) => g.episodes > g.unpriced_episodes);
  const openUSD = priced.reduce((a, g) => a + (g.open_net_usd || 0), 0);
  const openTurns = priced.reduce((a, g) => a + (g.open_turns || 0), 0);
  const voidUSD = priced.reduce((a, g) => a + (g.voided_net_usd || 0), 0);
  const parts = [el('strong', {}, 'Not yet settled: ')];
  if (open) {
    parts.push(document.createTextNode(num(open) + ' span(s) still accruing over '
      + num(openTurns) + ' turn(s), currently '));
    parts.push(el('strong', { class: openUSD >= 0 ? 'good-text' : 'bad-text' }, usd(openUSD)));
    parts.push(document.createTextNode('. That is money committed whose payoff is still arriving, '
      + 'not a loss: the summary turn pays a model call and a cache write up front while the '
      + 'saving accrues turn by turn afterwards'));
  }
  if (voided) {
    parts.push(document.createTextNode((open ? '. ' : '') + num(voided)
      + ' span(s) voided by the client compacting its own transcript, carrying '));
    parts.push(el('strong', { class: voidUSD >= 0 ? 'good-text' : 'bad-text' }, usd(voidUSD)));
    parts.push(document.createTextNode(' — reported apart, never averaged in'));
  }
  parts.push(document.createTextNode('.'));
  for (const x of parts) node.appendChild(x);
}

/**
 * renderEpisodeCoverage is the half that can make the panel look bad, and it is not optional.
 *
 * "When we summarized, we saved money" is true by construction. The question that decides whether
 * the trigger deserves its default is how many qualifying conversations it never fired on at all,
 * and what the cold rewrites cost THEM.
 */
function renderEpisodeCoverage(node, c, a) {
  if (!c || !c.conversations) {
    // No qualifying conversation at all is itself worth saying: it means nothing in this range
    // was ever big enough for the question to arise.
    node.hidden = !(c && c.window_unknown);
    if (!node.hidden) {
      clear(node).appendChild(document.createTextNode(
        num(c.window_unknown) + ' conversation(s) excluded because their model\u2019s context '
        + 'window is not published — a span measured against a guessed window would be wrong by '
        + 'whatever the guess is off by, so they are left out rather than estimated.'));
    }
    return;
  }
  const fill = pct((a && a.fill_frac ? a.fill_frac : c.fill_frac) * 100, 0);
  const parts = [
    el('strong', {}, 'Coverage: '),
    document.createTextNode(num(c.with_episode) + ' of ' + num(c.conversations)
      + ' conversations that reached ' + fill + ' of their context window produced a summary'),
  ];
  if (c.no_episode) {
    parts.push(document.createTextNode('. The other ' + num(c.no_episode) + ' never did'));
    if (c.no_episode_cold_usd) {
      parts.push(document.createTextNode(', and paid '));
      parts.push(el('strong', {}, usd(c.no_episode_cold_usd)));
      parts.push(document.createTextNode(' re-creating expired cache entries — the size of the '
        + 'opportunity there, not a saving anyone missed out on for certain'));
    }
  }
  if (c.window_unknown) {
    parts.push(document.createTextNode('. ' + num(c.window_unknown)
      + ' more excluded: no published context window to measure against'));
  }
  if (c.unpriced) {
    parts.push(document.createTextNode('. ' + num(c.unpriced) + ' on models with no known rates'));
  }
  parts.push(document.createTextNode('.'));
  clear(node);
  for (const p of parts) node.appendChild(p);
  node.hidden = false;
}

/**
 * renderEpisodeAssumptions prints the server's own statement of its arithmetic.
 *
 * The server sends it; the page does not compose it. That is the rule the KV-cache tab already
 * keeps — a formula restated in a template is a formula nothing tests, and this one has three
 * caveats a reader needs in order to size what they are looking at.
 */
function renderEpisodeAssumptions(node, a) {
  if (!a) { node.hidden = true; return; }
  clear(node).appendChild(el('details', {},
    el('summary', {}, 'How this is measured'),
    el('ul', {},
      el('li', {}, 'Span: ' + pct(a.span_frac * 100, 0) + ' more of the window, measured in '
        + a.span_measure),
      el('li', {}, 'Credit: ' + a.credit_source),
      el('li', {}, 'Cold: ' + a.cold_label),
      el('li', {}, 'Reads: ' + a.read_label),
      el('li', {}, 'Our cost: ' + a.debit_bound),
      el('li', {}, 'Voided: ' + a.void_rule),
      el('li', {}, 'Excluded: ' + a.window_rule),
      el('li', {}, el('strong', {}, 'Not counted: '), a.known_omission))));
  node.hidden = false;
}

// Two independent reads: the per-component table and the compaction-episode panel.
// Awaited together so one failing does not blank the other.
Object.assign(loaders, {
  components: async () => { await Promise.allSettled([loadComponents(), loadCompactionEpisodes()]); },
});
