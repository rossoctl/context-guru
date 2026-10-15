/* Insights view: what you declare on every request, never call, and could remove.
 *
 * Reads GET /api/insights, which is the inventory already SHAPED into advice — scope, verdict,
 * why, owner rollups, self-exclusion, the removable total. The shaping is in Go (dash/insightsshape.go)
 * rather than here, for two reasons: the honesty rules belong somewhere a test can reach them, and
 * a description comes from the item's own SKILL.md on disk, which a browser cannot read. This file
 * is a renderer and holds no rules of its own.
 *
 * It preserves the static report's look and behaviour deliberately, because the two are the same
 * view of the same question and a user moving between them should not have to relearn it. What it
 * adds, and the static file cannot have, is a session selector that actually re-queries.
 *
 * THE ONE INVARIANT HERE: selectable means removable, and the inverse. A tick box is a promise that
 * the generated command acts on that thing, so only `scope: "item"` rows get one. A plugin's skill
 * and a server's tool have no per-item switch — the OWNER carries the single box that maps to a
 * real command — and those rows carry a marker pointing at it instead. Offering the box anyway and
 * explaining the consequence afterwards is the wrong order: it lets the reader express a choice the
 * page cannot honour and then argues with it.
 */
'use strict';

/* Its own stylesheet, linked from here rather than from index.html so mounting this feature is ONE
 * line in the shared page. Same-origin, so `style-src 'self'` allows it. */
document.head.appendChild(el('link', { rel: 'stylesheet', href: 'insights.css' }));

/* Behaviour group, right after Inventory: that tab answers "what do I declare and what did I use",
 * and this one answers "what should I do about it". Adjacent because a reader who has just seen the
 * weight asks the second question immediately.
 *
 * mountTab (app.js) is the single place that knows the nav's DOM shape, and it returns the empty
 * section already appended and wired as this tab's tabpanel. Built from JS rather than markup
 * because every part of it is conditional on what the document says can be answered. */
const insightsView = mountTab({
  group: 'behaviour', after: 'tools', view: 'insights', label: 'Cleanup',
});
insightsView.appendChild(el('div', { id: 'insights-body' }));

/* Which items the reader has ticked, by name. Module state rather than DOM state, because the
 * tables re-render on every change and a checkbox's own `checked` would not survive it. */
let insSel = {};
/* The last document fetched, so a selection change re-renders without re-querying. */
let insDoc = null;
/* The window in force. 0 means all sessions. */
/* The selected range, as epoch ms. Zero means unbounded — the whole history, which is the
 * default BECAUSE the reader has to see the phases before choosing between them. Replaces an
 * earlier session-count selector: a session is not a unit of time (spans on one real store were
 * 0.0h, 1.1h and 99.1h), so "the last N sessions" was a window nobody could reason about. */
let insSince = 0;
let insUntil = 0;
/* insFullTimeline is the chart's axis, and it is deliberately NOT the timeline of the current
 * query. The chart is a CONTROL: narrowing re-queries, the response carries only the selected
 * days, and a chart rebuilt from that response spans only the selection — so the reader could
 * narrow once and then never widen again, because "all of this chart" had quietly come to mean
 * "all of the two days I already picked". Measured in a browser: after one drag the axis went
 * from 6 columns to 2. The axis therefore comes from the UNWINDOWED load and survives every
 * narrowing after it; only the tables follow the selection. */
let insFullTimeline = null;
/* insProject is the folder being reported on, or '' for every project pooled.
 *
 * A VIEW, NOT A FILTER, and so deliberately outside the shared filter state and outside the URL —
 * the same call state.dim makes for the Usage breakdown. Registering it as a filter dimension
 * would also break the nav contract, which asserts the dimension list is exactly fourteen.
 *
 * '' is "all projects" and is the resting state, so nothing a reader has today is taken away by
 * this feature: the view opens on the same pooled numbers it always showed. */
let insProject = '';
/* The projects the server has seen, filled from the facets endpoint. Held so a re-render does not
 * re-fetch a list that changes about as often as the reader starts a new project. */
let insProjects = null;

const INS_SCOPE_ITEM = 'item';
const INS_WINDOWS = [0, 1, 3, 5, 10, 25];

/* Only a `scope: "item"` row can be removed on its own, and `keep` is never on offer. */
function insSelectable(it) { return it.verdict !== 'keep' && it.scope === INS_SCOPE_ITEM; }

/* A stale item is one the reader is most likely to act on — it was useful once, so they remember
 * installing it, and the date says plainly that it is finished with. Auto-select takes it for the
 * same reason it takes `remove`: the evidence is a measurement, not a thin sample. */
function insRecommended(it) { return it.verdict === 'remove' || it.verdict === 'stale'; }

/* Why a row has no box, and where its real control is. Never a bare disabled control, which
 * invites the reader to wonder what they did wrong. */
function insLockReason(it) {
  if (it.verdict === 'keep') return 'not removable';
  if (it.scope === 'plugin') return 'no per-skill switch — select the plugin above instead';
  if (it.scope === 'server') return 'no per-tool switch — select the server above instead';
  return 'not removable';
}

function insShortName(n) { return n.indexOf(':') > -1 ? n.split(':').pop() : n; }

/* ── the one command ───────────────────────────────────────────────────────
 *
 * Routed by SCOPE and only by scope. `skillOverrides` has no effect on a skill that comes from a
 * plugin — verified on real request traffic — so emitting a per-skill key for one would be a
 * setting the user pastes that silently does nothing. The plugin is the unit there, whole.
 */
function insPlan() {
  const plan = { servers: {}, plugins: {}, deny: [], skills: [], unknown: [], usd: 0, count: 0 };
  if (!insDoc) return plan;
  for (const it of insDoc.items) {
    if (!insSel[it.name]) continue;
    plan.count++;
    plan.usd += it.usd || 0;
    if (it.scope === 'server') {
      if (!it.server) { plan.unknown.push(it.name); continue; }
      (plan.servers[it.server] = plan.servers[it.server] || []).push(it.name);
    } else if (it.scope === 'plugin') {
      if (!it.plugin) { plan.unknown.push(it.name); continue; }
      (plan.plugins[it.plugin] = plan.plugins[it.plugin] || []).push(it.name);
    } else if (it.scope === INS_SCOPE_ITEM) {
      /* Within scope "item" the settings key is the one the KIND answers to. A tool is denied; a
       * skill is overridden. Putting a tool in skillOverrides, or a skill in permissions.deny,
       * writes a key the setting does not read. */
      if (it.kind === 'skill') plan.skills.push(it.name); else plan.deny.push(it.name);
    } else {
      plan.unknown.push(it.name);
    }
  }
  return plan;
}

/* Built by hand rather than with JSON.stringify so the keys come out in the shape settings.json
 * actually wants, in a stable order, with no wrapper object around a fragment meant to be merged. */
function insSettingsBlock(plan) {
  const parts = [];
  if (plan.deny.length) {
    parts.push('  "permissions": {\n    "deny": [\n' +
      plan.deny.slice().sort().map((n) => '      "' + n + '"').join(',\n') + '\n    ]\n  }');
  }
  if (plan.skills.length) {
    parts.push('  "skillOverrides": {\n' +
      plan.skills.slice().sort().map((n) => '    "' + n + '": "off"').join(',\n') + '\n  }');
  }
  return parts.length ? '{\n' + parts.join(',\n') + '\n}' : '';
}

function insCommandText(plan) {
  const lines = [];
  const srv = Object.keys(plan.servers).sort();
  if (srv.length) {
    lines.push('# MCP servers — the server is the unit, so every tool it declares goes with it');
    for (const s of srv) {
      const first = insDoc.items.find((i) => i.name === plan.servers[s][0]);
      lines.push((first && first.command) || ('claude mcp remove ' + s));
    }
  }
  const plg = Object.keys(plan.plugins).sort();
  if (plg.length) {
    if (lines.length) lines.push('');
    lines.push("# plugins — skillOverrides does not reach a plugin's skills, so the plugin is the unit");
    for (const p of plg) {
      const first = insDoc.items.find((i) => i.name === plan.plugins[p][0]);
      lines.push((first && first.command) || ('claude plugin disable ' + p));
    }
    /* NOT `claude /reload-plugins`: /reload-plugins is a slash command typed inside a session, not
     * a shell subcommand, so prefixing it with `claude` produces a line that fails. A block whose
     * whole point is that it can be pasted must not carry one that cannot. */
    lines.push('# then, INSIDE Claude Code, type:  /reload-plugins   (or restart the session)');
  }
  const settings = insSettingsBlock(plan);
  if (settings) {
    if (lines.length) lines.push('');
    lines.push('# merge into ~/.claude/settings.json');
    lines.push(settings);
  }
  return lines.join('\n');
}

/* ── rendering ─────────────────────────────────────────────────────────────── */

function insItemTable(rows, opts) {
  const head = [el('th', {}), el('th', { text: 'item' }), el('th', { text: 'recommendation' })];
  /* No per-row "used / carried" sparkline. It was here, and it duplicated the chart above the
   * tables while crowding every row — the picture belongs in one place, at a size you can read
   * it. The `stale` verdict already carries its own evidence as a date and a day count, which is
   * what the sparkline was really for. */
  if (opts.sessions) head.push(el('th', { text: 'per session' }));
  /* EVERY NUMBER IN THIS TABLE IS WASTE, and the headers have to say so. "cost" and "billed
   * reads" read as what the item cost in total, which is a different figure: a capability you
   * actually invoked also costs tokens, and those were the price of using it rather than money
   * thrown away. This view is only about the waste, so the columns are named for it — otherwise a
   * reader reasonably sums them as a total spend and gets a number this page never computed. */
  /* THE THREE NUMBER COLUMNS ARE ONE FIGURE AT THREE SCALES, and the names have to make that
   * obvious without a legend. An earlier revision called the first "weight / req", leaning on a
   * carrying metaphor the page never explains — a reader asked what it meant, which is the column
   * failing. It is simply the size of the declaration: what this item adds to the prompt of every
   * request. The title attributes carry the full sentence, because a three-word header cannot. */
  head.push(el('th', {
    class: 'num', title: 'Tokens this item adds to the system prompt of EVERY request — its ' +
      'name, description and schema. Sent whether or not you use it.',
    text: 'tokens / request',
  }), el('th', {
    class: 'num', title: 'The per-request size above, multiplied by the requests that re-read ' +
      'it in sessions where it was never invoked. The tokens you were actually billed for.',
    text: 'billed for nothing',
  }), el('th', {
    class: 'num', title: 'Those billed tokens priced at the tier each request really paid — ' +
      'cache read on a hit, the premium write rate on the turn that built the prefix.',
    text: 'wasted',
  }));

  const body = rows.map((it) => {
    const cells = [];
    if (insSelectable(it)) {
      const cb = el('input', {
        type: 'checkbox', 'aria-label': 'select ' + it.name,
        onchange: (e) => { insSetMany([it.name], e.target.checked); },
      });
      cb.checked = !!insSel[it.name];
      insBoxes.push({ el: cb, names: [it.name] });
      cells.push(el('td', {}, cb));
    } else {
      cells.push(el('td', {}, el('span', {
        class: 'ins-lock', title: insLockReason(it),
        text: (it.scope === 'plugin' || it.scope === 'server') ? '↰' : '—',
      })));
    }
    /* The description is on the name's title: measured on a real inventory, 41 of 58 rows had
     * none, so a column for it took width from the numbers on every row. The FULL text, since a
     * tooltip has no layout to protect, and the full identifier too — a row labelled
     * `brainstorming` inside the superpowers fold is ambiguous out of context. */
    const full = it.desc_full || it.desc;
    cells.push(el('td', {
      class: 'ins-nm' + (full ? ' tip' : ''),
      title: full ? it.name + '\n\n' + full
        : (it.desc_known === false && it.kind === 'skill'
          ? it.name + '\n\nNo description: built into Claude Code, with no SKILL.md to read one from.'
          : it.name),
      text: insShortName(it.name),
    }));

    const vd = el('td', {},
      el('span', { class: 'ins-vd ' + (it.verdict || 'unknown'), text: it.verdict || 'no verdict' }));
    if (it.why) vd.appendChild(el('span', { class: 'ins-why', text: it.why }));
    cells.push(vd);

    if (opts.sessions) cells.push(el('td', { text: insSessionCell(it) }));
    /* An invoked item has no figure in any of these three columns, and the dash needs a reason on
     * hover rather than reading as missing data: its tokens were the price of using it. That is
     * not waste, and this page reports nothing else — so the cell is empty by design, not by
     * omission. */
    const why = it.invoked
      ? { title: 'invoked ' + (it.calls || 0) + ' time(s) \u2014 these tokens bought something, ' +
          'so there is no waste to report. This page counts waste only.' }
      : {};
    cells.push(el('td', Object.assign({ class: 'num' }, why),
      it.invoked || !it.tokens ? '—' : it.tokens.toLocaleString()));
    cells.push(el('td', Object.assign({ class: 'num' }, why),
      it.invoked || !it.reads ? '—' : it.reads.toLocaleString()));
    cells.push(el('td', Object.assign({ class: 'num' }, why),
      it.invoked ? '—' : (it.priced ? usd(it.usd) : 'unpriced')));
    return el('tr', {}, ...cells);
  });

  return el('div', { class: 'tblwrap' },
    el('table', { class: 'tbl' },
      el('thead', {}, el('tr', {}, ...head)),
      el('tbody', {}, ...body)));
}

/* The per-session spread, which is the insight the window exists for: the same unused skill costs
 * very differently in a 21-request session and a 287-request one, and the per-request weight hides
 * that completely. */
function insSessionCell(it) {
  const s = it.sessions || [];
  if (!s.length) return '—';
  const sorted = s.slice().sort((a, b) => (b.requests || 0) - (a.requests || 0));
  const hi = sorted[0], lo = sorted[sorted.length - 1];
  if (sorted.length > 1 && lo.requests > 0 && hi.requests / lo.requests >= 2) {
    return hi.requests + ' req vs ' + lo.requests + ' — ' +
      (hi.requests / lo.requests).toFixed(1) + '×';
  }
  return sorted.length + ' session' + (sorted.length === 1 ? '' : 's');
}

/* A SELECTION CHANGE MUST NOT REBUILD THE PAGE.
 *
 * This used to call insRender(), which clears #insights-body and builds it again — so every ticked
 * box collapsed all the open folds, moved focus to the top, and scrolled the review into view.
 * Selecting a second item then meant re-opening the fold you were working in, which makes
 * multi-select unusable and was reported as exactly that.
 *
 * So the two things that actually depend on the selection are updated in place: the checkboxes
 * that mirror it (an owner's box is checked or indeterminate from its children) and the review
 * block at the bottom. Everything else — the folds, the tables, the scroll position, the focused
 * element — is left exactly as the reader left it.
 */
function insSetMany(names, on) {
  for (const n of names) { if (on) insSel[n] = true; else delete insSel[n]; }
  insSyncSelection();
}

/* Every checkbox registers itself here with the names it governs, so a selection change can
 * refresh the controls without knowing how the page is laid out. Rebuilt by insRender, which is
 * the only thing that replaces the DOM. */
let insBoxes = [];

function insSyncSelection() {
  for (const b of insBoxes) {
    const on = b.names.filter((n) => insSel[n]).length;
    if (b.names.length === 1) {
      b.el.checked = on === 1;
    } else {
      /* An owner's box is checked only when all of its children are, and indeterminate while some
       * are — the same three-state convention a file tree uses, so a partial selection does not
       * read as "none". */
      b.el.checked = on > 0 && on === b.names.length;
      b.el.indeterminate = on > 0 && on < b.names.length;
    }
  }
  insRenderActions();
  const host = document.getElementById('insights-review');
  if (!host) return;
  /* Replacing the review would drop focus if it currently sits inside it, and the browser would
   * then move focus to the document — scrolling the reader away from the box they just ticked.
   * Only restore focus that was ALREADY in here: stealing it from a checkbox would be the same
   * bug in the other direction. */
  const refocus = host.contains && host.contains(document.activeElement);
  clear(host);
  insRenderReview(host);
  if (refocus) {
    const pre = host.querySelector('pre.copyblock');
    if (pre) pre.focus();
  }
}

function insOwnerBlock(o, children, opts) {
  const head = el('div', { class: 'ins-head' });
  /* The unit's one control, on the unit's own line. For a plugin or a server the unit is
   * indivisible, so a separate "select this whole plugin — all 1 items" row offered a choice that
   * does not exist. A `keep` child is never included: a built-in is removed by nothing here. */
  const pickable = children.filter((it) => it.verdict !== 'keep' &&
    (opts.unit ? (it.scope === 'plugin' || it.scope === 'server') : insSelectable(it)))
    .map((it) => it.name);
  if (pickable.length) {
    const cb = el('input', {
      type: 'checkbox',
      'aria-label': opts.unit ? 'select ' + o.name + ' for removal'
        : 'select every removable item in ' + o.name,
      onchange: (e) => insSetMany(pickable, e.target.checked),
    });
    const on = pickable.filter((n) => insSel[n]).length;
    cb.checked = on > 0 && on === pickable.length;
    cb.indeterminate = on > 0 && on < pickable.length;
    insBoxes.push({ el: cb, names: pickable });
    head.appendChild(cb);
  }
  head.appendChild(el('span', { class: 'ins-type', text: opts.type }));
  head.appendChild(el('span', { class: 'ins-name', text: o.name }));
  head.appendChild(el('span', { class: 'pill ' + (opts.pillClass || ''), text: opts.pill }));
  head.appendChild(el('span', { class: 'ins-meta', text: opts.meta }));
  /* LABELLED, not a bare figure. The owner header showed `$0.4964` with nothing saying what it
   * was, so it read as what the plugin cost — which is a different number, and one this page
   * never computes. It is the sum of what its unused children wasted. */
  head.appendChild(el('span', { class: 'ins-cost', title: 'Total wasted by this group over the ' +
    'measured window: the sum of its never-invoked children. What you actually used is not in it.' },
    el('span', { class: 'ins-cost-k', text: 'wasted ' }), opts.cost));
  if (opts.unitNote) head.appendChild(el('span', { class: 'ins-unit', text: opts.unitNote }));
  if (o.command) head.appendChild(el('code', { class: 'ins-cmd', text: o.command }));

  /* The detail is folded away until asked for. With 58 items across five owners the page opened as
   * a wall of rows, which buries the decision the reader came for: whether this whole thing is
   * worth its price. */
  const body = el('details', { class: 'ins-body' },
    el('summary', {},
      el('span', { class: 'ins-caret', text: '▸' }),
      children.length === 1 ? 'show the 1 item inside' : 'show the ' + children.length + ' items inside'));
  for (const band of (opts.bands || [])) {
    if (!band.rows.length) continue;
    body.appendChild(el('div', { class: 'ins-band' },
      el('span', { class: 'ins-band-n', text: band.label }),
      el('span', { class: 'ins-band-m', text: band.rows.length + ' items' })));
    body.appendChild(insItemTable(band.rows, opts));
  }
  if (!opts.bands) body.appendChild(insItemTable(children, opts));
  if (opts.note) body.appendChild(el('p', { class: 'hint', text: opts.note }));

  return el('div', { class: 'ins-own' + (opts.keep ? ' keep' : '') }, head, body);
}

/* The buttons, in their own host so a selection change can refresh their disabled state without
 * rebuilding the inventory above them. `Clear` goes through insSetMany too: resetting the object
 * and re-rendering would collapse every fold, which is the bug this split exists to fix. */
function insRenderActions() {
  const host = document.getElementById('insights-actions');
  if (!host || !insDoc) return;
  clear(host);
  const removeSet = (insDoc.items || []).filter(insRecommended);
  const chosen = Object.keys(insSel);
  host.appendChild(el('div', { class: 'actions' },
    el('button', {
      type: 'button', class: 'primary',
      disabled: removeSet.length === 0,
      text: 'Auto-select recommended (' + removeSet.length + ')',
      onclick: () => { insSel = {}; insSetMany(removeSet.map((i) => i.name), true); },
    }),
    el('button', {
      type: 'button',
      disabled: chosen.length === 0,
      text: chosen.length ? 'Clear (' + chosen.length + ')' : 'Clear',
      onclick: () => { insSetMany(chosen, false); },
    })));
}

function insRenderReview(root) {
  const plan = insPlan();
  const box = el('div', { class: 'ins-review' });
  box.appendChild(el('div', { class: 'ins-review-h', text: 'Your one command' }));

  if (!plan.count) {
    const anyRemove = insDoc.items.some(insRecommended);
    box.appendChild(el('p', { class: 'empty' },
      anyRemove
        ? 'Tick an item, or use auto-select to take what is past the observation floor.'
        : 'Nothing is auto-recommended yet: every unused item here is still below the ' +
          'observation floor. The picker still works — tick any item or owner above and the ' +
          'exact command to remove it appears here. You know what you installed and never use; ' +
          'the floor only governs what this view volunteers.'));
    root.appendChild(box);
    return;
  }

  /* tabindex so the block is reachable and selectable by keyboard. It is the only focusable thing
   * the review builds, which is why rebuilding the review used to STEAL focus from the checkbox
   * the reader had just ticked and scroll it into view — the other half of the multi-select
   * complaint. Focus is restored below if it was ours to begin with. */
  box.appendChild(el('pre', { class: 'copyblock', tabindex: '0', text: insCommandText(plan) }));

  /* The itemised decision. The command alone is not a review: `claude plugin disable superpowers`
   * does not say that 15 skills go with it or what they were worth, and a reader about to change
   * their own configuration is entitled to the whole decision in one place. */
  box.appendChild(el('div', { class: 'ins-review-h', text: 'Removing, in full' }));
  const list = el('ul', { class: 'ins-list' });
  const sum = (names) => names.reduce((a, n) => {
    const it = insDoc.items.find((i) => i.name === n);
    return a + ((it && it.usd) || 0);
  }, 0);
  for (const p of Object.keys(plan.plugins).sort()) {
    const k = plan.plugins[p];
    list.appendChild(el('li', {}, el('strong', { text: 'plugin ' + p }),
      ' — ' + k.length + (k.length === 1 ? ' skill' : ' skills') + ', ' + usd(sum(k)) +
      ' wasted this window: ' + k.map(insShortName).sort().join(', ')));
  }
  for (const s of Object.keys(plan.servers).sort()) {
    const k = plan.servers[s];
    list.appendChild(el('li', {}, el('strong', { text: 'MCP server ' + s }),
      ' — ' + k.length + (k.length === 1 ? ' tool' : ' tools') + ', ' + usd(sum(k)) +
      ' wasted this window: ' + k.map(insShortName).sort().join(', ')));
  }
  if (plan.deny.length) {
    list.appendChild(el('li', {}, el('strong', { text: 'tools denied' }),
      ' — ' + plan.deny.length + ', ' + usd(sum(plan.deny)) + ' wasted this window: ' +
      plan.deny.slice().sort().join(', ')));
  }
  if (plan.skills.length) {
    list.appendChild(el('li', {}, el('strong', { text: 'skills switched off' }),
      ' — ' + plan.skills.length + ', ' + usd(sum(plan.skills)) + ' wasted this window: ' +
      plan.skills.slice().sort().join(', ')));
  }
  box.appendChild(list);

  box.appendChild(el('div', { class: 'ins-total' }, el('strong', { text: usd(plan.usd) }),
    ' is what these ' + plan.count + (plan.count === 1 ? ' item' : ' items') +
    ' WASTED over the measured window: carried on every request and invoked in none of them. ' +
    'A measurement rather than a forecast — it already happened, so the same traffic would have ' +
    'cost that much less. Nothing you actually used is in this figure.'));

  box.appendChild(el('div', { class: 'ins-review-h', text: 'What to do next' }));
  const steps = el('ol', { class: 'ins-steps' });
  const hasShell = Object.keys(plan.plugins).length > 0 || Object.keys(plan.servers).length > 0;
  const hasSettings = plan.deny.length > 0 || plan.skills.length > 0;
  if (hasShell) steps.appendChild(el('li', { text: 'Run the shell lines in a terminal. They ' +
    'change your Claude Code installation, not this project.' }));
  if (hasSettings) steps.appendChild(el('li', { text: 'Merge the settings block into ' +
    '~/.claude/settings.json. If it already has a "permissions" or "skillOverrides" key, add to ' +
    'it rather than replacing it.' }));
  if (Object.keys(plan.plugins).length) {
    steps.appendChild(el('li', { text: 'Type /reload-plugins inside Claude Code, or start a new ' +
      'session, so the change takes effect.' }));
  } else if (hasSettings) {
    steps.appendChild(el('li', { text: 'Start a new session. The system prompt is built once per ' +
      'session, so the saving begins with the next one rather than this one.' }));
  }
  steps.appendChild(el('li', { text: 'Reload this view afterwards to confirm the weight is gone.' }));
  if (Object.keys(plan.plugins).length) {
    steps.appendChild(el('li', { text: 'Reversible: claude plugin enable <name> puts a disabled ' +
      'plugin back, with its skills.' }));
  }
  box.appendChild(steps);

  if (plan.unknown.length) {
    box.appendChild(el('div', { class: 'ins-warn', text: 'Not offered: ' +
      plan.unknown.join(', ') + '. These carry no mechanism this view can turn into a command, ' +
      'and guessing one would hand you a line that does nothing.' }));
  }
  root.appendChild(box);
}

/* ── the view ──────────────────────────────────────────────────────────────── */

/* One scale for every row, recomputed per render. Module-level so insItemTable can reach it
 * without threading it through four call sites. */

function insRender() {
  const root = document.getElementById('insights-body');
  if (!root || !insDoc) return;
  clear(root);
  /* The registry describes the DOM that is about to be discarded. */
  insBoxes = [];

  /* THE PAGE INTRODUCES ITSELF FIRST.
   *
   * It used to open with the session chips, then a thin muted line, then the tiles — so the first
   * thing a reader met was a control, and the one sentence that defines every number on the page
   * was the least prominent text on it. Reported as "I don't see explanations and clear
   * presentation", which was fair: the words existed and the layout hid them.
   *
   * Now it opens the way every other tab does, with a titled panel, and the definition is body
   * text inside it rather than a caption. The controls come after the explanation, because the
   * reader has to know what is being counted before choosing how much of it to count. */
  const intro = el('div', { class: 'panel' },
    el('h2', { text: 'What your unused capabilities cost' }),
    el('p', { text: 'Every figure on this page is WASTE: tokens sent in the system prompt of ' +
      'every request, for a tool, skill or MCP server that was never invoked in the sessions ' +
      'that carried it.' }),
    el('p', { text: 'What you actually used is NOT counted here. Those tokens bought something — ' +
      'they were the price of using the capability, not money thrown away. So nothing below is a ' +
      'total spend, and removing all of it would not change what your working sessions cost. It ' +
      'changes what you pay to carry capabilities you never reach for.' }),
    el('p', { text: 'The three number columns are one figure at three scales. ' +
      'TOKENS / REQUEST is the size of the declaration — what the item adds to the prompt every ' +
      'single time, whether you use it or not. BILLED FOR NOTHING is that size multiplied by the ' +
      'requests that re-read it without ever invoking it. WASTED prices those tokens at what ' +
      'each request really paid.' }),
    el('p', { class: 'hint', text: 'Hover any column header or item name for the long version. ' +
      'A row showing no figures was invoked, so it has no waste to report.' }));
  root.appendChild(intro);

  /* THE CHART IS THE CONTROL, and it comes before the tables because the reader chooses a
   * range from it. At rest it shows the whole history, so the range is picked from evidence
   * rather than guessed — a brush on a blank frame would make the reader supply the knowledge
   * this view was supposed to give them. */
  const chartPanel = el('div', { class: 'panel' },
    el('h2', { text: 'What your unused capabilities cost, worst first' }),
    /* Rewritten with the form. This described a heatmap's colours and its day grid, and before
     * that a magnitude ramp and a volume strip — none of which exist. A hint explaining an older
     * version of the picture is worse than none, because the reader trusts it. */
    el('p', { class: 'hint', text: 'One bar per capability you can remove, longest first, showing '
      + 'what carrying it costs you for nothing. Red means the evidence is sufficient to '
      + 'recommend removing it; amber means it has never been invoked but has not yet been '
      + 'observed long enough to be sure. Colours match the tables below, and the figure on each '
      + 'bar is the same number the tables report.' }));
  chartPanel.appendChild(insProjectPicker());
  const chart = insChart();
  if (chart) chartPanel.appendChild(chart);
  chartPanel.appendChild(insScopeLine());
  root.appendChild(chartPanel);

  /* The range, between the chart and everything it governs. A reader who scrolls past the chart
   * still has to know which period the numbers describe. */
  root.appendChild(insRangeBanner());


  const items = insDoc.items || [];
  const unremovable = items.filter((i) => i.scope === 'none');
  const removeSet = items.filter(insRecommended);

  /* The headline, in the page's own tile vocabulary. */
  const tiles = el('div', { class: 'grid' },
    el('div', { class: 'tile good' },
      el('div', { class: 'k', text: insSince ? 'wasted in range' : 'wasted, recoverable' }),
      el('div', { class: 'v', text: usd(insDoc.removable_usd || 0) }),
      el('div', { class: 's', text: items.filter((i) => i.scope && i.scope !== 'none').length +
        ' unused items you can remove · ' + (insDoc.removable_tokens || 0).toLocaleString() +
        ' tok/req thrown away' })),
    el('div', { class: 'tile' },
      el('div', { class: 'k', text: 'owners' }),
      el('div', { class: 'v', text: String((insDoc.owners || []).length) }),
      el('div', { class: 's', text: (insDoc.owners || []).filter((o) => o.kind === 'plugin').length +
        ' plugin(s), ' + (insDoc.owners || []).filter((o) => o.kind === 'server').length + ' MCP server(s)' })),
    el('div', { class: 'tile' },
      el('div', { class: 'k', text: 'recommended to remove' }),
      el('div', { class: 'v', text: String(removeSet.length) }),
      el('div', { class: 's', text: removeSet.length
        ? 'past the observation floor'
        : 'nothing past the floor yet — the list below is still complete' })));
  if (unremovable.length) {
    tiles.appendChild(el('div', { class: 'tile bad' },
      el('div', { class: 'k', text: insSince ? 'unrecoverable, in range' : 'wasted, unrecoverable' }),
      el('div', { class: 'v', text: usd(unremovable.reduce((a, i) => a + (i.usd || 0), 0)) }),
      el('div', { class: 's', text: unremovable.length + ' unused built-ins · real waste, ' +
        'but removing one breaks the agent' })));
  }
  root.appendChild(tiles);

  /* Owners, each one collapsed. */
  const byPlugin = {}, byServer = {};
  for (const it of items) {
    if (it.plugin) (byPlugin[it.plugin] = byPlugin[it.plugin] || []).push(it);
    else if (it.server) (byServer[it.server] = byServer[it.server] || []).push(it);
  }
  const owners = (insDoc.owners || []).slice().sort((a, b) => (b.usd || 0) - (a.usd || 0));
  const plugins = owners.filter((o) => o.kind === 'plugin');
  const servers = owners.filter((o) => o.kind === 'server');

  if (plugins.length) {
    root.appendChild(el('h2', { text: 'Plugins' }));
    root.appendChild(el('p', { class: 'hint', text: 'A plugin contributes skills and may ' +
      'contribute MCP servers. Its skills cannot be switched off one at a time, so the tick on ' +
      "the plugin's own line is the only control — and it removes all of them together." }));
    for (const o of plugins) {
      const kids = byPlugin[o.name] || [];
      root.appendChild(insOwnerBlock(o, kids, {
        type: 'plugin', pill: 'removable', unit: true, unitNote: 'the plugin is the unit',
        meta: o.skills + ' skills · ' + (o.mcp_tools || 0) + ' MCP tools · ' +
          (o.tokens || 0).toLocaleString() + ' tok/req',
        cost: usd(o.usd || 0),
        sessions: kids.some((k) => (k.sessions || []).length),
        bands: [
          { label: 'skills', rows: kids.filter((k) => k.kind === 'skill') },
          { label: 'mcp tools', rows: kids.filter((k) => k.kind === 'mcp_tool') },
        ],
        note: "skillOverrides has no effect on a plugin's skills, so the rows above carry no " +
          'checkboxes of their own. Disabling keeps the plugin installed; uninstall removes it. ' +
          'Either needs /reload-plugins.',
      }));
    }
  }
  if (servers.length) {
    root.appendChild(el('h2', { text: 'MCP servers' }));
    root.appendChild(el('p', { class: 'hint', text: 'Configured directly, independent of any ' +
      'plugin. Removing the server removes every tool it declares.' }));
    for (const o of servers) {
      const kids = byServer[o.name] || [];
      root.appendChild(insOwnerBlock(o, kids, {
        type: 'mcp server', pill: 'removable', unit: true, unitNote: 'the server is the unit',
        meta: o.tools + ' tools · ' + (o.tokens || 0).toLocaleString() + ' tok/req',
        cost: usd(o.usd || 0),
        sessions: kids.some((k) => (k.sessions || []).length),
        note: 'Denying a single tool keeps the server connected and the rest of its tools ' +
          'declared. There is no `claude mcp disable`; use the /mcp panel to switch one off.',
      }));
    }
  }

  /* Owned by nothing, and removable individually. */
  const solo = items.filter((i) => i.scope === INS_SCOPE_ITEM && !i.plugin && !i.server);
  if (solo.length) {
    root.appendChild(el('h2', { text: 'Removable individually' }));
    root.appendChild(el('p', { class: 'hint', text: 'Owned by no plugin and no server, so each ' +
      'one can be switched off on its own. This is usually where most of the waste is: a single ' +
      'heavy tool declaration outweighs a whole plugin of small skill entries.' }));
    root.appendChild(insOwnerBlock(
      { name: 'standalone items', usd: solo.reduce((a, i) => a + (i.usd || 0), 0) },
      solo.slice().sort((a, b) => (b.usd || 0) - (a.usd || 0)),
      {
        type: 'unowned', pill: 'removable individually', unit: false,
        meta: solo.length + ' items · ' +
          solo.reduce((a, i) => a + (i.tokens || 0), 0).toLocaleString() + ' tok/req',
        cost: usd(solo.reduce((a, i) => a + (i.usd || 0), 0)),
        sessions: solo.some((k) => (k.sessions || []).length),
        note: 'A skill is hidden with skillOverrides set to "off". Two cheaper settings keep it ' +
          'usable: "name-only" lists the name without the description, which is nearly all of ' +
          'the weight, and "user-invocable-only" hides it from the model while the slash ' +
          'command still works for you.',
      }));
  }

  /* Built-ins: listed so the weight is visible, offered at no scope. */
  if (unremovable.length) {
    root.appendChild(el('h2', { text: 'Claude Code built-ins' }));
    root.appendChild(el('p', { class: 'hint', text: 'Shown because the waste is real and you ' +
      'should know where the weight sits \u2014 not because it is available to you. These are the ' +
      'agent\u2019s own tools: removing one takes away a capability the model is expected to ' +
      'have, and anything that depended on it fails. No checkboxes are offered.' }));
    root.appendChild(insOwnerBlock(
      { name: 'built-in tools', usd: unremovable.reduce((a, i) => a + (i.usd || 0), 0) },
      unremovable.slice().sort((a, b) => (b.usd || 0) - (a.usd || 0)),
      {
        type: 'claude code', pill: 'removing breaks the agent', pillClass: 'bad', keep: true,
        unit: false,
        meta: unremovable.length + ' items · ' +
          unremovable.reduce((a, i) => a + (i.tokens || 0), 0).toLocaleString() + ' tok/req',
        cost: usd(unremovable.reduce((a, i) => a + (i.usd || 0), 0)),
        sessions: false,
        note: 'Removing one takes away a capability the model is expected to have, and anything ' +
          'that depended on it fails. The saving is real and almost never worth taking.',
      }));
  }

  /* The actions, then the review. Below the inventory, because this is the consequence of what the
   * reader just ticked — above it, it read as a header describing nothing yet. */
  root.appendChild(el('h2', { text: 'What you decided to remove' }));
  root.appendChild(el('p', { class: 'hint', text: 'Tick anything above and it is summarised here ' +
    'with the exact command. Nothing on this page changes your configuration \u2014 it writes you ' +
    'one command to run yourself.' }));
  root.appendChild(el('div', { id: 'insights-actions' }));
  root.appendChild(el('div', { id: 'insights-review' }));
  insRenderActions();
  insRenderReview(document.getElementById('insights-review'));

  /* The self-exclusion footnote. A reader who counts more skills in /plugin than here must be able
   * to find out why; "the tool hid its own cost" is a worse look than the cost. */
  const se = insDoc.self_excluded;
  if (se && se.items) {
    root.appendChild(el('p', { class: 'hint', text: se.plugin +
      ' is excluded from every table above: it is the plugin producing this view, so counting ' +
      'its weight as a saving it recommends would be circular. For the record, ' + se.items +
      ' skills, ' + (se.tokens || 0).toLocaleString() + ' tok/req' +
      (se.usd ? ', ' + usd(se.usd) + ' wasted this window' : '') +
      (se.command ? '. Remove it with: ' + se.command : '.') }));
  }
}

async function loadInsights() {
  const root = document.getElementById('insights-body');
  if (root && !insDoc) root.appendChild(el('p', { class: 'empty', text: 'Loading…' }));
  try {
    const q = { timeline: 1 };
    if (insSince) q.since = Math.round(insSince);
    if (insUntil) q.until = Math.round(insUntil);
    /* Scoped server-side, not filtered here: the verdicts, the observation floor and the owner
     * rollups are all computed over the filter's window, so narrowing in JS would leave the
     * verdicts describing every project while the rows described one. */
    if (insProject) q.project = insProject;
    insDoc = await api('insights', q);
    insVerdictMap = null;        // the verdicts moved with the document
    /* Captured only when nothing is selected, which is the one response that covers everything. */
    if (!insSince && !insUntil && insDoc.timeline) insFullTimeline = insDoc.timeline;
    /* The project list, fetched once. Its own request rather than the shared loadFacets, because
     * this is a VIEW-LOCAL control: adding it to FACET_DIMS would put it in the shared filter bar
     * and in the nav's dimension contract, which asserts that list is exactly fourteen long. */
    if (insProjects === null) {
      insProjects = [];
      try {
        const f = await api('facets');
        if (f && Array.isArray(f.project)) insProjects = f.project.filter(Boolean).sort();
      } catch (_) {
        /* A picker that cannot list projects is a missing control, not a broken page: the
         * pooled view below is still correct, which is what the reader came for. */
      }
    }
  } catch (e) {
    if (root) {
      clear(root);
      root.appendChild(el('p', { class: 'empty', text: 'Could not read the inventory: ' + e.message }));
    }
    return;
  }
  /* A selection naming an item the new window does not contain would silently inflate the command.
   * Dropped on every load rather than kept, because a tick the reader can no longer see is a tick
   * they cannot withdraw. */
  const present = new Set(insDoc.items.map((i) => i.name));
  for (const n of Object.keys(insSel)) if (!present.has(n)) delete insSel[n];
  insRender();
}


/* ── the range, which is the actual feature ─────────────────────────────────
 *
 * The chart is a CONTROL. Narrow the range and every table below re-queries, so the advice
 * describes the period the reader cares about rather than an average over three months of mixed
 * phases — a "general recommendation that may not be relevant any more".
 *
 * It RE-QUERIES rather than filtering the rendered rows, and that is not laziness: the verdicts,
 * the observation floor and the owner rollups are all computed server-side over the filter's
 * window. Filtering in JS would leave the verdicts describing all history while the rows describe
 * a slice — the quiet kind of mismatch this whole report exists to avoid.
 */

/* No preset range chips. They were a second selection surface beside the chart, and the chart IS
 * the control — "I don't need second range selection grid, I want to select directly from chart".
 * The banner's "show all history" button is the only range control that remains, because
 * clearing a selection needs a target that is not a gesture. */

/* The scope line. Stated as a sentence and not only as two dates, because every number below it
 * means something different depending on this range, and a reader who skipped the chips would
 * otherwise read a narrowed figure as an all-time one. */

/* ── the range, which is the actual feature ─────────────────────────────────
 *
 * The chart is a CONTROL. Narrow the range and every table below re-queries, so the advice
 * describes the period the reader cares about rather than an average over three months of mixed
 * phases — a "general recommendation that may not be relevant any more".
 *
 * It RE-QUERIES rather than filtering the rendered rows, and that is not laziness: the verdicts,
 * the observation floor and the owner rollups are all computed server-side over the filter's
 * window. Filtering in JS would leave the verdicts describing all history while the rows describe
 * a slice — the quiet kind of mismatch this whole report exists to avoid.
 */

/* No preset range chips. They were a second selection surface beside the chart, and the chart IS
 * the control — "I don't need second range selection grid, I want to select directly from chart".
 * The banner's "show all history" button is the only range control that remains, because
 * clearing a selection needs a target that is not a gesture. */

/* The scope line. Stated as a sentence and not only as two dates, because every number below it
 * means something different depending on this range, and a reader who skipped the chips would
 * otherwise read a narrowed figure as an all-time one. */
/* THE SELECTED RANGE, ABOVE THE TABLES AND IMPOSSIBLE TO MISS.
 *
 * Every figure below means something different depending on this, so a reader who scrolled past
 * the chart must still know what they are looking at. A banner rather than a line of prose,
 * because a narrowed table that LOOKS like an all-time table is the worst failure this view has:
 * it would have somebody remove a capability on the strength of one quiet week. */
function insRangeBanner() {
  if (!insSince && !insUntil) {
    return el('div', { class: 'ins-banner all' },
      el('span', { class: 'ins-banner-k', text: 'showing' }),
      el('strong', { text: 'all recorded history' }),
      el('span', { class: 'ins-banner-note', text: 'Drag the slider beneath the chart to narrow it.' }));
  }
  const fmt = (ms) => {
    const d = new Date(ms);
    return d.getDate() + ' ' + ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun',
      'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'][d.getMonth()] + ' ' + d.getFullYear();
  };
  const days = insUntil && insSince
    ? Math.max(1, Math.round((insUntil - insSince) / 86400000)) : null;
  const b = el('div', { class: 'ins-banner' },
    el('span', { class: 'ins-banner-k', text: 'selected range' }),
    el('strong', { text: fmt(insSince || insDoc.timeline.buckets[0]) +
      (insUntil ? ' \u2192 ' + fmt(insUntil) : ' \u2192 now') }));
  if (days) b.appendChild(el('span', { class: 'ins-banner-note', text: days + ' day(s)' }));
  b.appendChild(el('button', {
    type: 'button', class: 'ins-banner-clear', text: 'show all history',
    onclick: () => { insSince = 0; insUntil = 0; loadInsights(); },
  }));
  return b;
}

function insScopeLine() {
  const items = (insDoc.items || []);
  const unused = items.filter((i) => !i.invoked).length;
  const tl = insDoc.timeline;
  let span = 'all the traffic this proxy has kept';
  if (tl && tl.buckets && tl.buckets.length) {
    const a = new Date(tl.buckets[0]), b = new Date(tl.buckets[tl.buckets.length - 1]);
    const fmt = (d) => d.getDate() + ' ' + ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun',
      'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'][d.getMonth()];
    span = tl.buckets.length === 1 ? fmt(a)
      : fmt(a) + ' to ' + fmt(b) + ' (' + tl.buckets.length + ' days)';
  }
  const p = el('p', { class: 'ins-scope' },
    el('strong', { text: items.length + ' capabilities, ' + unused + ' never invoked' }),
    ' — measured over ' + span + '. Every figure below describes this range only.');
  /* Narrowing weakens the verdicts, because the observation floor is computed over the selected
   * window. That is correct — less evidence, weaker claims — but unsaid it reads as the
   * recommendations breaking, so it is said. */
  if (insSince) {
    p.appendChild(el('span', { class: 'ins-scope-note', text: ' A narrower range means less ' +
      'evidence, so fewer items will be recommended outright. That is the floor working, not a ' +
      'fault.' }));
  }
  return p;
}


/* Day buckets up to 60 columns, then rolled wider. 120 days of day-buckets is a column per 3px —
 * a texture, not a chart. Rolled here rather than in the endpoint so the brush can still resolve
 * a selection to exact days. */
function insRollup(tl) {
  const n = tl.buckets.length;
  const width = Math.max(1, Math.ceil(n / INS_MAX_COLS));
  const cols = [];
  for (let i = 0; i < n; i += width) {
    cols.push({ from: tl.buckets[i], fromIdx: i, toIdx: Math.min(n, i + width) - 1 });
  }
  const roll = (arr) => cols.map((c) => {
    let s = 0;
    for (let i = c.fromIdx; i <= c.toIdx; i++) s += arr[i] || 0;
    return s;
  });
  const rows = [];
  for (const name in tl.items) {
    const it = tl.items[name];
    const calls = roll(it.calls);
    const carried = roll(it.carried);
    /* EVERY CARRIED ROW, invoked or not.
     *
     * This filtered to `calls.some(v => v > 0)` — drawn only if it was ever invoked — and that
     * was a tautology once hue carried the verdict: an invoked capability is `keep` by
     * definition, so every drawn row came out grey, every time, whatever the palette said.
     * Reported as "why all come grey".
     *
     * The worse half is which rows it dropped. On the measured store it drew the 15 capabilities
     * the reader USES and hid the 43 they carry on every request and never call — the waste this
     * whole page reports. A row with carriage and no calls is not an empty row; it is the
     * finding. */
    if (!calls.some((v) => v > 0) && !carried.some((v) => v > 0)) continue;
    rows.push({ name, calls, carried, firstUsed: it.first_used_ts || 0,
      used: calls.reduce((x, y) => x + y, 0) });
  }
  /* WASTE FIRST, then the progression.
   *
   * Never-invoked rows sort to the top, heaviest carriage first, because they are the answer to
   * "what am I paying for and not using". Below them the invoked rows keep their first-use order,
   * which is what produces the diagonal that shows a phase change.
   *
   * Ordering by first use alone would bury the waste: a never-invoked capability has no first
   * use, so it would sort to one end as an accident of a missing timestamp rather than as a
   * deliberate statement about what matters. */
  rows.sort((a, b) => {
    if (!a.used !== !b.used) return a.used ? 1 : -1;          // unused rows first
    if (!a.used) {
      return b.carried.reduce((x, y) => x + y, 0) - a.carried.reduce((x, y) => x + y, 0);
    }
    return (a.firstUsed - b.firstUsed) || (b.used - a.used);
  });
  /* Capped — but with a GUARANTEED SHARE OF EACH KIND, which a plain head of the list does not
   * give. Sorting waste first and taking the top 28 produced a chart of nothing but waste: the
   * capabilities actually in use were all below the cut, so the reader lost the baseline that
   * makes the waste legible. "Everything is amber" is as uninformative as "everything is grey".
   *
   * So the cap is split: most of it goes to the worst waste, and a reserved slice keeps the most
   * active in-use rows, which is the comparison the picture is for. Both groups keep their own
   * ordering, and the used rows stay at the bottom so the first-use diagonal still reads. */
  const unused = rows.filter((r) => !r.used);
  const inUse = rows.filter((r) => r.used);
  const keepUsed = Math.min(inUse.length, Math.max(4, Math.round(INS_MAX_ROWS * 0.3)));
  const keepUnused = Math.min(unused.length, INS_MAX_ROWS - keepUsed);
  /* The most ACTIVE in-use rows, not the earliest, since the slice is about showing live traffic
   * next to the dead weight. Re-sorted back to first-use order afterwards for the diagonal. */
  const shownUsed = inUse.slice().sort((a, b) => b.used - a.used).slice(0, keepUsed)
    .sort((a, b) => (a.firstUsed - b.firstUsed) || (b.used - a.used));
  const shown = unused.slice(0, keepUnused).concat(shownUsed);
  const hidden = Math.max(0, rows.length - shown.length);
  return { cols, rows: shown, width, days: n, hidden, total: rows.length,
    shownUnused: keepUnused, totalUnused: unused.length };
}


/* ── the waste chart (Apache ECharts) ──────────────────────────────────────
 *
 * One horizontal bar per removable capability, longest first: what this costs you per request,
 * for nothing.
 *
 * WHY NOT A HEATMAP, which is what this was. A capability-by-day heatmap answers "when did I use
 * this", and that is worth seeing — once there are months of history with real phase changes in
 * them. On the store this actually runs against there are eight days in which nearly every
 * capability was carried every single day, so the heatmap rendered as a wall of one colour and
 * was read exactly that way. Four rounds of colour and geometry fixes did not change that,
 * because the form was answering a question the data could not yet support.
 *
 * The question the page DOES exist to answer has a clean ranking behind it: 55 removable items,
 * $31.70 in total, the worst single one at $7.14. A bar per capability says that at a glance,
 * needs no hover, and puts the thing to act on first.
 *
 * WHY A LIBRARY AT ALL, still. Axis layout, label measurement, value formatting and a range
 * control are the library's job; two earlier hand-built attempts failed on exactly those. The
 * dependency stays, the chart TYPE changes — which is a config change, not a rebuild.
 *
 * HORIZONTAL, because the labels are capability names: `mcp__web-search__google_pse_search` does
 * not fit under a vertical bar at any readable size, and rotating it is not reading it.
 */

const INS_BAR_PX = 22;
const INS_BAR_MIN_H = 220;
/* The most bars drawn. Twenty fits on a screen with room for the labels, and the rows are already
 * sorted worst-first so what is cut is the tail. The footnote says how many are not shown; the
 * tables below keep every one. */
const INS_MAX_BARS = 20;

/* The instance, kept so a re-render resizes rather than leaks: ECharts holds a root and its own
 * listeners, and dropping the DOM node without dispose() keeps both alive. */
let insEChart = null;

function insChartTheme() {
  const cs = getComputedStyle(document.documentElement);
  const v = (n, f) => cs.getPropertyValue(n).trim() || f;
  return {
    /* The VERDICT hues, read from the stylesheet rather than restated here, so a bar and the
     * table pill under it cannot disagree. The cell variants are the softened steps — see the
     * scheme comment in insights.css. */
    vd: {
      remove: v('--ins-cell-remove', '#cc6b60'),
      stale: v('--ins-cell-remove', '#cc6b60'),
      consider: v('--ins-cell-consider', '#f3cd80'),
      keep: v('--ins-cell-keep', '#9aa2ae'),
    },
    carried: v('--ins-carried', '#b0a9dc'),
    ink: v('--fg', '#1b1f1c'),
    muted: v('--fg-muted', '#5c635d'),
    faint: v('--fg-faint', '#8b918b'),
    border: v('--border', '#d8dbd6'),
    surface: v('--bg-raised', '#ffffff'),
    accent: v('--accent', '#2a78d6'),
  };
}

/* insBarRows is the ranked row set, and the ranking is the whole point of the chart.
 *
 * ONLY WHAT CAN BE REMOVED. A built-in gets no bar: its weight is real but removing it breaks the
 * agent, and putting the biggest unactionable item at the top of a chart about what to remove is
 * the one misreading this view must not invite. The tables already filter on scope; the chart has
 * to agree with them.
 *
 * BY MONEY, falling back to tokens. A model with no published rate reports $0, which would sort a
 * genuinely heavy capability to the bottom — so an unpriced row ranks on its per-request weight
 * and is labelled as unpriced rather than as free.
 */
function insBarRows() {
  const items = ((insDoc && insDoc.items) || []).filter(
    (it) => it.scope && it.scope !== 'none' && (it.usd > 0 || it.tokens > 0));
  items.sort((a, b) => (b.usd || 0) - (a.usd || 0) || (b.tokens || 0) - (a.tokens || 0));
  return items;
}

function insChart() {
  if (typeof echarts === 'undefined') {
    return el('p', { class: 'empty', text: 'The charting library did not load, so the picture is ' +
      'unavailable. The tables below are unaffected.' });
  }
  const all = insBarRows();
  if (!all.length) {
    return el('p', { class: 'empty', text: 'Nothing removable has a measured cost in this ' +
      'range, so there is nothing to rank. The tables below still list your inventory.' });
  }
  const rows = all.slice(0, INS_MAX_BARS);

  const host = el('div', { class: 'ins-echart' });
  const h = Math.max(INS_BAR_MIN_H, rows.length * INS_BAR_PX + 70);
  host.style.setProperty('height', h + 'px');

  /* Rendered after the node is in the document, because ECharts measures its container and a
   * detached div has no width. */
  setTimeout(() => insDrawChart(host, rows), 0);

  if (all.length <= rows.length) return host;
  return el('div', {}, host, insBarFootnote(rows.length, all.length));
}

/* insBarGutter is the width the row labels need, in pixels.
 *
 * Measured from the longest label rather than guessed, because the alternative — containLabel —
 * silently truncates when it runs out of room, and a clipped capability name reads as a
 * different capability. Capped so one pathological name cannot eat the plot. */
function insBarGutter(rows) {
  const longest = rows.reduce((m, r) => Math.max(m, insBarLabel(r).length), 0);
  return Math.min(260, Math.max(90, longest * 7.4 + 16));
}

/* insBarLabel is a capability's name at a width the axis gutter can hold.
 *
 * An MCP tool carries its server in its name — `mcp__web-search__google_pse_search` — which is
 * both the longest kind of label here and the most redundant, since the server is repeated on
 * every one of its tools. Rendered as `web-search/google_pse_search`, which is the same two
 * facts in half the width. Everything else keeps whatever insShortName leaves, and a name still
 * too long is truncated from the FRONT with an ellipsis rather than clipped silently: a label
 * missing its own beginning, with no mark to say so, reads as a different tool. */
function insBarLabel(it) {
  let n = insShortName(it.name);
  const mcp = n.match(/^mcp__([^_]+(?:_[^_]+)*?)__(.+)$/);
  if (mcp) n = mcp[1] + '/' + mcp[2];
  return n.length > 32 ? '…' + n.slice(-31) : n;
}

/* insBarFootnote names what the chart is not showing. A chart capped at 20 of 55 reads as the
 * whole inventory unless it says so, and the figure a reader would then quote is wrong. */
function insBarFootnote(shown, total) {
  return el('p', { class: 'hint', text: 'Showing the ' + shown + ' most expensive of ' + total +
    ' removable capabilities. The other ' + (total - shown) + ' cost less each; all of them are ' +
    'in the tables below.' });
}

function insDrawChart(host, rows) {
  if (!host.clientWidth) return;
  const t = insChartTheme();
  if (insEChart) { try { insEChart.dispose(); } catch (_) { /* already gone */ } }
  insEChart = echarts.init(host, null, { renderer: 'svg' });

  /* Longest bar at the TOP, which means last in the array: a category y axis counts upward from
   * the bottom. */
  const ordered = rows.slice().reverse();
  const priced = ordered.some((r) => r.usd > 0);

  insEChart.setOption({
    animation: false,
    textStyle: { fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace', fontSize: 11 },
    /* AN EXPLICIT LEFT GUTTER, not containLabel.
     *
     * containLabel asks ECharts to fit the labels, and when they do not fit it CLIPS them — so
     * `mcp__web-search__google_pse_search` rendered as `ch/google_pse_search`, a label missing
     * its own beginning with nothing to say so. Reserving the width instead means the longest
     * label decides the gutter and every one of them is whole. 7px per character is measured for
     * the 11px monospace this chart uses, with a floor so a chart of short names is not
     * lopsided. */
    grid: { left: insBarGutter(ordered), right: 92, top: 8, bottom: 28, containLabel: false },
    tooltip: {
      trigger: 'item',
      /* Pinned and instant. A tooltip that re-anchors per bar flickers while the pointer
       * travels, which is what a previous version of this chart did. */
      position: (pt, params, dom, rect, size) => [size.viewSize[0] - size.contentSize[0] - 8, 8],
      confine: true,
      transitionDuration: 0,
      formatter: (p) => {
        const r = ordered[p.dataIndex];
        if (!r) return '';
        const money = r.priced ? usd(r.usd) : 'unpriced';
        return '<strong>' + r.name + '</strong><br>' +
          money + ' wasted · ' + (r.tokens || 0).toLocaleString() + ' tok/request<br>' +
          (r.verdict || '') + (r.why ? '<br><span style="opacity:.75">' + r.why + '</span>' : '');
      },
    },
    xAxis: {
      type: 'value',
      /* Money if anything is priced, otherwise per-request weight — never a mix, which would put
       * two units on one axis. */
      name: priced ? 'wasted' : 'tokens per request',
      nameLocation: 'end',
      nameTextStyle: { color: t.faint, fontSize: 10, align: 'right' },
      axisLabel: { color: t.faint, formatter: (v) => (priced ? '$' + v : compact(v)) },
      axisLine: { show: false },
      axisTick: { show: false },
      splitLine: { lineStyle: { color: t.border, opacity: 0.6 } },
    },
    yAxis: { type: 'category',
      /* insBarLabel, not insShortName: an MCP tool is named `mcp__<server>__<tool>`, which is
       * wider than the gutter and was clipped to `__web-search__google_pse_search` — a label
       * missing its own beginning. The full name is in the tooltip and in the tables. */
      data: ordered.map((r) => insBarLabel(r)),
      axisLabel: { color: t.muted },
      axisLine: { lineStyle: { color: t.border } },
      axisTick: { show: false } },
    series: [{
      type: 'bar',
      data: ordered.map((r) => ({
        value: priced ? (r.usd || 0) : (r.tokens || 0),
        /* THE BAR CARRIES ITS VERDICT COLOUR, the same one the table pill uses, so red is
         * removable in both places. */
        itemStyle: { color: t.vd[r.verdict] || t.vd.keep, borderRadius: [0, 3, 3, 0] },
      })),
      barMaxWidth: 16,
      /* The value printed at the end of the bar: the one number the bar encodes, so the chart
       * does not depend on hover to be read. */
      label: { show: true, position: 'right', color: t.muted, fontSize: 10,
        formatter: (p) => (priced ? usd(p.value) : compact(p.value)) },
      emphasis: { focus: 'self', itemStyle: { borderColor: t.ink, borderWidth: 1 } },
    }],
  });

  /* Resize with the panel. ECharts cannot know its container changed. */
  if (window.ResizeObserver && !host.__ro) {
    host.__ro = new ResizeObserver(() => { if (insEChart) insEChart.resize(); });
    host.__ro.observe(host);
  }
}

/* insSelectedCols maps the active window onto column indices, or null when nothing is selected.
 * One place, because the band and the slider restore must agree: if they disagreed, the shading
 * would mark a different range than the handles, which is the confusion this whole change fixes. */
function insSelectedCols(g, since, until) {
  if (!since && !until) return null;
  let a = 0;
  let b = g.cols.length - 1;
  /* COMPARED START-TO-START, which is not a detail. The handler sets `since` to a column's own
   * `from`, so matching on the column's END (`buckets[toIdx] >= since`) selects the column BEFORE
   * the intended one whenever a column is a single day, because then its end and the next one's
   * start are the same instant. Observed: every cell fell outside the selection and the whole
   * chart greyed out while the banner named a correct two-day range. */
  for (let i = 0; i < g.cols.length; i++) {
    if (since && g.cols[i].from >= since) { a = i; break; }
  }
  for (let i = g.cols.length - 1; i >= 0; i--) {
    if (until && g.cols[i].from <= until) { b = i; break; }
  }
  return [a, Math.max(a, b)];
}

/* insColEnd is the timestamp one bucket PAST the last column, which is the final edge: the axis
 * needs n+1 boundaries for n columns, and the last one is where the last column stops. */
/* ── the project picker ────────────────────────────────────────────────────
 *
 * A `claude` agent is started in a folder, and that folder is what the user means by a project.
 * Several of them route through one proxy, so every figure on this page is a blend across them
 * until one is chosen.
 *
 * ALL PROJECTS IS THE DEFAULT, deliberately: it is what this view showed before the dimension
 * existed, and it carries the thickest evidence. A single project is the narrower truth, and the
 * banner and the basis both say so, because per-project evidence is thin — one to five sessions
 * against a floor of five sessions and seven days — so most verdicts there will honestly read
 * "not enough evidence yet" rather than "remove".
 */
function insProjectPicker() {
  /* ONE FOLDER IS STILL A CHOICE, because the UNATTRIBUTED requests are the other side of it.
   *
   * This stood down whenever fewer than two folders were recorded, reasoning that a one-entry
   * dropdown is a control whose only outcome is no change. That was wrong and it shipped: on the
   * real store there was one folder and 2,985 unattributed rows — every request written before
   * the column existed — so there were two scopes worth comparing and no control to compare them
   * with. It also printed "Every request recorded so far came from X", which was false for 2,985
   * of 2,987 rows.
   *
   * The only honest stand-down is when nothing has been recorded at all. */
  if (!insProjects || !insProjects.length) return el('span', {});

  const sel = el('select', {
    'data-testid': 'insights-project',
    'aria-label': 'Project folder',
    onchange: (ev) => {
      insProject = ev.currentTarget.value;
      /* Re-QUERY rather than re-render: scoping is a server-side decision, and insRender would
       * also collapse every open fold. loadInsights ends in a render of its own. */
      loadInsights();
    },
  });
  /* The pooled view first and selected by default. */
  const opts = [['', 'all projects']];
  for (const p of insProjects) opts.push([p, insShortProject(p)]);
  /* PROJECTS ONLY. No `unattributed` entry: the rows written before this column existed carry no
   * tool inventory to shape a report from, so selecting them produced a document with zero items
   * under a scoped heading — an empty page that reads as a broken one. Those rows are still in
   * `all projects`, which is where a figure nobody can attribute belongs. */
  for (const [v, label] of opts) {
    const o = el('option', { value: v, title: v || 'every project pooled together' }, label);
    if (v === insProject) o.setAttribute('selected', 'selected');
    sel.appendChild(o);
  }
  /* The hint names the EVIDENCE each scope rests on, which is the thing a reader cannot see for
   * themselves. `unattributed` needs its own wording: those requests predate the project column,
   * so they cannot be attributed later and the option is not going to empty itself. */
  const note = insProject
    ? 'Verdicts below use this project\u2019s sessions only.'
    : 'Every project pooled, which is the strongest evidence.';
  return el('div', { class: 'ins-proj' },
    el('label', {}, 'Project ', sel),
    el('span', { class: 'hint', text: note }));
}

/* insShortProject labels an option with the folder's own name, keeping the full path as the
 * title: `/Users/vitalyz/OpenSource/context-guru` does not fit in a select, and the last segment
 * is what the reader calls the project. The parent is kept too, because `notes` and
 * `notes/server` are different projects whose last segments alone would not distinguish them. */
function insShortProject(path) {
  const parts = path.split('/').filter(Boolean);
  if (parts.length <= 2) return path;
  return parts.slice(-2).join('/');
}

/* insVerdictOf is the chart's join back to the advice. The timeline names a capability; the
 * document's items[] carries the verdict for it. Built once per render rather than scanned per
 * cell, which would be O(rows x cols x items). */
let insVerdictMap = null;
function insVerdictOf(name) {
  if (!insVerdictMap) {
    insVerdictMap = {};
    for (const it of (insDoc && insDoc.items) || []) insVerdictMap[it.name] = it.verdict;
  }
  return insVerdictMap[name] || 'keep';
}


/* insLegendFor names only the states this chart actually drew. */
function insLegendFor(byVd, idle, idleOut, usedOut) {
  const out = [];
  if (byVd.remove.length || byVd.stale.length) out.push('removable waste');
  if (byVd.consider.length) out.push('below the evidence floor');
  if (byVd.keep.length) out.push('in use / built in');
  if (idle.length) out.push('carried, never used');
  if (usedOut.length || idleOut.length) out.push('outside the range');
  return out;
}

function insColEnd(g, tl) {
  const last = g.cols[g.cols.length - 1];
  const dayMs = 24 * 3600 * 1000;
  return tl.buckets[last.toIdx] + dayMs;
}

/* insEdgeLabel thins the edge labels so they do not collide, keeping the first and last. A value
 * axis has no `interval` for labels the way a category axis does, so the thinning is ours. */
function insEdgeLabel(edges, v, n) {
  const i = Math.round(v);
  if (i < 0 || i >= edges.length) return '';
  const every = Math.max(1, Math.ceil(n / 9));
  if (i !== 0 && i !== edges.length - 1 && i % every !== 0) return '';
  return edges[i];
}

function insColLabel(ms, width) {
  const d = new Date(ms);
  const mon = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun',
    'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'][d.getMonth()];
  return width > 1 ? d.getDate() + ' ' + mon : d.getDate() + ' ' + mon;
}
