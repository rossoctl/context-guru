#!/bin/sh
# context-guru-reset — undo context-guru's routing when Claude Code cannot do it for you.
#
# WHY THIS EXISTS, AND WHY IT IS NOT A SKILL
#
# The plugin's undo path is `/context-guru:uninstall`, which is a SKILL: it needs a working
# Claude Code session to run. The failure this script exists for takes that away. Routing every
# request through a local proxy means a bad routing key, a dead port or a rejected credential
# makes every API call fail — and then the agent that would have fixed it cannot talk to a model
# to be asked. A colleague hit exactly that: 401 on every request, and `/context-guru:uninstall`
# unavailable for the same reason he needed it.
#
# So this is deliberately the opposite of the rest of the plugin:
#
#   * plain POSIX sh, no python, no Go binary, no network, no plugin code;
#   * it is INSTALLED OUTSIDE THE PLUGIN, into `~/.local/state/context-guru` (and, when that
#     directory exists, a second copy on PATH at `~/.local/bin`). A hatch that lives inside the
#     thing that broke is unavailable exactly when it is needed — `/plugin uninstall`, a
#     marketplace refresh or a wiped plugin cache all take it with them;
#   * the primary path is `cp`, restoring a copy of each settings file taken BEFORE the first
#     edit, from a `context-guru-settings-json/` folder beside that settings file — not from a
#     shared, hashed, append-only ledger under the state directory. That used to be the design, and
#     it had a real defect: nothing ever removed an entry, so a scope touched once and cleanly
#     uninstalled since stayed on the list forever, and running this script for one broken project
#     would revert a completely different, unrelated scope right along with it. Checking exactly the
#     three real candidate files, each carrying its own folder, makes that leak impossible rather
#     than merely guarded against — see the checks below for the guard that still exists on TOP of
#     that, for a scope's own folder going stale in the same way;
#   * it takes its own backup before it writes, so running it is itself reversible. That copy also
#     goes into the SAME per-file recovery folder, for the same reason the pre-install copy does:
#     `/context-guru:install` closes the one real risk this reopens (a settings file can carry a
#     credential, and so can a copy of it) with a `.gitignore` entry added deterministically, rather
#     than by putting the copy somewhere a user would never think to look for it;
#   * it prints what it will do and asks, unless told `--yes`.
#
# It restores ROUTING. It does not fix a credential — see the report it prints at the end, which
# names the file and line an ANTHROPIC_* variable is exported from without ever printing a value.

set -eu

PROG="context-guru-reset"
DRY=0
YES=0
STAMP="$(date +%Y%m%d-%H%M%S)"
# Set when any step could not finish, so the exit code tells a script what a human would see.
INCOMPLETE=0
# Set ONLY by a problem with a FILE, and kept separate from INCOMPLETE on purpose.
#
# INCOMPLETE grew to include an environment condition (an ANTHROPIC_BASE_URL exported in the user's
# shell, which no settings change can override) and that immediately made the empty-plan branch lie
# in the other direction: with clean files and a routed shell it printed "your settings files are NOT
# back to their pre-install state" plus instructions to repair a file, two lines under a line saying
# that same file already matched its pre-install copy. Which sentence to print is a question about
# FILES; the exit code is a question about whether anything is left for a human. Two questions, two
# flags.
FILES_UNFIXED=0
RESTORED=0

usage() {
  cat <<USAGE
$PROG — put your Claude Code settings back the way they were before context-guru.

usage: $PROG [--dry-run] [--yes]

  --dry-run   show what would change and exit without writing anything
  --yes       do not ask for confirmation (for non-interactive use)

What it does, in order:
  1. checks each settings file context-guru can write to (project-local, project, user scope)
     for a recovery folder — \`<dir>/${RECOVERY_DIR_NAME}/\` — beside it;
  2. for one that shows context-guru's own keys RIGHT NOW, copies the current file into that same
     folder first, so this is reversible too;
  3. restores it from the copy taken before context-guru's first edit — or deletes it, if
     context-guru is the reason it exists;
  4. verifies no routing key is left, and reports anything it could not fix.

It never stops processes, never touches the network, and never edits a file with no context-guru
fingerprint in it right now, or with no recovery folder beside it.
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY=1 ;;
    --yes|-y) YES=1 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "$PROG: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

# The (at most three) settings files context-guru ever writes to, same set and same order
# `_option_file_candidates()` in settings.py enumerates for reading plugin options — project-local,
# project, user — so this script and that one can never disagree about where "the user-scope file"
# is. Respecting CLAUDE_CONFIG_DIR here (settings.py's `user_scope_files()` already does) matters:
# without it, a machine with that variable set would have this script check the wrong path and
# report every real entry there as "not present".
CLAUDE_DIR_USER="${CLAUDE_CONFIG_DIR:-$HOME/.claude}"
RECOVERY_DIR_NAME="context-guru-settings-json"

say()  { printf '%s\n' "$*"; }
warn() { printf '%s\n' "$*" >&2; }

# How many pre-reset snapshots (see the "saved current state:" copy below) to keep PER settings
# file. Mirrors settings.py's KEEP_BACKUPS: this hatch takes a fresh copy of the file it is about
# to overwrite on every run, and that was never bounded — a machine reset a few dozen times over
# its life accumulated that many complete copies of a settings file, credentials included,
# forever, in a directory nothing else here ever reads by hand.
KEEP_PRERESET=10

prune_prereset() {
  # $1: an UNQUOTED glob pattern covering every pre-reset snapshot for ONE settings file, passed
  # as a variable so word-splitting and globbing happen here rather than at the call site.
  ls -1t $1 2>/dev/null | tail -n "+$((KEEP_PRERESET + 1))" | while IFS= read -r _f; do
    rm -f "$_f"
  done
}

# ---------------------------------------------------------------------------
# redact: a filter for anything that prints FILE CONTENT or an environment value.
#
# Applied at every site that echoes something read off disk or out of the environment — the plan
# diff, the verify pass, the no-record grep, and the exported-base-URL lines in report_environment.
# "Remember to filter this one too" is how the first three leaks happened.
#
# WHY THIS IS AN ALLOWLIST FOR `"key": value` LINES.
#
# It began as a denylist of credential-ish key names and value shapes, and it leaked three times in
# three different shapes — the last round found `sk_live_` (the rule was literally `sk-`), secrets in
# a URL PATH, `?auth=` (the query rule carried its own narrower name list instead of reusing the one
# above it), and `"apiKeyHelper": {"cmd": …}` — a real Claude Code settings key whose entire purpose
# is producing a credential, which escaped because the rule required a quote straight after the colon.
# A denylist in a tool whose output the docs invite the user to paste into a bug report will leak every
# shape nobody has thought of yet.
#
# The objection to inverting it was correct too: redact every value and the diff no longer shows the
# permission grants it exists to show, which defeats the purpose of printing a diff at all. Both
# constraints are satisfiable, because the two cases have different shapes:
#
#   * `"key": value`  -> print the value only for keys known to be safe (settings that are structural,
#     enumerated, or the routing itself). Anything unrecognised is redacted, so a NEW credential key
#     is covered the day it is invented rather than the day somebody adds a rule.
#   * a bare array element -> print it only if it looks like a permission grant, `Tool(...)`. That is
#     the shape the diff exists to show, and it is narrow enough to be an allowlist rather than a
#     hope.
#
# The old denylist rules are kept as a second layer underneath, for content that is neither of those
# shapes (a bare URL from the environment, a value inside a safe key).
#
# ROUTING KEYS STAY VISIBLE. ANTHROPIC_BASE_URL / ANTHROPIC_UPSTREAM / CONTEXT_GURU_BIN are the
# diagnostic — a locked-out user has to see which port they are pointed at — so those keep scheme,
# host and first path segment and lose only userinfo, query and any deeper path.
#
# awk, not sed, because "print this value unless its key is in a set" needs a negative test that
# POSIX EREs cannot express; the sed version needed a lookahead. awk is as portable as sed (busybox
# and dash-only systems both have one) and the logic is legible, which matters more here than brevity.
# ---------------------------------------------------------------------------
REDACT_MARK='<value not shown>'
redact() {
  awk -v mark="$REDACT_MARK" '
  BEGIN {
    # Values printed in full. Structural, enumerated, or the routing itself.
    n = split("permissions allow deny ask additionaldirectories model theme statusline type " \
              "includecoauthoredby cleanupperioddays verbose autoupdates preferredNotifChannel " \
              "env hooks matcher outputstyle alwaysthinkingenabled", a, " ")
    for (i = 1; i <= n; i++) SAFE[tolower(a[i])] = 1
    # Routing: shown, but stripped back to what identifies the endpoint.
    n = split("anthropic_base_url anthropic_upstream context_guru_bin", a, " ")
    for (i = 1; i <= n; i++) ROUTE[tolower(a[i])] = 1
  }
  function shapes(t) {
    # The second layer. sk- AND sk_ (sk_live_/sk_test_ escaped a `sk-`-only rule).
    gsub(/sk[-_][A-Za-z0-9_-]{6,}/, "sk_" mark, t)
    gsub(/ghp_[A-Za-z0-9_]{6,}/, "ghp_" mark, t)
    gsub(/github_pat_[A-Za-z0-9_]{6,}/, "github_pat_" mark, t)
    gsub(/xox[baprs]-[A-Za-z0-9-]{6,}/, "xoxb-" mark, t)
    gsub(/AKIA[A-Z0-9]{6,}/, "AKIA" mark, t)
    gsub(/eyJ[A-Za-z0-9_.\/+-]{6,}/, "eyJ" mark, t)
    gsub(/[Bb]earer[ \t]+[A-Za-z0-9_.~+\/=-]{8,}/, "Bearer " mark, t)
    t = url_creds(t)
    return t
  }
  function url_creds(t,   q, head, qs, n, parts, i, kv, eq, name, rest, tail, out) {
    # user:pass@host, and credential-named query parameters.
    #
    # The query half is rebuilt by splitting on & rather than by a substitution loop. The loop version
    # did not terminate: its replacement text contains a space, so the [^&" \t]+ value pattern matched
    # the "<value" prefix of what had just been written and it replaced forever. A split has no such
    # failure mode, and this is a recovery tool — it must not be able to hang.
    #
    # The name test is `key|token|secret|auth|pass|sig|cred` applied to a lowercased name, which is the
    # same vocabulary the rest of the filter uses. The previous rule carried its own narrower copy
    # (key|token|secret only), which is why ?auth= printed in the clear — an internal inconsistency
    # rather than a missing idea.
    gsub(/:\/\/[^\/@ \t"]*:[^\/@ \t"]*@/, "://<credentials not shown>@", t)
    q = index(t, "?")
    if (q == 0) return t
    head = substr(t, 1, q)
    qs = substr(t, q + 1)
    n = split(qs, parts, "&")
    out = ""
    for (i = 1; i <= n; i++) {
      kv = parts[i]
      eq = index(kv, "=")
      name = (eq > 0 ? substr(kv, 1, eq - 1) : kv)
      if (eq > 0 && tolower(name) ~ /key|token|secret|auth|pass|sig|cred/) {
        rest = substr(kv, eq + 1)
        tail = ""
        if (match(rest, /["},]+$/)) tail = substr(rest, RSTART)
        kv = name "=" mark tail
      }
      out = out (i > 1 ? "&" : "") kv
    }
    return head out
  }
  function route_value(v,   lead, trail, i, c, cnt, head, rest, q, out) {
    # Keep scheme://host and one path segment; drop the query and anything deeper. A secret can sit in
    # a deep path (a Slack webhook is nothing but path), and the diagnostic only needs the endpoint.
    #
    # The surrounding quotes are lifted off first and put back at the end. Without that, the segment
    # walk below consumed the closing quote as if it were part of the URL and re-processed a query
    # url_creds had already redacted, emitting `…/anthropic?<value not shown>,` with the parameter
    # name and the closing quote both gone — valid redaction, unreadable JSON.
    if (substr(v, 1, 1) == "\"") { lead = "\""; v = substr(v, 2) }
    if (substr(v, length(v), 1) == "\"") { trail = "\""; v = substr(v, 1, length(v) - 1) }
    v = url_creds(v)
    if (match(v, /:\/\//)) {
      head = substr(v, 1, RSTART + RLENGTH - 1)
      rest = substr(v, RSTART + RLENGTH)
      cnt = 0
      out = ""
      for (i = 1; i <= length(rest); i++) {
        c = substr(rest, i, 1)
        if (c == "/") { cnt++; if (cnt > 1) { out = out "/" mark; break } }
        out = out c
      }
      return lead head out trail
    }
    return lead v trail
  }
  # Walks the line and judges EVERY `"key": value` on it, rather than only one at the start.
  #
  # The first version assumed the indented, one-key-per-line shape that settings.py writes. A test
  # caught what that misses: a hand-written settings file is usually COMPACT — the whole document on one
  # line — and that is exactly the `<` side of every diff this filter is used on. Such a line does not
  # begin with a key, so nothing engaged and an `authToken` value printed in the clear. Scanning also
  # handles nesting for free: `"apiKeyHelper": {"cmd": "…"}` puts `cmd` on the same line, and `cmd` is
  # judged on its own merits.
  function scan(t,   out, i, n, rest, kq, key, lk, ch, j, val) {
    out = ""; i = 1; n = length(t)
    while (i <= n) {
      rest = substr(t, i)
      if (match(rest, /^"[^"]*"[ \t]*:[ \t]*/)) {
        kq = substr(rest, 1, RLENGTH)
        key = kq; sub(/^"/, "", key); sub(/"[ \t]*:[ \t]*$/, "", key)
        lk = tolower(key)
        out = out kq
        i += RLENGTH
        ch = substr(t, i, 1)
        if (ch == "{" || ch == "[") { out = out ch; i++; continue }
        if (ch == "\"") {
          j = i + 1
          while (j <= n) {
            if (substr(t, j, 1) == "\\") { j += 2; continue }   # an escaped quote is not the end
            if (substr(t, j, 1) == "\"") break
            j++
          }
          val = substr(t, i, j - i + 1)
          i = j + 1
        } else {
          j = i
          while (j <= n && substr(t, j, 1) !~ /[,}\]]/) j++
          val = substr(t, i, j - i)
          i = j
        }
        if (lk in SAFE) out = out shapes(val)
        else if (lk in ROUTE) out = out route_value(val)
        else out = out "\"" mark "\""
        continue
      }
      out = out substr(t, i, 1); i++
    }
    return out
  }
  {
    line = $0
    pre = ""
    # Preserve a diff marker ("< ", "> ") or a grep -n line number, then reason about the rest.
    if (match(line, /^[<>][ \t]?/) || match(line, /^[0-9]+:/)) {
      pre = substr(line, 1, RLENGTH); line = substr(line, RLENGTH + 1)
    }
    lead = ""
    if (match(line, /^[ \t]+/)) { lead = substr(line, 1, RLENGTH); line = substr(line, RLENGTH + 1) }

    if (line ~ /^"/ && line !~ /^"[^"]*"[ \t]*:/) {
      # A bare array element. Allowlisted to the permission-grant shape, Tool(...) — what the diff is
      # for — and redacted otherwise.
      if (line ~ /^"[A-Za-z][A-Za-z0-9_]*\(.*\)",?$/) { print pre lead shapes(line); next }
      print pre lead "\"" mark "\"" (line ~ /,$/ ? "," : "")
      next
    }
    # shapes() over the scanned result, not instead of it. scan() copies anything that is not a
    # `"key": value` pair verbatim — which is correct for structure, but a bare value with no key at all
    # (report_environment hands this filter a raw exported URL) then reached the output untouched, and a
    # `user:pass@host` in it printed in the clear. The two layers are not alternatives.
    print pre lead shapes(scan(line))
  }'
}

# Does this URL point at this machine? S1 in review, and shellcheck SC2102 flagged the shape: this was a
# `case` pattern list in which `[::1]` is an unquoted BRACKET EXPRESSION — a glob matching one of the
# characters `:` and `1`, not the literal string — so it never matched an IPv6 loopback. With the three
# shapes the patterns simply lacked (no port, 0.0.0.0, https), FOUR of six loopback forms reported
# "clean" and exited 0 while the user's shell was still routed at a dead proxy: silence on the exact
# condition this report exists to surface.
#
# settings.py::_is_loopback already handled all six, and the Go test named for that property tests the
# PYTHON function. Nothing tested this shell path; TestTheHatchRunsUnderEveryShellItClaims and the
# exported-base-URL test now do, under every shell on the box.
is_loopback_url() {
  _u="${1#http://}"
  _u="${_u#https://}"
  [ "$_u" != "$1" ] || return 1          # no http(s) scheme: not ours to judge
  _host="${_u%%/*}"                      # strip any path
  case "$_host" in
    \[*\]*) _host="${_host%%\]*}]" ;;    # [::1] or [::1]:8787 -> [::1]
    *) _host="${_host%%:*}" ;;           # host:port -> host
  esac
  case "$_host" in
    127.0.0.1|localhost|0.0.0.0|"[::1]"|::1) return 0 ;;
  esac
  return 1
}

# ---------------------------------------------------------------------------
# The credential / environment report.
#
# Printed on EVERY path, including when there is nothing to restore, because the last real
# incident in this plugin's history was not a routing fault at all: both ANTHROPIC_API_KEY and
# ANTHROPIC_AUTH_TOKEN were set, the gateway rejected the one that won, and the export was one
# line in a shell rc. No amount of settings surgery reaches that, and a hatch that stays silent
# about it sends the user back to a session that still fails.
#
# Values are never printed. `grep -n` gives the line, and the value is replaced before output —
# a recovery tool that echoes a live API key into a terminal buffer is not a recovery tool.
# ---------------------------------------------------------------------------
report_environment() {
  say ""
  say "Environment (this is not something restoring a file can fix):"

  set +u
  api_set=0; tok_set=0
  [ -n "$ANTHROPIC_API_KEY" ] && api_set=1
  [ -n "$ANTHROPIC_AUTH_TOKEN" ] && tok_set=1
  base="$ANTHROPIC_BASE_URL"
  set -u

  if [ "$api_set" = 1 ] && [ "$tok_set" = 1 ]; then
    say "  ! ANTHROPIC_API_KEY and ANTHROPIC_AUTH_TOKEN are BOTH set in this shell."
    say "    Claude Code warns about this at startup and it is a real fault: only one of them"
    say "    wins, and if that is the stale one every request fails with 401 no matter how the"
    say "    routing is configured. Unset the one your gateway does not know."
  elif [ "$api_set" = 1 ] || [ "$tok_set" = 1 ]; then
    say "  - one Anthropic credential variable is set in this shell (value not shown)"
  else
    say "  - no Anthropic credential variable is set in this shell"
  fi

  if [ -n "${base:-}" ]; then
    if is_loopback_url "$base"; then
        say "  ! ANTHROPIC_BASE_URL is exported in THIS SHELL and points at a local proxy:"
        # Through redact, like every other printer of real content. This was the fourth site and the
        # worst one: it sits in the function whose own header promises values are never printed, and
        # it honoured that for the credential variables and the rc grep while echoing a base URL
        # verbatim — so an exported https://svc:SECRET@gw/anthropic went straight to the terminal.
        say "      $(printf '%s' "$base" | redact)"
        say "    A settings file cannot override an exported variable, so this shell stays"
        say "    routed until you unset it. Fix the line reported below, then open a new shell."
        INCOMPLETE=1
    else
      say "  - ANTHROPIC_BASE_URL is exported in this shell: $(printf '%s' "$base" | redact)"
    fi
  fi

  # Where those exports come from. Nothing here is modified — the file and line are the point,
  # because the user has to decide which credential is the good one, and this script cannot.
  found=0
  for rc in "$HOME/.zshrc" "$HOME/.zshenv" "$HOME/.zprofile" "$HOME/.bashrc" \
            "$HOME/.bash_profile" "$HOME/.profile"; do
    [ -f "$rc" ] || continue
    # ANTHROPIC_* rather than four hand-listed names. S2 in review: a credential in
    # ANTHROPIC_CUSTOM_HEADERS — the variable this project's own machines use for exactly that —
    # produced "No ANTHROPIC_* assignments found in your shell startup files", a false all-clear
    # printed under a heading that claims to have looked. `declare -x` / `typeset -x` are accepted
    # too, since bash users do write those. The `=.*` truncation below is what keeps this safe, and it
    # is why widening the NAME side costs nothing: the value never survives to be printed.
    hits="$(grep -nE '^[[:space:]]*((export|declare|typeset)[[:space:]]+(-[a-zA-Z]+[[:space:]]+)*)?ANTHROPIC_[A-Z0-9_]+=' "$rc" 2>/dev/null \
            | sed 's/=.*/=<value not shown>/' || true)"
    [ -n "$hits" ] || continue
    [ "$found" = 0 ] && say "" && say "  ANTHROPIC_* set from shell startup files:"
    found=1
    printf '%s\n' "$hits" | while IFS= read -r line; do
      printf '    %s:%s\n' "$rc" "$line"
    done
  done
  if [ "$found" = 0 ]; then
    say ""
    say "  No ANTHROPIC_* assignments found in your shell startup files."
  else
    say ""
    say "  Those lines run in every new shell. If one of them is the credential your gateway"
    say "  rejects, editing it is the fix; this script does not touch them."
  fi
}

final_advice() {
  say ""
  if [ "$RESTORED" -gt 0 ]; then
    say "Next: start a NEW claude session. A running one keeps the environment it started with,"
    say "so the session you are recovering will keep failing until you restart it."
  fi
  say "The plugin's hooks stay installed and are inert once routing is gone — each one exits"
  say "immediately in a project that is not routed. Nothing needs to be uninstalled to unblock you."
  say "Any proxy still running exits on its own idle timeout; nothing here signals a process."
}

# ---------------------------------------------------------------------------
# Build the plan. Two passes on purpose: everything is shown before anything is written, so the
# confirmation prompt is answered with the full picture rather than with trust.
#
# One fixed loop over the (at most three) files context-guru can ever write to, rather than a
# global record of everything it ever HAS written to. That is the fix, not a detail: the old design
# was a single ledger, `reset-manifest.tsv`, that nothing ever removed an entry from — not even a
# completely clean `/context-guru:uninstall` — so a file touched once (a `--scope user` test months
# ago, a project cleanly uninstalled since) stayed on it permanently, and running this script for a
# completely different, currently-broken project would sweep that unrelated scope back in too and
# revert it to a copy taken long before whatever the user had since edited into it — with no
# `.context-guru-backup-*` beside it to explain why, because the backup was the ORIGINAL copy under
# a shared state directory, nowhere near the file itself. Checking exactly the three real candidate
# paths, each carrying its own recovery folder beside it, makes that leak structurally impossible:
# there is no shared list left to leak from.
# ---------------------------------------------------------------------------
say "$PROG"
say ""
say "Files context-guru can write to:"

PLAN="$(mktemp "${TMPDIR:-/tmp}/cg-reset-plan.XXXXXX")"
# ENV_TMP is in here too (set later, in the empty-plan branch): an interrupt between its mktemp and
# its rm would otherwise leak a temp file into TMPDIR.
trap 'rm -f "$PLAN" ${ENV_TMP:+"$ENV_TMP"}' EXIT INT TERM

for path in "./.claude/settings.local.json" "./.claude/settings.json" "$CLAUDE_DIR_USER/settings.json"; do
  if [ ! -e "$path" ]; then
    say "  $path"
    say "      (not present)"
    continue
  fi
  recovery_dir="$(dirname "$path")/$RECOVERY_DIR_NAME"
  if [ ! -d "$recovery_dir" ]; then
    # No record at all for this file — a hand-edited settings file, an install that predates
    # recovery folders, or one wiped by hand. Answering with silence would be the most useless
    # thing this script could say to somebody whose sessions are down over exactly this file.
    say "  $path"
    hits="$(grep -n 'ANTHROPIC_BASE_URL\|ANTHROPIC_UPSTREAM\|CONTEXT_GURU_BIN' "$path" 2>/dev/null || true)"
    if [ -n "$hits" ]; then
      printf '%s\n' "$hits" | redact | sed 's/^/      /'
      say "      ^ if one of those points at 127.0.0.1 and you did not set it, delete that key."
      # A routing key IS present, with no recovery folder to act on — a real problem left for a
      # human, not a clean file. Without this, a plan whose only candidate lands here would fall
      # through to the empty-plan branch's "Nothing to restore", which is the opposite of true.
      INCOMPLETE=1; FILES_UNFIXED=1
    else
      say "      (no context-guru keys — this one is fine)"
    fi
    ls -1t "${path%.json}".context-guru-backup-*.json 2>/dev/null | sed 's/^/      backup: /' || true
    continue
  fi
  # Does this file show ANY sign of being ours RIGHT NOW? A recovery folder existing only proves
  # context-guru touched this file AT SOME POINT — it may have been cleanly uninstalled since, and
  # edited by the user for reasons that have nothing to do with context-guru afterwards. Per-scope
  # colocation stops that leaking to a DIFFERENT scope's run (see the comment above), but a scope's
  # own folder can still go stale in exactly this way, so the check stays.
  #
  # `"$context-guru"` is the meta key EVERY context-guru write sets (routing or statusline-only),
  # so it alone would be enough — the ANTHROPIC_*/CONTEXT_GURU_BIN check is belt-and-suspenders for
  # a hand-edited file that dropped the meta block but kept a key we wrote.
  if ! grep -qE 'ANTHROPIC_BASE_URL|ANTHROPIC_UPSTREAM|CONTEXT_GURU_BIN' "$path" 2>/dev/null \
     && ! grep -qF '$context-guru' "$path" 2>/dev/null; then
    say "  $path"
    say "      no context-guru keys here now (uninstalled through the normal path since this was"
    say "      recorded, or never actually left in this state) — skipping rather than reverting a"
    say "      file that is not currently context-guru's to fix"
    continue
  fi
  basename="$(basename "$path")"
  # Content-bearing recovery files end in .json; the marker does not (it is not JSON content) —
  # see the matching PRE_INSTALL_SUFFIX/recovery_stem() comment in settings.py, which this mirrors.
  stem="${basename%.json}"
  created_marker="$recovery_dir/$stem.created-by-us"
  original="$recovery_dir/$stem.pre-install.json"
  if [ -e "$created_marker" ]; then
    say "  $path"
    say "      DELETE (context-guru created this file; it did not exist before)"
    printf 'delete\t-\t%s\n' "$path" >> "$PLAN"
  elif [ -s "$original" ]; then
    # -s, not -f. B1 in review: a Ctrl-C during the first install used to leave a TRUNCATED copy that
    # O_EXCL then made permanent, and `-f` accepted it — so the hatch cheerfully restored an 8-byte
    # fragment over a real settings file and reported success. settings.py can no longer produce such
    # a file (copy_once writes to a temp and hardlinks it into place), but this side must refuse one
    # anyway: the copies are durable state that older versions of the plugin already wrote, and a
    # recovery tool should not depend on its writer having been correct.
    say "  $path"
    if cmp -s "$original" "$path"; then
      # Already identical to the pre-install copy, so a restore would copy a file onto itself and
      # leave another .context-guru-prereset-* behind for nothing. Running this twice is a normal
      # thing to do — the first run tells you to open a new session, and people re-run it to check
      # — so the second run has to be a genuine no-op rather than quiet extra work.
      say "      already matches the pre-install copy — nothing to do"
    else
      say "      RESTORE from the copy taken before the first edit:"
      say "      $original"
      # A whole-file restore reverts EVERYTHING in the file, not just the routing — and for
      # .claude/settings.local.json that is not hypothetical, because Claude Code appends permission
      # grants to it as the user approves tools. A months-old install means months of grants. The
      # copy of the current file taken below makes it recoverable, but silently reverting a user's
      # settings while a confirmation prompt says only "restore these files?" is not a choice they
      # were given. So: show them, in the plan, before they answer.
      #
      # `diff` rather than a JSON-aware comparison on purpose: parsing JSON in sh is exactly the
      # cleverness a recovery tool must not contain, and a plain diff of two small files answers the
      # question the user actually has, which is "what am I about to lose".
      say ""
      say "      ! this reverts the WHOLE file, not just the routing. Anything you changed in it"
      say "        since installing goes back too — permission grants Claude Code appended as you"
      say "        approved tools, a model or theme you set. Your current version is copied to"
      say "        $recovery_dir/ first (a *.pre-reset-*.json file there), so this is undoable."
      # The diff is shown as EVIDENCE, with no claim about which side of it is the user's.
      #
      # The first version of this counted "lines that are not context-guru's" by grepping our key
      # names out, and the number was false precision twice over: settings.py rewrites the file with
      # indent=2, so a compact original diffs on every line, and our own metadata spans lines that
      # carry none of the words being filtered. It reported 16 lines of "your" changes for a file
      # whose only real change was two permission grants. Since sh cannot compare JSON semantically
      # — and parsing it here is exactly the cleverness this script must not contain — the honest
      # move is to show the difference and say plainly what it includes.
      if command -v diff >/dev/null 2>&1; then
        dl="$(diff "$original" "$path" 2>/dev/null | grep -c '^[<>]' || true)"
        if [ "${dl:-0}" -gt 0 ]; then
          say ""
          say "        The two files differ (this includes context-guru's own keys, and the"
          say "        reformatting it applied when it wrote the file):"
          # 30, not 14. A hand-written settings file is usually compact and settings.py rewrites
          # it pretty-printed, so the first dozen diff lines are pure reformatting — at 14 the cut
          # landed BEFORE the user's own change (an added permission grant) in a test, which is the
          # one line they actually needed to see. Still bounded, because this prints to a terminal.
          diff "$original" "$path" 2>/dev/null | grep '^[<>]' | head -30 | redact | sed 's/^/          /'
          [ "${dl:-0}" -gt 30 ] && say "          ... and $((dl - 30)) more line(s)"
          say "        (\"<\" is the pre-install copy, \">\" is your file now.)"
        fi
      fi
      say ""
      printf 'restore\t%s\t%s\n' "$original" "$path" >> "$PLAN"
    fi
  else
    say "  $path"
    # No `.pre-install` at all — a marker file's absence carries no separate "missing" vs. "never
    # taken" distinction the way a manifest's `-` sentinel used to. Either this file already
    # carried context-guru's keys when it was first recorded (the common, honest case — see
    # `_looks_routed_by_us` in settings.py), or something outside this script removed the copy
    # since. Both read the same to a user: nothing here holds the original content.
    say "      ! no pre-edit copy was taken for this file — it already carried context-guru's"
    say "        keys when the record was created, so nothing here holds its original content."
    newest="$(ls -1t "$recovery_dir/$stem.context-guru-backup-"*.json 2>/dev/null | head -1 || true)"
    if [ -n "$newest" ]; then
      say "        a timestamped backup exists and is NOT restored automatically, because it"
      say "        may be a copy of a later state rather than of your original:"
      say "          $newest"
      say "        compare it yourself, then: cp \"$newest\" \"$path\""
    fi
    say "        left untouched."
    INCOMPLETE=1; FILES_UNFIXED=1
  fi
done

if [ ! -s "$PLAN" ]; then
  say ""
  # report_environment sets INCOMPLETE, and this branch used to test INCOMPLETE BEFORE running it —
  # so an ANTHROPIC_BASE_URL exported in the user's shell (the one condition no file restore can fix)
  # printed its `!` and still exited 0, while the identical condition on a non-empty plan exited 3.
  #
  # Its output is captured rather than printed here so the summary sentence still comes first. A
  # redirect does NOT create a subshell in POSIX sh, so INCOMPLETE set inside the function survives —
  # which `ENV_OUT=$(report_environment)` would silently NOT do, and the bug would be invisible.
  ENV_TMP="$(mktemp "${TMPDIR:-/tmp}/cg-reset-env.XXXXXX")"
  report_environment > "$ENV_TMP"
  # An empty plan is reached from TWO very different states, and saying the reassuring one about
  # both was a review finding: the missing-copy branch above deliberately adds nothing to the plan,
  # so a routed file with no original produced the correct "! ..." block and then "already back to
  # their pre-install state" as the LAST line — the opposite of the truth, to the one user who is
  # locked out. Exit 3 was right; nobody reads an exit code, they read the last line.
  if [ "$FILES_UNFIXED" = 0 ]; then
    say "Nothing to restore — your settings files are already back to their pre-install state."
    if [ "$INCOMPLETE" != 0 ]; then
      say ""
      say "Your FILES are fine. What is left is in the environment report below, and a settings"
      say "file cannot fix it — read the ! line there."
    fi
    cat "$ENV_TMP"; rm -f "$ENV_TMP"
    final_advice
    [ "$INCOMPLETE" = 0 ] && exit 0 || exit 3
  fi
  say "Nothing could be restored automatically — see the ! line(s) above. Your settings files are"
  say "NOT back to their pre-install state, and this tool has no copy that would put them there."
  say ""
  say "What works, in order of least effort:"
  say "  1. compare a timestamped backup and copy it back yourself — look in"
  say "     <dir>/context-guru-settings-json/ beside the file (the normal case), or beside the"
  say "     file itself if that folder could not be created:"
  say "       cp <name>.context-guru-backup-<newest>.json <file>"
  say "  2. or open the file and delete the ANTHROPIC_BASE_URL / ANTHROPIC_UPSTREAM /"
  say "     CONTEXT_GURU_BIN keys from its \"env\" block, leaving everything else alone."
  say "     That is all the routing is; nothing else has to change."
  cat "$ENV_TMP"; rm -f "$ENV_TMP"
  final_advice
  exit 3
fi

if [ "$DRY" = 1 ]; then
  say ""
  say "--dry-run: nothing written."
  report_environment
  # Honour INCOMPLETE here too. It used to exit 0 unconditionally, so --dry-run and a real run
  # disagreed about whether anything was left for a human — including when report_environment had
  # just found an exported ANTHROPIC_BASE_URL that no settings change can override.
  [ "$INCOMPLETE" = 0 ] && exit 0
  say ""
  say "(--dry-run found something a restore will not fix — see the ! lines above.)"
  exit 3
fi

if [ "$YES" = 0 ]; then
  say ""
  # </dev/tty rather than stdin: a `sh reset.sh` whose stdin is not a terminal must not read the
  # answer out of a pipe, or out of the rest of a script.
  if [ -r /dev/tty ]; then
    printf 'Restore these files now? [y/N] '
    read -r answer < /dev/tty || answer=""
    case "$answer" in
      y|Y|yes|YES) ;;
      *) say "Aborted; nothing was written."; exit 1 ;;
    esac
  else
    warn ""
    warn "$PROG: not a terminal, so nothing was written. Re-run with --yes to proceed."
    exit 2
  fi
fi

say ""
while IFS='	' read -r action original path; do
  # Reversible in its own right: whatever is there NOW is copied aside first, so a user who runs
  # this and then discovers the routing was not the problem has the routed version back.
  #
  # Written into the SAME recovery folder that already holds this file's `.pre-install` copy —
  # beside the settings file, not in a shared state directory. S6 in review flagged exactly this
  # move for the old design: a complete copy of a settings file — credentials included — sitting in
  # a project's working tree under a name no `.gitignore` anticipates is a real path to a committed
  # credential. Since then, `/context-guru:install` closes that properly rather than by relocating
  # things somewhere nobody would think to look: it adds a `.gitignore` entry for the whole recovery
  # folder deterministically (`settings.py gitignore-ensure`, no question asked, see install.sh).
  # This script does not depend on that having happened — it works either way — but benefits from
  # it exactly as the `.pre-install` copy already does.
  recovery_dir="$(dirname "$path")/$RECOVERY_DIR_NAME"
  mkdir -p "$recovery_dir" 2>/dev/null && chmod 700 "$recovery_dir" 2>/dev/null
  # Content-bearing recovery files end in .json — see the matching comment where $stem is first
  # computed, in the plan-building loop above (out of scope here; this is a separate loop reading
  # back from $PLAN, so it is recomputed).
  stem="$(basename "$path")"; stem="${stem%.json}"
  if [ -d "$recovery_dir" ] && [ -w "$recovery_dir" ]; then
    pre="$recovery_dir/$stem.pre-reset-$STAMP.json"
    preglob="$recovery_dir/$stem.pre-reset-*.json"
  else
    # An unwritable recovery folder must not turn a reversible restore into an irreversible one.
    pre="$path.context-guru-prereset-$STAMP.json"
    preglob="$path.context-guru-prereset-*.json"
  fi
  if [ -e "$path" ] && [ ! -e "$pre" ]; then
    if cp -p "$path" "$pre" 2>/dev/null || cp "$path" "$pre"; then
      say "  saved current state: $pre"
      prune_prereset "$preglob"
    else
      warn "  ! could not copy $path aside; leaving it alone rather than restoring over it"
      INCOMPLETE=1; FILES_UNFIXED=1
      continue
    fi
  fi
  case "$action" in
    delete)
      if rm -f "$path"; then
        say "  deleted:  $path"
        RESTORED=$((RESTORED + 1))
        # Nothing left in the recovery folder is useful once the file itself is gone — no
        # .pre-install (there never was one; see .created-by-us), so no reason to keep backups or
        # the pre-reset copy just taken above either. Mirrors settings.py's
        # maybe_delete_if_empty(), which removes the same folder the same way on a normal
        # uninstall. Best-effort: a folder that cannot be removed is not this script's failure —
        # the file itself is already gone, which is the property that matters.
        rm -rf "$recovery_dir" 2>/dev/null || true
      else
        warn "  ! could not delete $path"; INCOMPLETE=1; FILES_UNFIXED=1
      fi ;;
    restore)
      # cp onto the existing path, never `mv`: the settings file may be a symlink into a dotfiles
      # repository, and replacing the link would silently take the edit away from the file the
      # user actually manages. Same reason settings.py resolves the path before writing.
      # Compared after the copy, not assumed from cp's exit status. The advertised property was
      # "verify instead of trusting cp", and what the verify pass actually did was grep for key names
      # — which a truncated or empty file passes trivially, because it contains no keys at all. A
      # short write, a full disk or a signal mid-copy all end here, and none of them may be counted as
      # a restore.
      if cp "$original" "$path" && cmp -s "$original" "$path"; then
        say "  restored: $path"
        RESTORED=$((RESTORED + 1))
        # Every rolling per-edit backup for this file answers a question nobody has anymore once
        # the pre-install state is verified restored — mirrors settings.py's forget_backups().
        # This is the gap that let one survive a real `context-guru-reset` run: settings.py's own
        # `remove`/`off` commands learned this, but the hatch — a separate, plain-sh restore path —
        # never did. `.pre-reset-*.json` is deliberately left alone; that is the hatch's OWN safety
        # net for THIS run, a different concern from the generic per-edit backups.
        rm -f "$recovery_dir/$stem.context-guru-backup-"*.json 2>/dev/null || true
      else
        warn "  ! restoring $path from $original did not produce an identical file; the copy of your"
        warn "    current version is at $pre and nothing was counted as restored"
        INCOMPLETE=1; FILES_UNFIXED=1
      fi ;;
  esac
done < "$PLAN"

# ---------------------------------------------------------------------------
# Verify, rather than trusting that cp did what the plan said. This is the claim the user acts
# on — "you are unrouted now" — so it is checked against the files on disk.
# ---------------------------------------------------------------------------
say ""
say "Verifying:"
left=0
while IFS='	' read -r action original path; do
  [ -e "$path" ] || { say "  ok  $path (absent)"; continue; }
  if grep -q 'ANTHROPIC_BASE_URL\|ANTHROPIC_UPSTREAM\|CONTEXT_GURU_BIN' "$path" 2>/dev/null; then
    # Not automatically a failure: a user whose own gateway was replaced with --force gets that
    # gateway back here, and it is SUPPOSED to be in the restored file.
    say "  ?   $path still mentions an ANTHROPIC_* key:"
    grep -n 'ANTHROPIC_BASE_URL\|ANTHROPIC_UPSTREAM\|CONTEXT_GURU_BIN' "$path" | redact | sed 's/^/        /'
    if grep -q '127\.0\.0\.1\|localhost' "$path" 2>/dev/null; then
      say "        that points at a local proxy — if you did not set it yourself, delete the key."
      left=1
    else
      say "        that is your own value, restored as it was before the install."
    fi
  else
    say "  ok  $path (no context-guru keys)"
  fi
done < "$PLAN"
[ "$left" = 0 ] || { INCOMPLETE=1; FILES_UNFIXED=1; }

report_environment
final_advice

# FILES_UNFIXED chooses the sentence, INCOMPLETE chooses the exit code — the same split as the
# empty-plan branch, which is where this bug was fixed first and where it should have been fixed
# both times. Testing INCOMPLETE here meant a COMPLETELY successful restore, verified unrouted, run
# from a shell with an exported base URL (a hosted agent, or the shell they installed from) printed
# "Finished with something left for you" and never printed the count at all: $RESTORED was computed
# and thrown away on precisely the run where it is the good news, and the user who had just
# recovered was told the run did not finish.
if [ "$FILES_UNFIXED" = 0 ]; then
  say ""
  say "Done. $RESTORED file(s) put back."
  if [ "$INCOMPLETE" != 0 ]; then
    say ""
    say "The files are done. What is left is in the environment report above — a settings file"
    say "cannot fix it, so read the ! line there before starting a new session."
  fi
  [ "$INCOMPLETE" = 0 ] && exit 0 || exit 3
fi
say ""
say "Finished with something left for you — see the ! lines above."
exit 3
