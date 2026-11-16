// context-guru dashboard: the Settings tab.
// Moved out of app.js (#1089). app.js loads first and holds the shared DOM helpers,
// formatting, state and router that this file uses. See the file list in index.html.
'use strict';

// ── settings ───────────────────────────────────────────────────────────────
function componentPickers(pipeline, opts) {
  // The pipeline comes from the server as a resolved list of names (tenant.effective_config).
  // It used to be scraped out of the YAML text with a regex here, which only understood the
  // flow style and read an empty pipeline for a `preset:` account — so the grid showed
  // nothing running on the very configurations that ran the most.
  return { active: new Set(pipeline || []), all: (opts && opts.components) || [] };
}

function loadSettings() {
  const host = clear($('#settings-form'));
  const t = account.tenant;
  if (!t) { emptyState(host, 'Not signed in', 'Sign in to manage your configuration.'); return; }

  loadOptions().then((opts) => {
    clear(host);
    // Two states, and the page must not blur them. A tenant who has stored nothing
    // FOLLOWS the server default: config_yaml is empty, and drawing that would be a
    // blank form reading as "my configuration is gone". So the controls always show the
    // EFFECTIVE document — what the proxy actually runs — and when it is inherited they
    // are read-only and labelled as such, because it is not this tenant's choice yet.
    const inherited = !!t.config_inherited;
    const cfg = t.effective_config || {};
    const { active, all } = componentPickers(cfg.pipeline, opts);
    // The descriptors and the recommended prefill, both from /api/options. Nothing about a
    // field — its name, type, default, enum options, min or hint — is written in this file.
    const compFields = (opts && opts.component_fields) || {};
    const recommended = (opts && opts.recommended) || {};
    // Seeded with ONLY the keys the stored document states. See cfgState — an absent key is
    // not a zero. Secrets are dropped on the way in as well as on the way out: the server
    // does not read one back (config.readBlocks skips them), and if some future payload did,
    // holding it here would put a credential in a form's state and post it straight back.
    cfgState = {};
    for (const [cname, vals] of Object.entries(cfg.components || {})) {
      const secret = new Set((compFields[cname] || []).filter((fd) => fd.secret).map((fd) => fd.key));
      cfgState[cname] = Object.fromEntries(Object.entries(vals).filter(([k]) => !secret.has(k)));
    }
    // A best-effort read of a document the server could not fully load is drawn but never
    // editable: posting it back would write the fallback's guess over the real thing. The
    // server refuses that too (409), which is the backstop rather than the only guard.
    const cfgDisabled = inherited || !!cfg.parse_error;

    // One <details> per component that HAS knobs, drawn from the descriptors. Redrawn in
    // place when a component is enabled or disabled, or when the recommended values are
    // taken — cfgState survives a redraw, which is what makes the round trip work.
    const compHost = el('div', { 'data-testid': 'comp-fields' });
    const enabledNow = (cname) => {
      const cb = $('#comp-' + cname);
      return cb ? cb.checked : active.has(cname);
    };
    const drawComps = () => {
      clear(compHost);
      for (const cname of all) {
        const fields = compFields[cname] || [];
        if (!fields.length) continue; // takes no configuration (cachesplit)
        if (!cfgState[cname]) cfgState[cname] = {};
        const off = !enabledNow(cname);
        compHost.appendChild(renderComponentFields(cname, fields, cfgState[cname],
          cfgDisabled || off,
          { recommended: recommended[cname], redraw: drawComps, opts, off }));
      }
    };
    // Still the document itself, for the two things that are ABOUT the document rather than
    // about its contents: Customise stores a byte-identical copy of the default (comments and
    // all), and "identical to the current default" is a text comparison.
    const effective = t.effective_config_yaml || t.config_yaml || '';

    // Spend, first: it is what a shared box gets asked about most. Reported only —
    // your traffic runs on YOUR provider credential, so there is nothing to cap.
    host.appendChild(el('div', { class: 'spend' },
      el('div', { class: 'spend-label' }, `Spend this month: ${usd(t.spent_usd)}`),
      el('p', { class: 'hint' }, 'Billed to your own provider account, not to us.')));

    // Agent keys. Only relevant to agents that cannot send x-context-guru-token.
    //
    // The paste field exists because the alternative was a curl line carrying the
    // cg_dash cookie, and every part of that went wrong in practice: the cookie is not
    // displayed anywhere, so it meant a devtools detour, and the natural guess — pasting
    // the cg_live_ token into the cookie — fails with "no context-guru token", which
    // names a header this route does not even read. The browser already holds the
    // cookie. So the key is pasted HERE and sent in the Authorization slot by the same
    // fetch, which is the only step the person could not do for themselves.
    const keyIn = el('input', {
      type: 'password', id: 'agent-key', autocomplete: 'off', spellcheck: 'false',
      placeholder: 'paste your Bob API key', 'data-testid': 'agent-key-input',
    });
    const keyMsg = el('p', { class: 'hint', role: 'status', 'data-testid': 'agent-key-status' });
    host.appendChild(el('div', { class: 'field' },
      el('label', { for: 'agent-key' }, 'Bound agent keys'),
      el('div', { 'data-testid': 'agent-keys' },
        t.agent_keys > 0
          ? `${t.agent_keys} provider key${t.agent_keys === 1 ? '' : 's'} bound to this account.`
          : 'None bound.'),
      whyBlock('Why an agent needs one',
        'For agents that cannot send a custom header (Bob/BobShell): the proxy recognises ' +
        'them by the sha256 of the provider key they already send. Only the digest is ' +
        'stored — never the key, and it is not sent on anywhere. Keys under 20 characters ' +
        'are refused — the digest is the identity, so a short key would be a guessable ' +
        'account. A key already bound to another account is refused too, never moved: ' +
        'its owner unbinds it first.'),
      keyIn,
      el('div', { class: 'actions' }, el('button', {
        class: 'primary small', 'data-testid': 'agent-key-bind',
        onclick: async () => {
          const key = keyIn.value.trim();
          keyMsg.className = 'hint';
          if (!key) { keyMsg.textContent = 'Paste the key your agent sends first.'; return; }
          keyMsg.textContent = 'binding…';
          try {
            await ctl('/api/me/agent-key', {
              method: 'POST', headers: { authorization: `Bearer ${key}` },
            });
            // Cleared on success, so the key does not sit in a form field afterwards.
            keyIn.value = '';
            keyMsg.className = 'hint ok';
            keyMsg.textContent = 'Bound. Your agent is recognised by this key from now on.';
            await probeAccount();
            loadSettings();
          } catch (e) {
            keyMsg.className = 'hint warn-text';
            keyMsg.textContent = e.message;
          }
        },
      }, 'Bind this key')),
      keyMsg,
      t.agent_keys > 0
        ? el('button', {
          class: 'ghost small', 'data-testid': 'agent-keys-clear',
          onclick: async () => {
            if (!confirm('Unbind every provider key? Agents that rely on key ' +
              'recognition stop being identified until you bind again.')) return;
            try { await ctl('/api/me/agent-key', { method: 'DELETE' }); await probeAccount(); loadSettings(); } catch (e) { alert(e.message); }
          },
        }, 'Unbind all')
        : null));

    // Who may shape the compaction itself. A plain account keeps its own settings — the
    // upstreams it sends to, its capture consent, its tokens — but the pipeline is the
    // manager's to set, on this page and in PUT /api/me. Drawing a component grid that
    // the server answers 403 to would be a form that lies.
    const mgr = isManager();
    if (!mgr) {
      host.appendChild(el('div', { class: 'cfg-state', 'data-testid': 'cfg-state-managed' },
        el('div', {},
          el('strong', {}, 'Your manager sets your preset.'),
          ' Ask them for a change; everything else on this page is yours.')));
    }

    // Which configuration is in force, and how to change that.
    if (mgr) host.appendChild(inherited
      ? el('div', { class: 'cfg-state', 'data-testid': 'cfg-state-inherited' },
        el('div', {},
          el('strong', {}, 'Following the server default.'),
          ' The steps below are what your traffic uses now. They change when the ' +
          'operator changes the default.'),
        el('button', {
          class: 'ghost small', 'data-testid': 'cfg-customise',
          onclick: () => setStoredConfig(effective),
        }, 'Customise'))
      : el('div', { class: 'cfg-state', 'data-testid': 'cfg-state-own' },
        el('div', {},
          el('strong', {}, 'Using your own configuration.'),
          ' Changes to the server default do not reach you.',
          // Worth saying: this is the state the old registration bug left every account
          // in, and a saved copy of the default is the one case where following it
          // costs nothing.
          (opts && opts.default_config && effective.trim() === opts.default_config.trim())
            ? ' It is identical to the current default.' : ''),
        el('button', {
          class: 'ghost small', 'data-testid': 'cfg-follow-default',
          onclick: () => {
            if (!confirm('Discard your configuration and follow the server default? '
              + 'Recorded in the audit log; you can customise again at any time.')) return;
            setStoredConfig('');
          },
        }, 'Follow the server default')));

    // Mode.
    // The option text names the value a captured request will SHOW, because these are the
    // same dimension under two names: the config says sync, the request rows say active.
    const modeSel = el('select', { id: 'set-mode', 'data-testid': 'set-mode' },
      el('option', { value: 'sync' }, 'sync — your preset is applied (requests show Mode "active")'),
      el('option', { value: 'observe' }, 'observe — measure only, requests untouched (Mode "observe")'));
    modeSel.value = cfg.mode === 'observe' ? 'observe' : 'sync';
    modeSel.disabled = inherited;
    if (mgr) {
      host.appendChild(el('div', { class: 'field' },
        el('label', { for: 'set-mode' }, term('Mode', 'sync removes text from requests; observe only measures.')), modeSel,
        el('p', { class: 'hint' },
          'Observe is the safe way to try settings: nothing is changed.')));
    }

    // Upstreams, one per dialect, from the operator's allow-list.
    const ups = (opts && opts.upstreams) || [];
    for (const [key, label] of [['up_anthropic', 'Anthropic provider address'],
      ['up_openai', 'OpenAI provider address'], ['up_bob', 'Bob provider address']]) {
      const sel = el('select', { id: 'set-' + key, 'data-testid': 'set-' + key },
        el('option', { value: '' }, '— none —'),
        ...ups.map((u) => el('option', { value: u.name }, `${u.name} (${u.dialect})`)));
      sel.value = t[key] || '';
      host.appendChild(el('div', { class: 'field' }, el('label', { for: 'set-' + key }, label), sel));
    }

    // Components.
    // The id is load-bearing: saveSettings reads the checkboxes back through
    // '#comp-grid input', and without it every save wrote `pipeline: []`.
    const grid = el('div', { id: 'comp-grid', class: 'comp-grid', 'data-testid': 'comp-grid' });
    for (const name of all) {
      const id = 'comp-' + name;
      const cb = el('input', { type: 'checkbox', id, 'data-comp': name, 'data-testid': id });
      cb.checked = active.has(name);
      cb.disabled = cfgDisabled;
      // Enablement is pipeline membership, so this checkbox is what makes a component's
      // fields editable — and unticking it is what clears them on save.
      cb.addEventListener('change', () => drawComps());
      const warn = name === 'extract_llm'
        ? ' — calls a preset model on the request path (+117ms typical, up to ~945ms on file reads) and bills to the shared credential'
        : '';
      grid.appendChild(el('label', { class: 'comp', for: id }, cb,
        el('span', { class: 'comp-name' }, name),
        warn ? el('span', { class: 'comp-warn' }, warn) : null));
    }
    if (mgr) host.appendChild(el('div', { class: 'field' },
      el('label', {}, term('Preset steps', 'Each step removes one kind of waste from requests. Tick the ones you want.')), grid,
      el('p', { class: 'hint' }, 'Ticked steps run in the order shown.'),
      whyBlock('What saving changes',
        'A newly enabled component is appended at the end of the pipeline. Ticking one here ' +
        'is what makes its fields below editable, and unticking one CLEARS the keys it has ' +
        'in your document — a block is configuration, not enablement, so leaving a block ' +
        'behind for a component that does not run is the state nobody can read back. ' +
        'Saving rebuilds your pipeline and discards frozen preset decisions, so the ' +
        'next turn will not be cache-warm.')));

    // Idle keep-alive consent. Its own consent control, beside the transcript one, for the
    // same reason: it is a thing done with the user's property that they have to agree to.
    // Here it is their MONEY rather than their code, and the copy says what it buys, what it
    // costs, and where to see both — a mechanism that spends on its own initiative is only
    // acceptable if the person paying can read the ledger.
    const ka = el('input', { type: 'checkbox', id: 'set-keepalive', 'data-testid': 'set-keepalive' });
    ka.checked = !!(cfg.cache && cfg.cache.keepalive);
    // Editable under the same condition as the rest of the configuration document, because
    // that is where it is STORED: PUT /api/me answers 403 to a non-manager sending `config`,
    // and drawing an enabled box whose value the server discards is worse than not drawing
    // it. The ledger on Overview is visible to everyone regardless, which is the half that
    // matters for someone whose key is being spent.
    ka.disabled = inherited || !mgr;
    if (mgr) host.appendChild(el('div', { class: 'field' },
      el('label', { class: 'comp', for: 'set-keepalive' }, ka,
        el('span', { class: 'comp-name' },
          'Keep my prompt cache warm while I am away')),
      el('p', { class: 'hint' },
        'Sends tiny requests while you are idle so the provider keeps your prompt cached, '
        + 'which is cheaper than rebuilding it. After '
        + (((cfg.cache && cfg.cache.keepalive_idle_seconds) || 240)) + 's idle, up to '
        + (((cfg.cache && cfg.cache.keepalive_max_pings) || 2)) + ' pings. Billed to your own key.'),
      whyBlock('What it costs and what it saves',
        'A cache read costs 0.1x base input (0.05x on Opus 5.5 and Sonnet 5.5, 0.025x on Fable 5.1 and Mythos 5.1); re-creating a lapsed prefix costs 1.25x, so one '
        + 'ping buys back about 11.5 times its own cost at a 0.1x read rate (about 24 times at 0.05x, about 49 times at 0.025x). On this service\u2019s traffic, requests that '
        + 'resumed after the 5-minute window cost 8.5x a request that hit, and they were 23.6% '
        + 'of all spend. It is off by default because it is your money and nobody asked for it '
        + 'to be spent. Every ping is a row on your Requests tab marked cache strategy with its own '
        + 'cost, and the Overview ledger shows pings, ping cost, misses avoided and the net. '
        + 'It also RETAINS, in memory only, your provider key and that session\u2019s last '
        + 'request \u2014 which includes its conversation content \u2014 for the length of the '
        + 'idle gap: up to about 12 minutes on a 5-minute cache, and about 2 hours (at most 4 hours) '
        + 'on a 1-hour cache, because a request sent between your requests has '
        + 'nothing else to authenticate or replay. That is separate from the transcript-storage '
        + 'consent below, which is about writing content to DISK; this is held in memory, '
        + 'masked, overwritten on release, and never persisted. '
        + 'Be aware of the shape: it is a small tax on most of the sessions it touches, funding '
        + 'a large rebate on a few. Measured over 5 days of this service\u2019s traffic, 34 of '
        + '119 pinged sessions came out ahead and the worst single session paid $2.42 for '
        + 'nothing. Watch your own ledger and switch it off if you are not one of the winners.')));

    // The withdrawal, for the principal who cannot tick the box.
    //
    // The box above is drawn only for a manager, because the setting is STORED in the
    // configuration document and that document is manager-owned — so an account owner whose own
    // key was being spent had no way to stop it from this page. Consent withdrawal must never
    // be harder than consent, so turning it OFF is open to any principal on their own account
    // and turning it ON is not. One-way, deliberately.
    if (!mgr && cfg.cache && cfg.cache.keepalive) {
      host.appendChild(el('div', { class: 'field' },
        el('label', {}, 'Keep my prompt cache warm while I am away'),
        el('p', { class: 'hint' },
          'This is ', el('strong', {}, 'on'), ' for your account, set by a manager, and it '
          + 'spends your own key while you are away. You can switch it off here — turning '
          + 'it back on is a manager’s change, because the setting lives in the '
          + 'configuration document. See the Cache strategy tab for what it has cost and avoided '
          + 'on your traffic.'),
        el('div', { class: 'actions' }, el('button', {
          class: 'ghost', 'data-testid': 'set-keepalive-off',
          onclick: async () => {
            if (!confirm('Switch your cache strategy off for your account? Every armed session is '
              + 'disarmed and anything held for you is released immediately.')) return;
            try {
              await ctl('/api/me/keepalive', { method: 'DELETE' });
              alert('Switched off. Nothing further is held or pinged for this account.');
              loadSettings();
            } catch (e) { alert(e.message); }
          },
        }, 'Turn your cache strategy off'))));
    }

    // Content capture consent.
    const cap = el('input', {
      type: 'checkbox', id: 'set-capture', 'data-testid': 'set-capture',
    });
    cap.checked = !!t.capture_content;
    host.appendChild(el('div', { class: 'field' },
      el('label', { class: 'comp', for: 'set-capture' }, cap,
        el('span', { class: 'comp-name' }, 'Store my transcripts for the diff view')),
      el('p', { class: 'hint warn-text' },
        'Writes your agent output to disk. The manager can read what is stored. The ' +
        'redactor is best-effort, not a guarantee.'),
      whyBlock('What "best-effort" means here',
        'Source code and tool results are stored behind a redactor whose own review found ' +
        '11 of 22 realistic credential shapes passing through it. The manager can read ' +
        'whatever this stores. Off by default.')));

    // Trace recording consent. Separate from the box above on purpose: that one keeps
    // capped transcript text for the diff view; this one keeps the full redacted bodies
    // of every call as research traces. Off until the user ticks it; a manager cannot.
    const rec = el('input', {
      type: 'checkbox', id: 'set-record', 'data-testid': 'set-record',
    });
    rec.checked = !!t.record_traces;
    host.appendChild(el('div', { class: 'field' },
      el('label', { class: 'comp', for: 'set-record' }, rec,
        el('span', { class: 'comp-name' }, 'Record my sessions as research traces')),
      el('p', { class: 'hint warn-text' },
        'Writes the full request and response of every call, after redaction, to a ' +
        'directory of your own on this server. Only takes effect if the operator has ' +
        'turned recording on. Only you can change this.'),
      whyBlock('What is removed, and what is kept',
        'Credentials, email addresses and repository URLs are replaced with markers ' +
        'before anything is written. Source code and tool output are kept: a replayable ' +
        'trace needs them, so your name can still appear inside them. Off by default.')));

    // A stored document the server could not fully load. The fields below are a best-effort
    // read of it, and saving from them would post whatever that read happened to see — over
    // an already-broken document, on a page with no YAML box. So say so, and let the server's
    // 409 be the backstop rather than the only guard.
    if (mgr && cfg.parse_error) {
      host.appendChild(el('div', { class: 'state blocked', 'data-testid': 'cfg-unreadable' },
        el('div', { class: 'state-body' },
          el('strong', {}, 'Your stored configuration does not load.'),
          el('span', {}, cfg.parse_error),
          el('span', {}, 'The controls below are a guess at it and saving them is refused. '
            + 'A manager can repair the document on this account\u2019s page under Accounts.'))));
    }

    // Every component's configuration, one <details> each, drawn from /api/options.
    //
    // There is no YAML editor here any more. It was not a convenience, it was the failure:
    // the page rewrote the document with regular expressions, which corrupted any config
    // whose pipeline was written as a block sequence and produced "did not find expected
    // key" on every save, with no way out from the UI. Fields post fields; the server owns
    // the document.
    //
    // And the fields themselves are not written here either. The hand-written version of
    // this form covered 18 keys of 97 and one component of fourteen, and every field on it
    // was a second copy of a fact the server already stated — including a strategy list the
    // engine had outgrown, which silently rewrote a stored value. So the descriptors decide
    // what is drawn, and a knob added to a component appears here with no change to this
    // file.
    if (mgr) {
      host.appendChild(el('div', { class: 'field' },
        el('label', {}, 'Component configuration'),
        el('p', { class: 'hint' },
          'One group of options per step. A field left empty uses that step\'s built-in default.'),
        whyBlock('What happens when you save',
          'These fields are applied to your configuration on the server with a YAML library '
          + 'and the result is built once before it is stored, so a value that would not work '
          + 'is a refusal naming the field rather than a surprise on your next turn. A key you '
          + 'leave empty is REMOVED from the document and the component\u2019s own default '
          + 'takes over. Saving rebuilds your pipeline and discards frozen preset '
          + 'decisions, so the next turn will not be cache-warm.')));
      host.appendChild(compHost);
      drawComps();
    }

    // The whole document, read-only. The fields above are the knobs worth turning from a
    // page; they are not every key a configuration can hold, and an operator asking "what
    // am I actually running" deserves the answer rather than an inference from a form. Not
    // editable: a textarea here is the regex-rewriting save path this page was rebuilt to
    // remove. Anything the fields do not cover is a manager edit on the account page.
    if (effective) {
      host.appendChild(el('details', { class: 'field', 'data-testid': 'full-config' },
        el('summary', {}, 'Full configuration (read-only)'),
        el('p', { class: 'hint' },
          inherited
            ? 'The server default, which your traffic follows because you have stored no '
              + 'configuration of your own.'
            : 'Your stored document, exactly as the proxy builds it. The fields above write '
              + 'into it; every other key here was set by a manager and is left untouched.'),
        el('pre', { class: 'code', 'data-testid': 'full-config-yaml' }, effective)));
    }

    // Save covers the upstreams and the capture consent in both states; it leaves the
    // configuration alone while it is inherited (see saveSettings), so saving one of
    // those does not quietly turn a tracking account into a frozen copy.
    host.appendChild(el('div', { class: 'actions' },
      el('button', { class: 'primary', 'data-testid': 'settings-save', onclick: saveSettings }, 'Save')));
    // What a save DOES to the document, beside the button that does it. The page already warns
    // that saving discards frozen preset decisions; this is the other half, and it is here
    // because an operator lost 36 comment lines to a save and had no way to know they would.
    // Comment and key-order preservation is deliberately not built — it means rewriting the
    // exact code whose two previous rewrites each shipped a data-loss bug — so the honest
    // mitigation is to say so and to keep the prior document one button away.
    host.appendChild(el('p', { class: 'hint', 'data-testid': 'settings-save-warning' },
      'Saving rewrites your configuration document from these fields. ',
      el('strong', {}, 'Comments and key order are not preserved'),
      ' \u2014 the previous document is kept verbatim in the audit log below, with a button to '
      + 'copy it back. A component this page does not draw a control for is left exactly where '
      + 'it is, and a save from a page whose configuration has changed underneath it is refused '
      + 'rather than applied.'));
  });

  loadTokens();
  loadPassword();
  loadMachines();
  loadAudit();
}

/**
 * The password card. Two shapes, because two different things are true:
 *
 *   has_password  — a change form, and the CURRENT password is required. A stolen session
 *                   cookie must not be enough to take the account over and lock its owner
 *                   out of a credential they still know.
 *   no password   — an account older than passwords. There is nothing to check a new one
 *                   against, so the honest answer is the emailed reset, not a form with an
 *                   "old password" box it cannot fill.
 */
function loadPassword() {
  const host = clear($('#password-form'));
  const t = account.tenant;
  if (!t) return;
  const status = $('#password-status');
  status.textContent = '';
  if (!t.has_password) {
    emptyState(host, 'No password on this account',
      'It was created before passwords existed and signs in with a token. Use “Forgot your '
      + 'password?” on the sign-in page to set one by email — that proves the address, '
      + 'which is the only thing there is to prove here.');
    return;
  }
  const old = el('input', {
    type: 'password', id: 'pw-old', autocomplete: 'current-password', 'data-testid': 'pw-old',
  });
  const next = el('input', {
    type: 'password', id: 'pw-new', autocomplete: 'new-password', minlength: '8',
    'data-testid': 'pw-new',
  });
  const err = el('p', { class: 'hint warn-text', role: 'alert', 'data-testid': 'pw-error' });
  host.appendChild(el('div', { class: 'field' },
    el('label', { for: 'pw-old' }, 'Current password'), old));
  host.appendChild(el('div', { class: 'field' },
    el('label', { for: 'pw-new' }, 'New password'), next,
    el('p', { class: 'hint' }, 'At least 8 characters. Your other signed-in machines are '
      + 'signed out; this browser stays in.')));
  host.appendChild(err);
  host.appendChild(el('div', { class: 'actions' },
    el('button', {
      class: 'primary', 'data-testid': 'pw-save',
      onclick: async () => {
        err.textContent = '';
        status.textContent = 'saving…';
        try {
          const out = await ctl('/api/me/password', {
            method: 'POST',
            body: JSON.stringify({ old_password: old.value, new_password: next.value }),
          });
          status.textContent = 'changed';
          err.className = 'hint ok';
          err.textContent = out.note || '';
          loadMachines(); // the other machines are gone; the list must say so
        } catch (e) {
          status.textContent = '';
          err.className = 'hint warn-text';
          err.textContent = e.message;
        } finally {
          // Neither value stays in the DOM after being spent, whatever happened.
          old.value = '';
          next.value = '';
        }
      },
    }, 'Change password')));
}

/**
 * The machines this account is signed in on, each revocable on its own.
 *
 * Named loadMachines, not loadSessions: there was already a top-level loadSessions (the
 * Sessions TAB), and two function declarations with one name means the later one wins for
 * every caller — so `loaders.sessions` and both pager buttons were calling this, painting
 * "Could not list your sessions" into the Settings card and leaving the Sessions table on
 * its loading skeleton forever.
 */
async function loadMachines() {
  const host = clear($('#session-list'));
  let rows = [];
  try {
    rows = (await ctl('/api/me/sessions')).sessions || [];
  } catch (e) { errorState(host, 'Could not list your sessions', e); return; }
  if (!rows.length) { emptyState(host, 'Not signed in anywhere', 'This browser is the only session.'); return; }
  const tbl = el('table', { class: 'grid' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Machine'), el('th', {}, 'Browser'), el('th', {}, 'Address'),
      el('th', {}, 'Signed in'), el('th', {}, 'Last active'),
      el('th', {}, el('span', { class: 'vh' }, 'Row actions')))));
  const body = el('tbody');
  for (const s of rows) {
    body.appendChild(el('tr', {},
      el('td', {}, s.label || '—', s.current ? el('span', { class: 'muted' }, ' (this browser)') : ''),
      el('td', {}, el('span', { class: 'clip' }, s.user_agent || '—')),
      el('td', {}, el('code', {}, s.ip || '—')),
      el('td', {}, when(s.created_at)),
      el('td', {}, when(s.last_seen_at)),
      el('td', {}, el('button', {
        class: 'ghost small', 'data-testid': 'revoke-session-' + s.id,
        onclick: async () => {
          // Revoking the current browser IS signing out, so say so rather than doing it
          // silently and leaving a dashboard that 401s on its next fetch.
          if (!confirm(s.current
            ? 'This is the browser you are using — revoking it signs you out here.'
            : 'Sign this machine out? It will need to sign in again.')) return;
          try {
            await ctl('/api/me/sessions/' + s.id, { method: 'DELETE' });
            if (s.current) { location.reload(); return; }
            loadMachines();
          } catch (e) { alert(e.message); }
        },
      }, s.current ? 'Sign out here' : 'Revoke'))));
  }
  tbl.appendChild(body);
  host.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, tbl));
}

/** Store this configuration as the tenant's own, or '' to go back to following the
 *  server default. One call for both directions: the only difference between owning a
 *  configuration and inheriting one is whether a document is stored. */
async function setStoredConfig(yaml) {
  const status = $('#settings-saved');
  status.textContent = 'saving…';
  try {
    const out = await ctl('/api/me', { method: 'PUT', body: JSON.stringify({ config_yaml: yaml }) });
    account.tenant = out.tenant;
    status.textContent = 'saved';
    loadSettings();
  } catch (e) {
    status.textContent = '';
    alert('Not saved: ' + e.message);
  }
  setTimeout(() => { status.textContent = ''; }, 3000);
}

/**
 * cfgState is what Save posts as `components`: component name → DOTTED key → value, holding
 * ONLY the keys the stored document actually states plus the ones edited on this page.
 *
 * The distinction is the whole contract. An absent key means "the component's own default",
 * which is a DIFFERENT thing from a value — so a key this object does not carry is deleted
 * from the block on save and the component decides again. Inventing a value for a key the
 * document never stated is how a save wrote 20 over a deliberate `llm_max_per_session: 0`.
 * Every control below therefore renders an unstated key as EMPTY with its default as the
 * placeholder, and only writes into this object when somebody changes it.
 *
 * One entry per component with knobs, created on load and kept even when it empties: a name
 * present in `components` is what tells the server to CLEAR that block, so dropping an
 * emptied entry would silently ignore a field the operator just cleared.
 */
let cfgState = null;

/** openComps remembers which component sections are unfolded, so redrawing one (the
 *  recommended button) does not collapse the page under the cursor. */
const openComps = new Set();

/** fieldDefault is what an ABSENT key means: the descriptor's `default`, and when that is
 *  omitted the type's zero — the server omits it exactly when it is the zero value. */
function fieldDefault(fd) {
  if (fd.default !== undefined && fd.default !== null) return fd.default;
  switch (fd.type) {
    case 'bool': return false;
    case 'int': case 'float': return 0;
    case 'strings': return [];
    default: return '';
  }
}

/** fieldText renders a value for a text input. */
function fieldText(fd, v) {
  return fd.type === 'strings' ? (Array.isArray(v) ? v.join(', ') : String(v || '')) : String(v);
}

/* extract_llm used to have TWO passes behind two switches (per_output and
 * cold_cache.enabled) and a constructor that refused both off, so this file carried a
 * special case that flipped one back on. THE COLD SWEEP IS ITS OWN COMPONENT NOW
 * (extract_llm_sweep), extract_llm is unconditionally the warm/tail pass, and both follow
 * the ordinary rule: a component runs when it is ticked in Pipeline components. The special
 * case is gone with the thing it worked around — a checkbox that meant two different things
 * is exactly what the split removed. */

/**
 * renderComponentFields draws ONE component's whole configuration from the descriptors the
 * server serves at /api/options — no field names, types, defaults, enum options or hints in
 * this file at all.
 *
 * That is the point. The hand-written version reached 18 keys of 97, one component of
 * fourteen, and had already drifted: it offered four strategies where the engine parses
 * five, so a stored `deterministic` was not recognised and got rewritten to `code`, quietly
 * turning an LLM-free configuration into one that makes model calls. Anything hand-copied
 * here is a second source of truth for the same fact, which is the bug.
 *
 *   name      component name, used for the ids and for the one coupling below
 *   fields    the descriptors, in declaration order
 *   values    this component's entry in cfgState — MUTATED in place as controls change
 *   disabled  read-only: an inherited configuration, an unreadable one, or a component
 *             that is not in the pipeline
 *   ctx       { recommended, redraw, opts }
 */
function renderComponentFields(name, fields, values, disabled, ctx) {
  const { recommended, redraw, opts } = ctx || {};
  const tid = (key) => 'x-' + name + '-' + key.replace(/\./g, '-');
  const stated = (key) => Object.prototype.hasOwnProperty.call(values, key);
  const set = (key, v) => { values[key] = v; };
  // Deleting, not zeroing: "unset" hands the key back to the component's default, and the
  // server deletes exactly the declared leaf from the document.
  const unset = (key) => { delete values[key]; };
  // Every hint gains what an absent key does, phrased so it stays true whatever the control
  // currently holds — a live "currently unset" note goes stale the moment somebody types.
  const hintOf = (fd) => {
    const def = fieldDefault(fd);
    const shown = fd.type === 'strings' ? (def.length ? fieldText(fd, def) : 'nothing') : String(def);
    return (fd.hint ? fd.hint + ' ' : '') + `Unset means ${shown === '' ? 'empty' : shown}.`;
  };
  const field = (fd, ctl, ...extra) => el('div', { class: 'field cfg-field' },
    el('label', { for: tid(fd.key) }, el('code', {}, fd.key)),
    ctl, el('p', { class: 'hint' }, hintOf(fd)), ...extra);

  // The one place a combination is refused client-side, and it is refused because the
  // component's constructor refuses it. Rather than redraw, the sibling switch is flipped
  // in place and the note says what happened and what to do instead.
  const note = el('p', { class: 'hint warn-text', role: 'status', 'data-testid': 'cfg-note-' + name });
  const effective = (key) => {
    const fd = fields.find((f) => f.key === key);
    return stated(key) ? values[key] : (fd ? fieldDefault(fd) : false);
  };

  const sw = (fd) => {
    const cb = el('input', { type: 'checkbox', id: tid(fd.key), 'data-testid': tid(fd.key) });
    // The default's own value while unstated: a checkbox cannot draw "absent", but leaving
    // it alone still posts nothing, so an untouched form adds no key.
    cb.checked = !!(stated(fd.key) ? values[fd.key] : fieldDefault(fd));
    cb.disabled = disabled;
    cb.addEventListener('change', () => {
      set(fd.key, cb.checked);
      note.textContent = '';
    });
    return el('div', { class: 'field cfg-field' },
      el('label', { class: 'comp', for: tid(fd.key) }, cb, el('span', { class: 'comp-name' }, fd.key)),
      el('p', { class: 'hint' }, hintOf(fd)));
  };

  const numField = (fd) => {
    // min is semantics, not decoration: 0 on a CAP means unlimited and is a legitimate
    // choice, while 0 on a size threshold is not a setting, it is a removed brake — and the
    // server answers 400 for it. Carrying the right min is what stops a user earning that.
    const min = fd.min || 0;
    const inp = el('input', {
      type: 'number', min: String(min), step: fd.type === 'float' ? 'any' : '1',
      id: tid(fd.key), 'data-testid': tid(fd.key), placeholder: String(fieldDefault(fd)),
    });
    inp.value = stated(fd.key) ? String(values[fd.key]) : '';
    inp.disabled = disabled;
    const err = el('p', { class: 'hint warn-text', role: 'status', 'data-testid': tid(fd.key) + '-err' });
    const restore = () => { inp.value = stated(fd.key) ? String(values[fd.key]) : ''; };
    inp.addEventListener('change', () => {
      err.textContent = '';
      const raw = inp.value.trim();
      if (raw === '') { unset(fd.key); return; }
      const v = fd.type === 'float' ? Number(raw) : parseInt(raw, 10);
      if (!Number.isFinite(v)) {
        err.textContent = 'Not a number. Left as it was; clear the box to use the default.';
        restore();
        return;
      }
      if (v < min) {
        err.textContent = min > 0
          ? `Must be at least ${min}: ${v} here is not a setting, it removes the brake — `
            + 'every candidate clears a floor of 0. Left as it was.'
          : 'Cannot be negative. 0 is allowed here and means unlimited.';
        restore();
        return;
      }
      set(fd.key, v);
    });
    return field(fd, inp, err);
  };

  const textField = (fd) => {
    const inp = el('input', {
      type: fd.secret ? 'password' : 'text', id: tid(fd.key), 'data-testid': tid(fd.key),
      autocomplete: 'off', spellcheck: 'false',
      placeholder: fd.secret
        ? 'stored credential kept — type to replace it'
        : (fd.type === 'strings' ? 'comma separated' : String(fieldDefault(fd))),
    });
    // A secret is WRITE-ONLY. The server never reads it back into the form, and this never
    // puts one in the DOM: a value here would be a credential in every screenshot of this
    // page. Empty therefore means "leave the stored credential alone" — never "clear it" —
    // which is the same reading the server has (an absent secret is not a deletion).
    inp.value = fd.secret || !stated(fd.key) ? '' : fieldText(fd, values[fd.key]);
    inp.disabled = disabled;
    inp.addEventListener('change', () => {
      const raw = inp.value.trim();
      if (raw === '') { unset(fd.key); return; }
      set(fd.key, fd.type === 'strings' ? raw.split(',').map((s) => s.trim()).filter(Boolean) : raw);
    });
    return field(fd, inp);
  };

  const pick = (fd) => {
    // The empty option is how an enum gets back to "unset". The server reads an empty enum
    // as the component's default too, but deleting the key is the honest version of it.
    const sel = el('select', { id: tid(fd.key), 'data-testid': tid(fd.key) },
      el('option', { value: '' }, `— default (${fieldDefault(fd)}) —`),
      ...(fd.options || []).map((v) => el('option', { value: v }, v)));
    sel.value = stated(fd.key) ? String(values[fd.key]) : '';
    sel.disabled = disabled;
    sel.addEventListener('change', () => {
      if (sel.value === '') unset(fd.key); else set(fd.key, sel.value);
    });
    // Whether `source: config` can resolve to anything on THIS deployment. It cannot on the
    // hosted service, and a page that offers the choice silently is how an account watched
    // this component run 251 times and make zero model calls.
    const noStatic = fd.key === 'model.source' && opts && opts.compaction_model === false;
    return field(fd, sel, noStatic
      ? el('p', { class: 'hint warn-text' },
        'THIS DEPLOYMENT HAS NO CONFIGURED PRESET MODEL — it deliberately does not spend '
        + 'the operator’s credential on your traffic. So "config" here means the '
        + 'component has no model and never makes a call, however else it is configured.')
      : null);
  };

  const control = (fd) => {
    switch (fd.type) {
      case 'bool': return sw(fd);
      case 'int': case 'float': return numField(fd);
      case 'enum': return pick(fd);
      case 'string': case 'strings': return textField(fd);
      // A type this bundle does not know — the server is newer than the cached page. Shown
      // with no control rather than as a text box: a string posted where the server wants a
      // number is a 400, and inventing a control for an unknown type is guessing.
      default: return el('div', { class: 'field cfg-field' },
        el('label', {}, el('code', {}, fd.key)),
        el('p', { class: 'hint warn-text' },
          `This page does not know the field type “${fd.type}”, so it cannot draw a control `
          + 'for it. Reload; if it persists, the server is newer than this dashboard and a '
          + 'manager can set the key on the account page.'),
        el('p', { class: 'hint' }, hintOf(fd)));
    }
  };

  // A component that reaches a model is the one that can cost more than it saves, and that
  // is read off the descriptors (it has a model block) rather than from a list here.
  const callsModel = fields.some((fd) => fd.key === 'model.source');
  const setCount = fields.filter((fd) => stated(fd.key)).length;
  const det = el('details', { class: 'field comp-fields', 'data-testid': 'cfg-' + name });
  det.open = openComps.has(name);
  det.addEventListener('toggle', () => {
    if (det.open) openComps.add(name); else openComps.delete(name);
  });
  det.appendChild(el('summary', {},
    el('span', { class: 'comp-name' }, name),
    el('span', { class: 'muted' },
      ` — ${setCount} of ${fields.length} key${fields.length === 1 ? '' : 's'} set`
      + (disabled ? ', read-only' : ''))));
  // Why it is locked, when the reason is this component rather than the whole page (an
  // inherited or unreadable configuration is stated once, above, not fourteen times).
  if (ctx && ctx.off) {
    det.appendChild(el('p', { class: 'hint', 'data-testid': 'cfg-off-' + name },
      'Not editable here. Tick it under Pipeline components to configure it — a component '
      + 'that is not in the pipeline does not run, whatever its block says, and switching it '
      + 'off clears the keys below.'));
  }
  if (callsModel) {
    det.appendChild(el('p', { class: 'hint warn-text' },
      'This component calls a model to save tokens, so it can be net negative. Every call is '
      + 'recorded with its cost and its saving — open any request to see them.'));
  }
  det.appendChild(note);
  if (recommended) {
    det.appendChild(el('div', { class: 'actions' },
      el('button', {
        class: 'ghost small', 'data-testid': 'cfg-rec-' + name, disabled: disabled || null,
        onclick: () => { Object.assign(values, recommended); if (redraw) redraw(); },
      }, 'Use recommended values')));
    det.appendChild(el('p', { class: 'hint' },
      'Recommended is not the same thing as default: the defaults below are what an unset '
      + 'key means to the component, this is what we suggest you spend, from our own '
      + 'measurements. It fills the fields in; nothing is saved until you press Save.'));
  }
  for (const fd of fields) det.appendChild(control(fd));
  return det;
}

async function saveSettings() {
  const status = $('#settings-saved');
  status.textContent = 'saving…';
  const inherited = !!account.tenant.config_inherited;
  const prev = (account.tenant.effective_config || {}).pipeline || [];
  // EVERY box in the grid, checked or not: this is what the page drew a control for, which is a
  // different list from what is ticked. It is the claim the server needs in order to tell a
  // deliberate removal from a component this bundle cannot see.
  const drew = $$('#comp-grid input[type=checkbox]').map((c) => c.dataset.comp);
  const picked = new Set($$('#comp-grid input[type=checkbox]')
    .filter((c) => c.checked).map((c) => c.dataset.comp));
  // Keep the configured run order for everything still enabled; a newly ticked component is
  // appended, never inserted, because this grid does not know where in the pipeline it
  // belongs.
  const ordered = prev.filter((n) => picked.has(n))
    .concat([...picked].filter((n) => !prev.includes(n)));
  const body = {
    capture_content: $('#set-capture').checked,
    record_traces: $('#set-record').checked,
    up_anthropic: $('#set-up_anthropic').value,
    up_openai: $('#set-up_openai').value,
    up_bob: $('#set-up_bob').value,
  };
  // Omitted while the configuration is inherited: sending it would store a copy of
  // today's default, which is exactly the freeze this page exists to undo. Customise is
  // the deliberate way to start owning one.
  // Never sent by a plain account: the pipeline is the manager's field, and PUT /api/me
  // answers 403 to anyone else — sending it would fail the whole save, upstreams and
  // capture consent included.
  // Never posted from a form the server told us was a guess: the fields would carry the
  // fallback's reading of a document nobody can see. The server refuses this too (409).
  const unreadable = !!(account.tenant.effective_config || {}).parse_error;
  if (!inherited && isManager() && !unreadable) {
    // components carries, per component, ONLY the dotted keys the form actually holds a
    // value for. A key that is not here is DELETED from the block on the server and the
    // component's own default takes over, which is the same thing an absent key means — so
    // the page must not invent one. A component present with an empty object is how a
    // cleared block is expressed; a component absent from it entirely is left untouched.
    // cache carries the account's own consent, so it is sent whenever the form drew the
    // control — the tuning fields are omitted, which leaves the document's own values (or the
    // defaults) alone rather than freezing today's numbers into every saved document.
    body.config = {
      pipeline: ordered,
      // What this page DREW a control for, and what it drew it FROM. Two lines, and they are
      // what stop a save silently reverting a pipeline. The server preserves anything in the
      // stored document that is absent from BOTH lists — so a component this bundle has never
      // heard of (a cached app.js whose /api/options predates it) cannot be removed by
      // omission — and refuses the save outright when the base disagrees with the stored
      // document, which is what stops a stale render adding one back.
      pipeline_known: drew,
      pipeline_base: prev,
      mode: $('#set-mode').value, components: cfgState || {},
      cache: {
        keepalive: !!($('#set-keepalive') || {}).checked,
        head_ttl_1h: !!((account.tenant.effective_config || {}).cache || {}).head_ttl_1h,
      },
    };
  }
  try {
    const out = await ctl('/api/me', { method: 'PUT', body: JSON.stringify(body) });
    account.tenant = out.tenant;
    status.textContent = 'saved';
    loadSettings();
  } catch (e) {
    status.textContent = '';
    // The server's message names the offending key; showing it beats "invalid config".
    alert('Not saved: ' + e.message);
  }
  setTimeout(() => { status.textContent = ''; }, 3000);
}

async function loadTokens() {
  const host = clear($('#token-list'));
  try {
    const me = await ctl('/api/me');
    account.tokens = me.tokens || [];
  } catch { /* the gate will handle it */ }
  if (!account.tokens.length) { emptyState(host, 'No tokens', 'Mint one to get started.'); return; }
  const tbl = el('table', { class: 'grid' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Token'), el('th', {}, 'Label'),
      el('th', {}, 'Created'), el('th', {}, 'Last used'), el('th', {}, el('span', { class: 'vh' }, 'Row actions')))));
  const body = el('tbody');
  for (const t of account.tokens) {
    const revoked = t.revoked_at > 0;
    body.appendChild(el('tr', { class: revoked ? 'revoked' : '' },
      el('td', {}, el('code', {}, 'cg_live_' + t.prefix + '…')),
      el('td', {}, t.label || '—'),
      el('td', {}, when(t.created_at)),
      // "never" is load-bearing: a token that has never been used is one to suspect
      // was never delivered, or was pasted somewhere it does not belong.
      el('td', {}, t.last_used_at ? when(t.last_used_at) : el('span', { class: 'muted' }, 'never')),
      el('td', {}, revoked
        ? el('span', { class: 'muted' }, 'revoked ' + when(t.revoked_at))
        : el('button', {
          class: 'ghost small', 'data-testid': 'revoke-' + t.prefix,
          onclick: async () => {
            if (!confirm('Revoke this token? Any agent using it stops working immediately.')) return;
            try { await ctl('/api/me/tokens/' + t.prefix, { method: 'DELETE' }); loadTokens(); } catch (e) { alert(e.message); }
          },
        }, 'Revoke'))));
  }
  tbl.appendChild(body);
  host.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, tbl));
}

async function loadAudit() {
  const host = clear($('#audit-list'));
  try {
    const out = await ctl('/api/me/audit');
    const rows = out.audit || [];
    if (!rows.length) { emptyState(host, 'No changes yet', 'Configuration edits are recorded here.'); return; }
    const tbl = el('table', { class: 'grid' },
      el('thead', {}, el('tr', {},
        el('th', {}, 'When'), el('th', {}, 'Field'), el('th', {}, 'From'), el('th', {}, 'To'),
        el('th', {}, el('span', { class: 'vh' }, 'Row actions')))));
    const body = el('tbody');
    for (const e of rows.slice(0, 50)) {
      body.appendChild(el('tr', {},
        el('td', {}, when(e.at)), el('td', {}, el('code', {}, e.field)),
        el('td', { class: 'clip' }, e.before || '—'),
        el('td', { class: 'clip' }, e.after || '—'),
        // The way back. Saving rewrites the configuration document from fields, so comments and
        // key order do not survive it — an operator lost 36 comment lines to a save. The prior
        // document is stored here VERBATIM, so recovering it is a button rather than a support
        // request. No new storage: this row already holds the whole text.
        el('td', {}, e.field === 'config_yaml' && e.before
          ? el('button', {
            class: 'ghost small', 'data-testid': 'audit-copy-before',
            onclick: async () => {
              try {
                await navigator.clipboard.writeText(e.before);
                alert('The document as it was before this save is on your clipboard, comments '
                  + 'and key order included.');
              } catch {
                // A browser that refuses clipboard access still has to give the text up.
                prompt('Copy the document as it was before this save:', e.before);
              }
            },
          }, 'Copy the document as it was')
          : null)));
    }
    tbl.appendChild(body);
    host.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, tbl));
  } catch (e) { errorState(host, 'Could not read the audit log', e); }
}

Object.assign(loaders, { settings: loadSettings });
