#!/usr/bin/env bash
# Gate B: does an `env` block MERGE per-key across settings files, or does the
# highest-precedence file replace the whole object?
#
# Why it matters for distribution: `context-guru init` writes ANTHROPIC_BASE_URL into a
# settings file. If a lower-precedence file's `env` object is replaced wholesale by a
# higher one, then a user-scope install (~/.claude/settings.json, the LOWEST precedence)
# silently stops working in any repo that ships its own `env` block — i.e. exactly the
# most-configured repos, and with no error.
#
# Method: no model call, no network. A SessionStart hook dumps the environment it was
# given to a file. Claude Code runs SessionStart hooks before the first request, so the
# session is killed immediately afterwards and the dump is still written.
#
#   user scope (CLAUDE_CONFIG_DIR)  : CG_USER=user   CG_BOTH=user
#   project scope (.claude/…json)   :                CG_BOTH=project  CG_PROJ=project
#
# MERGE  => CG_USER=user,  CG_BOTH=project, CG_PROJ=project   (union, project wins the clash)
# REPLACE => CG_USER unset, CG_BOTH=project, CG_PROJ=project   (user object discarded)
#
# The real user's ~/.claude is never touched: CLAUDE_CONFIG_DIR relocates it.
set -uo pipefail

LAB="$(mktemp -d /tmp/cg-envmerge.XXXXXX)"
CFG="$LAB/cfgdir"
PROJ="$LAB/proj"
DUMP="$LAB/env.txt"
mkdir -p "$CFG" "$PROJ/.claude"

cat >"$CFG/settings.json" <<JSON
{
  "env": {
    "CG_USER": "user",
    "CG_BOTH": "user"
  },
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          { "type": "command", "command": "env | grep -E '^CG_' | sort > $DUMP" }
        ]
      }
    ]
  }
}
JSON

cat >"$PROJ/.claude/settings.json" <<JSON
{
  "env": {
    "CG_BOTH": "project",
    "CG_PROJ": "project"
  }
}
JSON

echo "lab: $LAB"
echo
echo "user   settings env: CG_USER=user  CG_BOTH=user"
echo "project settings env: CG_BOTH=project CG_PROJ=project"
echo

cd "$PROJ"
CLAUDE_CONFIG_DIR="$CFG" timeout 120 claude -p 'reply with the single word ok' \
  --model claude-haiku-4-5 --permission-mode bypassPermissions >"$LAB/claude.out" 2>&1
echo "claude exit=$? (output: $LAB/claude.out)"
echo

if [[ ! -s "$DUMP" ]]; then
  echo "RESULT: INCONCLUSIVE — hook produced no dump."
  echo "  Check $LAB/claude.out; the hook may not have run (trust gate?) or CG_ vars"
  echo "  were not exported to hook processes."
  exit 2
fi

echo "=== environment the hook saw ==="
cat "$DUMP"
echo

got_user=$(grep -c '^CG_USER=user$'    "$DUMP" || true)
got_both=$(grep -c '^CG_BOTH=project$' "$DUMP" || true)
got_proj=$(grep -c '^CG_PROJ=project$' "$DUMP" || true)

echo "=== verdict ==="
if [[ "$got_user" -eq 1 && "$got_both" -eq 1 && "$got_proj" -eq 1 ]]; then
  echo "MERGE — env objects union across scopes; the higher-precedence file wins only"
  echo "        the keys it actually sets. A user-scope install survives a project env block."
elif [[ "$got_user" -eq 0 && "$got_both" -eq 1 && "$got_proj" -eq 1 ]]; then
  echo "REPLACE — the project env object replaced the user one wholesale (CG_USER lost)."
  echo "        A user-scope install silently dies in any repo shipping its own env block."
  echo "        => init must write project-local scope, or verify per-repo."
else
  echo "UNEXPECTED — CG_USER=$got_user CG_BOTH=$got_both CG_PROJ=$got_proj"
  echo "        Inspect the dump above before drawing a conclusion."
fi
