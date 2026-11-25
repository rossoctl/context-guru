# Design: the OpenCode installer sets up the model provider and proves it works

Date: 2026-11-25. Status: draft. Follows
[2026-11-23-opencode-one-command-installer-design.md](2026-11-23-opencode-one-command-installer-design.md).

## Summary

After one installer run, OpenCode must answer through context-guru with no setup by hand. The installer:

1. says first which providers context-guru supports, and lets the user exit with no change;
2. sets up the connection: Anthropic or OpenAI directly, or a custom base URL (a company gateway);
3. starts OpenCode's own login for that provider, so the user enters the key there;
4. proves the result: a real answer, and at least 1 request counted by the proxy.

## Why

- The owner wants to release the OpenCode plugin only after a full end-to-end test passes.
- The end-to-end test failed for gateway users. The installer expected a direct login
  (`opencode auth login`), and IBM users reach Claude only through a LiteLLM gateway. See issue #1384.
- A provider named `litellm` silently skips the proxy. The plugin routes only provider IDs that contain
  `anthropic` or `openai`. Users cannot know this.

## Goals

- Internal users give a custom base URL and a key. External users pick Anthropic or OpenAI and give a key.
- One flow for both. No IBM host in the public repo.
- The installer prints a success line only when a request really went through the proxy.

## Non-goals

- Providers other than Anthropic and OpenAI (Google, Groq, local models).
- `opencode.jsonc` and config files with comments. As today, the installer prints the lines to add by hand.
- Windows.

## Anti-goals

- **The installer never reads, asks for, or stores a key.** OpenCode's own login stores it. This keeps the
  decision of the first design.
- A false success. A check that passes when the request went straight to the provider is a failure of this
  design.

## Facts checked on OpenCode 1.19.6

Tested in an isolated folder (`XDG_*` set to a temp folder) with a fake key and a local test server.

| Fact | Result |
|---|---|
| `opencode auth login -p <id>` skips OpenCode's provider menu | yes (`--provider`, in `--help`) |
| `opencode auth list` shows which providers have a login | yes: `Anthropic api` |
| A stored key works with a custom `baseURL` | yes: sent as `x-api-key` to `<baseURL>/messages` |
| OpenCode sends a second request with its default small model | yes: `claude-haiku-5-5`, for the session title |
| The IBM LiteLLM gateway takes the key as `x-api-key` | yes: HTTP 200 |
| A custom ID `anthropic-gateway` with `"npm": "@ai-sdk/anthropic"` keeps its own key | yes: the gateway got only its key; the `anthropic` key stayed |
| `opencode auth logout <id>` removes one provider's login | yes (in `--help`) |
| `opencode auth login -p anthropic-gateway` accepts a custom ID | **no**: "Unknown provider", also with the config loaded. `-p` knows only built-in providers. |
| OpenCode's login menu takes a custom ID | yes: the last choice "Other" asks "Enter provider id" (a-z, 0-9, hyphens) |

The small-model fact matters. A gateway names its models its own way (for example `aws/claude-haiku-5-5`).
So on the gateway path the installer must also set `small_model`, or every title request fails.

## Approaches

| | How the key gets in | Trade-off |
|---|---|---|
| **B (picked)** | The installer writes only the URL and the model. `opencode auth login -p <id>` stores the key. | The installer never sees the key. It works in the terminal, the IDE and the desktop app. It cannot test the key before it saves the config; the final check does that test. |
| A | `"apiKey": "{env:LITELLM_API_KEY}"` in `opencode.json`. | No key on disk from us. Apps not started from a shell do not see the variable. |
| C | The key in `opencode.json`. | Rejected: a plain-text key in a file that people often keep in a dotfiles repo. |

The owner picked B, for internal and external users both.

The owner also rejected a list of named providers in the first question ("why 1, 2?"). The question has
two ways in, plus an exit. A gateway URL in an env file was considered and dropped: a typed URL is enough,
and the env variables below cover scripted installs.

## Flow of `install` (changes only)

The new step **Your models** comes first, before any change. The rest of the flow stays as it is.

1. **Your models.** The installer prints:

   ```
   context-guru works with Anthropic and OpenAI models only.
   It can reach them directly, or through a gateway that
   speaks the Anthropic or OpenAI API (for example LiteLLM).
   Other providers (Google, Groq, local models, ...) are not routed.

   How do you reach your models?
     1) Anthropic or OpenAI directly
     2) A custom base URL (a company gateway)
     3) Another provider: exit, change nothing
   ```

   - Default: 1.
   - 3, or end of input: print "Nothing was changed. context-guru supports Anthropic and OpenAI models.",
     exit 0.
   - On 2 the installer asks, in this order:
     - **Base URL.** It must be `https://`, or `http://` on `127.0.0.1` or `localhost`. A trailing `/v1` is
       removed and added back once.
     - **API format:** Anthropic (default) or OpenAI. This gives the provider ID: `anthropic-gateway` or
       `openai-gateway`, with `"npm": "@ai-sdk/anthropic"` or `"@ai-sdk/openai"`.
       - **Why a separate ID:** a user can have a direct Anthropic key already. Under the plain ID `anthropic`,
         that key would go to the gateway (a key leak to a third party), or the gateway key would replace it
         and uninstall could not put it back. The plugin routes any ID that contains `anthropic` or `openai`.
       - When the config already has another provider of the same API format with its own `baseURL`, the
         plugin refuses two upstreams. The installer says so and stops before any change.
     - **Model**, as the gateway names it. No default: the installer has no key, so it cannot ask the gateway
       for its list.
     - **Small model** (for titles). Default: the same model.
   - Nothing is written in this step. The answers are kept for step 4.

2. **OpenCode.** As today.
3. **Your consent.** As today. The text names the connection: "directly to Anthropic", or "through the
   gateway at `<host>`".
4. **The plugin and the connection.** As today, plus on path 2: in `opencode.json`, add
   `provider.<id>` (`npm`, `name`, `options.baseURL` = `<url>/v1`, and both models under `models`), and set
   `model` and `small_model` to `<id>/<model>`.
   - **The record keeps each changed key's old value**, or "absent", for `provider.<id>`, `model` and
     `small_model`. Today the record keeps only `created` and `had_plugin`, and uninstall never reads the
     backup.
   - **Uninstall restores key by key.** It puts back a key only when its value is still the one the installer
     wrote. A key the user changed after install stays as the user left it. The whole-file backup stays a
     safety copy only.
   - When `provider.<id>` already exists with another `baseURL`, the installer shows the old and the new
     value and asks before it replaces it.
   - **A second run with the same answers changes nothing** and keeps the first run's old values in the
     record.
5. **Login.** The installer runs `opencode auth list`.
   - When the provider ID has a login, it says so and skips the login.
   - Else it runs `opencode auth login`, with the terminal attached. For a gateway, it says first: "In the
     list, pick Other, then type: anthropic-gateway. Then paste your gateway key." (`-p` refuses custom IDs.)
     After the login it runs `opencode auth list` again: the login counts only when the ID is there.
   - When the installer started the login, the record says so. **Uninstall then runs
     `opencode auth logout <id>`.** It never removes a login it did not start.
   - When the login fails or is cancelled: offer to undo this run (see below), then exit 1. With "no", say
     how to run the login and this installer again.
6. **Check that it works.** The question is now default **yes**.
   - Pass = `opencode run` answers with no `Error:`, `proxy=up`, and `stats_requests` ≥ 1.
   - Each failure names its cause: no answer (key, URL or model wrong: show OpenCode's error), proxy down
     (show the log path), or 0 requests (the provider was not routed).
   - A failure exits 1. A "no" to the check prints "Not checked." and exits 0.

## Path 1 after login

On path 1 the user may pick another provider in OpenCode's own menu. The installer runs `opencode auth list`
again after the login. When neither `anthropic` nor `openai` has a login, it says that context-guru cannot
route that provider, and it offers to undo this run. Then it exits 1.

**Undo this run** reverts only what this run changed: the config keys and plugin entries (key by key, as in
uninstall), the plugin folder when this run put it there, and a login this run started. It does not ask
uninstall's other questions (delete local data, uninstall OpenCode). An OpenCode that this run installed stays,
and the installer says so.

## Scripted installs

Env variables answer the questions in advance. Each one is optional.

| Variable | Answers |
|---|---|
| `CG_OC_GATEWAY_URL` | step 1: path 2 with this URL |
| `CG_OC_API` | `anthropic` or `openai` |
| `CG_OC_MODEL` | the model |
| `CG_OC_SMALL_MODEL` | the small model |

An IBM-internal guide can set these in its install command. The public repo has no IBM host.

The installer still needs a terminal, as today: the consent, the login and the check stay questions. A fully
unattended install is out of scope.

## Behavior change and data dependence

- **Behavior change:** yes, for users of the OpenCode installer only. Old: the installer never touched the
  provider, and the test prompt was default no. New: it asks how you reach your models, can write the
  provider's `baseURL`, `model` and `small_model`, starts OpenCode's login, and the test prompt is default yes
  and decides the exit code. To avoid it: answer 1 and "no" to the check; with a login already in place,
  nothing new is asked. The owner (a human developer) decides this before merge.
- **Data dependence:** metadata only. The installer reads provider names from `opencode auth list` and the
  proxy's request count. It writes a URL and model names. It reads no key, no prompt and no answer text,
  except the last 200 characters of the test answer, which it prints and does not store. The row in
  `docs/data-dependence/features/opencode-installer.md` gets these 2 reads.

## Tests

- `test_install_opencode.py`, each with a fake shell:
  - choice 3 and end of input change nothing and exit 0;
  - path 2 writes `baseURL`, `models`, `model` and `small_model`; uninstall puts back the earlier values;
  - a URL with `/v1`, with a trailing `/`, `http://` on a remote host (refused);
  - an existing `baseURL` asks before it replaces it;
  - `auth list` with a login skips the login; without one, `auth login -p <id>` runs;
  - path 1 with a non-routed provider after login offers undo and exits 1;
  - the check: each of the 3 failures exits 1 with its message; `stats_requests=0` fails;
  - the env variables skip their questions.
- End to end, by the owner, on macOS: path 2 with the IBM LiteLLM gateway, then uninstall.
- End to end in Docker (Linux): path 2 against a local test server that answers like Anthropic. Docker has no
  terminal for the login, so the test puts a test key for `anthropic-gateway` in OpenCode's auth file first.
  The login step then sees the login and skips. The check must still pass with `stats_requests` ≥ 1.
- A canary test (a fake key value): drive it through install, check and uninstall, and assert that it is in no
  installer output, no record and no config file.

## Left to the builder

- How the installer reads `opencode auth list` (the output has colors and box lines; match the provider
  display name per line).
- The exact wording of the messages, in the style of the existing ones.
