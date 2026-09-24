#!/usr/bin/env bash
# Put a context-guru-proxy binary on the user's PATH, without a toolchain.
#
# The binary is statically linked pure Go (scripts/gate-a-purego.sh proves it, and the release
# workflow asserts it on every run), so this is a download, a checksum, and a move. No
# compiler, no runtime dependencies, nothing to configure.
#
# Ranked by friction, and this script tries them in that order:
#
#   1. ALREADY INSTALLED — do nothing, report the version. This script is idempotent because
#      the install skill may be re-run, and re-downloading 30 MB to end up where we started is
#      not a neutral act on someone's laptop.
#   2. RELEASE TARBALL — the default path. Checksum-verified against the release's
#      checksums.txt, and on macOS the quarantine attribute is stripped, otherwise Gatekeeper
#      refuses an unsigned download with "cannot be verified" and the trial ends there.
#   3. go install — only if a Go toolchain is already present. Cheap to offer, and some people
#      would rather build.
#
# There is deliberately NO `brew` path yet: the tap repo does not exist and release signing is
# an open ownership question (see the spec). A `brew install` line that fails is worse than one
# that is absent, and adding the tap later changes nothing else here.
#
# Every fact this discovers is printed as `key=value` on stdout, so the calling skill can act
# on the outcome without parsing prose.
set -uo pipefail

REPO="${CONTEXT_GURU_REPO:-rossoctl/context-guru}"
VERSION="${CONTEXT_GURU_VERSION:-latest}"
DEST="${CONTEXT_GURU_DEST:-$HOME/.local/bin}"
BIN=context-guru-proxy

emit() { printf '%s\n' "$*"; }
die()  { emit "result=error"; emit "reason=$1"; exit 1; }

# =========================================================================================
# --route: the orchestrator
# =========================================================================================
#
# Steps 1-7 of skills/install/SKILL.md, as ONE script. The argument for moving them here is in
# docs/superpowers/specs/2026-09-14-plugin-install-orchestrator-design.md; the short version is
# that every defect in that skill's history has the same shape — the prose was right and the
# command a model emitted differed from it — and an ordering expressed as a numbered paragraph is
# a request, while the same ordering expressed as line order in a script is a fact.
#
# Two modes:
#   --mode local  (default)  install the binary, start a proxy on 127.0.0.1:<resolved port>,
#                            route to it. The URL is DERIVED and never passed in.
#   --mode attach            the proxy already exists elsewhere (a DAM gateway, a shared pod).
#                            Steps 1 and 5 are SKIPPED — nothing installed, nothing started —
#                            and --base-url is supplied and validated.
#
# Everything that needs a human is a FLAG, and there are exactly two such decisions: --scope and
# --on-conflict. They travel in argv rather than in a state file, deliberately: a gated command
# that reads its decisions from a file elsewhere shows the user nothing when they are asked to
# approve it, which turns one meaningful consent into two meaningless ones.
#
# SELF-LOCATION, and why it is not $CLAUDE_PLUGIN_ROOT: measured 2026-09-14, that variable is
# substituted into a `!`-block's command STRING but is NOT exported to the child process. A script
# reading it from its own environment gets nothing. So this locates its siblings from $0.

route_here() { CDPATH= cd -- "$(dirname -- "$0")" && pwd -P; }

# Every fact the plan and the confirm both report. Kept in one place so `--plan` cannot describe a
# different install from the one `--confirm` performs.
R_MODE=local R_SCOPE=project R_ONCONFLICT= R_BASEURL= R_HEALTHURL= R_NOHEALTH=0
R_STRATEGY= R_UPSTREAM= R_USERSCOPE=0 R_PLAN=0 R_CONFIRM=0 R_NOSTATUSLINE=0 R_NOGITIGNORE=0
R_PORT= R_PRESET= R_IDLE= R_BIN= R_ONPATH= R_FILE= R_EXISTING= R_CHAINED=false
R_ALREADY=false R_CONSENT=0 R_OURS= R_FROMENV=0 R_SPENDS= R_PIDFILE= R_PROXYLOG=
R_ONEXISTING= R_PORTSOURCE= R_EXISTINGPROJECTS=

route_die() { emit "result=error"; emit "reason=$1"; [ -n "${2:-}" ] && emit "detail=$2"; exit 3; }

# route_refuse is for the WRITING path: exit 2, a refusal a caller can distinguish from a failure.
#
# route_needs is for anything the PLAN discovers, and under --plan it exits 0. That distinction is
# not cosmetic, and getting it wrong made this script unusable in the exact shape it was designed
# for: the install skill runs `--route --plan` from a `!` block, the commonest real state of a
# hosted machine is "a base URL is already set", and the first version exited 2 there. Measured in a
# real sandboxed session, the whole skill invocation then produced NO OUTPUT AT ALL — the model
# never saw the plan, never asked the question, and the run looked like a success because nothing
# had been written.
#
# So: a plan REPORTS. `needs_decision` is data for the caller to act on, not an error to propagate,
# and only a command that actually declines to do something exits non-zero.
route_refuse() { emit "result=refused"; emit "reason=$1"; [ -n "${2:-}" ] && emit "note=$2"; exit 2; }
route_needs() {
  if [ "$R_PLAN" = 1 ]; then
    emit "result=needs_decision"; emit "reason=$1"
    [ -n "${2:-}" ] && emit "note=$2"
    [ -n "$R_EXISTING" ] && emit "existing_base_url=$R_EXISTING"
    emit "permission_rule=$(route_permission_rule)"
    exit 0
  fi
  route_refuse "$1" "${2:-}"
}

# One value from a key=value block, last occurrence wins (install.sh prints result= last).
kv() { printf '%s\n' "$1" | sed -n "s/^$2=//p" | tail -1; }

route_scope_file() {
  case "$R_SCOPE" in
    project) printf '%s\n' "$PWD/.claude/settings.local.json" ;;
    team)    printf '%s\n' "$PWD/.claude/settings.json" ;;
    user)    printf '%s\n' "${CLAUDE_CONFIG_DIR:-$HOME/.claude}/settings.json" ;;
    # Returns non-zero WITHOUT emitting. It used to call route_refuse here, and this function is
    # invoked as `R_FILE=$(route_scope_file)` - so every line route_refuse printed was captured into
    # the command substitution, assigned to R_FILE and thrown away. `--scope bogus` produced NO OUTPUT
    # AT ALL and exit 2, which is the exact defect this design was built to remove, reproduced for a
    # different flag. The caller emits, outside the substitution, where output survives.
    *)       return 2 ;;
  esac
}

# The rule the plugin cannot enforce for itself: a permission rule covers a command by PREFIX, so
# one rule over this directory covers every command the install runs. Printed as a fact so the
# absolute path is never hand-typed by a model.
route_permission_rule() { printf 'Bash(%s/**)\n' "$(dirname -- "$(route_here)")"; }

route_resolve_options() {
  local cfg
  cfg=$("$(route_here)/settings.py" config 2>/dev/null) || cfg=""
  # Per-option fallback, never "source= was set so everything is set". `settings.py config` prints
  # a line only for keys the user actually configured, so a partial config — port set, preset
  # never touched — reports a real source= and simply omits option_preset=.
  # The port is no longer a per-machine default. It used to be 8787 for EVERY project, so two
  # projects with different presets shared one proxy and start-proxy.sh took turns killing it,
  # wiping the in-memory store on every session start. `settings.py port alloc` is now the single
  # encoding of which port this project gets, in this order: a port already recorded for it (a
  # reinstall must never move a port whose URL is already written into a settings file), then one
  # explicitly configured in pluginConfigs, then a free one. Asked as `--dry-run` because this runs
  # on the PLAN path too, and a plan must not commit this project to a port — or take it out of
  # every other project's pool — before consent has been asked. The real allocation happens after
  # the consent gate, pinned to the port this reported.
  #
  # Fails open to the old behaviour: with settings.py unavailable, the configured option and then
  # 8787, exactly as before. A port we cannot allocate is not a reason to refuse an install.
  local pout
  pout=$("$(route_here)/settings.py" port alloc --dry-run 2>/dev/null) || pout=""
  R_PORT=$(kv "$pout" port)
  R_PORTSOURCE=$(kv "$pout" source)
  case "$R_PORT" in
    ''|*[!0-9]*) R_PORT=$(kv "$cfg" option_port); R_PORTSOURCE=fallback ;;
  esac
  : "${R_PORT:=8787}"
  : "${R_PORTSOURCE:=fallback}"
  R_PRESET=$(kv "$cfg" option_preset);        : "${R_PRESET:=off}"
  R_IDLE=$(kv "$cfg" option_idle_exit);       : "${R_IDLE:=24h}"
  [ -z "$R_STRATEGY" ] && R_STRATEGY=$(kv "$cfg" option_cache_strategy)
  : "${R_STRATEGY:=5-min-ping}"
  [ -z "$R_UPSTREAM" ] && R_UPSTREAM=$(kv "$cfg" option_upstream)
}

route_inspect() {
  local shown
  R_FILE=$(route_scope_file) \
    || route_refuse "unknown_scope" "--scope must be project, team or user; got '$R_SCOPE'"
  shown=$("$(route_here)/settings.py" show --file "$R_FILE" 2>/dev/null) || shown=""
  R_EXISTING=$(kv "$shown" base_url)
  R_OURS=$(kv "$shown" ours)
  # `settings.py show` prints the SENTINEL `(unset)` rather than an empty value, and reading that
  # as a real base URL made every clean project look already-routed — so the install refused with
  # `base_url_already_set` and named `(unset)` as the conflicting endpoint. Caught by the first
  # smoke run; it is the exact class of bug this script exists to remove, arriving in the script
  # itself. Normalise every "absent" spelling the siblings use, not just the one seen.
  case "$R_EXISTING" in "(unset)"|"(none)"|"") R_EXISTING= ;; esac
  # The environment matters as much as the file: on a hosted or containerised agent the base URL is
  # often set in the process environment and no settings file mentions it at all, so `show` reports
  # exists=false while the session is already routed somewhere.
  if [ -z "$R_EXISTING" ] && [ -n "${ANTHROPIC_BASE_URL:-}" ]; then
    R_EXISTING="$ANTHROPIC_BASE_URL"
    R_FROMENV=1
  fi
  # Already ours is NOT a conflict — but it is not nothing either, and reporting it as an empty
  # existing_base_url would let a re-run be narrated as a fresh install. Say which it is.
  #
  # This asked the URL's SHAPE (`*127.0.0.1:$R_PORT*`), which is the inference valid_base_url()'s own
  # docstring forbids twenty lines away in the sibling this calls: two local proxies are
  # indistinguishable by URL. So somebody else's proxy on our port was reported as ours, with
  # `existing_base_url=` emptied — the one question this design says can never be defaulted was never
  # asked, and the skill narrated it as "a re-run or a repair". Port 4000 would have claimed litellm's
  # own endpoint, the exact collision the docstring names.
  #
  # `show` now reports `ours=`, answered from the recorded installed_base_url. Prefer it whenever the
  # value came from the FILE, which is the case the shape test got wrong.
  if [ "$R_OURS" = true ]; then
    R_EXISTING=; R_ALREADY=true
  elif [ "$R_FROMENV" = 1 ]; then
    # The value came from the ENVIRONMENT, so there is no record to consult and shape is the only
    # signal there is. Kept, but anchored on `//host:port/` — the old unanchored match made port 8787
    # claim an endpoint on 87870. Stated plainly as the residual inference: a foreign proxy on our
    # port, injected through the environment rather than a file, still reads as ours here.
    case "$R_EXISTING" in
      *"//127.0.0.1:${R_PORT}/"*|*"//localhost:${R_PORT}/"*) R_EXISTING=; R_ALREADY=true ;;
    esac
  fi
  # A value in the FILE that is not ours stays exactly where it is: a conflict, reported, and
  # answerable only by a human. That is the case the shape match silently swallowed.
}

# The URL is DERIVED in local mode and never accepted as an argument, which is what makes the
# malformed-URL class unreachable from this path. In attach mode it is supplied, and validated by
# settings.py's valid_base_url() before anything is written.
route_url() {
  if [ "$R_MODE" = attach ]; then printf '%s\n' "$R_BASEURL"
  else printf 'http://127.0.0.1:%s/anthropic\n' "$R_PORT"; fi
}

route_health_url() {
  if [ -n "$R_HEALTHURL" ]; then printf '%s\n' "$R_HEALTHURL"
  elif [ "$R_MODE" = attach ]; then
    # Strip the trailing /anthropic and ask for /healthz beside it. Not assumable on a gateway,
    # which is why --health-url and --no-health-check both exist.
    printf '%s\n' "${R_BASEURL%/anthropic}/healthz"
  else printf 'http://127.0.0.1:%s/healthz\n' "$R_PORT"; fi
}

route_health_ok() {
  [ "$R_NOHEALTH" = 1 ] && return 0
  command -v curl >/dev/null 2>&1 || return 0   # fail open: no curl is not evidence of a dead proxy
  curl -fsS --max-time 5 "$(route_health_url)" >/dev/null 2>&1
}

# The exact command that would perform this install, decisions included. Printed by the plan and by
# the consent refusal so nothing has to be reassembled by hand — the failure mode on 2026-09-14 was a
# model re-typing a command and dropping part of it.
# Quote a value so it cannot contribute SYNTAX to the line we print for someone to run. Values that
# are already unambiguous pass through untouched, because the place this line is most likely to be
# read is a permission prompt shown to a human, and `--scope 'project'` reads worse than `--scope
# project` while being no safer.
#
# This is not hypothetical tidiness. `--base-url` was interpolated raw, valid_base_url() checked the
# host only for emptiness, and the two together were an arbitrary-command hole: a `check-url`-approved
# `http://x;touch /tmp/PWNED;cd /anthropic` produced a confirm_command whose payload EXECUTED when the
# line was run the way SKILL.md and the suite both run it. It fired regardless of the consent answer,
# because it rides the string the *plan* prints and the gate is downstream in route_main. The host
# character class in settings.py closes today's vector; this closes the class, so the next value added
# to this line cannot reopen it.
shq() {
  case "$1" in
    ""|*[!A-Za-z0-9._:/=@,+-]*) printf "'%s'" "${1//\'/\'\\\'\'}" ;;
    *)                          printf '%s' "$1" ;;
  esac
}

# Every flag that takes a value goes through this. `${2:?...}` wrote BASH's diagnostic to stderr and
# exited 1 - no `result=`, nothing on stdout - which is the third recorded instance of this design's
# oldest defect shape: `--plan` exiting 2 with no output, `--scope bogus` swallowed by a command
# substitution, and this. Closed as a class rather than one flag at a time. Its message was also the
# wrong artefact even when someone saw stderr: `line 623: 2: --scope needs a value` leads with bash's
# parameter name and a line number, so it reads as an internal error rather than a fact about input.
#
# NOT written as `R_SCOPE=$(need --scope "$2")`, which is the shape that looks natural and reproduces
# the bug being fixed: inside `$(...)` the emitted lines are captured into the variable and `exit` ends
# only the subshell, so the script would carry on with the refusal text as the flag's value. This
# validates in the CALLING shell and the branch assigns separately.
route_need_value() {   # route_need_value <flag> <value-or-empty>
  [ "$#" -ge 2 ] && [ -n "$2" ] && return 0
  emit "result=error"; emit "reason=missing_value"; emit "flag=$1"
  emit "note=$1 takes a value; refused rather than defaulted. A re-typed command is usually cut at \
the END, so a dropped value is at least as likely as a dropped flag - and a dropped flag already \
reports unknown_flag here."
  exit 2
}

route_confirm_command() {
  local c="$(shq "$(route_here)/install.sh") --route --scope $(shq "$R_SCOPE")"
  [ "$R_MODE" != local ] && c="$c --mode $(shq "$R_MODE")"
  [ -n "$R_BASEURL" ] && c="$c --base-url $(shq "$R_BASEURL")"
  [ -n "$R_ONCONFLICT" ] && c="$c --on-conflict $(shq "$R_ONCONFLICT")"
  # The strategy was missing, and it is the one decision that spends the user's money. Dropped from
  # here, a user who asked for `none` ("install it, but do not spend my quota") ran the printed
  # command, the strategy re-resolved to the `5-min-ping` default, and the install reported
  # `result=routed` — success, while doing the opposite of what was asked. The consent artefact has
  # to name the whole proposition the user was asked to agree to, not just the interception.
  [ -n "$R_STRATEGY" ] && c="$c --cache-strategy $(shq "$R_STRATEGY")"
  # --upstream, --health-url and --no-health-check were missing, and this is the same defect as the
  # --cache-strategy drop: a decision that travels in argv, absent from the printed line, silently
  # re-resolved to something else by the run that performs it. --upstream is the consequential one,
  # because it decides what gets WRITTEN: with `--upstream <gateway>` on the plan and a conflicting
  # existing endpoint, the printed command dropped it and the confirm run fell back to the endpoint
  # being displaced - so the file ended up naming an upstream the caller never asked for.
  #
  # Found by running the check the reviewer prescribed for a different finding: compare the consent
  # question against ANTHROPIC_UPSTREAM in the file the install writes. The question was right and the
  # file was wrong, which is the opposite of the defect being looked for.
  [ -n "$R_UPSTREAM" ] && c="$c --upstream $(shq "$R_UPSTREAM")"
  [ -n "$R_HEALTHURL" ] && c="$c --health-url $(shq "$R_HEALTHURL")"
  [ "$R_NOHEALTH" = 1 ] && c="$c --no-health-check"
  [ "$R_NOGITIGNORE" = 1 ] && c="$c --no-gitignore-check"
  [ "$R_USERSCOPE" = 1 ] && c="$c --i-understand-machine-wide"
  [ -n "$R_ONEXISTING" ] && c="$c --on-existing-projects $(shq "$R_ONEXISTING")"
  printf '%s --i-consent-to-traffic-interception\n' "$c"
}

# The proposition a human is asked to agree to, generated from the SAME resolved facts as the command
# above rather than composed from the skill's example paragraph. Two things drifted apart before this
# existed: the question mentioned the spending strategy and the gated command did not.
# The upstream route_main will ACTUALLY write: an explicit --upstream (or option_upstream) wins, and
# `chain` otherwise falls back to the endpoint being displaced. One encoding, because there were two -
# route_main defaulted only when R_UPSTREAM was empty while the question generator overrode it
# unconditionally. With a configured upstream those disagreed: the chain question named the DISPLACED
# endpoint as the one being kept and never named the gateway that actually ends up behind the proxy,
# and the replace question lost its REPLACING clause entirely because a non-empty R_UPSTREAM made that
# branch unreachable.
#
# Same shape as the --force gap and the state-dir drift: one rule, two encodings, and the copy drifts.
route_effective_upstream() {
  if [ -n "$R_UPSTREAM" ]; then printf '%s\n' "$R_UPSTREAM"
  elif [ "$R_ONCONFLICT" = chain ]; then printf '%s\n' "$R_EXISTING"
  fi
}

route_consent_question() {
  local q="route this project's model traffic through $(route_url)"
  [ "$R_SCOPE" = user ] && q="route THIS MACHINE's model traffic (every project) through $(route_url)"
  # Two clauses, deliberately independent, because they answer different questions: what becomes of
  # the endpoint the user already has, and which gateway ends up behind the proxy. Chaining them with
  # elif made a configured --upstream suppress the REPLACING sentence altogether - the endpoint was
  # still being displaced, and the question stopped saying so.
  local up; up=$(route_effective_upstream)
  if [ -n "$R_EXISTING" ]; then
    if [ "$up" = "$R_EXISTING" ]; then
      q="$q, keeping $R_EXISTING as the upstream so it still handles auth"
    else
      # Includes `chain` WITH an explicit --upstream: the proxy forwards to that instead, so their
      # endpoint is displaced rather than kept, however the conflict decision was spelled.
      q="$q, REPLACING $R_EXISTING (recorded, and /context-guru:uninstall puts it back)"
    fi
  fi
  # Always says where traffic actually goes next, even when that is "nowhere configured" - an
  # empty upstream is silent about the fact that it means api.anthropic.com, and that silence is
  # exactly what read as "still uses $R_EXISTING for outbound" to a human comparing the two options.
  if [ -n "$up" ] && [ "$up" != "$R_EXISTING" ]; then
    q="$q, forwarding on to $up as the upstream so it handles auth"
  elif [ -z "$up" ]; then
    q="$q, forwarding straight to api.anthropic.com"
  fi
  if [ "$R_MODE" = attach ]; then
    q="$q (attach mode: nothing is started, the URL is assumed to be already serving)"
  fi
  # Read from `strategy list`, never restated here. Hardcoding the names was correct for all three
  # that exist, and the catch-all asserted "(no spend)" about every name it had not been told about -
  # so a fourth strategy that spends, or 1-hour-head gaining a ping, would make this state something
  # false in the one sentence a human is asked to agree to. consent_question= and confirm_command=
  # could no longer disagree with each other; this is consent_question= disagreeing with STRATEGIES.
  #
  # An unknown spend status reads as SPENDING, not as free: the sentence a user consents to is the
  # wrong place to resolve an uncertainty in their favour.
  case "$R_SPENDS" in
    true)  q="$q, with cache strategy $R_STRATEGY, which SPENDS THE USER'S OWN QUOTA on idle \
turns to hold the cache warm" ;;
    false) q="$q, with cache strategy $R_STRATEGY (no spend)" ;;
    *)     q="$q, with cache strategy $R_STRATEGY (SPEND STATUS UNKNOWN - treat it as spending \
until Usage says otherwise)" ;;
  esac
  printf '%s\n' "$q"
}

# This mirrors settings.py's state_dir(), which is what writes the strategy config - NOT
# start-proxy.sh's, which has a `|| STATE=$TMPDIR` fallback this deliberately does not copy. Each path
# is now derived from whatever writes it: the strategy file from here, and the pidfile from the line
# start-proxy.sh prints under --emit-facts. Copying one writer's expression to find another writer's
# file is what drifted, and predicting that the symptom would be "a missing line rather than a wrong
# path" was correct and no comfort at all - the missing line was the whole function.
route_state_dir() {
  printf '%s\n' "${CONTEXT_GURU_STATE:-${XDG_STATE_HOME:-$HOME/.local/state}/context-guru}"
}

# What steps 4b and 5 may have left behind, reported on every failure path.
#
# "SETTINGS WERE NOT TOUCHED" is true, and "nothing happened" is what a caller infers from it. A proxy
# left listening on a port, with a pidfile, a dashboard DB and a strategy config, is not nothing — and
# the skill was instructing the model to say "nothing was written; the project is unrouted, which is a
# working project", whose first clause was false. It matters most when the health check fails
# TRANSIENTLY (a slow start, a busy laptop): the user is told nothing happened, and their retry meets a
# stale pidfile on an occupied port.
#
# This reports rather than cleans up. Stopping a proxy needs the pidfile-first, ownership-confirmed path
# that /context-guru:uninstall already owns, and half of one here would be worse than an honest line.
route_report_side_effects() {
  local st pf sf pid
  st=$(route_state_dir)
  # Straight from the script that wrote it. Falls back to the derived path only when step 5 never ran
  # (attach mode, or a failure before it), where there is no proxy of ours to report anyway.
  pf="$R_PIDFILE"
  [ -n "$pf" ] || pf="$st/proxy-${R_PORT}.pid"
  sf="$st/keepalive-${R_PORT}.yaml"
  [ -n "$R_PROXYLOG" ] && emit "proxy_log=$R_PROXYLOG"
  if [ -f "$pf" ]; then
    emit "pidfile=$pf"
    pid=$(cat "$pf" 2>/dev/null)
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      emit "proxy_started=true"
      emit "proxy_pid=$pid"
      emit "stop_command=kill $pid"
      emit "side_effect_note=A PROXY IS STILL RUNNING on port ${R_PORT} and was NOT stopped. Say so; \
do not tell the user nothing happened. /context-guru:uninstall stops it, or run stop_command above."
    else
      emit "proxy_started=false"
      emit "side_effect_note=a pidfile is present with nothing running behind it, so a retry meets a \
stale pidfile on port ${R_PORT}."
    fi
  fi
  [ -f "$sf" ] && emit "strategy_file=$sf"
  return 0
}

route_report() {
  emit "mode=$R_MODE"
  emit "scope=$R_SCOPE"
  emit "file=$R_FILE"
  emit "port=$R_PORT"
  emit "port_source=$R_PORTSOURCE"
  emit "preset=$R_PRESET"
  emit "idle_exit=$R_IDLE"
  emit "cache_strategy=$R_STRATEGY"
  emit "base_url=$(route_url)"
  emit "health_url=$(route_health_url)"
  emit "existing_base_url=$R_EXISTING"
  emit "already_routed=$R_ALREADY"
  emit "chained=$R_CHAINED"
  emit "upstream=$R_UPSTREAM"
  emit "permission_rule=$(route_permission_rule)"
  emit "consent_required=true"
  if [ "${1:-}" = per_conflict_answer ]; then
    # No R_UPSTREAM override: route_consent_question derives it the way route_main does. Overriding it
    # here was the second encoding that drifted.
    emit "consent_question_chain=$(R_ONCONFLICT=chain route_consent_question)"
    emit "consent_question_replace=$(R_ONCONFLICT=replace route_consent_question)"
  else
    emit "consent_question=$(route_consent_question)"
  fi
  # On the ONE path that asks a question, the answer is not in R_ONCONFLICT yet - so a single
  # confirm_command would be printed WITHOUT --on-conflict, and running it verbatim just re-hits the
  # same refusal. SKILL.md says the line carries every decision and that the caller adds nothing;
  # that was false here, and the way out for a model would have been to compose the flag itself,
  # which is the model-composed-syntax risk this whole design exists to remove.
  #
  # So the script prints one runnable command PER ANSWER and the model picks, never composes.
  if [ "${1:-}" = per_conflict_answer ]; then
    # Paired one-to-one, and generated together, so the question a user answers and the command that
    # answer runs cannot describe different things. `chain` and `replace` are genuinely different
    # propositions and that difference IS what the user is being asked to decide.
    emit "confirm_command_chain=$(R_ONCONFLICT=chain route_confirm_command)"
    emit "confirm_command_replace=$(R_ONCONFLICT=replace route_confirm_command)"
    # No `confirm_command=` and no `consent_question=` on this path, deliberately. Both keys mean one
    # thing everywhere else in this script - a line that RUNS, and the whole proposition - and
    # assigning either an explanatory sentence would be the value most likely to be re-read as if it
    # were the real thing. Absence is unambiguous; the paired keys are self-describing.
  else
    emit "confirm_command=$(route_confirm_command)"
  fi
}

route_main() {
  route_resolve_options

  # A typo'd strategy name used to reach step 4b, where `settings.py strategy set` correctly refused it
  # with exit 2 — and `|| true` plus a catch-all `*)` turned that refusal into `strategy_warning=`.
  # No config file written means `none`, so `--cache-strategy 5-minute-ping` installed a DIFFERENT
  # mechanism from the one named and reported `result=routed`. That is the same shape as the unknown
  # flag this script refuses eighty lines down: a dropped decision that reports success. Checked here,
  # before a binary is downloaded, against the one machine-readable list of names.
  local slist names
  slist=$("$(route_here)/settings.py" strategy list 2>/dev/null) || slist=""
  names=$(printf '%s\n' "$slist" | sed -n 's/^names=//p')
  # An empty list is NOT an unknown name, and conflating them made the script state a false thing
  # about the caller's input: with settings.py unavailable, `--cache-strategy 5-min-ping` - the
  # default, and obviously a strategy - was refused as "is not a strategy. Known: unavailable", with
  # the one fault the user could act on buried as a word inside a sentence about a typo. The skill's
  # next move is to ask which name they meant, about a name that was right. `${names:-unavailable}`
  # showed the empty case was anticipated; it just routed into the wrong reason.
  if [ -z "$names" ]; then
    route_needs "strategy_list_unavailable" "could not read the strategy list, so --cache-strategy \
$R_STRATEGY was not checked and cache_strategy= in this report may be wrong too. This is a broken \
plugin install rather than a bad argument: check python3 and $(route_here)/settings.py. Nothing was \
installed, started or written."
  fi
  case ",${names}," in
    *",${R_STRATEGY},"*) : ;;
    *) route_needs "unknown_strategy" "--cache-strategy $R_STRATEGY is not a strategy. Known: \
${names}. Nothing was installed, started or written." ;;
  esac
  # Whether this strategy spends the user's own money is half of what its NAME means, and STRATEGIES
  # is where that is decided - so it is read from the same output as the names rather than restated
  # here. Fact keys mangle `-` to `_`.
  R_SPENDS=$(printf '%s\n' "$slist" | sed -n "s/^spends_${R_STRATEGY//-/_}=//p")

  if [ "$R_MODE" = attach ]; then
    [ -n "$R_BASEURL" ] || route_needs "attach_needs_base_url" \
      "--mode attach has no local port to derive a URL from; pass --base-url"
    # Validate BEFORE doing anything, not at the write. `attach` exists precisely so a human can
    # type a URL, and the first version of this only found a typo at step 7 — after a health check
    # had been spent — reporting it as `settings_write_failed`, which names the wrong step.
    local vout
    vout=$("$(route_here)/settings.py" check-url --url "$R_BASEURL" 2>&1) || {
      if [ "$R_PLAN" = 1 ]; then
        emit "result=needs_decision"; emit "reason=invalid_base_url"
        emit "detail=$(kv "$vout" detail)"; emit "url=$R_BASEURL"
        emit "note=the supplied --base-url cannot be used; nothing was checked or written"
        exit 0
      fi
      emit "result=refused"; emit "reason=invalid_base_url"
      emit "detail=$(kv "$vout" detail)"; emit "url=$R_BASEURL"
      emit "note=nothing was started and nothing was written. In attach mode the URL is the one \
thing that cannot be derived, so it is the one thing checked first."
      exit 2
    }
  else
    [ -z "$R_BASEURL" ] || route_needs "base_url_is_local_mode_nonsense" \
      "--base-url is for --mode attach; in local mode the URL is derived from the resolved port"
  fi
  [ "$R_SCOPE" = user ] && [ "$R_USERSCOPE" != 1 ] && route_needs "user_scope_needs_flag" \
    "--scope user routes EVERY project on this machine, including every project that has nothing to \
do with context-guru. Confirm with the user, then add --i-understand-machine-wide"

  # A user-scope install meets the projects that already installed themselves. The machine-wide
  # route this is about to write will NOT reach them: a project's own settings file is more
  # specific, so it simply keeps winning, and the user who just asked for "everywhere" gets
  # everywhere-except-these with nothing said about it. Only they can say whether that is what they
  # meant, so it is a gate and not a note — and, like every other decision in this script, the
  # answer arrives as a flag rather than as prose a model interprets.
  # The list is read whenever the scope is user, NOT only when the question still needs asking:
  # `adopt` is performed at the very end of this script from exactly this variable, and reading it
  # only on the asking path left the confirming re-run — the one that carries the answer — with an
  # empty list and an `adopt` that silently adopted nothing while reporting success.
  if [ "$R_SCOPE" = user ]; then
    R_EXISTINGPROJECTS=$("$(route_here)/settings.py" scopes 2>/dev/null \
                           | sed -n '/^existing_project=/p' | grep -v ' scope=user ') || true
  fi
  if [ "$R_SCOPE" = user ] && [ -z "$R_ONEXISTING" ] && [ -n "$R_EXISTINGPROJECTS" ]; then
    # Emitted through the same two shapes as every other gate: needs_decision under --plan (the
    # caller has a question to ask, not an error to propagate), a refusal otherwise. route_report is
    # NOT called here, unlike the base-url gate further down: this runs before route_inspect, so
    # file=, existing_base_url= and already_routed= have no values yet and reporting them empty
    # would state something false about the install rather than nothing.
    if [ "$R_PLAN" = 1 ]; then
      emit "result=needs_decision"; emit "reason=project_installs_exist"
    else
      emit "result=refused"; emit "reason=project_installs_exist"
    fi
    printf '%s\n' "$R_EXISTINGPROJECTS" | while IFS= read -r l; do emit "$l"; done
    emit "port=$R_PORT"
    emit "note=these projects route themselves and will keep OVERRIDING the machine-wide route this \
install writes. Ask the user, then re-run with --on-existing-projects leave (they keep their own \
port and config, which is the safe answer) or adopt (their own routing and port are removed so they \
fall back to the machine-wide one; every file is backed up first). Nothing was installed, started \
or written."
    emit "permission_rule=$(route_permission_rule)"
    emit "confirm_command_leave=$(R_ONEXISTING=leave route_confirm_command)"
    emit "confirm_command_adopt=$(R_ONEXISTING=adopt route_confirm_command)"
    [ "$R_PLAN" = 1 ] && exit 0
    exit 2
  fi

  # A port this project is about to route to, held by a proxy another project owns. Only reachable
  # for a port that was explicitly configured or recorded — the scan skips every port another
  # project recorded — which is exactly the case that must not be resolved silently: allocation
  # cannot move a port the user chose, and start-proxy.sh (correctly) will not kill a proxy it does
  # not own, so an install that proceeded here would route this project at a proxy running somebody
  # else's configuration and report success.
  local owner_file owner_key
  owner_file="$(route_state_dir)/proxy-${R_PORT}.owner"
  owner_key=$(cat "$owner_file" 2>/dev/null) || owner_key=""
  if [ -n "$owner_key" ] && [ "$owner_key" != "$("$(route_here)/settings.py" project-key 2>/dev/null | sed -n 's/^key=//p' | head -1)" ]; then
    emit "owner_project=$owner_key"
    route_needs "port_owned_by_another_project" \
      "port $R_PORT is serving another project ($owner_key), and a proxy is never taken from the \
project that owns it. Clear the port= option for this project so one is allocated for it, or pick \
an unused port explicitly. Nothing was installed, started or written."
  fi

  route_inspect

  # THE ONE DECISION THAT CANNOT BE DEFAULTED. A base URL already set may be their company gateway,
  # a benchmark endpoint or another proxy; replacing it silently breaks their setup while looking
  # like success, and guessing which it is is not something a script can do.
  if [ -n "$R_EXISTING" ] && [ -z "$R_ONCONFLICT" ]; then
    if [ "$R_PLAN" = 1 ]; then
      # THE COMMON CASE on a hosted machine, and the reason route_needs exists. Report the whole
      # plan alongside it: the model has one question to ask and needs the real values to ask it.
      emit "result=needs_decision"; emit "reason=base_url_already_set"
      emit "note=ask the user, then re-run with --on-conflict chain (usually right: our proxy sits \
in front and theirs keeps handling auth), replace (theirs is recorded and uninstall puts it back), \
or abort"
      route_report per_conflict_answer
      exit 0
    fi
    emit "result=refused"; emit "reason=base_url_already_set"
    emit "existing_base_url=$R_EXISTING"
    emit "note=pass --on-conflict chain (usually right: our proxy sits in front and theirs keeps \
handling auth), replace (theirs is recorded and uninstall puts it back), or abort"
    # Runnable, for the same reason as the plan path: a refusal that names a flag but not a command
    # invites the caller to assemble one.
    emit "consent_question_chain=$(R_ONCONFLICT=chain route_consent_question)"
    emit "consent_question_replace=$(R_ONCONFLICT=replace route_consent_question)"
    emit "confirm_command_chain=$(R_ONCONFLICT=chain route_confirm_command)"
    emit "confirm_command_replace=$(R_ONCONFLICT=replace route_confirm_command)"
    exit 2
  fi
  if [ -n "$R_EXISTING" ] && [ "$R_ONCONFLICT" = abort ]; then
    emit "result=aborted"; emit "existing_base_url=$R_EXISTING"
    emit "note=nothing was changed, at the caller's request"; exit 0
  fi
  if [ -n "$R_EXISTING" ] && [ "$R_ONCONFLICT" = chain ]; then
    R_CHAINED=true
    R_UPSTREAM=$(route_effective_upstream)   # the one encoding; see that function
  fi

  if [ "$R_PLAN" = 1 ]; then
    emit "result=planned"
    if [ "$R_MODE" = attach ]; then emit "binary=(not needed in attach mode)"
    else emit "binary=would_check"; fi
    route_report
    emit "note=nothing was written, nothing started. Re-run without --plan to perform this."
    exit 0
  fi

  # ---- CONSENT GATE ---------------------------------------------------------------------
  #
  # Refuses to act at all without an explicit token. Everything below this line either intercepts the
  # user's model traffic or points it somewhere new, and three measurements say the approval prompt
  # cannot be relied on to ask about that:
  #
  #   * the auto-mode classifier is itself a model, so it is probabilistic — the same command string
  #     was denied in one trial and allowed in two others;
  #   * a skill can declare the prompt away. With `allowed-tools: Bash(.../install.sh)` in its
  #     frontmatter, this exact command ran with NO prompt at all;
  #   * in a non-interactive session there is no prompt, because there is no human to ask — and the
  #     measured result was a complete, unattended install of a traffic interceptor.
  #
  # The third is what this gate is really for: an unattended run now FAILS CLOSED instead of quietly
  # succeeding. What it cannot do is prove a human said yes — a caller can pass the flag unprompted.
  # It converts "we hope a prompt fires" into "this refuses without a token that only exists because
  # someone asked", which is a deterministic precondition and an auditable one, not a guarantee.
  if [ "$R_CONSENT" != 1 ]; then
    emit "result=refused"
    emit "reason=consent_required"
    emit "consent_required=true"
    emit "base_url=$(route_url)"
    emit "existing_base_url=$R_EXISTING"
    # The gate guards the ACT; it has to name the TERMS too. What the flag is spelled to gate is
    # "traffic gets intercepted", while what the user is actually asked includes which scope and which
    # cache strategy — and one of those strategies spends their own quota while nobody is at the
    # keyboard. Ask a narrower question than the command performs and the consent artefact is
    # under-specified relative to the consent. consent_question= is generated from the same resolved
    # facts as confirm_command=, so the two cannot drift.
    emit "consent_question=$(route_consent_question)"
    emit "note=nothing was installed, started or written. Ask the user to agree to exactly what \
consent_question= says, as a choice they pick, and pass --i-consent-to-traffic-interception only if \
they say yes. A silent or absent answer is a NO. Never pass it on your own judgement."
    emit "confirm_command=$(route_confirm_command)"
    exit 2
  fi

  # ---- step 0: commit the port -----------------------------------------------------------
  # The plan's port was a preview. THIS is the write, and it is pinned to the port the plan showed
  # (`--promised-port`): if that port stopped being the one a real allocation would pick — another
  # project took it in the gap, or the configured value changed — the allocator refuses rather than
  # writing a URL nobody agreed to, and the caller has to re-plan. plugin.json says the port must be
  # fixed rather than negotiated; writing a different one here would negotiate it silently.
  #
  # Fail-open on anything else: an unrecorded port still works for this session (the URL, the proxy
  # and the settings write all use $R_PORT regardless), so a bookkeeping failure is reported as a
  # warning, not a refusal. The one exception is the promise mismatch above, which is not a
  # bookkeeping failure — it means the port is wrong.
  local paout pares
  paout=$("$(route_here)/settings.py" port alloc --file "$R_FILE" --promised-port "$R_PORT" 2>&1) || true
  pares=$(kv "$paout" result)
  if [ "$(kv "$paout" reason)" = port_changed_since_plan ]; then
    emit "result=refused"; emit "reason=port_changed_since_plan"
    emit "promised=$R_PORT"; emit "port=$(kv "$paout" port)"
    emit "note=nothing was installed, started or written. Re-run --route --plan and ask the user \
again: the port they agreed to is no longer free for this project."
    exit 2
  fi
  case "$pares" in
    ok) : ;;
    *) emit "port_warning=$(kv "$paout" reason)" ;;
  esac

  # ---- step 1: the binary (local only) --------------------------------------------------
  # Re-invokes THIS script with no arguments rather than refactoring its linear body: the installer
  # already speaks key=value and already exits early on `result=present`, and $0 is the same
  # self-location the rest of this function uses.
  if [ "$R_MODE" = local ]; then
    local iout ires
    iout=$("$0" 2>&1); ires=$(kv "$iout" result)
    case "$ires" in
      present|installed) : ;;
      *) emit "result=error"; emit "reason=binary_install_failed"
         emit "detail=$(kv "$iout" reason)"
         emit "note=nothing else was touched. A checksum failure must never be worked around."
         exit 3 ;;
    esac
    R_ONPATH=$(kv "$iout" on_path)
    [ "$R_ONPATH" = false ] && R_BIN=$(kv "$iout" path)
  fi

  # ---- step 4b: the cache strategy, BEFORE the proxy starts -----------------------------
  # start-proxy.sh reads that file only when it STARTS a proxy, so a strategy written afterwards
  # does nothing until something restarts it. Written here, the very first proxy has it.
  local sout
  # --preset is what gets RECORDED, but it is no longer what stays in force: start-proxy.sh runs
  # `strategy sync` before every launch, so the plugin option wins from the next session onwards. It
  # is still passed here so the very first proxy has the right one without waiting for a sync.
  sout=$("$(route_here)/settings.py" strategy set --name "$R_STRATEGY" \
           --port "$R_PORT" --preset "$R_PRESET" 2>&1) || true
  case "$(kv "$sout" result)" in
    set|cleared|unchanged) : ;;
    # `not_ours` is genuinely non-fatal: a config we did not write is not a reason to refuse an
    # install. An unknown NAME is different in kind — it means the caller asked for something that
    # does not exist — and it is refused in route_main before the binary is touched, so it cannot
    # reach here. Anything else keeps the fail-open behaviour, which is where it belongs.
    *) emit "strategy_warning=$(kv "$sout" reason)" ;;
  esac

  # ---- step 5: start the proxy (local only), BEFORE writing any settings ----------------
  # Claude Code picks a settings `env` change up while the session is RUNNING. Writing the key
  # first once killed the installing session: its next API call went to a dead port and died with
  # Connection refused, never reaching the step that starts the proxy.
  if [ "$R_MODE" = local ]; then
    local sp=("$(route_here)/start-proxy.sh" --emit-facts --unrouted --port "$R_PORT"
              --preset "$R_PRESET" --idle-exit "$R_IDLE")
    [ -n "$R_UPSTREAM" ] && sp+=(--upstream "$R_UPSTREAM")
    [ -n "$R_BIN" ] && sp+=(--bin "$R_BIN")
    # Captured so the pidfile path comes from the script that WROTE it (see --emit-facts there), then
    # re-printed so the user still sees its notes. The two fact lines are stripped from the re-print
    # so route_report_side_effects stays the single emitter of them.
    local spout
    spout=$("${sp[@]}" 2>&1) || true
    printf '%s\n' "$spout" | sed '/^pidfile=/d; /^log=/d'
    R_PIDFILE=$(kv "$spout" pidfile)
    R_PROXYLOG=$(kv "$spout" log)
  fi

  # ---- step 6: prove something answers, BEFORE routing to it ----------------------------
  if ! route_health_ok; then
    emit "result=error"; emit "reason=health_check_failed"
    emit "health_url=$(route_health_url)"
    emit "note=SETTINGS WERE NOT TOUCHED. An unrouted project with no proxy is a working project; \
a routed one with no proxy is a broken one."
    # ...but "settings were not touched" is not "nothing happened", and the caller will read it as
    # the latter. Step 5 may have started a proxy and left a pidfile and a strategy config behind.
    # That matters most when a health check fails TRANSIENTLY (a slow start, a busy laptop): the user
    # is told nothing happened, and their retry meets a stale pidfile on an occupied port.
    route_report_side_effects
    exit 3
  fi

  # ---- step 7: write the one key -------------------------------------------------------
  local aout acode=0
  local add=("$(route_here)/settings.py" add --file "$R_FILE" --url "$(route_url)")
  [ -n "$R_UPSTREAM" ] && add+=(--upstream "$R_UPSTREAM")
  [ -n "$R_BIN" ] && add+=(--bin "$R_BIN")
  [ "$R_SCOPE" = user ] && add+=(--user-scope)
  # BOTH decisions overwrite the routing key - that is what routing through a proxy means. `chain`
  # differs from `replace` only in ADDITIONALLY recording the old value as the upstream, which the
  # --upstream line above already does. With --force added for `replace` alone, the answer this design
  # calls usually right - and the plan's one question exists to ask - died at this step whenever the
  # existing endpoint was in a settings FILE: settings.py refused, correctly by its own rules, and the
  # user who answered the one question they were asked got `result=error` for it.
  #
  # Every chain test supplied the conflict through $ANTHROPIC_BASE_URL, where there is nothing in the
  # file to overwrite, which is why this was not caught. The file-sourced conflict is the shape a
  # PREVIOUS context-guru install or a checked-in team settings file leaves behind.
  #
  # --force is not a bypass here: `add` records `previous_base_url`, so /context-guru:uninstall puts
  # their gateway back. That record is what makes overwriting safe, and there is a test for it.
  case "$R_ONCONFLICT" in replace|chain) add+=(--force) ;; esac
  aout=$("${add[@]}" 2>&1) || acode=$?
  local ares; ares=$(kv "$aout" result)
  case "$ares" in
    added|completed|unchanged|repointed) : ;;
    *) emit "result=error"; emit "reason=settings_write_failed"
       emit "detail=$(kv "$aout" reason)"; emit "exit=$acode"
       emit "note=no routing was written."
       route_report_side_effects        # "may be running" is knowable; say which.
       exit 3 ;;
  esac

  # ---- step 8: re-check, and UNDO the routing if nothing answers ------------------------
  # The rollback is automatic on purpose. The skill used to ask the model to OFFER removing the key
  # here, and a routed project with no proxy is the one state strictly worse than not installing —
  # too important to depend on a model choosing to offer it.
  if ! route_health_ok; then
    "$(route_here)/settings.py" remove --file "$R_FILE" --url "$(route_url)" >/dev/null 2>&1 || true
    emit "result=error"; emit "reason=health_check_failed_after_write"
    emit "rolled_back=true"
    emit "note=the routing key was REMOVED again, so the project is unrouted rather than broken."
    # The rollback undoes the ROUTING KEY and nothing else. Say so, rather than letting
    # `rolled_back=true` be read as "everything was undone".
    route_report_side_effects
    exit 3
  fi

  # ---- step 9: adopt the projects that route themselves, if that is what was asked -------
  # LAST, and deliberately so. Adopting unroutes OTHER projects, and it is only safe to take their
  # own routing away once the machine-wide route they are about to fall back on is written AND
  # health-checked — step 8 above is what proves that. Unrouting them first and then failing here
  # would leave every one of them with no route at all.
  if [ "$R_ONEXISTING" = adopt ] && [ -n "$R_EXISTINGPROJECTS" ]; then
    local ap af aout2
    printf '%s\n' "$R_EXISTINGPROJECTS" | while IFS= read -r l; do
      ap=$(printf '%s\n' "$l" | sed -n 's/^existing_project=\([^ ]*\) .*/\1/p')
      af=$(printf '%s\n' "$l" | sed -n 's/.* file=\(.*\)$/\1/p')
      [ -n "$ap" ] || continue
      # `remove` is the same surface uninstall uses, and it is the safe one: it only removes routing
      # it can prove is ours (is_ours), it takes a backup, and it puts back whatever base URL was
      # there before us. No --url, so that check decides alone.
      if [ -n "$af" ] && [ "$af" != "(none)" ]; then
        aout2=$("$(route_here)/settings.py" remove --file "$af" 2>&1) || true
        # `remove`'s own backup= is NOT relayed. It stopped being a path: a clean removal deletes
        # every rolling backup it just took (forget_backups), so the field now carries a fixed
        # sentence saying so — the same sentence for every project, containing spaces and an em
        # dash, inside a line callers parse on spaces. The recoverable artifact is the recovery
        # FOLDER beside each file, which the note on the gate above already points at.
        emit "adopted_project=$ap unrouted=$(kv "$aout2" result) recovery_dir=$(dirname "$af")/context-guru-settings-json"
        # The per-project port option goes too: with no route of its own, a leftover port would aim
        # that project's hooks at a port nothing serves — configured-looking and broken, which is
        # worse than the state before.
        "$(route_here)/settings.py" port unset --file "$af" >/dev/null 2>&1 || true
      fi
      # And the record, so nothing reports the project as still having its own routing. Run FROM
      # that project's directory, because project_key() is what files the record and it answers for
      # the cwd — there is no second way to name a project, on purpose.
      ( CDPATH= cd -- "$ap" 2>/dev/null && "$(route_here)/settings.py" port release >/dev/null 2>&1 ) || true
    done
  fi

  emit "result=routed"
  emit "settings_result=$ares"
  emit "backup=$(kv "$aout" backup)"
  emit "reset_hatch=$(kv "$aout" reset_hatch)"
  emit "replaced=$(kv "$aout" replaced)"
  route_install_statusline
  route_ensure_gitignore
  route_report
}

# ---- statusline, on by default -------------------------------------------------------------
# A savings-focused install with nothing showing the savings is a worse first impression than a
# status line that turns out to be unwanted, and unwanted is one command away (--no-statusline
# here, or `settings.py off` any time after).
#
# SAME FILE ROUTING JUST USED — not hardcoded to user scope. This used to write unconditionally
# to $HOME/.claude/settings.json on the theory that it "renders nothing in a project that is not
# routed, so it's safe regardless of scope" (skills/statusline/SKILL.md). That argument covers
# whether the RENDER is safe; it says nothing about whether the WRITE is wanted, and the answer
# was no: a project-scope install (the default) silently wrote and backed up the user's
# machine-wide settings file every time. $R_FILE is already the file THIS run just wrote routing
# to, decided by --scope exactly like the routing write two lines above route_main's call to this
# function — reuse it rather than deciding scope a second time. `--user-scope` is passed through
# for the same reason `add`'s own routing call gets it at line ~650: `--scope user` for this
# install already went through --i-understand-machine-wide, so this is not a second confirmation,
# it is the same one applying to a second key in the same file.
#
# Never fatal — a statusline write failing is not a routing failure, it is reported as its own
# fact and nothing about `result=routed` above changes.
#
# REAL absolute path, not the ${CLAUDE_PLUGIN_ROOT} placeholder. That placeholder only expands
# when CLAUDE CODE ITSELF substitutes it — documented for hook commands, MCP/LSP server config,
# and skill/agent content, never for `statusLine`. A skill's own markdown (see
# skills/statusline/SKILL.md) gets it substituted before the model ever runs the command, because
# that substitution happens on the skill's rendered TEXT — but install.sh is a plain script file,
# not skill text, so writing the literal placeholder here just puts an unexpandable string into
# settings.json forever: the status line reports `statusline=on` and then never renders anything,
# because the shell later runs `python3 "/scripts/statusline.py"` (empty expansion) and fails
# silently. `route_here()` already resolves this script's own directory from `$0` for every other
# call in this file — reuse it instead of a placeholder nothing will ever expand.
route_install_statusline() {
  [ "$R_NOSTATUSLINE" = 1 ] && { emit "statusline=skipped"; return 0; }
  local sout scode=0
  # --no-backup: the routing `add` a few lines up already backed up this file's pre-install
  # state. Without this, this second call took its own backup a moment later — of "routed, no
  # statusline yet", a state nobody would ever restore to — so one install left two backup files
  # on disk. See settings.py's --no-backup for why this is the only caller that passes it.
  local sl=("$(route_here)/settings.py" add --file "$R_FILE" \
    --statusline "python3 \"$(route_here)/statusline.py\"" --no-backup)
  [ "$R_SCOPE" = user ] && sl+=(--user-scope)
  sout=$("${sl[@]}" 2>&1) || scode=$?
  local sres; sres=$(kv "$sout" result)
  case "$sres" in
    added|unchanged) emit "statusline=on" ;;
    conflict) emit "statusline=skipped"; emit "statusline_reason=$(kv "$sout" reason)" ;;
    *) emit "statusline=skipped"; emit "statusline_reason=write_failed"; emit "statusline_exit=$scode" ;;
  esac
}

# ---- keeping the recovery folder out of git — deterministic, on by default, no question --------
#
# A recovery folder beside the settings file (settings.py's `context-guru-settings-json/`, holding
# a copy of the file taken before this install) can carry a credential, same as the settings file
# itself already can — nothing new is exposed by putting it in the project directory that isn't
# already exposed by the file it copies. What IS worth closing is the same gap `.context-guru-
# backup-*` already has and nobody has fixed yet: a name no existing `.gitignore` anticipates,
# sitting in a working tree, one `git add -A` away from a commit.
#
# This is deliberately NOT a consent-gated decision like `--scope user` or the traffic-interception
# flag below: it changes nothing about what the user is exposed to, it is fully reversible (delete
# the line), and the check for whether it is even needed is entirely mechanical — see
# settings.py's `gitignore-ensure`. Modelled on route_install_statusline immediately above: on by
# default, best-effort, never fatal, one opt-out flag for symmetry.
route_ensure_gitignore() {
  [ "$R_NOGITIGNORE" = 1 ] && { emit "gitignore=skipped"; return 0; }
  local gout gres
  gout=$("$(route_here)/settings.py" gitignore-ensure --file "$R_FILE" 2>&1) || true
  gres=$(kv "$gout" result)
  case "$gres" in
    added|unchanged) emit "gitignore=$gres" ;;
    *) emit "gitignore=skipped"; emit "gitignore_reason=$(kv "$gout" reason)" ;;
  esac
}

# --- argument parsing. Unknown flags are refused rather than ignored: a silently dropped --scope
# --- would write the wrong file and report success.
if [ "${1:-}" = --route ]; then
  shift
  while [ $# -gt 0 ]; do
    case "$1" in
      --plan)     R_PLAN=1 ;;
      --confirm)  R_CONFIRM=1 ;;
      --mode)     route_need_value --mode "${2:-}";     R_MODE="$2"; shift ;;
      --scope)    route_need_value --scope "${2:-}";    R_SCOPE="$2"; shift ;;
      --on-conflict) route_need_value --on-conflict "${2:-}"; R_ONCONFLICT="$2"; shift ;;
      --on-existing-projects) route_need_value --on-existing-projects "${2:-}"
        case "$2" in
          leave|adopt) R_ONEXISTING="$2" ;;
          # Validated HERE rather than where it is used, for the same reason --scope is: an
          # unrecognised value that reaches the gate would read as "no answer given" and re-ask a
          # question the caller already answered, with a typo as the invisible cause.
          *) emit "result=error"; emit "reason=unknown_on_existing_projects"
             emit "value=$2"
             emit "note=--on-existing-projects must be leave or adopt. Nothing was done."
             exit 3 ;;
        esac
        shift ;;
      --cache-strategy) route_need_value --cache-strategy "${2:-}"; R_STRATEGY="$2"; shift ;;
      --base-url) route_need_value --base-url "${2:-}"; R_BASEURL="$2"; shift ;;
      --health-url) route_need_value --health-url "${2:-}"; R_HEALTHURL="$2"; shift ;;
      --upstream) route_need_value --upstream "${2:-}"; R_UPSTREAM="$2"; shift ;;
      --no-health-check) R_NOHEALTH=1 ;;
      --no-statusline) R_NOSTATUSLINE=1 ;;
      --no-gitignore-check) R_NOGITIGNORE=1 ;;
      --i-understand-machine-wide) R_USERSCOPE=1 ;;
      --i-consent-to-traffic-interception) R_CONSENT=1 ;;
      *) emit "result=error"; emit "reason=unknown_flag"; emit "flag=$1"
         emit "note=refused rather than ignored: a dropped flag writes the wrong file and reports success"
         exit 2 ;;
    esac
    shift
  done
  case "$R_MODE" in local|attach) : ;; *)
    emit "result=error"; emit "reason=unknown_mode"; emit "mode=$R_MODE"; exit 2 ;;
  esac
  case "$R_ONCONFLICT" in ""|chain|replace|abort) : ;; *)
    emit "result=error"; emit "reason=unknown_on_conflict"; emit "value=$R_ONCONFLICT"; exit 2 ;;
  esac
  # --scope belongs here with the other two. Without it, a bad value fell through to
  # route_scope_file, whose refusal was swallowed by a command substitution - and `--scope global`
  # is a plausible mistake, because the user-facing flag this skill documents is `--global` while the
  # value this script accepts is `user`. Validated at parse time, so it fails before anything runs and
  # says which values exist.
  case "$R_SCOPE" in project|team|user) : ;; *)
    emit "result=error"; emit "reason=unknown_scope"; emit "value=$R_SCOPE"
    emit "note=--scope takes project, team or user. The user-facing --global maps to --scope user."
    exit 2 ;;
  esac
  route_main
  exit 0
fi

# --- 1. already there, and is it the version we want? -------------------------------------
#
# This used to return `result=present` for ANY binary on PATH regardless of version, and read
# CONTEXT_GURU_VERSION only afterwards — so on the very change that creates a release channel
# there was no way to upgrade. It also reported the version by taking `head -1` of `--help`,
# which recorded "Usage of context-guru-proxy:" as the installed version.
installed_version() { # prints e.g. v0.1.2, or "" if the binary cannot say
  "$1" --version 2>/dev/null | awk '{print $2; exit}'
}

# A plain text file, not a binary invocation, so start-proxy.sh can learn the installed version
# WITHOUT ever executing $BIN on every session start. That distinction is load-bearing: a hook
# that ran an arbitrary configured binary just to read its version would run it on a machine
# where that binary is actually something else entirely, or (as a test double proved) something
# that does not distinguish --version from "start serving" at all. Best effort: a state directory
# this cannot write to must never fail an install that would otherwise succeed.
record_installed_version() { # $1 = the version now confirmed on disk
  st=$(route_state_dir) || return 0
  mkdir -p "$st" 2>/dev/null || return 0
  printf '%s\n' "$1" >"${st}/proxy-version.tmp-$$" 2>/dev/null || return 0
  mv -f "${st}/proxy-version.tmp-$$" "${st}/proxy-version" 2>/dev/null || true
}

# Extracted out of the ordinary install flow so `--check-latest` (below) can ask the identical
# question — web redirect first, API only as a distinguishing fallback — without a download ever
# being on the table. Prints exactly one line: `tag=<TAG>` on success, or
# `error=github_rate_limited` / `error=no_release_found:<http code>` on failure — encoded in
# stdout rather than a variable the function sets, because every caller invokes this through
# `$(...)`, and a variable assigned inside a command-substitution subshell never reaches the
# parent shell. (A real bug here, caught by testing rather than by inspection: the first version
# set a global `RESOLVE_REASON` and every caller read it back as permanently empty.)
resolve_latest_tag() {
  local v
  v=$(curl -fsSLI --max-time "${CONTEXT_GURU_CHECK_TIMEOUT:-10}" -o /dev/null -w '%{url_effective}' \
        "https://github.com/${REPO}/releases/latest" 2>/dev/null)
  v="${v##*/}"
  case "$v" in ''|releases|latest) v="" ;; esac
  if [ -z "$v" ]; then
    local api code
    api=$(curl -sSL --max-time "${CONTEXT_GURU_CHECK_TIMEOUT:-10}" -w '\n%{http_code}' \
            "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null)
    code=$(printf '%s' "$api" | tail -1)
    v=$(printf '%s' "$api" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
    if [ -z "$v" ]; then
      case "$code" in
        403|429) printf 'error=github_rate_limited\n' ;;
        *)       printf 'error=no_release_found:%s\n' "${code:-unreachable}" ;;
      esac
      return 1
    fi
  fi
  printf 'tag=%s\n' "$v"
}

# --check-latest: resolve installed vs. latest and REPORT, never download, never install, never
# write the proxy binary. Exists because the ordinary flow above can only say "a newer release
# exists" as a side effect of actually fetching one (CONTEXT_GURU_UPGRADE=1) — which is the wrong
# shape for "I asked to check, not to install" (see skills/update/SKILL.md). `installed_version`
# still runs $BIN here: unlike start-proxy.sh's SessionStart hook, this is a deliberate,
# user-initiated invocation, not a blind call on every session start.
if [ "${1:-}" = --check-latest ]; then
  have=""
  if command -v "$BIN" >/dev/null 2>&1; then
    have=$(installed_version "$(command -v "$BIN")")
    [ -n "$have" ] && record_installed_version "$have"
  fi
  emit "installed_version=${have:-unknown}"
  command -v curl >/dev/null 2>&1 || { emit "result=checked"; emit "update_available=unknown"; emit "reason=no_curl"; exit 0; }
  resolved=$(resolve_latest_tag) || true
  case "$resolved" in
    tag=*) latest="${resolved#tag=}" ;;
    *)     emit "result=checked"; emit "update_available=unknown"
           emit "reason=${resolved#error=}"
           exit 0 ;;
  esac
  emit "latest_version=${latest}"
  if [ -n "$have" ] && { [ "$have" = "${latest#v}" ] || [ "$have" = "$latest" ]; }; then
    emit "result=checked"; emit "update_available=false"
  else
    emit "result=checked"; emit "update_available=true"
  fi
  exit 0
fi

if command -v "$BIN" >/dev/null 2>&1; then
  have_path=$(command -v "$BIN")
  have=$(installed_version "$have_path")
  emit "path=${have_path}"
  emit "version=${have:-unknown}"
  # An explicit CONTEXT_GURU_VERSION means "I want that one" — honour it even when something is
  # already installed. `latest` resolves below and is compared there.
  if [ "$VERSION" != latest ] && [ "$VERSION" = "$have" ]; then
    emit "result=present"
    [ -n "$have" ] && record_installed_version "$have"
    exit 0
  fi
  if [ "$VERSION" = latest ] && [ -n "$have" ] && [ "${CONTEXT_GURU_UPGRADE:-}" != 1 ]; then
    # Do not silently re-download on every install run; say what is there and how to move.
    emit "result=present"
    emit "note=set CONTEXT_GURU_UPGRADE=1 to check for and install a newer release"
    record_installed_version "$have"
    exit 0
  fi
  if [ -z "$have" ]; then
    emit "note=the installed binary does not support --version; it predates the release channel"
  fi
  emit "note=upgrading from ${have:-unknown}"
fi

case "$(uname -s)" in
  Darwin) OS=darwin ;;
  Linux)  OS=linux ;;
  *)      die "unsupported_os_$(uname -s): build from source, see docs/get-started/quickstart-proxy.md" ;;
esac
case "$(uname -m)" in
  x86_64|amd64)  ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *)             die "unsupported_arch_$(uname -m)" ;;
esac
emit "platform=${OS}/${ARCH}"

command -v curl >/dev/null 2>&1 || die "no_curl"

# --- 2. release tarball -------------------------------------------------------------------
if [ "$VERSION" = latest ]; then
  # Resolve to a CONCRETE tag once, then use it for both the tarball and checksums.txt — the two
  # must come from the same release, and two independent /latest/download follows could straddle a
  # release published between them. `resolve_latest_tag` (above) is the same web-redirect-first,
  # API-only-as-fallback resolution `--check-latest` uses, so a download and a mere check can never
  # disagree about what "latest" means.
  resolved=$(resolve_latest_tag) || true
  case "$resolved" in
    tag=*) VERSION="${resolved#tag=}" ;;
    error=github_rate_limited)
      die "github_rate_limited: GitHub's API is rate limited for this IP (60/hour, shared with everything behind the same address), so the latest version could not be resolved. This says NOTHING about whether a release exists. Wait for the window to reset, or set CONTEXT_GURU_VERSION=vX.Y.Z to skip resolution entirely." ;;
    *)
      die "no_release_found: no published release for ${REPO} (HTTP ${resolved#error=no_release_found:}); build from source or set CONTEXT_GURU_VERSION" ;;
  esac
fi
NUM="${VERSION#v}"
TARBALL="context-guru_${NUM}_${OS}_${ARCH}.tar.gz"
BASE="https://github.com/${REPO}/releases/download/${VERSION}"
emit "version=${VERSION}"

# try_source_build is distribution option 3 from the header comment, which the first version of
# this script documented and never implemented — on a machine that had Go 1.26.4 on PATH.
#
# It is a FALLBACK, not a path anyone is steered to: it needs a toolchain, which is the gate this
# whole change exists to remove. But when there is no downloadable asset and a toolchain is right
# there, refusing to use it is worse than using it.
# report_path emits the two facts every successful install owes the caller, from BOTH install
# paths. The `go install` fallback used to return straight out of the script, so a user who landed
# on it got no `on_path` line at all — and ~/.local/bin frequently is not on PATH. The install
# looked clean, then a LATER session's hook said "the proxy binary is not on PATH", with nothing
# connecting the two. install/SKILL.md reads on_path to warn them, so the skill was silent too.
report_path() {
  emit "result=installed"
  emit "path=${DEST}/${BIN}"
  record_installed_version "$VERSION"
  # Report — do not fix — a PATH that will not find it. Editing the user's shell rc is a bigger
  # intrusion than this script is entitled to, and the skill can tell them in context.
  case ":${PATH}:" in
    *":${DEST}:"*) emit "on_path=true" ;;
    *)             emit "on_path=false"
                   emit "note=add ${DEST} to your PATH, or the session hook will not find the proxy" ;;
  esac
}

try_source_build() {
  command -v go >/dev/null 2>&1 || return 1
  # "attempted", because this line is printed BEFORE the build runs: it appears even when the
  # build then fails, and `result=` is what says whether anything was installed.
  emit "fallback=go_install_attempted"
  # CGO off: the binary is pure Go, and requiring a C toolchain here would reintroduce the gate.
  # GOBIN does not need creating first: checked on Linux with Go 1.26.4 — `go install` creates a
  # missing GOBIN directory itself, so the tarball path's `mkdir -p` is not needed here.
  if CGO_ENABLED=0 GOBIN="$DEST" go install "github.com/${REPO}/cmd/context-guru-proxy@${VERSION}" 2>"$TMP/go.err"; then
    emit "built_from=source"
    report_path
    return 0
  fi
  emit "go_install_failed=$(tail -1 "$TMP/go.err" 2>/dev/null | tr -d '\n')"
  return 1
}

TMP=$(mktemp -d) || die "no_tmpdir"
trap 'rm -rf "$TMP"' EXIT

# The raw curl error used to reach stdout and break this script's "every fact is a key=value
# line" contract, which the calling skill parses. Keep curl quiet and report the failure as data.
if ! curl -fsSL -o "$TMP/$TARBALL" "$BASE/$TARBALL" 2>"$TMP/curl.err"; then
  emit "download_url=$BASE/$TARBALL"
  # A published tag with no assets reaches exactly here — the release exists, the artifact does
  # not — which is what a pre-release repository looks like before the first build is attached.
  if try_source_build; then
    exit 0
  fi
  die "download_failed: $BASE/$TARBALL (no asset for this platform, and no Go toolchain to build from source)"
fi

# Checksum. The download is unsigned, there is no signature anywhere yet, and this script strips
# macOS quarantine from the file below — so this is the ONLY integrity check in the path.
#
# It is therefore fail-CLOSED, in every branch. The first version of this was fail-open: a missing
# or unfetchable checksums.txt printed one advisory line and installed anyway, which meant an
# unverified binary landed on a PATH directory and ran — a binary that then handles all of the
# user's LLM traffic and holds their API key. The comment above it said "a failure here is fatal,
# never a warning" while the code did the opposite.
#
# CONTEXT_GURU_INSECURE=1 exists for the one legitimate case (a local build served from a file
# path with no checksums file) and says what it is in its name.
verify_checksum() {
  curl -fsSL -o "$TMP/checksums.txt" "$BASE/checksums.txt" 2>/dev/null ||
    die "checksum_unavailable: could not fetch $BASE/checksums.txt, so the download cannot be verified. Set CONTEXT_GURU_INSECURE=1 to install anyway (not recommended)."
  want=$(awk -v f="$TARBALL" '$2 == f || $2 == "*"f {print $1}' "$TMP/checksums.txt" | head -1)
  [ -n "$want" ] ||
    die "checksum_absent: $TARBALL is not listed in checksums.txt, so the download cannot be verified. Set CONTEXT_GURU_INSECURE=1 to install anyway (not recommended)."
  if command -v sha256sum >/dev/null 2>&1; then
    got=$(sha256sum "$TMP/$TARBALL" | awk '{print $1}')
  else
    got=$(shasum -a 256 "$TMP/$TARBALL" | awk '{print $1}')
  fi
  [ "$want" = "$got" ] || die "checksum_mismatch: expected $want got $got"
  emit "checksum=verified"
}

if [ "${CONTEXT_GURU_INSECURE:-}" = 1 ]; then
  emit "checksum=SKIPPED_BY_CONTEXT_GURU_INSECURE"
else
  verify_checksum
fi

tar xzf "$TMP/$TARBALL" -C "$TMP" || die "untar_failed"

# FIND the binary rather than assuming where it sits.
#
# The release archive wraps its contents in a directory (goreleaser `wrap_in_directory: true`), so
# the binary is at `<archive-name>/context-guru-proxy` and not at the root. It is wrapped for a
# reason worth not undoing: a flat archive plus the documented `tar xzf` with no `-C` overwrites the
# README.md and LICENSE of whatever directory the user is standing in.
#
# Searching handles both layouts, so this script does not break the next time the packaging changes
# — and the failure it avoids is the worst-placed one there is: a stranger's first install, reporting
# `binary_not_in_tarball`, which reads as a broken release rather than a moved file.
found=$(find "$TMP" -type f -name "$BIN" 2>/dev/null | head -1)
[ -n "$found" ] || die "binary_not_in_tarball: no $BIN anywhere in $TARBALL"

mkdir -p "$DEST" || die "cannot_create_$DEST"
# Install to a temp name and RENAME into place, so the destination is never absent or partial.
#
# Not for the reason it was suggested: the review's premise was ETXTBSY on Linux when writing over
# a running binary, and that was tested on Linux and does NOT happen — coreutils `install` unlinks
# the destination first, so the upgrade succeeds. But that unlink is itself the window worth
# closing: between it and the new file appearing, a SessionStart hook firing in another project
# finds no binary and reports "not on PATH". rename(2) swaps the directory entry in one step, so
# there is no instant at which $DEST/$BIN does not exist.
install -m 755 "$found" "$DEST/$BIN.new" || die "install_failed_to_$DEST"
mv -f "$DEST/$BIN.new" "$DEST/$BIN" || die "install_failed_to_$DEST"

# macOS: without this, the first run dies with "cannot be verified" and the evaluator concludes
# the project is broken. Notarization would remove the need and requires a paid Apple account.
if [ "$OS" = darwin ] && command -v xattr >/dev/null 2>&1; then
  xattr -d com.apple.quarantine "$DEST/$BIN" 2>/dev/null || true
  emit "quarantine=cleared"
fi

report_path
