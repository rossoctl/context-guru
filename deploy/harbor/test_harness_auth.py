#!/usr/bin/env python3
"""Does the harness hand the agent the right credentials, without leaking them?

Run it directly: `python3 deploy/harbor/test_harness_auth.py`. No framework, because
what it guards is small and specific:

  * the command each harness builds must reference `${CG_AGENT_KEY}` for the shell to
    expand, not interpolate a value. Getting this wrong is silent — an f-string
    replacement field looks identical to a shell variable, compiles cleanly, and fails
    with a NameError only once containers have already been built and paid for.
  * no credential may appear in the command string. It is the process's argv (readable
    by anyone on the box via ps), it is what harbor persists into the jobs dir for
    resume, and both outlive the run.
  * the local no-auth deployment must keep working with the sk-proxy placeholder. Most
    contributors run the harness that way and a hosted-only harness is a regression.
"""
import importlib, os, sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

AGENT_HARNESSES = ["terminalbench", "swebench", "swebench_rtk", "terminalbench_rtk",
                   "terminalbench_headroom"]

FAKE_KEY = "sk-a-real-looking-provider-key"
FAKE_TOKEN = "cg_live_" + "z" * 32


def render(mod):
    """run_harbor's command and child environment, without running anything."""
    import subprocess
    captured = {}

    def fake_run(args, **kw):
        captured["cmd"] = args[-1] if isinstance(args, list) else args
        captured["env"] = kw.get("env") or {}
        class R:
            returncode = 0
            stdout = stderr = b""
        return R()

    real = subprocess.run
    subprocess.run = fake_run
    try:
        # Signatures differ by one optional argument across the family.
        try:
            mod.run_harbor(["some-task"], "/tmp/does-not-exist", 1, 4.0, 4.0, 1.5)
        except TypeError:
            mod.run_harbor(["some-task"], "/tmp/does-not-exist", 1, 4.0, 4.0, 1.5, 2)
    finally:
        subprocess.run = real
    return captured["cmd"], captured["env"]


def check(name, hosted):
    mod = importlib.import_module(name)
    cmd, env = render(mod)

    assert "${CG_AGENT_KEY}" in cmd or "$CG_AGENT_KEY" in cmd, \
        f"{name}: the command does not reference CG_AGENT_KEY for the shell to expand:\n{cmd}"
    assert env.get("CG_AGENT_KEY"), f"{name}: CG_AGENT_KEY missing from the child environment"

    for secret in (FAKE_KEY, FAKE_TOKEN):
        assert secret not in cmd, \
            f"{name}: a credential was interpolated into the command line:\n{cmd}"

    if hosted:
        assert env["CG_AGENT_KEY"] == FAKE_KEY, \
            f"{name}: hosted mode must forward the caller's own provider key, got a placeholder"
        assert env.get("CG_AGENT_HEADERS") == f"x-context-guru-token: {FAKE_TOKEN}", \
            f"{name}: the tenant token must ride in x-context-guru-token"
        assert "ANTHROPIC_CUSTOM_HEADERS" in cmd, \
            f"{name}: the agent is never told to send the token header"
    else:
        import cgenv
        assert env["CG_AGENT_KEY"] == cgenv.PLACEHOLDER, \
            f"{name}: the local single-tenant path must keep using the placeholder"
        assert "ANTHROPIC_CUSTOM_HEADERS" not in cmd, \
            f"{name}: a no-auth local proxy must not be sent a token header"
    print(f"  {name}: ok")


def main():
    saved = dict(os.environ)
    try:
        os.environ["CG_GATEWAY_BASE"] = "https://gateway.example/v1"
        os.environ["CG_GATEWAY_KEY"] = FAKE_KEY
        os.environ["CG_LAN"] = "10.0.0.1"

        for hosted in (False, True):
            if hosted:
                os.environ["CG_TOKEN"] = FAKE_TOKEN
            else:
                os.environ.pop("CG_TOKEN", None)
            print(f"{'hosted' if hosted else 'local no-auth'}:")
            for name in AGENT_HARNESSES:
                # Re-imported per deployment: agent_auth() is read at call time, but the
                # modules cache nothing, so a plain import_module is enough.
                sys.modules.pop(name, None)
                check(name, hosted)

        # The gateway resolver's own guards, and the CG_TOKEN shape check.
        import cgenv
        cgenv.demo()
        os.environ["CG_TOKEN"] = "not-a-cg-token"
        try:
            cgenv.agent_auth()
        except SystemExit:
            pass
        else:
            raise AssertionError("a malformed CG_TOKEN must be refused before any spend")
    finally:
        os.environ.clear()
        os.environ.update(saved)
    print("harness auth: ok")


if __name__ == "__main__":
    main()
