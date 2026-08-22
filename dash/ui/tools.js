// The Inventory view: what an account CARRIES in every prompt (tool schemas, MCP
// servers, skill listings) against what it actually INVOKES, and the control to stop
// carrying the rest.
//
// It is one appended file rather than an edit to app.js, and it mounts itself: the tab,
// the section and the loader registration all happen here, so this feature is additive to
// the shared page and the shared page has one line about it. Every helper it uses —
// el(), clear(), num(), usd(), pct(), emptyState(), sortRows(), rangeLabel(), api() — is
// app.js's, because a second design system on one page is how a page stops looking like
// one product.
//
// THE HONESTY THIS FILE IS FOR. The measurement behind this page is that 82.7% of a
// declared prompt is never touched, which is a big enough number to be worth lying with.
// Four rules are therefore structural here, not editorial:
//
//   1. A session with NO captured inventory is not a session with nothing unused. Every
//      row in a database written before this capture existed is in that state, so
//      `coverage.not_captured` gets its own visible answer and the headline is SUPPRESSED
//      entirely when nothing at all was captured — a 0% bar over an empty corpus reads as
//      "you use everything you carry", which is the opposite of what is known.
//   2. A projection is not a saving. What could be avoided and what a removal actually
//      avoided are different colours, different words and different panels, and the
//      realized figure is never computed here — it comes from the filter's own accounting
//      or it is absent.
//   3. An unpriced model has tokens and no dollar. `priced:false` renders a pill, never
//      $0.
//   4. Every removal suggestion carries its basis: how many sessions declared it, that
//      none invoked it, and over what window. "Never invoked in the 6 sessions you have"
//      is a much weaker claim than "never invoked in 400", and the reader must be able to
//      tell which one they are being shown.
'use strict';

// Its own stylesheet, linked from here rather than from index.html so mounting this
// feature is ONE line in the shared page. Same-origin, so `style-src 'self'` allows it.
document.head.appendChild(el('link', { rel: 'stylesheet', href: 'tools.css' }));

// ── mount ──────────────────────────────────────────────────────────────────
// The tab goes next to Usage: both answer "where is the money", and this one is
// actionable, so it does not belong at the far end behind the account tabs.
(function mountToolsTab() {
  const tabs = $('.tabs');
  const tab = el('button', {
    role: 'tab', class: 'tab', 'data-view': 'tools', 'data-testid': 'tab-tools',
    'aria-selected': 'false',
  }, 'Inventory');
  const after = $('.tab[data-view="usage"]', tabs);
  tabs.insertBefore(tab, after ? after.nextSibling : null);
})();

// The section is built here too, in DOM rather than markup, because every part of it is
// conditional on what the report says can be answered.
const toolsView = el('section', { class: 'view', id: 'view-tools', hidden: 'hidden' });
$('#main').appendChild(toolsView);

// ── local state ────────────────────────────────────────────────────────────
// Its own sort state, NOT app.js's state.sort: that one is the components table's and is
// mirrored into the URL for that view only, so sharing it would make a sort of this table
// silently reorder that one.
const tools = {
  report: null,
  // control is the removal path's state: null when the endpoint is not there (an older
  // proxy, or the feature not built yet), in which case the checkboxes render disabled
  // with the reason rather than vanishing — a control that disappears looks like a page
  // that has no opinion.
  control: null,
  sort: 'unused_reads',
  dir: 'desc',
  busy: '',
};

/** Column key per header cell, in header order. '' for a header that does not sort. */
const TOOL_COLS = ['name', 'kind', 'tokens', 'sessions_declared', 'sessions_used',
  'calls', 'unused_reads', 'unused_usd'];

// ── formatting shared by every table here ──────────────────────────────────

/**
 * money renders a dollar figure, or says the model had no rates.
 *
 * This is honesty rule 3 in one function: an unpriced row's tokens are real and its
 * dollars are unknown, and `usd(0)` renders "$0", which is a claim that carrying it was
 * free. Every dollar cell on this page goes through here.
 */
function money(v, priced) {
  if (!priced) {
    return el('span', {
      class: 'pill missing',
      title: 'This ran on a model with no known rates. Its tokens are counted; its cost is not known.',
    }, 'unpriced');
  }
  return document.createTextNode(usd(v));
}

/**
 * kindPill names what sort of thing a row is, because removability depends on it.
 *
 * `kind` is NOT the answer on its own, and reading it as though it were is the bug this
 * function exists to hold shut. Claude Code's own tools and a third-party agent's own tools
 * are the SAME kind — both are `tool`, because the stored taxonomy cannot tell them apart
 * (see dash/toolremoval.go) — and the report answers the question in a separate `builtin`
 * boolean. This label used to map kind `tool` straight to the string "built-in", so every
 * removable client tool an SDK application had declared rendered a pill saying "built-in"
 * inside the list of things the page was recommending be removed. The reader was doing
 * exactly the right thing by not trusting it.
 *
 * So `builtin` is checked FIRST and gets its own word and its own colour, and the plain
 * client tool gets the label that says what it actually is: something else declared it.
 */
const KIND_LABEL = {
  tool: 'client tool', mcp_tool: 'MCP', server_tool: 'provider', skill: 'skill',
};
function kindPill(t) {
  const k = t.kind || 'tool';
  if (t.builtin) {
    return el('span', {
      class: 'pill bad', 'data-kind': 'builtin',
      title: "One of Claude Code's own tools. Removing it does not slim the agent — it takes "
        + 'away equipment the model is expected to have.',
    }, 'Claude Code');
  }
  return el('span', { class: k === 'server_tool' ? 'pill neutral' : 'pill', 'data-kind': k },
    KIND_LABEL[k] || k);
}

/** A number cell. */
function numTd(v) { return el('td', { class: 'num', text: num(v) }); }
/** A compacted number cell, for the read counts that run to hundreds of millions. */
function readTd(v) {
  return el('td', { class: 'num', title: num(v) + ' tokens', text: compact(v) });
}

// ── what the numbers mean ──────────────────────────────────────────────────

/**
 * Every figure on this page, in plain English, keyed by its tile.
 *
 * It lands in app.js's TILE_INFO rather than a table of its own, because tile() reads that
 * one and because the whole point of that table is that all forty explanations can be read
 * side by side and checked for a consistent voice — a second registry would be a second
 * voice with a second standard of honesty.
 *
 * Every entry was written against the expression behind it, and follows the three rules
 * TILE_INFO states: what it is before how it is computed, the CATCH named out loud, and
 * never a description of a number the code does not compute. This page had none at all: the
 * one tab whose figures are used to decide what to delete was the one tab that did not say
 * what its figures meant.
 */
Object.assign(TILE_INFO, {
  'inv-declared': {
    what: 'How many tokens of tool schemas, MCP tool schemas and the skills listing a '
      + 'typical session of yours carries — before you type anything. Every turn of the '
      + 'session pays for it again.',
    how: 'The declaration set each session carried, summed, then averaged over the sessions '
      + 'whose inventory was captured. Measured with a real BPE tokenizer over the exact '
      + 'JSON sent, not estimated from characters.',
    catch: 'It does NOT include your system prompt, your CLAUDE.md or the conversation — '
      + 'only the declarations. The system prompt is shown as its own region in "What every '
      + 'request carries", where the whole prefix is broken down.',
  },
  'inv-used': {
    what: 'The share of those declared tokens belonging to capabilities that were actually '
      + 'invoked at least once.',
    how: 'Declared tokens minus never-invoked tokens. A capability counts as invoked in a '
      + 'session if that session called it even once; a skill counts when the Skill tool ran '
      + 'with its name.',
    catch: 'Invoked ONCE and invoked four hundred times look identical here. This is a '
      + 'measure of whether a declaration was ever worth carrying, not of how hard it worked.',
  },
  'inv-unused': {
    what: 'Tokens you carried in every request of a session that never once called the thing '
      + 'they declare. This is the waste this page exists to find.',
    how: 'For each session, the summed weight of every capability it declared and did not '
      + 'invoke — averaged over the captured sessions.',
    catch: 'Never-invoked is not the same as useless. A tool that goes uncalled for fifty '
      + 'sessions may be doing its job by being available on the fifty-first. The built-ins '
      + 'are excluded from the actionable list for exactly this reason.',
  },
  'inv-avoidable': {
    what: 'What those never-invoked declarations cost you in real money over this window.',
    how: 'Each never-invoked declaration\'s token weight, multiplied by the number of '
      + 'requests in the sessions that carried it, priced at the tier each of those requests '
      + 'actually paid — cache-read for a hit, cache-CREATION for the turn that wrote the '
      + 'prefix, full input price where there was no cache at all.',
    catch: 'It is a PROJECTION, not a saving: it is what would not have been billed had none '
      + 'of it ever been carried. Nothing here has been saved. What removals actually avoided '
      + 'is a different panel and is never copied into this one.',
  },
  'inv-prompt-tokens': {
    what: 'The size of your system prompt — the standing instructions sent ahead of every '
      + 'single request, separately from the tool declarations.',
    how: 'The largest system prompt any captured session in scope carried, measured with the '
      + 'same tokenizer. The MAX, not an average: the question is how big the thing you would '
      + 'be reading is.',
    catch: 'Its SIZE is always recorded; its TEXT is only stored under transcript-capture '
      + 'consent. A session recorded before this feature existed contributes neither, and the '
      + 'coverage line below says how many that is.',
  },
  'inv-declared-set': {
    what: 'The summed weight of your whole declaration set, counted once — every tool schema, '
      + 'every MCP tool schema, and the skills listing.',
    how: 'Each declaration\'s own measured weight, added up. Each SKILL is an entry inside the '
      + 'listing rather than a separate block, so the listing is counted and the entries are '
      + 'not counted again on top of it.',
    catch: 'This is a TOTAL over the set. The headline "Declared per session" tile is a MEAN '
      + 'over captured sessions and is a different, smaller number — sessions range from a '
      + 'two-tool sidechain to the full catalogue.',
  },
  'inv-prefix-total': {
    what: 'Everything that goes in front of your conversation on every request: the system '
      + 'prompt plus every declaration.',
    how: 'The system prompt\'s tokens plus the summed weight of the whole declaration set. '
      + 'This is the whole that every share on this page is a part of.',
    catch: 'It is one SESSION\'s prefix, not an average of several — a prefix averaged over '
      + 'sessions is not a prefix anybody sent. Which session is named beside the panel.',
  },
  'inv-real-usd': {
    what: 'Money that was genuinely not billed, because requests were actually sent without '
      + 'the declarations you opted out of.',
    how: 'Measured on the requests themselves after a removal took effect — the declaration '
      + 'set that went out was smaller, and the difference is priced at the tier those '
      + 'requests paid.',
    catch: 'This is the only figure on the page that is a SAVING rather than a projection. '
      + 'It is normally much smaller than the projected figure above, and the two are not '
      + 'comparable: one is what happened, the other is what could have.',
  },
  'inv-real-reads': {
    what: 'Tokens the provider never had to read, because they were no longer in the prompt.',
    how: 'The weight of each removed declaration, times the requests that would otherwise '
      + 'have re-read it.',
    catch: 'Counted with a local tokenizer over the declarations we removed, so it is not a '
      + 'figure off an invoice. The dollar tile beside it is the priced one.',
  },
  'inv-real-items': {
    what: 'How many capabilities you have switched off through this page.',
    how: 'A count of the entries on your account\'s declaration-removal list.',
    catch: 'Switching one off changes the cached prompt prefix, so the next turn of every '
      + 'session in flight re-writes its whole prompt once at the cache-creation rate. That '
      + 'is why it is worth switching off everything you mean to in one pass.',
  },
  'inv-skill-listing': {
    what: 'What the skills LISTING itself costs — the prose block that tells the model which '
      + 'skills exist, not the skills\' own instructions.',
    how: 'The measured weight of the listing block, averaged over the sessions that carried '
      + 'one.',
    catch: 'The listing is one indivisible block: it is waste only when the session invoked '
      + 'NO skill at all. Removing one skill from it shrinks it a little; removing the last '
      + 'one removes the block.',
  },
  'inv-skill-declared': {
    what: 'How many skills the listing advertised to the model.',
    how: 'Counted by parsing the listing block out of the request.',
    catch: 'A listing whose format this parser cannot read reports as UNKNOWN rather than as '
      + 'zero skills. "We could not read this" and "there is nothing here" are different '
      + 'answers and only one of them makes it safe to remove something.',
  },
  'inv-skill-invoked': {
    what: 'How many of those skills were ever actually run.',
    how: 'A skill counts as invoked when the Skill tool was called with its name — the only '
      + 'place in the request where a skill invocation is identifiable.',
    catch: 'A skill run by a subagent whose traffic did not come through this proxy is not '
      + 'visible here and would read as never invoked.',
  },
  'inv-skill-waste': {
    what: 'Tokens spent re-reading the skills listing in sessions that ran no skill at all.',
    how: 'The listing\'s weight times the requests of every session that invoked no skill.',
    catch: 'Sessions that ran even one skill contribute nothing here, however many skills '
      + 'they left untouched — the block is indivisible.',
  },
  'inv-skill-usd': {
    what: 'What that listing waste cost, in money.',
    how: 'Those tokens priced at the tier each request actually paid.',
    catch: 'A projection like the other dollar figures above: this is what would not have '
      + 'been billed had the listing not been carried, not money that has been saved.',
  },
});

// ── the headline ───────────────────────────────────────────────────────────

/**
 * renderHeadline is "what you carry versus what you use", and its first job is to REFUSE
 * to answer when the corpus cannot answer.
 *
 * The suppression is honesty rule 1. With captured === 0 every figure below is a true 0
 * over an empty set, and a tile reading "0 tokens never used" beside a full-width bar is
 * indistinguishable from perfect utilisation. So the whole band is replaced by the state
 * that is actually true: nothing has been captured yet, here is what starts it.
 */
function renderHeadline(host, rep) {
  const c = rep.coverage, t = rep.totals;
  if (!c.captured) {
    emptyState(host,
      c.sessions ? 'No inventory captured for these sessions yet'
        : 'No sessions in this window',
      c.sessions
        ? num(c.sessions) + ' session' + (c.sessions === 1 ? '' : 's') + ' ran here, and none of '
          + 'them recorded which tools they declared — their requests predate this capture, or '
          + 'the declarations were dropped. That is NOT "nothing is unused": it is not known '
          + 'either way. The next session through this proxy fills this in.'
        : 'Widen the time range, or send a session through this proxy.');
    return;
  }
  const used = Math.max(0, t.declared_tokens - t.unused_tokens);
  host.appendChild(tileGroup('What you carry, and what you use',
    'per session, averaged over the ' + num(c.captured) + ' session'
    + (c.captured === 1 ? '' : 's') + ' whose inventory was captured', [
    tile('inv-declared', 'Declared per session', num(t.declared_tokens),
      'tool schemas, MCP tools and the skills listing'),
    tile('inv-used', 'Invoked at least once', num(used),
      pct(100 - t.unused_pct) + ' of what you declare', used ? 'good' : ''),
    // The number this page exists for. Deliberately the only 'bad' tile: three red tiles
    // in a row is a page shouting, and shouting is what gets a dashboard ignored.
    // The multiplier NAMES ITS POPULATION, because the page carries two of them and they
    // differ by about 2x: this is the plain mean over every TOOL-BEARING session in scope,
    // while the removal-value table below uses the REQUEST-WEIGHTED mean over the CAPTURED
    // ones. Both are real and they answer different questions ("how long is a session" vs
    // "how long is the session a typical request belongs to"). Two multipliers on one page
    // with neither naming its denominator is how a reader concludes the page contradicts
    // itself — and on production traffic the pair reads 3.9 against 150.6.
    tile('inv-unused', 'Never invoked', num(t.unused_tokens),
      pct(t.unused_pct) + ' of every prompt, re-read ' + t.requests_per_session.toFixed(1)
      + '× per session — the plain mean over all ' + num(c.sessions) + ' sessions here',
      t.unused_pct >= 25 ? 'bad' : ''),
    tile('inv-avoidable', 'Avoidable — projected',
      t.priced ? usd(t.unused_usd) : num(t.unused_reads) + ' tok',
      t.priced ? 'if none of it had been carried' : 'no dollar: some models here are unpriced'),
  ], 'headline'));
  host.appendChild(gauge(t));
}

/**
 * gauge is the one picture on this page: one bar, two segments, drawn to SCALE.
 *
 * To scale is honesty rule 6. A pair of tiles can make 4% and 84% look equally
 * significant because the type size is the same; a proportional bar cannot. So a small
 * waste renders as a sliver and reads as a sliver, with no layout that needs it to be big.
 */
function gauge(t) {
  const total = t.declared_tokens || 1;
  const unusedPct = Math.min(100, 100 * t.unused_tokens / total);
  // "Every prompt this account sends" was the heading here, and nobody could tell what it
  // meant — it reads as a count of prompts, or as a claim about all traffic, and it is
  // neither. What the bar shows is the weight of the tool schemas, MCP tool schemas and
  // skills listing that ride along on EVERY request of a session, split by whether anything
  // ever called them. So the heading says that, and the sub-line says what the number is
  // averaged over instead of only its unit.
  const box = el('div', { class: 'panel cg-gauge', 'data-testid': 'inv-gauge' },
    el('div', { class: 'cg-gauge-head' },
      el('h2', {}, 'What every request carries before you type anything'),
      el('span', { class: 'section-note', text: num(t.declared_tokens)
        + ' tokens of tool and skill declarations, per request, averaged over the sessions '
        + 'whose inventory was captured' })),
    el('div', { class: 'cg-gauge-track' },
      el('span', {
        class: 'cg-seg cg-used', style: 'width:' + (100 - unusedPct).toFixed(2) + '%',
      }),
      el('span', {
        class: 'cg-seg cg-unused', style: 'width:' + unusedPct.toFixed(2) + '%',
        'data-testid': 'inv-gauge-unused',
      })));
  box.appendChild(el('div', { class: 'cg-gauge-legend' },
    el('span', {}, el('i', { class: 'cg-sw cg-used' }),
      'invoked ' + num(t.declared_tokens - t.unused_tokens) + ' tok'),
    el('span', {}, el('i', { class: 'cg-sw cg-unused' }),
      'never invoked ' + num(t.unused_tokens) + ' tok (' + pct(t.unused_pct) + ')')));
  if (!t.unused_tokens) {
    box.appendChild(el('p', { class: 'hint ok' },
      'Nothing in this scope was declared and left unused. There is nothing to remove.'));
  }
  return box;
}

/**
 * renderCoverage is the denominator, always shown, even when it is clean.
 *
 * Honesty rule 1's visible half. Hiding it when not_captured is 0 would mean the reader
 * only ever learns that coverage is a thing on the days it is bad, and cannot tell a
 * complete answer from a page that has stopped mentioning it.
 */
function renderCoverage(host, rep) {
  const c = rep.coverage;
  const p = el('p', { class: 'hint' },
    num(c.sessions) + ' session' + (c.sessions === 1 ? '' : 's') + ' in scope, '
    + num(c.requests) + ' requests. ');
  const box = el('div', { class: 'panel cg-coverage', 'data-testid': 'inv-coverage' },
    el('h2', {}, 'What these numbers are computed over'), p);
  host.appendChild(box);
  if (c.captured) {
    p.appendChild(document.createTextNode(num(c.captured) + ' carried a captured inventory '
      + 'and are the only sessions any figure above is averaged over.'));
  }
  if (c.not_captured) {
    // Not a zero, and not folded into the average: the explicit third answer.
    box.appendChild(
      el('div', { class: 'state blocked', 'data-testid': 'inv-not-captured' },
        el('div', { class: 'state-body' },
          el('strong', {}, num(c.not_captured) + ' of ' + num(c.sessions)
            + ' sessions have no captured inventory'),
          el('span', {}, 'Their requests predate tool capture, or their declarations were '
            + 'dropped. They are counted in NEITHER direction — not as fully used, not as '
            + 'fully wasted.' + (c.captured
              ? ' The percentages above therefore describe the ' + num(c.captured)
                + ' captured session' + (c.captured === 1 ? '' : 's') + ' only.'
              : ' With none captured, this page reports no percentage at all rather than a 0%.')),
          el('span', {}, 'This is the normal state of a history recorded before the feature '
            + 'existed. Sessions from here on are captured.'))));
  }
  if (c.unpriced_sessions) {
    box.appendChild(
      el('div', { class: 'state', 'data-testid': 'inv-unpriced' },
        el('div', { class: 'state-body' },
          el('strong', {}, num(c.unpriced_sessions) + ' session'
            + (c.unpriced_sessions === 1 ? '' : 's') + ' ran an unpriced model'),
          el('span', {}, 'Their wasted tokens are in every token column. Their dollars are in '
            + 'no total on this page, and are shown as '),
          el('span', {}, 'unpriced rather than as $0.'))));
  }
  box.appendChild(el('details', { class: 'why' },
    el('summary', {}, 'How a token weight becomes a dollar, and at which tier'),
    el('p', {}, 'A declaration is written once into the prompt and then RE-READ by every '
      + 'later turn of the session. The token counts on this page are those re-reads, not the '
      + 'one-off size.'),
    // Two populations, both named. They differ by ~40x on real traffic and used to sit on one
    // page with nothing connecting them: the mean counts every tool-bearing session including
    // one-request sidechains, the weighted figure describes the session a typical REQUEST
    // belongs to, which is where the re-reads actually are.
    el('p', {}, 'Averaged over every tool-bearing session in scope that is '
      + (rep.totals.requests_per_session || 0).toFixed(1) + ' requests. But the session a '
      + 'typical REQUEST belongs to runs for '
      + (rep.totals.requests_per_session_typical || 0).toFixed(0) + ' turns — most sessions are '
      + 'one-request sidechains, so the plain average is not what carrying a declaration costs. '
      + 'The per-session projection below uses the second figure and says so.'),
    el('p', {}, 'Each re-read is priced at the tier that request actually paid: cache read '
      + 'for a hit, cache CREATION for the turn that wrote the prefix, and full input rate '
      + 'for a turn with no cache at all. It is not all valued at the cache-read rate, which '
      + 'would understate the cold turns, and not all at the fresh-input rate, which is the '
      + 'same tokens at roughly ten times the price. A figure quoted at one flat tier is a '
      + 'different number from the one here and should not be compared to it.')));
}

// ── the control surface ────────────────────────────────────────────────────

/**
 * renderUnused is the actionable list: what you carry, never call, and can stop carrying.
 *
 * It is GROUPED BY WHAT ONE ACTION REMOVES, which is the change that turned it from a wall
 * into a decision. Flat and sorted by weight, eighty rows of `$0.0035` each read as eighty
 * separate judgement calls, and the two rows worth acting on were somewhere in the middle of
 * them. An MCP server is not eighty decisions: `claude mcp remove <server>` takes the whole
 * thing, so the server is the row and its tools are the detail. The same for a plugin's
 * skills, which come and go with the plugin.
 *
 * Every group carries the command that removes it, VISIBLE — not behind a disclosure. The
 * command already existed on the API (dash/toolremoval.go) and was rendered in exactly one
 * place: the built-ins table, i.e. the one group nobody should act on. A reader looking for
 * "how do I get rid of this" found nothing here and the danger warning there.
 *
 * Built-ins and provider tools are still excluded and still live in their own collapsed
 * section at the end — they dominate by weight and are the one group where acting on
 * "never invoked" is a mistake.
 */
function renderUnused(host, rep) {
  const rows = rep.tools.concat(rep.skills.skills || [])
    .filter((t) => !t.sessions_used && t.sessions_declared > 0
      && !t.builtin && t.kind !== 'server_tool');
  const groups = groupRemovable(rows);
  const panel = el('div', { class: 'panel', 'data-testid': 'inv-unused-panel' },
    el('div', { class: 'section' },
      el('h2', {}, 'Carried by every request, never once called'),
      el('span', { class: 'section-note' }, rows.length
        ? num(groups.length) + ' thing' + (groups.length === 1 ? '' : 's') + ' to remove, '
          + num(rows.length) + ' declaration' + (rows.length === 1 ? '' : 's') + ' between them'
        : 'nothing')),
    el('p', { class: 'note' }, rows.length
      ? 'Grouped by what ONE action removes, heaviest first. The command under each group is '
        + 'the whole fix — copy it, run it, and that group stops being sent.'
      : 'Nothing here was carried without being called.'));
  host.appendChild(panel);
  if (!rows.length) {
    emptyState(panel.appendChild(el('div')), 'Nothing to remove',
      'Every declaration in this scope was invoked in at least one session.');
    return;
  }
  if (rep.coverage.captured < 5) {
    // The same "never invoked" row means something quite different over three sessions than
    // over four hundred, and the reader must be told which they are looking at BEFORE the
    // list, not asked to infer it from a denominator.
    panel.appendChild(el('p', { class: 'hint', 'data-testid': 'inv-small-sample' },
      'Only ' + num(rep.coverage.captured) + ' session'
      + (rep.coverage.captured === 1 ? '' : 's') + ' have been captured so far, so this is a '
      + 'small sample: a row below is evidence that nothing has called that item YET, not '
      + 'proof that nothing will. Every row states its own denominator.'));
  }
  panel.appendChild(reversibilityNote());
  if (!tools.control) {
    // Graceful degradation, stated. The analysis and the COMMANDS are the whole value and
    // stand on their own; only the one-click switch is missing.
    panel.appendChild(el('div', { class: 'state', 'data-testid': 'inv-control-absent' },
      el('div', { class: 'state-body' },
        el('strong', {}, 'One-click opt-out is not enabled on this proxy'),
        el('span', {}, 'It changes nothing about the list below. Every group carries the '
          + 'command that removes it from your own configuration, which is the more direct '
          + 'fix anyway — it stops the declaration at the source rather than filtering it '
          + 'here.'))));
  }
  const list = el('div', { class: 'inv-groups' });
  for (const g of groups.slice(0, EXPANDED_GROUPS)) list.appendChild(removalGroup(g, rep));
  panel.appendChild(list);
  const tail = groups.slice(EXPANDED_GROUPS);
  if (tail.length) {
    // Folded, never dropped: the total is stated on the closed summary so the reader can see
    // at a glance whether the tail is worth opening, which is the only honest way to hide it.
    const tailUSD = tail.reduce((n, g) => n + g.usd, 0);
    const tailPriced = tail.every((g) => g.priced);
    const det = el('details', { class: 'why', 'data-testid': 'inv-group-tail' },
      el('summary', {}, 'The remaining ' + num(tail.length) + ' — '
        + (tailPriced ? usd(tailUSD) : compact(tail.reduce((n, g) => n + g.reads, 0)) + ' tok')
        + ' between them'));
    const rest = el('div', { class: 'inv-groups' });
    for (const g of tail) rest.appendChild(removalGroup(g, rep));
    det.appendChild(rest);
    panel.appendChild(det);
  }
}

/**
 * groupRemovable collapses the candidate rows into the units a single action removes.
 *
 * One group per MCP server, one per plugin (its skills go together), and one row per
 * standalone item. Sorted by what the group costs, because that is the order a reader should
 * work down — and within a group by weight, for the same reason.
 */
function groupRemovable(rows) {
  const by = new Map();
  for (const t of rows) {
    // The key is the ACTION, not the taxonomy: two tools of one MCP server share a key
    // because one command removes both.
    let key = t.kind + '/' + t.name;
    let label = t.name;
    let sub = '';
    // Grouped ONLY when the server says the mechanism is group-wide. `claude mcp remove
    // <server>` takes a whole server and `claude plugin disable <plugin>` takes a whole
    // plugin, so those are one action. A PLUGIN-bundled MCP server is not: it was never
    // added by hand, so there is no `claude mcp remove` name for it and its removal is a
    // per-tool deny (see dash/toolremoval.go). Grouping those anyway put the heaviest tool's
    // one-tool snippet under a heading that said "removes all 2 of them", which is a
    // dashboard telling a reader that pasting a command will do something it will not.
    const r = t.removal || {};
    if (t.kind === 'mcp_tool' && t.server && r.kind === 'mcp_tool' && r.command) {
      key = 'server/' + t.server;
      label = t.server;
      sub = 'MCP server';
    } else if (t.kind === 'skill' && r.kind === 'plugin_skill' && r.command) {
      key = 'plugin/' + t.name.split(':')[0];
      label = t.name.split(':')[0];
      sub = 'plugin';
    }
    let g = by.get(key);
    if (!g) {
      g = { key, label, sub, items: [], tokens: 0, reads: 0, usd: 0, priced: true };
      by.set(key, g);
    }
    g.items.push(t);
    g.tokens += t.tokens;
    g.reads += t.unused_reads;
    g.usd += t.unused_usd;
    if (!t.priced) g.priced = false;
  }
  const out = [...by.values()];
  for (const g of out) g.items.sort((a, b) => b.tokens - a.tokens);
  // Ordered by what each row LEADS WITH, which is the dollar figure. Ordering by token weight
  // while displaying money first puts a 314-token group that cost $0.04 above a 295-token one
  // that cost $0.13, because weight per request and total waste are different quantities —
  // and a list a reader is told to work down must be in the order of the number they read.
  // Unpriced groups fall back to the token-side total, so a scope with no rates still sorts.
  out.sort((a, b) => (b.priced && a.priced ? b.usd - a.usd : b.reads - a.reads));
  return out;
}

// howManyGroupsExpanded is how many removal groups are shown open before the tail folds.
//
// The list is ordered by cost and the distribution is steep — on real traffic the first few
// groups are dollars and the rest are fractions of a cent each. Eight is enough to cover
// everything worth a decision and short enough that the panel below it is still on the screen;
// past that the reader is scrolling through change.
//
// ponytail: a fixed count. If a real account ever has a flat cost distribution, this becomes
// "expand while the group is worth more than 1% of the total".
const EXPANDED_GROUPS = 8;

/**
 * removalGroup is one removable unit: what it costs, the command that removes it, and its
 * members.
 *
 * The command comes from the HEAVIEST member's Removal, which for an MCP server is the
 * server-level `claude mcp remove` and for a plugin skill is `claude plugin disable` — both
 * of which take the whole group. A group of one is its own answer. The server decides the
 * mechanism (dash/toolremoval.go); this only renders it, because a confidently wrong command
 * in a dashboard is worse than none: it gets pasted.
 */
function removalGroup(g, rep) {
  const many = g.items.length > 1;
  const head = el('div', { class: 'inv-group-head' },
    el('div', { class: 'inv-group-id' },
      el('span', { class: 'inv-group-name comp-name', text: g.label }),
      g.sub ? el('span', { class: 'pill neutral', text: g.sub }) : kindPill(g.items[0]),
      many ? el('span', { class: 'section-note', text: num(g.items.length) + ' declarations' }) : null),
    el('div', { class: 'inv-group-cost' },
      el('span', { class: 'inv-group-usd' }, money(g.usd, g.priced)),
      el('span', { class: 'section-note', text: num(g.tokens) + ' tok in every request · '
        + compact(g.reads) + ' read for nothing' })));
  const wrap = el('div', { class: 'inv-group', 'data-testid': 'inv-group-' + g.key }, head);
  // The fix, visible and copyable, at group level. This is the answer to the question the
  // page is opened with, so it is not behind a disclosure and not in a detail view.
  wrap.appendChild(removalCell(g.items[0], many
    ? 'Removes all ' + num(g.items.length) + ' of them'
    : ''));
  // A group of ONE has already said its name and its cost in the head, so it does not get a
  // member list repeating both. It still needs the two things the row carries and the head does
  // not — the evidence sentence, and the reveal for what it actually puts in the prompt — so
  // those are lifted into the card.
  if (!many) {
    wrap.appendChild(soloRowExtras(g.items[0], rep));
    return wrap;
  }
  const list = el('div', { class: 'cg-items' });
  for (const t of g.items) list.appendChild(unusedRow(t, rep));
  if (g.items.length > 6) {
    wrap.appendChild(el('details', { class: 'why' },
      el('summary', {}, 'The ' + num(g.items.length) + ' declarations, and the evidence for each'),
      list));
  } else {
    wrap.appendChild(list);
  }
  return wrap;
}

/**
 * soloRowExtras is the evidence and the prompt-text reveal for a group of one.
 *
 * Separate from unusedRow because the checkbox and the name would be a second copy of the card
 * head. The basis sentence is NOT optional here for the same reason it is not optional there:
 * "never invoked in 5 sessions" and "never invoked in 400" are different claims and the reader
 * has to be able to tell which one they are being shown.
 */
function soloRowExtras(t, rep) {
  const c = rep.coverage;
  const wrap = el('div', { class: 'inv-solo', 'data-testid': 'inv-item-' + t.kind + '/' + t.name },
    el('span', { class: 'cg-item-basis', 'data-testid': 'inv-basis-' + t.kind + '/' + t.name },
      'Unused across ' + num(t.sessions_declared) + ' of your ' + num(c.captured)
      + ' captured session' + (c.captured === 1 ? '' : 's') + ' — ' + rangeLabel() + '.'));
  wrap.appendChild(promptTextReveal(t));
  return wrap;
}

/**
 * unusedRow is one candidate, its evidence, and its switch.
 *
 * The checkbox and the name are inside a <label> and NOTHING ELSE IS. A copy button inside a
 * label is a button that toggles the checkbox when you click it, which is how an affordance
 * added for convenience becomes a change nobody meant to make.
 */
function unusedRow(t, rep) {
  const c = rep.coverage;
  const key = t.kind + '/' + t.name;
  const off = !!(tools.control && tools.control.excluded
    && tools.control.excluded.some((e) => e.kind === t.kind && e.name === t.name));
  // Only a provider-side tool is inert AS A ROW: it can never be dropped here, whatever this
  // proxy supports. A missing endpoint disables every switch, but that is one fact about the
  // page, stated once by renderUnused — repeating it under forty rows buries the evidence
  // sentence that is the point of the row.
  const fixed = t.kind === 'server_tool';
  const box = el('div', {
    class: 'cg-item' + (off ? ' cg-off' : '') + (fixed ? ' cg-fixed' : ''),
    'data-testid': 'inv-item-' + key,
  });
  const cb = el('input', {
    type: 'checkbox', 'data-testid': 'inv-toggle-' + key,
    id: 'inv-cb-' + key,
    disabled: fixed || !tools.control || tools.busy === key,
    checked: off ? 'checked' : null,
    onchange: (ev) => toggleExcluded(t, ev.currentTarget.checked),
  });
  box.appendChild(cb);
  box.appendChild(el('label', { class: 'cg-item-main', for: 'inv-cb-' + key },
    el('span', { class: 'cg-item-name comp-name', text: t.name }),
    kindPill(t),
    t.server ? el('span', { class: 'cg-item-srv', text: t.server }) : null,
    el('span', { class: 'cg-item-weight' },
      num(t.tokens) + ' tok/request · ' + compact(t.unused_reads) + ' read for nothing · '),
    el('span', { class: 'cg-item-usd' }, money(t.unused_usd, t.priced))));
  // The basis. One sentence, always present, always naming the denominator and the window.
  box.appendChild(el('span', { class: 'cg-item-basis', 'data-testid': 'inv-basis-' + key },
    'Unused across ' + num(t.sessions_declared) + ' of your ' + num(c.captured)
    + ' captured session' + (c.captured === 1 ? '' : 's') + ' — ' + rangeLabel() + '.'));
  if (fixed) {
    box.appendChild(el('span', { class: 'comp-warn' }, 'Provider-side tool. It is part of the '
      + 'API request the agent builds, not something this proxy declares, so it cannot be '
      + 'dropped here.'));
  }
  if (off) {
    box.appendChild(el('span', { class: 'cg-item-basis', text: 'Opted out. Switch back on to '
      + 'restore it; that pays the one-time prefix rebuild again.' }));
  }
  box.appendChild(promptTextReveal(t));
  return box;
}

/**
 * reversibilityNote states the one-time cost of acting, up front rather than in a
 * changelog after the fact.
 *
 * Removing a declaration changes the cached prompt PREFIX, and a changed prefix is not a
 * cheap event: the next turn of every affected session re-reads nothing and re-writes the
 * whole prompt at the cache-creation rate. It is worth paying once for a batch and
 * absurd to pay once per item, so the note says that rather than leaving the reader to
 * discover it from the Overview's prefix-change line.
 */
function reversibilityNote() {
  return el('details', { class: 'why', 'data-testid': 'inv-reversibility' },
    el('summary', {}, 'What opting out does, what it costs once, and how to undo it'),
    el('p', {}, 'Opting an item out stops context-guru declaring it on later requests. It is '
      + 'fully reversible: switching it back on restores the declaration exactly, and nothing '
      + 'about your agent\'s own configuration is edited.'),
    el('p', {}, 'The one-time cost is a cache miss. Tool declarations sit at the very front of '
      + 'the cached prompt, so changing the set changes the prefix: the next turn of each '
      + 'session in flight re-writes its whole prompt at the cache-CREATION rate instead of '
      + 'reading it back. That is a real charge, it is larger per turn than the saving per '
      + 'turn, and it is paid once — which is why you should switch off everything you mean to '
      + 'switch off in one pass, and why toggling an item back and forth pays it every time.'),
    el('p', {}, 'Sessions already running keep whatever they declared at their first turn. The '
      + 'change lands on the next new session, and on the next prefix rebuild of the old ones.'));
}

/** unusedRow is one candidate, its evidence, and its switch. */
function unusedRow(t, rep) {
  const c = rep.coverage;
  const key = t.kind + '/' + t.name;
  const off = !!(tools.control && tools.control.excluded
    && tools.control.excluded.some((e) => e.kind === t.kind && e.name === t.name));
  // A provider-side tool is declared by the API, not by the client, so there is nothing
  // here to stop sending. Saying so beats a switch that silently does nothing.
  // Only a provider-side tool is inert AS A ROW: it can never be dropped here, whatever
  // this proxy supports. A missing endpoint disables every switch, but that is one fact
  // about the page, stated once by renderUnused — repeating it under all forty rows buries
  // the evidence sentence that is the point of the row.
  const fixed = t.kind === 'server_tool';
  const why = fixed ? 'Provider-side tool. It is part of the API request the agent builds, not '
    + 'something this proxy declares, so it cannot be dropped here.' : '';
  const box = el('label', {
    class: 'cg-item' + (off ? ' cg-off' : '') + (fixed ? ' cg-fixed' : ''),
    'data-testid': 'inv-item-' + key,
  });
  const cb = el('input', {
    type: 'checkbox', 'data-testid': 'inv-toggle-' + key,
    disabled: fixed || !tools.control || tools.busy === key,
    checked: off ? 'checked' : null,
    onchange: (ev) => toggleExcluded(t, ev.currentTarget.checked),
  });
  box.appendChild(cb);
  box.appendChild(el('span', { class: 'cg-item-main' },
    el('span', { class: 'cg-item-name comp-name', text: t.name }),
    kindPill(t),
    t.server ? el('span', { class: 'cg-item-srv', text: t.server }) : null,
    el('span', { class: 'cg-item-weight' },
      num(t.tokens) + ' tok/request · ' + compact(t.unused_reads) + ' read for nothing · '),
    el('span', { class: 'cg-item-usd' }, money(t.unused_usd, t.priced))));
  // The basis. One sentence, always present, always naming the denominator and the window.
  box.appendChild(el('span', { class: 'cg-item-basis', 'data-testid': 'inv-basis-' + key },
    'Unused across ' + num(t.sessions_declared) + ' of your ' + num(c.captured)
    + ' captured session' + (c.captured === 1 ? '' : 's') + ' — ' + rangeLabel() + '.'));
  if (why) box.appendChild(el('span', { class: 'comp-warn', text: why }));
  if (off) {
    box.appendChild(el('span', { class: 'cg-item-basis', text: 'Opted out. Switch back on to '
      + 'restore it; that pays the one-time prefix rebuild again.' }));
  }
  return box;
}

/**
 * renderRealized reports what removals ACTUALLY avoided, and reports nothing at all when
 * the filter has no accounting to offer.
 *
 * This is honesty rule 2, and the reason the projection cannot be reused here: the
 * projection is what the corpus says COULD have been avoided, which is a statement about
 * the past under a counterfactual. A realized saving is a statement about requests that
 * were actually sent with a smaller declaration set. Substituting one for the other is
 * exactly the flattering mistake, so an absent realized figure prints as absent.
 */
function renderRealized(host) {
  const r = tools.control && tools.control.realized;
  const panel = el('div', { class: 'panel', 'data-testid': 'inv-realized' },
    el('div', { class: 'section' }, el('h2', {}, 'Realized by your removals'),
      el('span', { class: 'section-note' }, 'measured on requests actually sent — not a projection')));
  host.appendChild(panel);
  if (!r) {
    emptyState(panel.appendChild(el('div')), 'Nothing removed yet, so nothing realized',
      'This panel only ever shows what a removal actually avoided on requests that were '
      + 'really sent. The projected figure above is a different measurement and is not '
      + 'copied down here.');
    return;
  }
  panel.appendChild(el('div', { class: 'tiles' },
    tile('inv-real-usd', 'Actually not billed', r.priced ? usd(r.usd) : compact(r.reads) + ' tok',
      r.priced ? 'since ' + when(r.since) : 'unpriced model; tokens only', r.usd ? 'good' : ''),
    tile('inv-real-reads', 'Tokens not re-read', compact(r.reads),
      num(r.requests || 0) + ' requests sent without them'),
    tile('inv-real-items', 'Items opted out', num((tools.control.excluded || []).length),
      'reversible, one prefix rebuild each')));
}

/** toggleExcluded posts one change and repaints from the server's answer, not from the DOM. */
async function toggleExcluded(t, on) {
  const key = t.kind + '/' + t.name;
  tools.busy = key;
  try {
    const res = await fetch('/api/toolfilter', {
      method: 'POST',
      headers: { 'content-type': 'application/json', accept: 'application/json' },
      body: JSON.stringify({
        kind: t.kind, name: t.name, server: t.server || '',
        action: on ? 'exclude' : 'include',
      }),
    });
    if (!res.ok) throw new Error(res.status + ' ' + res.statusText);
    tools.control = await res.json();
  } catch (err) {
    // The switch must not look like it worked. Repaint from the last known server state.
    alert('Could not change this: ' + ((err && err.message) || err));
  } finally {
    tools.busy = '';
    renderTools();
  }
}

// ── tables ─────────────────────────────────────────────────────────────────

/** tsortable wires this page's own headers to this page's own sort state. */
function tsortable(table, keys, onSort) {
  $$('thead th', table).forEach((th, i) => {
    const key = keys[i];
    if (!key) return;
    const label = th.textContent;
    th.setAttribute('aria-sort', key === tools.sort
      ? (tools.dir === 'asc' ? 'ascending' : 'descending') : 'none');
    clear(th).appendChild(el('button', {
      class: 'sort', title: 'Sort by ' + label,
      onclick: () => {
        tools.dir = tools.sort === key && tools.dir === 'desc' ? 'asc' : 'desc';
        tools.sort = key;
        onSort();
      },
    }, label));
  });
}

/**
 * toolTable is the per-tool detail, sortable by any column.
 *
 * Client-side sorting is correct here specifically because /api/tools returns the WHOLE
 * inventory for the scope — there is no pagination for a sort to lie about. (That is the
 * same reason app.js sorts Components and refuses to sort Sessions.)
 */
function toolTable(host, rep) {
  const panel = el('div', { class: 'panel' },
    el('h2', {}, 'Every declaration, by weight'),
    el('p', { class: 'note' }, 'Everything you added, sorted by what it cost to carry unused. '
      + '"Read for nothing" is this item\'s size times the requests that re-read it in the '
      + "sessions that never called it. Claude Code's own tools are in their own section at "
      + 'the end of this page.'));
  const table = el('table', { class: 'tbl', 'data-testid': 'inv-tools-table' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Name'), el('th', {}, 'Kind'),
      el('th', { class: 'num' }, 'Tokens'), el('th', { class: 'num' }, 'Sessions'),
      el('th', { class: 'num' }, 'Used in'), el('th', { class: 'num' }, 'Calls'),
      el('th', { class: 'num' }, 'Read for nothing'), el('th', { class: 'num' }, 'Cost of that'))));
  const body = el('tbody');
  table.appendChild(body);
  panel.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, table));
  host.appendChild(panel);
  tsortable(table, TOOL_COLS, () => renderTools());
  const listed = rep.tools.filter((x) => !x.builtin && x.kind !== 'server_tool');
  if (!listed.length) {
    tableMessage(body, 8, 'No declarations captured',
      'Nothing in this scope recorded what it declared.');
    return;
  }
  // Same exclusion as the actionable list: the agent's own tools have their own section at
  // the end of the page, so this table is the things a reader can actually decide about.
  for (const t of sortRows(rep.tools.filter((x) => !x.builtin && x.kind !== 'server_tool'),
    tools.sort, tools.dir)) {
    body.appendChild(el('tr', { class: t.sessions_used ? '' : 'cg-row-unused' },
      el('td', {}, el('span', { class: 'comp-name trunc', title: t.name, text: t.name })),
      el('td', {}, kindPill(t), t.server ? el('span', { class: 'cg-item-srv', text: t.server }) : null),
      numTd(t.tokens), numTd(t.sessions_declared),
      el('td', { class: 'num' }, t.sessions_used
        ? num(t.sessions_used)
        : el('span', { class: 'pill missing' }, 'never')),
      numTd(t.calls), readTd(t.unused_reads),
      el('td', { class: 'num' }, money(t.unused_usd, t.priced))));
  }
}

/**
 * serverTable is the MCP rollup, and it is here because it is the unit of the DECISION: a
 * user adds and removes an MCP server, not one of its nineteen tools. A per-tool table
 * alone makes the reader do that addition in their head.
 */
function serverTable(host, rep) {
  if (!rep.servers.length) return;
  const panel = el('div', { class: 'panel' },
    el('h2', {}, 'MCP servers'),
    el('p', { class: 'note' }, 'One row per server, because that is what you add or remove. '
      + 'A server whose tools are all unused is one decision, not many.'));
  const table = el('table', { class: 'tbl', 'data-testid': 'inv-servers-table' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Server'), el('th', { class: 'num' }, 'Tools'),
      el('th', { class: 'num' }, 'Tools used'), el('th', { class: 'num' }, 'Tokens'),
      el('th', { class: 'num' }, 'Sessions'), el('th', { class: 'num' }, 'Calls'),
      el('th', { class: 'num' }, 'Read for nothing'), el('th', { class: 'num' }, 'Cost of that'))));
  const body = el('tbody');
  table.appendChild(body);
  panel.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, table));
  host.appendChild(panel);
  for (const s of rep.servers) {
    body.appendChild(el('tr', { class: s.tools_used ? '' : 'cg-row-unused' },
      el('td', {}, el('span', { class: 'comp-name', text: s.server })),
      numTd(s.tools),
      el('td', { class: 'num' }, s.tools_used
        ? num(s.tools_used) + ' of ' + num(s.tools)
        : el('span', { class: 'pill missing' }, 'none of ' + num(s.tools))),
      numTd(s.tokens), numTd(s.sessions_declared), numTd(s.calls),
      readTd(s.unused_reads),
      el('td', { class: 'num' }, money(s.unused_usd, s.priced))));
  }
}

/**
 * skillsPanel is the skills half, reported apart from tools because it can be UNKNOWN.
 *
 * A skills listing is prose in the system prompt, not JSON, so a Claude Code version whose
 * format moved gives a listing whose SIZE is known and whose CONTENTS are not. That third
 * state gets its own panel copy: "unknown" must never render as "no skills", which is the
 * same failure as not_captured rendering as zero.
 */
function skillsPanel(host, rep) {
  const s = rep.skills;
  const panel = el('div', { class: 'panel', 'data-testid': 'inv-skills' },
    el('h2', {}, 'Skills'));
  host.appendChild(panel);
  if (s.state === 'absent') {
    emptyState(panel.appendChild(el('div')), 'No skills listing in this scope',
      'No session here carried the Skill tool\'s listing.');
    return;
  }
  panel.appendChild(el('p', { class: 'note' },
    'Skills are declared as prose in the system prompt, and the listing is ONE indivisible '
    + 'block: it is waste only in a session that invoked no skill at all.'));
  if (s.state === 'unknown') {
    panel.appendChild(el('div', { class: 'state blocked', 'data-testid': 'inv-skills-unknown' },
      el('div', { class: 'state-body' },
        el('strong', {}, 'A skills listing was present in ' + num(s.unknown_sessions)
          + ' session' + (s.unknown_sessions === 1 ? '' : 's') + ' and could not be read'),
        el('span', {}, 'Its size is honest — ' + num(s.listing_tokens) + ' tokens per request '
          + '— and its contents are not known. This is not "you have no skills": it is an '
          + 'agent version whose listing format this parser does not recognise.'))));
  }
  panel.appendChild(el('div', { class: 'tiles' },
    tile('inv-skill-listing', 'Listing weight', num(s.listing_tokens) + ' tok',
      'carried on every request of a session that has skills'),
    tile('inv-skill-declared', 'Skills declared', num(s.declared),
      s.state === 'unknown' ? 'as far as the parser could read' : 'from the listing'),
    tile('inv-skill-invoked', 'Ever invoked', num(s.invoked),
      num(s.calls) + ' calls', s.invoked ? 'good' : s.declared ? 'bad' : ''),
    tile('inv-skill-waste', 'Listing read for nothing', compact(s.unused_listing_reads),
      'in sessions that invoked no skill'),
    // `priced` and not "is the figure non-zero": a zero on a priced corpus means the
    // listing was never wasted, and a zero on an unpriced one means nobody knows.
    tile('inv-skill-usd', 'Cost of that',
      rep.totals.priced ? usd(s.unused_listing_usd) : 'unpriced',
      rep.totals.priced ? 'the listing\'s own weight, priced'
        : 'no rates for the model that carried it')));
  if (!(s.skills || []).length) return;
  const table = el('table', { class: 'tbl compact', 'data-testid': 'inv-skills-table' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Skill'), el('th', { class: 'num' }, 'Tokens'),
      el('th', { class: 'num' }, 'Sessions'), el('th', { class: 'num' }, 'Used in'),
      el('th', { class: 'num' }, 'Calls'), el('th', { class: 'num' }, 'Read for nothing'),
      el('th', { class: 'num' }, 'Cost of that'))));
  const body = el('tbody');
  table.appendChild(body);
  for (const t of s.skills) {
    body.appendChild(el('tr', { class: t.sessions_used ? '' : 'cg-row-unused' },
      el('td', {}, el('span', { class: 'comp-name trunc', title: t.name, text: t.name })),
      numTd(t.tokens), numTd(t.sessions_declared),
      el('td', { class: 'num' }, t.sessions_used
        ? num(t.sessions_used) : el('span', { class: 'pill missing' }, 'never')),
      numTd(t.calls), readTd(t.unused_reads),
      el('td', { class: 'num' }, money(t.unused_usd, t.priced))));
  }
  panel.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, table));
}

/**
 * renderComposition is "who owns the prompt", as one part-to-whole bar.
 *
 * FORM: a horizontal stacked bar, which is what the data's job asks for — part-to-whole with
 * long-named categories. Not a pie (five parts, one of them 1%, and a pie makes the two
 * middle ones indistinguishable), not a treemap (it would imply a hierarchy that is not in
 * the data), and not five tiles (a tile row cannot show that these are shares of one thing).
 *
 * COLOR: the four groups a reader can ACT on take the four categorical slots; the group they
 * must not act on takes the de-emphasis gray. That is the one design decision in this panel
 * worth defending, and it is the owner's request rendered rather than annotated: "separate
 * between built in tools to others that can and should be changed" is a statement about
 * COLOR, because color is what the eye reads before any label. Four hues of equal weight
 * asserted that Claude Code's own equipment was a fifth removable category, which is exactly
 * the reading that would get somebody to delete Read.
 *
 * Also, and it is the point of this round: the SYSTEM PROMPT is in the bar. It is normally the
 * largest single region of the prefix, and leaving it out made a "composition of your prompt"
 * that was a composition of the tools array — a complete-looking answer to a question it was
 * not answering.
 */
function renderComposition(host, rep) {
  const all = (rep.tools || []).concat(rep.skills.skills || []);
  const sum = (f) => all.filter(f).reduce((n, t) => n + t.tokens, 0);
  const mcp = sum((t) => t.kind === 'mcp_tool');
  const provider = sum((t) => t.kind === 'server_tool');
  const builtin = sum((t) => t.builtin);
  const client = sum((t) => t.kind === 'tool' && !t.builtin);
  // Skills contribute their LISTING and nothing else. Each skill row's token weight is the
  // weight of its own ENTRY IN that listing — a sub-slice of the same block — so adding the
  // rows to the listing counts every skill twice. It did: the segments summed to 43,933 while
  // the panel printed 36,950 beside them as "the whole that each share is a part of", and the
  // legend percentages were shares of the inflated figure. The server's own denominator
  // (declared_set_tokens = listing + every tool) is the one that partitions.
  const skills = rep.skills.listing_tokens || 0;
  const system = (rep.prompt && rep.prompt.tokens) || 0;
  const declared = rep.totals.declared_set_tokens || (mcp + provider + builtin + client + skills);
  const whole = declared + system;

  const panel = el('div', { class: 'panel', id: 'inv-composition', 'data-testid': 'inv-composition' },
    el('div', { class: 'section' },
      el('h2', {}, 'Who owns your system prompt'),
      el('span', { class: 'section-note' }, num(whole) + ' tokens in front of every request')),
    el('p', { class: 'note' }, 'Everything that goes out before your conversation does, grouped '
      + 'by whether it is yours to remove. Coloured groups are choices you can change; the grey '
      + 'one is the agent\'s own equipment and removing any of it breaks the agent rather than '
      + 'slimming it.'));
  host.appendChild(panel);

  // Actionable groups first and in the fixed categorical order, so a filter that empties one
  // does not repaint the survivors — the color of a group follows the GROUP, never its rank.
  const groups = [
    { label: 'MCP tools — yours to remove', value: mcp, color: SERIES[0] },
    { label: 'The skills listing — yours to remove', value: skills, color: SERIES[1],
      note: 'one block; each skill is an entry inside it, not a separate weight' },
    { label: 'Other client tools — whatever agent sent these', value: client, color: SERIES[2] },
    { label: 'Your system prompt — yours, but not from this page', value: system, color: SERIES[3],
      note: 'change it in CLAUDE.md, your output style or your agent definition' },
    { label: "Claude Code built-ins and provider tools — do not remove",
      value: builtin + provider, color: 'var(--s-mute)' },
  ].filter((g) => g.value > 0);
  stackedShare(panel.appendChild(el('div')), groups, {
    testid: 'inv-share', format: num,
    note: 'Drawn to scale.' + (system ? '' : ' No session in this window recorded a system '
      + 'prompt, so that region is absent rather than zero — see the panel below.'),
  });

  // The per-item detail: ONE measure across many long-named categories, so ONE hue. Twelve
  // colours here would assert that twelve different things are being plotted.
  const top = all.filter((t) => t.tokens > 0 && !t.builtin)
    .sort((a, b) => b.tokens - a.tokens).slice(0, 12);
  if (top.length) {
    panel.appendChild(el('h3', {}, 'The heaviest removable declarations'));
    barRows(panel.appendChild(el('div', { 'data-testid': 'inv-heaviest' })), top.map((t) => ({
      label: t.name,
      value: t.tokens,
      display: num(t.tokens) + ' tok  ' + pct(t.share_pct, 1),
      formula: (t.sessions_used ? num(t.calls) + ' calls in ' + num(t.sessions_used)
        + ' of ' + num(t.sessions_declared) + ' sessions' : 'never invoked in '
        + num(t.sessions_declared) + ' session' + (t.sessions_declared === 1 ? '' : 's')),
    })));
    panel.appendChild(el('p', { class: 'hint' }, 'Share is of the ' + num(declared)
      + ' tokens of DECLARATIONS, not of the whole prefix above — the system prompt is not a '
      + 'declaration and is not in that denominator.'));
  }
}

/**
 * renderRemovalValue is "what would I actually save", per model and across a session.
 *
 * Two figures, because they differ by two orders of magnitude and quoting either alone
 * misleads in a different direction:
 *
 *   - The FIRST request pays the cache-CREATION rate for the declaration, once.
 *   - Every later turn of the session re-reads it at the cache-READ rate, about a tenth as
 *     much each — but there are ~150 of them, so this is where the money is.
 *
 * Per model, because the rates differ by 2.5x across the models this account actually ran on,
 * and one blended figure would belong to none of them.
 */
function renderRemovalValue(host, rep) {
  const models = (rep.models || []).filter((m) => m.priced);
  const panel = el('div', { class: 'panel', 'data-testid': 'inv-removal-value' },
    el('h2', {}, 'What removing something is worth'),
    el('p', { class: 'note' }, 'Per 1,000 tokens of declarations dropped. Prices differ per '
      + 'model, so this is per model rather than blended.'));
  host.appendChild(panel);
  if (!models.length) {
    emptyState(panel.appendChild(el('div')), 'No priced models in this window',
      'These sessions ran on models with no known rates, so a token weight cannot be turned '
      + 'into a dollar figure. The token columns are still exact.');
    return;
  }
  // The session multiplier, named, with the statistic it is. This has to be explicit: the
  // median session here is ONE request and the plain mean under four, because most sessions
  // are one-request sidechains — so either of those would say carrying a declaration costs
  // about one re-read, when the sessions the money is in run to ~150 turns.
  const turns = rep.totals.requests_per_session_typical || rep.totals.requests_per_session || 1;
  const med = rep.totals.requests_per_session_median;
  panel.appendChild(el('div', { class: 'state', 'data-testid': 'inv-session-basis' },
    el('div', { class: 'state-body' },
      el('strong', {}, 'Session length used: ' + turns.toFixed(0) + ' requests'),
      el('span', {}, 'Measured from this account\'s own history, not assumed. It is the '
        + 'request-weighted average over the ' + num(rep.coverage.captured) + ' CAPTURED '
        + 'session' + (rep.coverage.captured === 1 ? '' : 's') + ' — how many turns the session '
        + 'that a TYPICAL REQUEST belongs to runs for.'),
      // The headline tile quotes a different multiplier over a different population, and the
      // two are reconciled HERE, where the reader meets the second one — not left as an
      // apparent contradiction for them to resolve.
      el('span', {}, 'The "Never invoked" tile at the top quotes '
        + rep.totals.requests_per_session.toFixed(1) + '× instead. That is the PLAIN mean over '
        + 'all ' + num(rep.coverage.sessions) + ' tool-bearing sessions in scope, captured or '
        + 'not. Neither figure is wrong: one answers "how long is a session", this one answers '
        + '"how long is the session a typical request belongs to", and the second is the one a '
        + 'per-session cost has to be projected from.'),
      // The explanation is CONDITIONAL on the median actually being small, because the
      // reason ("most sessions are one-request sidechains") is a fact about a particular
      // population and not about the statistic. Printed unconditionally it told a reader
      // whose median was 244 requests that most of their sessions were single calls, which
      // is both false and a very odd thing for a page to say about the reader's own history.
      el('span', {}, med < turns / 2
        ? 'The MEDIAN session over the same ' + num(rep.coverage.captured) + ' session'
          + (rep.coverage.captured === 1 ? '' : 's') + ' is only ' + num(med) + ' request'
          + (med === 1 ? '' : 's') + ', because short sessions outnumber long ones — a title '
          + 'generation, a single tool call. Projecting a per-session cost from the median '
          + 'would understate it by ' + (med > 0 ? (turns / med).toFixed(0) + 'x' : 'far')
          + ', so the weighted figure is the one used here.'
        : 'The MEDIAN session over the same ' + num(rep.coverage.captured) + ' session'
          + (rep.coverage.captured === 1 ? '' : 's') + ' is ' + num(med) + ' request'
          + (med === 1 ? '' : 's') + ', close to the weighted figure — so on this scope the '
          + 'two statistics agree and either would do. They part company on a scope with many '
          + 'short sessions, which is why the weighted one is what the table uses.'),
      // n, on the figure a dollar projection rests on. This project has concluded a difference
      // from too few samples more than once; a basis panel is the place to stop doing that.
      el('span', {}, 'Both figures are over the ' + num(rep.coverage.captured) + ' session'
        + (rep.coverage.captured === 1 ? '' : 's') + ' whose inventory was captured'
        + (rep.coverage.captured < 20
          ? ' — a small sample, so treat the per-session column below as indicative rather '
            + 'than measured.' : '.')))));

  const table = el('table', { class: 'tbl' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Model'),
      el('th', { class: 'num' }, 'Requests'),
      el('th', { class: 'num', title: 'The declaration enters the prompt at the cache-creation rate, once.' },
        'First request'),
      el('th', { class: 'num', title: 'Every later turn re-reads it at the cache-read rate.' },
        'Full session (' + turns.toFixed(0) + ' turns)'))));
  const body = el('tbody');
  for (const m of models) {
    const first = 1000 * m.cache_write_usd_per_token;
    const session = first + 1000 * m.cache_read_usd_per_token * Math.max(0, turns - 1);
    body.appendChild(el('tr', {},
      el('td', {}, el('code', { text: m.model })),
      el('td', { class: 'num', text: num(m.requests) }),
      el('td', { class: 'num', text: usd(first) }),
      el('td', { class: 'num', text: usd(session) })));
  }
  table.appendChild(body);
  panel.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, table));
  panel.appendChild(el('p', { class: 'hint' },
    'To value one item, multiply by its token weight ÷ 1,000. The heaviest removable '
    + 'declaration in this scope is '
    + num(Math.max(0, ...((rep.tools || []).filter((t) => !t.builtin).map((t) => t.tokens))))
    + ' tokens.'));
}

/**
 * renderBuiltins is the built-in tools, LAST on the page, collapsed, behind a warning.
 *
 * They are here because they are a real and large share of the prompt and hiding that would be
 * dishonest. They are collapsed and warned about because they are the one group on this page
 * where acting on the number is a mistake: removing Read or Bash does not slim the agent, it
 * removes a capability the model is expected to have, and everything that depended on it fails.
 * The page's whole job is to make waste easy to act on, which is exactly why the not-waste has
 * to be made hard to act on in the same breath.
 */
function renderBuiltins(host, rep) {
  const rows = (rep.tools || []).filter((t) => t.builtin || t.kind === 'server_tool');
  if (!rows.length) return;
  const tok = rows.reduce((n, t) => n + t.tokens, 0);
  // The warning belongs on the CLOSED bar. It used to live only inside the expanded panel, so
  // the thing a first-time reader actually saw was a plain grey strip with a neutral heading —
  // indistinguishable from the collapsed detail panels elsewhere on the page, and reading as
  // "more of the same, but folded away" rather than as "this is the one section that is not a
  // saving". A danger that is only visible after you have opened the door is decoration.
  const det = el('details', { class: 'panel inv-builtins', 'data-testid': 'inv-builtins' },
    el('summary', {},
      el('span', { class: 'pill bad' }, 'not a saving'),
      el('span', { class: 'inv-builtins-title' }, "The agent's own tools — " + num(rows.length)
        + ' items, ' + num(tok) + ' tokens, ' + pct(100 * tok
        / Math.max(1, rep.totals.declared_set_tokens), 0) + ' of what you declare'),
      el('span', { class: 'section-note' },
        'removing any of these breaks the agent — expand only if you know why you are here')));
  det.appendChild(el('div', { class: 'state blocked', 'data-testid': 'inv-builtins-danger' },
    el('div', { class: 'state-body' },
      el('strong', {}, 'Removing any of these will break the agent'),
      el('span', {}, 'These are Claude Code\'s own tools and the provider\'s. They are '
        + 'not unused capacity you are wasting money on — they are the equipment the model is '
        + 'expected to have, and a session missing one does not degrade gracefully: anything '
        + 'that needed it simply fails.'),
      el('span', {}, 'They are listed because they are ' + pct(100 * tok
        / Math.max(1, rep.totals.declared_set_tokens), 0) + ' of what you carry and pretending '
        + 'otherwise would be dishonest. Acting on that number is almost always the wrong call.'),
      el('span', {}, 'A low "never invoked" count here is normal and is NOT a reason to remove '
        + 'one: a tool that goes uncalled for fifty sessions is doing its job by being '
        + 'available on the fifty-first.'))));
  const table = el('table', { class: 'tbl' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Tool'), el('th', {}, 'Kind'), el('th', { class: 'num' }, 'Tokens'),
      el('th', { class: 'num' }, 'Share'), el('th', { class: 'num' }, 'Sessions used'),
      el('th', {}, 'If you really must'))));
  const body = el('tbody');
  for (const t of rows.slice().sort((a, b) => b.tokens - a.tokens)) {
    body.appendChild(el('tr', {},
      el('td', {}, el('code', { text: t.name })),
      el('td', {}, kindPill(t)),
      numTd(t.tokens),
      el('td', { class: 'num', text: pct(t.share_pct, 1) }),
      el('td', { class: 'num' }, t.sessions_used
        ? num(t.sessions_used) : el('span', { class: 'pill missing' }, 'never')),
      el('td', {}, removalCell(t))));
  }
  table.appendChild(body);
  det.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, table));
  host.appendChild(det);
}

/**
 * removalCell is the exact thing a user runs, in the form its kind actually needs.
 *
 * Four different mechanisms, and getting them wrong is worse than saying nothing, so the
 * server decides which applies (see dash/toolremoval.go) and this only renders it. The one
 * semantic worth repeating at every call site: the snippets use the BARE tool name, because a
 * scoped permission rule blocks the call and leaves the declaration in the prompt — i.e.
 * saves exactly nothing, which is the opposite of what a reader of this page wants.
 *
 * `scope` is an optional line naming what the command takes with it (for an MCP server, every
 * tool it declares). It is stated because a reader about to paste `claude mcp remove x` should
 * know it is not a per-tool command before they run it, not after.
 */
function removalCell(t, scope) {
  const r = t.removal || {};
  const wrap = el('div', { class: 'inv-removal' });
  if (r.danger) wrap.appendChild(el('span', { class: 'pill bad', title: r.note }, 'breaks the agent'));
  if (!r.command && !r.settings) {
    wrap.appendChild(el('span', { class: 'hint', text: r.effect || 'No known mechanism.' }));
    if (r.note) wrap.appendChild(whyBlock('Why not', r.note, 'Why ' + t.name + ' cannot be removed here'));
    return wrap;
  }
  if (scope) wrap.appendChild(el('span', { class: 'section-note', text: scope }));
  if (r.command) wrap.appendChild(copyBox(r.command, 'Run this'));
  if (r.settings) {
    wrap.appendChild(copyBox(r.settings, 'Or add to ' + (r.settings_path || 'settings.json')));
  }
  wrap.appendChild(whyBlock('What this does', (r.effect || '') + (r.note ? ' ' + r.note : ''),
    'What removing ' + t.name + ' does'));
  return wrap;
}

/**
 * copyBox renders a copy-pasteable snippet with a button that copies it.
 *
 * The text is SELECTABLE regardless, and the button is an addition rather than the only route:
 * the clipboard API is unavailable over plain HTTP on some browsers and silently rejects in
 * others, so a copy affordance that is the only way to get the text is a dead end. On failure
 * it says so rather than pretending it worked.
 */
function copyBox(text, label) {
  const btn = el('button', { class: 'btn small', type: 'button' }, 'Copy');
  btn.addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(text);
      btn.textContent = 'Copied';
    } catch (_) {
      btn.textContent = 'Select it manually';
    }
    setTimeout(() => { btn.textContent = 'Copy'; }, 2000);
  });
  return el('div', { class: 'inv-copy' },
    el('div', { class: 'inv-copy-head' }, el('span', { class: 'section-note', text: label }), btn),
    el('pre', { class: 'inv-snippet', text: text }));
}

// ── the prompt text itself ──────────────────────────────────────────────────

/**
 * The prompt text, fetched once per report and shared by every reveal on the page.
 *
 * Its own request rather than fields on /api/tools, because it is tens of kilobytes for a
 * real catalogue and almost nobody opens this page to read a tool schema. Its own STATE
 * rather than a fetch per reveal, because forty reveals would be forty requests for one
 * session's rows.
 *
 * `state` is one of: 'idle' (not asked for yet), 'loading', 'ok', 'absent' (the endpoint is
 * not there — an older proxy), 'error'.
 */
const prompt = { state: 'idle', view: null, byName: new Map() };

/**
 * loadPrompt fetches the prefix text once and repaints only the reveals that are open.
 *
 * Deliberately NOT a renderTools() — that clears the view and rebuilds every <details>, so
 * the reveal whose click started the fetch would close itself the moment the text arrived.
 * Each reveal registers a repaint of its own body instead.
 */
async function loadPrompt() {
  if (prompt.state === 'loading' || prompt.state === 'ok') return;
  prompt.state = 'loading';
  repaintPrompt();
  try {
    const v = await api('prompt');
    prompt.view = v;
    prompt.byName = new Map();
    for (const r of v.regions || []) prompt.byName.set(r.kind + '/' + r.name, r);
    prompt.state = 'ok';
  } catch (err) {
    if (aborted(err)) return;
    // A 404 is an older proxy that has the report and not this route. That is "the feature
    // is not there", not "something broke", and the two must not read the same.
    prompt.state = /404|not found/i.test(String((err && err.message) || err)) ? 'absent' : 'error';
  }
  repaintPrompt();
}

/** repaintPrompt reruns every registered reveal's own paint. */
function repaintPrompt() {
  for (const fn of promptWaiters) {
    try { fn(); } catch (_) { /* a detached node is not an error worth surfacing */ }
  }
}

/**
 * promptTextReveal is one capability's own slice of the prompt, on demand.
 *
 * Four states, and the difference between them is the whole honesty of this panel:
 *
 *   - has_text false and nothing captured for the account: transcript storage is off, and
 *     the message names WHICH party can turn it on (see /api/prompt's blocked_by). Never an
 *     empty panel that looks broken.
 *   - has_text false with other rows captured: this row predates the column. "Not recorded
 *     yet", from the coverage count, never a fabricated default.
 *   - has_text true: the text, revealed on click, with its measured weight and share.
 *   - the endpoint absent: an older proxy. Say so and stop.
 */
function promptTextReveal(t) {
  const det = el('details', { class: 'why inv-text', 'data-testid': 'inv-text-' + t.kind + '/' + t.name },
    el('summary', {}, 'What it puts in your prompt — ' + num(t.tokens) + ' tokens'));
  det.addEventListener('toggle', () => { if (det.open) loadPrompt(); });
  const body = el('div', { class: 'inv-text-body' });
  det.appendChild(body);
  const paint = () => {
    clear(body);
    if (prompt.state === 'idle' || prompt.state === 'loading') {
      body.appendChild(el('p', { class: 'hint' }, 'Reading…'));
      return;
    }
    if (prompt.state === 'absent') {
      body.appendChild(el('p', { class: 'hint' }, 'This proxy records the token weight of each '
        + 'declaration but not its text. The weight above is exact.'));
      return;
    }
    if (prompt.state === 'error') {
      body.appendChild(el('p', { class: 'hint' }, 'Could not read the prompt text.'));
      return;
    }
    const reg = prompt.byName.get(t.kind + '/' + t.name);
    if (reg && reg.has_text) {
      body.appendChild(el('p', { class: 'hint' }, num(reg.tokens) + ' tokens, '
        + pct(reg.share, 1) + ' of the prefix that session carried.'));
      body.appendChild(el('pre', { class: 'inv-snippet inv-text-pre', text: reg.text }));
      return;
    }
    notCapturedState(body);
  };
  det.addEventListener('toggle', paint);
  // Re-render on the shared fetch completing, which is why paint is a listener and not a
  // one-shot: a reveal opened while the request was in flight would otherwise stay on "Reading…".
  promptWaiters.push(() => { if (det.open) paint(); });
  return det;
}

/** Reveals waiting on the shared fetch. Cleared with the view, like every other local state. */
let promptWaiters = [];

/**
 * notCapturedState says why there is no text, and names the party who can change it.
 *
 * Two different absences, and telling a reader to enable their own setting when the
 * operator's service-wide gate is the one that is shut is the bug this distinction exists to
 * prevent — the same one captureState fixes on the server.
 */
function notCapturedState(host) {
  const v = prompt.view || {};
  if (v.blocked_by === 'operator') {
    return emptyState(host, 'Prompt text is not stored on this deployment',
      'The operator has content capture switched off service-wide, so no prompt or transcript '
      + 'text is written to disk for any account. Every token weight on this page is still '
      + 'exact — only the text is unavailable, and there is nothing for you to enable.');
  }
  if (v.blocked_by === 'tenant') {
    return emptyState(host, 'Not captured — enable transcript storage to see this',
      'Your account has not opted in to content capture, so the text of your tool schemas, '
      + 'skills and system prompt is not stored. Turning it on in your account settings starts '
      + 'recording it from the next session; it does not backfill. The token weights above are '
      + 'recorded either way.');
  }
  if (v.rows && !v.text_rows) {
    return emptyState(host, 'Not recorded yet',
      'All ' + num(v.rows) + ' declarations in this window were written before prompt text '
      + 'was captured, so their text does not exist to show. Sessions from here on record it. '
      + 'This is a gap in the history, not an empty prompt.');
  }
  return emptyState(host, 'Not recorded for this one',
    'The token weight is exact and the text was not stored for this row — it was written before '
    + 'this capture, or its session predates your account\'s opt-in. '
    + (v.text_rows ? num(v.text_rows) + ' of ' + num(v.rows) + ' declarations in this window '
      + 'do have their text.' : ''));
}

/**
 * renderPromptPanel is the whole prefix, decomposed into the regions that own it.
 *
 * This is the answer to "show me the full system prompt": one session's actual prefix, with
 * every region marked and readable, so the shares on this page stop being assertions. It is
 * ONE SESSION's — a prefix averaged over sessions is not a prefix anybody sent — and it says
 * which one.
 *
 * Behind a disclosure because it is a wall of text by nature and because it is the one panel
 * that costs a second request. Not collapsed for the SIZE figures, which are always shown:
 * "your system prompt is 12,400 tokens" is the fact most readers came for.
 */
function renderPromptPanel(host, rep) {
  const p = rep.prompt || {};
  const prefix = (p.tokens || 0) + (rep.totals.declared_set_tokens || 0);
  const panel = el('div', { class: 'panel', 'data-testid': 'inv-prompt-panel' },
    el('div', { class: 'section' },
      el('h2', {}, 'Your system prompt, and what shares it'),
      el('span', { class: 'section-note' },
        'the standing text ahead of every request, before your conversation')));
  host.appendChild(panel);
  // The third tile is rendered only when there IS a system prompt. Without one the prefix and
  // the declaration total are the same number, and two identical figures side by side under
  // different labels read as a bug — the reader has to work out that they agree because one
  // component of the sum is missing, which is the panel's own coverage story told badly.
  const tiles = [
    tile('inv-prompt-tokens', 'System prompt', p.sessions
      ? num(p.tokens) + ' tok' : 'not captured',
      p.sessions ? 'recorded in ' + num(p.sessions) + ' session'
        + (p.sessions === 1 ? '' : 's') : 'no session in this window recorded one'),
    tile('inv-prefix-total', 'Whole prefix per request', num(prefix) + ' tok', p.sessions
      ? 'system prompt + every declaration'
      : 'declarations only — no system prompt recorded here'),
  ];
  if (p.tokens) {
    // Its OWN tile key, not the headline's `inv-declared`. That one is a per-session MEAN over
    // captured sessions (35,207 here) and this is the declaration SET's total (36,950) — two
    // different quantities, and sharing a key would have given them one explanation that was
    // false for whichever tile the reader was looking at.
    tiles.push(tile('inv-declared-set', 'Declarations', num(rep.totals.declared_set_tokens) + ' tok',
      prefix ? pct(100 * rep.totals.declared_set_tokens / prefix, 0) + ' of the prefix' : ''));
  }
  panel.appendChild(el('div', { class: 'tiles' }, ...tiles));

  // The coverage count, always, and BEFORE the reveal: a reader must know how much of their
  // history can answer this before they conclude anything from the one session below.
  if (p.rows) {
    panel.appendChild(el('p', { class: 'hint', 'data-testid': 'inv-prompt-coverage' },
      num(p.text_rows) + ' of ' + num(p.rows) + ' declarations in this window stored their '
      + 'text' + (p.text_rows < p.rows
        ? '. The rest were written before prompt text was captured, or by an account that has '
          + 'not opted in to content capture — they are a gap in the history, not empty prompts.'
        : '.')));
  }

  const det = el('details', { class: 'why', 'data-testid': 'inv-prompt-reveal' },
    el('summary', {}, 'Read the whole thing, region by region'));
  det.addEventListener('toggle', () => { if (det.open) loadPrompt(); });
  const body = el('div');
  det.appendChild(body);
  const paint = () => {
    clear(body);
    if (prompt.state === 'loading' || prompt.state === 'idle') {
      body.appendChild(el('p', { class: 'hint' }, 'Reading…'));
      return;
    }
    if (prompt.state === 'absent') {
      emptyState(body, 'This proxy does not record prompt text',
        'It records the token weight of every region, which is what the figures above are. '
        + 'Reading the text needs a newer proxy.');
      return;
    }
    if (prompt.state === 'error') {
      emptyState(body, 'Could not read the prompt text', 'The figures above are unaffected.');
      return;
    }
    const v = prompt.view || {};
    if (!v.captured) { notCapturedState(body); return; }
    body.appendChild(el('p', { class: 'note' }, 'One session\'s actual prefix — '
      + num(v.tokens) + ' tokens across ' + num((v.regions || []).length) + ' regions, captured '
      + when(v.ts) + '. Ordered heaviest first, which is NOT the order the model reads them in: '
      + 'the array order a client sends its tools in is not recorded.'));
    const regions = body.appendChild(el('div', { class: 'inv-prompt-regions' }));
    for (const r of v.regions || []) regions.appendChild(promptRegion(r));
  };
  det.addEventListener('toggle', paint);
  promptWaiters.push(() => { if (det.open) paint(); });
  panel.appendChild(det);
}

/** promptRegion is one marked region of the prefix: who owns it, how much, and the text. */
function promptRegion(r) {
  const isSys = r.kind === 'system_prompt';
  const name = isSys ? 'The system prompt itself'
    : (r.kind === 'skill_listing' ? 'The skills listing' : r.name);
  const det = el('details', { class: 'inv-region' + (isSys ? ' inv-region-sys' : ''),
    'data-testid': 'inv-region-' + r.kind + '/' + r.name },
  el('summary', {},
    el('span', { class: 'inv-region-name comp-name', text: name }),
    isSys ? el('span', { class: 'pill neutral' }, 'system') : kindPill(r),
    el('span', { class: 'section-note', text: num(r.tokens) + ' tok · ' + pct(r.share, 1) })));
  if (r.has_text) {
    det.appendChild(el('pre', { class: 'inv-snippet inv-text-pre', text: r.text }));
  } else {
    notCapturedState(det.appendChild(el('div')));
  }
  return det;
}

/**
 * renderSelfRemoved credits the reductions the USER made themselves.
 *
 * Every other measurement here is about content context-guru removed. When somebody reads this
 * page, removes an MCP server and stops carrying 4,000 tokens a request, no component ran and
 * no filter fired — so the saving registered nowhere at all, and the product got no credit for
 * the one outcome it most wants to cause. This finds it by treating the inventory as the time
 * series it is: present in the early sessions, absent from the later ones.
 *
 * It is never added into the filter's realized savings, because an account that removed a
 * server locally AND has it on the server-side filter list would be credited twice for one
 * reduction. Overlapping rows are marked and left for the reader.
 */
function renderSelfRemoved(host, rep) {
  const rows = rep.self_removed || [];
  if (!rows.length) return;
  const panel = el('div', { class: 'panel', 'data-testid': 'inv-self-removed' },
    el('h2', {}, 'You removed these yourself'),
    el('p', { class: 'note' }, num(rows.length) + ' declaration'
      + (rows.length === 1 ? '' : 's') + ' stopped appearing partway through this window. That '
      + 'is a real reduction and it is credited here — no component removed it, so it appears '
      + 'in no other figure on this dashboard.'));
  host.appendChild(panel);
  const table = el('table', { class: 'tbl' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Item'), el('th', { class: 'num' }, 'Was costing'),
      el('th', { class: 'num' }, 'Sessions since'), el('th', { class: 'num' }, 'Avoided since'),
      el('th', {}, 'Evidence'))));
  const body = el('tbody');
  for (const r of rows) {
    body.appendChild(el('tr', {},
      el('td', {}, el('code', { text: r.name })),
      el('td', { class: 'num', text: num(r.tokens) + ' tok/req' }),
      numTd(r.sessions_after),
      el('td', { class: 'num' }, money(r.avoided_usd, r.priced)),
      // 12, not 3: the server already drops everything below 3, so a 3-threshold here never
      // rendered. A dozen comparable sessions is what the copy has always claimed.
      el('td', {}, r.sessions_after < 12
        ? el('span', { class: 'pill missing', title: 'Only ' + num(r.sessions_after)
          + ' comparable sessions have run since it was last seen, so this may simply be a run '
          + 'of sessions that did not need it rather than a removal.' }, 'weak — few sessions')
        : el('span', { title: 'Declared in ' + num(r.sessions_before) + ' sessions, then absent '
          + 'from the ' + num(r.sessions_after) + ' that followed.' },
        num(r.sessions_before) + ' → 0')),
      r.overlap ? el('td', {}, el('span', { class: 'pill warn' }, 'also server-side filtered')) : null));
  }
  table.appendChild(body);
  panel.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, table));
}

// ── load + render ──────────────────────────────────────────────────────────

function renderTools() {
  const host = clear(toolsView);
  const rep = tools.report;
  if (!rep) return;
  // Every reveal in the old DOM is gone, so its repaint closure is too. Cleared here rather
  // than trusted to garbage collection, because the list would otherwise grow by forty
  // entries on every tab switch and repaint detached nodes forever.
  promptWaiters = [];
  renderHeadline(host, rep);
  if (rep.coverage.captured) {
    // Composition first: "who owns the prompt" is the question the page exists for, and it
    // was answerable only by reading a sortable table.
    renderComposition(host, rep);
    // Then the prompt itself, which is the other half of that question and the only place
    // the reader can check any of it against the actual text.
    renderPromptPanel(host, rep);
    renderRemovalValue(host, rep);
    renderUnused(host, rep);
    renderSelfRemoved(host, rep);
    renderRealized(host);
    toolTable(host, rep);
    serverTable(host, rep);
    skillsPanel(host, rep);
  }
  renderCoverage(host, rep);
  // Built-ins LAST, collapsed, behind the danger warning. Deliberately after the coverage
  // panel: nothing above it should invite reading these as a saving.
  if (rep.coverage.captured) renderBuiltins(host, rep);
}

/**
 * loadTools fetches the report, and the removal state BEST-EFFORT.
 *
 * The two are deliberately not one request and not one failure: the analysis is the
 * feature, the control is an addition to it, and a proxy that cannot offer the control
 * must still show the analysis rather than an error page.
 */
async function loadTools() {
  loadingState(toolsView, 4);
  let control = null;
  try {
    const c = await api('toolfilter');
    // enabled:false is a proxy that has the endpoint and is not acting; treat it as absent
    // so the copy says "not enabled" instead of offering a switch that does nothing.
    if (c && c.enabled) control = c;
  } catch (err) {
    if (aborted(err)) return;
    control = null; // 404 on an older proxy, or the feature is not built yet
  }
  try {
    tools.report = await api('tools');
    tools.control = control;
  } catch (err) {
    if (aborted(err)) return;
    errorState(toolsView, 'Could not read the tool inventory', err);
    return;
  }
  renderTools();
}

// Registered here, so mounting this whole view is one line in the shared page.
Object.assign(loaders, { tools: loadTools });
