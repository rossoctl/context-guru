# Running context-guru as a hosted service

One instance, one hostname, many users. Each user points their agent at it with a
token we mint, gets their own dashboard, and manages their own compaction config.

Hosted mode is off unless you pass `--upstreams`. Without that flag this binary
behaves exactly as it always has, so existing deployments and the benchmark
harnesses in `deploy/harbor/` are unaffected.

!!! tip "Looking for the local path? You do not need any of this."
    context-guru is still a single binary you run on your own machine:

    ```sh
    context-guru-proxy --dashboard        # :4000, dashboard at /dashboard/
    ```

    No accounts, no tenant database, no systemd, no nginx, no cold storage. Start at
    [Quickstart: proxy](get-started/quickstart-proxy.md) and the
    [Dashboard](dashboard.md); everything on this page is the *shared-box* deployment
    and is additive. The seam is one field — with no `--upstreams` the tenancy layer
    is nil and the control plane is not mounted at all (`/api/me` 404s), which is
    regression-tested rather than merely intended.

## What changes when hosted mode is on

| | Single-tenant | Hosted |
|---|---|---|
| Who may call the proxy | anyone who can reach the port | a valid `cg_live_…` token |
| Pipeline | `--config` / `--preset`, one for the process | the caller's own from the control database, or the [server default they track](#tenants-track-the-default-they-are-not-stamped-with-a-copy-of-it) |
| Compaction state store | one, shared | one per tenant |
| Upstream + credential | fixed at boot from flags/env | the tenant's chosen name from the allow-list; **the caller's own provider key forwarded** |
| Dashboard data | everything | scoped to the caller; managers may widen |
| `/stats` | open | loopback or a manager (it is a service-wide aggregate) |
| Transcript capture | operator's flag | operator's flag **and** the tenant's consent |

## Credentials, and what is never stored

No secret is written anywhere it could be read back — every value in the control
database is a digest of something, and none of them can be replayed as its input.

**User tokens.** We mint them; the database holds `sha256(token)` and the first 8
characters, and nothing else. After `Register` returns, the process no longer has the
plaintext — so there is no code path that can print one, and a dump of the control
database cannot be replayed against the proxy. Show it once, at registration.

**Provider keys belong to the caller.** Your agent sends its own key in
`Authorization` / `x-api-key` / `x-goog-api-key`, and the proxy **forwards it
unchanged** — your traffic, your provider account, your invoice. The service holds no
provider credential of its own and stores none: the key is read off the request and
dropped.

That is why identity moved to a header of its own. `x-context-guru-token` carries the
`cg_live_…` token, and `copyHeaders` strips every `x-context-guru-*` header before
forwarding, so a token cannot leave the box by construction. A token presented in an
auth slot instead is still accepted (some tools have nowhere else to put it) and is
scrubbed out on the way — recognisable because only *our* tokens are shaped
`cg_live_` + 26 characters, which no provider key is.

A caller with no credential of their own gets a **401**. It never falls back to
whatever key the box happens to hold: that fallback is the defect this design removes.

**Dashboard credentials are separate, and also only ever stored derived**: an argon2id
hash of the password, `sha256` of the session cookie, `sha256` of the 5-minute email
code, and — for an agent that can carry no header of ours — `sha256` of the provider key
it was bound with. See [the sign-in flow](#the-sign-in-flow).

**Server-held keys are an explicit opt-in, not the default.** An allow-list entry may
name a `key_env`, and then that variable's value replaces the caller's auth on
forward. That is for a gateway deployment where the agent holds only a placeholder —
eval containers, a local single-tenant proxy. Omit `key_env` (the hosted default) and
the caller's own credential goes through.

## Operator setup

### 0. Prerequisites

Everything below was derived from the scripts in `deploy/service/`, which are the authority.

| For | You need |
|---|---|
| Building the binary | Go 1.26 and a C toolchain (`CGO_ENABLED=1`) — or an already-installed `/usr/local/bin/context-guru-proxy`, which `install` keeps if there is no fresh build |
| Installing | root, and systemd. The scripts are written for **RHEL 9** (`dnf`, and nginx 1.20's config dialect) |
| The TLS front end | `nginx`, plus a certificate and key at `/etc/context-guru/tls/{fullchain,privkey}.pem` |
| Each upstream | a host to send it to. A key only if you want the server to hold one — the default forwards each caller’s own |
| Cold storage (optional) | `rclone`, and a browser somewhere for the one OAuth step |
| Grafana (optional) | `podman` or `docker` |

### 1. The upstream allow-list

Copy `deploy/service/upstreams.example.yaml` to `/etc/context-guru/upstreams.yaml`
and set the hosts. A tenant chooses an upstream by **name**; there is no way for a
tenant to supply a URL, which is what keeps the proxy from becoming an
unauthenticated forwarder to anything this host can reach.

The loader validates at boot and **refuses to start** if any named credential is
missing from the environment. That is deliberate: with no key to inject, the proxy
would forward the caller's own header — the token we minted — to a third party.

### 2. Install the service

`deploy/service/install.sh` is the whole deployment. It is idempotent — safe to re-run
after changing a credential, adding an upstream, or rebuilding the binary — and it has
eight subcommands:

| `sudo ./install.sh …` | Does |
|---|---|
| `preflight` | Says what is missing, and nothing else. Needs root. |
| `install` | User, group, directories, binary, units, drop-ins. Does **not** start. |
| `start` | `preflight`, then `enable --now` for the service and the backup timer. |
| `nginx` | Installs the TLS front end (needs nginx and a certificate present). |
| `status` | Unit state, the last 15 journal lines, the backup timer. |
| `grafana` | Prometheus + Grafana over `/metrics`, loopback only — see [Grafana](#grafana). |
| `grafana-status` | Containers, scrape health, dashboard-provisioning errors. |
| `grafana-remove` | Drops both containers, **keeps** the metrics history. |

#### From a bare RHEL 9 box

```sh
# 1. Build. The installer copies bin/context-guru-proxy; with no fresh build it keeps
#    whatever is already at /usr/local/bin, and with neither it refuses.
make build

# 2. Create the account, directories, binary, units and drop-ins.
sudo deploy/service/install.sh install

# 3. Real hosts. No key_env, so each caller's own provider key is forwarded and no
#    credential file is needed. (Gateway deployments only: add key_env: NAME, put the
#    key in /etc/context-guru/credentials/<name>, chmod 0400, and re-run install.)
sudoedit /etc/context-guru/upstreams.yaml

# 4. Site settings — MANAGER_EMAIL above all. This file, NOT the unit.
sudoedit /etc/systemd/system/context-guru.service.d/20-local.conf

# 5. Preflight, then start.
sudo deploy/service/install.sh start
```

Then the TLS front end ([2b](#2b-tls-front-end)), the email path
([3](#3-configure-the-email-path)), a registration mode
([4](#4-choose-how-accounts-are-created)), and cold storage ([5](#5-cold-storage-on-box)).
Only the email path blocks a first *account*; none of them block a first request.

#### The four things worth understanding before you run it

**The `cg` system user, and its primary group.** A system account with no home and
`/usr/sbin/nologin`: it runs one binary and owns one directory. The installer creates the
**group first, explicitly**, because `useradd --system` without `--gid` falls back to the
distribution default — on RHEL that is `users` — and then every `install -g cg` in the
script fails with "invalid group" and aborts a half-finished install. Owning a dedicated
group is also what makes `0700` on `/var/lib/context-guru` mean *only this service*
rather than *anyone in `users`*. An account created before this script existed is moved
onto the right primary group by `install`, and `preflight` fails if it is wrong.

**Upstreams are named, never supplied.** `/etc/context-guru/upstreams.yaml` is the
operator's allow-list, and a tenant picks an entry by **name**. There is no field
anywhere that accepts a URL from a user, which is the difference between a proxy and an
unauthenticated forwarder to anything this host's network can reach. The example ships
`REPLACE-WITH-…` placeholder hosts and `preflight` fails while they are still there —
starting with them means every request 502s.

**Server-held keys, when you want them, arrive by `LoadCredential`.** The default needs
none — the caller's own provider key is forwarded — but an allow-list entry may name a
`key_env`, and then the value lives in
`/etc/context-guru/credentials/<entry>` (root-owned, `0700` directory, `0400` files) and
systemd hands it to the process as a credential rather than an `Environment=` line —
which `systemctl show` would print to anyone who can read the unit. `install` **generates**
`10-credentials.conf` from whatever is actually in that directory, because a hand-written
`LoadCredential` naming a file that does not exist fails the whole unit with an error that
does not say which file. So adding a server key is: drop it in, re-run `install`.
Both spellings of a name work (`ibm-litellm` and `ibm_litellm`), and preflight accepts
either.

**`20-local.conf` is created once and never overwritten.** Site settings — `MANAGER_EMAIL`,
`REGISTER_DOMAINS`, `LISTEN_ADDR`, the per-tenant limits, `METRICS_TOKEN` — live in
`/etc/systemd/system/context-guru.service.d/20-local.conf`. The shipped unit is
**replaced on every install** so a fix to it actually lands, which is exactly why nothing
a human edits may live there: settings like `MANAGER_EMAIL` used to be edited in the unit,
and since installing is idempotent and gets re-run to regenerate the credential drop-in,
every re-run silently reverted the operator's edits. The drop-in is the fix. After editing
it: `sudo systemctl daemon-reload && sudo systemctl restart context-guru`.

#### Preflight is a gate, not a suggestion

`preflight` checks the user and its group, the binary, the start wrapper, the state
directory, the allow-list and its placeholders, a credential file for every `key_env`
named, the credential drop-in, the *effective* `MANAGER_EMAIL` (from the unit or any
drop-in — checking only one is how "I set it and it still says empty" happens), and the
rclone config.

`start` runs it first and **refuses to start on any failure**. That is deliberate:
`Restart=always` plus a missing prerequisite is a crash loop that respawns every two
seconds and buries the real cause under a hundred identical journal entries. Do not
`systemctl enable --now` by hand to get around it.

It needs root, and not for ceremony: the credentials directory is `0700 root` and the
state directory is `0700 cg`, so an unprivileged run cannot stat either and reports both
as missing — a false negative in a diagnostic sends someone chasing a credential they
already installed.

### 2b. TLS front end

```
sudo dnf -y install nginx
sudo deploy/service/install.sh nginx
```

Needs a certificate at `/etc/context-guru/tls/{fullchain,privkey}.pem`; the installer
says so rather than letting nginx fail to start. Until TLS is up, leave `LISTEN_ADDR` on
loopback — every user's token crosses the network on every request, so an unencrypted
LAN listener is not an option for a service holding other people's credentials.

Two nginx settings are load-bearing rather than stylistic, and getting either wrong looks
like context-guru being broken: `proxy_buffering off` (the proxy streams SSE both to
agents and to the dashboard) and `proxy_read_timeout 600s` (agent turns and the live feed
both outlive the 60-second default).

`install.sh nginx` checks the certificate rather than only that two files exist: that it
parses and has not expired, that it **matches the private key**, that it is valid for more
than 21 days, and that `fullchain.pem` holds **at least two certificates**.

!!! warning "`fullchain.pem` must be leaf **then** intermediate"
    A `fullchain.pem` containing only the leaf is the classic failure, and it is miserable to
    debug from the client side: a browser that cached the intermediate from another site
    verifies fine, while **every agent fails**, because a client trusting only the root has
    no way to bridge the gap. `install.sh nginx` says so explicitly, and the fix is a
    concatenation:

    ```sh
    cat leaf.pem intermediate.pem > /etc/context-guru/tls/fullchain.pem
    ```

There is deliberately **no port-80 server block**, and the installer reports it if anything
else starts listening there — a distro default server can come back with a package upgrade.
Every request carries a `cg_live_` token in a header, so a `301` to `https://` would arrive
after the credential was already sent in cleartext.

### 2c. Prove the public TLS path

[`deploy/service/tls-smoke.sh`](https://github.com/rossoctl/context-guru/blob/main/deploy/service/tls-smoke.sh)
checks the deployment from a *client's* point of view, through `https://$CG_HOST` and never
through `127.0.0.1:4000` — because every failure it exists to catch only appears once nginx
is in front.

```sh
CG_TOKEN=cg_live_… deploy/service/tls-smoke.sh      # authenticated checks included
deploy/service/tls-smoke.sh                          # without a token: skips those, runs the rest
```

`CG_HOST` defaults to `contextguru.vpc.cloud9.ibm.com` and `CG_CA` to
`/etc/context-guru/ibm-internal-root-ca.pem`; override either from the environment. Passing
the root explicitly is the point rather than a detail — it proves a client trusting **only**
that root can verify the server, which is the situation every IBM laptop is in. The CA copy
lives beside the config and not in `$ETC/tls/`, which is `0700 root`: correct for a private
key, wrong for a public root certificate that ops scripts and unprivileged clients need to
read. (Putting it there silently downgraded the script to the system trust store and turned
every check into a certificate error.)

What it asserts, and why each one is a real failure someone has shipped:

| Check | A failure means |
|---|---|
| chain verifies to the root, `depth=1` present | the intermediate is not being served — works in a browser, fails for every agent |
| certificate valid > 21 days | not enough warning to get a reissue through the normal process |
| a wrong hostname is **rejected** | the certificate is a wildcard, or the client is not checking names |
| `/healthz` 200 over TLS | the front end is not reaching the proxy |
| port 80 **refuses** connections | a cleartext request would leak the token before any redirect |
| `/stats` and `/metrics` are `403` **through nginx**, `200` on loopback | a service-wide, every-tenant aggregate is public; the loopback gate stopped meaning anything once the peer became the reverse proxy |
| `/api/stats`, `/api/requests`, `/api/sessions` are `401` without a token | per-tenant data is public |
| the `cg_dash` cookie carries `Secure` and `HttpOnly` | the flag comes from `X-Forwarded-Proto`, so this is the check TLS termination can break |
| `/api/events` produces bytes within 24 s | `proxy_buffering` is on, and the dashboard looks simply dead |

The SSE check waits past the 20-second keepalive on purpose: on an idle service the first
bytes are the server's own `: keepalive` comment, and its arrival *is* the proof — it
originated server-side and reached the client unbuffered.

!!! note "The installer does not place the root CA file"
    `install.sh` creates `$ETC/tls/` and installs the nginx config, but nothing in it writes
    `/etc/context-guru/ibm-internal-root-ca.pem`. Put it there by hand (readable, not `0700`)
    if you want `tls-smoke.sh` to test against the root rather than falling back to the
    system trust store — it says which one it used.

### 3. Configure the email path

**Nothing about accounts works until this is set.** Registration mails a 6-digit code to
prove the address is real, and signing in mails one as a second factor. With no mail path
`POST /api/register` answers `503` naming the missing variable, and the startup banner
warns. That is deliberate: a registration whose code went nowhere is worse than one that
refuses.

All of it is read from the environment *per send*, so pointing at a different relay needs
no restart.

| Variable | Meaning |
|---|---|
| `CG_SMTP_HOST` | Relay hostname. **Required.** Empty means no mail path. |
| `CG_SMTP_PORT` | Default `25`. |
| `CG_SMTP_FROM` | Envelope and `From:` address. Default `context-guru@<hostname>`. |
| `CG_SMTP_HELO` | EHLO name. Default the local hostname. |
| `CG_SMTP_USER` / `CG_SMTP_PASSWORD` | SMTP AUTH, if the relay wants it. Usually empty inside a corporate network, where the relay authorises by source IP. Read at send time, never stored. |
| `CG_SMTP_INSECURE=1` | Skip STARTTLS certificate verification. For a relay with an internal-CA or mismatched certificate **only**. |
| `CG_MAIL_DEV_SINK` | Development escape hatch — see the warning below. |

STARTTLS is used whenever the relay advertises it, and a failure to negotiate **aborts the
send** rather than continuing in the clear: a code that travels plaintext because a
certificate expired is exactly the silent downgrade that makes "we use TLS" untrue.

On the IBM internal network `na.relay.ibm.com:25` accepts mail from this host, advertises
STARTTLS, verifies against the public trust store, and needs no AUTH:

```
Environment=CG_SMTP_HOST=na.relay.ibm.com
Environment=CG_SMTP_FROM=context-guru@<this-host>.fyre.ibm.com
```

Egress on **465 and 587 is blocked** from this network; only 25 to the internal relay is
open. Do not configure a public provider — it will time out.

!!! danger "`CG_MAIL_DEV_SINK` must never be set on a real deployment"
    `CG_MAIL_DEV_SINK=log` writes the six digits to the **server log** at INFO, so anyone
    who can read the log can complete anyone's sign-in. Any other value is treated as a
    file path, appended to with mode `0600`. It applies only when `CG_SMTP_HOST` is empty,
    and every send through it logs a warning saying what it just did. It exists so a
    deployment with no relay can still create its first account.

### 4. Choose how accounts are created

**Self-registration is open by default**: anyone whose address passes `--register-domains`
can create an account, once they enter the code mailed to it. Two things make that safe
enough to be the default — the address is now *proven* rather than claimed, and an account
no longer spends the operator's money (each user forwards their own provider key).

`CG_REGISTER` is read per request, so switching modes needs no restart.

| `CG_REGISTER` | `POST /api/register` |
|---|---|
| unset / `open` | **Default.** Self-service, capped at 3 attempts per minute per client address, plus mandatory email verification. |
| `invite` | Additionally requires an exact `code` in the request body, compared against `CG_REGISTER_CODE` in constant time. With no code configured it refuses rather than falling through to open. |
| `closed` | Refused with `403`. Nothing can create an account — see the bootstrap note below. |

Anything the resolver does not recognise — a typo, `Open`, `" open"` — normalises to
**open**, the default, like every other setting in this project. `closed` and `invite` are
the deliberate departures, so a typo cannot silently disable accounts. The startup banner
and `/api/whoami` both report the mode through the same resolver the gate uses, so a log
line cannot disagree with what is enforced.

#### The sign-in flow

Two phases, and phase one issues **no cookie** — knowing a password is never by itself a
signed-in browser.

| Step | Route | Result |
|---|---|---|
| Register | `POST /api/register` `{email, password}` | account created **unverified**, no token, code mailed. The reply carries `code_expires_at` so the UI can count down against the server's clock. |
| Confirm | `POST /api/verify` `{email, code}` | verified, first `cg_live_` token returned **once**, session opened. |
| Sign in | `POST /api/login` `{email, password}` | password checked, fresh code mailed. |
| Second factor | `POST /api/verify` `{email, code}` | session opened. |

`/api/verify` decides which flow it is from the pending code's own *purpose*, never from
anything the client says — otherwise a login code could be spent on the branch that mints
a token.

- **Codes** are 6 digits, valid **5 minutes**, one-time, and stored only as a hash. A new
  code replaces the pending one rather than adding to it.
- **Passwords** are stored as **argon2id** — 64 MiB, 3 passes, 2 lanes, a fresh 16-byte
  random salt per account — in PHC string format, so the parameters can be raised later
  without invalidating a single existing password. Minimum 8 characters, no composition
  rules: length is what buys entropy.
- **Brute force.** A code tolerates **5 wrong guesses**, after which it is destroyed rather
  than merely refused. On top of that, password attempts are capped at 5/minute and code
  submissions at 10/minute, charged against **both** the email address and the client
  address — either bucket alone is trivially sidestepped (per-email only lets one host
  grind every account; per-IP only lets a botnet grind one account). Deliberately a rolling
  window rather than a sticky "locked for 30 minutes", which anyone could aim at a
  colleague by typing their address.
- **Proxy tokens are unchanged**, and are still how agents authenticate to the proxy. They
  are a separate credential: a token cannot sign in to the dashboard once the account has a
  password, and a dashboard session cannot send traffic. An account created before
  passwords existed still signs in with its token, because it has nothing else.
- **Several machines at once** is the expected state. Each session records a label,
  User-Agent, address, and last-seen time; Settings → *Signed-in machines* lists them and
  revokes them one at a time (`GET`/`DELETE /api/me/sessions/{id}`).

#### Bootstrapping when you start `closed`

!!! warning "`POST /api/register` is the only route that creates an account"
    There is no manager-side create. A manager can reissue a token for a tenant that
    **already exists**, set its row quota, or disable an account — but cannot bring one
    into being. So a control database on `closed` has **no path to a first account at all**.

`MANAGER_EMAIL` has to be set *before* the manager registers: the role is assigned at
registration by comparing the address, which is how the first manager exists at all without
an interactive bootstrap step to forget and then work around. If you have set
`CG_REGISTER=closed`, open `invite` briefly, register, and close it again — no restart
needed, the variable is re-read per request.

#### How the `open` limit is keyed

Three attempts per minute, and the bucket is not the raw `RemoteAddr`:

- **IPv6 is keyed on the `/64` prefix.** Per-address limiting is meaningless against
  IPv6 — the smallest allocation anyone gets is a `/64`, so 2^64 source addresses means
  2^64 free buckets. One allocation, one budget. IPv4 is keyed on the exact address.
- **`X-Forwarded-For` is trusted from a loopback peer only.** A remote caller cannot make
  `RemoteAddr` loopback, so for it the header is ignored entirely and the bucket is its own
  address — honouring a remote client's header would hand it a fresh bucket per request. A
  loopback peer *is* the reverse proxy on this host, and ignoring the header there put every
  client of the real deployment into one bucket: a registration denial-of-service for
  legitimate users at 3/min service-wide, and no per-attacker control at all. nginx uses
  `$proxy_add_x_forwarded_for`, which appends the peer it saw, so the **last** element is
  the one our own proxy wrote and the earlier ones are client-supplied noise.

Residual: a process on this host can forge the header and get unlimited buckets. Anything
with local access can already read the control database, so that is not the boundary this
defends.

`--register-domains ibm.com` narrows *which* addresses may register, in any mode. It is an
**exact-domain-or-subdomain** match on the part after the `@` (`ibm.com` and
`x.ibm.com` pass; `notibm.com` does not).

**What these modes do not do.** The address is now proven — a code has to arrive in that
mailbox — but reachability is not entitlement. Anyone with a real mailbox in an allowed
domain can register, and can register more than once with more than one address; the rate
limit only slows a single source. What that costs is not the operator's money — every
account spends its own provider credential — but accounts on your box, consuming its CPU
and its row quota (`--dashboard-max-rows-per-tenant`). An invite code is a shared secret
with no per-use accounting: once it leaks it is `open` until you rotate it. A port the whole
internet can reach wants `invite`.

### 5. Cold storage on Box

```
deploy/service/box-setup.sh check     # what is installed and configured
deploy/service/box-setup.sh install   # rclone (falls back to a user-local install)
deploy/service/box-setup.sh token     # prints the browser step, which cannot be automated
deploy/service/box-setup.sh paste     # write a token obtained elsewhere, from stdin
deploy/service/box-setup.sh verify    # write, read back, compare, delete
```

The OAuth step needs a browser and this host is headless, so `token` prints two exact
routes: authorize on your own machine with `rclone authorize "box" --client-id ""
--client-secret ""` and paste the JSON here, or forward port 53682 and let the server
run the flow. Everything else the script does itself.

**Run `box-setup.sh` as your own user, before the installer.** It writes
`~/.config/rclone/rclone.conf`, and `install.sh install` copies that file to
`/var/lib/context-guru/rclone.conf` (mode `0600`, owned by `cg`) — so the order is
box-setup, then install. An existing `/var/lib/context-guru/rclone.conf` is left alone;
set `RCLONE_SRC=` to copy from somewhere else. Without it, `preflight` warns rather than
fails, and the service runs with cold storage disabled — which means eviction **deletes**
instead of archiving.

The nightly control-database backup needs no separate step: `install` lays down
`context-guru-backup.{service,timer}` and its script, and `start` enables the timer
alongside the service. `install.sh status` shows when it last ran and when it runs next.

## Deploying somewhere that is not IBM

The procedure above is the whole deployment anywhere; four things are IBM-specific, and each
one is a decision rather than a line to copy.

**The certificate authority.** The IBM deployment's certificate comes from the IBM Internal
Intermediate CA, which every IBM-managed machine already trusts — so for its users there is
nothing to install. Off that network you have three options, and only one of them is not a
support burden: a **publicly trusted** certificate (Let's Encrypt or an ACME CA, with renewal
automated), your **own internal CA** (every client machine, container and CI runner then needs
that root in its trust store, which is the failure users hit first), or a **self-signed**
certificate, which `install.sh nginx` will print a one-liner for and which is a test
convenience, not a deployment. Whatever you pick, `fullchain.pem` must still carry the
intermediate, and `tls-smoke.sh` with `CG_HOST` and `CG_CA` set is still the check.

**The firewall exception.** The IBM deployment is reachable from `9.0.0.0/8` only, with an
inbound exception for **TCP/443 and nothing else**. Port 80 is not requested and
[not served](#2b-tls-front-end). Reproduce both properties: a public deployment that opens 80
"for the redirect" leaks a token per mistyped URL. Keep `LISTEN_ADDR` on loopback so the only
way in is through the TLS front end.

**Registration policy.** `REGISTER_DOMAINS=ibm.com` in the shipped drop-in is what makes
`open` mode tolerable there: an attacker needs a real, reachable mailbox in a domain you
control, since [registration mails a code](#3-configure-the-email-path) to it. With no
domain restriction, `open` means anyone with any working mailbox can mint an account on
your box, and the only brakes are the mailed code and 3 attempts/minute per client
address. A publicly reachable deployment wants `invite` with a code you rotate, or
`closed` with accounts you create yourself.

**No spend caps, and none needed.** Every tenant's traffic is billed to the credential
that tenant's agent sends, so there is no shared budget to guard. Month-to-date cost is
still computed and shown, per tenant, on Settings and in the manager's roster — it needs
`MODEL_INFO` on to be non-zero, because an unpriced row costs $0.00.

Two things that are *not* IBM-specific and should not be relaxed: cold storage on Box is one
rclone remote name away from being any other remote, and `/metrics` plus Grafana binding
loopback-only is about cross-tenant spend data, not about IBM.

## User setup

!!! tip "Are you a user of the IBM deployment, not its operator?"
    [Connect to the IBM service](get-started/connect-ibm-service.md) is the five-minute
    version of this section: register, trust the CA, point one agent at it, and turn it on and
    off per session.

**Keep your own provider key where it already is.** The proxy forwards it, so your
traffic is billed to you. What you add is a base URL and the context-guru token, and the
token goes in `x-context-guru-token` — never in the slot your key occupies.

```bash
# Claude Code — your own key stays in ANTHROPIC_API_KEY (or ANTHROPIC_AUTH_TOKEN)
export ANTHROPIC_BASE_URL=https://cg.<host>/anthropic
export ANTHROPIC_CUSTOM_HEADERS="x-context-guru-token: cg_live_xxxxxxxx"

# OpenAI-dialect tools — your own key stays in OPENAI_API_KEY; send the header
#   x-context-guru-token: cg_live_xxxxxxxx
export OPENAI_BASE_URL=https://cg.<host>/openai/v1

# Bob — its client sets every header itself and cannot carry ours, so bind the key
# it already sends (stored as sha256 only, never in plaintext). One-time, from the
# Setup tab, with your dashboard cookie:
export CUSTOM_BASE_URL=https://cg.<host>
curl -sS -XPOST https://cg.<host>/api/me/agent-key \
  -H "Authorization: Bearer $BOBSHELL_API_KEY" -b "cg_dash=<your dashboard cookie>"
```

### How each agent identifies itself

| Agent | Mechanism | Why |
|---|---|---|
| Claude Code | `ANTHROPIC_CUSTOM_HEADERS="x-context-guru-token: …"` (or the same pair in `~/.claude/settings.json` `env`) | Documented `Name: Value`, newline-separated for several. Leaves `ANTHROPIC_API_KEY` / `ANTHROPIC_AUTH_TOKEN` free for your own key. |
| OpenAI-dialect tools | `x-context-guru-token` header, however your tool sets extra headers | Same slot rule; `OPENAI_API_KEY` stays yours. |
| Bob (BobShell) | `sha256(BOBSHELL_API_KEY)`, bound once via `POST /api/me/agent-key` | Bob's client builds `Content-Type`, `User-Agent`, `Authorization`, `x-instance-id` and `x-team-id` itself and exposes no hook for another header; its `headers` setting applies to MCP servers only. So it is recognised by the credential it already sends. |

**Binding a key: two refusals.** A key under 20 characters is refused `400` — identity here
*is* the key's digest, so a guessable key would be a guessable account. A digest already
bound to another account is refused `403` and never moved: binding a digest someone else had
bound used to transfer their traffic, and with `capture_content` on their captured
transcripts, to whoever bound it. Its owner unbinds first, then the new holder binds.

**The floor is enforced on bind and deliberately not on resolve**, which matters when
upgrading an install that already holds bindings. A stored row is a digest with no length
attached and no plaintext behind it, so a short key bound before the floor existed cannot be
audited — nothing can find it or measure it — and it keeps identifying its account. Enforcing
the floor at resolve time instead would be worse than leaving it: the check would have to run
against every arriving key, and the first short one would stop identifying an account that had
been working, breaking a tenant's agent to fix a weakness in *their* choice of key with no
warning and no way for them to see why. So the asymmetry is the intended trade — new bindings
are held to the floor, existing ones keep working. The one-way consequence is worth knowing
before you reach for it: unbinding is the only way to clear such a row, and once cleared the
same short key cannot be bound again, because a fresh bind now hits the `400`. On a database
with no bindings yet, none of this arises.

Agent *family* (claude-code / bob / codex / …) is read from the `User-Agent`, which none
of this touches.

The path carries the dialect; the tenant's configuration carries which upstream each
dialect goes to. So switching between Claude Code and Bob needs no per-agent choice
of gateway, and the same token works in all of them.

!!! warning "Verify the traffic actually arrives — an `export` is not proof"
    For Claude Code, an `env` block in `~/.claude/settings.json` **overrides the variable
    you exported**, and the override is silent: Claude Code answers normally, nothing
    errors, and the only symptom is an empty dashboard and no savings, because nothing
    ever reached the service. This has already caught us once on this very deployment.

    After setting the variables, confirm your own dashboard's request count moves. If it
    stays at zero, read
    [Use with Claude Code](how-to/use-with-claude-code.md#steps) — it has the
    two-command diagnosis and both fixes.

**Escape hatches**, worth knowing before you need them: unset the base URL to bypass
the proxy entirely, or send `x-context-guru-bypass: true` to keep the metrics without
the compaction.

## Default configuration

Every new tenant starts on `codesmart` minus the LLM extractor (and minus `codesafe`'s
blind `collapse`):

```yaml
pipeline: [format, toon, dedup, failed_run, cmdfilter, extract, cachesplit]
components:
  extract:
    min_tokens: 400
mode: sync
```

Fully deterministic, which is the property that matters on a shared box: no
cheap-model calls, so it adds no upstream spend, contends for no shared LLM budget,
and puts near-zero latency on anyone else's agent turn.

### Tenants TRACK the default; they are not stamped with a copy of it

`Register` does not copy the default into the new tenant's row. **An empty stored
configuration means "follow the server default"**, and it is resolved live on every request
by the same `Registry.Config` the settings page reads — so when the operator improves the
default, every tracking account is running the new one on its next turn.

Tracking is not a flag. It is the **absence of a stored document**, which makes both
transitions one write and keeps the settings page and the proxy from ever disagreeing about
who is following what. The account view (`/api/me`, `/api/whoami`, `/api/tenants`, …) carries
three fields — see
[the tenant view's configuration fields](reference/routes.md#the-tenant-views-configuration-fields)
for the exact shapes:

| Field | Tracking the default | Own configuration |
|---|---|---|
| `config_yaml` — what is **stored** | `""` | the tenant's document |
| `effective_config_yaml` — what their traffic **runs** | the server default | the same document |
| `config_inherited` | `true` | `false` |

`config_yaml` **changed meaning** with this: it is now the stored value, not the resolved
one. It is also the only field a settings save writes back, so a round trip through the form
cannot silently turn tracking into a frozen copy of today's default.

**What Settings shows in each state.** The controls always render the *effective* document,
because drawing the empty stored one would read as "my configuration is gone":

| State | Settings shows |
|---|---|
| Tracking | *Following the server default.* The pipeline, mode and YAML are shown **read-only**, labelled as the operator's, with a note that they change when the operator changes the default. One button: **Customise**. |
| Own configuration | *Using your own configuration*, plus the warning that changes to the server default do not reach you — and, when the stored document is byte-identical to the current default, that it is identical. Controls editable. One button: **Follow the server default** (confirmed, audited). |

Moving between them: **Customise** stores the current effective document as your own, so
nothing changes at the moment you take ownership; **Follow the server default** clears the
stored document. Saving upstreams or capture consent while tracking leaves the configuration
alone, deliberately.

!!! warning "Customising opts you out of improvements to the default"
    A tenant with a stored configuration **stops receiving changes to the server default**
    until they choose to follow it again. That is the trade, and it is the reason the old
    behaviour was a bug rather than a preference: `Register` used to stamp every new row with
    a copy of the default, which froze every account on the default *as it existed on its
    registration day* — adding a component to the default reached nobody who had already
    registered, and there was no way to ask for "just follow it".

    Rows the old `Register` stamped are **not migrated**, deliberately. Clearing a stored
    config safely would mean recognising a byte-identical *previous* default, and none were
    ever recorded — no constant, no schema row. A guess that misses does nothing; a guess
    that hits deletes a configuration somebody chose. Those accounts keep exactly what they
    have, and **Follow the server default** is the one-click, audited way out.

A tenant can enable `extract_llm` on their settings page. It is a real tradeoff, not
a free upgrade — measured at +117 ms per request, up to ~945 ms when file reads are
frequent — and its model calls go out on **that tenant's own credential**, the same one
the request carries. With no caller credential available the component skips (fail
open); it never borrows the server's.

## Managing users

A manager sees every account's metrics and configuration, and no account's transcripts.
That split is the whole shape of this section: everything below is available to a manager,
and reading somebody's captured source code is not.

The Tenants tab is the console. Per account it offers the full configuration (pipeline,
per-component settings, mode, upstreams, row quota, capture consent, role), an A/B variant
label, disable/enable with a reason, a reissued token, a password reset it cannot complete,
and — behind a disclosure — purge and delete.

### Editing somebody else's configuration

`PATCH /api/tenants/{id}` writes the same fields a tenant can write for themselves, plus
the manager-only ones (`role`, `max_rows`, `disabled`, `disabled_reason`, `variant`).
`config_yaml` goes through the **same strict loader the proxy builds with**, so a typo is a
`400` naming the offending key rather than a surprise at request time, and a rejected
document leaves the stored one untouched.

Two behaviours to know:

- **Saving the form does not end an account's tracking of the server default.** While
  `config_inherited` is true the editor omits `config_yaml` unless the YAML was actually
  edited — otherwise every save would freeze a copy of today's default onto an account that
  had asked to follow it.
- **Saving rebuilds that tenant's pipeline**, which discards their frozen compaction
  decisions. Their next turn is not cache-warm. That cost is real and it is theirs, so it is
  stated on the form.

Config resolution still fails **open**: an account whose stored document somehow does not
build is forwarded uncompacted (logged loudly), never refused.

### A/B testing

A **variant** is a name a manager puts on a set of accounts. It selects nothing and changes
nothing on the request path — the configuration each account runs is still the one on its own
row — so assigning one can never break an agent. What it buys is a dimension to group by:
`GET /api/variants` folds each account's existing aggregates into one row per variant, with
the per-component acted/reverted/saved breakdown underneath.

The smallest thing that works, deliberately. There is **no variant column on the metrics
table**: adding one would mean a schema bump, and in this project that renames the whole
metrics database aside and starts fresh — a label a manager can change at any time has no
business costing anyone their history. The fold is a sum of sums.

!!! warning "What the comparison cannot show, and why there is no p-value"
    The panel reports **directional** figures with their denominators, and refuses to go
    further. There is no significance test because the inputs do not support one:

    - **Assignment is not randomised.** A manager chose who is in each variant, so a
      difference may be a difference between the *people*.
    - **Workloads are not held constant** — agent, model, task mix, and how much anyone
      worked this week.
    - **Per-request cost is not per-task cost.** An earlier study in this repo was misled
      exactly here: two arms with the same reward differed in **step count**, so the arm with
      cheaper requests spent more overall. Nothing in the dashboard can see task outcomes.
    - **`incomplete_rows`** counts requests the provider gave no usage for. Where that
      approaches the request count, that row's money figures are *unknown*, not low.
    - **`saved_usd` is a counterfactual** — what the same traffic was priced at uncompacted,
      minus what it actually cost including context-guru's own model spend.
    - **The variant is read as it is today** and applied to the whole window, so moving an
      account between variants retro-labels its past traffic. The audit log records when that
      happened; the panel cannot.

    Every one of those is served with the data (`caveats` in the payload) rather than kept in
    this document, so the numbers and their limits cannot be quoted apart.

### Disable, with a reason

`disabled` refuses that account everywhere: `403` on the request path, `403` at sign-in, and
its dashboard sessions are ended. `disabled_reason` is the manager's note, and it is
**returned to the account's owner** in both refusals — without it, "disabled" is
indistinguishable from the proxy being broken to the person whose work just stopped.
Re-enabling clears the note, because a stale reason on a live account is the next thing
somebody acts on.

### Purge and delete, across both databases

The control database and the metrics database are **separate files with no foreign key
between them**, so deleting an account row does not touch that tenant's traffic. Both
operations therefore run in a deliberate order:

| Step | Purge | Delete |
|---|---|---|
| 1. Cold-storage objects for their archived sessions | deleted | deleted |
| 2. `archived_sessions` index rows | deleted, **only for objects that actually went** | same |
| 3. `request_content`, `request_components`, `requests`, `tenant_spend` | deleted | same |
| 4. The account row, its tokens, sessions, agent keys, pending codes | **kept** | deleted (cascade) |
| 5. A second sweep of the metrics database | — | yes |

Why in that order:

- **Objects before the index.** `archived_sessions` is the only record of what is in the
  bucket, so removing it first would orphan the objects permanently — still costing storage,
  no longer findable. An index row whose object could not be deleted is **kept**, and the
  call answers `502` with the paths, so a retry can find them.
- **Data before the account.** A failure is then a `502` with the account intact and the
  operation retryable. Deleting the account first would leave rows owned by an id nothing
  answers to: invisible in every view and unreachable by any retry.
- **The second sweep** exists because capture is asynchronous (a 250 ms flush). A request in
  flight during step 3 can land moments later; after step 4 nothing can authenticate as that
  tenant, so that pass is final.
- Child rows are deleted **explicitly** as well as by `ON DELETE CASCADE`, because whether
  orphans are left should not depend on a per-connection pragma.

Both require the tenant's **email or id typed back** (`{"confirm": "…"}`); the UI keeps the
buttons inert until it matches, and both write an audit row. The audit trail of a deletion
**outlives the account** — `tenant_config_audit` deliberately has no foreign key on its
target.

### Passwords: what a manager can and cannot do

| Action | Who | Effect |
|---|---|---|
| `POST /api/me/password` | the account's owner | Requires the **current** password even though the caller holds a session, because a stolen cookie must not become permanent ownership. Signs out every *other* machine. |
| `POST /api/password-reset` → `/verify` | anyone with the mailbox | Self-service recovery. Phase one answers identically for an unknown address; phase two spends the code plus the new password and ends every session. Opens **no** session: signing in still wants the password and a fresh code. |
| `POST /api/tenants/{id}/password-reset` | a manager | Mails **that account** a code. The manager never sees it and cannot set a password; the old password keeps working until its owner finishes. Audited. |

A manager who could set a password could sign in as that user and read their transcripts,
which is the one boundary this service promises its users. So the recovery path a manager
gets is "start it", never "do it".

## Storage

Three tiers, and the split is the design.

**`cg-control.db` — local, permanent, backed up.** Tenants, tokens, per-tenant
configuration, the audit log. Real forward migrations (`PRAGMA user_version`), never
rebuilt, never evicted. It stays a few MB and is the only file whose loss is
unrecoverable, so it is the only file that gets a nightly backup.

**`cg.db` — local, hot, derived.** The request metrics the dashboard queries. A row is
well under a kilobyte, so millions of requests fit comfortably; it is rebuilt on a
schema change and archived under pressure. Kept local because it must be fast and
transactional.

**Box — cold, effectively unlimited.** Whole sessions as single gzipped JSONL objects,
written once and read rarely.

```
archive/<tenant>/<yyyy>/<mm>/<session>.full.jsonl.gz      whole session
archive/<tenant>/<yyyy>/<mm>/<session>.content.jsonl.gz   transcripts only
backup/cg-control-<stamp>.db.gz                           nightly control snapshot
```

### Why rclone as a subprocess and not a mount

The obvious move is `rclone mount box: ~/mnt/box`, point `DASHBOARD_DB` at it, and get
unlimited space for free. Do not do this, and the code deliberately makes it awkward
to.

SQLite requires POSIX byte-range locking and an `fsync` that means something. A
FUSE-over-HTTPS layer provides neither, and rclone's own `mount` documentation
excludes database files for that reason. The failure mode is not slow queries, it is
silent corruption. Separately, every page read would become an HTTPS round trip, so a
dashboard query touching a few thousand pages would take minutes, and SQLite's WAL
checkpointing would run straight into Box's API rate limits.

So `dash.Rclone` shells out to `rclone rcat` / `cat` / `lsjson` / `deletefile` for
whole objects. No FUSE, no mount unit, no `fuse` package, one object per session rather
than per request, and rclone owns the OAuth token — including refreshing it — so this
process never holds a Box credential.

### Eviction is migration, not deletion

This is what makes the LRU stop being destructive. With `ARCHIVE_REMOTE` set, every
path that used to delete a session now uploads it first:

1. Export the session to gzipped JSONL.
2. Upload it.
3. **Stat it and compare the size.**
4. Only then delete the local rows.

Step 3 is not ceremony. A `Put` that returns success is not proof: a truncated upload,
a proxy that swallowed the body, a remote that accepted and dropped it all look like
success from the writer's side. A failed or short upload leaves the local copy exactly
where it was, and the next pass retries — so the worst case is that disk was not
reclaimed, never that history is gone.

Three triggers, in order:

| Trigger | Default | What moves |
|---|---|---|
| `--archive-content-after` | 24h idle | transcripts only; metrics stay locally queryable |
| `--archive-session-after` | 30d idle | the whole session |
| disk watermark | 0.90 → 0.85 | oldest whole sessions, as a backstop |

Content goes first because it is where the bytes are — capped at 16 KB per message
across many messages per request, against well under a kilobyte of metrics. Moving it
early is what keeps `cg.db` small enough that **the disk rule never fires at all**,
which is the point: the watermark is now a backstop for a Box outage, not the normal
mechanism.

The archiver runs on **its own goroutine**, never the writer's. An rclone round trip
takes seconds, and the writer owes the request path a fast insert — a blocked writer
means a full queue means dropped events, which is observability failing exactly when
the system is busy.

### When Box is down

Archiving fails soft: the local copy stays, a warning is logged, the next pass retries.

If the remote is down *and* the disk fills, there is a genuine conflict, and the
default resolves it toward keeping the service up: the session is deleted and an
`ERROR` is logged saying plainly that the history is lost. A full filesystem takes down
every user's agent, which is worse than losing old metrics.

`--archive-required` inverts that — nothing is deleted without a confirmed archive, and
the filesystem is allowed to fill instead. Pick it deliberately.

### Browsing archived history

The archive **index is local and permanent** (`archived_sessions`, one small row per
session). So the Sessions view lists a user's whole history, archived parts included,
instantly and even while Box is unreachable. Only opening a specific archived session
costs a round trip.

`GET /api/archive` lists the index; `GET /api/archive/{session}` fetches one back.
Fetching is **read-only** — it does not reinsert the rows, because dragging a session
back into the hot tier would re-trigger the eviction that put it there. A request whose
transcripts are archived reports `content_archived: true` and fetches them inline for
that one request, under a timeout.

An unreachable remote returns `503`, distinct from `404` for "never archived". Those
mean very different things to whoever is looking, and conflating them makes a Box
outage look like data loss.

### Restoring the control database

```
systemctl stop context-guru
rclone cat box:context-guru/backup/cg-control-<stamp>.db.gz \
  | gunzip > /var/lib/context-guru/cg-control.db
chown cg:cg /var/lib/context-guru/cg-control.db
systemctl start context-guru
```

The backup uses `sqlite3 .backup`, not `cp`: copying a live SQLite file while the
service writes to it yields a torn snapshot that may not open at all. It also opens the
snapshot and counts the tenants before uploading, because a backup verified only at
restore time is a backup nobody has verified.

## Grafana

The proxy exposes Prometheus text at `/metrics`. No client library is involved — see
`proxy/promexport.go` for why a dependency tree was not worth taking to serialise a few
dozen series we already compute.

!!! warning "`cg_*` will not equal the dashboard, and that is correct"
    Two families are served, and a panel beside the dashboard invites exactly the wrong
    comparison. **`cg_*`** is the in-memory aggregator — the same snapshot `/stats` reports —
    **counted in this process since it started** and **summed over every tenant**, so it
    restarts from 0 with the process (`rate()` handles that). **`cg_tenant_*`** is
    database-backed and **tenant-scoped**, cached for just under a scrape interval.

    So the two legitimately disagree: observed live, `/metrics` read 24 requests / 28,644
    tokens-before while the dashboard read 26 / 28,656 — a restart and a tenant scope, not a
    counting error. **Use `cg_tenant_*` for the persistent, per-tenant numbers.** Every
    in-process series now carries this caveat in its own HELP text, because HELP travels into
    every scraper, explorer and panel tooltip, and a note only in the docs is a note the
    person reading the panel never sees. See
    [Routes](reference/routes.md#get-metrics-the-two-families-do-not-agree).

The installer brings up Prometheus and Grafana beside the proxy, provisioned, in two
commands:

```sh
sudo deploy/service/install.sh grafana          # both containers, config, dashboards
sudo deploy/service/install.sh grafana-status   # scrape health + provisioning errors
# then, signed in as a MANAGER at /dashboard/:
#   https://<the host>/grafana/d/context-guru/context-guru
# or without the front end:
ssh -L 3000:127.0.0.1:3000 <the host>
# then http://127.0.0.1:3000/grafana/d/context-guru/context-guru
```

The front end publishes Grafana at `/grafana/` behind an nginx `auth_request` that only a
context-guru **manager**'s browser session satisfies — Grafana never sees a request that
fails it, not even to show its login page. That gate is also the sign-in: it names the
manager in a header Grafana's auth-proxy trusts from the loopback peer only, so a manager
lands in Grafana as an **Admin with no second password to hold**. Prometheus (9090) and Loki
(3100) are not published at all.

Containers rather than packages because Prometheus is in no RHEL 9 repository, so the
alternative is a packaged Grafana beside a tarball Prometheus with two unrelated sets of
paths to keep straight. Either podman or docker is used, whichever is present.

**Both bind loopback only**, which is why the `ssh -L` line is part of the procedure and
not a suggestion: `/metrics` is a service-wide view carrying every tenant's spend, and
Grafana's session cookie is as good as its admin password. Neither belongs on a shared
box's LAN interface. The built-in `admin` account is break-glass only — a first install
seeds it with a random value that is neither printed nor saved, because Grafana's default
when it is unset is `admin/admin`; set one yourself if you want that door, with the one
command in `deploy/grafana/README.md`. `grafana-remove` drops the containers and
deliberately **keeps** the metrics history.

The full procedure, the by-hand equivalent, password rotation, scraping a proxy on another
host, and the panel-by-panel reading guide live in
[`deploy/grafana/README.md`](https://github.com/rossoctl/context-guru/blob/main/deploy/grafana/README.md)
rather than being duplicated here.

![The provisioned context-guru Grafana dashboard over a 45-minute scrape window: the
health row reads UP, 4.0 req/m, a 41.5% compaction rate and 11 ms of added latency; the
savings row reads 14.1 K tokens removed and $0.09 saved this month, with actual spend
tracking below the uncompacted baseline; the component row ranks dedup and cmdfilter
first by tokens removed, hit rate and time
spent.](img/hosted/04-grafana.png)

Two dashboards are provisioned, each answering a different question.

**`context-guru`** — *is this thing working and paying for itself?* Six rows, in the order
you would actually ask: is it up and healthy · am I saving tokens and money · which
components earn their place · who is using it · is storage healthy · is anything failing.

**`context-guru-slo`** — *is the service meeting its obligations?* Availability, the latency
the service is itself responsible for, whether the observability path is dropping events,
and an HTTP error-rate SLI over `refused + processed` (a refused request never reaches the
aggregator, so `cg_requests_total` alone is the wrong denominator).

Every panel carries a description saying what a *bad* value looks like.

#### Two panels that lie, and are documented as lying

Worth knowing before you read either dashboard as a verdict:

- **Cache hit ratio reads `n/a`, not 0**, against an upstream that reports no cache tiers
  in its usage block (IBM LiteLLM does not). That zero is the upstream's silence, not a
  cache miss. The cost of rendering it neutral is that a genuine collapse to exactly 0
  would also read `n/a`; the metric cannot tell the two apart, and only the upstream can
  fix that.
- **`Availability, 30 days` is meaningless until Prometheus has retained 30 days.**
  `avg_over_time(up[30d])` averages the samples that exist, so a Prometheus started an hour
  ago reports a flattering 100%.

Those and the rest — what `cg_refused_requests_total` does *not* count, the per-tenant
series cap, month-to-date counters resetting at the first of the month, and the fact that
no alert rules are provisioned — are in
[`deploy/grafana/README.md`, "Known gaps"](https://github.com/rossoctl/context-guru/blob/main/deploy/grafana/README.md#known-gaps).

**Access.** `/metrics` is a service-wide view that includes per-tenant cost, so in
hosted mode it is gated exactly like `/stats`: loopback needs nothing (Prometheus
normally runs beside the proxy), anything else needs the bearer token from
`METRICS_TOKEN`.

**No emails in labels.** Series carry the tenant id and the account's label, never the
email. Metrics are typically the least access-controlled surface in an organisation,
and personal data does not belong in a scrape target. There is a test asserting it.

**The three numbers worth alerting on**, in order:

1. `cg_cache_hit_ratio` falling. Compaction that mutates an already-cached prefix
   forces a re-write at roughly 12× the read price. This moves before the bill does.
2. `cg_dash_events_total{disposition="dropped"}` above zero. The capture queue filled
   and observability is degrading under load — exactly when it is most wanted.
3. `cg_llm_failures_total{kind="timeout"}` in a run. The compaction model is silently
   doing nothing, so the deployment looks fast because it stopped working.

`cg_archive_configured` at 0 is the fourth: while it is 0, disk pressure deletes
instead of migrating.

Series colours in the dashboard are pinned rather than left to Grafana's classic
palette, which cycles hues and repaints the survivors when a series disappears. The
three used are validated colourblind-safe against Grafana's dark surface (worst
all-pairs CVD ΔE 9.4, normal-vision 20.9), and the meaning is consistent across
panels: **blue is what actually happened, orange is the comparison to read it
against**, so the gap between them is the story. No panel uses two y-axes.

## Accounts, in the browser

`/dashboard/` detects which world it is in by calling `GET /api/whoami`, which answers
**200 in every case**: `hosted: false` means this is a single-tenant proxy and every
account control stays hidden, `hosted: true` with `authenticated: false` shows the
sign-in gate, and an authenticated answer shows the dashboard — carrying the account,
its tokens and the registration mode, so the probe and the first render are one round
trip. The mode is detected rather than built in, because a compile-time flag is one more
thing to keep in step with the server.

| View | What it does |
|---|---|
| Sign in / Register | Registration takes an email, a password, a token label, and an invite code if the deployment is in `invite` mode; entering the [mailed 6-digit code](#the-sign-in-flow) verifies the address, returns the token **once**, and signs you in — so registration flows straight to Setup with the token already substituted into the snippets. On a `closed` deployment the attempt is refused — see [step 4](#4-choose-how-accounts-are-created). Signing in later is password + a fresh mailed code, and an account created before passwords existed can still sign in with its token. Either way the browser only ever holds the session cookie. |
| Setup | The three copy-paste blocks, with your own token and this deployment's real base URL (derived from the request, so it is correct behind nginx and on loopback alike). Your provider key stays where it already is; the blocks only add the base URL and the `x-context-guru-token` header — or, for Bob, the one-time key-binding curl. |
| Settings | Mode, upstream per dialect, component toggles, content-capture consent, raw YAML, month-to-date spend, bound agent keys, token management, signed-in machines, and your own configuration-change history. Read-only while you are [tracking the server default](#tenants-track-the-default-they-are-not-stamped-with-a-copy-of-it), with **Customise** to take ownership. |
| Archive | What has moved to cold storage, from the local index. Opening one fetches it back read-only. |
| Tenants | Manager only: every account with its month-to-date spend, disable an account, reissue a lost token. |

![Setup immediately after registering: a green banner reads "Your new token is filled in
below. It is shown once and cannot be recovered — copy it somewhere safe now", above
copy-paste export blocks for Claude Code, Bob and OpenAI-dialect tools carrying this
deployment's base URL and the freshly minted token (redacted
here).](img/hosted/01-register.png)

![The Settings page: spend this month, $86.42, reported above the
knobs; a mode selector set to "sync — compaction is applied"; one upstream dropdown per
dialect, populated from the operator's allow-list; a grid of pipeline-component
checkboxes with extract_llm carrying its own latency-and-billing warning; and the
transcript-capture consent box.](img/hosted/02-settings.png)

![The manager-only Tenants view: six placeholder accounts, each with its role,
month-to-date spend, when it was last seen, the first line of its
configuration, and per-row Metrics, Disable and Reissue token buttons. The
disabled account is greyed out with its actions
inert.](img/hosted/03-tenants.png)

Two rules the UI enforces because the server does:

- **Config edits are validated on save**, and a rejected document names the offending
  key. A settings page that accepts what the proxy will later refuse lets someone break
  their own agent and not find out until they use it.
- **Upstreams are a dropdown of the operator's allow-list**, never a text field.

The page keeps its strict same-origin CSP, no npm, no bundler and no CDN. `style-src
'self'` blocks inline style attributes, which is why styling goes through the CSSOM.

## Limits

| Bound | Flag | Default |
|---|---|---|
| Requests per minute, per tenant | `--tenant-rpm` | 0 (unlimited) |
| In-flight requests, per tenant | `--tenant-concurrent` | 0 (unlimited) |
| Concurrent compaction-model calls, process-wide | `--cheap-model-concurrent` | 4 |

The compaction-model bound is process-wide rather than per tenant on purpose: the point
is to stop one tenant's `extract_llm` traffic from making everyone else's agents wait on
a shared, rate-limited backend.

**There is no spend cap.** Each tenant's traffic is billed to the credential their own
agent sends, so there is nothing shared to ration. Cost is still recorded and displayed;
it needs `MODEL_INFO` on to be non-zero, and prices load asynchronously on first use, so
the very first request after a restart is recorded as `partial` and unpriced.

## Privacy

- **Transcript capture has two independent switches, and they default differently.**
  `--dashboard-content` / `DASHBOARD_CONTENT` is process-wide and this repository ships it
  `false`. The per-tenant switch behind it is created **on** — registration writes
  `capture_content: true`. Either one alone stops the writes, so on a stock install the
  operator's switch is what keeps a new account's source code off disk, and opening it starts
  capturing every account that has not turned its own switch off.
- **Whether *your* instance captures content is a fact about its environment, not about the
  shipped default — so read it, do not assume it.** A drop-in overrides the unit, so
  `DASHBOARD_CONTENT` in `context-guru.service` is not the effective value:
  `systemctl show context-guru -p Environment` is. Do this before answering a user who asks
  whether their source code is stored, because with the per-tenant switch defaulting on, an
  enabled operator switch means a new account's message content — agent output, tool results,
  source code — is captured from its first request. Both off switches belong in the same
  breath as the answer: a tenant clears their own consent on **Settings**, an operator sets
  `DASHBOARD_CONTENT=false` for everyone. Neither is retroactive in either direction. The
  redactor in front of the write is a best-effort denylist — a review of 22 realistic
  credential shapes found 11 passing through it — so this is a decision about real source
  code landing on disk.
- **A manager sees everyone's metrics and nobody's transcripts.** Reading another
  user's source code is not an administrative need, and the consent they gave was for
  their own view. A manager may *withdraw* that consent, *purge* what it produced and
  *delete* the account, and may start a password reset — but cannot set a password, which
  is what would otherwise let them sign in as a user and read it all anyway. See
  [Managing users](#managing-users).
- **Hosted mode refuses to start with `CONTEXT_GURU_DUMP` or `CONTEXT_GURU_CAPTURE` set.**
  Either one appends every tenant's pristine request bodies to a single process-wide file:
  unredacted, with no tenant column, on a path the redactor never runs. That bypasses both
  per-tenant capture consent and the scrubber, so the boot fails naming the variable rather
  than warning. Unset it, or drop `--upstreams` to run single-tenant, where the hook only
  sees your own traffic.
- Session keys are namespaced by tenant. Without that, two people running the same
  agent against the same repository hash to the same session id and would share one
  sticky offload set and one cached-prefix boundary — a cross-tenant collision
  arrived at by nobody doing anything wrong.

## Operational notes

- **This box becomes a single point of failure for everyone's agent.** That is the
  real cost of the design. `Restart=always`, the documented escape hatch above, and
  the fail-open invariant are the mitigations; say it out loud to users rather than
  letting them discover it during an outage.
- Evicting an idle tenant from the in-memory tenancy cache (`--max-tenancies`, 256)
  costs that tenant one cold cache on its next turn. It is logged at WARN, because
  otherwise it shows up as an unexplained cost spike.
- Changing a tenant's configuration rebuilds their pipeline and store, which discards
  their frozen compaction decisions. That is the honest cost of changing your own
  pipeline mid-session, and it is why the settings page is worth reading before
  clicking.
- **The Box OAuth token expires.** rclone refreshes it automatically while it is being
  used, but a remote left idle long enough will start failing with a 500 from Box. The
  fix is `rclone config reconnect box:` (the colon is part of the name). The symptom in
  the log is a run of `archiving failed` warnings with the local copies intact — no data
  is at risk, but disk stops being reclaimed, so it is worth alerting on.
- **Watch `ARCHIVE_BWLIMIT`.** rclone will use every bit of upload bandwidth it can
  get, and this box also carries everybody's agent traffic. The unit ships 8M; measure
  the real uplink before raising it, and remember `--bwlimit` is in bytes per second
  while speed tests report bits.
- Archiving is a background trickle against a rate-limited API: one object per session,
  serialised, at most `--archive-batch` (50) per pass every 15 minutes. A large backlog
  drains over hours rather than minutes, by design.
