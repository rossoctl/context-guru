---
name: install
description: Install a local context-guru proxy and route this project's Claude Code sessions through it, so long sessions stop paying to re-create the prompt cache. Also installs the terminal status line, in the same scope routing uses, on by default. Use when the user asks to install, set up, enable, try or start context-guru, or to route Claude Code through it. Accepts --global to route every project on the machine instead of just this one, --cache-strategy <none|5-min-ping|1-hour-head> to override the default cache strategy, --attach <url> to point at a proxy that already exists (a gateway or shared pod) instead of starting one, and --no-statusline to skip the automatic status line install.
---

# Install context-guru for Claude Code

<!-- Deliberately NO `allowed-tools:` here. The `!` block below pre-executes at render and needs no
     tool permission, so the only thing an allowed-tools line would grant is the ONE command that
     starts a traffic-intercepting proxy and repoints ANTHROPIC_BASE_URL. Measured in a real
     sandboxed session: with `Bash(.../install.sh)` declared, that command ran with no prompt at all.
     A plugin granting itself the permission the classifier exists to ask about is the plugin
     answering a question that belongs to its user. -->

!`"${CLAUDE_PLUGIN_ROOT}/scripts/install.sh" --route --plan`

**The block above is this project's real state, gathered before you were asked anything.** It ran
during rendering, so you did not choose to run it and cannot have mistyped it — read it rather than
re-deriving any of it. It wrote nothing and started nothing.

Your job is the part a script does badly: putting one question to a human, and reporting honestly.
Everything else — resolving the port, ordering the proxy before the routing key, deriving the URL,
health-checking, recording the undo — is in `install.sh --route`, where it is line order rather than
a numbered paragraph somebody can read differently.

## 1. Say what you are about to do, in three lines

- Routes this project's requests through a **local** proxy (`127.0.0.1`) that forwards to Anthropic.
  **No API key is added** — a Pro/Max login keeps working.
- **The real risk: if the proxy is down, requests HANG** rather than failing. A `UserPromptSubmit`
  hook restarts it. This is why one project, not the machine, is the default scope.
- `/context-guru:uninstall` reverses it. If routing itself breaks, no skill can run — so the install
  drops a plain-sh escape hatch outside the plugin, and step 4 tells them where.

If `cache_strategy=5-min-ping` (the default), one more clause: keep-alive is on, so it spends a little
of their own quota on idle turns to hold the cache warm, and `/context-guru:cache-strategy-picker`
names the alternatives and what each costs. Do not recommend one — they differ in what they spend, and
that is the user's call. Always, one more: a read-only status line goes on too, in the same file routing itself just used (`--no-statusline` skips it, `/context-guru:statusline off` undoes it after).

Fuller detail is in `docs/how-to/install-plugin.md`. Point at it; do not recite it.

## 2. Ask ONCE, then run one command

**Read `result=` in the plan first.** A plan always exits 0 — `needs_decision` is something for you
to act on, not a failure to report as one.

- `result=planned` — the plan is clean. If `already_routed=true`, say so: this is a re-run or a
  repair, not a fresh install.
- `result=needs_decision reason=base_url_already_set` — `existing_base_url=` names somebody's endpoint. It
  may be their company gateway, a benchmark endpoint or another proxy. **This is the one question**,
  and on a hosted agent the answer is nearly always chain: our proxy sits in front and theirs keeps
  handling auth and model routing. Replacing it outright usually breaks that agent's authentication.
- `result=needs_decision reason=user_scope_needs_flag` — they passed `--global`. Confirm once, naming the
  blast radius: **every** Claude Code session on the machine, including projects that have nothing
  to do with context-guru, which is also every session they could use to fix it.
- `result=needs_decision reason=project_installs_exist` — user scope, and the `existing_project=`
  lines name projects that route themselves. The machine-wide route will **not** reach them: their
  own settings file is more specific and keeps winning — not what "everywhere" means to the asker.
  Ask **here**: this works from inside one of those projects, so never answer it by sending them
  elsewhere. *Keep this project on its own settings and port, or fold it into the machine-wide one?*
  Then `--on-existing-projects leave` (both stay, each on its own port and config) or `adopt` (their
  routing and port option are removed, each file backed up, so they fall back to the machine-wide
  route). `adopt` runs last, after the route is proven healthy, and reports `adopted_project=…
  unrouted=… recovery_dir=…` per project — read those rather than assuming. `unrouted=removed` is
  the success value; `unchanged` or `conflict` means that project kept its routing and must be
  reported as still overriding. Relay two more per-project lines rather than folding them into
  "adopted": `adopted_project_still_overriding=<dir> base_url=<url>` — that project's own
  pre-context-guru URL was rightly restored, so it still overrides the machine-wide route the user
  asked for. `adopted_proxy_left_running=<dir> port=<n>` — its old proxy still answers, so its state
  was left alone; report the port. `adopted_proxy_stopped=` needs no comment.
  `adopted_project_is_this_project=<dir>` — they ran this from a project that was itself adopted
  (converting a project-local install); it was folded in like any other, so its old proxy is stopped
  and the machine-wide one, on its own port, is what serves it now. `adopted_proxy_kept=<dir>
  port=<n>` — that project's proxy was on the port this install serves (a pinned port), so it stays.
- `result=needs_decision reason=port_owned_by_another_project` — `owner_project=` already has a proxy
  on that port, running its preset, and a proxy is never taken from its owner. Clear the `port` option
  so one is allocated, or ask for an unused one.
- `result=refused reason=port_changed_since_plan` — the port they agreed to is no longer the one this
  project gets. **Nothing was written.** Re-run `--route --plan` and ask again on the new port.
- `result=refused reason=port_recorded_by_another_project` — this project and `owner_project=` both
  have `port=` recorded; both are equally real, and picking one would put two projects with different
  presets on one proxy. **Nothing was written.** Report both and ask which keeps the port; the other
  needs its `port` option cleared (or an uninstall) so a fresh one is allocated.
- `port_unwound=` on a failure — `true` (with `port_unwound_parts=option,record`) means step 0's own
  bookkeeping was taken back, so "nothing was written" is literal. `nothing_to_undo` means a re-install
  failed and the working install's record and pinned `options.port` are **deliberately** still there —
  do not offer to clear them. Absent: they may still be there; say so, do not guess.
- `result=error reason=binary_install_failed` — read `detail=`. `no_release_found` wants the source
  build offer (`make build-static`, Go 1.26, no C toolchain). Anything naming a checksum
  (`checksum_mismatch`, `checksum_unavailable`, `checksum_absent`) is a **hard stop**: it is the only
  integrity check in the path and the binary is about to carry all of their LLM traffic. Never set
  `CONTEXT_GURU_INSECURE=1` on their behalf.

**One question, covering everything that needs their agreement — and you do not write it.** The
script generates it from the resolved facts and prints it (`consent_question*=`, below). A worked
example used to sit here, and it was the hazard this design removes: a hand-written sentence beside a
generated one is two sources for one question, and the hand-written one drifts.

### Get an explicit yes, as a choice they pick

**`--route` refuses to do anything without `--i-consent-to-traffic-interception`.** That is
deliberate and it is not a formality: everything the command does either intercepts their model
traffic or points it somewhere new, and the approval prompt cannot be relied on to ask about that —
it is probabilistic, a skill can declare it away, and in an unattended session there is no prompt
because there is no human. So the script fails closed and the consent has to come from a person.

**Ask it as a two-option choice, not as prose they can skim.** Use `AskUserQuestion` if you have it,
so it renders as something they pick rather than something they might answer sideways:

- **question**: what the `consent_question*=` line says, generated from the same resolved facts as the
  command it authorises. Say all of it — phrase it naturally, but every fact has to survive, and do not
  drop the money: a question narrower than the command it authorises is not consent to that command.
- **option 1 — "Yes, route this project"**: what they get, and that `/context-guru:uninstall` reverses it.
- **option 2 — "No, don't change anything"**: nothing is installed, started or written.

Without `AskUserQuestion`, ask in plain text with exactly two numbered options and stop for an answer.

**A silent or absent answer is a NO.** If nothing comes back — a non-interactive run, a session with
no human — report what the plan found and stop. Do not infer consent from the fact that they typed
`/context-guru:install`, and do not pass it because a refusal is inconvenient. **Never pass it on your
own judgement.** Passing it is you asserting that a person said yes.

### Then run one command

**When the plan came back `needs_decision reason=base_url_already_set`**, there is no single
`confirm_command=` — the answer is the thing being asked for. The plan prints one runnable line per
answer instead, **paired**: `consent_question_chain=` with `confirm_command_chain=`, and
`consent_question_replace=` with `confirm_command_replace=`. Ask using the two `consent_question_*`
lines as the two options — they name the endpoint the user already has and say what becomes of it,
which is the whole of what they are deciding — then run the `confirm_command_*` line matching their
answer, verbatim. For *abort*, run nothing.

There is deliberately **no** `consent_question=` or `confirm_command=` on that path: the answer is the
thing being asked for, so neither could be complete, and both those keys mean something exact
everywhere else. Do not add `--on-conflict` to any other line yourself; if the paired lines are
missing, re-run `--plan` rather than composing a command.

**Otherwise, run the `confirm_command=` line from the plan, verbatim.** Copy it; do not retype it, do not
reorder it, and do not add or drop a flag. It is printed with every decision already resolved — scope,
mode, base URL, conflict, cache strategy, machine-wide acknowledgement — and it ends with
`--i-consent-to-traffic-interception`, so **the only thing you add is nothing.**

There is deliberately no example command here. There used to be, and it was the defect this section
exists to prevent: it hardcoded `--on-conflict chain`, which is wrong whenever the plan came back with
nothing already set, and it spelled the path `"${CLAUDE_PLUGIN_ROOT}/scripts/install.sh"` — a variable
that is substituted into a `` !`` ``-block's command string but is **not** exported to a Bash tool call,
so on the one gated command it could expand to `/scripts/install.sh`, and a model that hit "no such
file" would improvise a path. `confirm_command=` carries the absolute path the script resolved from
`$0`, which is why it is the only spelling to use.

If a decision in it looks wrong, go back to `## 2` and change the input to the plan, then re-read the
line it prints. Editing the line by hand is how a decision the user made gets silently dropped.

**Expect this one command to be gated, and do not try to get around it.** It starts a
traffic-intercepting proxy and repoints `ANTHROPIC_BASE_URL`; auto mode is right to ask, because
installing a plugin by name is not the same as consenting to have your model traffic intercepted.
The command names its own scope and upstream, so approving it **is** the consent rather than a second
copy of the question. If it is denied, hand them three options and no fourth: approve the prompt, run
that exact command themselves with `!`, or add the rule the plan printed as `permission_rule=`. Do
not reword the command to look like less than it is, and never write routing while no proxy answers.

## 3. Read the result

- `result=routed` — done. `settings_result=` says which: `added`, `unchanged` (already correct),
  `completed` (a repair of an earlier partial attempt — report it as success, not "nothing to do"),
  or `repointed` (moved to a new port).
- `result=error reason=health_check_failed` — **no routing was written**, so the project is unrouted,
  which is a working project. Say what the log shows rather than guessing. Do **not** say "nothing
  happened": check `proxy_started=`. If it is `true`, a proxy IS still listening on that port and was
  not stopped — say so, and pass on `stop_command=` or point at `/context-guru:uninstall`. A health
  check often fails transiently (a slow start, a busy laptop), and a user told "nothing happened" will
  retry into their own stale pidfile and an occupied port.
- `result=error reason=health_check_failed_after_write` with `rolled_back=true` — the **routing key**
  was removed again automatically, so they are unrouted rather than broken. The rollback undoes the
  routing key and nothing else: `proxy_started=`, `pidfile=` and `strategy_file=` say what is still
  there. Report those too rather than implying a full undo.
- `result=error reason=settings_write_failed detail=unparseable_json` — their settings file was
  already broken. Do not rewrite it; tell them where it is.
- `result=error reason=settings_write_failed detail=base_url_already_set` — a **refusal**, not a broken
  file: something was already at that key and the run did not carry a decision authorising a replace.
  It should not be reachable once `--on-conflict` is passed, so treat it as a bug worth reporting
  rather than something to retry with a flag you chose yourself. Check `proxy_started=` — the proxy
  starts before this step, so one may be running.
- `result=refused reason=consent_required` — you ran it without the flag, or without asking. Nothing
  was installed, started or written. Go back and ask; do not simply re-run it with the flag appended.
- `result=refused reason=unknown_strategy` — the `--cache-strategy` name does not exist (a typo, e.g.
  `5-minute-ping` for `5-min-ping`). Nothing was installed, started or written; the `note=` lists the
  real names. Ask which they meant — do not pick one for them, because the names differ in whether
  they spend the user's quota.
- `strategy_warning=` — the strategy could not be written even though the name was valid (usually a
  config at that path we did not write). The proxy is fine; mention it and move on.
- `statusline=on` — also installed; mention it once, same as `recovery_dir=` (a `.gitignore`'d folder beside the routed file). `skipped` is not an install failure.

## 4. Then tell them

- **Do not say it only takes effect next session.** Claude Code picks the `env` change up live —
  that is why the proxy is started first. What is true: this session began before the proxy existed,
  so `/context-guru:status` may have nothing to show yet, and a new session is the clean way to look.
- Name the **cache strategy** from the result, and what it costs.
- Dashboard: `http://127.0.0.1:<port>/dashboard/` — the four billed token tiers are where the cache
  effect shows.
- **Name the port**, and that it is this project's own: `port_source=scanned` means allocated just
  now, `recorded` a re-run, `configured` their own pinning. One project per port is deliberate, so a
  number they did not pick is correct. `port_warning=` means it works but was not recorded — mention
  it, since a later install could hand the same port to another project.
- `/context-guru:status` for numbers, `/context-guru:cache-strategy-picker` to change the strategy,
  `/context-guru:uninstall` to undo.
- **`reset_hatch=` verbatim, on its own line, as your last line.** This is the only moment the user
  is certain to be able to read it: the failure it exists for is "every request through the proxy
  fails", and in that state no skill can run — including uninstall. It happened to a colleague.

  ```
  If Claude ever stops being able to talk after this, run: <the reset_hatch path>
  ```

  `reset_hatch=unavailable` means the state directory was unwritable — say so, because then their
  only undo is the `backup=` path.

## Do not

- **Do not investigate their machine.** No `env | grep` over credentials, not `ANTHROPIC_API_KEY`,
  not `AWS_*`; no `ps aux`, `lsof` or port scanning. An unbounded version of this was denied as
  `[Credential Exploration]` on a real install. This plugin does not read credentials and must not
  appear to — a proxy plugin sweeping for API keys is indistinguishable from the thing people are
  right to fear. The plan already tells you every environment fact you need.
- **Do not re-run the individual scripts** (`settings.py add`, `start-proxy.sh`) to do this by hand.
  The ordering between them is load-bearing and getting it wrong once killed the installing session.
- Do not add any other key. Not `ANTHROPIC_API_KEY`, not `ANTHROPIC_AUTH_TOKEN` — a credential
  variable is what would take them off subscription billing.
- Do not prefix the command with environment variables. Permission rules match by command PREFIX, so
  `FOO=1 .../install.sh` is a command nobody can approve. Every option is a flag for that reason.
- **Do not pass `--i-consent-to-traffic-interception` unless a person answered yes to a question you
  asked.** It is not a flag that makes a refusal go away; it is you telling the script, on their
  behalf, that they agreed to have their model traffic intercepted.
- Do not claim it works because a command exited 0. `result=routed` is the claim.
