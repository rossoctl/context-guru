# Connect to the IBM service

There is a context-guru instance running for IBM engineers. You do not build anything, you
do not run a proxy, and you do not hand it a provider key — you register once, get a token,
and set two environment variables per agent.

```
your agent ──▶ https://contextguru.vpc.cloud9.ibm.com ──▶ the model gateway
                (rewrites `messages`, then forwards)        (the operator's credential)
                        │
                        └──▶ your own dashboard: savings, sessions, before/after diffs
```

| | |
|---|---|
| Host | `contextguru.vpc.cloud9.ibm.com`, **HTTPS on 443 only** |
| Reachable from | IBM-internal networks (`9.0.0.0/8`) |
| Credential | one service-issued token, `cg_live_` + 26 characters |
| Your provider key | **stays yours** — your agent keeps sending it, and the proxy forwards it upstream unchanged |
| Cost control | none needed: every account's traffic is billed to that account's own provider credential |
| Transcript capture | your account consents **on registration** — [what that means, and the off switch](#three-things-worth-knowing-before-you-rely-on-it) |
| Default pipeline | `[format, toon, dedup, failed_run, cmdfilter, extract, cachesplit]`, `mode: sync` |

The default pipeline is **fully deterministic** — no cheap-model calls anywhere in it. That
is why it is the default on a shared box: it adds no upstream spend, contends for no shared
model budget, and puts near-zero latency on your agent turn.

!!! warning "There is no port 80, on purpose"
    A mistyped `http://contextguru.vpc.cloud9.ibm.com` **fails to connect**, and that is the
    designed behaviour rather than a gap. Every request carries your `cg_live_…` token AND
    your own provider key in headers; a `301` to `https://` arrives *after* the client has
    already put both on the wire in cleartext, and a redirect cannot retract a credential
    that has been sent. Failing loudly is the only outcome that keeps them off the network.

## 1. Register

Open **<https://contextguru.vpc.cloud9.ibm.com/dashboard/>**, register with your IBM email
address, a password of at least 12 characters, and a label for the token. We mail a 6-digit
code to that address; entering it within 5 minutes is what creates the account. Signing in
later is that same password plus a fresh mailed code.

The token is shown **once**, after the code. The server keeps only `sha256(token)` and its first 8
characters (for display and revocation), so there is no code path that can print it back to
you and a lost token has to be reissued, not recovered. Copy it somewhere safe now.

!!! note "If registration is refused"
    Self-registration has three modes, and the operator picks one. A `403` means it is
    `closed` (ask the operator to reissue you an account) or `invite` (ask for the invite
    code and enter it in the form). See
    [Choose how accounts are created](../hosted.md#4-choose-how-accounts-are-created).

## 2. Make sure your machine trusts the certificate

The certificate is issued by the **IBM INTERNAL INTERMEDIATE CA**, chaining to the **IBM
Internal Root CA**. Any IBM-managed machine already trusts that root and there is nothing
to do. A container, a fresh VM, or a CI runner usually does not — and this is the most
common first-run failure, so check before anything else:

```sh
curl -sS -o /dev/null -w '%{http_code}\n' https://contextguru.vpc.cloud9.ibm.com/healthz
```

`200` means you are fine. A certificate-verification error means the trust store is missing
the IBM root, and every agent on that machine will fail the same way — usually with a
message about a self-signed certificate in the chain, which is misleading: the chain is
fine, your machine simply does not know the root.

Install the root CA system-wide:

=== "RHEL / Fedora"

    ```sh
    sudo cp ibm-internal-root-ca.pem /etc/pki/ca-trust/source/anchors/
    sudo update-ca-trust
    ```

=== "Debian / Ubuntu"

    ```sh
    sudo cp ibm-internal-root-ca.pem /usr/local/share/ca-certificates/ibm-internal-root-ca.crt
    sudo update-ca-certificates
    ```

=== "One tool only"

    ```sh
    export NODE_EXTRA_CA_CERTS=/path/to/ibm-internal-root-ca.pem   # Claude Code, Bob (Node)
    export SSL_CERT_FILE=/path/to/ibm-internal-root-ca.pem         # Python / OpenSSL
    export REQUESTS_CA_BUNDLE=/path/to/ibm-internal-root-ca.pem    # python-requests
    export CURL_CA_BUNDLE=/path/to/ibm-internal-root-ca.pem        # curl
    ```

    Per-process, so it fixes one agent and leaves the machine's trust store alone. Useful in
    a container you do not own.

!!! note "Where the root CA PEM comes from is not in this repo"
    The certificate is distributed through IBM's own internal CA channels, not by
    context-guru — this page cannot tell you a download URL it can verify. Ask the operator;
    the deployment keeps a readable copy at `/etc/context-guru/ibm-internal-root-ca.pem`,
    which is the path
    [`deploy/service/tls-smoke.sh`](https://github.com/rossoctl/context-guru/blob/main/deploy/service/tls-smoke.sh)
    defaults to.

## 3. Point your agent at it

One token, three dialects. The path carries the dialect; your account's settings decide
which upstream each dialect goes to.

**Leave your provider key exactly where it is.** The service forwards it, so your traffic
is billed to your own account. The context-guru token travels in its own header,
`x-context-guru-token`, so it never competes for the slot your key occupies.

```bash
# Claude Code — ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN stay YOUR key
export ANTHROPIC_BASE_URL=https://contextguru.vpc.cloud9.ibm.com/anthropic
export ANTHROPIC_CUSTOM_HEADERS="x-context-guru-token: cg_live_…"

# OpenAI-dialect tools — OPENAI_API_KEY stays your key; send the header
#   x-context-guru-token: cg_live_…
export OPENAI_BASE_URL=https://contextguru.vpc.cloud9.ibm.com/openai/v1

# Bob — BOBSHELL_API_KEY stays your key, and Bob can send no header of ours, so bind
# that key to your account once (sha256 only; the key itself is never stored)
export CUSTOM_BASE_URL=https://contextguru.vpc.cloud9.ibm.com
curl -sS -XPOST https://contextguru.vpc.cloud9.ibm.com/api/me/agent-key \
  -H "Authorization: Bearer $BOBSHELL_API_KEY" -b "cg_dash=<your dashboard cookie>"
```

The token is read from `x-context-guru-token` first. It is still accepted in
`Authorization`, `x-api-key` or `x-goog-api-key` for tools that have nowhere else to put
it — recognised by its `cg_live_` shape, and scrubbed out before the request is forwarded —
but a slot holding the token cannot also hold your provider key, so prefer the header.

Bob may also need `BOBSHELL_DEFAULT_AUTH_TYPE=custom` for it to use `BOBSHELL_API_KEY` at
all — see [Host adapters](../integrations.md#use-it-with-an-agent-bob-bobshell).

!!! warning "An `export` is not proof the traffic arrives"
    For Claude Code an `env` block in `~/.claude/settings.json` **silently overrides the
    variable you exported**. The failure looks exactly like success: Claude Code answers
    normally, nothing errors, and the only symptom is an empty dashboard and zero savings,
    because nothing ever reached the service. This has already caught us on this deployment.
    Run the check in [Is it actually on?](#is-it-actually-on) before you trust a number.

## Turn it on for one session only

Nothing below edits a config file. Each recipe affects exactly the command you run, so
another terminal, another repo and another agent are untouched.

### Claude Code

```sh
# One command, one session. Your own key stays in ANTHROPIC_API_KEY.
ANTHROPIC_BASE_URL=https://contextguru.vpc.cloud9.ibm.com/anthropic \
ANTHROPIC_CUSTOM_HEADERS='x-context-guru-token: cg_live_…' \
  claude
```

If `~/.claude/settings.json` has an `env` block, the line above does **nothing** — the
settings file wins. `--settings` takes a JSON string as well as a path, and it is read as
*additional* settings for this invocation only, so it overrides the global file without
touching it:

```sh
claude --settings '{"env":{
  "ANTHROPIC_BASE_URL":"https://contextguru.vpc.cloud9.ibm.com/anthropic",
  "ANTHROPIC_CUSTOM_HEADERS":"x-context-guru-token: cg_live_…"}}'
```

Verified on Claude Code 2.1.215: with a global `env` block present, the `--settings` form
sent `POST /v1/messages` to the URL named there, and the exported variable alone did not.

### Bob

```sh
CUSTOM_BASE_URL=https://contextguru.vpc.cloud9.ibm.com \
  bob "your task"
```

Bob keeps its own `BOBSHELL_API_KEY`. Because it can carry no header of ours, it is
identified by the sha256 of that key — bind it once with the `curl` above, and rebind
whenever you rotate the key.

Bob's base URL is the **host**; Bob appends its own `/inference/…` and `/admin/…` paths, and
the proxy passes its control-plane calls through verbatim so the CLI still boots.

### Or one shell, then any number of commands

```sh
cg-on()  { export ANTHROPIC_BASE_URL=https://contextguru.vpc.cloud9.ibm.com/anthropic \
                  ANTHROPIC_CUSTOM_HEADERS="x-context-guru-token: $CG_TOKEN" \
                  CUSTOM_BASE_URL=https://contextguru.vpc.cloud9.ibm.com; }
cg-off() { unset ANTHROPIC_BASE_URL ANTHROPIC_CUSTOM_HEADERS CUSTOM_BASE_URL; }
```

Keep `CG_TOKEN` in your own secret store, not in `.bashrc`. These are a convenience over
the raw commands above, not a different mechanism — and `cg-on` does not help Claude Code on
a machine whose `settings.json` sets `ANTHROPIC_BASE_URL`.

## Turn it off for one session only

```sh
# Claude Code — straight to whatever provider your normal config uses.
env -u ANTHROPIC_BASE_URL -u ANTHROPIC_CUSTOM_HEADERS claude

# Bob — back to its own endpoint.
env -u CUSTOM_BASE_URL bob "your task"
```

`env -u` only removes an *environment* variable. If Claude Code is routed through the
service by your `settings.json` rather than by an export, unsetting the variable changes
nothing — point it back at the provider explicitly for the one invocation instead:

```sh
claude --settings '{"env":{"ANTHROPIC_BASE_URL":"https://your-normal-gateway.example"}}'
```

## Keep the metrics, skip the compaction

`x-context-guru-bypass: true` skips the pipeline **for that request only**: the body is
forwarded byte-identical, no component runs, no expand tool is injected — and the request is
still recorded, so it appears in your dashboard under the `bypassed` reason and feeds
`upstream_ms_avg_bypassed`, the latency baseline for a with/without comparison.

Reach for it when you are debugging whether context-guru is implicated in a problem at all.
It is a sharper instrument than switching the base URL off, because the request still goes
through the same host, the same TLS, the same nginx and the same account — so if the problem
survives a bypass, compaction was never the cause.

```sh
# Claude Code — verified on 2.1.215: these headers reach the wire. Several pairs are
# newline-separated, which is how the token and the bypass travel together.
ANTHROPIC_CUSTOM_HEADERS='x-context-guru-token: cg_live_…
x-context-guru-bypass: true' \
ANTHROPIC_BASE_URL=https://contextguru.vpc.cloud9.ibm.com/anthropic \
  claude
```

!!! note "No equivalent for Bob"
    Bob 1.0.6 exposes no environment variable for extra request headers — its client
    builds `Content-Type`, `User-Agent`, `Authorization`, `x-instance-id` and `x-team-id`
    itself, its `headers` setting applies to MCP servers only, and the header-ish knobs in
    its bundle are `CUSTOM_BASE_URL`, `CUSTOM_TIMEOUT` and the `BOBSHELL_*` auth set. That
    is also why Bob is identified by its key digest rather than a token header. So for Bob,
    use
    [turn it off for one session](#turn-it-off-for-one-session-only) instead, and accept
    that you lose the metrics row for those requests.

The proxy also bypasses one thing on its own: **the agent's own compaction request**.
Compacting the request that asks for a summary would destroy the content the summary is
meant to carry verbatim, and that loss is unrecoverable once the summary replaces the
transcript. It shows up in `/stats` as `components.bypass.gates.agent_compaction` rather
than happening silently.

## Measure without modifying — `observe` mode

`observe` forwards every request **untouched, byte for byte**, and measures off-path what
this pipeline *would* have saved. It is the safe way to try a configuration: read the
`potential_*` numbers, then switch the same config to `sync`. See
[Operating modes](../how-to/operating-modes.md).

!!! warning "Mode is per **account**, not per session"
    There is no header and no environment variable that switches mode for one session — the
    only per-request overrides are `x-context-guru-session`, `x-context-guru-bypass` and
    `x-context-guru-pipeline`. Changing to `observe` changes it for **every** session you
    run until you change it back, and it also rebuilds your pipeline and store, which
    discards the frozen compaction decisions your current sessions are replaying.

The button is on the dashboard's **Settings** page, which is the way to do this. If you need
it scripted, the control plane is cookie-authenticated on purpose — a proxy token buys
inference, it does not administer the account — so it is two calls:

```sh
jar=$(mktemp)
printf '{"token":"%s"}' "$CG_TOKEN" \
  | curl -sS -X POST https://contextguru.vpc.cloud9.ibm.com/api/login \
      -H 'Content-Type: application/json' --data @- -c "$jar" -o /dev/null

# Read what you are running now: `effective_config_yaml`, and `config_inherited`.
curl -sS -b "$jar" https://contextguru.vpc.cloud9.ibm.com/api/me | python3 -m json.tool

# Store that same document with mode: observe.
curl -sS -b "$jar" -X PUT https://contextguru.vpc.cloud9.ibm.com/api/me \
  -H 'Content-Type: application/json' \
  -d '{"config_yaml":"pipeline: [format, toon, dedup, failed_run, cmdfilter, extract, cachesplit]\ncomponents:\n  extract:\n    min_tokens: 400\nmode: observe\n"}'
```

The token travels in a request **body**, not a URL, so it stays out of nginx's access log,
and `--data @-` keeps it off the command line and out of `ps(1)`.

!!! warning "Storing a config opts you out of improvements to the default"
    A new account **tracks** the server default — an empty stored document, resolved live on
    every request — so when the operator improves the default you get it on your next turn.
    Saving your own document (including just to flip the mode) stops that until you press
    **Follow the server default** again. See
    [Tenants track the default](../hosted.md#tenants-track-the-default-they-are-not-stamped-with-a-copy-of-it).

## Is it actually on?

Because the silent-override failure looks identical to success, the honest test is a
**negative** one: point the client somewhere unroutable and confirm it *fails*.

=== "Claude Code"

    ```sh
    ANTHROPIC_BASE_URL=https://127.0.0.1:1/nope ANTHROPIC_AUTH_TOKEN=bogus \
      claude -p 'say PONG' --max-turns 1        # must FAIL
    ```

    If that prints `PONG`, your environment variable is being ignored — something else is
    choosing the base URL, almost always an `env` block in `~/.claude/settings.json`:

    ```sh
    python3 -c "import json;print(json.load(open('$HOME/.claude/settings.json')).get('env',{}).keys())"
    ```

    Then use the `--settings` form above. Full diagnosis and both fixes:
    [Use with Claude Code](../how-to/use-with-claude-code.md#steps).

=== "Bob"

    ```sh
    CUSTOM_BASE_URL=https://127.0.0.1:1 BOBSHELL_DEFAULT_AUTH_TYPE=custom \
    BOBSHELL_API_KEY=bogus bob "say PONG"      # must FAIL
    ```

Then verify **positively**, which is the only proof that counts: open
[the dashboard](../dashboard.md) and watch your own request count move. Zero requests after
a full agent turn means the traffic never arrived, whatever your shell says.

## When it says no

| Status | What happened |
|---|---|
| **401** | No token (or an unknown/revoked one), **or no provider credential of your own**. The service never falls back to somebody else's key. Nothing is treated as an anonymous account. |
| **403** | The account is disabled, or self-registration is closed. |
| **429** | A per-tenant rate or in-flight limit. This one *is* worth retrying. |
| **502** | No upstream configured for that route, or the provider failed. The operator's problem. |
| connection refused on `http://` | You used port 80. There deliberately isn't one — see the warning at the top of this page. |

## Three things worth knowing before you rely on it

**Your account consents to transcript capture the moment it is created.** Two independent
switches decide whether your message content is stored, and only one of them starts closed:
the operator's service-wide switch, and your account's own consent — which registration turns
**on** for you. So if the operator has capture enabled, then from your very first request the
before/after text of your messages — agent output, tool results, source code — is written to
the service's database, scrubbed of known credential shapes and capped at 16 KB per message.
It is what makes the diff view work, and only your own account can read yours: a manager sees
everyone's metrics and nobody's transcripts. The scrubber is a pattern denylist, so treat this
as storage you have agreed to, not a guarantee.

**Check which state you are in, and turn it off if you do not want it.** Open a request in the
dashboard: `content_captured` is the effective answer for your account, and
`capture_blocked_by` names whichever switch is closed (`"operator"`, `"tenant"`, or `""` when
nothing is blocking and your content *is* being stored). To turn it off for yourself, clear the
transcript-capture consent on the **Settings** tab — metrics and savings keep working, you lose
the diff view. An operator turns it off for everyone with `DASHBOARD_CONTENT=false`. Either
switch alone stops the writes, neither is retroactive in either direction, and a proxy you run
yourself from this repository ships with the operator's switch **off**.

**It fails open.** Any component error or panic reverts that component only, and the
original request is always forwarded as a valid fallback. Compaction going wrong costs you
savings, not a turn.

**The box is a single point of failure for everyone's agent.** That is the real cost of the
design, and it is stated here rather than left to be discovered during an outage. The escape
hatch is the one above: unset the base URL and your agent goes straight to the provider.

See also: [Use with Claude Code](../how-to/use-with-claude-code.md) ·
[Routes & headers](../reference/routes.md#per-request-headers) ·
[Dashboard](../dashboard.md) · [Running the service](../hosted.md)
