#!/usr/bin/env bash
# ExecStart wrapper for context-guru.service.
#
# Its whole job is to turn systemd's credential files into the environment variables the
# upstream allow-list refers to, then exec the proxy.
#
# Why a script and not a one-liner in the unit: systemd performs its OWN variable
# expansion on ExecStart, so `$f` and `${n}` in an inline `sh -c` are substituted (with
# nothing) before sh ever sees them. Escaping every dollar as `$$` works but is a trap the
# next person edits back out, and the failure mode is a crash loop that says nothing
# useful. A file has no expansion hazard and can be run by hand.
#
# Each file in $CREDENTIALS_DIRECTORY becomes UPSTREAM_<FILENAME>_KEY, upper-cased with
# dashes turned into underscores — so /etc/context-guru/credentials/ibm-litellm becomes
# UPSTREAM_IBM_LITELLM_KEY, which is what key_env names in upstreams.yaml.

set -euo pipefail

BIN="${CONTEXT_GURU_BIN:-/usr/local/bin/context-guru-proxy}"

if [ -n "${CREDENTIALS_DIRECTORY:-}" ] && [ -d "$CREDENTIALS_DIRECTORY" ]; then
  for f in "$CREDENTIALS_DIRECTORY"/*; do
    [ -f "$f" ] || continue
    name=$(basename "$f" | tr 'a-z-' 'A-Z_')
    # Read with command substitution so a trailing newline in the file is stripped: a key
    # with a newline glued on fails upstream authentication in a way that looks like a
    # wrong key rather than a malformed one.
    value=$(cat "$f")
    export "UPSTREAM_${name}_KEY=$value"
  done
fi

# Nothing is echoed about the credentials — not even their names on failure. The proxy
# itself reports which key_env is unset, by name, without ever printing a value.
exec "$BIN" "$@"
