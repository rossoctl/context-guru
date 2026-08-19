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
  const box = el('div', { class: 'panel cg-gauge', 'data-testid': 'inv-gauge' },
    el('div', { class: 'cg-gauge-head' },
      el('h2', {}, 'Every prompt this account sends'),
      el('span', { class: 'section-note', text: num(t.declared_tokens) + ' declared tokens' })),
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
  const rows = rep.tools.concat(rep.skills.skills || [])
    .filter((t) => !t.sessions_used && t.sessions_declared > 0);
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
    el('p', { class: 'note' }, 'Sorted by what it cost to carry unused. '
      + '"Read for nothing" is this item\'s size times the requests that re-read it in the '
      + 'sessions that never called it.'));
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
  if (!rep.tools.length) {
    tableMessage(body, 8, 'No declarations captured',
      'Nothing in this scope recorded what it declared.');
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

// ── load + render ──────────────────────────────────────────────────────────

function renderTools() {
  const host = clear(toolsView);
  const rep = tools.report;
  if (!rep) return;
  renderHeadline(host, rep);
  if (rep.coverage.captured) {
    renderUnused(host, rep);
    renderRealized(host);
    toolTable(host, rep);
    serverTable(host, rep);
    skillsPanel(host, rep);
  }
  renderCoverage(host, rep);
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
