# Connect to the IBM service

There is a context-guru instance running for IBM engineers. You build nothing, run no proxy,
and never hand it a provider key: register once, get a token, set two environment variables.

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
| Your provider key | **not needed, and never asked for** |
| Cost control | a monthly spend cap per account, **$50** by default |
| Default pipeline | `[format, toon, dedup, failed_run, cmdfilter, extract, cachesplit]`, `mode: sync` |

The default pipeline is fully deterministic — no model calls anywhere in it — so it adds no
upstream spend and near-zero latency to your agent turn.

## Steps

1. **Register.** Open **<https://contextguru.vpc.cloud9.ibm.com/dashboard/>** and register
   with your IBM email and a label for the token.

    The token is shown **once**. The server keeps only `sha256(token)` and its first 8
    characters, so a lost token is reissued, never recovered. Copy it somewhere safe now.

2. **Check your machine trusts the certificate.** This is the most common first-run failure.

    ```sh
    curl -sS -o /dev/null -w '%{http_code}\n' https://contextguru.vpc.cloud9.ibm.com/healthz
    ```

    `200` and you are done. A certificate error means the IBM internal root CA is missing
    from your trust store — see Troubleshooting below.

3. **Point your agent at it.** One token, three dialects; the path carries the dialect.

    ```bash
    # Claude Code
    export ANTHROPIC_BASE_URL=https://contextguru.vpc.cloud9.ibm.com/anthropic
    export ANTHROPIC_AUTH_TOKEN=cg_live_…

    # Bob  (base URL is the host; Bob appends its own /inference and /admin paths)
    export CUSTOM_BASE_URL=https://contextguru.vpc.cloud9.ibm.com
    export BOBSHELL_DEFAULT_AUTH_TYPE=custom
    export BOBSHELL_API_KEY=cg_live_…

    # OpenAI-dialect tools
    export OPENAI_BASE_URL=https://contextguru.vpc.cloud9.ibm.com/openai/v1
    export OPENAI_API_KEY=cg_live_…
    ```

    The token is accepted in `Authorization`, `x-api-key` or `x-goog-api-key`, and every one
    of those slots is stripped before the request is forwarded upstream.

4. **Confirm the traffic actually arrives.** Open [the dashboard](../dashboard.md) and watch
   your request count move after a full agent turn. An `export` is not proof — for Claude
   Code an `env` block in `~/.claude/settings.json` silently overrides it, and the failure
   looks exactly like success.

## Turn it on or off for one session

Nothing here edits a config file; each recipe affects only the command you run.

```sh
# On, for one Claude Code session
ANTHROPIC_BASE_URL=https://contextguru.vpc.cloud9.ibm.com/anthropic \
ANTHROPIC_AUTH_TOKEN=cg_live_… \
  claude

# On, for one Bob session
CUSTOM_BASE_URL=https://contextguru.vpc.cloud9.ibm.com \
BOBSHELL_DEFAULT_AUTH_TYPE=custom \
BOBSHELL_API_KEY=cg_live_… \
  bob "your task"

# Off — straight to your normal provider
env -u ANTHROPIC_BASE_URL -u ANTHROPIC_AUTH_TOKEN claude
env -u CUSTOM_BASE_URL -u BOBSHELL_DEFAULT_AUTH_TYPE -u BOBSHELL_API_KEY bob "your task"
```

If Claude Code is routed by your `settings.json` rather than by an export, `env -u` changes
nothing — pass a per-invocation settings file instead:

```sh
claude --settings '{"env":{
  "ANTHROPIC_BASE_URL":"https://contextguru.vpc.cloud9.ibm.com/anthropic",
  "ANTHROPIC_AUTH_TOKEN":"cg_live_…"}}'
```

`--settings` accepts a JSON string as well as a path, and is read as additional settings for
that invocation only.

## Keep the metrics, skip the compaction

`x-context-guru-bypass: true` forwards the body byte-identical — no component runs, no expand
tool is injected — while still recording the request, so it appears in your dashboard under
the `bypassed` reason and feeds `upstream_ms_avg_bypassed`, the latency baseline for a
with/without comparison.

```sh
ANTHROPIC_CUSTOM_HEADERS='x-context-guru-bypass: true' \
ANTHROPIC_BASE_URL=https://contextguru.vpc.cloud9.ibm.com/anthropic \
ANTHROPIC_AUTH_TOKEN=cg_live_… \
  claude
```

It is the sharpest way to find out whether context-guru is implicated in a problem at all:
the request still goes through the same host, TLS, nginx and account, so if the problem
survives a bypass, compaction was not the cause. Bob exposes no way to set extra request
headers, so turn it off for the session instead and accept the missing metrics row.

The service also bypasses the agent's own compaction request on its own — see
[When your agent compacts](../how-to/agent-compaction.md).

## Measure without modifying

`observe` mode forwards every request untouched and measures off-path what the pipeline
*would* have saved. Read the `potential_*` numbers, then switch the same config to `sync`.
The switch is on the dashboard's **Settings** page. See
[Sync & observe](../how-to/operating-modes.md).

Mode is per **account**, not per session: there is no header for it, and changing it applies
to every session you run until you change it back. It also rebuilds your pipeline and store,
discarding the frozen compaction decisions your current sessions are replaying.

## When it says no

| Status | What happened |
|---|---|
| **401** | No token, or an unknown/revoked one. |
| **403** | The account is disabled, or self-registration is closed. |
| **402** | Monthly spend cap reached; the body names the figures. Ask a manager to raise the cap, or wait for the month to turn over. |
| **429** | A per-tenant rate or in-flight limit. Worth retrying. |
| **502** | No upstream configured for that route, or the provider failed. The operator's problem. |
| connection refused on `http://` | You used port 80. There deliberately isn't one. |

<details markdown="1">
<summary>Troubleshooting</summary>

**A certificate-verification error.** The certificate is issued by the IBM INTERNAL
INTERMEDIATE CA, chaining to the IBM Internal Root CA. IBM-managed machines already trust it;
containers, fresh VMs and CI runners usually do not. The error often mentions a self-signed
certificate in the chain, which is misleading — the chain is fine, your machine does not know
the root. Install it system-wide:

```sh
# RHEL / Fedora
sudo cp ibm-internal-root-ca.pem /etc/pki/ca-trust/source/anchors/ && sudo update-ca-trust

# Debian / Ubuntu
sudo cp ibm-internal-root-ca.pem /usr/local/share/ca-certificates/ibm-internal-root-ca.crt \
  && sudo update-ca-certificates
```

Or per-process, to fix one tool in a container you do not own:

```sh
export NODE_EXTRA_CA_CERTS=/path/to/ibm-internal-root-ca.pem   # Claude Code, Bob (Node)
export SSL_CERT_FILE=/path/to/ibm-internal-root-ca.pem         # Python / OpenSSL
export REQUESTS_CA_BUNDLE=/path/to/ibm-internal-root-ca.pem    # python-requests
export CURL_CA_BUNDLE=/path/to/ibm-internal-root-ca.pem        # curl
```

The PEM is distributed through IBM's own internal CA channels, not by context-guru, so this
page cannot give you a download URL it can verify. Ask the operator; the deployment keeps a
readable copy at `/etc/context-guru/ibm-internal-root-ca.pem`.

**`http://` fails to connect.** There is no port 80 on purpose. Every request carries your
`cg_live_…` token in a header, and a `301` to `https://` arrives only *after* the client has
already put that token on the wire in cleartext. Failing loudly is the only outcome that keeps
the token off the network.

**Registration is refused with a `403`.** Self-registration is either `closed` (ask the
operator to issue you an account) or `invite` (ask for the invite code and enter it in the
form). See [Choose how accounts are created](../hosted.md#3-choose-how-accounts-are-created).

**Zero requests in the dashboard, but the agent works fine.** Something else is choosing the
base URL. The honest test is a negative one — point the client somewhere unroutable and
confirm it *fails*:

```sh
ANTHROPIC_BASE_URL=https://127.0.0.1:1/nope ANTHROPIC_AUTH_TOKEN=bogus \
  claude -p 'say PONG' --max-turns 1        # must FAIL

CUSTOM_BASE_URL=https://127.0.0.1:1 BOBSHELL_DEFAULT_AUTH_TYPE=custom \
  BOBSHELL_API_KEY=bogus bob "say PONG"     # must FAIL
```

If Claude Code still prints `PONG`, check for an `env` block and use the `--settings` form
above:

```sh
python3 -c "import json;print(json.load(open('$HOME/.claude/settings.json')).get('env',{}).keys())"
```

**Bob ignores `BOBSHELL_API_KEY`.** It also needs `BOBSHELL_DEFAULT_AUTH_TYPE=custom` — see
[Host adapters](../integrations.md#use-it-with-an-agent-bob-bobshell).

**I want to change my config from a script.** The control plane is cookie-authenticated on
purpose: a proxy token buys inference, it does not administer the account. Two calls:

```sh
jar=$(mktemp)
printf '{"token":"%s"}' "$CG_TOKEN" \
  | curl -sS -X POST https://contextguru.vpc.cloud9.ibm.com/api/login \
      -H 'Content-Type: application/json' --data @- -c "$jar" -o /dev/null

curl -sS -b "$jar" https://contextguru.vpc.cloud9.ibm.com/api/me | python3 -m json.tool

curl -sS -b "$jar" -X PUT https://contextguru.vpc.cloud9.ibm.com/api/me \
  -H 'Content-Type: application/json' \
  -d '{"config_yaml":"mode: observe\n"}'
```

The token travels in a request body, not a URL, so it stays out of nginx's access log, and
`--data @-` keeps it out of `ps(1)`.

**My config stopped tracking the server default.** A new account resolves the server default
live on every request, so operator improvements reach you on your next turn. Saving your own
document — including just to flip the mode — stops that until you press **Follow the server
default** again. See
[Tenants track the default](../hosted.md#tenants-track-the-default-they-are-not-stamped-with-a-copy-of-it).

</details>

## Two things worth knowing

**It fails open.** Any component error reverts that component only and the original request
is forwarded. Compaction going wrong costs you savings, not a turn.

**The box is a single point of failure for everyone's agent.** The escape hatch is above:
unset the base URL and your agent goes straight to the provider.

See also: [Use with Claude Code](../how-to/use-with-claude-code.md) ·
[Routes & headers](../reference/routes.md#per-request-headers) ·
[Dashboard](../dashboard.md) · [Running the service](../hosted.md)
