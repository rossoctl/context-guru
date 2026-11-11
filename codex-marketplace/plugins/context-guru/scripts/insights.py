#!/usr/bin/env python3
"""Produce deterministic, ranked findings from the local proxy's measured statistics."""

import argparse
import json
import os
from pathlib import Path
import urllib.request


def state_dir():
    return Path(os.environ.get("XDG_STATE_HOME", Path.home() / ".local/state")) / "context-guru-codex"


def record():
    try:
        return json.loads((state_dir() / "install.json").read_text())
    except (OSError, ValueError):
        return {}


def get(port, path):
    with urllib.request.urlopen(f"http://127.0.0.1:{port}{path}", timeout=30) as response:
        return json.load(response)


def number(value):
    return value if isinstance(value, (int, float)) and not isinstance(value, bool) else None


def finding(identifier, severity, message, fix, **measurements):
    return {"id": identifier, "severity": severity, "message": message,
            "fix": fix, **measurements}


def report(area="all"):
    port = record().get("port")
    if not isinstance(port, int):
        return {"result": "not_installed", "findings": []}
    try:
        stats = get(port, "/api/stats")
    except Exception as error:
        return {"result": "proxy_unavailable", "detail": str(error), "findings": []}
    savings = stats.get("savings", {}).get("all", {})
    findings = []
    total = number(savings.get("total_saved_usd"))
    if total is None:
        total = number(stats.get("total_saved_usd"))
    if total is not None and area in ("all", "components"):
        findings.append(finding("measured-net-savings", "ok" if total >= 0 else "high",
                                "Measured net context-guru value",
                                ("none" if total >= 0 else
                                 "Run $context-guru-preset-picker."), usd=total,
                                basis="measured over the retained window"))
    keepalive = number(savings.get("keepalive_net_usd"))
    if keepalive is None:
        keepalive = number(stats.get("keepalive_net_usd"))
    if keepalive is not None and area in ("all", "idle"):
        findings.append(finding(
            "keepalive-net", "ok" if keepalive >= 0 else "high",
            "Measured cache keep-alive net value",
            ("Keep the current strategy." if keepalive >= 0 else
             "python3 ../../scripts/codex_plugin.py configure --cache-strategy none"),
            usd=keepalive, basis="measured over the retained window"))
    saved_tokens = number(stats.get("saved_tokens_unique"))
    if saved_tokens is None:
        saved_tokens = number(stats.get("saved_tokens"))
    if saved_tokens is not None and area in ("all", "components"):
        findings.append(finding("saved-tokens", "info",
                                "Tokens removed by configured components", "none",
                                tokens=saved_tokens,
                                basis="measured in tokens; unpriced, so no dollar figure"))
    if area in ("all", "components"):
        for name, values in (stats.get("components") or {}).items():
            if not isinstance(values, dict):
                continue
            tokens = number(values.get("saved_tokens_unique"))
            if tokens is None:
                tokens = number(values.get("saved_tokens"))
            if tokens is not None:
                findings.append(finding(
                    f"component-{name}", "info", f"Measured contribution from {name}", "none",
                    tokens=tokens, basis="measured in tokens; unpriced, so no dollar figure"))
    if area in ("all", "capabilities"):
        try:
            capabilities = get(port, "/api/tools")
        except Exception:
            capabilities = None
        rows = capabilities if isinstance(capabilities, list) else (
            capabilities.get("tools") or [] if isinstance(capabilities, dict) else [])
        for row in rows:
            if not isinstance(row, dict):
                continue
            name = row.get("name") or row.get("tool")
            calls = number(row.get("calls"))
            if name and calls == 0:
                findings.append(finding(
                    f"unused-capability-{name}", "warning", f"{name} was declared but never used",
                    row.get("fix") or "Disable this capability in its owning configuration.",
                    basis="measured size of the problem, NOT a projected saving"))
    severity = {"high": 0, "warning": 1, "ok": 2, "info": 3}
    findings.sort(key=lambda row: (severity[row["severity"]],
                                   -abs(row.get("usd", 0)), row["id"]))
    return {"result": "ok", "area": area, "requests": stats.get("requests"),
            "findings": findings}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("area", nargs="?", default="all",
                        choices=("all", "capabilities", "idle", "components"))
    parser.add_argument("--json", action="store_true")
    args = parser.parse_args()
    data = report(args.area)
    if args.json:
        print(json.dumps(data, sort_keys=True))
    else:
        print(f"result={data['result']}")
        if "requests" in data:
            print(f"requests={data['requests']}")
        for index, finding in enumerate(data["findings"], 1):
            for key, value in finding.items():
                print(f"finding.{index}.{key}={value}")
    return 0 if data["result"] == "ok" else 1


if __name__ == "__main__":
    raise SystemExit(main())
