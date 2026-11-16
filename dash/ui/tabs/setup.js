// context-guru dashboard: the Setup tab.
// Moved out of app.js (#1089). app.js loads first and holds the shared DOM helpers,
// formatting, state and router that this file uses. See the file list in index.html.
'use strict';

// ── setup ──────────────────────────────────────────────────────────────────
// TOKEN_SLOT is what stands in for a real token in every block on this page when we do
// not have the plaintext. A NAMED slot rather than the account's real prefix plus an
// ellipsis: these blocks exist to be pasted, and a credential fragment in one produces a
// line that silently cannot work.
const TOKEN_SLOT = 'cg_live_YOUR_TOKEN_HERE';

/** claudeSettings is the WHOLE settings a Claude Code user ends up with — not a diff.
 *  A beginner cannot apply a diff. The env block is the routing; the hooks tell the
 *  keep-alive when a session ends (exit, Ctrl-C, closing the terminal, /clear, logout) or is
 *  compacted, so it stops pinging a cache nobody will read. The hook sends only the session
 *  id, event and reason, and a failed post changes nothing. */
function claudeSettings(base, tok) {
  // Only the session id, event name and reason/source leave the machine: the hook picks them out
  // of Claude Code's payload (restricted to id characters, so nothing else can ride along) instead
  // of forwarding it, which also holds the working directory and the transcript path. The token
  // never reaches an argv (/proc/<pid>/cmdline is world-readable): printf is a shell builtin, and
  // sed and curl read it on stdin as curl config lines.
  const signal = String.raw`P=$(cat); f() { printf '%s' "$P" | tr -d '\n' | sed -n "s/.*\"$1\" *: *\"\([A-Za-z0-9._:-]*\)\".*/\1/p"; }; `
    + String.raw`printf '%s\n' "$ANTHROPIC_CUSTOM_HEADERS" | sed 's/^/header = "/; s/$/"/' `
    + String.raw`| curl -s -o /dev/null -m 2 -K - -H 'Content-Type: application/json' `
    + String.raw`--data "{\"session_id\":\"$(f session_id)\",\"hook_event_name\":\"$(f hook_event_name)\",\"reason\":\"$(f reason)\",\"source\":\"$(f source)\"}" `
    + `${base}/signal || true`;
  const hook = (matcher) => [
    `      { ${matcher}"hooks": [ { "type": "command", "command": ${JSON.stringify(signal)}, "timeout": 5 } ] }`,
  ];
  return [
    '{',
    '  "env": {',
    `    "ANTHROPIC_BASE_URL": "${base}/anthropic",`,
    `    "ANTHROPIC_CUSTOM_HEADERS": "x-context-guru-token: ${tok}"`,
    '  },',
    '  "hooks": {',
    '    "SessionEnd": [',
    ...hook(''),
    '    ],',
    '    "SessionStart": [',
    ...hook('"matcher": "compact", '),
    '    ]',
    '  }',
    '}',
  ];
}

// AGENTS covers the agents that are NOT Claude Code — those two are still shell exports,
// because neither reads ~/.claude/settings.json. Claude Code has the numbered walkthrough
// above them instead.
const AGENTS = [
  {
    name: 'Bob (BobShell)',
    path: '/',
    // Bob's client builds every request header itself and offers no hook for another
    // one, so it cannot carry the token. Instead it is recognised by the sha256 of the
    // key it already sends — bound once on the Settings tab, never stored in plaintext.
    //
    // The variable name is version-dependent, and getting it wrong is silent: Bob simply
    // talks to its default gateway and nothing appears here. bobshell 2.x reads
    // BOB_GATEWAY_URL (checked against the 2.0.1 bundle, where CUSTOM_BASE_URL does not
    // appear at all); the older build read CUSTOM_BASE_URL. Both are listed, because a
    // spare export costs nothing and a missing one costs an afternoon.
    lines: (base, tok) => [
      `export BOB_GATEWAY_URL=${base}    # bobshell 2.x — check with: bob --version`,
      `export CUSTOM_BASE_URL=${base}    # older builds read this one instead`,
      '# Your Bob key stays your own (BOB_API_KEY; BOBSHELL_API_KEY still works).',
      '# Bob can send no header of ours, so bind that key once on the Settings tab:',
      '#   Settings → Bound agent keys → paste the key → Bind this key',
    ],
  },
  {
    name: 'OpenAI-dialect tools',
    path: '/openai/v1',
    lines: (base, tok) => [
      `export OPENAI_BASE_URL=${base}/openai/v1`,
      '# OPENAI_API_KEY stays your own provider key; send the token as a header:',
      `#   x-context-guru-token: ${tok}`,
    ],
  },
];

function copyButton(text) {
  return el('button', {
    class: 'ghost small', 'data-testid': 'copy',
    onclick: async (ev) => {
      const b = ev.currentTarget;
      try {
        await navigator.clipboard.writeText(text);
        b.textContent = 'copied';
      } catch {
        // Clipboard access can be refused (insecure origin, permissions). Say so
        // rather than appearing to succeed — the user needs to know to select manually.
        b.textContent = 'select manually';
      }
      setTimeout(() => { b.textContent = 'copy'; }, 1500);
    },
  }, 'copy');
}

// ── logs: Grafana over an SSH tunnel ───────────────────────────────────────
//
// Grafana and Loki bind LOOPBACK on the box, deliberately — the public path serves the
// proxy and this dashboard, nothing else. So the affordance here is copyable TEXT and
// never a hyperlink: an <a href> to 127.0.0.1:3000 is dead for every reader who has not
// already opened a tunnel, and a dead link is worse than instructions. Nothing is
// embedded either: an iframe would mean loosening a CSP that is worth more than the
// convenience.
//
// The address is written WITHOUT a scheme, on purpose as well as for brevity: the offline
// guarantee is enforced by a test that greps every served asset for URL schemes, and an
// asset that must reference no external origin should not start carrying URLs at all.
const GRAFANA_HOSTPORT = '127.0.0.1:3000';
const GRAFANA_LOGS_PATH = '/d/context-guru-logs/context-guru-logs';

/** copyBlock is a titled, copyable code block. Nothing new: it reuses the Setup tab's
 *  copy button and its <pre class="code">. */
function copyBlock(title, lines, testid) {
  const text = lines.join('\n');
  return el('div', { class: 'copyblock' },
    el('div', { class: 'setup-head' },
      el('h3', { text: title }),
      copyButton(text)),
    el('pre', { class: 'code', 'data-testid': testid }, text));
}

/** renderLogsHelp fills the Config tab's Logs panel: the tunnel, the address, and one
 *  LogQL query to paste into it. */
function renderLogsHelp() {
  const host = $('#logs-help');
  if (!host) return;
  clear(host);
  host.appendChild(copyBlock('1 · Open the tunnel from your machine',
    ['ssh -L 3000:' + GRAFANA_HOSTPORT + ' ' + (location.hostname || '<the host>')], 'logs-tunnel'));
  host.appendChild(copyBlock('2 · Open this address in a browser',
    [GRAFANA_HOSTPORT + GRAFANA_LOGS_PATH], 'logs-address'));
  host.appendChild(copyBlock('3 · Or query Loki directly, in Explore',
    ['{job="context-guru"} | json | level=~"WARN|ERROR"'], 'logs-query'));
  host.appendChild(el('p', { class: 'note', text:
    'Grafana and Loki bind loopback on the box, so this is text rather than a link — a ' +
    'link would be dead until the tunnel is up.' }));
}

/** logQueryBlock is the drawer's version: the LogQL that selects THIS session's lines. */
function logQueryBlock(session, tenant) {
  const sel = '{job="context-guru"' + (tenant ? ', tenant="' + tenant + '"' : '') + '}';
  return el('div', {},
    copyBlock('Logs for this session (Grafana → Explore → Loki)',
      [sel + ' | json | session="' + session + '"'], 'logs-session-query'));
}

/** step is one numbered instruction: the number, the sentence, and whatever it needs
 *  pasted underneath. Short on purpose — a step that needs a paragraph is two steps. */
function step(n, title, ...body) {
  return el('div', { class: 'setup-step', 'data-testid': 'setup-step-' + n },
    el('div', { class: 'setup-step-n' }, String(n)),
    el('div', { class: 'setup-step-body' }, el('h3', {}, title), ...body));
}

/** revealToken is the one-time reveal: the plaintext, big, with a copy button and a
 *  warning that cannot be scrolled past. Rendered only when we actually hold the
 *  plaintext, which is only ever the reply to registration or to minting. */
function revealToken(tok) {
  return el('div', { class: 'token-reveal', 'data-testid': 'token-reveal' },
    el('div', { class: 'setup-head' },
      el('h3', {}, 'Your context-guru token'),
      copyButton(tok)),
    el('pre', { class: 'code token-plain', 'data-testid': 'token-plain' }, tok),
    el('p', { class: 'warn-text', 'data-testid': 'token-once' },
      'Shown once. We store only its hash, so nobody — including us — can show it again. '
      + 'Copy it somewhere safe now; if you lose it, mint a new one on Settings.'));
}

function loadSetup() {
  const host = clear($('#setup-blocks'));
  const base = account.baseURL || location.origin;
  // The plaintext only exists at mint time, so a returning user gets the named slot.
  const tok = account.freshToken || TOKEN_SLOT;
  const settings = claudeSettings(base, tok);

  if (account.freshToken) host.appendChild(revealToken(account.freshToken));

  host.appendChild(el('div', { class: 'setup-steps' },
    step(1, 'Open your Claude Code settings file',
      el('pre', { class: 'code' }, '~/.claude/settings.json'),
      el('p', { class: 'hint' }, 'No such file? Create it — an empty file is fine.')),
    step(2, 'Put this in it',
      el('div', { class: 'setup-head' }, el('span', { class: 'hint' },
        'Your token is already filled in.'), copyButton(settings.join('\n'))),
      el('pre', { class: 'code', 'data-testid': 'setup-claude' }, settings.join('\n')),
      el('p', { class: 'hint' },
        'Already have an "env" or "hooks" block? Add just these entries inside it. Leave '
        + 'every other key alone. The hooks tell us when a session ends or is compacted, so '
        + 'your cache strategy stops; they send only the session id, the event name and its '
        + 'reason. Remove the "hooks" block to opt out.')),
    step(3, 'Keep your own key where it is',
      el('p', { class: 'hint' },
        'ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN stays yours: we forward it, so your '
        + 'traffic is billed to you. Without it every request answers 401.')),
    step(4, 'Restart Claude Code, then check this dashboard',
      el('p', { class: 'hint' },
        'Ask it anything. Requests appear on Overview within a second.'))));

  host.appendChild(el('div', { class: 'banner warn', 'data-testid': 'setup-trap' },
    el('div', {}, el('strong', {}, 'Empty dashboard after exporting the variable? '),
      'An "env" block in ~/.claude/settings.json silently overrides an exported '
      + 'ANTHROPIC_BASE_URL. Claude Code answers normally and nothing reaches us. '
      + 'Put the block in that file — step 2 — rather than in your shell.')));

  const others = el('details', { class: 'setup-others' },
    el('summary', {}, 'Other agents (Bob, OpenAI-dialect tools)'));
  for (const a of AGENTS) {
    const lines = a.lines(base, tok);
    others.appendChild(el('div', { class: 'setup-block' },
      el('div', { class: 'setup-head' },
        el('h3', { text: a.name }),
        copyButton(lines.join('\n'))),
      el('pre', { class: 'code' }, lines.join('\n'))));
  }
  host.appendChild(others);

  // The banner above the blocks stays for the returning user's benefit — it is the only
  // thing on the page that explains why step 2 shows a placeholder instead of a token.
  const banner = $('#setup-token-banner');
  banner.hidden = !!account.freshToken;
  if (!account.freshToken) {
    banner.className = 'banner';
    banner.hidden = false;
    banner.textContent = 'Paste your own token over ' + TOKEN_SLOT + ' below. Tokens are '
      + 'shown once, at creation; mint a new one on Settings if you no longer have it.';
  }
}

Object.assign(loaders, { setup: loadSetup });
