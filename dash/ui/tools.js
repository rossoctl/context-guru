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
  // controlDoc is the same document even when it is NOT usable, kept for its `reason`. A control
  // that vanishes with no explanation is a page with no opinion; the reason is the whole
  // difference between "this proxy cannot do it" and "pick an account first".
  controlDoc: null,
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
      + 'invoke — averaged over the captured sessions. The percentage beside it is a share of '
      + 'the DECLARED tokens, not of the whole prefix: the system prompt is not a declaration '
      + 'and is not in that denominator, so the same tokens are a smaller share of everything '
      + 'a request actually carries. The prefix panel below gives that whole.',
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
  host.appendChild(tileGroup('What you carry that you can change, and what you use',
    'per session, averaged over the ' + num(c.captured) + ' session'
    + (c.captured === 1 ? '' : 's') + ' whose inventory was captured — MCP tools and skills only, '
    + 'which are the things you can actually turn off', [
    tile('inv-declared', 'Declared per session', num(t.declared_tokens),
      'your MCP tool schemas and the skills listing'),
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
    //
    // The share's denominator is DECLARED tokens, so it is a share of the declarations and
    // not of the prompt. It read "of every prompt" while the prefix panel below says in as
    // many words that the system prompt is not in that denominator — the same page naming two
    // different wholes for one ratio, and erring in the direction that inflates it.
    tile('inv-unused', 'Never invoked', num(t.unused_tokens),
      pct(t.unused_pct) + ' of every declaration you carry, re-read '
      + t.requests_per_session.toFixed(1)
      + '× per session — the plain mean over all ' + num(c.sessions) + ' sessions here',
      t.unused_pct >= 25 ? 'bad' : ''),
    tile('inv-avoidable', 'Avoidable — projected',
      t.priced ? usd(t.unused_usd) : num(t.unused_reads) + ' tok',
      t.priced ? 'if none of it had been carried' : 'no dollar: some models here are unpriced'),
  ], 'headline'));
  host.appendChild(gauge(t));
  // What these tiles LEFT OUT, and how big it is. Without this line the reader meets a
  // composition bar further down that is several times larger than the headline and has to work
  // out for themselves which of the two is lying. Neither is; they answer different questions.
  if (t.aside_tokens) {
    // The QUALIFIER stays level 1 — DESIGN §3.3 is explicit that a denominator is never behind
    // disclosure — and the REASONING moves to level 2. What was here was 93 px of prose in the
    // reader's first screen, which is the screen the decisions have to be in, and the only part
    // of it a reader needs before scrolling is the first clause.
    const aside = el('div', { class: 'hint inv-aside', 'data-testid': 'inv-aside-note' },
      el('span', {}, 'Excludes a further ' + num(t.aside_tokens) + ' tokens per session of '
        + 'Claude Code\'s own tools and the provider\'s — deliberately: removing those breaks '
        + 'the agent rather than slimming it.'));
    aside.appendChild(whyBlock('why they are out of these four figures',
      'Counting them as waste would make the headline a number whose only action breaks things. '
      + 'They are the equipment the model is expected to have, not unused capacity you are '
      + 'paying for by mistake. They ARE counted in the prefix bar further down, whose job is '
      + 'the whole prompt, and they are listed in full in their own section at the end of this '
      + 'page.', 'Why the agent\'s own tools are excluded from the headline'));
    host.appendChild(aside);
  }
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
      el('h2', {}, 'What every request carries that you could stop carrying'),
      el('span', { class: 'section-note', text: num(t.declared_tokens)
        + ' tokens of YOUR MCP tool and skill declarations, per request, averaged over the '
        + 'sessions whose inventory was captured — the agent\'s own tools are excluded and are '
        + 'shown separately' })),
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
  // Two different zeroes, and they must not read the same. An account that declared MCP tools and
  // skills and used all of them has nothing to remove; an account that declared NONE has nothing
  // to remove FROM, which on a plain Claude Code session is the normal state — and saying "nothing
  // was left unused" there implies a clean bill over an empty set while 15,000 tokens of built-ins
  // sit in the section at the end of the page.
  if (!t.declared_tokens) {
    box.appendChild(el('p', { class: 'hint' }, 'No MCP tools and no skills were declared in this '
      + 'scope, so there is nothing here you can turn off. That is not "no waste": what these '
      + 'sessions carried was the agent\'s own tools'
      + (t.aside_tokens ? ' — ' + num(t.aside_tokens) + ' tokens a session of them' : '')
      + ', which are listed at the end of the page and are not yours to remove.'));
  } else if (!t.unused_tokens) {
    box.appendChild(el('p', { class: 'hint ok' },
      'Everything you declared in this scope was invoked at least once. There is nothing to '
      + 'remove.'));
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
      + (rep.totals.requests_per_session_typical || 0).toFixed(0) + ' turns'
      + (shortSessionsDominate(rep)
        ? ' — most sessions are one-request sidechains, so the plain average is not what '
          + 'carrying a declaration costs. '
        : ', because request-weighting counts each session as many times as it made requests '
          + 'and that is where the re-reads are. ')
      + 'The per-session projection below uses the second figure and says so.'),
    el('p', {}, 'Each re-read is priced at the tier that request actually paid: cache read '
      + 'for a hit, cache CREATION for the turn that wrote the prefix, and full input rate '
      + 'for a turn with no cache at all. It is not all valued at the cache-read rate, which '
      + 'would understate the cold turns, and not all at the fresh-input rate, which is the '
      + 'same tokens at roughly ten times the price. A figure quoted at one flat tier is a '
      + 'different number from the one here and should not be compared to it.')));
}

/**
 * shortSessionsDominate is the ONE place the "most sessions are one-request sidechains"
 * claim is decided.
 *
 * It is a claim about THIS account's population, not about the statistic: printed
 * unconditionally it told a reader whose median was 244 requests that most of their sessions
 * were single calls. Two callers seven hundred lines apart both make it, and they had already
 * diverged once — the basis panel guarded, the coverage note not — so the condition lives
 * here and neither caller owns a copy of it.
 */
function shortSessionsDominate(rep) {
  const turns = rep.totals.requests_per_session_typical || rep.totals.requests_per_session || 1;
  // Strict <, and undefined compares false: an older proxy that sends no median gets no
  // claim about its population rather than the claim by default.
  return rep.totals.requests_per_session_median < turns / 2;
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
  // ONE block about the switches, covering both reasons they may be inert. They were two
  // stacked `.state` panels saying overlapping things in 200 px of the reader's first screen, and
  // both end in the same sentence: use the command or the snippet, which is the better fix anyway.
  const gated = !SKILL_SWITCH_SAFE && rows.some((t) => t.kind === 'skill');
  const why = (tools.controlDoc && tools.controlDoc.reason) || '';
  if (!tools.control || gated) {
    const box = el('div', { class: 'state', 'data-testid': 'inv-control-absent' },
      el('div', { class: 'state-body' },
        el('strong', {}, !tools.control
          ? (why ? 'The one-click switches are not live in this view'
            : 'One-click opt-out is not enabled on this proxy')
          : 'The per-skill switches are off on purpose'),
        !tools.control && why ? el('span', {}, why) : null,
        // Kept on the SAME block rather than a second one, and kept visible rather than folded:
        // a disabled control whose reason is only in a title attribute reads as a broken build,
        // and a title does not exist on a phone, in a screen reader or in a screenshot.
        gated ? el('span', { 'data-testid': 'inv-skill-switch-gated' }, SKILL_SWITCH_REASON) : null,
        // No third sentence. It used to end "use the command instead, which is the better fix
        // anyway" — which is now the block directly ABOVE this one, in full, with the snippets in
        // it. Saying it twice cost 50 px of the reader's first screen.
        el('span', {}, 'Nothing about the ordered list below changes.')));
    panel.appendChild(ctaBlock(groups, rep));
    panel.appendChild(box);
    panel.appendChild(reversibilityNote());
  } else {
    panel.appendChild(ctaBlock(groups, rep));
    panel.appendChild(reversibilityNote());
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

/** gSessions is how many sessions carried a group: the widest of its members' populations. */
function gSessions(g) {
  return Math.max(0, ...g.items.map((t) => t.sessions_declared || 0));
}

/**
 * prefixRebuildUSD is the one-time cost of acting, in money, or null when it cannot be priced.
 *
 * Removing a declaration changes the cached prompt PREFIX, so the next turn of every session in
 * flight re-writes the whole prefix at the cache-CREATION rate instead of reading it back. The
 * charge is therefore the whole prefix at the DIFFERENCE between the two rates — not the prefix
 * at the write rate, which bills the reader again for a read they were going to pay for anyway,
 * and not the removed item's own weight, which is not what gets re-written.
 *
 * Priced at the model that carried the most requests in scope, and that model is NAMED where the
 * figure is shown: the rates differ 3x across the models a single account here ran on, so an
 * unattributed blend belongs to none of them.
 */
function prefixRebuildUSD(rep) {
  const models = (rep.models || []).filter((m) => m.priced);
  const prefix = ((rep.prompt && rep.prompt.tokens) || 0) + (rep.totals.declared_set_tokens || 0);
  if (!models.length || !prefix) return null;
  const m = models.slice().sort((a, b) => b.requests - a.requests)[0];
  const rate = Math.max(0, m.cache_write_usd_per_token - m.cache_read_usd_per_token);
  if (!rate) return null;
  return { usd: prefix * rate, model: m.model, prefix };
}

/**
 * paybackRow is DESIGN §3.7 for one removal: the benefit, the cost of taking it, and the signed
 * net, as ONE component that cannot render half of itself.
 *
 * The cost used to exist only in reversibilityNote()'s third paragraph — behind a disclosure,
 * below the whole list, as prose — which is the arrangement §3.7 exists to forbid. A reader met
 * eleven cards each leading with a dollar they would save and not one showing the dollar they
 * would pay to save it, and on this corpus the one-time cost is LARGER than a single session's
 * benefit for every group on the page. A benefit shown without its cost was half a number.
 *
 * Unpriced, or a scope with no rates at all, renders `n/a` with its reason on all three lines
 * rather than a $0 — a zero here would be a claim that acting is free.
 */
function paybackRow(g, rep) {
  const rebuild = prefixRebuildUSD(rep);
  const sessions = gSessions(g);
  const per = g.priced && sessions ? g.usd / sessions : null;
  const net = per !== null && rebuild ? per - rebuild.usd : null;
  const even = net !== null && per > 0 ? Math.ceil(rebuild.usd / per) : null;
  const part = (cls, label, value, note) => el('span', { class: 'inv-pb' },
    el('span', { class: 'inv-pb-label', text: label }),
    el('span', { class: 'inv-pb-val ' + cls }, value),
    note ? el('span', { class: 'inv-pb-note', text: note }) : null);
  // ONE row, three parts, in one component — a renderer that can emit the benefit without the
  // cost is the bug §3.7 exists to make impossible. The cost's BASIS is stated once at panel
  // level (ctaBlock) rather than on every card, because the prefix rebuild is the same charge
  // whichever of these groups you remove: eight copies of one sentence is the "40 tooltips"
  // defect with the tooltips unfolded, and it was 110 px per card.
  return el('div', { class: 'inv-payback', 'data-testid': 'inv-payback-' + g.key },
    part('good', 'Benefit', per === null
      ? na('This scope has no model rates, so its tokens cannot be turned into a dollar.')
      : document.createTextNode('+' + usd(per)), 'per later session'),
    part('bad', 'Cost', rebuild
      ? document.createTextNode('\u2212' + usd(rebuild.usd))
      : na('No priced model in this window, so the prefix rebuild cannot be priced.'),
    'once, one prefix rebuild'),
    // The net is bold and keeps its sign, and it is NOT demoted when negative — on this corpus
    // it is negative over the first session for every group, which is the most useful thing the
    // card says and the reason the cost had to come out from behind a disclosure.
    part(net === null ? '' : net < 0 ? 'bad' : 'good', 'Net', net === null
      ? na('Needs both a priced benefit and a priced rebuild; one of them is unknown here.')
      : document.createTextNode((net < 0 ? '\u2212' : '+') + usd(Math.abs(net))),
    net === null ? 'not computable here'
      : even === null ? 'over the first session'
        : 'over session 1 \u2014 ahead from session ' + num(even + 1)));
}

/**
 * na renders an unavailable figure as DESIGN §3.6 requires: same size as the number it
 * replaces, muted rather than red, and carrying its reason in BOTH title and aria-label.
 *
 * Local rather than style.css's `.na`, which is `--fg-faint` and italic — §3.6 asks for
 * `--fg-muted`, upright, with the dotted underline that tells a reader a reason exists.
 */
function na(reason) {
  return el('span', { class: 'inv-na', title: reason, 'aria-label': 'not available: ' + reason },
    'n/a');
}

/**
 * ctaBlock is the one thing to DO, stated at the top of the list rather than inferred from it.
 *
 * Everything below is per-group; this is the whole set in two snippets, because reversibility's
 * only real instruction — switch off everything you mean to in ONE pass, since the prefix
 * rebuild is paid per pass and not per item — is impossible to follow from a page that offers
 * one command at a time.
 *
 * Neither snippet is composed by hand. The shell lines are the server's own `removal.command`
 * strings joined by newlines, and the settings snippet is built by merging the server's own
 * PARSED `removal.settings` objects, so the syntax is always the server's (see
 * dash/toolremoval.go). A dashboard that invents the syntax of a command it tells you to paste
 * is worse than one that says nothing, because this one gets pasted.
 *
 * The settings route leads, and it leads for a safety reason as well as a convenience one: it
 * writes the reader's own config and never goes near POST /api/toolfilter, whose skill parser
 * currently mis-attributes rows (see excludeToggle).
 */
function ctaBlock(groups, rep) {
  const wrap = el('div', { class: 'inv-cta', 'data-testid': 'inv-cta' });
  const cmds = [];
  const over = {};
  let unmerged = 0;
  let overCount = 0;
  let skillTotal = 0;
  for (const g of groups) {
    const r = g.items[0].removal || {};
    if (r.command && !r.danger) cmds.push(r.command);
    for (const t of g.items) {
      const s = (t.removal || {}).settings;
      if (!s) { if (t.kind === 'skill') { skillTotal++; unmerged++; } continue; }
      let merged = false;
      try {
        const o = JSON.parse(s);
        if (o && o.skillOverrides) { Object.assign(over, o.skillOverrides); merged = true; }
      } catch (_) { merged = false; }
      if (merged) overCount++;
      else if (t.kind === 'skill') unmerged++;
      if (t.kind === 'skill') skillTotal++;
    }
  }
  const tok = groups.reduce((n, g) => n + g.tokens, 0);
  // The ordered names themselves, in one line. This is the answer to "what sits in my prompt and
  // what would removing it save", and the CARDS below are that answer in full — but a card is
  // ~250 px and the headline band above is 560, so on a 900 px viewport not one name reached the
  // reader's first screen. The list is already sorted by cost; this is its first five, in order,
  // each with the figure it is sorted by. Not a sixth granularity: it is an index of the run of
  // cards immediately below it, and it names no row those cards do not.
  const lead = groups.slice(0, 5);
  const rest = groups.length - lead.length;
  // Deliberately NOT a grand dollar total. The page already carries one in the headline, and a
  // second computed from these rows lands on a different figure — the server double-books the
  // skills listing against its own entries in `totals` (dash/toolapi.go), so a sum of rows and
  // the headline disagree by that amount. Two totals under one heading is the defect this tab
  // already has once, between its tile and its bar; the fix is the server's, not a third number.
  wrap.appendChild(el('div', { class: 'inv-cta-head' },
    el('strong', { text: 'Do this' }),
    el('span', { class: 'section-note', text: num(groups.length) + ' thing'
      + (groups.length === 1 ? '' : 's') + ' to remove · ' + num(tok)
      + ' tokens out of every request' })));
  const idx = el('ol', { class: 'inv-lead', 'data-testid': 'inv-lead' });
  for (const g of lead) {
    idx.appendChild(el('li', {},
      el('span', { class: 'inv-lead-name comp-name', text: g.label }),
      el('span', { class: 'inv-lead-usd' }, money(g.usd, g.priced)),
      el('span', { class: 'section-note', text: num(g.tokens) + ' tok/request · '
        + (g.items.length > 1 ? num(g.items.length) + ' declarations · ' : '')
        + 'never called in ' + num(gSessions(g)) + ' session'
        + (gSessions(g) === 1 ? '' : 's') })));
  }
  if (rest > 0) {
    idx.appendChild(el('li', { class: 'inv-lead-rest' },
      el('span', { class: 'section-note', text: 'and ' + num(rest) + ' more below, in the same '
        + 'order — each card carries its own figure, its evidence and its command' })));
  }
  wrap.appendChild(idx);
  if (Object.keys(over).length) {
    // "all N" would be false whenever a plugin-bundled skill is removed by a `claude plugin
    // disable` command instead of a skillOverrides entry — and a reader who pastes this and finds
    // skills still declared would reasonably conclude the snippet did not work.
    wrap.appendChild(copyBox(JSON.stringify({ skillOverrides: over }, null, 2),
      'Add to ~/.claude/settings.json — switches off ' + num(overCount)
      + (overCount === skillTotal ? ' never-invoked skill' + (overCount === 1 ? '' : 's')
        : ' of the ' + num(skillTotal) + ' never-invoked skills') + ' at once'));
  }
  if (cmds.length) {
    wrap.appendChild(copyBox(cmds.join('\n'), 'Run these ' + num(cmds.length)
      + ' command' + (cmds.length === 1 ? '' : 's') + ' — one per removable group'));
  }
  if (!cmds.length && !Object.keys(over).length) {
    wrap.appendChild(el('p', { class: 'hint' }, 'None of these groups has a known removal '
      + 'mechanism, so there is nothing to copy in one piece. Each card below states what it '
      + 'knows about its own row.'));
  }
  if (unmerged) {
    // Never silently short. A skill whose snippet this could not parse is named as missing from
    // the combined one, because a reader who pastes it and finds one skill still declared would
    // reasonably conclude the whole snippet did not work.
    wrap.appendChild(el('p', { class: 'hint' }, num(unmerged) + ' skill'
      + (unmerged === 1 ? ' is' : 's are') + ' NOT in that snippet: the server does not express '
      + 'their removal as a skillOverrides entry — a plugin-bundled skill goes with its plugin. '
      + 'They are in the command list' + (cmds.length ? ' above' : ' on their own cards') + '.'));
  }
  // The cost every card below quotes, defined ONCE here, at level 1 — a shared basis stated per
  // card is eight copies of one sentence, and stated nowhere is the §3.7 violation this whole
  // block exists to fix.
  const rebuild = prefixRebuildUSD(rep);
  // The cost FIGURE is on every card at level 1 (paybackRow). What folds is its derivation — what
  // a prefix rebuild is and why it is charged once — which is level 2 by DESIGN §3.3 and was four
  // lines of the reader's first screen.
  const one = rebuild
    ? 'Each card below prices the same one-time cost against its own benefit: ' + usd(rebuild.usd)
      + ' — the next turn of every session in flight re-writes the whole ' + num(rebuild.prefix)
      + '-token prefix at cache-creation rates instead of reading it back, valued at '
      + rebuild.model + ', the model that carried most of these requests. It is charged ONCE for '
      + 'the whole pass, so removing everything you mean to in one go costs the same as removing '
      + 'one thing, and toggling something back and forth pays it every time.'
    : 'No model in this window has known rates, so the one-time prefix-rebuild cost each card '
      + 'below prices against its benefit reads n/a rather than zero.';
  wrap.appendChild(whyBlock('Both edit your own config, and the one-time cost is paid once',
    'These stop the declaration at its source rather than filtering it here, so they survive this '
    + 'proxy and nothing on this page has to stay switched on for them to keep working. ' + one,
    'What these snippets do, and what acting on them costs once'));
  return wrap;
}

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
  // The group-level switch, for the group whose MECHANISM is group-wide: an MCP server. It posts
  // one `mcp_server` exclusion, which is the same unit `claude mcp remove <server>` beside it
  // takes and the same unit the config stores as `mcp__<server>`.
  //
  // It exists because the page grouped by "what one action removes" and then offered a switch per
  // TOOL only: a reader who had decided to drop a nineteen-tool server had to tick nineteen
  // boxes, each one a separate write, each one its own prefix rebuild — which is the one thing
  // the reversibility note tells them not to do.
  const srv = many && g.sub === 'MCP server'
    ? { kind: 'mcp_server', name: '', server: g.label, tokens: g.tokens } : null;
  const head = el('div', { class: 'inv-group-head' },
    el('div', { class: 'inv-group-id' },
      srv ? excludeToggle(srv) : null,
      el('span', { class: 'inv-group-name comp-name', text: g.label }),
      g.sub ? el('span', { class: 'pill neutral', text: g.sub }) : kindPill(g.items[0]),
      many ? el('span', { class: 'section-note', text: num(g.items.length) + ' declarations' }) : null),
    el('div', { class: 'inv-group-cost' },
      el('span', { class: 'inv-group-usd' }, money(g.usd, g.priced)),
      // The denominator, on the figure the list is ORDERED by. It was the one number on this
      // card with no population attached, which on a page carrying two different session
      // multipliers is the one place a reader cannot supply it themselves.
      el('span', { class: 'section-note', text: 'avoidable across the ' + num(gSessions(g))
        + ' session' + (gSessions(g) === 1 ? '' : 's') + ' that carried it' }),
      el('span', { class: 'section-note', text: num(g.tokens) + ' tok in every request · '
        + compact(g.reads) + ' read for nothing' })));
  const wrap = el('div', { class: 'inv-group', 'data-testid': 'inv-group-' + g.key }, head);
  // The cost of acting, BESIDE the benefit rather than below it (DESIGN §3.7).
  wrap.appendChild(paybackRow(g, rep));
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
 * unusedRow is one candidate, its evidence, its switch, and the command that removes it AT SOURCE.
 *
 * The checkbox and the name are inside a <label> and NOTHING ELSE IS. A copy button inside a
 * label is a button that toggles the checkbox when you click it, which is how an affordance
 * added for convenience becomes a change nobody meant to make. There were two copies of this
 * function in this file; the older one wrapped the whole row in the label, and being second it
 * won — so the reveal and the command below were unreachable code for as long as both existed.
 *
 * EVERY row carries its own command, not just the group head. A group of one already showed it,
 * and a group of many showed the SERVER-level command once at the top — which is right for "get
 * rid of this whole MCP server" and no answer at all to "get rid of just this one tool", the
 * question a reader with a nineteen-tool server actually has. The server command remains at the
 * group head; this is the per-item one beside it.
 */
function unusedRow(t, rep) {
  const c = rep.coverage;
  const key = t.kind + '/' + t.name;
  const off = isExcluded(t);
  // Only a provider-side tool is inert AS A ROW: it can never be dropped here, whatever this
  // proxy supports. A missing endpoint disables every switch, but that is one fact about the
  // page, stated once by renderUnused — repeating it under forty rows buries the evidence
  // sentence that is the point of the row.
  const fixed = t.kind === 'server_tool';
  const box = el('div', {
    class: 'cg-item' + (off ? ' cg-off' : '') + (fixed ? ' cg-fixed' : ''),
    'data-testid': 'inv-item-' + key,
  });
  box.appendChild(excludeToggle(t));
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
  if (t.kind === 'skill' && !SKILL_SWITCH_SAFE) {
    // ONE clause here, not the whole reason. renderUnused states it once above the list — 24
    // copies of a five-line explanation is the "40 tooltips" defect with the tooltips unfolded,
    // and it buries the evidence sentence that is the point of the row.
    box.appendChild(el('span', { class: 'comp-warn' }, 'Per-skill switch off for now — see the '
      + 'note above the list. The command and the snippet both work.'));
  } else if (fixed) {
    box.appendChild(el('span', { class: 'comp-warn' }, 'Provider-side tool. It is part of the '
      + 'API request the agent builds, not something this proxy declares, so it cannot be '
      + 'dropped here.'));
  }
  if (off) {
    // NOT "this is no longer sent". The filter keeps any declaration your own system prompt still
    // NAMES — stripping a declaration whose prose survives invites the model to narrate the call
    // instead of making it, which nothing surfaces — so an opt-out can be recorded and correctly
    // declined at request time. The page cannot tell which from here (the decision is per request,
    // in apply, and nothing stores it per item), so it states the condition rather than implying
    // an outcome it has not checked.
    box.appendChild(el('span', { class: 'cg-item-basis', text: 'Opted out: context-guru stops '
      + 'sending it from your next session — unless your own system prompt still names it, in '
      + 'which case it is deliberately kept and will go on appearing here. Switching back on '
      + 'restores it and pays the one-time prefix rebuild again.' }));
  }
  box.appendChild(promptTextReveal(t));
  // The per-item removal, folded: the group head already carries the one-command answer, so
  // forty open copy-boxes under it would bury the evidence sentence that is the point of the row.
  // Folded is not hidden — the summary names the item and the mechanism.
  box.appendChild(removalDetails(t, 'Remove just ' + t.name + ' from your own setup'));
  return box;
}

/**
 * removalDetails is removalCell behind a disclosure, for the places that show one row per item.
 *
 * The tables and the member lists have one of these per row, and removalCell renders two
 * copy-boxes and a note — open, that is a page of snippets where a table was. The summary states
 * the item, so a reader scanning for "how do I get rid of THIS one" does not have to open every
 * row to find out which is which.
 */
function removalDetails(t, summary) {
  const det = el('details', { class: 'why inv-removal-fold',
    'data-testid': 'inv-removal-' + t.kind + '/' + t.name },
  el('summary', {}, summary));
  // Built lazily: removalCell is cheap, but forty of them per table times four tables is DOM
  // nobody has looked at yet, and the copy buttons register listeners.
  let built = false;
  det.addEventListener('toggle', () => {
    if (!det.open || built) return;
    built = true;
    det.appendChild(removalCell(t));
  });
  return det;
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
    // A sentence, not a panel-sized empty state. The absence IS the answer here, and its only
    // job is to sit beside the projection so the two are never confused — 155 px of centred
    // "nothing" six screens away did that job worse than one line does.
    panel.appendChild(el('p', { class: 'note' }, 'Nothing has been switched off yet, so nothing '
      + 'has been realized. This panel only ever reports what a removal actually avoided on '
      + 'requests that were really sent; the projected figure above is a different measurement '
      + 'and is never copied down here.'));
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
  const key = t.kind + '/' + (t.name || t.server || '');
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

/**
 * foldPanel is a panel whose heading is its own disclosure, and it returns the BODY.
 *
 * The four tables below the decision list re-answer the same 39 declarations at four
 * granularities — grouped, flat, per-server, per-skill — across 5,436 px of four tables and 31
 * columns. Every one of them is genuinely useful to somebody and none of them is the decision, so
 * they are derivation and derivation is level 2 (DESIGN §3.3). Not deleted: a reader who wants a
 * flat sortable view of one column is exactly who these are for.
 *
 * The summary carries the row COUNT, so the reader can tell whether opening it is worth it — the
 * one honest way to hide a table (NN/g's discoverable hidden-column count, applied to a panel).
 * A <details class="panel"> is the same shape renderBuiltins already uses, so this adds no nesting
 * level over what the page had.
 */
function foldPanel(testid, title, note) {
  const body = el('div', { class: 'inv-fold-body' });
  el('details', { class: 'panel inv-fold', 'data-testid': testid },
    el('summary', {},
      el('span', { class: 'inv-fold-title', text: title }),
      note ? el('span', { class: 'section-note', text: note }) : null),
    body);
  return body;
}

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
  const panel = foldPanel('inv-tools-fold', 'Every declaration you control, one flat row each',
    num((rep.tools || []).length) + ' rows, sortable by any column');
  panel.appendChild(el('div', {},
    el('p', { class: 'note' }, 'Your MCP tools, sorted by what it cost to carry them unused. '
      + '"Read for nothing" is this item\'s size times the requests that re-read it in the '
      + "sessions that never called it. Claude Code's own tools and the provider's are in their "
      + 'own section at the end of this page, and in none of the figures above. Skills have their '
      + 'own table below, because a skills listing can be unreadable in a way a tools array '
      + 'cannot.')));
  const table = el('table', { class: 'tbl', 'data-testid': 'inv-tools-table' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Name'), el('th', {}, 'Kind'),
      el('th', { class: 'num' }, 'Tokens'), el('th', { class: 'num' }, 'Sessions'),
      el('th', { class: 'num' }, 'Used in'), el('th', { class: 'num' }, 'Calls'),
      el('th', { class: 'num' }, 'Read for nothing'), el('th', { class: 'num' }, 'Cost of that'),
      el('th', {}, 'How to remove it'))));
  const body = el('tbody');
  table.appendChild(body);
  panel.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, table));
  host.appendChild(panel.closest('details'));
  tsortable(table, TOOL_COLS, () => renderTools());
  // The server already excluded the built-ins and the provider's tools from rep.tools — they are
  // in rep.aside, out of every total. No filter here: a second copy of that rule in the client
  // is a second place for it to drift, and the drift is invisible because both look plausible.
  if (!rep.tools.length) {
    tableMessage(body, 9, 'No MCP tools captured',
      'No session in this scope declared an MCP tool. Anything else it declared is in the '
      + 'skills table below, or in the agent\'s own section at the end.');
    return;
  }
  for (const t of sortRows(rep.tools, tools.sort, tools.dir)) {
    body.appendChild(el('tr', { class: t.sessions_used ? '' : 'cg-row-unused' },
      el('td', {}, el('span', { class: 'comp-name trunc', title: t.name, text: t.name })),
      el('td', {}, kindPill(t), t.server ? el('span', { class: 'cg-item-srv', text: t.server }) : null),
      numTd(t.tokens), numTd(t.sessions_declared),
      el('td', { class: 'num' }, t.sessions_used
        ? num(t.sessions_used)
        : el('span', { class: 'pill missing' }, 'never')),
      numTd(t.calls), readTd(t.unused_reads),
      el('td', { class: 'num' }, money(t.unused_usd, t.priced)),
      // Every row, used or not. The actionable list above covers the never-invoked ones; a reader
      // who wants rid of something they DO use occasionally had nowhere to find the command.
      el('td', {}, removalDetails(t, 'Command'))));
  }
}

/**
 * serverTable is the MCP rollup, and it is here because it is the unit of the DECISION: a
 * user adds and removes an MCP server, not one of its nineteen tools. A per-tool table
 * alone makes the reader do that addition in their head.
 */
function serverTable(host, rep) {
  if (!rep.servers.length) return;
  const panel = foldPanel('inv-servers-fold', 'The same rows rolled up per MCP server',
    num(rep.servers.length) + ' server' + (rep.servers.length === 1 ? '' : 's'));
  panel.appendChild(el('p', { class: 'note' }, 'One row per server, because that is what you add '
    + 'or remove. A server whose tools are all unused is one decision, not many — which is the '
    + 'unit the list at the top of the page already groups by.'));
  const table = el('table', { class: 'tbl', 'data-testid': 'inv-servers-table' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Server'), el('th', { class: 'num' }, 'Tools'),
      el('th', { class: 'num' }, 'Tools used'), el('th', { class: 'num' }, 'Tokens'),
      el('th', { class: 'num' }, 'Sessions'), el('th', { class: 'num' }, 'Calls'),
      el('th', { class: 'num' }, 'Read for nothing'), el('th', { class: 'num' }, 'Cost of that'))));
  const body = el('tbody');
  table.appendChild(body);
  panel.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, table));
  host.appendChild(panel.closest('details'));
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
  const panel = foldPanel('inv-skills', 'Skills, one row each, with the listing’s own figures',
    num((s.skills || []).length) + ' skill' + ((s.skills || []).length === 1 ? '' : 's')
    + ' · listing ' + num(s.listing_tokens) + ' tok/request');
  host.appendChild(panel.closest('details'));
  if (s.state === 'absent') {
    emptyState(panel.appendChild(el('div')), 'No skills listing in this scope',
      'No session here carried the Skill tool\'s listing.');
    return;
  }
  panel.appendChild(el('p', { class: 'note' },
    // NOT "in the system prompt". The listing arrives as prose in an injected system-ROLE
    // message, which is a different region of the prefix — and now that the panel above shows
    // the two as separate regions, saying otherwise here makes the page contradict itself.
    'Skills are declared as prose in an injected system message — a separate region of the '
    + 'prefix from your system prompt — and the listing is ONE indivisible '
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
  if (!SKILL_SWITCH_SAFE) {
    // ONE visible block, not 24 tooltips. A hover-only reason does not exist on a phone, in a
    // screen reader or in a screenshot, and a disabled control with no stated reason reads as a
    // broken build rather than as a guard.
    panel.appendChild(el('div', { class: 'state blocked', 'data-testid': 'inv-skill-switch-gated-table' },
      el('div', { class: 'state-body' },
        el('strong', {}, 'The per-skill switches are disabled on purpose'),
        el('span', {}, SKILL_SWITCH_REASON),
        el('span', {}, 'The listing weight, the declared count and every token and dollar column '
          + 'in this panel are measured off the listing BLOCK and are unaffected — it is only the '
          + 'mapping from one row to one name that is not yet safe to act on.'))));
  }
  const table = el('table', { class: 'tbl compact', 'data-testid': 'inv-skills-table' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Off'), el('th', {}, 'Skill'), el('th', { class: 'num' }, 'Tokens'),
      el('th', { class: 'num' }, 'Sessions'), el('th', { class: 'num' }, 'Used in'),
      el('th', { class: 'num' }, 'Calls'), el('th', { class: 'num' }, 'Read for nothing'),
      el('th', { class: 'num' }, 'Cost of that'), el('th', {}, 'How to remove it'))));
  const body = el('tbody');
  table.appendChild(body);
  for (const t of s.skills) {
    body.appendChild(el('tr', { class: t.sessions_used ? '' : 'cg-row-unused' },
      // The switch, on EVERY skill and not only the never-invoked ones. A skill the reader used
      // once and does not want to keep paying for is exactly the case the actionable list above
      // cannot reach, and editing a config file by hand was the only route to it.
      el('td', {}, excludeToggle(t)),
      el('td', {}, el('span', { class: 'comp-name trunc', title: t.name, text: t.name })),
      numTd(t.tokens), numTd(t.sessions_declared),
      el('td', { class: 'num' }, t.sessions_used
        ? num(t.sessions_used) : el('span', { class: 'pill missing' }, 'never')),
      numTd(t.calls), readTd(t.unused_reads),
      el('td', { class: 'num' }, money(t.unused_usd, t.priced)),
      el('td', {}, removalDetails(t, 'Command'))));
  }
  panel.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, table));
  panel.appendChild(el('p', { class: 'hint' }, 'Switching a skill off here stops context-guru '
    + 'sending its entry in the listing, from your next session. It does not edit your own '
    + 'configuration — the command beside each row does that, permanently and at source. Two '
    + 'caveats worth knowing. The Skill tool takes a free-form name, so a model that remembers an '
    + 'unlisted skill can still run it: that is deliberate — it fails open — but it means this is '
    + 'a way to stop PAYING for a skill, not a way to forbid it. And a skill your own system '
    + 'prompt names by name is KEPT whatever you switch here, because removing a declaration '
    + 'while the prose describing it survives is the one way this can quietly make the agent '
    + 'worse.'));
}

// SKILL_SWITCH_SAFE gates the per-SKILL opt-out switch on the server's skill parser being
// correct about which row is which.
//
// internal/skills/skills.go requires a ": " delimiter to split a listing entry, and a name-only
// line has none — so 15 of this corpus's 39 real skills are dropped and each dropped name is
// absorbed into the PREVIOUS surviving row. AGGREGATES are fine (the listing's weight is measured
// off the block, not the rows); per-row IDENTITY is not. POST /api/toolfilter resolves the name
// through that same parser, so switching one skill off removes the run of skills folded into it
// and reports one. That is a data-loss bug, it is armed in hosted mode, and a prominent
// call-to-action on a skill row is exactly what would get it hit.
//
// So the switch renders DISABLED with its reason stated in the panel, and the safe route leads:
// the `skillOverrides` snippet in ctaBlock is the reader's own config and never touches the
// parser. impl-safety is fixing skills.go.
//
// TO CLEAR IT: flip this to true in the same PR that lands the parser fix, and delete
// SKILL_SWITCH_REASON's mention of it. A field on /api/toolfilter would be better than a constant
// here — a client-side flag can be flipped without the server being ready — and that is worth
// asking for rather than assuming.
const SKILL_SWITCH_SAFE = false;
const SKILL_SWITCH_REASON = 'This switch is inert on a skill row for now. The server\'s skills '
  + 'parser cannot yet tell two adjacent listing entries apart, so this switch would remove more '
  + 'than the one skill it names. The listing\'s totals are unaffected. Use the settings snippet '
  + 'at the top of the page, or the command on this row — both edit your own configuration and '
  + 'neither goes through that parser.';

/**
 * excludeToggle is the one-click opt-out, extracted so a table row and a card row share it.
 *
 * There were two copies of this checkbox and they had already diverged once (one had the id/for
 * pairing, the other wrapped the whole row in a label). Now there is one, and the two call sites
 * differ only in what they put beside it.
 */
function excludeToggle(t) {
  const key = t.kind + '/' + (t.name || t.server || '');
  const off = isExcluded(t);
  // Every term here is about the ROW or the SERVER, never about the reader: a provider-side tool
  // and a built-in cannot be dropped from here at all, and a skill cannot be dropped SAFELY until
  // the parser can name one (SKILL_SWITCH_SAFE).
  const gated = t.kind === 'skill' && !SKILL_SWITCH_SAFE;
  const fixed = t.kind === 'server_tool' || t.builtin || gated;
  const cb = el('input', {
    type: 'checkbox', 'data-testid': 'inv-toggle-' + key, id: 'inv-cb-' + key,
    title: gated ? SKILL_SWITCH_REASON
      : fixed ? 'This one is not yours to drop from here.'
        : (tools.control ? 'Stop sending this declaration' : 'One-click opt-out is not enabled on '
          + 'this proxy — use the command beside this row'),
    disabled: fixed || !tools.control || tools.busy === key,
    checked: off ? 'checked' : null,
    onchange: (ev) => toggleExcluded(t, ev.currentTarget.checked),
  });
  return cb;
}

/**
 * isExcluded matches a row against the account's removal list. One definition, three callers.
 *
 * An MCP SERVER is matched on its server name rather than on `name`: the config stores it as the
 * bare `mcp__<server>` form and excludedFrom reports that back with kind `mcp_server` and an empty
 * tool name, so a name comparison would never match and the switch would spring back off after a
 * write that had landed.
 */
function isExcluded(t) {
  const list = (tools.control && tools.control.excluded) || [];
  if (t.kind === 'mcp_server') {
    return list.some((e) => e.kind === 'mcp_server' && e.server === t.server);
  }
  return list.some((e) => e.kind === t.kind && e.name === t.name);
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
  const panel = foldPanel('inv-removal-value', 'The per-token rates behind those figures',
    'per 1,000 tokens dropped, per model — the derivation of the per-session numbers above');
  host.appendChild(panel.closest('details'));
  panel.appendChild(el('p', { class: 'note' }, 'Per 1,000 tokens of declarations dropped. Prices '
    + 'differ per model, so this is per model rather than blended.'));
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
      el('span', {}, shortSessionsDominate(rep)
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
    + num(Math.max(0, ...(rep.tools || []).concat(rep.skills.skills || [])
      .map((t) => t.tokens)))
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
  // rep.aside, not a filter over rep.tools: the server decides this split, because it is the same
  // decision that keeps these rows out of every total. A client-side filter would be a second
  // copy of the rule, and a page whose table and whose headline disagreed about which group a row
  // was in would look right in both places.
  const rows = rep.aside || [];
  if (!rows.length) return;
  const tok = rows.reduce((n, t) => n + t.tokens, 0);
  // The warning belongs on the CLOSED bar. It used to live only inside the expanded panel, so
  // the thing a first-time reader actually saw was a plain grey strip with a neutral heading —
  // indistinguishable from the collapsed detail panels elsewhere on the page, and reading as
  // "more of the same, but folded away" rather than as "this is the one section that is not a
  // saving". A danger that is only visible after you have opened the door is decoration.
  const share = pct(100 * tok / Math.max(1, rep.totals.declared_set_tokens), 0);
  const builtins = rows.filter((t) => t.builtin).length;
  const provider = rows.filter((t) => t.kind === 'server_tool').length;
  const det = el('details', { class: 'panel inv-builtins', 'data-testid': 'inv-builtins' },
    el('summary', {},
      el('span', { class: 'pill bad' }, 'not a saving'),
      el('span', { class: 'inv-builtins-title' }, "The agent's own tools — " + num(rows.length)
        + ' items, ' + num(tok) + ' tokens, ' + share + ' of what you declare'),
      el('span', { class: 'section-note' },
        'removing any of these breaks the agent — expand only if you know why you are here')));
  det.appendChild(el('div', { class: 'state blocked', 'data-testid': 'inv-builtins-danger' },
    el('div', { class: 'state-body' },
      el('strong', {}, 'Removing any of these will break the agent'),
      el('span', {}, 'These are Claude Code\'s own tools' + (provider
        ? ' (' + num(builtins) + ') and the provider\'s (' + num(provider) + ')' : '')
        + '. They are not unused capacity you are wasting money on — they are the equipment the '
        + 'model is expected to have, and a session missing one does not degrade gracefully: '
        + 'anything that needed it simply fails.'),
      // The reason they are down here AND out of the numbers, stated where a reader who scrolled
      // this far will meet it. Without this the page looks like it is hiding its biggest group.
      el('span', {}, 'They are ' + share + ' of everything you declare, and they are in NONE of '
        + 'the figures above — not the headline, not the bar, not the totals, not the tables. '
        + 'That is deliberate: a "you never touch 82% of what you carry" assembled mostly out of '
        + 'Read, Bash and Grep is a true number and useless advice, because the only action it '
        + 'suggests is one that breaks your agent. The composition bar near the top of the page '
        + 'is the one place they are counted, because that bar\'s job is to show the whole '
        + 'prompt.'),
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
      // FOLDED, unlike every other command on this page, and that asymmetry is the point. The
      // requirement for this section is that it be visible and deliberately not easy to act on;
      // twenty-two open copy-boxes reading `claude --disallowedTools "Bash"` is the opposite of
      // that, however loud the warning above them. A reader who means it opens the section and
      // then opens the row.
      el('td', {}, removalDetails(t, 'If you really must'))));
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
const promptView = { state: 'idle', view: null, byName: new Map() };

/**
 * loadPrompt fetches the prefix text once and repaints only the reveals that are open.
 *
 * Deliberately NOT a renderTools() — that clears the view and rebuilds every <details>, so
 * the reveal whose click started the fetch would close itself the moment the text arrived.
 * Each reveal registers a repaint of its own body instead.
 */
async function loadPrompt() {
  if (promptView.state === 'loading' || promptView.state === 'ok') return;
  promptView.state = 'loading';
  repaintPrompt();
  try {
    const v = await api('prompt');
    promptView.view = v;
    promptView.byName = new Map();
    for (const r of v.regions || []) promptView.byName.set(r.kind + '/' + r.name, r);
    promptView.state = 'ok';
  } catch (err) {
    if (aborted(err)) return;
    // A 404 is an older proxy that has the report and not this route. That is "the feature
    // is not there", not "something broke", and the two must not read the same.
    promptView.state = /404|not found/i.test(String((err && err.message) || err)) ? 'absent' : 'error';
  }
  repaintPrompt();
}

/** repaintPrompt reruns every registered reveal's own paint. */
function repaintPrompt() {
  for (const fn of promptWaiters) {
    try {
      fn();
    } catch (err) {
      // Still fail-open — one broken reveal must not take the other thirty-six with it — but no
      // longer SILENT. This catch swallowed a real ReferenceError in the region list during
      // development and the panel simply rendered empty: the same failure shape as the duplicate
      // function definition uiinventory_test.go guards, where the page renders, nothing warns, and
      // a whole feature is absent. perf.mjs counts console errors, so this makes it measurable.
      console.error('inventory: a prompt repaint failed and was skipped', err);
    }
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
    if (promptView.state === 'idle' || promptView.state === 'loading') {
      body.appendChild(el('p', { class: 'hint' }, 'Reading…'));
      return;
    }
    if (promptView.state === 'absent') {
      body.appendChild(el('p', { class: 'hint' }, 'This proxy records the token weight of each '
        + 'declaration but not its text. The weight above is exact.'));
      return;
    }
    if (promptView.state === 'error') {
      body.appendChild(el('p', { class: 'hint' }, 'Could not read the prompt text.'));
      return;
    }
    const reg = promptView.byName.get(t.kind + '/' + t.name);
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
  const v = promptView.view || {};
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
 * renderPromptPanel is the whole prefix: drawn to scale, then broken into the regions that own
 * it, then into the parts the system prompt was assembled from — and none of that behind a click.
 *
 * This is the answer to "show me the full system prompt, with each region attributed". It is ONE
 * SESSION's actual prefix — a prefix averaged over sessions is not a prefix anybody sent — and it
 * says which one.
 *
 * It also absorbs what used to be a separate composition panel. That panel summarised this same
 * prefix into five legend numbers and had 1,005 px to itself, while the prefix itself had 254 px
 * and rendered nothing until clicked: the SUMMARY of the prompt was four times the size of the
 * prompt, and both were shares of a whole the reader could not see. One panel, one denominator,
 * bar first and then the thing the bar is a summary of.
 *
 * The regions still fetch on a second request, so the list paints its own loading state and
 * repaints when the shared fetch lands (promptWaiters) rather than blocking the panel.
 */
function renderPromptPanel(host, rep) {
  const p = rep.prompt || {};
  const prefix = (p.tokens || 0) + (rep.totals.declared_set_tokens || 0);
  const panel = el('div', { class: 'panel', 'data-testid': 'inv-prompt-panel' },
    el('div', { class: 'section' },
      el('h2', {}, 'What every request actually carries, region by region'),
      el('span', { class: 'section-note' },
        num(prefix) + ' tokens of standing text and declarations, ahead of your conversation')));
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
    // captured sessions and this is the declaration SET's total — two different quantities, and
    // sharing a key would have given them one explanation that was false for whichever tile the
    // reader was looking at.
    tiles.push(tile('inv-declared-set', 'Declarations', num(rep.totals.declared_set_tokens) + ' tok',
      prefix ? pct(100 * rep.totals.declared_set_tokens / prefix, 1) + ' of the prefix' : ''));
  }
  panel.appendChild(el('div', { class: 'tiles' }, ...tiles));

  // The bar, to scale, over the same whole the tiles just named.
  promptShareBar(panel, rep, prefix);

  // The coverage count, always, and BEFORE the regions: a reader must know how much of their
  // history can answer this before they conclude anything from the one session below.
  if (p.rows) {
    panel.appendChild(el('p', { class: 'hint', 'data-testid': 'inv-prompt-coverage' },
      num(p.text_rows) + ' of ' + num(p.rows) + ' declarations in this window stored their '
      + 'text' + (p.text_rows < p.rows
        ? '. The rest were written before prompt text was captured, or by an account that has '
          + 'not opted in to content capture — they are a gap in the history, not empty prompts.'
        : '.')));
  }

  // NO outer <details>. The regions are the panel's subject, and 63 named regions with their
  // weights and shares behind one closed summary is a panel that renders nothing: measured,
  // this list was 0 atoms in 254 px. What stays behind a click is each region's TEXT, which is
  // the wall of prose the old disclosure was actually protecting the reader from.
  const body = el('div', { class: 'inv-prompt-regions', 'data-testid': 'inv-prompt-regions' });
  panel.appendChild(body);
  const paint = () => paintRegions(body, rep);
  promptWaiters.push(paint);
  paint();
  // Fetched on render rather than on toggle, because there is no toggle any more. It is still
  // one shared request per report, still cached in promptView, and still not a renderTools().
  loadPrompt();
}

/**
 * promptShareBar is "who owns the prefix", as one part-to-whole bar, over the WHOLE prefix.
 *
 * FORM: a horizontal stacked bar, which is what the data's job asks for — part-to-whole with
 * long-named categories. Not a pie (five parts, one of them 1%, and a pie makes the two middle
 * ones indistinguishable), not a treemap (it would imply a hierarchy that is not in the data),
 * and not five tiles (a tile row cannot show that these are shares of one thing).
 *
 * COLOR: the four groups a reader can ACT on take the four categorical slots; the group they must
 * not act on takes the de-emphasis gray. Four hues of equal weight asserted that Claude Code's
 * own equipment was a fifth removable category, which is the reading that gets somebody to
 * delete Read.
 */
function promptShareBar(host, rep, prefix) {
  // Two sources, because the report keeps two lists: rep.tools/rep.skills is what the reader
  // controls, rep.aside is what they do not. This bar is the ONE place both are counted — its
  // job is the whole prompt, and a composition that omitted the largest group would be a
  // composition of nothing.
  const mine = (rep.tools || []).concat(rep.skills.skills || []);
  const aside = rep.aside || [];
  const sum = (rows, f) => rows.filter(f).reduce((n, t) => n + t.tokens, 0);
  const mcp = sum(mine, (t) => t.kind === 'mcp_tool');
  const provider = sum(aside, (t) => t.kind === 'server_tool');
  const builtin = sum(aside, (t) => t.builtin);
  const client = sum(mine, (t) => t.kind === 'tool' && !t.builtin);
  // Skills contribute their LISTING and nothing else. Each skill row's token weight is the
  // weight of its own ENTRY IN that listing — a sub-slice of the same block — so adding the rows
  // to the listing counts every skill twice.
  //
  // ponytail / cross-PR: this is also the client-side half of the `totals` double-booking that
  // dash/toolapi.go still has (impl-safety is fixing it), which is why the headline tile above
  // reads larger than these segments sum to. When that fix lands, `declared_set_tokens` and the
  // headline agree and this line stops being a workaround and becomes just the correct source.
  const skills = rep.skills.listing_tokens || 0;
  const system = (rep.prompt && rep.prompt.tokens) || 0;
  const declared = rep.totals.declared_set_tokens || (mcp + provider + builtin + client + skills);
  const whole = prefix || declared + system;

  host.appendChild(el('p', { class: 'note' }, 'Grouped by whether it is yours to remove. '
    + 'Coloured groups are choices you can change; the grey one is the agent’s own equipment, '
    + 'and removing any of it breaks the agent rather than slimming it.'));
  // Actionable groups first and in the fixed categorical order, so a filter that empties one does
  // not repaint the survivors — the colour of a group follows the GROUP, never its rank.
  const groups = [
    { label: 'MCP tools — yours to remove', value: mcp, color: SERIES[0] },
    { label: 'The skills listing — yours to remove', value: skills, color: SERIES[1],
      note: 'one block; each skill is an entry inside it, not a separate weight' },
    { label: 'Other client tools — whatever agent sent these', value: client, color: SERIES[2] },
    { label: 'Your system prompt — yours, but not from this page', value: system, color: SERIES[3],
      note: 'change it in CLAUDE.md, your output style or your agent definition' },
    { label: 'Claude Code built-ins and provider tools — do not remove',
      value: builtin + provider, color: 'var(--s-mute)' },
  ].filter((g) => g.value > 0);
  stackedShare(host.appendChild(el('div')), groups, {
    testid: 'inv-share', format: num,
    note: 'Drawn to scale, over the ' + num(whole) + ' tokens named above.'
      + (system ? '' : ' No session in this window recorded a system prompt, so that region is '
        + 'absent rather than zero.'),
  });
}

/**
 * paintRegions lists every region of one session's prefix, in the order a reader can act in.
 *
 * NOT heaviest-first, which is what the server sends and what the closed disclosure used to
 * show. Heaviest-first on this corpus is twenty-four of Claude Code's own tools before anything
 * the reader owns — the same inversion the page has at every other scale. So the order is by
 * WHOSE it is, each run under its own heading, and the built-ins fold.
 *
 * The split between "the reader's client tools" and "the agent's own" comes from rep.aside, which
 * is the list the SERVER built when it kept those rows out of every total. The region documents
 * carry no `builtin` flag of their own, and re-deriving one here would be a second copy of a rule
 * that is invisible when it drifts.
 */
function paintRegions(body, rep) {
  clear(body);
  if (promptView.state === 'loading' || promptView.state === 'idle') {
    // Reserves roughly the final height so the panel below does not jump when the text lands.
    body.appendChild(el('div', { class: 'inv-regions-skel', 'aria-busy': 'true' },
      el('p', { class: 'hint' }, 'Reading the prefix…')));
    return;
  }
  if (promptView.state === 'absent') {
    emptyState(body, 'This proxy does not record prompt text',
      'It records the token weight of every region, which is what the figures above are. '
      + 'Reading the text needs a newer proxy.');
    return;
  }
  if (promptView.state === 'error') {
    emptyState(body, 'Could not read the prompt text', 'The figures above are unaffected.');
    return;
  }
  const v = promptView.view || {};
  if (!v.captured) { notCapturedState(body); return; }
  const regions = v.regions || [];
  const maxShare = Math.max(0.01, ...regions.map((r) => r.share || 0));
  body.appendChild(el('p', { class: 'note' }, 'One session’s actual prefix — '
    + num(v.tokens) + ' tokens across ' + num(regions.length) + ' regions, captured '
    + when(v.ts) + '. Grouped by owner; within a group, heaviest first. That is NOT the order '
    + 'the model reads them in — the array order a client sends its tools in is not recorded. '
    + 'Each bar is drawn against the heaviest region here (' + pct(maxShare, 1) + ' of the '
    + 'prefix), not against the whole prefix, so that the small ones are visible at all; the '
    + 'percentage on every row is the true share of the prefix.'));
  const own = new Set((rep.aside || []).map((t) => t.kind + '/' + t.name));
  const bucket = (r) => {
    if (r.kind === 'system_prompt') return 0;
    if (r.kind === 'skill_listing') return 1;
    if (r.kind === 'mcp_tool') return 2;
    if (r.kind === 'skill') return 3;
    return own.has(r.kind + '/' + r.name) ? 5 : 4;
  };
  const HEADS = [
    ['Your system prompt', 'the standing instructions, split at its own headings'],
    ['The skills listing', 'one indivisible block naming every skill'],
    ['Your MCP servers', 'one region per declared MCP tool'],
    ['The skills, one entry each', 'each is a slice of the listing above, not extra weight'],
    ['Other client tools', 'declared by whatever agent sent these requests'],
    ['Claude Code’s own tools', 'not yours to remove'],
  ];
  for (let b = 0; b < HEADS.length; b++) {
    const rows = regions.filter((r) => bucket(r) === b).sort((x, y) => y.tokens - x.tokens);
    if (!rows.length) continue;
    const tok = rows.reduce((n, r) => n + r.tokens, 0);
    const share = rows.reduce((n, r) => n + (r.share || 0), 0);
    const head = el('h3', { class: 'inv-regions-h' },
      el('span', { text: HEADS[b][0] }),
      el('span', { class: 'section-note', text: num(rows.length) + ' region'
        + (rows.length === 1 ? '' : 's') + ' · ' + num(tok) + ' tok · ' + pct(share, 1)
        + ' of the prefix · ' + HEADS[b][1] }));
    // Two runs fold, for two different reasons, and both are stated on the closed summary.
    //
    // The agent's own tools (5) because they are the heaviest group and the one group where acting
    // on the number is a mistake — the same asymmetry the built-ins section at the end of the page
    // uses. The individual skill entries (3) because each one is a SLICE of the skills-listing
    // region immediately above them, not a sibling of it: they are already named, weighed and
    // priced one row each in the decision list at the top of the page, and repeating twenty-four
    // of them here is the fifth granularity of the same rows.
    if (b === 5 || b === 3) {
      const det = el('details', { class: 'inv-regions-fold',
        'data-testid': b === 5 ? 'inv-regions-builtin' : 'inv-regions-skills' },
      el('summary', {}, b === 5 ? el('span', { class: 'pill bad' }, 'not a saving') : null, head));
      for (const r of rows) det.appendChild(promptRegion(r, maxShare));
      body.appendChild(det);
      continue;
    }
    body.appendChild(head);
    for (const r of rows) body.appendChild(promptRegion(r, maxShare));
  }
}

/**
 * promptRegion is one marked region of the prefix: who owns it, how much, and the text.
 *
 * `maxShare` is the heaviest region's share, and the bar is drawn against IT rather than against
 * 100% of the prefix. Every region here is under a fifth of the prefix, so a 100% track draws
 * sixty-three slivers and communicates nothing; normalised to the heaviest, length is readable and
 * the exact share is still on the row as a numeral. The panel's note says which scale it is —
 * an unlabelled normalisation is an unstated denominator.
 */
function promptRegion(r, maxShare) {
  const isSys = r.kind === 'system_prompt';
  const name = isSys ? 'The system prompt itself'
    : (r.kind === 'skill_listing' ? 'The skills listing' : r.name);
  const det = el('details', { class: 'inv-region' + (isSys ? ' inv-region-sys' : ''),
    'data-testid': 'inv-region-' + r.kind + '/' + r.name,
    // The system prompt open by default: it is the region the reader asked to see, it is the
    // largest single one, and it is the only one with an internal structure to read. Everything
    // else is one JSON object or one block of prose, where the summary IS the answer.
    open: isSys ? 'open' : null },
  el('summary', {},
    el('span', { class: 'inv-region-name comp-name', text: name }),
    isSys ? el('span', { class: 'pill neutral' }, 'system') : kindPill(r),
    el('span', { class: 'section-note', text: num(r.tokens) + ' tok · ' + pct(r.share, 1)
      + ((r.parts || []).length ? ' · ' + num(r.parts.length) + ' parts' : '') }),
    // The share, drawn. 63 regions whose only proportion cue is a percentage in 11 px type is a
    // table pretending to be a picture: length is one of the attributes people read accurately
    // and a numeral is not, so the number keeps its bar. Widths are shares of the WHOLE prefix,
    // the same denominator the bar above uses, so the two cannot disagree.
    el('span', { class: 'inv-region-bar', 'aria-hidden': 'true' },
      el('i', { style: 'width:' + Math.max(0.6, Math.min(100,
        (100 * (r.share || 0)) / Math.max(0.01, maxShare || 100))).toFixed(2) + '%' }))));
  if (!r.has_text) {
    notCapturedState(det.appendChild(el('div')));
    return det;
  }
  // The system prompt gets decomposed. Every other region is one JSON object or one block of
  // prose, so there is nothing to decompose and the raw text IS the answer.
  if ((r.parts || []).length) {
    det.appendChild(promptParts(r));
  }
  if ((r.parts || []).length) {
    // Only the decomposed region needs a second level: "the parts" and "the whole as sent" are two
    // different readings of it. Everything else is one JSON object or one block of prose, where a
    // nested disclosure was a second click to reach the only thing behind the first — and 62 of
    // them, one per region, is a third level DESIGN §3.3 does not allow.
    det.appendChild(el('details', { class: 'why', 'data-testid': 'inv-whole-' + r.kind },
      el('summary', {}, 'Or read the whole thing assembled, as it was sent — '
        + num(r.tokens) + ' tokens'),
      el('pre', { class: 'inv-snippet inv-text-pre', text: r.text })));
  } else {
    det.appendChild(el('pre', { class: 'inv-snippet inv-text-pre', text: r.text }));
  }
  return det;
}

/**
 * promptParts is the system prompt broken into the sections it was assembled from.
 *
 * This is the answer to "show me the parts". A 12,000-token system prompt is not one decision:
 * it is the agent's identity preamble, plus the harness rules, plus the environment block, plus
 * whatever the reader's own CLAUDE.md contributed — and only some of those are theirs to change.
 * Undivided, the page could show the number and not the answer.
 *
 * The split is on HEADINGS and the panel says so, because "part" could equally mean the wire
 * blocks (`system` is an array, each block separately cacheable) and a reader comparing this
 * against a breakpoint count would otherwise be misled. Those boundaries are not recoverable —
 * they are joined before anything is stored — so headings are what there is, on every row ever
 * captured rather than only on rows written from today.
 *
 * The parts' weights are measured per part and their SUM is printed beside the region's own
 * total, never reconciled with it: BPE is not additive across a split point, so the two are
 * different measurements of the same bytes and neither is the error. Hiding that would be a
 * small lie in the service of a tidy column.
 */
function promptParts(r) {
  const wrap = el('div', { class: 'inv-parts', 'data-testid': 'inv-parts' });
  const drift = (r.parts_tokens || 0) - r.tokens;
  wrap.appendChild(el('p', { class: 'note' }, num(r.parts.length) + ' parts, split at the '
    + 'prompt\'s own markdown headings — which is how it is organised, not how it is CACHED: the '
    + 'API sends the system prompt as an array of blocks and those boundaries are not recorded. '
    + 'Heaviest part first is NOT the order the model reads them in; that is the order below.'));
  for (const p of r.parts) {
    wrap.appendChild(el('details', { class: 'inv-part inv-part-l' + (p.level || 0),
      'data-testid': 'inv-part-' + p.title },
    el('summary', {},
      el('span', { class: 'inv-part-name comp-name', text: p.title }),
      el('span', { class: 'section-note', text: num(p.tokens) + ' tok · ' + pct(p.share, 1) })),
    el('pre', { class: 'inv-snippet inv-text-pre', text: p.text })));
  }
  wrap.appendChild(el('p', { class: 'hint' }, 'The parts measure '
    + num(r.parts_tokens || 0) + ' tokens against the region\'s ' + num(r.tokens)
    + (drift === 0 ? ' — they agree here.'
      : ', a difference of ' + num(Math.abs(drift)) + '. That is not an error in either: the '
        + 'tokenizer is not additive across a split, so measuring the pieces and measuring the '
        + 'whole are two measurements of the same bytes. The region\'s own figure is the one '
        + 'every other number on this page uses.')));
  return wrap;
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
  const panel = foldPanel('inv-self-removed', 'You removed these yourself',
    num(rows.length) + ' declaration' + (rows.length === 1 ? '' : 's')
    + ' stopped appearing partway through this window — modelled, not measured');
  panel.appendChild(el('p', { class: 'note' }, num(rows.length) + ' declaration'
      + (rows.length === 1 ? '' : 's') + ' stopped appearing partway through this window. The '
      + 'tokens genuinely stopped being billed, no component of ours was involved, and the '
      + 'reduction is credited to YOU — on the Overview tab it is the "Removed by you, not by us" '
      + 'line, and it is inside "Total the bill came down" and deliberately outside "Total cost '
      + 'avoided", which is the figure this product claims for itself.'));
  host.appendChild(panel.closest('details'));
  // The honest limit of the method, up front rather than as a footnote under the weakest row.
  // This is the finding that decided where the figure is allowed to appear: the inference is
  // right about the facts and can be wrong about the cause, and no threshold fixes that.
  panel.appendChild(el('details', { class: 'why', 'data-testid': 'inv-self-removed-basis' },
    el('summary', {}, 'How this is worked out, and when it is wrong'),
    el('p', {}, 'The inventory is a time series. A name that appears in the early sessions of '
      + 'this window and in none of the later ones stopped being declared, and the strength of '
      + 'the claim is the number of COMPARABLE sessions that ran afterwards without it — a '
      + 'session that declared no MCP tool at all cannot testify about a missing MCP tool, so '
      + 'those are not counted. Every row shows its own count and anything under a dozen is '
      + 'marked weak.'),
    el('p', {}, 'What it cannot tell you is WHY the name went. On this deployment\'s own history '
      + 'the largest candidate was the editor integration, which is declared only when a session '
      + 'runs inside the IDE — so "you removed it" and "you worked in a terminal that day" look '
      + 'identical here, and no threshold separates them. The reduction is real either way; the '
      + 'attribution is a guess, which is why this figure is labelled as modelled everywhere it '
      + 'appears and is kept out of the total that claims our credit.'),
    el('p', {}, 'A row the server-side filter is already credited for is marked, and excluded '
      + 'from the Overview figure rather than counted twice.')));
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
    // The projection's OUTCOME, immediately beside the projection. Honesty rule 2 — a
    // projection is not a saving — was right, and was executed as SIZE: 155 px at 66% page
    // depth against a $2.16 projection in 100 pt type in the first screen. Size does not
    // distinguish two claims, it just hides one of them, and a reader who never meets the pair
    // cannot tell them apart at all. ADJACENCY distinguishes them. Nothing else about the rule
    // changes: the projection is still never copied down here, and an absent realized figure
    // still prints as absent.
    renderRealized(host);
    // Then the decision, which is the only thing on this page a reader can act in: the named
    // groups, what each is worth per session and across every session, in that order, with the
    // command that removes it and the one-time cost of running it.
    renderUnused(host, rep);
    // Then the evidence for it — the prefix drawn to scale and broken into the regions that own
    // it, with no click required. This panel used to be 254 px of tiles above a closed
    // disclosure while the panel that merely SUMMARISED the same prefix into five legend
    // numbers had 1,005 px to itself. Those two are now one panel, and this is it.
    renderPromptPanel(host, rep);
    // Everything below re-answers the rows above at a different granularity — grouped, flat,
    // per-server, per-skill — so it is derivation and it is folded (DESIGN §3.3 level 2). It
    // was 5,436 px of four tables and 31 columns answering the same 39 declarations.
    renderRemovalValue(host, rep);
    toolTable(host, rep);
    serverTable(host, rep);
    skillsPanel(host, rep);
    renderSelfRemoved(host, rep);
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
  let controlDoc = null;
  try {
    const c = await api('toolfilter');
    // enabled:false is a proxy that has the endpoint and is not acting; treat it as absent so
    // the copy says "not enabled" instead of offering a switch that does nothing. But KEEP the
    // document either way: it carries the REASON, and discarding it is why the page could only
    // ever say "one-click opt-out is not enabled on this proxy" — which on a hosted deployment
    // was simply false. The proxy had it; the request was ambiguous.
    controlDoc = c || null;
    if (c && c.enabled) control = c;
  } catch (err) {
    if (aborted(err)) return;
    control = null; // 404 on an older proxy, or the feature is not built yet
    controlDoc = null;
  }
  try {
    tools.report = await api('tools');
    tools.control = control;
    tools.controlDoc = controlDoc;
  } catch (err) {
    if (aborted(err)) return;
    errorState(toolsView, 'Could not read the tool inventory', err);
    return;
  }
  renderTools();
}

// Registered here, so mounting this whole view is one line in the shared page.
Object.assign(loaders, { tools: loadTools });
