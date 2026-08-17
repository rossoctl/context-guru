"""Base URLs and credentials for the benchmark harnesses.

One module, because this is the second time the scheme changed and the first time it
had to be edited in twelve copies of the same six lines.

There are two credentials in play and confusing them is the whole hazard:

  * the PROVIDER key — the IBM LiteLLM gateway key. It pays for the tokens. Under the
    new scheme the caller forwards it upstream itself, so the agent under test must
    hold the real one rather than a placeholder.
  * the context-guru TENANT token (`cg_live_…`) — it says WHO is calling, and buys
    nothing. It rides in the `x-context-guru-token` header, never in an auth slot,
    because an auth slot can hold only one value and the provider key needs it.

Neither is ever written to a file, a log, or a command line by this module: they are
read from the environment at call time and handed to the child process through its
environment. `--ae NAME='${VAR}'` is how they reach the agent's container — harbor
resolves `${VAR}` from its own environment (harbor/src/harbor/utils/env.py) and
persists the TEMPLATE rather than the value, so the token does not land in a
jobs-dir run config either.

Two deployments are supported and both must keep working:

  hosted     CG_TOKEN is set. The proxy runs with --upstreams and authenticates every
             request; the agent sends the real gateway key plus the token header.
  local      CG_TOKEN is unset. The proxy is single-tenant with a server-held key
             (start_proxy puts it in the proxy's own ANTHROPIC_API_KEY) and the agent
             sends the `sk-proxy` placeholder. This is how most contributors run the
             harness, so it is the default, not the fallback.
"""
import json
import os
from pathlib import Path

# The placeholder the agent presents to a single-tenant proxy that injects its own
# server-held key. Recognisably not a credential, which is the point.
PLACEHOLDER = "sk-proxy"

TOKEN_PREFIX = "cg_live_"  # tenant.TokenPrefix

# Names of the intermediate variables the harness exports for harbor to expand. Not
# credentials themselves — just where the values live for the length of one run.
KEY_VAR = "CG_AGENT_KEY"
HEADERS_VAR = "CG_AGENT_HEADERS"


def token():
    """The context-guru tenant token, or "" for a local no-auth proxy."""
    return os.environ.get("CG_TOKEN", "").strip()


def gateway():
    """(base_url, key) of the UPSTREAM the benchmark proxy forwards to.

    CG_GATEWAY_BASE / CG_GATEWAY_KEY win, so a run can be pointed anywhere without
    touching a settings file. Otherwise the values come from ~/.claude/settings.json,
    which is where this box keeps its gateway routing.

    That fallback is also the trap this function exists to close. Once your own Claude
    Code talks to context-guru, `ANTHROPIC_BASE_URL` in that file is the context-guru
    service and `ANTHROPIC_AUTH_TOKEN` may be a cg_live_ token. Feeding either to the
    benchmark proxy points it at another context-guru — a proxy loop whose symptom is
    doubled latency and a savings figure computed over already-compacted traffic. So
    both are checked, and a wrong one is a hard error naming the fix.
    """
    base = os.environ.get("CG_GATEWAY_BASE", "").strip()
    key = os.environ.get("CG_GATEWAY_KEY", "").strip()
    if not (base and key):
        try:
            e = json.load(open(Path("~/.claude/settings.json").expanduser()))["env"]
        except Exception as exc:
            raise SystemExit(
                f"cannot read ~/.claude/settings.json for gateway routing ({exc}); "
                "set CG_GATEWAY_BASE and CG_GATEWAY_KEY instead")
        base = base or e.get("ANTHROPIC_BASE_URL", "").strip()
        key = key or e.get("ANTHROPIC_AUTH_TOKEN", "").strip()
    if not (base and key):
        raise SystemExit("no gateway upstream: set CG_GATEWAY_BASE and CG_GATEWAY_KEY")
    if key.startswith(TOKEN_PREFIX):
        raise SystemExit(
            "the gateway key resolved to a context-guru token (cg_live_…), which buys "
            "no tokens and would make the benchmark proxy forward to itself.\n"
            "  Set CG_GATEWAY_KEY to your PROVIDER key, and CG_TOKEN to the cg token.")
    if base.rstrip("/").endswith(("/anthropic", "/openai/v1", "/openai")):
        raise SystemExit(
            f"the gateway base URL resolved to a context-guru route ({base}) — the "
            "benchmark proxy would forward to another context-guru, and every measured "
            "saving would be computed over already-compacted traffic.\n"
            "  Set CG_GATEWAY_BASE to the real provider/gateway base URL.")
    return base, key


def agent_auth():
    """What the agent under test needs to reach the benchmark proxy.

    Returns (env_overlay, ae_flags):
      env_overlay  extra variables for the harbor subprocess's environment. Put them
                   in the child's env — never interpolate them into a shell string.
      ae_flags     a ready-to-append `--ae …` fragment. The values are `${VAR}`
                   templates, so no credential appears on a command line.
    """
    tok = token()
    if not tok:
        # Local single-tenant proxy: the proxy holds the upstream key, the agent holds
        # a placeholder. Unchanged from before the auth scheme moved.
        overlay = {KEY_VAR: PLACEHOLDER}
        ae = [f"--ae ANTHROPIC_API_KEY='${{{KEY_VAR}}}'",
              f"--ae ANTHROPIC_AUTH_TOKEN='${{{KEY_VAR}}}'"]
        return overlay, " ".join(ae)
    if not tok.startswith(TOKEN_PREFIX):
        raise SystemExit(f"CG_TOKEN is not shaped like a context-guru token "
                         f"({TOKEN_PREFIX}…); the proxy would refuse every request")
    _, key = gateway()
    # The agent forwards its OWN provider key upstream, so both auth slots carry the
    # real gateway key. The tenant token goes in its own header — putting it in an auth
    # slot works (the proxy recognises and scrubs it) but then that slot can no longer
    # carry the provider key, and the request 401s at the gateway instead.
    overlay = {KEY_VAR: key, HEADERS_VAR: f"x-context-guru-token: {tok}"}
    ae = [f"--ae ANTHROPIC_API_KEY='${{{KEY_VAR}}}'",
          f"--ae ANTHROPIC_AUTH_TOKEN='${{{KEY_VAR}}}'",
          f"--ae ANTHROPIC_CUSTOM_HEADERS='${{{HEADERS_VAR}}}'"]
    return overlay, " ".join(ae)


def stats_request(url):
    """A urllib Request for the proxy's /stats.

    Loopback needs no credential (proxy.statsTrusted), and the harnesses run on the
    proxy's host — but a token is sent when there is one, so the same call also works
    through a front end where loopback stopped meaning "local caller".
    """
    import urllib.request
    req = urllib.request.Request(url)
    if tok := token():
        req.add_header("x-context-guru-token", tok)
    return req


def demo():
    """Self-check: the two deployments resolve, and no credential leaks into a flag."""
    env = dict(os.environ)
    try:
        os.environ.pop("CG_TOKEN", None)
        os.environ["CG_GATEWAY_BASE"] = "https://gateway.example/v1"
        os.environ["CG_GATEWAY_KEY"] = "sk-secret-provider-key"

        # local: placeholder only, real key untouched.
        overlay, ae = agent_auth()
        assert overlay == {KEY_VAR: PLACEHOLDER}, overlay
        assert "sk-secret-provider-key" not in ae
        assert "ANTHROPIC_CUSTOM_HEADERS" not in ae, "no token header without CG_TOKEN"

        # hosted: real key in the env overlay, header carries the token, and the flag
        # string contains NEITHER value — only ${VAR} templates.
        os.environ["CG_TOKEN"] = TOKEN_PREFIX + "x" * 32
        overlay, ae = agent_auth()
        assert overlay[KEY_VAR] == "sk-secret-provider-key"
        assert overlay[HEADERS_VAR].startswith("x-context-guru-token: " + TOKEN_PREFIX)
        for secret in ("sk-secret-provider-key", TOKEN_PREFIX + "x" * 32):
            assert secret not in ae, f"credential leaked into --ae flags: {ae}"
        assert "ANTHROPIC_CUSTOM_HEADERS" in ae

        # a cg token in the provider slot is a hard error, not a silent proxy loop.
        os.environ["CG_GATEWAY_KEY"] = TOKEN_PREFIX + "y" * 32
        try:
            gateway()
        except SystemExit:
            pass
        else:
            raise AssertionError("a cg_live_ gateway key must be refused")

        # so is a base URL pointing back at a context-guru route.
        os.environ["CG_GATEWAY_KEY"] = "sk-secret-provider-key"
        os.environ["CG_GATEWAY_BASE"] = "https://cg.example.com/anthropic"
        try:
            gateway()
        except SystemExit:
            pass
        else:
            raise AssertionError("a context-guru base URL must be refused")
    finally:
        os.environ.clear()
        os.environ.update(env)
    print("cgenv: ok")


if __name__ == "__main__":
    demo()
