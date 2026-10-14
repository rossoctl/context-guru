# Test the proxy against a Claude Pro/Max subscription, in an isolated sandbox

A runbook for re-verifying that context-guru works correctly for a **subscription (OAuth)
login** — the documented no-API-key setup in
[Install the plugin: You do not need an API key](install-plugin.md#you-do-not-need-an-api-key) —
without touching the machine's real install, real credentials, or real proxies.

Write this up because the first time it was done
([issue #391](https://github.com/rossoctl/context-guru/issues/391),
[PR #392](https://github.com/rossoctl/context-guru/pull/392)) it took far longer than it should
have, almost entirely because of sandbox-isolation mistakes rather than anything about the actual
bug. Every pitfall below was hit for real in that session. Re-run this whenever a change could
plausibly affect the subscription path: anything touching `proxy/proxy.go`'s request/response
handling, `proxy/keepalive.go`, header forwarding, or the plugin's install/route scripts.

## Why this needs a sandbox at all

A subscription login lives in the same `~/.claude` credential store (file or macOS Keychain) as
whatever account is already logged in for day-to-day use. Testing against it for real, without
isolation, risks: switching the real session's active account, routing real traffic through a
throwaway test proxy, or upgrading the real proxy binary as a side effect of testing an update.
None of that is reversible by apologizing afterward. `CLAUDE_CONFIG_DIR` is the one environment
variable that isolates all of it at once — the credentials file **and** the macOS Keychain entry
are keyed to it, so a sandboxed login and the real one never collide.

## Prerequisites

- A Claude Pro or Max account separate from whatever account the operator's real Claude Code
  session uses day to day (testing against the *same* account works too, but a separate one makes
  it obvious which traffic is which).
- The operator present to run `/login` interactively — there is no headless subscription login,
  and the agent running this test should never read the resulting credential file itself (ask,
  don't grab).
- Know what else is already running on this machine first:

  ```sh
  pgrep -fl 'context-guru-prox[y]'          # every real proxy, by exact PID
  env | grep -E '^ANTHROPIC_'               # what this shell already exports — see Pitfall 1
  ```

  Pick a port range and a `CONTEXT_GURU_STATE` directory that don't overlap anything printed
  above. Everything below assumes `/tmp/cg-sub-sandbox` and port `8899`; change both if either is
  taken.

## Step 1 — build the sandbox and seed the binary

```sh
SANDBOX=/tmp/cg-sub-sandbox
rm -rf "$SANDBOX"
mkdir -p "$SANDBOX/bin" "$SANDBOX/state" "$SANDBOX/claude-home" "$SANDBOX/project"

# Isolated binary download — does NOT touch ~/.local/bin. Pin a version, or omit
# CONTEXT_GURU_VERSION for latest.
CONTEXT_GURU_DEST="$SANDBOX/bin" CONTEXT_GURU_VERSION=v0.4.1 \
  bash context-guru-plugin/scripts/install.sh
"$SANDBOX/bin/context-guru-proxy" --version
```

> **Pitfall 1 — the install banner is misleading, but the write is scoped correctly.**
> `install.sh`'s "already installed" check runs `command -v context-guru-proxy` against `PATH`,
> not `$CONTEXT_GURU_DEST` — so its first few output lines (`path=`, `version=`,
> `note=upgrading from …`) describe the **real** `~/.local/bin/context-guru-proxy`, even when the
> actual download and `install -m 755 … && mv -f …` only ever write to `$CONTEXT_GURU_DEST`. Don't
> read those lines as "it touched the real binary" — verify independently instead:
>
> ```sh
> md5 "$SANDBOX/bin/context-guru-proxy" /Users/<you>/.local/bin/context-guru-proxy
> ```

```sh
cat > "$SANDBOX/claude-home/settings.json" <<'EOF'
{
  "env": { "ANTHROPIC_BASE_URL": "http://127.0.0.1:8899/anthropic" }
}
EOF
```

> **Pitfall 2 — exporting `ANTHROPIC_BASE_URL` in the shell does nothing.** Claude Code's own
> `settings.json` `env` block **overrides the process environment entirely** — this is the same
> trap documented in
> [Use with Claude Code: Troubleshooting](use-with-claude-code.md#keep-the-api-key-out-of-claude-code).
> The base URL has to be written into the file under `CLAUDE_CONFIG_DIR`, not exported.

## Step 2 — get the subscription credential in, without reading it

Ask the operator to run this **themselves**, in their own terminal:

```sh
CLAUDE_CONFIG_DIR=/tmp/cg-sub-sandbox/claude-home claude
```

…then `/login`, choosing the subscription account. `/status` inside that session confirms which
account landed (`Login method: Claude Pro account`, with the email). The agent running this test
should never need to open the resulting credentials file — the isolation guarantee is that
`CLAUDE_CONFIG_DIR` keys both the file *and* the Keychain entry, so this never touches the
operator's real login.

## Step 3 — start a standalone proxy (no plugin) for the proxy-layer tests

Point straight at `api.anthropic.com`, not whatever gateway the operator's real setup uses — a
subscription token is meaningless against a company gateway, and this is the whole reason the bug
in #391 existed: it never reproduced against a gateway that doesn't gzip its responses.

```sh
env -i PATH="$PATH" HOME="$HOME" \
  "$SANDBOX/bin/context-guru-proxy" \
    --listen 127.0.0.1:8899 \
    --idle-exit=0 \
    --anthropic-upstream https://api.anthropic.com \
    --dashboard --dashboard-db "$SANDBOX/state/dashboard.db" \
    --preset off \
  >"$SANDBOX/state/proxy.log" 2>&1 &
disown
```

`env -i` here is deliberate: it strips the shell's own exported vars so the proxy process can't
inherit a stray `ANTHROPIC_API_KEY` — see Pitfall 3.

Every real turn after this needs the same env scrubbed, every time:

```sh
cd "$SANDBOX/project"
env -u ANTHROPIC_AUTH_TOKEN -u ANTHROPIC_API_KEY -u ANTHROPIC_CUSTOM_HEADERS \
    -u ANTHROPIC_BASE_URL -u ANTHROPIC_UPSTREAM \
  CLAUDE_CONFIG_DIR="$SANDBOX/claude-home" \
  timeout 100 claude -p 'Reply with exactly: PING' --max-turns 1 --permission-mode bypassPermissions
```

> **Pitfall 3 — the operator's own shell almost certainly has
> `ANTHROPIC_AUTH_TOKEN`/`ANTHROPIC_API_KEY`/`ANTHROPIC_UPSTREAM` set**, pointing at whatever
> gateway or key their day-to-day Claude Code session uses. Three distinct failure shapes this
> causes if any of these leak through:
> - Into `claude -p`: Claude Code's auth precedence ranks `ANTHROPIC_AUTH_TOKEN` **above** the
>   subscription login, so the sandbox silently tests the wrong credential against the wrong
>   upstream. (It still "works" — that's what makes it dangerous. You get a real 401, correctly
>   forwarded, and can mistake it for a proxy bug.)
> - Into the proxy's own env: a non-empty `ANTHROPIC_API_KEY` flips the proxy into gateway mode,
>   which **deletes** the caller's `Authorization` header and injects `x-api-key` instead
>   (`setUpstreamAuth`, `proxy/proxy.go`) — exactly backwards from what a subscription test needs.
> - Into `install.sh --route`: the installer's own conflict check reads the live
>   `ANTHROPIC_BASE_URL`, so a leaked real value makes it refuse with `base_url_already_set`
>   against a URL you never set yourself, which reads like a bug in the installer and isn't one.
>
> Strip all five every time, as shown above. Don't rely on having done it once earlier in the
> session.

> **Pitfall 4 — the "always auto-update" preference can upgrade the REAL binary mid-session.**
> If `/context-guru:update`'s "always" answer gets recorded during a session that also still has
> the real proxy's `SessionStart` hook firing, the real `~/.local/bin/context-guru-proxy` can be
> silently upgraded in the background — independent of anything the sandbox is doing. It's
> harmless (already-running processes keep their old binary in memory until restarted), but don't
> be surprised by it, and don't attribute it to the sandbox work.

## Step 4 — verify cache write and cache read for real

One cold turn, then one more in the *same* Claude Code session (`--continue`, same working
directory):

```sh
# turn 1 — cold
claude -p 'Reply with exactly: WRITE1' --max-turns 1 ...        # cache_write > 0, cache_read likely 0

# turn 2 — same session
claude -p --continue 'Reply with exactly: READ1' --max-turns 1 ...   # cache_read should equal turn 1's cache_write
```

Read the proxy's own request log (`grep cg.request`) rather than trusting a summary — it prints
`cache_read=`/`cache_write=`/`usage_reported=` per request.

> **Pitfall 5 — `cache_read > 0` on a session's very first request doesn't mean context-guru's
> keep-alive did anything.** Anthropic's own server-side prompt cache is keyed by content hash,
> not by which local proxy process or Claude Code session sent the request — so a brand-new
> sandbox proxy's first-ever request can land a real cache hit purely because an *unrelated*
> earlier session (even through a different proxy instance) wrote the same system-prompt prefix
> within the TTL window. This is correct, expected Anthropic-side behavior and genuinely saves
> money, but it is **not** evidence that *this proxy's* keep-alive mechanism fired. Only
> `keepalive.pings > 0` plus a positive `keepalive_saved_usd` is that evidence — see Step 5.

## Step 5 — verify keep-alive for real (the part that's easy to get wrong)

The `auto` cache strategy isn't exercised by the standalone binary unless you hand it the same
kind of config the plugin would otherwise generate:

```sh
cat > "$SANDBOX/state/keepalive.yaml" <<'EOF'
preset: cache
cache:
  keepalive: true
  keepalive_idle_seconds: 270
  keepalive_max_pings: 2
  keepalive_max_usd_per_ping: 0.50
  keepalive_min_prefix_tokens: 1000
EOF
# pass --config "$SANDBOX/state/keepalive.yaml" instead of --preset off when starting the proxy
```

> **Pitfall 6 — a single-turn session is permanently un-pingable, no matter how long you wait.**
> `pingable()` (`proxy/keepalive.go`) requires `turn >= 1` — i.e. the tracked session needs a
> **second** request before the idle clock means anything. Waiting out the idle window after only
> one turn just produces `keepalive.skipped` forever. The correct sequence:
>
> 1. Turn 1 (cold write).
> 2. Turn 2, immediately, same session (`--continue`) — this is what makes the entry live
>    (`keepalive.live_sessions` becomes `1`).
> 3. **Now** wait out the real idle window (`keepalive_idle_seconds`, 270s by default) — there is
>    no shortcut; this really does take ~5 real minutes. Poll `/stats` rather than a fixed sleep:
>
>    ```sh
>    until [ "$(curl -s localhost:8899/stats | python3 -c \
>      'import json,sys; print(json.load(sys.stdin)["keepalive"]["pings"])')" != "0" ]; do
>      sleep 10
>    done
>    ```
> 4. A **third** turn, in the same session, confirms the payoff: check `savings.all` in `/stats`
>    for `keepalive_saved_usd` and `keepalive_net_usd` (net of the ping's own small cost). A real
>    run looked like: ping cost `$0.0109`, next read saved `$0.1896`, net `$0.1787`.
>
> Skipping step 2, or checking savings right after step 3 instead of after step 4, both look like
> "keep-alive doesn't work" when it's actually just not been given the chance to.

> **Pitfall 7 — `--continue` resolves by working directory, not by session id.** Claude Code's
> own session history is scoped to the project path. Running step 2/4 above from a *different*
> `cwd` than step 1 silently starts a **brand new session** instead of erroring — you'll see a
> different `session=` in the proxy log and a keep-alive entry that never accumulates a second
> turn. Always run every turn in a given test sequence from the exact same directory.

## Step 6 — test the plugin layer, in a SEPARATE project directory

```sh
mkdir -p "$SANDBOX/plugin-project" "$SANDBOX/plugin-state"
cd "$SANDBOX/plugin-project"
env -u ANTHROPIC_BASE_URL -u ANTHROPIC_AUTH_TOKEN -u ANTHROPIC_API_KEY \
    -u ANTHROPIC_CUSTOM_HEADERS -u ANTHROPIC_UPSTREAM \
  PATH="$SANDBOX/bin:$PATH" \
  CONTEXT_GURU_DEST="$SANDBOX/bin" \
  CONTEXT_GURU_STATE="$SANDBOX/plugin-state" \
  CONTEXT_GURU_PORT_BASE=8920 \
  CLAUDE_CONFIG_DIR="$SANDBOX/claude-home" \
  bash context-guru-plugin/scripts/install.sh --route --scope project \
    --cache-strategy auto --upstream https://api.anthropic.com \
    --i-consent-to-traffic-interception
```

That last flag is a hard gate the installer enforces on purpose
(`consent_required=true`, "never pass it on your own judgement") — get explicit sign-off from
whoever is asking for the test before passing it, every time, not just once per session.

> **Pitfall 8 — reusing the Step 3 project directory here silently breaks everything after it.**
> `install.sh --route --scope project` writes `.claude/settings.local.json` **in the current
> directory**, and that project-scoped file takes precedence over the user-scoped
> `claude-home/settings.json` from Step 1. If Step 6 runs in the same directory Step 3–5 used,
> every `claude -p` turn run from that directory *afterward* — including ones you think are still
> testing the standalone proxy — silently reroutes to the plugin's new port instead. The symptom
> is confusing: the standalone proxy's log just stops growing, with no error. **Use a dedicated,
> never-reused directory per proxy-under-test.**

> **Pitfall 9 — slash commands need an actual plugin install, not just `enabledPlugins: true` in
> settings.json.** Hand-editing `enabledPlugins`/`extraKnownMarketplaces` into the sandboxed
> `settings.json` is not sufficient — `installed_plugins.json` stays empty and
> `/context-guru:status` etc. silently report "isn't available in this session" instead of
> loading (Claude Code degrades gracefully here rather than erroring, which makes the gap easy to
> miss). Run the real install command against the sandbox:
>
> ```sh
> CLAUDE_CONFIG_DIR="$SANDBOX/claude-home" claude plugin install context-guru@context-guru
> ```

## Step 7 — verify a user can actually SEE their savings

Don't stop at `/stats` JSON — that's not what a user looks at. Check all four surfaces for real:

```sh
# the actual skills, through a real turn
claude -p '/context-guru:status' --max-turns 6 ...
claude -p '/context-guru:insights' --max-turns 6 ...

# the dashboard
curl -s localhost:8920/dashboard/ | wc -c      # should be real HTML, tens of KB
curl -s localhost:8920/api/sessions
```

> **Pitfall 10 — the status line resolves its own port from the live process environment, not
> just the settings file.** `resolve_routed_port()` (`context-guru-plugin/scripts/settings.py`)
> deliberately checks the actual `ANTHROPIC_BASE_URL` the calling process has, matching what
> Claude Code's own hook invocation would have — this is intentional anti-false-positive behavior
> (an earlier version rendered a permanent `cg!` warning for a project that merely *had* an
> install recorded, even when its real traffic went elsewhere). Testing `statusline.py` directly
> with `python3 scripts/statusline.py < payload.json` needs that same env var exported to match,
> or it silently prints nothing:
>
> ```sh
> echo '{"session_id":"...", "workspace":{"current_dir":"..."}, "cost":{"total_cost_usd":0.95}, ...}' \
>   | ANTHROPIC_BASE_URL="http://127.0.0.1:8920/anthropic" \
>     CLAUDE_CONFIG_DIR="$SANDBOX/claude-home" \
>     python3 context-guru-plugin/scripts/statusline.py
> ```

## Diagnostic technique: capture raw upstream bytes when `/stats` looks wrong

If usage/savings numbers look wrong (zero when they shouldn't be, or vice versa) and the
proxy's own counters (`usage_unparsed`, `usage_unreadable`) are also zero, the proxy may be
silently misreading something upstream is actually sending correctly. Don't guess — capture the
literal bytes. A minimal stdlib-only relay works, logging the raw response before anything
decompresses or parses it:

```python
# forwards POST /anthropic/... -> api.anthropic.com, logs the exact raw response body+headers
import http.server, http.client, ssl
class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length) if length else b""
        conn = http.client.HTTPSConnection("api.anthropic.com", context=ssl.create_default_context())
        headers = {k: v for k, v in self.headers.items() if k.lower() not in ("host", "content-length", "connection")}
        path = self.path.removeprefix("/anthropic")
        conn.request("POST", path, body=body, headers=headers)
        resp = conn.getresponse(); resp_body = resp.read()
        with open("/tmp/raw_capture.log", "ab") as f:
            f.write(f"STATUS {resp.status}\n".encode())
            for k, v in resp.getheaders(): f.write(f"{k}: {v}\n".encode())
            f.write(b"\n" + resp_body + b"\n---\n")
        self.send_response(resp.status)
        for k, v in resp.getheaders():
            if k.lower() not in ("content-length", "transfer-encoding", "connection"): self.send_header(k, v)
        self.send_header("Content-Length", str(len(resp_body))); self.end_headers()
        self.wfile.write(resp_body)
    def log_message(self, *a): pass
http.server.HTTPServer(("127.0.0.1", 8900), Handler).serve_forever()
```

Point `ANTHROPIC_BASE_URL` at `http://127.0.0.1:8900/anthropic` instead of the proxy, run one
turn, then inspect `/tmp/raw_capture.log` directly (gzip-decompress if `Content-Encoding: gzip`
is present — `gzip.decompress(body)` in Python). This is exactly how #391's root cause (a missing
`Accept-Encoding` strip, meaning the proxy's own usage parser was scanning gzip binary for `data:`
text and never matching) was confirmed rather than guessed at.

## Cleanup

```sh
# kill by exact PID only — several real proxies may share "context-guru-proxy" in their command
# line, and a pattern-based pkill does not distinguish them
kill <sandbox-proxy-pid> <sandbox-plugin-proxy-pid>
pgrep -fl 'cg-sub-sandbox' || echo "all sandbox proxies stopped"

rm -rf /tmp/cg-sub-sandbox

# confirm nothing real moved
pgrep -fl 'context-guru-prox[y]'
```

> **Pitfall 11 — never a `pkill` pattern containing `proxy`.** Confirm every PID individually
> before and after. This machine typically has several unrelated real proxies running
> concurrently (production routing, other sandboxes, Codex e2e tests) that share the same binary
> name in `ps`.

## Related

- [Install the plugin](install-plugin.md) · [Use with Claude Code](use-with-claude-code.md)
- [Measure savings](measure-savings.md)
- [Cache keep-alive](cache-keepalive.md)
- The bug this runbook exists because of:
  [#391](https://github.com/rossoctl/context-guru/issues/391) /
  [#392](https://github.com/rossoctl/context-guru/pull/392)
