// context-guru dashboard: the Tenants tab (manager): tenants, the account editor and A/B variants.
// Moved out of app.js (#1089). app.js loads first and holds the shared DOM helpers,
// formatting, state and router that this file uses. See the file list in index.html.
'use strict';

// ── tenants (manager) ──────────────────────────────────────────────────────

/** captureCell renders the EFFECTIVE transcript state for one account: consent AND the
 *  operator's service-wide gate. Consent alone is not retention. */
function captureCell(consented, operatorGate) {
  if (!consented) return el('span', { class: 'pill neutral', text: 'not consented' });
  if (operatorGate === false) {
    return el('div', {},
      el('span', { class: 'pill missing', text: 'none stored' }),
      el('div', { class: 'muted small', text: 'consented, but capture is off service-wide' }));
  }
  if (operatorGate === null) {
    return el('div', {},
      el('span', { class: 'pill partial', text: 'consented' }),
      el('div', { class: 'muted small', text: 'service-wide gate unknown from here' }));
  }
  return el('span', { class: 'pill complete', text: 'stored' });
}

async function loadTenants() {
  const host = clear($('#tenants-list'));
  loadingState(host);
  try {
    const out = await ctl('/api/tenants');
    const rows = out.tenants || [];
    // Storing a transcript needs BOTH gates: the operator's service-wide one and the
    // account's own consent. A column showing only the second reads as "transcripts are
    // being retained for this account" while the operator's gate is shut and none exist.
    // The server's own configuration is the only place the gate is published.
    let operatorGate = null; // null = could not read it; do not guess either way
    try {
      const cfg = await api('config');
      const dash = (cfg.config || {}).dashboard || {};
      if (typeof dash.capture_content === 'boolean') operatorGate = dash.capture_content;
    } catch (_) { /* stays null: the column says "unknown" rather than inventing a state */ }
    $('#tenants-count').textContent = `${rows.length} account${rows.length === 1 ? '' : 's'}`;
    clear(host);
    if (operatorGate === false) {
      host.appendChild(el('div', { class: 'state blocked', 'data-testid': 'tenants-capture-gate' },
        el('div', { class: 'state-body' },
          el('strong', { text: 'No transcripts are being stored on this proxy' }),
          el('span', { text:
            'Transcript storage is off service-wide (--dashboard-content), which overrides every ' +
            'account\'s own consent below: an account marked "consented" has nothing stored and ' +
            'no diff view. Turning it on is yours to do, not theirs.' }))));
    }
    const tbl = el('table', { class: 'grid' },
      el('thead', {}, el('tr', {},
        el('th', {}, 'Account'), el('th', {}, 'Role'), el('th', {}, term('Group', 'The A/B test group this account was put in, if any.')), el('th', {}, 'Spend'),
        el('th', {}, 'Last seen'), el('th', {}, term('Transcripts', 'Whether the text of this account\'s requests is being stored for the diff view.')), el('th', {}, 'Configuration'),
        el('th', {}, el('span', { class: 'vh' }, 'Row actions')))));
    const body = el('tbody');
    for (const t of rows) {
      body.appendChild(el('tr', { class: t.disabled ? 'revoked' : '' },
        el('td', {}, el('div', {}, t.email),
          el('div', { class: 'muted small' }, t.label),
          // Why an account is off, printed where the manager will read it — the same
          // sentence its owner is shown when their agent is refused.
          t.disabled
            ? el('div', { class: 'small warn-text' },
              t.disabled_reason ? 'disabled: ' + t.disabled_reason : 'disabled (no reason recorded)')
            : null),
        el('td', {}, t.role),
        el('td', {}, t.variant
          ? el('span', { class: 'ab-name' }, t.variant)
          : el('span', { class: 'muted' }, '—')),
        el('td', {}, usd(t.spent_usd)),
        el('td', {}, t.last_seen_at ? when(t.last_seen_at) : el('span', { class: 'muted' }, 'never')),
        // The EFFECTIVE answer to "are this account's transcripts being kept", which is
        // the AND of both gates — never the account's flag on its own.
        el('td', {}, captureCell(t.capture_content, operatorGate)),
        // The EFFECTIVE pipeline, plus whose it is: an account that follows the default
        // stores nothing, and an empty cell would read as a broken account.
        //
        // Not the document's first line, which is what this cell used to show. Since the
        // settings form writes the document through a YAML marshaller its keys come out
        // alphabetically, so line one is the word `components:` for every account that has
        // ever saved — a cell that reads as "configured with no components" on exactly the
        // accounts that are configured. The pipeline is the fact anyone opens this column
        // for anyway.
        el('td', {},
          el('code', { class: 'clip' },
            ((t.effective_config || {}).pipeline || []).join(' → ')
            || (t.effective_config_yaml ? 'no components' : '—')),
          t.config_inherited ? el('div', { class: 'muted small' }, 'server default') : null),
        // Two buttons, not five. Everything that CHANGES this account — its configuration,
        // its variant, disable, a reissued token, a reset, purge, delete — is in the editor,
        // where the account's own facts are on screen next to the control. A row of small
        // ghost buttons that each act on a different account by id is how the wrong row
        // gets clicked.
        el('td', {}, el('div', { class: 'row-actions' },
          el('button', {
            class: 'ghost small', onclick: () => showTenantMetrics(t.id),
          }, 'Metrics'),
          el('button', {
            class: 'ghost small', 'data-testid': 'manage-' + t.id,
            onclick: () => openTenantEditor(t.id),
          }, 'Manage')))));
    }
    tbl.appendChild(body);
    host.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, tbl));
  } catch (e) {
    clear(host);
    errorState(host, 'Could not list tenants', e);
  }
  // The A/B card lives in this view, above the roster: a variant is assigned here, so the
  // comparison belongs next to the assignment rather than behind another tab.
  loadVariants();
}

/** Jump to this tenant's traffic. Managers get ?tenant= on every read route, so the
 *  existing views work unchanged once the filter is set. */
function showTenantMetrics(id) {
  // quiet: go() below pushes the URL and refetches once. Setting it loudly here would
  // fetch the current view with the new scope and then immediately fetch Overview.
  setFilter('tenant', id, { quiet: true });
  go('overview');
}

// ── the account editor (manager) ───────────────────────────────────────────
//
// One panel per account, opened from the roster and linkable (#tenants?acct=<id>), because
// "the account I mean" is a thing managers send each other. It reuses the request drawer:
// same focus trap, same Escape, same Back-closes-it behaviour — a second panel would need
// all of that again and would get some of it wrong.
//
// Everything that mutates an account is in here rather than in its row, so the facts and
// the controls are on screen together. The alternative — a row of small buttons that each
// act on a different account by id — is how the wrong row gets clicked.

/** openTenantEditor shows one account. fromURL suppresses the history push. */
async function openTenantEditor(id, fromURL) {
  if (!fromURL) { state.drawer = { acct: id }; syncURL(false); }
  const body = openDrawer('Account', null);
  loadingState(body, 4);
  let rows = [];
  try {
    rows = (await ctl('/api/tenants')).tenants || [];
  } catch (e) { errorState(body, 'Could not read the roster', e); return; }
  const t = rows.find((x) => x.id === id);
  if (!t) {
    emptyState(clear(body), 'No such account', 'It may have just been deleted.');
    return;
  }
  await loadOptions(); // the upstream allow-list, so the selects offer only what is accepted
  renderTenantEditor(clear(body), t);
}

/** reloadTenantEditor repaints the panel and the roster behind it after a change, so the
 *  two can never disagree about what was just saved. */
function reloadTenantEditor(id) {
  loadTenants();
  openTenantEditor(id, true);
}

function renderTenantEditor(host, t) {
  $('#drawer-title').textContent = t.email;
  const status = el('span', { class: 'muted small', 'data-testid': 'acct-status' });
  const say = (msg, bad) => {
    status.textContent = msg || '';
    status.className = 'small ' + (bad ? 'warn-text' : 'muted');
  };

  // What this account IS, before anything about changing it — the same fact band the
  // request drawer uses.
  host.appendChild(el('div', { class: 'kv-band' },
    el('div', { class: 'kv' },
      kv('Role', t.role),
      kv('Variant', t.variant || 'none'),
      kv('Month to date', usd(t.spent_usd)),
      kv('Last seen', t.last_seen_at ? when(t.last_seen_at) : 'never'),
      kv('Registered', when(t.created_at)),
      kv('Provider keys bound', num(t.agent_keys)),
      kv('Row quota', t.max_rows ? num(t.max_rows) : 'server default'),
      kv('Password', t.has_password ? 'set' : 'never set'),
      kv('Status', t.disabled ? 'disabled' : 'active'))));

  // Says where the transcripts are rather than leaving a manager to hunt: the drawer and
  // diff viewer are the tenant's own, reached by pointing the account selector here.
  host.appendChild(el('div', { class: 'state blocked' },
    el('div', { class: 'state-body' },
      el('strong', { text: 'You can read this account’s transcripts' }),
      el('span', {
        text: 'Pick this account in the selector, then open any request or session diff. '
          + 'Only what they consented to capture exists to read.',
      }))));

  const fields = el('div');
  host.appendChild(fields);

  const label = textField(fields, 'acct-label', 'Machine label', t.label);
  const variant = textField(fields, 'acct-variant', 'A/B variant', t.variant,
    'A label only — it changes nothing about their pipeline. Letters, digits, dot, dash '
    + 'or underscore. Empty means not in a test.');
  const role = el('select', { id: 'acct-role', 'data-testid': 'acct-role' },
    el('option', { value: 'user' }, 'user'),
    el('option', { value: 'manager' }, 'manager — can administer every account'));
  role.value = t.role;
  fields.appendChild(el('div', { class: 'field' },
    el('label', { for: 'acct-role' }, 'Role'), role));

  const rowsCap = el('input', {
    type: 'number', min: '0', step: '1000', id: 'acct-rows', 'data-testid': 'acct-rows',
  });
  rowsCap.value = String(t.max_rows || 0);
  fields.appendChild(el('div', { class: 'field' },
    el('label', { for: 'acct-rows' }, 'Retained request rows'), rowsCap,
    el('p', { class: 'hint' }, '0 follows the server default. Over the cap their oldest '
      + 'sessions are archived or dropped — theirs, not everyone’s.')));

  const ups = (account.options && account.options.upstreams) || [];
  const upSel = {};
  for (const [key, text] of [['up_anthropic', 'Anthropic provider address'],
    ['up_openai', 'OpenAI provider address'], ['up_bob', 'Bob provider address']]) {
    const sel = el('select', { id: 'acct-' + key, 'data-testid': 'acct-' + key },
      el('option', { value: '' }, '— none —'),
      ...ups.map((u) => el('option', { value: u.name }, u.name + ' (' + u.dialect + ')')));
    sel.value = t[key] || '';
    upSel[key] = sel;
    fields.appendChild(el('div', { class: 'field' }, el('label', { for: 'acct-' + key }, text), sel));
  }

  const cap = el('input', { type: 'checkbox', id: 'acct-capture', 'data-testid': 'acct-capture' });
  cap.checked = !!t.capture_content;
  fields.appendChild(el('div', { class: 'field' },
    el('label', { class: 'comp', for: 'acct-capture' }, cap,
      el('span', { class: 'comp-name' }, 'Store their transcripts')),
    el('p', { class: 'hint' }, 'Their consent, which you can withdraw but never read '
      + 'through. Turning it off stops new capture; it deletes nothing.')));

  // Whether the configuration is theirs or the server default is the first thing to say,
  // because saving a tracking account's form must not silently freeze a copy of today's
  // default onto it — the same trap the user's own settings page avoids.
  const effective = t.effective_config_yaml || '';
  const inherited = !!t.config_inherited;
  fields.appendChild(el('div', { class: 'cfg-state' },
    el('div', {}, inherited
      ? el('span', {}, el('strong', {}, 'Following the server default.'),
        ' Saving this form leaves that alone unless you edit the YAML below.')
      : el('span', {}, el('strong', {}, 'Has its own configuration.'),
        ' Changes to the server default do not reach them.'))));
  const yaml = el('textarea', {
    id: 'acct-yaml', rows: 12, spellcheck: 'false', 'data-testid': 'acct-yaml',
    'aria-label': 'Their full configuration, YAML',
  });
  yaml.value = effective;
  fields.appendChild(el('details', { class: 'field' },
    el('summary', {}, 'Full configuration (YAML)'), yaml,
    el('p', { class: 'hint' }, 'Pipeline, per-component settings and mode. Validated on '
      + 'save by the same loader the proxy builds with, so a typo is a refusal naming the '
      + 'key rather than a surprise at request time. Saving rebuilds their pipeline, which '
      + 'discards their frozen preset decisions — their next turn will not be cache-warm.')));

  fields.appendChild(el('div', { class: 'actions' },
    el('button', {
      class: 'primary', 'data-testid': 'acct-save',
      onclick: async () => {
        say('saving…');
        const patch = {
          label: label.value.trim(),
          variant: variant.value.trim(),
          role: role.value,
          max_rows: Number(rowsCap.value) || 0,
          capture_content: cap.checked,
          up_anthropic: upSel.up_anthropic.value,
          up_openai: upSel.up_openai.value,
          up_bob: upSel.up_bob.value,
        };
        // Omitted while it is inherited AND untouched: sending it would store a copy of
        // today's default and quietly end their tracking of it.
        if (!inherited || yaml.value.trim() !== effective.trim()) patch.config_yaml = yaml.value;
        try {
          await ctl('/api/tenants/' + t.id, { method: 'PATCH', body: JSON.stringify(patch) });
          reloadTenantEditor(t.id);
        } catch (e) { say('not saved: ' + e.message, true); }
      },
    }, 'Save'),
    status));

  // Availability and recovery. Disabling asks for a reason because the reason is what the
  // account's owner is shown when their agent is refused — without it, "disabled" is
  // indistinguishable from the proxy being broken.
  host.appendChild(el('h2', {}, 'Availability'));
  const reason = el('input', {
    type: 'text', id: 'acct-reason', maxlength: '200', 'data-testid': 'acct-reason',
    placeholder: 'e.g. paused pending the finance review',
  });
  reason.value = t.disabled_reason || '';
  if (!t.disabled) {
    host.appendChild(el('div', { class: 'field' },
      el('label', { for: 'acct-reason' }, 'Reason (shown to them)'), reason,
      el('p', { class: 'hint' }, 'Returned in the 403 their agent receives and in the '
        + 'refusal at sign-in. They are who reads this; write it for them.')));
  }
  host.appendChild(el('div', { class: 'actions' },
    el('button', {
      class: 'ghost', 'data-testid': 'acct-toggle',
      onclick: async () => {
        const patch = t.disabled
          ? { disabled: false }
          : { disabled: true, disabled_reason: reason.value.trim() };
        if (!t.disabled && !confirm('Disable ' + t.email + '? Their agents stop immediately '
          + 'and they are signed out of the dashboard.')) return;
        try {
          await ctl('/api/tenants/' + t.id, { method: 'PATCH', body: JSON.stringify(patch) });
          reloadTenantEditor(t.id);
        } catch (e) { say(e.message, true); }
      },
    }, t.disabled ? 'Enable' : 'Disable'),
    el('button', {
      class: 'ghost', 'data-testid': 'acct-reset',
      onclick: async () => {
        if (!confirm('Email ' + t.email + ' a password reset code?\n\nYou will not see the '
          + 'code and cannot set their password. Their current password keeps working '
          + 'until they finish.')) return;
        try {
          const out = await ctl('/api/tenants/' + t.id + '/password-reset', { method: 'POST' });
          say(out.note || 'reset code mailed');
        } catch (e) { say(e.message, true); }
      },
    }, 'Email a password reset'),
    el('button', {
      class: 'ghost',
      onclick: async () => {
        if (!confirm('Mint a replacement token for ' + t.email + '? Hand it over on a '
          + 'channel you trust; it is shown once.')) return;
        try {
          const out = await ctl('/api/tenants/' + t.id + '/tokens',
            { method: 'POST', body: JSON.stringify({ label: 'reissued' }) });
          prompt('Copy this token now — it cannot be recovered:', out.token);
        } catch (e) { say(e.message, true); }
      },
    }, 'Reissue token')));

  host.appendChild(dangerZone(t, say));
}

/** kv renders one label/value pair of the drawer's fact band. */
function kv(k, v) {
  return el('div', {}, el('div', { class: 'k' }, k), el('div', { class: 'v' }, String(v)));
}

/** textField appends a labelled text input and returns it. */
function textField(host, id, labelText, value, hint) {
  const input = el('input', { type: 'text', id, 'data-testid': id });
  input.value = value || '';
  host.appendChild(el('div', { class: 'field' },
    el('label', { for: id }, labelText), input,
    hint ? el('p', { class: 'hint' }, hint) : null));
  return input;
}

/**
 * dangerZone builds the purge and delete controls.
 *
 * Both are irreversible and both act on somebody else's data BY ID, so the mistake worth
 * engineering against is acting on the wrong account. The buttons are therefore inert until
 * that account's own address has been typed into the box beside them — the server demands
 * the same string, so this is the visible half of a check that is enforced anyway.
 *
 * Folded away behind a summary, and never a bare one-click button.
 */
function dangerZone(t, say) {
  const confirmBox = el('input', {
    type: 'text', id: 'acct-confirm', 'data-testid': 'acct-confirm',
    autocomplete: 'off', spellcheck: 'false', placeholder: t.email,
  });
  const purge = el('button', { class: 'ghost small', disabled: true, 'data-testid': 'acct-purge' },
    'Purge their stored data');
  const del = el('button', { class: 'destructive small', disabled: true, 'data-testid': 'acct-delete' },
    'Delete this account and its data');
  const matches = () => confirmBox.value.trim().toLowerCase() === t.email.toLowerCase();
  const sync = () => { purge.disabled = !matches(); del.disabled = !matches(); };
  confirmBox.addEventListener('input', sync);

  // What it removed, reported afterwards: a destructive action that answers "ok" tells the
  // person who pressed it nothing about whether it did anything.
  const report = el('p', { class: 'hint', 'data-testid': 'acct-purge-report' });
  const run = async (path, method, verb) => {
    if (!matches()) return;
    if (!confirm(verb + '.\n\nThis cannot be undone. Continue?')) return;
    say('working…');
    try {
      const out = await ctl(path, {
        method, body: JSON.stringify({ confirm: confirmBox.value.trim() }),
      });
      const p = out.purged || {};
      say('');
      report.className = 'hint ok';
      report.textContent = out.status + ': ' + num(p.requests) + ' requests, '
        + num(p.components) + ' component rows, ' + num(p.content) + ' stored transcripts, '
        + num(p.archives) + ' archived sessions (' + num(p.objects)
        + ' objects deleted from cold storage).';
      loadTenants();
      if (method === 'DELETE') {
        // The account is gone, so the panel describing it must not stay open showing a form
        // whose every button would now 404.
        setTimeout(() => { dismissDrawer(); state.drawer = null; syncURL(true); }, 1500);
      } else {
        confirmBox.value = '';
        sync();
      }
    } catch (e) {
      say('');
      report.className = 'hint warn-text';
      report.textContent = e.message;
    }
  };
  purge.addEventListener('click', () => run('/api/tenants/' + t.id + '/purge', 'POST',
    'Purge every stored request, component row and transcript for ' + t.email
    + ', including its archives in cold storage. The account itself keeps working'));
  del.addEventListener('click', () => run('/api/tenants/' + t.id, 'DELETE',
    'Delete ' + t.email + ': the account, its tokens, its sessions and all of its stored '
    + 'data in both databases and in cold storage'));

  return el('details', { class: 'danger', 'data-testid': 'acct-danger' },
    el('summary', {}, 'Danger zone'),
    el('p', { class: 'hint' },
      'Purge clears their history and leaves the account working. Delete removes the '
      + 'account too. Both reach the metrics database and cold storage, and neither can be '
      + 'undone — the audit record of having done it is all that is kept.'),
    el('div', { class: 'field' },
      el('label', { for: 'acct-confirm' }, 'Type ' + t.email + ' to enable these'),
      confirmBox),
    el('div', { class: 'danger-act' }, purge, del),
    report);
}

// ── A/B variants (manager) ─────────────────────────────────────────────────
//
// A variant is a name a manager put on a set of accounts; this panel groups the metrics
// that already exist by it. There is deliberately no significance test and no winner
// highlighting: assignment is not random and the workloads are not comparable, so a test
// statistic here would look like evidence without being any.
//
// A TABLE rather than a chart, on purpose. This is a handful of groups across eight
// measures whose DENOMINATORS have to sit beside the figures — a table's job. A bar chart
// of "spend per variant" would be precisely the misleading artefact the note above it warns
// about: two bars, no sample size, no confounds.

async function loadVariants() {
  const host = clear($('#ab-list'));
  const caveatHost = clear($('#ab-caveats'));
  loadingState(host, 2);
  const range = Number($('#ab-range').value) || 0;
  const q = range > 0 ? '?since=' + (Date.now() - range) : '';
  let out;
  try {
    out = await ctl('/api/variants' + q);
  } catch (e) { clear(host); errorState(host, 'Could not compare variants', e); return; }
  const rows = out.variants || [];
  clear(host);
  if (!rows.some((v) => v.variant)) {
    emptyState(host, 'No variants assigned yet',
      'Open an account from the roster below and give it a variant name. Two names over two '
      + 'groups is an A/B test; accounts with no name are grouped as unassigned.');
    return;
  }
  const tbl = el('table', { class: 'grid' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Variant'), el('th', {}, 'Accounts'), el('th', {}, 'Requests'),
      el('th', {}, 'Tokens in → out'), el('th', {}, 'Saved'), el('th', {}, 'Spent'),
      el('th', {}, 'Spent / request'), el('th', {}, 'Saved (est.)'),
      el('th', {}, 'Provider cache'),
      el('th', {}, 'Unpriced'))));
  const body = el('tbody');
  for (const v of rows) {
    const perReq = v.requests > 0 ? v.spent_usd / v.requests : null;
    const savedPct = v.tokens_before > 0 ? (100 * v.saved) / v.tokens_before : null;
    body.appendChild(el('tr', {},
      el('td', {},
        v.variant ? el('span', { class: 'ab-name' }, v.variant)
          : el('span', { class: 'muted' }, 'unassigned'),
        // Several configurations inside one variant means it is not one treatment. Said on
        // the row, because it invalidates that row's comparison specifically.
        (v.configs || []).length > 1
          ? el('span', { class: 'denom warn-text' }, (v.configs || []).length + ' different configs')
          : null),
      el('td', {}, num(v.tenants),
        el('span', { class: 'denom' }, num(v.reporting) + ' with traffic')),
      el('td', {}, num(v.requests),
        el('span', { class: 'denom' }, num(v.sessions) + (v.sessions === 1 ? ' session' : ' sessions'))),
      el('td', {}, compact(v.tokens_before) + ' → ' + compact(v.tokens_after)),
      el('td', {}, compact(v.saved),
        el('span', { class: 'denom' }, savedPct === null ? 'no traffic' : pct(savedPct) + ' of input')),
      el('td', {}, usd(v.spent_usd)),
      // The only normalisation offered, and the one this project has already been misled
      // by: it says nothing about cost per TASK, because a variant that needs more turns
      // can be cheaper per request and dearer per job.
      el('td', {}, perReq === null ? '—' : usd(perReq),
        el('span', { class: 'denom' }, 'not per task')),
      el('td', {}, usd(v.saved_usd),
        el('span', { class: 'denom' }, 'counterfactual')),
      // Beside it, because an arm that compacts deeper can win on the column above while
      // losing more than that on the cache it destroyed. Both figures, because they answer
      // different questions: the provider's cache saving is the control variable — the thing a
      // deep pipeline silently spends.
      el('td', {}, usd(v.cache_saved_usd),
        el('span', { class: 'denom' }, 'provider cache')),
      // Rows the provider priced for nobody. Where this approaches the request count, the
      // money columns on this row are unknown rather than small.
      el('td', {}, num(v.incomplete_rows),
        v.requests > 0 && v.incomplete_rows >= v.requests
          ? el('span', { class: 'denom warn-text' }, 'money unknown')
          : null)));
    // Which component did the work, folded across the variant's accounts — the row that
    // turns "arm B is cheaper" into something anyone can act on.
    const comps = v.components || [];
    if (comps.length) {
      const inner = el('table', { class: 'grid' },
        el('thead', {}, el('tr', {}, el('th', {}, 'Component'), el('th', {}, 'Ran'),
          el('th', {}, 'Acted'), el('th', {}, 'Reverted'), el('th', {}, 'Saved'))));
      const ibody = el('tbody');
      for (const c of comps.slice(0, 20)) {
        ibody.appendChild(el('tr', {},
          el('td', {}, el('code', {}, c.component)),
          el('td', {}, num(c.runs)),
          el('td', {}, num(c.acted) + ' (' + pct(100 * (c.act_rate || 0)) + ')'),
          el('td', {}, c.reverted ? el('span', { class: 'warn-text' }, num(c.reverted)) : '0'),
          el('td', {}, compact(c.saved_unique))));
      }
      inner.appendChild(ibody);
      body.appendChild(el('tr', {}, el('td', { colspan: '9' },
        el('details', {}, el('summary', { class: 'hint' },
          'Components in ' + (v.variant || 'unassigned')), inner))));
    }
  }
  tbl.appendChild(body);
  host.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, tbl));

  // The full caveat list comes from the server rather than being written twice: the API
  // decides what this comparison cannot show, and a second copy in the page would drift.
  const list = el('ul');
  for (const c of out.caveats || []) list.appendChild(el('li', {}, c));
  caveatHost.appendChild(el('details', { class: 'why' },
    el('summary', {}, 'What this comparison cannot tell you'),
    el('p', { class: 'hint' }, out.description || ''), list));
}

Object.assign(loaders, { tenants: loadTenants });
