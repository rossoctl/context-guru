# Redeploy: from operator-pays to caller-pays

This is the cutover runbook for the change that makes each caller's **own** provider
credential reach the upstream, instead of one operator key being spent by everyone.

Read it before running it. Step 2 is the one that actually changes who pays; every
other step exists to make step 2 safe.

## Why this is a runbook and not a script

The code change alone does nothing. `key_env` on an upstream entry means "drop the
caller's auth header and inject this server key instead", and it wins over
caller-pays. A deployment whose `upstreams.yaml` still names `key_env` keeps billing
the operator no matter what the binary does — the config file decides, not the code.

The same edit is also the point of no return for existing users: after step 2 a
request with no provider key of its own gets `401`, where yesterday it was served.
That is the intended behaviour, and it will look like an outage to anyone who never
had to supply a key. Tell people first.

## 0. Pre-flight

```sh
systemctl is-active context-guru nginx          # both active
curl -sS -o /dev/null -w '%{http_code}\n' https://contextguru.vpc.cloud9.ibm.com/healthz
sudo -u cg test -r /var/lib/context-guru/cg-control.db && echo "control db readable by cg"
```

Build the binary from the reviewed commit and keep the current one to roll back to:

```sh
cd <repo> && CGO_ENABLED=1 go build -o /tmp/cg-new ./cmd/context-guru-proxy
sudo cp -a /usr/local/bin/context-guru-proxy /usr/local/bin/context-guru-proxy.prev
```

## 1. Announce, then stop

Every agent pointed at this host fails while it is down, and fails *differently*
afterwards. Stop it only once you have told users to add their own key.

```sh
sudo systemctl stop context-guru
```

## 2. Remove `key_env` from every upstream — the actual change

`deploy/service/upstreams.example.yaml` in this repo is the caller-pays shape; copy
its structure. Keep every `name`, `dialect` and `base_url` byte-identical to what is
live — only the three `key_env:` lines come out.

```sh
sudo cp -a /etc/context-guru/upstreams.yaml /etc/context-guru/upstreams.yaml.bak-$(date +%F)
sudo sed -i '/^\s*key_env:/d' /etc/context-guru/upstreams.yaml
sudo grep -c '^\s*key_env:' /etc/context-guru/upstreams.yaml    # must print 0
```

Then update the file's header comment: it currently claims the proxy refuses to start
if a named variable is unset, which stopped being true. Leaving a stale comment on
the file that decides who pays is how this went wrong the first time.

## 3. Environment: delete a lie, add the mail path, turn on DEBUG

`TENANT_MONTHLY_CAP_USD` no longer exists in the binary — the per-tenant spend cap was
removed, because a caller spending their own credential needs no cap from us. Left in
the unit it reads as a live control that is silently doing nothing, which is worse
than absent.

```sh
sudo sed -i '/^Environment=TENANT_MONTHLY_CAP_USD=/d' \
  /etc/systemd/system/context-guru.service.d/20-local.conf
```

Add (a new drop-in, so secrets in `20-local.conf` are never reopened):

```ini
# /etc/systemd/system/context-guru.service.d/30-caller-pays.conf
[Service]
# Mail: the relay authorises by source IP, so no credential is needed. Port 25 is the
# only SMTP port open outbound from this box; 465/587/2525 are blocked.
Environment=CG_SMTP_HOST=na.relay.ibm.com
Environment=CG_SMTP_PORT=25
# MUST be a routable domain. The default (context-guru@contextguru) has no SPF and
# will be filtered, and a filtered code means nobody can complete registration.
Environment=CG_SMTP_FROM=Osher.Elhadad@ibm.com
Environment=CG_LOG_LEVEL=debug
# Fairness first: without this the byte rule deletes oldest-first GLOBALLY, so one
# heavy tenant evicts everyone's history.
Environment=DASHBOARD_MAX_ROWS_PER_TENANT=100000
```

```sh
sudo systemctl daemon-reload
```

## 4. Wipe both databases

The owner asked to start clean. Do it here, with the service down, so nothing is
half-migrated.

```sh
sudo rm -f /var/lib/context-guru/cg.db /var/lib/context-guru/cg.db-wal /var/lib/context-guru/cg.db-shm
sudo rm -f /var/lib/context-guru/cg-control.db /var/lib/context-guru/cg-control.db-wal \
           /var/lib/context-guru/cg-control.db-shm
```

Wiping the control DB is what makes the account-claim migration moot: there are no
pre-existing unverified accounts to claim and no pre-floor agent-key bindings to
invalidate. On a deployment that must keep its accounts, do **not** wipe — the
migration stamps in-use accounts as verified in the same transaction that adds the
column, which is what closes that hole.

## 5. Install and start

```sh
sudo install -m 0755 -o root -g root /tmp/cg-new /usr/local/bin/context-guru-proxy
sudo systemctl start context-guru && sleep 2
sudo systemctl is-active context-guru
journalctl -u context-guru -n 40 --no-pager | grep -iE 'hosted|mail|register|refus'
```

The startup banner must report the mail path. If it warns that nobody can sign in,
`CG_SMTP_HOST` did not take — fix it before telling anyone the service is up.

## 6. Verify, in this order

```sh
# 1. Reachable and TLS still valid
curl -sS -o /dev/null -w '%{http_code}\n' https://contextguru.vpc.cloud9.ibm.com/healthz

# 2. A request with NO provider key must be refused, not served on the operator's key.
#    This is the assertion that proves step 2 took effect.
curl -sS -o /dev/null -w '%{http_code}\n' \
  -H 'x-context-guru-token: cg_live_doesnotmatter' \
  -H 'content-type: application/json' \
  -d '{"model":"aws/claude-sonnet-5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}' \
  https://contextguru.vpc.cloud9.ibm.com/anthropic/v1/messages
# expect 401

# 3. /stats and /metrics must NOT be reachable through the front end
curl -sS -o /dev/null -w '%{http_code}\n' https://contextguru.vpc.cloud9.ibm.com/stats
# expect 403 (loopback-only, and any forwarded header disqualifies the caller)
```

Then register a real account through the browser, complete the mailed code, and run
one real agent turn. An empty dashboard after a successful-looking agent run means the
traffic never arrived — check for an `env` block in `~/.claude/settings.json`, which
silently overrides an exported `ANTHROPIC_BASE_URL`.

## 7. Rollback

Rolling back the binary alone is not enough: the old binary **refuses to start**
without a `key_env` on every upstream. Restore both together.

```sh
sudo systemctl stop context-guru
sudo cp -a /etc/context-guru/upstreams.yaml.bak-<date> /etc/context-guru/upstreams.yaml
sudo cp -a /usr/local/bin/context-guru-proxy.prev /usr/local/bin/context-guru-proxy
sudo rm -f /etc/systemd/system/context-guru.service.d/30-caller-pays.conf
sudo systemctl daemon-reload && sudo systemctl start context-guru
```

The databases are not restored by this, and do not need to be: `cg.db` is a derived
view by design, and a wiped control DB means everyone re-registers.

## What this runbook deliberately does not do

- **It does not expose Grafana.** Grafana remains on `127.0.0.1:3000`, reachable only
  through `ssh -L 3000:127.0.0.1:3000 <host>`. Publishing an admin console to all of
  `9.0.0.0/8` is a separate decision, with its own review.
- **It does not open a second port.** There is no port 80 and no direct 3000/9090/3100,
  by design: every request carries a credential in a header, and a redirect cannot
  retract a credential already sent in cleartext.
