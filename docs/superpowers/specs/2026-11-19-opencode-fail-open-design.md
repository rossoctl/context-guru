# Design: OpenCode plugin falls back to the provider when the proxy is down (#1159)

Date: 2026-11-19
Issue: [#1159](https://github.com/rossoctl/context-guru/issues/1159)
Status: proposal. The owner must approve the behavior changes in section 7.

## 1. Result: yes, a `fetch` option in the `config` hook works in OpenCode 1.19.5

The plugin can send a request to the provider when the proxy is down. It sets a `fetch`
function in `provider.options` in the `config` hook. No v1 hook can change the URL per request.

Evidence (OpenCode tag `v1.19.5`, commit `810c0eb`):

- **The config object is not cloned.** `plugin/index.ts:152,245-247` passes the live `cfg` from
  `config.get()` to each `config` hook. `config/config.ts:620-622` returns the same cached
  object every time. `provider/provider.ts:1491-1495` reads `cfg.provider` after the hooks ran
  ("includes any modifications from plugin config() hook").
- **A function value survives the merges.** `provider.ts:1539` uses remeda `mergeDeep`
  (remeda 2.26.0). It recurses into plain objects only; a function value is copied by
  reference. `provider.ts:1791` copies options with a spread. The SDK cache key at
  `provider.ts:1842-1848` uses `JSON.stringify`, which drops the function. That is harmless.
- **OpenCode keeps a custom `fetch` and wraps it.** `provider.ts:1852` sets
  `options.fetch = timeoutFetch(options)`. `timeoutFetch` (`provider.ts:96-124`) calls
  `options.fetch` when one exists, with OpenCode's abort and timeout signal.
- **The factory gets the options.** `provider.ts:1856-1863` (bundled) and `1879-1883`
  (npm) call `create*({ name, ...options })`.
- **The AI SDK uses it.** `@ai-sdk/anthropic@3.0.111` (pinned in
  `packages/opencode/package.json:60`): `fetch?: FetchFunction` in `dist/index.d.ts:1206`,
  passed on in `createAnthropic`, `dist/index.mjs:6115`. `@ai-sdk/openai@3.0.88`
  (`package.json:70`): `dist/index.d.ts:1209`, `dist/index.mjs:7500-7512`.
- **No v1 hook changes the URL.** The `Hooks` list is `packages/plugin/src/index.ts:223-334`.
  `chat.params` changes only model parameters and `providerOptions`. `chat.headers` changes
  only headers (`index.ts:247-260`, called in `session/llm/request.ts:113,133`).
- **OpenCode 1.19.5 runs only the v1 `server` entry.** `plugin/index.ts:113-124` calls
  `server(input, options)`. The v2 API at this tag (`packages/plugin/src/v2/promise/context.ts`)
  has an `aisdk.sdk` hook with `options`, and no `provider.transform` or `session.hook`. So our
  `setupV2` targets a different OpenCode 2 shape. It is out of scope here (see section 9).

Where the 66 s comes from: OpenCode retries a failed call 5 times with backoff 2, 4, 8, 16 and
30 s (`session/retry.ts:26-31,47-82`; `processor.ts:674`). Each attempt hits the dead port.
I did not measure this split. It matches the 66 s in the issue.

## 2. Approach

**Chosen: a fail-open `fetch` per routed provider, set in the `config` hook.**

- `routeProviders` sets `options.baseURL` to the proxy, as today. It also sets
  `options.fetch = failOpenFetch({ state, dialect, upstream, inner })`. `inner` is a `fetch`
  that another plugin set before us, or the global `fetch`.
- The wrapper sends to the proxy when the proxy is usable. It sends to the upstream when the
  proxy is down or routing is disabled.
- The direct URL is the route's upstream plus the path after `/<dialect>`. This is the mapping
  that the proxy uses (`proxy/proxy.go:598-600`: `/anthropic/v1/messages` to
  `<upstream>/v1/messages`). So the direct request goes to the same place the proxy would send it.
- A module-level state per project (`up`, `down`, `restarting`) connects `ensureProxyRunning`
  in `chat.headers` to the wrapper.

**Rejected:**

- **Change `baseURL` back after a failure.** The SDK instance is built once and cached
  (`provider.ts:1846-1863`). A later change to `cfg` has no effect until OpenCode restarts.
- **Keep the proxy alive at uninstall and stop only new routing.** This fixes case 1 of the
  issue only. A crash or a failed restart still breaks every request. It also leaves running
  a proxy that the user asked to stop.
- **Inject `fetch` from an `auth` hook loader.** The loader runs only when stored auth exists
  (`provider.ts:1669`), and it conflicts with the user's own auth plugins.

## 3. Data flow

| State | `chat.headers` | `fetch` wrapper | Result |
|---|---|---|---|
| Proxy up | `/healthz` ok, sets `up` | sends to proxy | as today |
| Proxy down, restart fails | restart, waits at most the cap, sets `down` | sends to upstream once | user gets a reply, without context-guru |
| Proxy restarting | waits at most the cap | `up`: proxy. Cap reached: upstream. The restart goes on in the background | next request uses the proxy when it is healthy |
| Proxy dies between the hook and the send | (`up`) | proxy refuses the connection: sends to upstream once, sets `down` | user gets a reply |
| After `/context-guru-uninstall` | sees `disabled`, sets `down`, no restart | sends to upstream | open sessions keep working, no OpenCode restart needed |
| After `/context-guru-install` again | `disabled` is gone, normal recovery | proxy again when healthy | routing comes back |

The wrapper reads the `disabled` marker itself too. So uninstall works even when no
`chat.headers` call ran first.

## 4. Failure modes: what counts as "proxy down"

Fall back **only** when the proxy cannot have received the request:

- Connection refused: Node/undici `cause.code === "ECONNREFUSED"`; Bun `code === "ConnectionRefused"`.
  OpenCode runs on Bun. The test must cover both names.
- `/healthz` fails or takes longer than 1.5 s in `ensureProxyRunning`. Then the request does
  not go to the proxy at all. That is the only "timeout" case that falls back.

**Never** fall back when the proxy may have forwarded the request:

- The proxy answered with any status (4xx, 5xx). Return the response unchanged.
- A connection reset or a timeout after the connection opened. The body may already be upstream.
- A stream that already started, then broke. Return the error.
- An abort from OpenCode's signal (user cancel, `headerTimeout`).
- A body that cannot be sent twice (a stream). The AI SDK sends a JSON string, so this is rare.
  Send no fallback and return the error.

The wrapper sends a request at most once to each target. OpenCode's own retry decides the rest.

## 5. Credentials

- The direct request uses the same `init`: same method, body, signal and headers. The AI SDK
  put the user's key (`x-api-key`, `Authorization`) in those headers. The wrapper does not
  read, add or change a credential.
- The wrapper removes only our own `x-context-guru-session` header. The provider does not need it.
- The log line names the project, dialect, upstream host and the error code. It never contains
  headers, the full URL with query, the body or the error object.
- A test sends a canary key and asserts that it is absent from every log call and every error.

## 6. Restart wait cap

- Today one request can wait up to 45 s for the lock (`withLock`, `waitMs`) plus up to 60
  health checks (`waitForHealthy`: 15 s when refused, up to 105 s when `/healthz` hangs).
- **Proposal: `chat.headers` waits at most 5 s** for `ensureProxyRunning`. It races the
  single-flight recovery with a 5 s timer, in `ensureProxyRunning` itself. The recovery goes on
  in the background. A Go proxy is healthy in under 1 s, so 5 s leaves room.
- After a failed recovery, the state stays `down` for 30 s. Requests in that time go direct
  and do not start a new restart. This stops one restart attempt per OpenCode retry.

## 7. Behavior changes (owner must approve each)

1. **Fallback when the proxy is down.** Who: every OpenCode plugin user. Old: every request
   fails with `ECONNREFUSED` after about 66 s. New: the request goes to the provider. It does
   not get compaction, offload or keep-alive, and `/stats` does not count it. Off: plugin
   option `fallback: false`.
2. **Uninstall takes effect at once.** Who: users who run `/context-guru-uninstall` in an open
   OpenCode. Old: requests fail until OpenCode restarts. New: requests go to the provider at
   once. The "restart OpenCode" note in the uninstall output changes. Off: `fallback: false`.
3. **Restart wait cap of 5 s and a 30 s `down` window.** Who: users whose proxy dies. Old: a
   request waits up to about 60 s per attempt. New: at most 5 s, then direct. Off: with
   `fallback: false` the old waits stay.

Data dependence: unchanged. The wrapper reads only the request URL and the error code. It
stores nothing. `docs/data-dependence.md` needs no new row; the PR states this.

## 8. Test plan (TDD, `node --test` in `opencode-plugin/test/`)

Stubs: an upstream `http.createServer` that records requests; a proxy port that is closed
(refused); a proxy that answers 500; a proxy that accepts and sends nothing (hangs); a proxy
that sends headers and then drops the connection.

1. `routeProviders` sets `options.fetch` to a function for each routed provider, and chains a
   `fetch` that was there before.
2. Refused: one request reaches the upstream, with the same path suffix, body and canary key.
   No `x-context-guru-session` header.
3. Proxy 500: the caller gets the 500. The upstream gets 0 requests.
4. Hang: the caller's abort ends the request. The upstream gets 0 requests.
5. Broken stream: the error reaches the caller. The upstream gets 0 requests.
6. `disabled` marker: the upstream gets the request; the proxy stub gets 0.
7. Non-executable binary: `chat.headers` returns in under 6 s. The next `fetch` goes direct.
   A second request in the 30 s window starts no new spawn.
8. Slow restart that is healthy in 1 s: the request goes to the proxy.
9. Canary: the key is absent from every `client.app.log` call and every thrown message.
10. `fallback: false`: refused stays an error, as today.
11. End to end, by hand: repeat steps 1 and 2 of the issue on OpenCode 1.19.5 with a gateway.

## 9. Open questions

1. **OpenCode 2.** The v2 API at 1.19.5 has `aisdk.sdk` with `options`. Our `setupV2` uses
   `provider.transform` and `session.hook("http.request")`, which this tag does not have. Which
   OpenCode 2 release do we target, and does its `http.request` hook allow a URL change?
2. **An auth plugin that sets its own `fetch`.** Its loader runs after the config merge, and
   `mergeDeep` lets it win (`provider.ts:1663-1680`). Then our wrapper is lost, and the old
   behavior stays. Detect and log this, or accept it?
3. **Native LLM runtime.** With `experimentalNativeLlm` (`session/llm.ts:225`) OpenCode does
   not use the AI SDK `fetch`. Is that path in scope?
4. Should a fallback show in the TUI (a toast), or only in the log?
