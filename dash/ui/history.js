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
/* NO PROJECT SCOPE HERE, deliberately.
 *
 * This page answers one machine-wide question — what did you install and never use — and a
 * project picker cannot narrow it: "never used anywhere" does not change when you select a
 * folder. An earlier version had one, and the fixed figure sitting beside per-project ones is
 * what made the page unreadable. Per-project usage lives on Cleanup, which is scoped throughout. */

function histShortDay(ms) {
  if (!ms) return '—';
  return new Date(ms).toLocaleDateString(undefined, { day: 'numeric', month: 'short' });
}


function histRender() {
  const root = document.getElementById('history-body');
  if (!root) return;
  clear(root);
  const d = histDoc;
  if (!d) return;

  const inst = d.installed_skills || 0;
  const never = (d.never_used || []).length;
  const days = d.first_ts && d.last_ts
    ? Math.max(1, Math.round((d.last_ts - d.first_ts) / 86400000)) : 0;

  /* ONE QUESTION, ANSWERED ONCE: what did you install and never use?
   *
   * An earlier version of this tab showed four counts against four different populations —
   * skills used here of skills used anywhere, skills never used of all installed, a machine-wide
   * tab count, and a per-project tab count that included tools. Reported, fairly, as "not a
   * single place where I can understand something": no two of those numbers could be compared,
   * and two of them contradicted each other outright.
   *
   * So the page states one claim, in one population — installed skills — and everything that is
   * not that claim is either below it or gone. Tools live in their own section further down,
   * never in a count beside skills, because they are not removable and mixing them is what made
   * "0 of 6" sit next to "Used (1)". */
  root.appendChild(el('div', { class: 'ins-head' },
    el('h2', { text: 'What you installed and never used' }),
    /* THE SPAN AND THE KIND OF CLAIM, under the heading where it cannot be missed. This tab and
     * Cleanup are opened side by side by the dashboard skill and answer different questions over
     * different spans; read without this, History's longer span looks like better evidence for
     * money it does not carry. */
    el('p', { class: 'hint', text: 'Across all your Claude Code history, including the time '
      + 'before context-guru was installed. Counts only, no money: for what carrying these has '
      + 'cost, see Cleanup, which prices the time since install.' })));

  if (!inst) {
    root.appendChild(el('p', { class: 'empty', text:
      'No installed skills were found on this machine. Plugin skills live under '
      + '~/.claude/plugins; a hosted proxy cannot see them at all.' }));
    return;
  }

  /* THE HEADLINE, as a sentence rather than a tile. A tile invites comparison with the tile
   * beside it, and there is nothing here it should be compared with. */
  const head = el('p', { class: 'ins-total' });
  head.appendChild(el('strong', { text: never + ' of your ' + inst + ' installed skills' }));
  head.appendChild(el('span', { text: ' have never been invoked'
    + (days ? ', in ' + days + ' days of history' : '') + '. Each one is sent with every '
    + 'request you make.' }));
  root.appendChild(head);

  /* WHY NO MONEY. Stated here, not in a footnote: the absence of a cost column reads as zero. */
  const imp = d.imported_requests || 0;
  const meas = d.measured_requests || 0;
  root.appendChild(el('p', { class: 'hint', text:
    'From ' + nf.format(meas + imp) + ' requests — ' + nf.format(meas) + ' this proxy '
    + 'measured and ' + nf.format(imp) + ' recovered from Claude Code’s own transcripts '
    + 'from before it was installed. No money is shown here, deliberately: a transcript records '
    + 'what a request spent in total and never which tools it declared, so what carrying an '
    + 'unused skill cost is not recoverable from it. For money, see Cleanup — which covers '
    + 'only the span this proxy watched.' }));

  /* THE LIST, grouped by owner, because that is the unit of removal: a plugin's skills come and
   * go together, so 31 rows are not 31 decisions. */
  const groups = d.never_used_owners || [];
  if (!never) {
    root.appendChild(el('p', { class: 'empty', text:
      'Every installed skill has been used at least once. Nothing to remove.' }));
  } else {
    const grouped = groups.reduce((a, g) => a + g.skills.length, 0);
    root.appendChild(el('p', { class: 'hint', text: 'By the plugin or marketplace that installed '
      + 'them \u2014 which is what you remove, since a plugin\u2019s skills come and go together. '
      + groups.length + (groups.length === 1 ? ' owner, ' : ' owners, ') + grouped
      + ' skills in total.' }));
    for (const g of groups) {
      const sec = el('details', { class: 'ins-own', open: 'open' });
      sec.appendChild(el('summary', {},
        el('span', { class: 'ins-nm', text: g.owner }),
        el('span', { class: 'ins-meta', text: g.skills.length
          + (g.skills.length === 1 ? ' skill never used' : ' skills never used') })));
      const body = [];
      for (const n of g.skills) {
        /* The owner prefix is already the heading; repeating it on every row is noise. */
        const short = n.startsWith(g.owner + ':') ? n.slice(g.owner.length + 1) : n;
        body.push(el('tr', {}, el('td', { class: 'ins-name', text: short })));
      }
      sec.appendChild(el('div', { class: 'tblwrap' },
        el('table', { class: 'tbl' }, el('tbody', {}, ...body))));
      root.appendChild(sec);
    }
  }

  /* THE CONTROL THAT MAKES THE HEADLINE CHECKABLE. A reader who cannot see what DID get used
   * cannot judge a claim about what did not. Folded, because it is not the finding. */
  const usedSkills = (d.used || []).filter((u) => u.skill);
  if (usedSkills.length) {
    const sec = el('details', { class: 'ins-own' });
    sec.appendChild(el('summary', {},
      el('span', { class: 'ins-nm', text: 'The ' + usedSkills.length + ' you do use' }),
      el('span', { class: 'ins-meta', text: 'with when each was last reached for' })));
    sec.appendChild(el('div', { class: 'tblwrap' },
      el('table', { class: 'tbl' },
        el('thead', {}, el('tr', {},
          el('th', { text: 'Skill' }),
          el('th', { class: 'num', text: 'Calls' }),
          el('th', { text: 'First used' }),
          el('th', { text: 'Last used' }))),
        el('tbody', {}, ...usedSkills.map((u) => el('tr', {},
          el('td', { class: 'ins-name', text: u.name }),
          el('td', { class: 'num', text: nf.format(u.calls || 0) }),
          el('td', { text: histShortDay(u.first_ts) }),
          el('td', { text: histShortDay(u.last_ts) })))))));
    root.appendChild(sec);
  }

  /* TOOLS, IN THEIR OWN SECTION AND THEIR OWN COUNT. Never beside a skill count: Bash cannot be
   * removed, so it belongs to a different question, and putting the two populations in adjacent
   * numbers is exactly what made this page unreadable. */
  const tools = (d.used || []).filter((u) => !u.skill);
  if (tools.length) {
    const sec = el('details', { class: 'ins-own' });
    sec.appendChild(el('summary', {},
      el('span', { class: 'ins-nm', text: 'Built-in tools' }),
      el('span', { class: 'ins-meta', text: tools.length + ' invoked — context only, these '
        + 'are part of Claude Code and cannot be removed' })));
    sec.appendChild(el('div', { class: 'tblwrap' },
      el('table', { class: 'tbl' },
        el('thead', {}, el('tr', {},
          el('th', { text: 'Tool' }),
          el('th', { class: 'num', text: 'Calls' }),
          el('th', { text: 'Last used' }))),
        el('tbody', {}, ...tools.map((u) => el('tr', {},
          el('td', { class: 'ins-name', text: u.name }),
          el('td', { class: 'num', text: nf.format(u.calls || 0) }),
          el('td', { text: histShortDay(u.last_ts) })))))));
    root.appendChild(sec);
  }

  /* The exclusion, disclosed. This tool's own skills are held out of every figure above —
   * counting the weight of the reporter would be circular — but they do occupy the prompt, and a
   * reader who counts more skills in /plugin than appear here has to be able to find out why. */
  const se = d.self_excluded;
  if (se && se.items) {
    root.appendChild(el('p', { class: 'hint', text:
      se.plugin + '’s own ' + se.items + ' skills are excluded from every figure above: it '
      + 'is the plugin producing this view, so counting its own weight here would be circular. '
      + 'They do occupy the prompt like any other skill — the Cleanup tab reports what they '
      + 'cost.' }));
  }
  if (d.basis) {
    root.appendChild(el('p', { class: 'hint', text: 'Basis: ' + d.basis }));
  }
}

async function loadHistory() {
  const root = document.getElementById('history-body');
  if (root && !histDoc) {
    clear(root);
    root.appendChild(el('p', { class: 'empty', text: 'Loading…' }));
  }
  try {
    histDoc = await api('history');
    histRender();
  } catch (e) {
    if (e.name === 'AbortError') return;
    if (root) {
      clear(root);
      root.appendChild(el('p', { class: 'empty', text: 'Could not load history: ' + e.message }));
    }
  }
}
