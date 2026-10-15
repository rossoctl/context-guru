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
    el('h2', { text: 'What you carry, and when you actually used it' }),
    /* Rewritten with the form, for the third time: it has described a magnitude ramp, a day
     * grid, and a cost ranking, each after that thing stopped existing. A hint explaining an
     * older version of the picture is worse than none, because the reader trusts it. */
    el('p', { class: 'hint', text: 'One bar per thing you can switch off, divided into periods. '
      + 'Red means you carried it through that period and never called it; green means you used '
      + 'it. A bar that is all red is pure waste. A bar that turns red partway is something you '
      + 'stopped using — which is usually the easiest thing to remove. A plugin appears as one '
      + 'bar because the plugin is the only unit you can disable; its individual skills are in '
      + 'the tables below. Bars marked "(built in)" are Claude Code\u2019s own tools \u2014 shown '
      + 'in a paler green so you can see what real usage looks like, but they cannot be removed '
      + 'and so never appear in the tables.' }));
  chartPanel.appendChild(insProjectPicker());
  chartPanel.appendChild(insRangePicker());
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


/* ── the usage chart (Apache ECharts) ──────────────────────────────────────
 *
 * One stacked bar per removable capability. Each bar is divided into time buckets, and each
 * bucket is red when the capability was carried but never invoked in that period, green when it
 * was invoked. So a bar that is entirely red is pure waste, and a bar that is green early and red
 * later is something you stopped using — which is the pattern worth finding.
 *
 * WHY THIS FORM, after two others. A heatmap carried the same information and was unreadable:
 * 160 small cells, with no legible way to tell a used day from a carried one. A ranked bar chart
 * of cost was readable and answered a narrower question — it said what was expensive, never when
 * it stopped being used. The decision this page supports needs both, and "did I stop using it"
 * is a shape over time rather than a number.
 *
 * GROUPED BY WHAT CAN ACTUALLY BE REMOVED. A plugin's skills collapse into one bar for the
 * plugin, because `skillOverrides` has no effect on them — verified on real traffic — so the
 * plugin is the only unit that can be switched off. Measured: 55 removable items become 28 bars,
 * wide enough for horizontal labels with no scrolling. Offering 15 separate bars for one
 * plugin's skills would offer 15 removals that do not exist.
 *
 * BUILT-INS NEVER APPEAR. Their weight is real, but removing one breaks the agent, and the
 * biggest unactionable item at the top of a chart about what to remove is the one misreading this
 * view must not invite.
 *
 * BUCKETS FOLLOW THE WINDOW, so a month is never 30 slivers and a week is never one undivided
 * block — the density failure that sank the heatmap, parameterised away.
 */

const INS_BAR_PX = 26;
const INS_USAGE_MIN_H = 260;

/* The instance, kept so a re-render resizes rather than leaks: ECharts holds a root and its own
 * listeners, and dropping the DOM node without dispose() keeps both alive. */
let insEChart = null;

/* The window, in days back from now, or 0 for everything the store holds. Coarse on purpose: the
 * question is "what pattern does this capability show over time", and an exact date range is both
 * more precision than that needs and more clicks to express. */
let insWindowDays = 0;

function insChartTheme() {
  const cs = getComputedStyle(document.documentElement);
  const v = (n, f) => cs.getPropertyValue(n).trim() || f;
  return {
    wasted: v('--ins-wasted', '#d08a80'),
    used: v('--ins-used', '#66b366'),
    baseline: v('--ins-baseline', '#bcd9bc'),
    ink: v('--fg', '#1b1f1c'),
    muted: v('--fg-muted', '#5c635d'),
    faint: v('--fg-faint', '#8b918b'),
    border: v('--border', '#d8dbd6'),
    surface: v('--bg-raised', '#ffffff'),
    accent: v('--accent', '#2a78d6'),
  };
}

/* insIsBaseline marks Claude Code's own built-ins.
 *
 * They are charted, because without them this chart is solid red on real data — no removable
 * capability here has ever been invoked, so the green that makes the form readable comes entirely
 * from the built-ins. But they are never actionable: the weight is real and removing one takes
 * away a capability the model is expected to have. So they sit below a divider, in their own
 * desaturated colour, and nothing offers to remove them. */
function insIsBaseline(it) { return !it.scope || it.scope === 'none'; }

/* insRemovalGroup is the key a capability is charted under: the thing that would actually be
 * switched off. A plugin for a plugin's skill, a server for an MCP tool, the item itself for
 * anything individually removable. */
function insRemovalGroup(it) {
  if (it.plugin) return { key: 'plugin:' + it.plugin, label: it.plugin, kind: 'plugin' };
  if (it.server) return { key: 'server:' + it.server, label: it.server, kind: 'server' };
  return { key: 'item:' + it.name, label: insShortName(it.name), kind: 'item' };
}

/* insBucketDays is how many days one segment covers, chosen from the window so a bar always has
 * roughly four to eight segments. A segment thinner than a few pixels is the heatmap's failure
 * mode; a bar with one segment shows no pattern at all. */
function insBucketDays(days) {
  if (days <= 10) return 1;        // a week or so: daily
  if (days <= 35) return 7;        // a month: weekly
  if (days <= 80) return 14;       // a quarter: fortnightly
  return 30;                       // longer: monthly
}

/* insUsageBars turns the per-day timeline into one row per removal group, each row holding a
 * used/wasted pair per bucket.
 *
 * A GROUP IS USED IN A BUCKET IF ANY OF ITS MEMBERS WAS. That is the honest reading for a bar
 * whose unit is the removal: if one skill of a plugin was invoked that week, switching the plugin
 * off that week would have cost you something. Summing invocations instead would let a busy
 * member hide a dead one, which is the opposite of what the chart is for.
 */
function insUsageBars(tl) {
  const byName = {};
  for (const it of (insDoc && insDoc.items) || []) byName[it.name] = it;

  /* ONE SEGMENT PER DAY, not per bucket.
   *
   * Each day is a thin line, and a bar is those lines stacked — so the bar's own surface carries
   * the history and no aggregation hides a day. Bucketing was there to keep segments thick
   * enough to aim at, which mattered when the tooltip followed the segment; it does not now,
   * because the tooltip is pinned and names the date it is describing. */
  const nDays = tl.buckets.length;
  const width = 1;
  const cols = [];
  for (let i = 0; i < nDays; i++) {
    cols.push({ from: tl.buckets[i], fromIdx: i, toIdx: i });
  }
  /* EMPTY PERIODS ARE DROPPED, not drawn.
   *
   * A period in which NOTHING was carried is a period you did not work — a weekend, a holiday —
   * and drawing it put a white band straight through every bar on the chart, which reads as the
   * chart being broken rather than as time off. Measured: two idle days split all 28 bars in
   * half. The axis is "periods in which you worked", which is the honest subject for a question
   * about usage patterns, and the tooltip still names the real dates. */
  const anyTraffic = cols.map((c) => {
    for (const nm in tl.items) {
      const srs = tl.items[nm];
      for (let i = c.fromIdx; i <= c.toIdx; i++) {
        if ((srs.carried[i] || 0) > 0 || (srs.calls[i] || 0) > 0) return true;
      }
    }
    return false;
  });
  const kept = cols.filter((c, i) => anyTraffic[i]);
  cols.length = 0;
  kept.forEach((c) => cols.push(c));

  const groups = {};
  for (const name in tl.items) {
    const it = byName[name];
    if (!it) continue;
    const base = insIsBaseline(it);
    const g = base
      ? { key: 'base:' + it.name, label: insShortName(it.name), kind: 'builtin' }
      : insRemovalGroup(it);
    const row = groups[g.key] || (groups[g.key] = {
      key: g.key, label: g.label, kind: g.kind, baseline: base, members: [], usd: 0, tokens: 0,
      used: cols.map(() => 0), carried: cols.map(() => 0),
    });
    row.members.push(it.name);
    row.usd += it.usd || 0;
    row.tokens += it.tokens || 0;
    const s = tl.items[name];
    cols.forEach((c, ci) => {
      for (let i = c.fromIdx; i <= c.toIdx; i++) {
        if ((s.calls[i] || 0) > 0) row.used[ci] = 1;
        /* SUMMED, not flagged. This set `carried[ci] = 1`, which threw the amount away: a day
         * with 860 requests rendered exactly like one with 206, and every day of a bar looked
         * identical however much it had actually cost. The count is what the shade needs. */
        row.carried[ci] += s.carried[i] || 0;
      }
    });
  }

  /* Worst first: the bar with the most wasted buckets, then by money. A reader scans from the top,
   * so the top is where the answer goes. */
  const all = Object.keys(groups).map((k) => groups[k]);
  /* The busiest day anywhere on the chart, so every bar is shaded on one scale and two bars can
   * be compared. Per-bar scaling would make a cheap capability's worst day look as dark as an
   * expensive one's. */
  const peak = all.reduce((m, r) => Math.max(m, ...r.carried), 0);
  all.forEach((r) => { r.peak = peak; });
  const wastedCount = (r) => r.carried.reduce((n, c, i) => n + (c && !r.used[i] ? 1 : 0), 0);
  const rank = (a, b) => wastedCount(b) - wastedCount(a) || (b.usd || 0) - (a.usd || 0);
  /* Two blocks, not one ordering: everything you can act on, worst first, then the baseline
   * underneath. Interleaving them by waste would put an unremovable built-in at the top of a
   * chart about what to remove. The baseline keeps only the most-used few — it is there for
   * contrast, not for completeness, and twenty built-ins would crowd out the subject. */
  const act = all.filter((r) => !r.baseline).sort(rank);
  const base = all.filter((r) => r.baseline)
    .sort((a, b) => b.used.reduce((n, u) => n + u, 0) - a.used.reduce((n, u) => n + u, 0))
    .slice(0, 5);
  return { rows: act.concat(base), cols, width, actionable: act.length, baseline: base.length };
}

/* The lightest a carried day may be drawn.
 *
 * Without a floor a cheap day fades into the surface and reads as a day the capability was not
 * carried at all — which is the one distinction this chart turns on.
 *
 * 0.62, raised from 0.35 after looking at it: the lower floor spread the shades nicely and left
 * the whole chart washed out, so red stopped reading as red. A narrower band costs some
 * resolution between a quiet day and a middling one, and buys back the thing the colour is for —
 * telling waste from use at a glance. The exact figure for any day is in the tooltip. */
const INS_SHADE_FLOOR = 0.62;

/* insDayShade turns a day's carriage count into an opacity within that day's own colour.
 *
 * SCALED AGAINST THE BUSIEST DAY ON THE CHART, not linearly from zero. Measured on the real
 * store: per-day counts run from 1 to 601 with a median of 358, so a linear ramp from zero would
 * put nearly every day in the top two shades and show nothing at all. Dividing by the peak
 * spreads the days that actually occur across the whole range.
 *
 * The square root rather than the raw ratio, because the distribution is top-heavy: without it
 * the many mid-volume days bunch together and only the single busiest looks different. */
function insDayShade(count, peak) {
  if (count <= 0) return 0;
  const frac = Math.sqrt(count / Math.max(1, peak));
  return INS_SHADE_FLOOR + (1 - INS_SHADE_FLOOR) * Math.min(1, frac);
}

/* insRangePicker chooses the window in weeks and months rather than by date. */
function insRangePicker() {
  const opts = [[0, 'all history'], [7, 'last week'], [14, 'last 2 weeks'],
    [21, 'last 3 weeks'], [30, 'last month'], [90, 'last 3 months']];
  const sel = el('select', {
    'data-testid': 'insights-window',
    'aria-label': 'Analysis window',
    onchange: (ev) => {
      insWindowDays = Number(ev.currentTarget.value) || 0;
      /* Re-query rather than re-render: the verdicts and the evidence floor are computed
       * server-side over the window, so narrowing locally would leave the advice describing a
       * period the rows no longer cover. */
      const now = Date.now();
      insSince = insWindowDays ? now - insWindowDays * 86400000 : 0;
      insUntil = 0;
      loadInsights();
    },
  });
  for (const [v, label] of opts) {
    const o = el('option', { value: String(v) }, label);
    if (v === insWindowDays) o.setAttribute('selected', 'selected');
    sel.appendChild(o);
  }
  return el('div', { class: 'ins-proj' },
    el('label', {}, 'Window ', sel),
    el('span', { class: 'hint', text: insWindowDays
      ? 'Buckets are sized to the window, so each bar stays readable.'
      : 'Everything the proxy has recorded.' }));
}

function insChart() {
  if (typeof echarts === 'undefined') {
    return el('p', { class: 'empty', text: 'The charting library did not load, so the picture is ' +
      'unavailable. The tables below are unaffected.' });
  }
  const tl = insDoc && insDoc.timeline;
  if (!tl || !tl.buckets.length) return null;
  const g = insUsageBars(tl);
  if (!g.rows.length) {
    return el('p', { class: 'empty', text: 'Nothing removable was carried in this window, so ' +
      'there is no usage pattern to show. The tables below still list your inventory.' });
  }

  const host = el('div', { class: 'ins-echart' });
  /* A fixed plot height: the bars are vertical now, so the row count widens the chart rather
   * than lengthening it, and the page should not grow with the number of tools. */
  const h = 460;
  host.style.setProperty('height', h + 'px');
  setTimeout(() => insDrawChart(host, g), 0);
  return host;
}

function insDrawChart(host, g) {
  if (!host.clientWidth) return;
  const t = insChartTheme();
  if (insEChart) { try { insEChart.dispose(); } catch (_) { /* already gone */ } }
  insEChart = echarts.init(host, null, { renderer: 'svg' });

  /* Worst at the LEFT, which is first in the array: a category x axis reads left to right, so
   * unlike the horizontal form this needs no reversing. */
  const rows = g.rows;
  const nCols = g.cols.length;

  /* One series per bucket, stacked: that is how a bar gets divided along time. Each series holds
   * one segment for every bar, coloured per bar by whether THAT capability was used in THAT
   * period — so the colour varies within a series, which is why it is set per data point. */
  const series = g.cols.map((c, ci) => ({
    type: 'bar',
    stack: 'time',
    name: insColLabel(c.from, g.width),
    /* ADJACENT BARS. `barCategoryGap: 0` removes the space between capabilities so the chart
     * reads as one continuous field, and barWidth '100%' lets each bar fill its slot. A gap
     * between bars is space spent on nothing when there are 33 of them. */
    barCategoryGap: 0,
    barGap: '-100%',
    barWidth: '100%',
    /* focus 'self': only the hovered SEGMENT highlights, never its whole series. The default
     * is 'series', which would light up that period across all 28 bars at once — the
     * whole-grid flash a previous version of this chart was reported for. */
    /* NO FOCUS, AND NO BLUR. `focus: 'self'` was added to stop a hover highlighting a whole
     * series, and it caused something worse: focus works by BLURRING everything that is not the
     * hovered item, so pointing anywhere — an empty cell especially — faded the entire chart to
     * near-invisible, and moving the pointer in and out made it flash. Reported as "when i move
     * mouse on empty cell all chart dissapear".
     *
     * `focus: 'none'` is the explicit default: nothing is blurred, nothing is dimmed, and the
     * only feedback is the hovered segment's own outline. The tooltip, which is pinned, says
     * what is under the pointer — that is the feedback a reader needs, and it costs the rest of
     * the chart nothing. */
    emphasis: { disabled: true },
    blur: { itemStyle: { opacity: 1 } },
    /* EVERY PERIOD GETS A SEGMENT, which is what lets one bar mix red and green.
     *
     * This drew a segment only where `carried` was set, and that produced two wrong pictures at
     * once: a built-in's bar was pure green with holes in it — floating blocks with gaps beneath
     * — because the days it was not declared vanished rather than reading as unused; and no bar
     * ever mixed the two colours, which is the one thing the form exists to show. A tool used in
     * some periods and not others is the pattern worth finding, so every period is drawn and the
     * COLOUR carries the state.
     *
     * A period the capability was not carried at all is drawn in the surface colour: the bar
     * keeps its full height so periods line up across every column, and an absent period reads
     * as a gap rather than as waste it never cost you. */
    data: rows.map((r) => ({
      value: 1,
      itemStyle: {
        color: !r.carried[ci] ? t.surface
          : r.baseline ? (r.used[ci] ? t.baseline : t.wasted)
            : (r.used[ci] ? t.used : t.wasted),
        /* The SHADE is how much that day cost; the HUE is still whether it was used. So a deep
         * red is an expensive wasted day and a pale red a cheap one, and the two questions stay
         * on separate channels. */
        opacity: r.carried[ci] ? insDayShade(r.carried[ci], r.peak) : 1,
        /* NO border. At one day per segment a 1px rule is as wide as the line it separates, so
         * it ate the colour it was meant to delimit — the bar became mostly surface. A run of
         * same-coloured days reading as one block is correct here: that IS the finding. */
        borderWidth: 0,
      },
    })),
  }));

  insEChart.setOption({
    animation: false,
    textStyle: { fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace', fontSize: 11 },
    /* The bottom gutter holds the rotated labels; the left holds nothing but a little air. */
    /* The left gutter holds the date labels now, the bottom holds the rotated capability names. */
    grid: { left: 62, right: 24, top: 12, bottom: insUsageGutter(rows), containLabel: false },
    tooltip: {
      trigger: 'item',
      /* Pinned and instant. A tooltip that re-anchors per segment flickers while the pointer
       * travels, which an earlier version of this chart did. */
      position: (pt, params, dom, rect, size) => [size.viewSize[0] - size.contentSize[0] - 8, 8],
      confine: true,
      transitionDuration: 0,
      formatter: (p) => {
        const r = rows[p.dataIndex];
        if (!r) return '';
        const ci = p.seriesIndex;
        const when = insColLabel(g.cols[ci].from, g.width) +
          (g.width > 1 ? ' + ' + (g.width - 1) + ' day(s)' : '');
        const state = !r.carried[ci] ? 'not carried'
          : (r.used[ci] ? 'invoked — earning its weight' : 'carried, never invoked');
        /* The exact figure for THAT day, because the shade is a comparison and this is the
         * number. Carriage count times the per-request weight is what that day actually cost. */
        const dayTok = r.carried[ci] * (r.tokens || 0);
        const dayLine = r.carried[ci]
          ? '<br>' + r.carried[ci].toLocaleString() + ' request(s) carried it that day · ' +
            dayTok.toLocaleString() + ' tok'
          : '';
        const inside = r.members.length > 1
          ? '<br><span style="opacity:.75">' + r.members.length + ' items inside; see the ' +
            'tables below</span>'
          : '';
        return '<strong>' + r.label + '</strong><br>' + when + ': ' + state + dayLine + '<br>' +
          (r.usd ? usd(r.usd) + ' wasted · ' : '') +
          (r.tokens || 0).toLocaleString() + ' tok/request' + inside;
      },
    },
    legend: {
      bottom: 4, left: 'center', itemWidth: 11, itemHeight: 11, itemGap: 16,
      icon: 'roundRect', selectedMode: false,
      textStyle: { color: t.muted, fontSize: 10 },
      /* The legend names the two STATES, not the buckets: a reader needs to know what red and
       * green mean, and twenty-eight date entries would be noise. Hand-built because the series
       * are the buckets. */
      data: [{ name: 'carried, never invoked', itemStyle: { color: t.wasted } },
        { name: 'invoked in that period', itemStyle: { color: t.used } },
        { name: "built in — can't be removed", itemStyle: { color: t.baseline } }],
    },
    /* VERTICAL BARS: tools along X, time stacking upward on Y — one column per capability, each
     * divided into periods from oldest at the bottom to newest at the top.
     *
     * Labels rotate 45°, because a capability name is far wider than its column: at 28 columns
     * across a page each is roughly 40px, and `claude-md-management` is not going to fit
     * horizontally. Rotated beats truncated — a name missing its end reads as a different tool. */
    xAxis: { type: 'category',
      /* A BASELINE BAR SAYS SO IN ITS OWN LABEL.
       *
       * `Skill` is one of Claude Code's own built-in TOOLS — the one the agent calls to invoke a
       * skill — not a skill you installed. It is in the chart because the built-ins supply the
       * only green on this store, and absent from the tables because the tables list what can be
       * removed. Both are right, and the reader still had to ask what it was: "What is Skill
       * that appears in my Chart i don't see such skill bellow in detailed view".
       *
       * So the label carries the distinction rather than relying on a colour the eye has to
       * decode. The suffix is deliberately plain words, not a symbol: a glyph needs its own
       * legend and this needs none. */
      data: rows.map((r) => (r.baseline ? r.label + ' (built in)' : r.label)),
      axisLabel: { color: t.muted, rotate: 45, interval: 0, fontSize: 10 },
      axisLine: { lineStyle: { color: t.border } },
      axisTick: { show: false } },
    /* THE TIME AXIS, LABELLED WITH REAL DATES.
     *
     * It was hidden, which left the chart's whole vertical dimension unexplained — a reader could
     * see that a bar changed colour partway up but not WHEN, which is the question the form
     * exists to answer.
     *
     * Each stacked segment is exactly one unit tall, so value n is day n: the formatter maps the
     * axis position back to the bucket's own date. Ticks every `step` days rather than on all of
     * them, because at one day per segment the labels would overlap — the axis shows enough
     * scale to read the bar against, and the pinned tooltip names the exact day under the
     * pointer. */
    yAxis: {
      type: 'value', min: 0, max: nCols, interval: insDateTickStep(nCols),
      axisLabel: { color: t.faint, fontSize: 10,
        formatter: (v) => {
          const i = Math.round(v);
          /* The axis counts segment EDGES, so edge i is the start of day i; the last edge is one
           * day past the end and has no bucket of its own. */
          if (i < 0 || i >= g.cols.length) return '';
          return insColLabel(g.cols[i].from, 1);
        } },
      axisLine: { lineStyle: { color: t.border } },
      axisTick: { show: false },
      splitLine: { show: false },
    },
    series,
  });

  if (window.ResizeObserver && !host.__ro) {
    host.__ro = new ResizeObserver(() => { if (insEChart) insEChart.resize(); });
    host.__ro.observe(host);
  }
}

/* insUsageGutter reserves the width the row labels need, measured from the longest.
 *
 * Explicitly, not via containLabel, which CLIPS when it runs out of room — a capability name
 * missing its own beginning reads as a different capability. 7.4px per character is measured for
 * the 11px monospace this chart uses. */
/* insDateTickStep keeps the date labels from colliding: roughly eight of them, whatever the
 * window holds, so a week shows every day and a quarter shows every fortnight. */
function insDateTickStep(nDays) {
  return Math.max(1, Math.ceil(nDays / 8));
}

function insUsageGutter(rows) {
  const longest = rows.reduce((m, r) => Math.max(m, (r.label || '').length), 0);
  /* A label rotated 45° is about 0.7 of its length tall. Reserved explicitly rather than via
   * containLabel, which CLIPS when short of room — and a capability name missing its own end
   * reads as a different capability. The legend sits below this, hence the extra 34. */
  return Math.min(220, Math.max(70, Math.round(longest * 7.4 * 0.7) + 34));
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
