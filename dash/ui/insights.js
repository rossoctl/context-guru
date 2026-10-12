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
let insSessions = 0;

const INS_SCOPE_ITEM = 'item';
const INS_WINDOWS = [0, 1, 3, 5, 10, 25];

/* Only a `scope: "item"` row can be removed on its own, and `keep` is never on offer. */
function insSelectable(it) { return it.verdict !== 'keep' && it.scope === INS_SCOPE_ITEM; }

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
  if (opts.sessions) head.push(el('th', { text: 'per session' }));
  head.push(el('th', { class: 'num', text: 'tok / req' }),
    el('th', { class: 'num', text: 'billed reads' }),
    el('th', { class: 'num', text: 'cost' }));

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
    cells.push(el('td', { class: 'num', text: it.tokens ? it.tokens.toLocaleString() : '—' }));
    cells.push(el('td', { class: 'num', text: it.reads ? it.reads.toLocaleString() : '—' }));
    cells.push(el('td', {
      class: 'num',
      text: it.invoked ? '—' : (it.priced ? usd(it.usd) : 'unpriced'),
    }));
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
  head.appendChild(el('span', { class: 'ins-cost', text: opts.cost }));
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
  const removeSet = (insDoc.items || []).filter((i) => i.verdict === 'remove');
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
    const anyRemove = insDoc.items.some((i) => i.verdict === 'remove');
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
      ' over this window: ' + k.map(insShortName).sort().join(', ')));
  }
  for (const s of Object.keys(plan.servers).sort()) {
    const k = plan.servers[s];
    list.appendChild(el('li', {}, el('strong', { text: 'MCP server ' + s }),
      ' — ' + k.length + (k.length === 1 ? ' tool' : ' tools') + ', ' + usd(sum(k)) +
      ' over this window: ' + k.map(insShortName).sort().join(', ')));
  }
  if (plan.deny.length) {
    list.appendChild(el('li', {}, el('strong', { text: 'tools denied' }),
      ' — ' + plan.deny.length + ', ' + usd(sum(plan.deny)) + ' over this window: ' +
      plan.deny.slice().sort().join(', ')));
  }
  if (plan.skills.length) {
    list.appendChild(el('li', {}, el('strong', { text: 'skills switched off' }),
      ' — ' + plan.skills.length + ', ' + usd(sum(plan.skills)) + ' over this window: ' +
      plan.skills.slice().sort().join(', ')));
  }
  box.appendChild(list);

  box.appendChild(el('div', { class: 'ins-total' }, el('strong', { text: usd(plan.usd) }),
    ' is what these ' + plan.count + (plan.count === 1 ? ' item' : ' items') +
    ' cost over the measured window, carried on every request and never invoked. That is a ' +
    'measurement rather than a forecast: it already happened, so the same traffic would cost ' +
    'that much less.'));

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

function insRender() {
  const root = document.getElementById('insights-body');
  if (!root || !insDoc) return;
  clear(root);
  /* The registry describes the DOM that is about to be discarded. */
  insBoxes = [];

  /* The window strip. Unlike the static report's chips, these RE-QUERY — a served page can, and
   * that is the whole reason this surface is worth building. */
  const win = el('div', { class: 'ins-win' },
    el('span', { class: 'ins-win-lbl', text: 'sessions analysed' }));
  for (const n of INS_WINDOWS) {
    const b = el('button', {
      type: 'button',
      class: 'chip' + (insSessions === n ? ' on' : ''),
      text: n === 0 ? 'all' : 'last ' + n,
      onclick: () => { if (insSessions !== n) { insSessions = n; loadInsights(); } },
    });
    win.appendChild(b);
  }
  root.appendChild(win);

  const items = insDoc.items || [];
  const unremovable = items.filter((i) => i.scope === 'none');
  const removeSet = items.filter((i) => i.verdict === 'remove');

  /* The headline, in the page's own tile vocabulary. */
  const tiles = el('div', { class: 'grid' },
    el('div', { class: 'tile good' },
      el('div', { class: 'k', text: 'recoverable' }),
      el('div', { class: 'v', text: usd(insDoc.removable_usd || 0) }),
      el('div', { class: 's', text: items.filter((i) => i.scope && i.scope !== 'none').length +
        ' items you can remove · ' + (insDoc.removable_tokens || 0).toLocaleString() + ' tok/req' })),
    el('div', { class: 'tile' },
      el('div', { class: 'k', text: 'owners' }),
      el('div', { class: 'v', text: String((insDoc.owners || []).length) }),
      el('div', { class: 's', text: (insDoc.owners || []).filter((o) => o.kind === 'plugin').length +
        ' plugin(s), ' + (insDoc.owners || []).filter((o) => o.kind === 'server').length + ' MCP server(s)' })),
    el('div', { class: 'tile' },
      el('div', { class: 'k', text: 'recommended' }),
      el('div', { class: 'v', text: String(removeSet.length) }),
      el('div', { class: 's', text: removeSet.length
        ? 'past the observation floor'
        : 'nothing past the floor yet — the list below is still complete' })));
  if (unremovable.length) {
    tiles.appendChild(el('div', { class: 'tile bad' },
      el('div', { class: 'k', text: 'built-ins (keep)' }),
      el('div', { class: 'v', text: usd(unremovable.reduce((a, i) => a + (i.usd || 0), 0)) }),
      el('div', { class: 's', text: unremovable.length + ' items · not available to you' })));
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
    root.appendChild(el('h3', { text: 'Plugins' }));
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
    root.appendChild(el('h3', { text: 'MCP servers' }));
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
    root.appendChild(el('h3', { text: 'Removable individually' }));
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
    root.appendChild(el('h3', { text: 'Claude Code built-ins' }));
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
  root.appendChild(el('h3', { text: 'What you decided to remove' }));
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
      (se.usd ? ', ' + usd(se.usd) + ' over this window' : '') +
      (se.command ? '. Remove it with: ' + se.command : '.') }));
  }
}

async function loadInsights() {
  const root = document.getElementById('insights-body');
  if (root && !insDoc) root.appendChild(el('p', { class: 'empty', text: 'Loading…' }));
  try {
    insDoc = await api('insights', insSessions ? { sessions: insSessions } : undefined);
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
