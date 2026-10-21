"""results/ -> summary.md and report.html. Local files only; nothing is published or fetched."""
from __future__ import annotations

import base64
import fnmatch
import html
import json
import pathlib
import sys

ORDER = ("FAIL", "BLOCKED", "KNOWN", "PASS")
DEFAULT_KNOWN = pathlib.Path(__file__).with_name("known-findings.txt")


def explains(finding: tuple, row: dict) -> bool:
    """A known finding explains a verdict row when one of its check globs matches the id AND its evidence
    substring is in the row's actual value: a new defect under an old check id is not the old finding."""
    globs, evidence = finding[0], finding[1]
    return any(fnmatch.fnmatchcase(row["id"], g) for g in globs.split()) and evidence in json.dumps(row["actual"])


def known(path) -> list[tuple[str, str, str]]:
    """(check globs, evidence substring, why) per line of `<globs> | <evidence>  # why`."""
    p = pathlib.Path(path)
    if not p.exists():
        return []
    out = []
    for line in p.read_text().splitlines():
        body = line.strip()
        if not body or body.startswith("#"):
            continue
        head, _, why = body.partition(" # ")
        globs, bar, evidence = head.partition("|")
        if not bar or not globs.strip() or not evidence.strip():
            raise ValueError(f"known finding without `<globs> | <evidence>`: {line!r}")
        out.append((globs.strip(), evidence.strip(), why.strip()))
    return out


def unknown_fails(results_dir, known_findings=DEFAULT_KNOWN) -> list[str]:
    """FAIL ids no known-findings line explains: what the smoke gate stops on."""
    kn = known(known_findings)
    v = json.loads((pathlib.Path(results_dir) / "verdicts.json").read_text())
    return [r["id"] for r in v if r["verdict"] == "FAIL" and not any(explains(f, r) for f in kn)]


def _cell(x, n=120) -> str:
    return json.dumps(x)[:n].replace("|", "\\|")


def build(results_dir: str, known_findings: str | None = None) -> None:
    d = pathlib.Path(results_dir)
    v = json.loads((d / "verdicts.json").read_text())
    phases = [json.loads(l) for l in (d / "phases.jsonl").read_text().splitlines() if l.strip()]
    counts = {k: sum(r["verdict"] == k for r in v) for k in ORDER}
    overall = "FAIL" if counts["FAIL"] or any(p["status"] != "ok" for p in phases) else "PASS"
    mut = None
    for m in sorted(d.parent.glob("*-mutation/mutation.json")):
        mut = json.loads(m.read_text())
    if isinstance(mut, dict):
        mut = mut.get("mutations", [])
    lines = [f"# PR #578 e2e review: {overall}", "",
             " ".join(f"{k} {counts[k]}" for k in ORDER), "",
             "## Phases", "", "| phase | status | seconds |", "|---|---|---|"]
    lines += [f"| {p['phase']} | {p['status']} | {p['seconds']} |" for p in phases]
    lines += ["", "## Failures and blocked checks", "", "| id | verdict | expected | actual | snapshot |", "|---|---|---|---|---|"]
    for r in sorted(v, key=lambda r: ORDER.index(r["verdict"])):
        if r["verdict"] == "PASS":
            continue
        lines.append(f"| {r['id']} | {r['verdict']} | {_cell(r['expected'])} | {_cell(r['actual'])} | {r['snapshot']} |")
    kfind = known(known_findings or DEFAULT_KNOWN)
    if kfind:
        bad = [r for r in v if r["verdict"] in ("FAIL", "KNOWN")]
        lines += ["", "## Known PR findings", "", "| finding | checks | evidence | FAIL/KNOWN here |", "|---|---|---|---|"]
        lines += [f"| {f[2]} | {f[0]} | {f[1]} | {sum(explains(f, r) for r in bad)} |" for f in kfind]
        rest = [r["id"] for r in bad if not any(explains(f, r) for f in kfind)]
        lines += ["", "Not explained by a known finding: " + (", ".join(rest) if rest else "none")]
    notes = (d / "notes.md").read_text() if (d / "notes.md").exists() else ""
    if notes:
        lines += ["", notes.rstrip()]
    tp = d / "timings.json"
    if tp.exists():
        st = json.loads(tp.read_text()).get("stress", {})
        if st:
            lines += ["", "## Stress timings", "", "| measure | value |", "|---|---|"]
            lines += [f"| {k} | {val} |" for k, val in st.items()]
    if mut:
        lines += ["", "## Mutation round", "", "| break | caught | turned red |", "|---|---|---|"]
        lines += [f"| {m['id']} | {m['caught']} | {','.join(m['turned_red'])} |" for m in mut]
    lines += ["", "The oracle is independent of the PR's code, not of its author: both were written in the "
              "same session. The mutation round is what shows its checks can fail."]
    (d / "summary.md").write_text("\n".join(lines) + "\n")

    def img(rel):
        p = d / rel
        if not rel or not p.exists():
            return ""
        return f'<img loading="lazy" src="data:image/png;base64,{base64.b64encode(p.read_bytes()).decode()}">'

    # Each PNG is embedded once, in the gallery; a check row links to it (many checks share one page).
    def link(rel):
        stem = pathlib.Path(rel).stem
        return f'<a href="#snap-{html.escape(stem)}">{html.escape(stem)}</a>' if rel and (d / rel).exists() else ""

    rows = "".join(
        f'<tr class="{r["verdict"]}"><td>{html.escape(r["id"])}</td><td>{r["verdict"]}</td>'
        f'<td><code>{html.escape(json.dumps(r["expected"])[:300])}</code></td>'
        f'<td><code>{html.escape(json.dumps(r["actual"])[:300])}</code></td><td>{link(r["snapshot"])}</td></tr>'
        for r in sorted(v, key=lambda r: ORDER.index(r["verdict"])))
    shots = "".join(f'<figure id="snap-{html.escape(p.stem)}">{img("snapshots/" + p.name)}<figcaption>{p.stem}</figcaption></figure>'
                    for p in sorted((d / "snapshots").glob("*.png")))
    page = f"""<!doctype html><meta charset="utf-8"><title>PR 578 e2e review</title>
<style>:root{{--bg:#fff;--fg:#1b1b1f;--bad:#b3261e;--ok:#1e6b3a;--mut:#666}}
@media (prefers-color-scheme:dark){{:root{{--bg:#141417;--fg:#e8e8ea;--bad:#ff8a80;--ok:#7bd88f;--mut:#aaa}}}}
body{{background:var(--bg);color:var(--fg);font:14px system-ui,sans-serif;margin:0;padding:16px;max-width:1200px}}
table{{border-collapse:collapse;width:100%}}td,th{{border-bottom:1px solid #8884;padding:6px;vertical-align:top;text-align:left}}
tr.FAIL td:nth-child(2){{color:var(--bad);font-weight:600}}tr.PASS td:nth-child(2){{color:var(--ok)}}
pre{{white-space:pre-wrap;font-size:12px}}img{{max-width:320px;border:1px solid #8884}}figure{{display:inline-block;margin:8px}}code{{font-size:12px;word-break:break-word}}
.f button{{margin-right:6px}}</style>
<h1>PR #578 e2e review: {overall}</h1><p>{" · ".join(f"{k} {counts[k]}" for k in ORDER)}</p>
<p class="f"><button onclick="f('')">all</button><button onclick="f('FAIL')">failures</button></p>
<table><tr><th>check</th><th>verdict</th><th>expected</th><th>actual</th><th>snapshot</th></tr>{rows}</table>
<h2>Summary</h2><pre>{html.escape((d / "summary.md").read_text())}</pre>
<h2>Snapshots</h2>{shots}
<script>function f(k){{for(const r of document.querySelectorAll('tr[class]'))r.hidden=k&&r.className!==k}}</script>"""
    (d / "report.html").write_text(page)


if __name__ == "__main__":
    # --unknown-fails <dir> [known-file]: one id per line; any error exits non-zero so the gate closes.
    if sys.argv[1] == "--unknown-fails":
        print("\n".join(unknown_fails(*sys.argv[2:4])))
    else:
        build(sys.argv[1])
