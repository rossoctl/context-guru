#!/bin/sh
# context-guru-reset for IBM Bob: undo the Bob plugin when Bob or the proxy is broken.
#
# It needs no Python. It removes our rc-file block, our hooks in Bob's settings and our launchd
# agent, so Bob talks to IBM directly again. It keeps the dashboard data.
#   - The rc block: perl (in macOS) removes the exact block, the same bytes setup wrote. It
#     accepts both forms: ${VAR:-} (from 2026-11-09) and the legacy bare $VAR.
#   - The hooks: when the settings file still has the bytes we wrote, we copy back the user's
#     original bytes. Else osascript (JavaScript, in macOS) removes only the groups with our tag.
# macOS only (plan D14).
set -eu

DRY=0
YES=0
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY=1 ;;
    --yes|-y) YES=1 ;;
    -h|--help) echo "usage: context-guru-reset [--dry-run] [--yes]"; exit 0 ;;
    *) echo "context-guru-reset: unknown argument: $arg" >&2; exit 2 ;;
  esac
done

STATE_BASE=${XDG_STATE_HOME:-"$HOME/.local/state"}
STATE_DIR="$STATE_BASE/context-guru-bob"
RECORD="$STATE_DIR/install.json"
HOOKS_DIR="$STATE_DIR/hooks"
SETTINGS="$HOME/.bob/settings/settings.json"
LABEL=com.rossoctl.context-guru.bob
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
TAG='# context-guru-bob-hook'

# The record is JSON we wrote ourselves, one key per line. Read two keys without Python.
record_value() {
  [ -f "$RECORD" ] || return 0
  sed -n "s/^  \"$1\": \"\\(.*\\)\",\{0,1\}\$/\\1/p" "$RECORD" | head -n 1
}
# follow PATH: print where one symlink hop leads (the bob_route rule). Print nothing for a
# dangling link or a deeper chain: we do not write through those.
follow() {
  if [ ! -L "$1" ]; then printf '%s\n' "$1"; return 0; fi
  dest=$(readlink "$1") || return 0
  case "$dest" in /*) ;; *) dest="$(dirname "$1")/$dest" ;; esac
  if [ -L "$dest" ] || [ ! -e "$dest" ]; then return 0; fi
  printf '%s\n' "$dest"
}

RECORDED_RC=$(record_value rc_file)
RC_FILE=$RECORDED_RC
if [ -n "$RC_FILE" ] && [ ! -f "$RC_FILE" ]; then
  RC_FILE=""  # the recorded file is gone: scan below, and report left_alone if we find nothing
fi
PORT=$(sed -n 's/^  "port": \([0-9][0-9]*\),\{0,1\}$/\1/p' "$RECORD" 2>/dev/null | head -n 1)
RC_CREATED=0
grep -q '^  "rc_created": true' "$RECORD" 2>/dev/null && RC_CREATED=1
if [ -z "$RC_FILE" ]; then
  # No record: setup may have been killed before it wrote one. Look in each file setup can
  # write for $SHELL, and take the first that holds our start marker. The perl step below
  # still removes the block only when it is exactly ours.
  case "$(basename "${SHELL:-}")" in
    zsh) candidates=".zshrc" ;;
    bash) candidates=".bash_profile .bash_login .profile" ;;
    *) candidates="" ;;
  esac
  for name in $candidates; do
    file=$(follow "$HOME/$name")
    if [ -n "$file" ] && [ -f "$file" ] && grep -q '^# >>> context-guru (bob) >>>$' "$file"; then
      RC_FILE=$file
      break
    fi
  done
fi
SETTINGS_LINK=$SETTINGS
SETTINGS=$(follow "$SETTINGS_LINK")

echo "record=${RECORD}"
echo "rc_file=${RC_FILE:-(none)}"
echo "settings=${SETTINGS:-$SETTINGS_LINK (a link I do not follow)}"
echo "service_file=$PLIST"

if [ "$DRY" = 1 ]; then echo "result=planned"; exit 0; fi
if [ "$YES" != 1 ]; then
  printf 'Remove context-guru from Bob (rc block, hooks, proxy service)? [y/N] '
  read -r answer
  case "$answer" in y|Y|yes|YES) ;; *) echo "result=cancelled"; exit 0 ;; esac
fi

clean=1

# 1. The rc block. The perl program removes the block only when it is exactly ours.
if [ -n "$RC_FILE" ] && [ -f "$RC_FILE" ]; then
  [ "$RC_FILE" = "$RECORDED_RC" ] || echo "rc_file_found=$RC_FILE"
  tmp="$RC_FILE.context-guru-reset.$$"
  # Exit 0: removed. 3: no block. 4: a block that is not exactly ours.
  set +e
  /usr/bin/perl -0777 -e '
    my ($path, $out) = @ARGV;
    open(my $in, "<:raw", $path) or exit 5; local $/; my $text = <$in>; close $in;
    my $start = "# >>> context-guru (bob) >>>"; my $end = "# <<< context-guru (bob) <<<";
    my $n = () = $text =~ /\Q$start\E/g; my $m = () = $text =~ /\Q$end\E/g;
    exit 3 if $n == 0 && $m == 0;
    exit 4 unless $n == 1 && $m == 1;
    my $re = qr/\n\Q$start\E\n# IBM Bob reads this when it starts\. Remove it with the context-guru-uninstall skill\.\nif \[ "(?:\$VSCODE_RESOLVING_ENVIRONMENT|\$\{VSCODE_RESOLVING_ENVIRONMENT:-\})" = 1 \] && \[ "(?:\$__CFBundleIdentifier|\$\{__CFBundleIdentifier:-\})" = com\.ibm\.software\.bob \]; then\n  export VITE_GATEWAY_BASE_URL=http:\/\/127\.0\.0\.1:(\d{1,5})\nfi\nexport BOB_GATEWAY_URL=http:\/\/127\.0\.0\.1:(\d{1,5})\n\Q$end\E\n/;
    $text =~ $re or exit 4; exit 4 unless $1 eq $2;
    $text =~ s/$re//;
    open(my $o, ">:raw", $out) or exit 5; print $o $text; close $o or exit 5; exit 0;
  ' "$RC_FILE" "$tmp"
  status=$?
  set -e
  case "$status" in
    0)
      if [ "$RC_CREATED" = 1 ] && [ ! -s "$tmp" ]; then
        rm -f "$tmp" "$RC_FILE"
      else
        chmod "$(stat -f %Lp "$RC_FILE")" "$tmp"
        mv "$tmp" "$RC_FILE"
      fi
      echo "rc_block=removed" ;;
    3) rm -f "$tmp"; echo "rc_block=absent" ;;
    *) rm -f "$tmp"; clean=0; echo "rc_block=left_alone"
       echo "rc_detail=The block in $RC_FILE is not exactly ours. Remove it by hand, between the marker lines." ;;
  esac
elif [ -n "$RECORDED_RC" ]; then
  # The record names a file we cannot find, and no startup file holds our block. We cannot tell
  # where the block went, so we do not claim it is gone.
  clean=0
  echo "rc_block=left_alone"
  echo "rc_detail=$RECORDED_RC is missing. Check your startup files for the context-guru (bob) block."
else
  echo "rc_block=absent"
fi

# 2. Our hooks. The exact restore runs only when the record says it is still safe, the file is
# still the bytes we wrote, and the user's bytes are there (bob_hooks.remove has the same rule).
existed=false
grep -q '"existed": true' "$HOOKS_DIR/record.json" 2>/dev/null && existed=true
exact=false
if [ -n "$SETTINGS" ] && [ -f "$SETTINGS" ] && [ -f "$HOOKS_DIR/after.json" ] &&
   grep -q '"exact": true' "$HOOKS_DIR/record.json" 2>/dev/null &&
   cmp -s "$SETTINGS" "$HOOKS_DIR/after.json"; then
  if [ "$existed" = false ] || [ -f "$HOOKS_DIR/before.json" ]; then exact=true; fi
fi
result=""
if [ "$exact" = true ]; then
  if [ "$existed" = true ]; then
    mode=$(stat -f %Lp "$SETTINGS")
    cp "$HOOKS_DIR/before.json" "$SETTINGS.context-guru-reset.$$"
    chmod "$mode" "$SETTINGS.context-guru-reset.$$"
    mv "$SETTINGS.context-guru-reset.$$" "$SETTINGS"
  else
    rm -f "$SETTINGS"
  fi
  result=removed
  echo "hooks=removed"
elif [ -n "$SETTINGS" ] && [ -f "$SETTINGS" ] && grep -q "$TAG" "$SETTINGS"; then
  # Drop an empty `hooks` key only when the user's file had none before setup.
  had_hooks=true
  grep -q '"had_hooks_key": false' "$HOOKS_DIR/record.json" 2>/dev/null && had_hooks=false
  mode=$(stat -f %Lp "$SETTINGS")
  set +e
  # The program goes to a temp file first: a heredoc inside $(...) breaks on its quotes.
  js=$(mktemp "${TMPDIR:-/tmp}/context-guru-reset.XXXXXX")
  cat > "$js" <<'EOF'
// JSON.parse keeps the last of two equal keys and drops the other. We refuse such a file, as
// bob_hooks.py does, rather than rewrite it without the user's first value.
function hasDuplicateKeys(src) {
  var stack = [], i = 0, n = src.length, expectKey = false;
  while (i < n) {
    var c = src[i];
    if (c === '"') {
      var j = i + 1, s = "";
      while (j < n && src[j] !== '"') { if (src[j] === "\\") { s += src[j]; j++; } s += src[j]; j++; }
      var k = j + 1;
      while (k < n && /\s/.test(src[k])) { k++; }
      if (src[k] === ":" && stack.length && stack[stack.length - 1] !== null) {
        var seen = stack[stack.length - 1];
        if (Object.prototype.hasOwnProperty.call(seen, s)) { return true; }
        seen[s] = true;
      }
      i = j + 1; continue;
    }
    if (c === "{") { stack.push({}); } else if (c === "[") { stack.push(null); }
    else if (c === "}" || c === "]") { stack.pop(); }
    i++;
  }
  return false;
}

function run(argv) {
  ObjC.import("Foundation");
  var path = argv[0], tag = argv[1], hadHooks = argv[2] === "true";
  var text = $.NSString.stringWithContentsOfFileEncodingError(path, $.NSUTF8StringEncoding, null);
  if (!text) { return "unreadable"; }
  var data;
  try { data = JSON.parse(text.js); } catch (e) { return "not_plain_json"; }
  if (hasDuplicateKeys(text.js)) { return "duplicate_keys"; }
  var hooks = data.hooks;
  if (!hooks || typeof hooks !== "object") { return "absent"; }
  var ours = function (group) {
    return group && Array.isArray(group.hooks) && group.hooks.length > 0 &&
      group.hooks.every(function (h) { return h && typeof h.command === "string" &&
        h.command.slice(-tag.length) === tag; });
  };
  Object.keys(hooks).forEach(function (event) {
    if (!Array.isArray(hooks[event])) { return; }
    var kept = hooks[event].filter(function (g) { return !ours(g); });
    if (kept.length) { hooks[event] = kept; } else { delete hooks[event]; }
  });
  if (!hadHooks && Object.keys(hooks).length === 0) { delete data.hooks; }
  var out = $(JSON.stringify(data, null, 2) + "\n");
  return out.writeToFileAtomicallyEncodingError(path, true, $.NSUTF8StringEncoding, null) ? "removed" : "write_failed";
}
EOF
  result=$(osascript -l JavaScript "$js" "$SETTINGS" "$TAG" "$had_hooks" 2>&1)
  rm -f "$js"
  # writeToFileAtomically replaces the file with a new one in the default mode: put ours back.
  [ "$result" = removed ] && chmod "$mode" "$SETTINGS"
  set -e
  echo "hooks=$result"
  [ "$result" = removed ] || [ "$result" = absent ] || clean=0
else
  result=absent
  echo "hooks=absent"
fi
if [ "$result" = removed ] || [ "$result" = absent ]; then
  rm -f "$HOOKS_DIR/record.json" "$HOOKS_DIR/before.json" "$HOOKS_DIR/after.json"
  rmdir "$HOOKS_DIR" 2>/dev/null || true
fi

# 3. The launchd agent: only ours (ContextGuruManaged = true).
if [ -f "$PLIST" ]; then
  if /usr/bin/plutil -extract ContextGuruManaged raw -o - "$PLIST" 2>/dev/null | grep -qx true; then
    launchctl bootout "gui/$(id -u)/$LABEL" >/dev/null 2>&1 || true
    rm -f "$PLIST"
    echo "service=stopped"
  else
    clean=0
    echo "service=not_ours"
  fi
else
  echo "service=absent"
fi

# Only a config of ours (its first line is our marker), as uninstall does. A file the user wrote stays.
if [ -n "$PORT" ] && [ "$(head -n 1 "$STATE_DIR/bob-$PORT.yaml" 2>/dev/null)" = "# Managed by the context-guru Bob plugin." ]; then
  rm -f "$STATE_DIR/bob-$PORT.yaml"
fi
if [ "$clean" = 1 ]; then
  rm -f "$RECORD"
  echo "result=removed"
else
  echo "result=partly_removed"
fi
echo "data_dir=$STATE_DIR"
echo "Next: quit and reopen Bob, then log in once. Bob keeps its login per backend address."
