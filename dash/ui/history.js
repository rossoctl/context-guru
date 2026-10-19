/* The History tab: what this machine's own past says about the capabilities installed on it.
 *
 * A DIFFERENT KIND OF CLAIM FROM EVERY OTHER TAB, which is the whole reason it is its own view
 * rather than more columns on Cleanup. Every other figure in this dashboard is "this cost you X
 * and you never used it", and that needs the proxy to have watched the traffic. This one is "you
 * used this / you never used this", which Claude Code's own session transcripts answer alone — so
 * it works on the day the proxy is installed, across months it never saw.
 *
 * WHAT IT REFUSES TO SAY is as much the point as what it says, and the refusal has to be visible
 * on screen rather than only in the JSON. A transcript records what a request spent in TOTAL and
 * never which tools it declared, so the cost of carrying an unused skill is not recoverable from
 * it. This tab therefore names candidates and prices nothing. An invented figure here would be
 * worse than an absent one: it would cover a longer span than the measured figures on Cleanup and
 * would therefore look like better evidence for the same kind of claim. */

document.head.appendChild(el('link', { rel: 'stylesheet', href: 'insights.css' }));

/* Behaviour group, right after Cleanup. Cleanup answers "what is this costing me"; this answers
 * "what have I ever actually used" — the question a reader asks first on install day, when
 * Cleanup has no history to reason from yet. */
const historyView = mountTab({
  group: 'behaviour', after: 'insights', view: 'history', label: 'History',
});
historyView.appendChild(el('div', { id: 'history-body' }));

/* The last document fetched, so a tab switch re-renders without re-querying. */
let histDoc = null;
/* Which half of the inventory is on screen: 'unused' or 'used'. Unused first, because that is
 * the finding — a list of what you have been paying to carry and have never reached for. */
let histTab = 'unused';
/* The folder being reported on, or '' for every project pooled. Scopes the USED list only — see
 * the two-scope note in histRender. */
let histProject = '';
/* The folders on record, for the picker. Fetched once. */
let histProjects = null;

/* A day, in the reader's own locale. Dates rather than counts wherever one will fit: "used once
 * in March" and "used daily since March" are different facts and a bare total cannot tell them
 * apart, and a date is what a reader can check against their own memory. */
function histDay(ms) {
  if (!ms) return '—';
  return new Date(ms).toLocaleDateString(undefined, { day: 'numeric', month: 'short', year: 'numeric' });
}
function histShortDay(ms) {
  if (!ms) return '—';
  return new Date(ms).toLocaleDateString(undefined, { day: 'numeric', month: 'short' });
}

/* Whole days between two instants, for "last used N days ago". */
function histDaysSince(ms) {
  if (!ms) return null;
  return Math.floor((Date.now() - ms) / 86400000);
}

function histRender() {
  const root = document.getElementById('history-body');
  if (!root) return;
  clear(root);
  const d = histDoc;
  if (!d) return;

  /* THE SPAN FIRST, because every claim below is "in this window" and a reader who misses the
   * window misreads all of them. Stated as a span and a day count: "40 days" is what makes
   * "never used" mean something. */
  const days = d.first_ts && d.last_ts
    ? Math.max(1, Math.round((d.last_ts - d.first_ts) / 86400000)) : 0;
  const header = el('div', { class: 'ins-head' });
  header.appendChild(el('h2', { text: 'What you have actually used' }));
  if (days) {
    header.appendChild(el('p', { class: 'hint', text:
      'From this machine\'s own session history: ' + histDay(d.first_ts) + ' to ' +
      histDay(d.last_ts) + ', ' + days + ' days.' }));
  }
  root.appendChild(header);

  /* THE HEADLINE, and the one finding available on install day with no inference at all: the
   * inventory is on disk and the invocations are in the transcripts. Both halves present, because
   * "40 never used" is not a finding without "of 46 installed". */
  const inst = d.installed_skills || 0;
  const usedInst = d.used_installed || 0;
  const never = (d.never_used || []).length;
  if (inst) {
    /* TWO DIFFERENT HEADLINES, because the two scopes answer different questions.
     *
     * Pooled: the waste list, and nothing else — "never used in ANY project" is the one claim
     * strong enough to act on, since those skills are carried on every request and have earned
     * nothing in the whole recorded history.
     *
     * A project: what was used HERE, with the unused count SPLIT. "43 not used here" is not a
     * waste figure — a skill used in another project is being used correctly — so the waste half
     * and the used-elsewhere half are stated separately. */
    const grid = el('div', { class: 'grid' });
    if (!histProject) {
      grid.appendChild(el('div', { class: 'tile' + (never ? ' bad' : '') },
        el('div', { class: 'k', text: 'never used in any project' }),
        el('div', { class: 'v', text: never + ' of ' + inst }),
        el('div', { class: 's', text: 'installed on this machine and never once invoked, in '
          + days + ' days of history. Carried in the prompt on every request regardless.' })));
      grid.appendChild(el('div', { class: 'tile' },
        el('div', { class: 'k', text: 'skills you do use' }),
        el('div', { class: 'v', text: String(usedInst) }),
        el('div', { class: 's', text: 'invoked at least once somewhere on this machine' })));
    } else {
      const elsewhere = d.unused_here_but_used_elsewhere || 0;
      const usedHere = d.used_here || 0;
      const wasteHere = Math.max(0, (d.unused_here || 0) - elsewhere);
      /* THE TWO ADDENDS BESIDE THEIR SUM.
       *
       * Reported against the live page: "how is it possible that every project writes a
       * different number of used tools but the same number of unused tools, if the number of all
       * tools is the same?" The figures were right; the layout hid the identity the reader was
       * trying to check. Of the skills used anywhere on this machine, this project uses some and
       * the others are used in different projects -- usedHere + elsewhere is that total, and
       * showing it is what makes the row add up on screen. */
      grid.appendChild(el('div', { class: 'tile' },
        el('div', { class: 'k', text: 'used in this project' }),
        el('div', { class: 'v', text: usedHere + ' of ' + (usedHere + elsewhere) }),
        el('div', { class: 's', text: 'of the ' + (usedHere + elsewhere) + ' skills you use '
          + 'anywhere, this project uses ' + usedHere + ' and the other ' + elsewhere
          + ' are used in different projects — which is not waste' })));
      grid.appendChild(el('div', { class: 'tile' + (wasteHere ? ' bad' : '') },
        el('div', { class: 'k', text: 'never used, anywhere' }),
        el('div', { class: 'v', text: wasteHere + ' of ' + inst }),
        /* LABELLED AS NOT BELONGING TO THE PROJECT. It cannot move when a project is selected --
         * picking a folder does not change which skills you have used somewhere -- so beside two
         * figures that do move it reads as a number that is stuck unless this says otherwise. */
        el('div', { class: 's', text: 'the waste, and the same in every project: picking a '
          + 'folder does not change which skills you have used somewhere' })));
    }
    grid.appendChild(el('div', { class: 'tile' },
      el('div', { class: 'k', text: 'evidence' }),
      el('div', { class: 'v', text: days + ' days' }),
      el('div', { class: 's', text: 'a short span means a skill may not have come up yet, '
        + 'not that it is unused' })));
    root.appendChild(grid);
  }

  /* HOW MUCH OF THIS THE PROXY ACTUALLY WATCHED. The tab's most confusable property: a reader
   * comparing this span with Cleanup's, and not being told, reads the longer one as the better
   * evidence — when in fact it is the weaker, because the imported part has no cost data at all. */
  const imp = d.imported_requests || 0;
  const meas = d.measured_requests || 0;
  if (imp || meas) {
    root.appendChild(el('p', { class: 'hint' },
      el('strong', { text: 'Where this comes from. ' }),
      el('span', { text:
        nf.format(meas) + ' requests this proxy measured itself, and ' + nf.format(imp) +
        ' recovered from Claude Code\u2019s own transcripts from before it was installed. ' +
        'The usage above counts both. No money is shown on this tab, deliberately: a transcript ' +
        'records what a request spent in total and never which tools it declared, so what ' +
        'carrying an unused skill cost is not recoverable from it. For money, see Cleanup — ' +
        'which covers only the span this proxy watched.' })));
  }

  /* The two halves of the inventory, as tabs rather than two long lists: they answer opposite
   * questions and a reader is asking one of them at a time. */
  root.appendChild(histProjectPicker());
  root.appendChild(histTabs());
  const body = el('div', { id: 'history-list' });
  root.appendChild(body);
  histRenderList(body);

  /* THE EXCLUSION, DISCLOSED. This plugin's own skills are held out of every figure above —
   * counting the weight of the tool producing the view would be circular — but they do occupy
   * the prompt like any other, and a reader who counts more skills in `/plugin` than appear here
   * has to be able to find out why. "The tool hid its own cost" is a worse look than the cost. */
  const se = d.self_excluded;
  if (se && se.items) {
    root.appendChild(el('p', { class: 'hint', text:
      se.plugin + '’s own ' + se.items + ' skills are excluded from every figure above: it ' +
      'is the plugin producing this view, so counting its own weight here would be circular. ' +
      'They do occupy the prompt like any other skill — the Cleanup tab reports what they cost.' }));
  }

  /* The basis, verbatim from the document. Rendered rather than paraphrased, because it is the
   * field whose job is stopping this page being read as a bill. */
  if (d.basis) {
    root.appendChild(el('p', { class: 'hint', text: 'Basis: ' + d.basis }));
  }
}

function histTabs() {
  const mk = (id, label, n) => el('button', {
    class: histTab === id ? 'on' : null,
    'aria-pressed': String(histTab === id),
    'data-testid': 'history-tab-' + id,
    onclick: () => {
      if (histTab === id) return;
      histTab = id;
      /* Just the list and the two buttons, not the whole view: a tab switch must not cost the
       * reader their scroll position, and both halves come from the document already in hand —
       * no re-query. */
      histSwapList();
    },
  }, label + ' (' + n + ')');
  return el('div', { class: 'ins-tabs' },
    mk('unused', 'Never used', (histDoc.never_used || []).length),
    mk('used', 'Used', (histDoc.used || []).length),
    el('span', { class: 'hint', text: histTab === 'unused'
      ? 'Installed here and absent from every invocation in the window.'
      : 'Everything the history shows being reached for, with when.' }));
}

/* histSwapList redraws the list and re-stamps the two buttons, leaving the rest of the view
 * alone. The same reason insights.js swaps its chart in place rather than re-rendering. */
function histSwapList() {
  const tabs = document.querySelector('#history-body .ins-tabs');
  const list = document.getElementById('history-list');
  if (!tabs || !list || !tabs.parentNode) return;
  tabs.parentNode.replaceChild(histTabs(), tabs);
  histRenderList(list);
}

function histRenderList(root) {
  clear(root);
  if (histTab === 'unused') return histRenderUnused(root);
  return histRenderUsed(root);
}

/* THE FINDING. Installed and never once reached for, over the whole recoverable span. */
function histRenderUnused(root) {
  const names = histDoc.never_used || [];
  if (!names.length) {
    root.appendChild(el('p', { class: 'empty', text:
      histDoc.installed_skills
        ? 'Every installed skill has been used at least once in this window.'
        : 'No installed skills were found on this machine. Plugin skills live under ' +
          '~/.claude/plugins; a hosted proxy cannot see them at all.' }));
    return;
  }
  root.appendChild(el('p', { class: 'hint', text:
    'Installed on this machine and never invoked in ANY project across ' + Math.max(1, Math.round(
      ((histDoc.last_ts - histDoc.first_ts) / 86400000))) + ' days of history. Each is carried ' +
    'in the prompt on every request. What that costs is on the Cleanup tab, for the part of the ' +
    'window this proxy measured.' }));

  /* Grouped by owner, because removal operates at that level: a plugin's skills come and go
   * together, so fifteen rows from one plugin are one decision, not fifteen. */
  const byOwner = new Map();
  for (const n of names) {
    const owner = n.includes(':') ? n.slice(0, n.indexOf(':')) : '(standalone)';
    if (!byOwner.has(owner)) byOwner.set(owner, []);
    byOwner.get(owner).push(n);
  }
  const owners = [...byOwner.entries()].sort((a, b) => b[1].length - a[1].length);
  const body = [];
  for (const [owner, list] of owners) {
    for (const n of list.sort()) {
      body.push(el('tr', {},
        el('td', { class: 'ins-name', text: n }),
        el('td', { text: owner })));
    }
  }
  root.appendChild(el('div', { class: 'tblwrap' },
    el('table', { class: 'tbl' },
      el('thead', {}, el('tr', {},
        el('th', { text: 'Skill' }),
        el('th', { text: 'Owner' }))),
      el('tbody', {}, ...body))));
}

/* The other half: what the history shows being reached for, and when. */
function histRenderUsed(root) {
  const used = histDoc.used || [];
  if (!used.length) {
    root.appendChild(el('p', { class: 'empty', text: 'No invocations in this window.' }));
    return;
  }
  root.appendChild(el('p', { class: 'hint', text:
    (histProject
      ? 'Invoked in ' + insShortProject(histProject) + ', most-used first. '
      : 'Invoked in any project, most-used first. ') +
    'First and last dates rather than a bare total: "used once in March" and "used daily since ' +
    'March" are different facts, and a count cannot tell them apart.' }));
  const body = used.map((u) => {
    const since = histDaysSince(u.last_ts);
    return el('tr', {},
      el('td', { class: 'ins-name', text: u.name }),
      el('td', { text: u.skill ? 'skill' : 'tool' }),
      el('td', { class: 'num', text: nf.format(u.calls || 0) }),
      el('td', { text: histShortDay(u.first_ts) }),
      /* "N days ago" only past the staleness floor, where it is a finding rather than noise:
       * a skill used yesterday does not need the arithmetic spelled out. */
      el('td', { text: histShortDay(u.last_ts) + (since !== null && since > 14
        ? ' (' + since + 'd ago)' : '') }));
  });
  root.appendChild(el('div', { class: 'tblwrap' },
    el('table', { class: 'tbl' },
      el('thead', {}, el('tr', {},
        el('th', { text: 'Capability' }),
        el('th', { text: 'Kind' }),
        el('th', { class: 'num', text: 'Calls' }),
        el('th', { text: 'First used' }),
        el('th', { text: 'Last used' }))),
      el('tbody', {}, ...body))));
}

/* The project picker. Scopes the USED list and nothing else.
 *
 * Deliberately NOT the never-used count, which stays machine-wide: a skill you run in your notes
 * and never in a Go repo is not waste, so scoping that figure to a folder reports deliberate
 * usage as abandonment. The two tiles say which scope each carries. */
function histProjectPicker() {
  if (!histProjects || !histProjects.length) return el('span', {});
  const sel = el('select', {
    'data-testid': 'history-project',
    'aria-label': 'Project folder',
    onchange: (ev) => {
      histProject = ev.currentTarget.value;
      /* Re-query: scoping is a server-side decision. */
      loadHistory();
    },
  });
  const opts = [['', 'all projects']];
  for (const p of histProjects) opts.push([p, insShortProject(p)]);
  for (const [v, label] of opts) {
    const o = el('option', { value: v, title: v || 'every project pooled together' }, label);
    if (v === histProject) o.setAttribute('selected', 'selected');
    sel.appendChild(o);
  }
  return el('div', { class: 'ins-proj' },
    el('label', {}, 'Project ', sel),
    el('span', { class: 'hint', text: histProject
      ? 'Scopes what was USED below. The never-used count above stays machine-wide.'
      : 'Every project pooled.' }));
}

async function loadHistory() {
  const root = document.getElementById('history-body');
  if (root && !histDoc) {
    clear(root);
    root.appendChild(el('p', { class: 'empty', text: 'Loading…' }));
  }
  try {
    const q = {};
    if (histProject) q.project = histProject;
    histDoc = await api('history', q);
    /* The folder list comes from the UNSCOPED load only.
     *
     * A scoped response carries one project, so capturing the list from it would shrink the
     * picker to the folder already chosen and leave the reader no way back — the same trap
     * insights.js documents for its chart axis, where one drag cut the axis from six columns to
     * two. Captured once, while the first load is still pooled. */
    if (!histProjects && !histProject) {
      histProjects = (histDoc.projects || []).map((p) => p.project).filter(Boolean);
    }
    histRender();
  } catch (e) {
    if (e.name === 'AbortError') return;
    if (root) {
      clear(root);
      root.appendChild(el('p', { class: 'empty', text: 'Could not load history: ' + e.message }));
    }
  }
}
