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
#   * it is INSTALLED OUTSIDE THE PLUGIN, into the state directory next to its own backups. A
#     hatch that lives inside the thing that broke is unavailable exactly when it is needed —
#     `/plugin uninstall`, a marketplace refresh or a wiped plugin cache all take it with them;
#   * the primary path is `cp`, restoring a copy of each settings file taken BEFORE the first
#     edit, so recovery does not depend on parsing anything;
#   * it takes its own backup before it writes, so running it is itself reversible;
#   * it prints what it will do and asks, unless told `--yes`.
#
# It restores ROUTING. It does not fix a credential — see the report it prints at the end, which
# names the file and line an ANTHROPIC_* variable is exported from without ever printing a value.

set -eu

PROG="context-guru-reset"
STATE="${CONTEXT_GURU_STATE:-${XDG_STATE_HOME:-$HOME/.local/state}/context-guru}"
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

usage: $PROG [--dry-run] [--yes] [--state DIR]

  --dry-run   show what would change and exit without writing anything
  --yes       do not ask for confirmation (for non-interactive use)
  --state DIR read the record from DIR instead of
              \${XDG_STATE_HOME:-\$HOME/.local/state}/context-guru

What it does, in order:
  1. reads the record of every settings file context-guru edited;
  2. copies each of those aside (.context-guru-prereset-*) so this is reversible too;
  3. restores each from the copy taken before context-guru's first edit — or deletes the
     file, if context-guru is the reason it exists;
  4. verifies no routing key is left, and reports anything it could not fix.

It never stops processes, never touches the network, and never edits a file it has no record
of editing.
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY=1 ;;
    --yes|-y) YES=1 ;;
    --state) shift; [ $# -gt 0 ] || { echo "$PROG: --state needs a directory" >&2; exit 2; }; STATE="$1" ;;
    --state=*) STATE="${1#--state=}" ;;
    -h|--help) usage; exit 0 ;;
    *) echo "$PROG: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

MANIFEST="$STATE/reset-manifest.tsv"

say()  { printf '%s\n' "$*"; }
warn() { printf '%s\n' "$*" >&2; }

# ---------------------------------------------------------------------------
# redact: a filter for anything that prints FILE CONTENT.
#
# Found in review, and it contradicted this script's own stated principle 150 lines below: the
# whole-file warning diffs the settings file, `env` is exactly where people keep
# ANTHROPIC_API_KEY, and so `--dry-run` — the invocation the docs tell people to run FIRST —
# printed a live key twice, once per side of the diff.
#
# Who reads this output is what makes it worse than the usual secret-in-a-log: somebody debugging
# a 401, whose next move is very likely to paste the whole thing into an issue, a chat or a
# screenshot, precisely BECAUSE the output is designed to be read and acted on.
#
# Applied at every site that prints file content rather than at the one the reviewer found — the
# diff, the verify pass and the no-record grep all echo lines from the user's settings — because
# "remember to filter this one too" is how the first leak happened.
#
# Redacts the VALUE and keeps the line, so the diff still shows THAT a credential line changed,
# which is the whole point of showing a diff. Three shapes:
#   * any JSON key whose name contains key/token/secret/password/credential, in any case;
#   * an Anthropic/OpenAI-style `sk-...` value, whatever the field is called;
#   * credentials embedded in a URL (https://user:pass@host).
#
# Case-insensitivity is spelled out with bracket classes on purpose: BSD sed (macOS, where this
# script runs most) has no `I` flag on `s///`, so `[Kk][Ee][Yy]` is the portable spelling.
# ---------------------------------------------------------------------------
REDACT_NAME='[Kk][Ee][Yy]|[Tt][Oo][Kk][Ee][Nn]|[Ss][Ee][Cc][Rr][Ee][Tt]|[Pp][Aa][Ss][Ss][Ww][Oo][Rr][Dd]|[Cc][Rr][Ee][Dd][Ee][Nn][Tt][Ii][Aa][Ll]'
redact() {
  sed -E -e "s/(\"[A-Za-z0-9_-]*(${REDACT_NAME})[A-Za-z0-9_-]*\"[[:space:]]*:[[:space:]]*\")[^\"]*/\1<value not shown>/g" \
         -e 's/sk-[A-Za-z0-9_-]{6,}/sk-<value not shown>/g' \
         -e 's#(://)[^/@[:space:]"]*:[^/@[:space:]"]*@#\1<credentials not shown>@#g'
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
    case "$base" in
      http://127.0.0.1:*|http://localhost:*|http://[::1]:*)
        say "  ! ANTHROPIC_BASE_URL is exported in THIS SHELL and points at a local proxy:"
        say "      $base"
        say "    A settings file cannot override an exported variable, so this shell stays"
        say "    routed until you unset it. Fix the line reported below, then open a new shell."
        INCOMPLETE=1 ;;
      *)
        say "  - ANTHROPIC_BASE_URL is exported in this shell: $base" ;;
    esac
  fi

  # Where those exports come from. Nothing here is modified — the file and line are the point,
  # because the user has to decide which credential is the good one, and this script cannot.
  found=0
  for rc in "$HOME/.zshrc" "$HOME/.zshenv" "$HOME/.zprofile" "$HOME/.bashrc" \
            "$HOME/.bash_profile" "$HOME/.profile"; do
    [ -f "$rc" ] || continue
    hits="$(grep -nE '^[[:space:]]*(export[[:space:]]+)?ANTHROPIC_(API_KEY|AUTH_TOKEN|BASE_URL|UPSTREAM)=' "$rc" 2>/dev/null \
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
# No record at all. This is a real state — a hand-edited settings file, an install that predates
# the record, or a wiped state directory — and answering it with "nothing to do" would be the
# most useless thing this script could say to somebody whose sessions are down.
# ---------------------------------------------------------------------------
if [ ! -f "$MANIFEST" ]; then
  warn "$PROG: no record of any edit at $MANIFEST"
  say ""
  say "That means this script cannot restore anything automatically. Nothing is lost: check"
  say "these files by hand for an \"ANTHROPIC_BASE_URL\" pointing at 127.0.0.1, and either"
  say "delete that key or restore one of the timestamped backups beside the file"
  say "(*.context-guru-backup-*, newest last):"
  say ""
  # EVERY candidate is printed, present or not. Printing only the ones that exist meant that in a
  # directory that happened to hold none of them the output was the heading "check these files by
  # hand:" followed by nothing at all — which is the precise uselessness this branch was written to
  # avoid, delivered to somebody whose sessions are down. A named path they can check and rule out
  # is worth more than silence, and the absent ones are also the answer to "is it the global one?".
  for f in "./.claude/settings.local.json" "./.claude/settings.json" "$HOME/.claude/settings.json"; do
    if [ -f "$f" ]; then
      say "  $f"
      hits="$(grep -n 'ANTHROPIC_BASE_URL\|ANTHROPIC_UPSTREAM\|CONTEXT_GURU_BIN' "$f" 2>/dev/null || true)"
      if [ -n "$hits" ]; then
        printf '%s\n' "$hits" | redact | sed 's/^/      /'
        say "      ^ if one of those points at 127.0.0.1 and you did not set it, delete that key."
      else
        say "      (no context-guru keys — this one is fine)"
      fi
      ls -1t "$f".context-guru-backup-* 2>/dev/null | sed 's/^/      backup: /' || true
    else
      say "  $f"
      say "      (not present)"
    fi
  done
  say ""
  say "  The first two are relative to the project you are in: $(pwd)"
  say "  If you were routed from a different project, run this again from there."
  report_environment
  final_advice
  exit 3
fi

# ---------------------------------------------------------------------------
# Read the record and build the plan. Two passes on purpose: everything is shown before anything
# is written, so the confirmation prompt is answered with the full picture rather than with trust.
#
# Format, one edited settings file per line:  <1|0 existed before>\t<original copy|->\t<path>
# `0` means context-guru CREATED that file, so putting things back means deleting it.
# ---------------------------------------------------------------------------
say "$PROG"
say "Record: $MANIFEST"
say ""
say "Files context-guru edited:"

PLAN="$(mktemp "${TMPDIR:-/tmp}/cg-reset-plan.XXXXXX")"
trap 'rm -f "$PLAN"' EXIT INT TERM

n=0
while IFS='	' read -r existed original path; do
  case "$existed" in ''|'#'*) continue ;; esac
  [ -n "${path:-}" ] || continue
  n=$((n + 1))
  if [ ! -e "$path" ]; then
    say "  $path"
    say "      gone already — nothing to restore"
    continue
  fi
  if [ "$existed" = 0 ]; then
    say "  $path"
    say "      DELETE (context-guru created this file; it did not exist before)"
    printf 'delete\t-\t%s\n' "$path" >> "$PLAN"
  elif [ -f "$original" ]; then
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
      say "        *.context-guru-prereset-* first, so this is undoable."
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
    # `-` is what the record carries when no copy was ever taken (a project that was already
    # routed when the record was created, or a file whose first recorded touch was a REMOVAL).
    # Rendering it as a path — "the pre-edit copy is missing (-)" — reads like a bug in the tool
    # rather than a known limit of what it holds, and it is the normal case for pre-hatch installs.
    if [ "$original" = "-" ] || [ -z "$original" ]; then
      say "      ! no pre-edit copy was taken for this file — it already carried context-guru's"
      say "        keys when the record was created, so nothing here holds its original content."
    else
      say "      ! the pre-edit copy is missing ($original)"
    fi
    newest="$(ls -1t "$path".context-guru-backup-* 2>/dev/null | head -1 || true)"
    if [ -n "$newest" ]; then
      say "        a timestamped backup exists and is NOT restored automatically, because it"
      say "        may be a copy of a later state rather than of your original:"
      say "          $newest"
      say "        compare it yourself, then: cp \"$newest\" \"$path\""
    fi
    say "        left untouched."
    INCOMPLETE=1; FILES_UNFIXED=1
  fi
done < "$MANIFEST"

if [ "$n" = 0 ]; then
  say "  (none recorded)"
fi

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
  say "  1. compare a timestamped backup beside the file and copy it back yourself:"
  say "       cp <file>.context-guru-backup-<newest> <file>"
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
  pre="$path.context-guru-prereset-$STAMP"
  if [ -e "$path" ] && [ ! -e "$pre" ]; then
    if cp -p "$path" "$pre" 2>/dev/null || cp "$path" "$pre"; then
      say "  saved current state: $pre"
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
      else
        warn "  ! could not delete $path"; INCOMPLETE=1; FILES_UNFIXED=1
      fi ;;
    restore)
      # cp onto the existing path, never `mv`: the settings file may be a symlink into a dotfiles
      # repository, and replacing the link would silently take the edit away from the file the
      # user actually manages. Same reason settings.py resolves the path before writing.
      if cp "$original" "$path"; then
        say "  restored: $path"
        RESTORED=$((RESTORED + 1))
      else
        warn "  ! could not restore $path from $original"; INCOMPLETE=1; FILES_UNFIXED=1
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

if [ "$INCOMPLETE" = 0 ]; then
  say ""
  say "Done. $RESTORED file(s) put back."
  exit 0
fi
say ""
say "Finished with something left for you — see the ! lines above."
exit 3
