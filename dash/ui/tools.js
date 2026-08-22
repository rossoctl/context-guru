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

/** kindPill names what sort of thing a row is, because removability depends on it. */
const KIND_LABEL = {
  tool: 'built-in', mcp_tool: 'MCP', server_tool: 'provider', skill: 'skill',
};
function kindPill(t) {
  const k = t.kind || 'tool';
  return el('span', { class: k === 'server_tool' ? 'pill neutral' : 'pill', 'data-kind': k },
    KIND_LABEL[k] || k);
}

/** A number cell. */
function numTd(v) { return el('td', { class: 'num', text: num(v) }); }
/** A compacted number cell, for the read counts that run to hundreds of millions. */
function readTd(v) {
  return el('td', { class: 'num', title: num(v) + ' tokens', text: compact(v) });
}

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
    tile('inv-unused', 'Never invoked', num(t.unused_tokens),
      pct(t.unused_pct) + ' of every prompt, re-read ' + t.requests_per_session.toFixed(0)
      + '× per session', t.unused_pct >= 25 ? 'bad' : ''),
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
      + 'later turn of the session — ' + (rep.totals.requests_per_session || 0).toFixed(1)
      + ' requests per session here. The token counts on this page are those re-reads, not '
      + 'the one-off size.'),
    el('p', {}, 'Each re-read is priced at the tier that request actually paid: cache read '
      + 'for a hit, cache CREATION for the turn that wrote the prefix, and full input rate '
      + 'for a turn with no cache at all. It is not all valued at the cache-read rate, which '
      + 'would understate the cold turns, and not all at the fresh-input rate, which is the '
      + 'same tokens at roughly ten times the price. A figure quoted at one flat tier is a '
      + 'different number from the one here and should not be compared to it.')));
}

// ── the control surface ────────────────────────────────────────────────────

/**
 * renderUnused is the actionable panel: what is declared by sessions and invoked by none.
 *
 * Every row states its own basis (honesty rule 4). The temptation is a tidy "Remove"
 * button and a token count, and that button would be asking the reader to act on absence
 * of evidence: an item unused across 400 sessions and an item unused across the one
 * session captured this morning look identical unless the row says which it is.
 */
function renderUnused(host, rep) {
  // Built-ins and provider tools are excluded here and listed in their own collapsed section
  // at the end of the page. They dominate this list by weight and they are the one group where
  // acting on "never invoked" is a mistake — a built-in that went uncalled for fifty sessions
  // is doing its job by being there on the fifty-first. Leaving them in made the actionable
  // list mostly un-actionable.
  const rows = rep.tools.concat(rep.skills.skills || [])
    .filter((t) => !t.sessions_used && t.sessions_declared > 0
      && !t.builtin && t.kind !== 'server_tool');
  const panel = el('div', { class: 'panel', 'data-testid': 'inv-unused-panel' },
    el('h2', {}, 'Declared by every session, invoked by none'),
    el('p', { class: 'note' }, rows.length
      ? num(rows.length) + ' item' + (rows.length === 1 ? '' : 's') + ' were carried and never '
        + 'called. Each row says how much evidence that rests on.'
      : 'Nothing here was carried without being called.'));
  host.appendChild(panel);
  if (!rows.length) {
    emptyState(panel.appendChild(el('div')), 'Nothing to remove',
      'Every declaration in this scope was invoked in at least one session.');
    return;
  }
  if (rep.coverage.captured < 5) {
    // Honesty rule 4's other half: the same "never invoked" row means something quite
    // different over three sessions than over four hundred, and the reader must be told
    // which they are looking at BEFORE the list, not asked to infer it from a denominator.
    panel.appendChild(el('p', { class: 'hint', 'data-testid': 'inv-small-sample' },
      'Only ' + num(rep.coverage.captured) + ' session'
      + (rep.coverage.captured === 1 ? '' : 's') + ' have been captured so far, so this is a '
      + 'small sample: a row below is evidence that nothing has called that item YET, not '
      + 'proof that nothing will. Every row states its own denominator.'));
  }
  panel.appendChild(reversibilityNote());
  if (!tools.control) {
    // Graceful degradation, stated. The analysis is the whole page's value and stands on
    // its own; only the one-click removal is missing.
    panel.appendChild(el('div', { class: 'state blocked', 'data-testid': 'inv-control-absent' },
      el('div', { class: 'state-body' },
        el('strong', {}, 'Opting an item out is not enabled on this proxy'),
        el('span', {}, 'The list below is still the answer: these are the declarations you '
          + 'are paying to carry and not using. Removing them is a change to the agent\'s own '
          + 'configuration until this proxy can do it for you.'))));
  }
  const list = el('div', { class: 'cg-items' });
  for (const t of rows) list.appendChild(unusedRow(t, rep));
  panel.appendChild(list);
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
 * renderComposition answers "who owns the system prompt", as one bar drawn to scale.
 *
 * FORM. The data's job is part-to-whole across a handful of named groups, and the form
 * heuristic's answer for that is a stacked bar — horizontal, because the names are long. A
 * treemap was the intuitive choice and is the wrong one here: it earns its keep when there are
 * dozens of comparable parts and a hierarchy worth navigating, and with four groups it is a
 * harder-to-read bar. A pie was rejected for the usual reason — angle comparison is the least
 * accurate encoding there is, and it needs a legend to say anything at all.
 *
 * The FOUR groups are also the removability groups, which is what makes this chart actionable
 * rather than decorative: MCP tools and skills are things a user adds and can drop, built-ins
 * are things they must not, and provider tools are not theirs to control. Colour therefore
 * follows a real property of the entity and not its rank, so a filter that changes the sizes
 * never repaints the meaning.
 */
function renderComposition(host, rep) {
  const all = (rep.tools || []).concat(rep.skills.skills || []);
  const sum = (f) => all.filter(f).reduce((n, t) => n + t.tokens, 0);
  const mcp = sum((t) => t.kind === 'mcp_tool');
  const provider = sum((t) => t.kind === 'server_tool');
  const builtin = sum((t) => t.builtin);
  const client = sum((t) => t.kind === 'tool' && !t.builtin);
  const skills = sum((t) => t.kind === 'skill') + (rep.skills.listing_tokens || 0);

  const panel = el('div', { class: 'panel', id: 'inv-composition', 'data-testid': 'inv-composition' },
    el('h2', {}, 'Who owns your system prompt'),
    el('p', { class: 'note' }, 'Every declaration you carry, grouped by whether it is yours to '
      + 'remove. ' + num(rep.totals.declared_set_tokens) + ' tokens in total — the whole that '
      + 'each share below is a part of.'));
  host.appendChild(panel);

  // Four groups, so four categorical steps and no fifth invented hue. `client` and `builtin`
  // are folded into one segment only when one of them is empty, which is the honest way to
  // stay inside a validated four-step palette.
  const groups = [
    { label: 'MCP tools (yours — removable)', value: mcp },
    { label: 'Skills, incl. the listing (yours — removable)', value: skills },
    { label: 'Other client tools (whatever agent sent these)', value: client },
    { label: "Claude Code built-ins (do not remove)", value: builtin + provider },
  ].filter((g) => g.value > 0);
  stackedShare(panel.appendChild(el('div')), groups, {
    testid: 'inv-share', format: num,
    note: 'Drawn to scale. The first two groups are the ones worth acting on; the last is the '
      + 'agent\'s own equipment and removing any of it breaks the agent rather than slimming it.',
  });

  // The per-item detail: ONE measure across many long-named categories, so one hue, not
  // twelve. Twelve colours here would assert twelve different things are being plotted.
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
  panel.appendChild(el('div', { class: 'state', 'data-testid': 'inv-session-basis' },
    el('div', { class: 'state-body' },
      el('strong', {}, 'Session length used: ' + turns.toFixed(0) + ' requests'),
      el('span', {}, 'Measured from this account\'s own history, not assumed. It is the '
        + 'request-weighted average — how many turns the session that a TYPICAL REQUEST '
        + 'belongs to runs for.'),
      el('span', {}, 'The plain average session here is '
        + (rep.totals.requests_per_session || 0).toFixed(1) + ' requests and the median is '
        + num(rep.totals.requests_per_session_median) + ', because most sessions are '
        + 'one-request sidechains — a title generation, a single tool call. Projecting a '
        + 'per-session cost from either of those would understate it by about 40×, so the '
        + 'weighted figure is the one used here and this panel says so.'))));

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
  const det = el('details', { class: 'panel inv-builtins', 'data-testid': 'inv-builtins' },
    el('summary', {},
      el('span', { class: 'inv-builtins-title' }, "The agent's own tools — " + num(rows.length)
        + ' items, ' + num(tok) + ' tokens'),
      el('span', { class: 'section-note' }, 'expand only if you know why you are here')));
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
 * removalCell is the exact thing a user runs, per item, in the form its kind actually needs.
 *
 * Four different mechanisms, and getting them wrong is worse than saying nothing, so the
 * server decides which applies (see dash/toolremoval.go) and this only renders it. The one
 * semantic worth repeating at every call site: the snippets use the BARE tool name, because a
 * scoped permission rule blocks the call and leaves the declaration in the prompt — i.e. saves
 * exactly nothing, which is the opposite of what a reader of this page wants.
 */
function removalCell(t) {
  const r = t.removal || {};
  const wrap = el('div', { class: 'inv-removal' });
  if (r.danger) wrap.appendChild(el('span', { class: 'pill bad', title: r.note }, 'breaks the agent'));
  if (!r.command && !r.settings) {
    wrap.appendChild(el('span', { class: 'hint', text: r.effect || 'No known mechanism.' }));
    if (r.note) wrap.appendChild(whyBlock('Why not', r.note, 'Why ' + t.name + ' cannot be removed here'));
    return wrap;
  }
  if (r.command) wrap.appendChild(copyBox(r.command, 'Run this'));
  if (r.settings) {
    wrap.appendChild(copyBox(r.settings, 'Add to ' + (r.settings_path || 'settings.json')));
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
      el('td', {}, r.sessions_after < 3
        ? el('span', { class: 'pill missing', title: 'Only ' + num(r.sessions_after)
          + ' sessions have run since it was last seen, so this may simply be a session that '
          + 'did not need it rather than a removal.' }, 'weak — too few sessions')
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
  renderHeadline(host, rep);
  if (rep.coverage.captured) {
    // Composition first: "who owns the prompt" is the question the page exists for, and it
    // was answerable only by reading a sortable table.
    renderComposition(host, rep);
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
