#!/usr/bin/env bash
# End-to-end check of the PUBLIC TLS path, from a client's point of view.
#
# Everything here goes through https://$CG_HOST — never 127.0.0.1:4000 — because the
# failures this is meant to catch only exist once nginx is in front: a chain that is
# missing its intermediate, a cookie that loses its Secure flag, an SSE stream that
# nginx buffers into uselessness, and service-wide endpoints that a loopback gate stops
# protecting the moment the peer is the reverse proxy rather than the caller.
#
#   CG_TOKEN=cg_live_...  ./tls-smoke.sh
#
# The token comes from the environment and is never echoed, never written to a file and
# never passed on a command line another user could see in ps(1).

set -uo pipefail

CG_HOST="${CG_HOST:-contextguru.vpc.cloud9.ibm.com}"
# The IBM Internal Root CA. Passing it explicitly rather than relying on the system
# trust store is the point: it proves a client that trusts ONLY this root can verify
# the server, which is exactly the situation every IBM laptop is in.
#
# This copy lives beside the config rather than in $ETC/tls/, which is mode 0700 and
# root-only — correct for the PRIVATE KEY, wrong for a public root certificate that
# ops scripts and unprivileged clients need to read. Putting it there silently
# downgraded this script to the system trust store and turned every check below into a
# certificate error, which is a confusing way to learn about a directory mode.
CG_CA="${CG_CA:-/etc/context-guru/ibm-internal-root-ca.pem}"
BASE="https://$CG_HOST"

pass=0 fail=0
ok()   { printf '  \033[32m✓\033[0m %s\n' "$1"; pass=$((pass+1)); }
no()   { printf '  \033[31m✗\033[0m %s\n' "$1"; fail=$((fail+1)); }
head_() { printf '\n\033[1m%s\033[0m\n' "$1"; }

curlca=(curl -sS --cacert "$CG_CA")
# CA_ARG is what the openssl calls use. The earlier version rewrote only `curlca` on the
# fallback and left `-CAfile "$CG_CA"` on every openssl call, so running as a user who
# cannot read the CA reported "chain does NOT verify (missing intermediate?)" on a
# perfectly good chain — a false alarm pointing at the wrong thing entirely.
CA_ARG=(-CAfile "$CG_CA")
if [ ! -r "$CG_CA" ]; then
  echo "note: $CG_CA unreadable; falling back to the system trust store" >&2
  curlca=(curl -sS)
  CA_ARG=()
fi

# reachable() distinguishes "the service said no" from "nothing answered".
#
# Every negative check below MUST go through this, because a check of the form "this
# request failed, therefore the server correctly refused it" passes just as happily when
# the box is powered off. A dead host reporting "✓ hostname validation rejects a wrong
# name" and "✓ port 80 refuses connections" is the worst possible output: it is an
# all-clear produced by the absence of a service.
reachable() { timeout 10 bash -c "</dev/tcp/$CG_HOST/443" 2>/dev/null; }
if ! reachable; then
  printf '\033[31mFATAL\033[0m: nothing is listening on %s:443 — refusing to report\n' "$CG_HOST" >&2
  echo "  Every negative check below would 'pass' against a dead host, which reads as an" >&2
  echo "  all-clear. Start the service (systemctl status nginx context-guru) and re-run." >&2
  exit 2
fi

head_ "TLS"
if out=$(echo | timeout 15 openssl s_client -connect "$CG_HOST:443" \
          -servername "$CG_HOST" "${CA_ARG[@]}" 2>&1); then
  grep -q 'Verify return code: 0 (ok)' <<<"$out" \
    && ok "chain verifies to the IBM Internal Root CA" \
    || no "chain does NOT verify (missing intermediate?)"
  # depth=1 present means the server sent the intermediate itself. Without it, a
  # client holding only the root fails, and browsers that cache the intermediate from
  # another site succeed — the classic "works on my machine" TLS bug.
  grep -q 'depth=1' <<<"$out" && ok "intermediate is served" || no "intermediate NOT served"
  proto=$(sed -n 's/^ *Protocol *: *//p' <<<"$out" | head -1)
  case "$proto" in TLSv1.2|TLSv1.3) ok "protocol $proto" ;; *) no "unexpected protocol: $proto" ;; esac
else
  no "TLS handshake failed"
fi

# A cert valid today is not the same as a cert valid next month. 21 days is enough
# warning to get a certificate reissued through a normal review process.
#
# Fetch ONCE into a file rather than opening two more connections, and account for every
# outcome. The earlier version wrapped this in `if enddate=$(pipeline)` with no else: when
# the pipeline failed the check simply vanished from the report — no ✓, no ✗, not counted —
# so the run said "N passed, 0 failed" and nobody learned the certificate expired in three
# days. A check that can silently not-run is worse than no check.
pem=$(mktemp)
if echo | timeout 15 openssl s_client -connect "$CG_HOST:443" -servername "$CG_HOST" 2>/dev/null >"$pem" \
     && [ -s "$pem" ]; then
  if enddate=$(openssl x509 -noout -enddate -in "$pem" 2>/dev/null | cut -d= -f2) && [ -n "$enddate" ]; then
    if openssl x509 -checkend $((21*86400)) -noout -in "$pem" >/dev/null 2>&1; then
      ok "certificate valid > 21 days (expires $enddate)"
    else
      no "certificate expires within 21 days ($enddate) — reissue now"
    fi
  else
    no "could not read the certificate's expiry date (openssl x509 failed)"
  fi
else
  no "could not retrieve the certificate to check its expiry"
fi
rm -f "$pem"

# A wrong hostname MUST fail — but it must fail because the NAME was rejected, not because
# nothing answered. reachable() above already proved the port is live, so a connect
# failure here is attributable to certificate validation.
if [ -z "$(getent hosts "$CG_HOST" | awk '{print $1; exit}')" ]; then
  no "cannot resolve $CG_HOST, so the hostname-validation check cannot be trusted"
elif "${curlca[@]}" --resolve "wrong.invalid:443:$(getent hosts "$CG_HOST" | awk '{print $1; exit}')" \
     "https://wrong.invalid/healthz" >/dev/null 2>&1; then
  no "hostname validation did NOT reject a wrong name"
else
  ok "hostname validation rejects a wrong name"
fi

head_ "Service"
code=$("${curlca[@]}" -o /dev/null -w '%{http_code}' "$BASE/healthz")
[ "$code" = 200 ] && ok "/healthz 200 over TLS" || no "/healthz returned $code"

# Port 80 must NOT answer. See nginx.conf: a redirect would arrive after the client had
# already put its token on the wire in cleartext, so refusing the connection is the
# safe outcome and an open port 80 is the regression.
if timeout 5 bash -c "</dev/tcp/$CG_HOST/80" 2>/dev/null; then
  no "port 80 is OPEN — a cleartext request would leak the token before any redirect"
else
  ok "port 80 refuses connections"
fi

head_ "Service-wide endpoints are not public"
# These are aggregates over EVERY tenant. They are gated on the caller being on this
# host, and that gate is only meaningful if it accounts for the reverse proxy: behind
# nginx every request has a loopback peer. A 200 here is a cross-tenant data leak.
for p in /stats /metrics; do
  code=$("${curlca[@]}" -o /dev/null -w '%{http_code}' "$BASE$p")
  [ "$code" = 403 ] && ok "$p refused through nginx (403)" \
                    || no "$p returned $code through nginx — expected 403"
done
# ...while staying available to local ops and the Prometheus job on 127.0.0.1:4000.
code=$(curl -sS -o /dev/null -w '%{http_code}' http://127.0.0.1:4000/stats 2>/dev/null || echo 000)
[ "$code" = 200 ] && ok "/stats still 200 on loopback (ops + Prometheus)" \
                  || no "/stats on loopback returned $code — ops visibility broken"

head_ "Per-tenant data requires a credential"
for p in /api/stats /api/requests /api/sessions; do
  code=$("${curlca[@]}" -o /dev/null -w '%{http_code}' "$BASE$p")
  [ "$code" = 401 ] && ok "$p 401 without a token" || no "$p returned $code without a token"
done

head_ "Grafana is behind the manager gate, and its sign-in header cannot be forged"
# X-Cg-Grafana-User is an AUTHENTICATION: Grafana's auth-proxy signs in whoever it names,
# as an Admin over every tenant's spend. nginx must therefore refuse an unauthenticated
# request whatever headers it carries, and must never pass a client's own copy through.
# Both checks below, and the body check is not decoration: a 401 rendered by GRAFANA would
# mean the request reached it and only the login step said no.
for hdr in "" "X-Cg-Grafana-User: attacker@ibm.com"; do
  what=$([ -z "$hdr" ] && echo "no credential" || echo "a forged sign-in header")
  body=$(mktemp)
  code=$("${curlca[@]}" ${hdr:+-H "$hdr"} -o "$body" -w '%{http_code}' "$BASE/grafana/api/user")
  [ "$code" = 401 ] && ok "/grafana/ 401 with $what" \
                    || no "/grafana/ returned $code with $what — expected 401"
  # nginx's own refusal page names nginx and nothing else. Anything Grafana-shaped in here
  # means the gate let the request through to it.
  if grep -qiE 'grafana|isGrafanaAdmin|orgRole|<script' "$body"; then
    no "the refusal body carries Grafana content ($(wc -c <"$body") bytes) — the gate leaked"
  else
    ok "the refusal body carries zero Grafana content ($(wc -c <"$body") bytes)"
  fi
  rm -f "$body"
done

if [ -z "${CG_TOKEN:-}" ]; then
  head_ "Authenticated checks"
  echo "  skipped: set CG_TOKEN=cg_live_... to run them"
else
  head_ "Authenticated checks"
  jar=$(mktemp); trap 'rm -f "$jar"' EXIT
  # The token travels in a request BODY, not a URL, so it stays out of nginx's access
  # log; --data @- keeps it off the command line and out of ps(1).
  code=$(printf '{"token":"%s"}' "$CG_TOKEN" \
    | "${curlca[@]}" -X POST "$BASE/api/login" -H 'Content-Type: application/json' \
        --data @- -c "$jar" -o /dev/null -w '%{http_code}')
  if [ "$code" = 200 ]; then
    ok "dashboard login 200"
    # Secure is the whole reason to test through TLS: the flag is set from
    # X-Forwarded-Proto, so it is present here and correctly absent for a local
    # http:// install. Field 4 of the Netscape cookie jar is the secure flag.
    awk 'NF>=7 && $6=="cg_dash" && $4=="TRUE"' "$jar" | grep -q . \
      && ok "cg_dash cookie has Secure" || no "cg_dash cookie is NOT Secure over TLS"
    # Pinned to cg_dash, not just "some cookie is HttpOnly". A bare grep for the marker
    # passes if ANY cookie carries it, so cg_dash could regress to script-readable while a
    # different HttpOnly cookie kept the check green — shipping an XSS-readable auth cookie
    # with a ✓ beside it. In the Netscape jar the flag is a `#HttpOnly_` prefix on the
    # DOMAIN field, so it has to be tested on cg_dash's own row.
    awk 'NF>=7 && $6=="cg_dash" && $1 ~ /^#HttpOnly_/' "$jar" | grep -q . \
      && ok "cg_dash cookie has HttpOnly" || no "cg_dash cookie is not HttpOnly"
    for p in /api/stats /api/sessions; do
      code=$("${curlca[@]}" -b "$jar" -o /dev/null -w '%{http_code}' "$BASE$p")
      [ "$code" = 200 ] && ok "$p 200 with cookie" || no "$p returned $code with cookie"
    done
    # SSE must arrive incrementally. With proxy_buffering on, nginx holds the whole
    # response and the dashboard looks simply dead.
    #
    # Two traps this avoids, both of which produced a false failure here first:
    #   - Write to a FILE, never through a pipe. `| head -c N` emits nothing until it
    #     has N bytes, so piping a trickling stream into it reports "no data" no matter
    #     how well the stream works.
    #   - Wait past the 20s keepalive. On an idle service the first bytes are the
    #     server's `: keepalive` comment, so any window under 20s times out on a
    #     perfectly healthy feed.
    # Arriving keepalive IS the proof: it originated server-side and reached us
    # unbuffered, which is exactly what proxy_buffering off has to guarantee.
    sse=$(mktemp)
    timeout 24 "${curlca[@]}" -b "$jar" -N "$BASE/api/events" -o "$sse" 2>/dev/null &
    ssepid=$!
    wait $ssepid 2>/dev/null
    if [ -s "$sse" ]; then
      ok "SSE feed streams through nginx ($(wc -c <"$sse") bytes, proxy_buffering off)"
    else
      no "SSE feed produced nothing in 24s — check proxy_buffering"
    fi
    rm -f "$sse"
  else
    no "dashboard login returned $code"
  fi
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
