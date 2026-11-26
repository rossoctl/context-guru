# OpenCode installer provider setup: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The OpenCode installer sets up the model connection (direct, or a custom gateway URL), starts
OpenCode's own login, and passes only when a real request went through the proxy.

**Architecture:** All changes are in `opencode-plugin/scripts/install_opencode.py`, in its existing `Installer`
class and helpers. Questions go through `Terminal`, programs through `Shell`; tests fake both, as today.
Uninstall and the new "undo this run" share one revert helper.

**Tech Stack:** Python 3 standard library, `unittest`. No new dependencies.

**Spec:** [docs/superpowers/specs/2026-11-25-opencode-installer-provider-setup-design.md](../specs/2026-11-25-opencode-installer-provider-setup-design.md)

**Worktree:** `.claude/worktrees/feat-oc-provider`, branch `feat/opencode-installer-provider-setup` (based on
`feat/opencode-one-command-installer`, PR #1296). Baseline: `python3 -m unittest test_install_opencode` in
`opencode-plugin/scripts` runs 42 tests, OK.

## Global Constraints

- The installer never reads, asks for, or stores a key. Only `opencode auth login` sees it.
- Gateway provider IDs: `anthropic-gateway` (`"npm": "@ai-sdk/anthropic"`) and `openai-gateway`
  (`"npm": "@ai-sdk/openai"`).
- Base URL: `https://`, or `http://` only on `127.0.0.1` or `localhost`. Strip one trailing `/` and one `/v1`;
  write `<url>/v1`.
- Pass rule: `opencode run` exit 0 and no `Error:` in its output, `proxy=up`, `stats_requests` ≥ 1.
- Choice 3 and end of input at the first question: "Nothing was changed. context-guru supports Anthropic and
  OpenAI models.", exit 0.
- Env variables: `CG_OC_GATEWAY_URL`, `CG_OC_API` (`anthropic`|`openai`), `CG_OC_MODEL`, `CG_OC_SMALL_MODEL`.
- No IBM host anywhere in the repo.
- Message style: ASD-STE100, like the existing messages. Commits: `git commit -s`, conventional title,
  `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`.

## Review Focus

1. **A user with a direct `anthropic` login picks path 2.** The direct key must stay and must never go to the
   gateway. Test in Task 3.
2. **The user edits `model` after install, then uninstalls.** The user's value stays. Test in Task 2.
3. **The proxy is up but counted 0 requests** (the provider was not routed). This must fail, not pass. Test in
   Task 4.
4. **A second install with the same answers.** No duplicate entries, and the record keeps the first old values.
   Test in Task 2.
5. **A base URL pasted with `/v1/` or `/v1`**, or `http://` to a remote host. Test in Task 1.

---

### Task 0: Check `auth login -p` with a custom ID

The spec marks this fact as not checked. It decides the login argv in Task 3.

- [ ] **Step 1:** In a terminal, with `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME` and `XDG_CACHE_HOME`
  set to a new temp folder, and an `opencode.json` there that has an `anthropic-gateway` provider (as in the
  spec), run `opencode auth login -p anthropic-gateway`. Paste `FAKE`.
- [ ] **Step 2:** Run `opencode auth list` with the same variables.
  Expected: a line `anthropic-gateway api`. If the login refuses the ID, Task 3 uses `opencode auth login`
  with no `-p`, and the installer first says "Pick Other, then type: anthropic-gateway". Write the result in the
  spec's facts table.

This needs a human at a terminal. If the executor has none, ask the owner to run it.

### Task 1: The "Your models" step

**Files:**
- Modify: `opencode-plugin/scripts/install_opencode.py` (constants, new dataclass and function, `install()`,
  `INSTALL_STEPS`)
- Test: `opencode-plugin/scripts/test_install_opencode.py`

**Interfaces:**
- Produces:
  - `INSTALL_STEPS = ("Check this computer", "Your models", "OpenCode", "Your consent", "The context-guru plugin", "Login", "Check that it works")`
  - `@dataclass(frozen=True) class Connection: provider: str | None; base_url: str | None; model: str | None; small_model: str | None`.
    Path 1: all `None`. Path 2: `provider` is `"anthropic-gateway"` or `"openai-gateway"`, `base_url` ends with `/v1`.
  - `def gateway_url(text: str) -> str` (raises `Stop` on a bad URL)
  - `Installer.ask_connection(self, documents: dict) -> Connection | None` (`None` = choice 3)
- Existing tests that answer the first question change: each gets a leading `"1"` answer.

- [ ] **Step 1: Write the failing tests**

```python
def test_choice_3_changes_nothing_and_exits_0(self):
    code = self.installer("3").install()
    self.assertEqual(code, 0)
    self.assertIsNone(self.read("opencode.json"))
    self.assertEqual(self.npm_calls(), [])
    self.assertIn("Nothing was changed. context-guru supports Anthropic and OpenAI models.", self.terminal.text())

def test_end_of_input_at_the_first_question_changes_nothing_and_exits_0(self): ...  # FakeTerminal with an `eof` flag that raises Stop like Terminal.ask
def test_the_first_question_names_the_supported_providers_first(self):
    # "context-guru works with Anthropic and OpenAI models only." comes before "How do you reach your models?"
def test_gateway_url_strips_v1_and_slash(self):
    for text in ("https://gw.example/v1", "https://gw.example/v1/", "https://gw.example/", "https://gw.example"):
        self.assertEqual(install_opencode.gateway_url(text), "https://gw.example/v1")
def test_gateway_url_refuses_http_to_a_remote_host(self):
    with self.assertRaises(install_opencode.Stop): install_opencode.gateway_url("http://gw.example")
    self.assertEqual(install_opencode.gateway_url("http://127.0.0.1:4000"), "http://127.0.0.1:4000/v1")
    self.assertEqual(install_opencode.gateway_url("http://localhost:4000/v1"), "http://localhost:4000/v1")
def test_path_2_asks_url_format_model_and_small_model(self):
    c = self.installer("2", "https://gw.example", "", "aws/claude-sonnet-5-5", "aws/claude-haiku-5-5").ask_connection({"opencode.json": {}})
    self.assertEqual(c, install_opencode.Connection("anthropic-gateway", "https://gw.example/v1", "aws/claude-sonnet-5-5", "aws/claude-haiku-5-5"))
def test_path_2_small_model_defaults_to_the_model(self): ...  # empty answer -> small_model == model
def test_path_2_openai_format_gives_openai_gateway(self): ...  # format answer "openai"
def test_path_2_model_has_no_default_and_asks_again(self): ...  # "" then "m" -> model "m"
def test_env_variables_answer_the_questions(self):
    # CG_OC_GATEWAY_URL, CG_OC_API=openai, CG_OC_MODEL=m, CG_OC_SMALL_MODEL=s -> no question asked
def test_another_provider_with_its_own_base_url_stops_before_any_change(self):
    # opencode.json has provider.anthropic.options.baseURL "https://other.example/v1"; path 2 with anthropic format
    # -> Stop naming both upstreams; opencode.json unchanged; no npm call
```

- [ ] **Step 2: Run** `python3 -m unittest test_install_opencode -k models -k gateway -k choice -k env -k path_2 -k end_of_input`
  Expected: FAIL with `AttributeError` on `Connection` / `gateway_url` (not an import error of the module).
- [ ] **Step 3: Implement** `Connection`, `gateway_url`, `ask_connection`, and call it in `install()` right
  after `read_configs()`, before `ensure_opencode()`. Store the result on `self.connection`. On `None`, say the
  exit line and return 0. The opening text and the 3 choices are the spec's exact copy. The two-upstream check:
  any `provider.<id>` with the same dialect as the gateway (id contains `anthropic` / `openai`, as
  `providerDialect` in `index.js`) other than our gateway ID, that has an `options.baseURL`.
- [ ] **Step 4: Update the existing tests:** prepend `"1"` to the answers of each test that runs `install()`.
  Run the whole file. Expected: all pass.
- [ ] **Step 5: Commit** `feat(opencode): ask how the user reaches the models, before any change`

### Task 2: Write the connection, restore it key by key

**Files:** same two files (`install_plugin`, `uninstall`, a new `revert` helper).

**Interfaces:**
- Consumes: `Connection` (Task 1).
- Produces:
  - Record field `"connection": {"provider": str, "wrote": {key: value}, "before": {key: value | ABSENT}}`, keys
    are dotted JSON paths: `"provider.anthropic-gateway"`, `"model"`, `"small_model"`. `ABSENT = {"absent": True}`.
  - `Installer.write_connection(self, document: dict) -> None` (path 2 only; called in `install_plugin` before
    `write_json` of `opencode.json`)
  - `Installer.revert(self, record: dict) -> list[str]` — removes the plugin entries (today's loop in
    `uninstall`) and restores connection keys; returns the `by_hand` lines. `uninstall` calls it.

- [ ] **Step 1: Write the failing tests**

```python
def test_path_2_writes_the_gateway_provider_and_both_models(self):
    # answers: path, URL, format (default), model, small model, consent, test prompt
    self.installer("2", "https://gw.example", "", "m", "s", "y", "n").install()
    doc = self.read("opencode.json")
    self.assertEqual(doc["provider"]["anthropic-gateway"], {"npm": "@ai-sdk/anthropic", "name": "Anthropic gateway",
        "options": {"baseURL": "https://gw.example/v1"}, "models": {"m": {"name": "m"}, "s": {"name": "s"}}})
    self.assertEqual((doc["model"], doc["small_model"]), ("anthropic-gateway/m", "anthropic-gateway/s"))
def test_path_1_writes_no_provider_and_no_model(self): ...
def test_uninstall_puts_back_the_old_model_and_removes_the_gateway(self):
    # before: {"model": "anthropic/claude-x"}; install path 2; uninstall -> {"model": "anthropic/claude-x"}, no "provider", no "small_model"
def test_uninstall_keeps_a_model_the_user_changed_after_install(self):
    # after install, user sets model "openai/gpt-x"; uninstall -> model stays "openai/gpt-x"
def test_a_second_install_with_the_same_answers_changes_nothing(self):
    # file bytes and record "before" equal after run 1 and run 2
def test_an_existing_gateway_with_another_url_asks_before_replacing(self):
    # provider.anthropic-gateway.options.baseURL "https://old.example/v1"; answer "n" -> Stop, file unchanged; "y" -> new URL
```

- [ ] **Step 2: Run** the new tests. Expected: FAIL on a missing `provider` key / `write_connection`.
- [ ] **Step 3: Implement** `write_connection`, `revert`, and the record field. Restore rule: put back `before`
  only when the current value equals `wrote`; for `ABSENT`, delete the key (and an empty `provider` object).
  On a second run, keep the first run's `before`.
- [ ] **Step 4: Run** the whole file. Expected: all pass.
- [ ] **Step 5: Commit** `feat(opencode): set up a gateway connection, and restore it key by key on uninstall`

### Task 3: The login step and "undo this run"

**Files:** same two files. `Shell` and `FakeShell` get `interactive`.

**Interfaces:**
- Produces:
  - `Shell.interactive(self, cmd: list[str]) -> int` — runs with the installer's own stdin/stdout/stderr (the
    terminal), no capture, returns the exit code. `FakeShell.interactive` keeps the argv and adds the provider
    to `self.logins` unless `self.login_fails`.
  - `def logged_in(text: str) -> set[str]` — from `opencode auth list` output: strip ANSI codes, take each
    `●` line's first word after `●`, lower-cased (`"anthropic"`, `"openai"`, `"anthropic-gateway"`). The
    `Environment` block counts too (an `ANTHROPIC_API_KEY` is a login).
  - Record field `"login_started": "<provider id>"`.
  - `Installer.login(self) -> None` (raises `Stop` after the undo offer)
  - `Installer.undo(self) -> None` — `revert(self.record)`, remove the plugin folder when this run put it, run
    `opencode auth logout <id>` when `login_started`, delete the record; says that an OpenCode this run installed
    stays.
- `FakeShell.run` answers `["opencode","auth","list"]` from `self.logins`, and `["opencode","auth","logout",id]`.
- `FakeShell.logins` defaults to `{"anthropic", "anthropic-gateway"}`, so the tests of Tasks 1 and 2 keep their
  answers (the login skips). Tests of this task set it.
- Task 4 makes the test prompt default yes, so the earlier tests that answer `"n"` to it keep working unchanged.

- [ ] **Step 1: Write the failing tests**

```python
def test_logged_in_reads_credentials_and_environment(self):
    text = "\x1b[0m┌  Credentials /x/auth.json\n│\n●  Anthropic api\n●  anthropic-gateway api\n└  2 credentials\n┌  Environment\n●  OpenAI OPENAI_API_KEY\n"
    self.assertEqual(install_opencode.logged_in(text), {"anthropic", "anthropic-gateway", "openai"})
def test_path_2_starts_the_login_for_the_gateway_id(self):
    # interactive calls == [["opencode","auth","login","-p","anthropic-gateway"]]; "Paste your gateway key." shown first
def test_path_2_with_a_direct_anthropic_login_still_logs_in_to_the_gateway(self):
    # logins = {"anthropic"} -> login for anthropic-gateway runs; no "auth logout anthropic" ever
def test_an_existing_login_skips_the_login(self): ...   # logins={"anthropic-gateway"} -> no interactive call, no login_started
def test_uninstall_logs_out_only_a_login_the_installer_started(self): ...  # both cases
def test_a_failed_login_offers_undo_and_exits_1(self):
    # login_fails; answer "y" to undo -> opencode.json back to before, plugin folder gone, exit 1
def test_path_1_with_a_non_routed_provider_offers_undo_and_exits_1(self):
    # path 1, login adds "groq" only -> message names that context-guru cannot route it; undo "y"; exit 1
def test_undo_keeps_an_opencode_this_run_installed_and_says_so(self): ...
```

- [ ] **Step 2: Run** the new tests. Expected: FAIL on missing `logged_in` / `interactive`.
- [ ] **Step 3: Implement** them. Insert the `Login` step between `install_plugin` and `finish`. Path 1: when
  `{"anthropic","openai"} & logged_in(...)` is not empty, skip; else run `opencode auth login` (no `-p`), then
  check again. Use the argv from Task 0.
- [ ] **Step 4: Run** the whole file. Expected: all pass.
- [ ] **Step 5: Commit** `feat(opencode): start OpenCode's login, and undo the run when it cannot route`

### Task 4: The final check proves the proxy carried the request

**Files:** same two files (`finish`, `consent`).

- [ ] **Step 1: Write the failing tests** (`FakeShell` gets `self.requests = 1`, printed as `stats_requests=`)

```python
def test_the_check_is_default_yes(self): ...          # empty answer runs `opencode run`
def test_the_check_passes_with_one_request_through_the_proxy(self):
    # exit 0; "✓ The proxy started, and OpenCode answered through it."
def test_zero_requests_fails_with_exit_1(self):
    # requests = 0 -> exit 1; message says the request did not go through the proxy; no "✓"
def test_no_answer_fails_with_exit_1_and_shows_the_error(self): ...   # run_fails "exit" and "error-exit-0"
def test_proxy_down_fails_with_exit_1_and_names_the_log(self): ...
def test_no_to_the_check_says_not_checked_and_exits_0(self): ...
def test_consent_names_the_gateway_host(self): ...    # path 2: "through the gateway at gw.example"; path 1: "directly"
```

- [ ] **Step 2: Run** them. Expected: FAIL (today the check is default no and ignores the count).
- [ ] **Step 3: Implement.** `finish()` returns the exit code; `install()` returns it. The failure messages name
  the cause, not `opencode auth login` alone (this closes issue #1384's hint part).
- [ ] **Step 4: Run** the whole file. Expected: all pass.
- [ ] **Step 5: Commit** `feat(opencode): pass the install check only when the proxy counted the request`

### Task 5: Canary test, docs, release test

**Files:**
- Test: `opencode-plugin/scripts/test_install_opencode.py`
- Modify: `opencode-plugin/README.md` (a "Company gateway" section), `docs/data-dependence/features/opencode-installer.md`,
  `docs/superpowers/specs/2026-11-23-opencode-one-command-installer-design.md` (one line: the key decision moved
  to the new spec), the module docstring of `install_opencode.py`, and `opencode-plugin/release/test_release.py`
  only when its scripted answers break.

- [ ] **Step 1: Write the canary test:** `test_a_key_never_reaches_output_record_or_config`. `FakeShell.interactive`
  writes `CANARY-KEY-4711` into the fake auth file (outside `config_dir()` and `state_root()`), as the real login
  would. Run install (path 2), check and uninstall. Assert the canary is absent from `self.terminal.text()`, the
  record, every file under `config_dir()` and `state_root()`.
- [ ] **Step 2: Run** it. Expected: PASS (it guards; it is not red first, and say so in the commit).
- [ ] **Step 3: Docs.** README: the 3 choices, the gateway IDs and why, the env variables, a LiteLLM example
  with `gw.example` (no IBM host). Data-dependence row: the 2 new reads (`opencode auth list` provider names, the
  proxy's request count).
- [ ] **Step 4: Run** `python3 -m unittest discover -s opencode-plugin/scripts` and
  `python3 -m unittest opencode-plugin/release/test_release.py`, and `go test ./components/all/ -run DataDep`.
  Expected: all OK.
- [ ] **Step 5: Commit** `docs(opencode): the gateway setup, its data use, and a key canary test`

### Task 6: End to end, then review

- [ ] **Step 1: Docker (Linux).** Debian, Node 22, the release tarball from `package.sh` served on
  `127.0.0.1:8799`, and a local test server that answers `/v1/messages` like Anthropic. Seed `auth.json` with
  `{"anthropic-gateway":{"type":"api","key":"TEST"}}`. Run the installer on path 2 with
  `CG_OC_GATEWAY_URL=http://127.0.0.1:<port>`. Expected: `✓ The proxy started, and OpenCode answered through it.`,
  exit 0, and the test server saw `x-api-key: TEST`. Then uninstall: `opencode.json` and `auth.json` are as before.
- [ ] **Step 2: macOS, by the owner.** Path 2 with the company gateway; then path 1 is not needed for IBM.
  Paste the check lines. Then uninstall.
- [ ] **Step 3:** When both pass, set `TESTED_OPENCODE = "1.19.6"`, run the tests, commit
  `chore(opencode): mark OpenCode 1.19.6 as tested`.
- [ ] **Step 4:** Push, open the PR (behavior change named at the top, as the spec says), run the
  `offload-review` skill.

### Task 7: The gateway key (design amendment, 2026-11-25)

**Spec:** the section "Amendment: the gateway key" of the design.

**Interfaces:**
- `Terminal.secret(self, question: str) -> str` (hidden input; `Stop` at end of input). `FakeTerminal.secret`
  keeps the question, never the answer.
- `class Web: get(self, url: str, headers: dict) -> tuple[int, str]` (raises `OSError` on a network or TLS
  error). `FakeWeb` in the tests keeps each URL and header set.
- `Installer(terminal, shell, source, downloaded=False, web=None)`.
- `auth_path() -> Path` = `$XDG_DATA_HOME/opencode/auth.json`; `def write_key(provider: str, key: str) -> None`
  (atomic, 0600, keeps other entries).
- The key lives on `self.key` only, never in `Connection`, the record, or a message.

- [ ] Tests first: the question order and wording; hidden key; key check 200 lists `claude` IDs; 401 asks again
  (3 times, then stops); a network error falls back to typed model names; a saved key is offered; the key goes to
  `auth.json` with mode 0600 and other entries stay; the record has the ID only; uninstall logs out; the canary
  test covers the key file, the web requests (only to the gateway host) and all output; Ctrl-C gives exit 130;
  the config folder is shown; the consent names the choice.
- [ ] Rewrite the Task 3 tests that expected the "pick Other" login on the gateway path: that behavior is
  replaced by the amendment.
- [ ] Commit `feat(opencode): ask the gateway key, check it, and list the models`.
