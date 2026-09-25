---
name: install
description: Install a local context-guru proxy and route this project's Claude Code sessions through it, so long sessions stop paying to re-create the prompt cache. Also installs the terminal status line, in the same scope routing uses, on by default. Use when the user asks to install, set up, enable, try or start context-guru, or to route Claude Code through it. Accepts --global to route every project on the machine instead of just this one, --cache-strategy <none|5-min-ping|1-hour-head> to override the default cache strategy, --attach <url> to point at a proxy that already exists (a gateway or shared pod) instead of starting one, and --no-statusline to skip the automatic status line install.
---

# Install context-guru for Claude Code

<!-- Deliberately NO `allowed-tools:` here. The `!` block needs no tool permission, so the only thing
     such a line would grant is the ONE command that intercepts traffic — measured: with
     `Bash(.../install.sh)` declared it ran with no prompt. A plugin granting itself the permission
     the classifier exists to ask about answers a question that belongs to its user. -->

!`"${CLAUDE_PLUGIN_ROOT}/scripts/install.sh" --route --plan`

**The block above is this project's real state, gathered before you were asked anything.** It ran
during rendering, so you did not choose to run it and cannot have mistyped it — read it rather than
re-deriving any of it. It wrote nothing and started nothing.

**It ran with no flags, so it describes a project-scope install of the directory you are in.** If they
passed `--global` (or `--cache-strategy`, `--attach`, `--mode`), it is about a different install from
the one they asked for — wrong `port=`, wrong `file=`. Re-run it once with their flags (`--global` is
`--scope user`) and read THAT plan. Never translate this one into a machine-wide question.

Your job is the part a script does badly: putting one question to a human, and reporting honestly.
Everything else — the port, the proxy-before-routing order, the URL, the health check, the undo — is
in `install.sh --route`, as line order rather than a paragraph somebody can read differently.

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

**Read `result=` in the plan first.** A plan always exits 0 — `needs_decision` is for you to act on,
not a failure to report.

- `result=planned` — the plan is clean. If `already_routed=true`, say so: this is a re-run or a
  repair, not a fresh install.
- `result=needs_decision reason=base_url_already_set` — `existing_base_url=` names somebody's endpoint. It
  may be their company gateway, a benchmark endpoint or another proxy. **This is the one question**,
  and on a hosted agent the answer is nearly always chain: our proxy sits in front and theirs keeps
  handling auth and model routing. Replacing it outright usually breaks that agent's authentication.
- `result=needs_decision reason=user_scope_needs_flag` **or** `project_installs_exist` — the
  machine-wide question, and it is **one** question however it is labelled. The `reason=` says what
  is missing from argv; the `note=` and the `confirm_command_*` lines carry the whole thing. Two
  halves: the blast radius (**every** Claude Code session on the machine, including projects with
  nothing to do with context-guru, which is also every session they could use to fix it), and, when
  `existing_project=` lines are present, what becomes of the projects that route themselves — the
  machine-wide route will **not** reach them, because their own settings file is more specific and
  keeps winning, which is not what "everywhere" means to the asker. Ask both **here**: this works
  from inside one of those projects, so never answer it by sending them elsewhere. *Keep this
  project on its own settings and port, or fold it into the machine-wide one?* Then run
  `confirm_command_leave=` (both stay, each on its own port and config) or `confirm_command_adopt=`
  verbatim — `--i-understand-machine-wide` is already in both, so **you add nothing**, and do not
  re-plan to "collect" a flag you were told not to add.
  `pending_decision=base_url_already_set` means a second question is owed about `existing_base_url=`,
  so there is deliberately **no** `consent_question=` yet: ask this one, run the matching
  `plan_command_leave=`/`plan_command_adopt=` line, ask the conflict question from the plan that comes
  back. `adopt` reports `adopted_project=…
  unrouted=… recovery_dir=…` per project — read those rather than assuming. `unrouted=removed` is
  the success value; `unchanged` or `conflict` means that project kept its routing and must be
  reported as still overriding. Relay these per-project lines rather than folding them into
  "adopted": `adopted_project_still_overriding=<dir> base_url=<url>` (its own pre-context-guru URL
  was rightly restored, so it still overrides the route the user asked for);
  `adopted_proxy_left_running=<dir> port=<n>` (its old proxy still answers and its state was left
  alone — report the port); `adopted_project_is_this_project=<dir>` (they ran this from a project
  that was itself adopted, so its old proxy is stopped and the machine-wide one now serves it);
  `adopted_proxy_kept=<dir> port=<n>` (its proxy was on the port this install serves — a pinned
  port — so it stays). `adopted_proxy_stopped=` needs no comment.
- `result=needs_decision reason=port_owned_by_another_project` — `owner_project=` already has a proxy
  on that port, running its preset, and a proxy is never taken from its owner. Clear the `port` option
  so one is allocated, or ask for an unused one.
- `result=refused reason=port_changed_since_plan` — the port they agreed to is no longer the one this
  project gets. **Nothing was written.** Re-run `--route --plan` and ask again on the new port.
- `result=refused reason=port_recorded_by_another_project` — this project and `owner_project=` both
  have `port=` recorded, and picking one would put two projects with different presets on one proxy.
  **Nothing was written.** Report both and ask which keeps it; the other needs its `port` option
  cleared (or an uninstall) so a fresh one is allocated.
- `port_unwound=` on a failure — `true` (`port_unwound_parts=option,record`) means step 0's own
  bookkeeping was taken back, so "nothing was written" is literal. `nothing_to_undo` means a
  re-install failed and the working install's record and pinned `options.port` are **deliberately**
  still there — do not offer to clear them. Absent: they may be; say so, do not guess.
- `result=error reason=binary_install_failed` — read `detail=`. `no_release_found` wants the source
  build offer (`make build-static`, Go 1.26, no C toolchain). Anything naming a checksum
  (`checksum_mismatch`, `checksum_unavailable`, `checksum_absent`) is a **hard stop**: it is the only
  integrity check in the path and the binary is about to carry all of their LLM traffic. Never set
  `CONTEXT_GURU_INSECURE=1` on their behalf.

**One question, covering everything that needs their agreement — and you do not write it.** The
script generates it from the resolved facts and prints it (`consent_question*=`, below). No worked
example: a hand-written sentence beside a generated one is two sources for one question, and the
hand-written one drifts.

### Get an explicit yes, as a choice they pick

**`--route` refuses to do anything without `--i-consent-to-traffic-interception`.** Not a formality:
everything the command does intercepts their model traffic or points it somewhere new, and the
approval prompt cannot be relied on to ask — it is probabilistic, a skill can declare it away, and an
unattended session has no human to prompt. The script fails closed; the consent comes from a person.

**Ask it as a two-option choice, not as prose they can skim.** Use `AskUserQuestion` if you have it,
so it renders as something they pick rather than something they might answer sideways:

- **question**: what the `consent_question*=` line says, generated from the same resolved facts as the
  command it authorises. Say all of it — phrase it naturally, but every fact has to survive, and do not
  drop the money: a question narrower than the command it authorises is not consent to that command.
- **option 1 — "Yes, route this project"**, or **"Yes, route every project on this machine"** when
  the plan says `scope=user`: what they get, and that `/context-guru:uninstall` reverses it. A label
  naming a narrower blast radius than the command is the consent going missing in the one word they
  actually read.
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

There is deliberately **no** `consent_question=` or `confirm_command=` on that path: the answer is
the thing being asked for, so neither could be complete. Never add `--on-conflict` yourself; if the
paired lines are missing, re-run `--plan`.

**Otherwise, run the `confirm_command=` line from the plan, verbatim.** Copy it; do not retype it, do not
reorder it, and do not add or drop a flag. It is printed with every decision already resolved — scope,
mode, base URL, conflict, cache strategy, machine-wide acknowledgement — and it ends with
`--i-consent-to-traffic-interception`, so **the only thing you add is nothing.**

No example command here, deliberately: the one that used to be hardcoded `--on-conflict chain`
(wrong whenever nothing is already set) and spelled the path `"${CLAUDE_PLUGIN_ROOT}/…"`, which is
substituted in a `` !`` ``-block but **not** exported to a Bash tool call — so it could expand to
`/scripts/install.sh` and a model hitting "no such file" would improvise one. `confirm_command=`
carries the absolute path the script resolved from `$0`.
If a decision in it looks wrong, change the input to the plan and re-read the line it prints. Editing
the line by hand is how a decision the user made gets silently dropped.

**Expect this one command to be gated, and do not try to get around it.** Installing a plugin by
name is not consenting to have your model traffic intercepted, so auto mode is right to ask; the
command names its own scope and upstream, so approving it **is** the consent. If denied, hand them
three options and no fourth: approve the prompt, run that exact command themselves with `!`, or add
the plan's `permission_rule=`. Never reword the command to look like less than it is, and never
write routing while no proxy answers.

## 3. Read the result

- `result=routed` — done. `settings_result=` says which: `added`, `unchanged` (already correct),
  `completed` (a repair of an earlier partial attempt — report it as success, not "nothing to do"),
  or `repointed` (moved to a new port).
- `result=error reason=health_check_failed` — **no routing was written**, so the project is unrouted,
  which is a working project. Say what the log shows rather than guessing, and do **not** say
  "nothing happened": check `proxy_started=`, and if `true` a proxy IS still listening — say so and
  pass on `stop_command=` or `/context-guru:uninstall`. Health checks fail transiently, and a user
  told "nothing happened" retries into their own stale pidfile and an occupied port.
- `result=error reason=health_check_failed_after_write` with `rolled_back=true` — the **routing key**
  was removed automatically, so they are unrouted rather than broken. It undoes that key and nothing
  else: report `proxy_started=`, `pidfile=` and `strategy_file=` rather than implying a full undo.
- `result=error reason=settings_write_failed detail=unparseable_json` — their settings file was
  already broken. Do not rewrite it; tell them where it is.
- `result=error reason=settings_write_failed detail=base_url_already_set` — a **refusal**, not a broken
  file: something was already at that key and the run did not carry a decision authorising a replace.
  It should not be reachable once `--on-conflict` is passed, so treat it as a bug worth reporting
  rather than something to retry with a flag you chose yourself. Check `proxy_started=` — the proxy
  starts before this step, so one may be running.
- `result=refused reason=consent_required` — you ran it without the flag, or without asking. Nothing
  was installed, started or written. Go back and ask; do not simply re-run it with the flag appended.
- `result=refused reason=unknown_strategy` — the `--cache-strategy` name does not exist (e.g.
  `5-minute-ping` for `5-min-ping`). Nothing was written; `note=` lists the real names. Ask which they
  meant — do not pick, because the names differ in whether they spend the user's quota.
- `strategy_warning=` — the strategy could not be written even though the name was valid (usually a
  config at that path we did not write). The proxy is fine; mention it and move on.
- `statusline=on` — also installed; mention it once, same as `recovery_dir=` (a `.gitignore`'d folder beside the routed file). `skipped` is not an install failure.

## 4. Then tell them

- **Do not say it only takes effect next session.** Claude Code picks the `env` change up live, which
  is why the proxy starts first. What is true: this session began before the proxy existed, so
  `/context-guru:status` may have nothing to show yet; a new session is the clean way to look.
- Name the **cache strategy** from the result, and what it costs.
- Dashboard: `http://127.0.0.1:<port>/dashboard/` — the four billed token tiers are where the cache
  effect shows.
- **Name the port**, and whose it is: `port_source=scanned` means allocated just now, `recorded` a
  re-run, `configured` their own pinning. One project per port is deliberate, so a number they did
  not pick is correct. `port_warning=` means it works but was not recorded — mention it, since a
  later install could hand the same port to another project.
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
  not `AWS_*`; no `ps aux`, `lsof` or port scanning — an unbounded version was denied as
  `[Credential Exploration]` on a real install. A proxy plugin sweeping for API keys is
  indistinguishable from the thing people are right to fear. The plan already tells you every
  environment fact you need.
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
