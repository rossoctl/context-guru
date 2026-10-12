#!/usr/bin/env python3
"""Read the proxy's own measurements and turn them into RANKED, PRICED, ACTIONABLE findings.

This is the data half of `/context-guru:insights` and its three focused siblings. The skills
narrate; every number, every ranking and every counterfactual is computed here. That split is
deliberate and it is the whole reason this file exists rather than four skills holding eight curl
commands: a dollar figure a model composed from two JSON fields in prose is a dollar figure nobody
can reproduce, and this repo's entire dashboard exists to stop that happening.

WHAT IT IS NOT. It is not `claude doctor`. Doctor answers "is the tool installed correctly" — a
health check whose findings are all about itself. This answers "what is your configuration
COSTING you", from your own billed traffic, and every finding ends in a command. The two
structural things borrowed from doctor, because they are genuinely good, are its per-check shape
(label -> verdict -> issue -> one-line Fix) and its terminal "nothing to fix" state. Everything
else here has no counterpart there: money, a measured window, coverage, counterfactuals, and a
refusal in place of a number when the sample cannot carry one.

HARD RULES, each of which is a way this could otherwise state something untrue:

  * READ-ONLY. Every request is a GET against the local proxy's dashboard API. Nothing here writes
    a settings file, arms a strategy, or sends a keep-alive ping. The fixes are PRINTED, never
    applied — applying one is a thing the user asks for, in the skill, with the command in front of
    them.
  * NEVER A FABRICATED MEASUREMENT. A figure the proxy did not measure is absent, not zero. Absence
    and zero are different claims about the same field and this file keeps them apart everywhere:
    `priced=false` means "no rate for that model", `coverage_not_captured` means "those sessions
    cannot answer", and a refusal from the server is forwarded as a refusal rather than softened
    into a small number.
  * NEVER A POINT ESTIMATE WHERE THE SERVER REFUSES TO GIVE ONE. /api/keepalive/recommend
    deliberately has no point-estimate field — every account in the production corpus has a 90%
    interval whose relative half-width is at least 62%. So the keep-alive finding carries lo/hi and
    this file does not average them into a headline.
  * A PROJECTION IS LABELLED AND IT IS REFUSED ON A SHORT WINDOW. `usd_month` is the window figure
    scaled to 30 days, and it is emitted only when the measured window is at least
    MIN_PROJECTION_DAYS long. Scaling four hours of traffic by 180 produces a number with a dollar
    sign and no meaning.
  * NO CREDENTIAL, NO PROMPT TEXT, NO TRANSCRIPT. Every endpoint read here returns numbers, enum
    labels and configuration identifiers. /api/prompt — the one dashboard route that serves prompt
    TEXT — is deliberately not read: a cost report does not need the bytes to say what they cost.

WHY PYTHON AND NOT SHELL. The same reason statusline.py is: JSON off an HTTP endpoint, with
timeouts, is what `json` and `urllib.request` do directly, and the shell equivalent needs `jq`
(not guaranteed installed) plus a curl subprocess per endpoint.

OUTPUT is flat `key=value` lines, the same shape `settings.py` emits, because that is what this
repo's skills already read. Rows are indexed rather than nested (`finding.1.fix=...`), so a skill
can grep one out without a JSON parser. `--json` prints the assembled document instead, for a
human who wants the raw thing.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import settings  # noqa: E402  - same directory, and the ONE place option files are resolved

# There is deliberately no default-port constant in this file. Since per-project ports landed, 8787
# is not a safe answer for "no port configured" — it is PORT_SCAN_START, the first port the
# allocator hands out, so on a machine with any install at all it belongs to whichever project
# installed first. The one case where it IS the answer (a record written before ports were per
# project, which therefore carries no port) lives with the rest of the rule, in
# settings.resolve_routed_port and settings.LEGACY_SINGLE_PROXY_PORT.

# /api/tools, /api/components and /api/facets are the three reads core itself puts on a longer
# timeout (dashHeavyTimeout) because the default is simply too short for them on a large database.
# This is a deliberate, user-invoked command rather than a status line, so it can afford to wait —
# but it still must not hang a session for ever.
HEAVY_TIMEOUT_SECONDS = 30.0
LIGHT_TIMEOUT_SECONDS = 8.0

# How long the measured window must be before a 30-day projection is offered at all. Two days,
# because a projection's whole claim is that the window is REPRESENTATIVE, and one working day is
# a day of one task: the tool nobody needed on Tuesday is the tool Wednesday's task needs. It is
# the same argument the server's own suggester makes for its 7-day span floor, applied to a weaker
# claim (scaling a figure, not authorising a removal) and therefore with a lower floor.
MIN_PROJECTION_DAYS = 2.0

DAYS_PER_MONTH = 30.0

# Severity is a TIE-BREAK, never the sort key. Findings are ranked by money, because that is what
# the report is for and because a "high severity" finding worth $0.02 above a "medium" worth $18 is
# a ranking that trains the reader to ignore the order. Severity only decides between two findings
# whose money is equal or, more often, both unpriced.
SEVERITY_ORDER = {"high": 0, "medium": 1, "low": 2, "info": 3, "ok": 4}

# The presets this plugin offers (plus house/codesmart/housellm, still explainable for anyone who
# set one of those directly through /plugin configure's raw form before or instead of the picker),
# each with the component pipeline it actually builds. This table is DUPLICATED from
# config/config.go's presetPipelines on purpose — the plugin ships as a directory of scripts with
# no Go in it — and TestPluginPresetPipelinesAgreeWithConfig pins every entry against
# config.PresetPipeline so the duplicate cannot drift. Without that test this table would be a
# second, quietly wrong idea of what a preset does, which is exactly what it is here to report on.
# TestEveryPresetOfferedIsOneInsightsCanExplain pins the other direction: every name settings.py's
# PRESETS offers through the picker must have an entry here too.
PRESET_PIPELINES: dict[str, tuple[str, ...]] = {
    "off": (),
    "cache": ("cachesplit",),
    "house": ("format", "dedup", "toon", "cmdfilter", "searchfold", "textclean", "extract",
              "cachesplit", "toolfilter"),
    "codesmart": ("format", "textclean", "searchfold", "dedup", "failed_run", "cmdfilter",
                  "extract_llm", "extract", "linecap", "cachesplit"),
    "housellm": ("format", "dedup", "toon", "cmdfilter", "searchfold", "textclean", "extract_llm",
                 "extract_llm_sweep", "extract", "cachesplit", "toolfilter"),
    "conservative": ("format", "dedup", "toon", "cmdfilter", "searchfold", "textclean", "extract",
                      "cachesplit", "toolfilter"),
    "medium": ("format", "dedup", "toon", "cmdfilter", "searchfold", "textclean", "extract_llm",
               "extract", "cachesplit", "toolfilter"),
    "high": ("summarize", "format", "dedup", "toon", "cmdfilter", "searchfold", "textclean",
              "extract_llm", "extract", "cachesplit", "toolfilter"),
    "xhigh": ("summarize", "format", "dedup", "toon", "cmdfilter", "searchfold", "textclean",
              "extract_llm", "extract_llm_sweep", "extract", "cachesplit", "toolfilter"),
}

# What each component this plugin can turn on actually does, in one line a developer can act on,
# and — the part that matters for a counterfactual — WHERE its "what would it have saved me"
# number comes from, or the honest statement that there isn't one.
#
# `counterfactual` is the load-bearing field and it has exactly three legal values:
#
#   "measured"   — the proxy has a figure for what this component WOULD have removed, measured
#                  from traffic it never touched. Only two components have one, and both are
#                  measured rather than simulated: `toolfilter`'s is the qualified-suggestion
#                  total (the declarations it would have withheld, priced at the tier the
#                  requests that carried them were really billed), and `cachesplit`'s is the
#                  keep-alive tab's addressable-expiry figure.
#   "related"    — no counterfactual for the component itself, but a MEASURED quantity of the
#                  waste it targets exists (compaction credit, tool-output weight). Reported as
#                  evidence that there is something here to win, never as a saving.
#   "none"       — nothing measured either way. Named with what it does and no number at all.
#                  Quoting a study figure from another corpus as this account's saving is the
#                  single easiest way to make this whole report untrustworthy.
COMPONENTS: dict[str, dict[str, str]] = {
    "format":     {"what": "normalises message shape; removes no content", "counterfactual": "none"},
    "textclean":  {"what": "strips redundant whitespace and control noise from tool output",
                   "counterfactual": "none"},
    "searchfold": {"what": "folds repeated search/grep hit blocks", "counterfactual": "none"},
    "dedup":      {"what": "drops tool outputs that are byte-identical to an earlier one",
                   "counterfactual": "none"},
    "toon":       {"what": "re-encodes tabular tool output more compactly", "counterfactual": "none"},
    "cmdfilter":  {"what": "trims command echo and shell preamble from bash output",
                   "counterfactual": "none"},
    "failed_run": {"what": "offloads the output of runs a later run superseded",
                   "counterfactual": "none"},
    "linecap":    {"what": "caps very long single tool outputs", "counterfactual": "none"},
    "collapse":   {"what": "head/tail collapse as a last-resort catch-all", "counterfactual": "none"},
    "mask":       {"what": "offloads tool outputs older than a threshold", "counterfactual": "related"},
    "extract":    {"what": "keeps the relevant part of a large tool output, reversibly",
                   "counterfactual": "related"},
    "extract_llm": {"what": "a cheap model decides what of a tool output is still relevant "
                            "(spends on its own)", "counterfactual": "related"},
    "extract_llm_sweep": {"what": "a periodic second pass of extract_llm over older turns",
                          "counterfactual": "related"},
    "summarize":  {"what": "a compaction model rewrites the transcript", "counterfactual": "related"},
    "smartcrush": {"what": "structure-aware compression of large outputs", "counterfactual": "none"},
    "agentdiet":  {"what": "the published AgentDiet baseline, for A/B only", "counterfactual": "none"},
    "cachesplit": {"what": "moves the cache breakpoint off the volatile tail so the prefix stays "
                           "cacheable", "counterfactual": "measured"},
    "cacheinject": {"what": "places a cache breakpoint where none was set", "counterfactual": "none"},
    "toolfilter": {"what": "stops sending tool/skill declarations you never invoke",
                   "counterfactual": "measured"},
}


# ---------------------------------------------------------------------------
# Plumbing: the port, the fetch, the formatting
# ---------------------------------------------------------------------------

def _resolve_port() -> tuple[str | None, str]:
    """(port, source) — the port this directory's install is on, and what said so. `None` when
    nothing claims to route this directory, which is NOT the same as 8787.

    One line of code, because the rule belongs to exactly one function now, and it is
    `settings.resolve_reportable_port`: the REPORT question, not the gate. A cost report is
    about an install and has to keep answering in a session whose environment points elsewhere,
    where `resolve_routed_port()` correctly says nothing routes here.
    This file used to carry its own copy in two parts
    (`_resolve_port` walking the option files, `_recorded_port` walking install-scope.json), and
    statusline.py and the two hooks each carried a third variant. They were not the same rule, and
    the ways they differed were invisible from any one of them — which is how the status line went
    permanently blank on a correct install while this report, reading the same machine, was right.
    Everything those docstrings recorded lives in the shared one; the two properties worth
    restating here, because THIS file is a money report:

      * Never $CLAUDE_PLUGIN_OPTION_PORT. Claude Code puts those variables into HOOK environments
        only, and this script runs from a skill's Bash block, so the "configured" value it used to
        read was always the default whatever the user set.
      * Never 8787 as a fallback. Ports are per project from PORT_SCAN_START upwards, so the
        "default" is whichever project installed first. Measured before the fix: from a directory
        with no install, this reported `proxy_up=true` off an UNRELATED proxy on 8787 and priced
        that install's traffic as this account's spend, with `port_source=(default)` the only
        signal and no finding raised. `None` here becomes a finding and `unavailable=port`.

    Read-only, which `TestInsightsWritesNothingAndPingsNothing` asserts: the shared rule uses
    `_read_install_scopes()` deliberately in place of `resolve_install_scope()`, which self-heals
    legacy rows by WRITING as it answers. A cost report must not edit the state it reports on.
    """
    return settings.resolve_reportable_port()


def _configured_options() -> tuple[dict, dict]:
    """Every configured option, and the file EACH one came from. A key absent from the result is
    UNCONFIGURED, and the caller applies the plugin.json default for that key only.

    Genuinely per option, which this claimed and did not do: it used to return the first file whose
    options dict was non-empty, so a project file holding only `preset` hid a `port`, `upstream` or
    `idle_exit` set in `~/.claude/settings.json`. With one install per machine that could not be
    seen. With a project install and a machine-wide one it is the normal state — and #308's
    uninstall path deliberately leaves the project's file in place with its `port` option removed,
    which is exactly this shape. `port` resolved correctly through `_resolve_port` all along, which
    made the mismatch harder to notice rather than easier: the report named the project's file and
    then printed `upstream=(anthropic)` for a machine-wide install that has an upstream.

    `cmd_config` in settings.py answers a different question (which single file holds the options
    this project's install wrote, for an installer that must undo precisely what it wrote) and is
    correct as it stands.
    """
    opts: dict = {}
    sources: dict = {}
    for path in settings._option_file_candidates("context-guru@context-guru"):
        if not os.path.exists(path):
            continue
        try:
            with open(path, encoding="utf-8") as fh:
                data = json.load(fh)
        except (OSError, ValueError):
            continue
        found = (((data.get("pluginConfigs") or {}).get("context-guru@context-guru") or {})
                 .get("options") or {})
        if not isinstance(found, dict):
            continue
        for key, value in found.items():
            # Most specific first, so the first file to name a key wins it — the same precedence
            # Claude Code itself applies, and the same order _resolve_port walks.
            if key not in opts:
                opts[key] = value
                sources[key] = path
    return opts, sources


class Fetcher:
    """One GET per endpoint, with per-endpoint timeouts and every failure recorded rather than
    raised.

    Failures are DATA. A dashboard route that 404s means the proxy is running without `--dashboard`
    and every insight is unavailable — which is itself the single most actionable finding this
    command can produce, so it must survive being discovered rather than crash the report.
    """

    def __init__(self, port: str) -> None:
        self.port = port
        self.errors: dict[str, str] = {}

    def reachable(self, path: str = "/healthz") -> bool:
        """Did anything answer at all — WITHOUT parsing the body.

        Separate from get() because /healthz answers with plain text, so routing it through a JSON
        parse reports a healthy proxy as absent. That was the first version of this and it made
        every run say the proxy was down while it was serving the very figures being printed.
        """
        try:
            with urllib.request.urlopen(f"http://127.0.0.1:{self.port}{path}",
                                        timeout=LIGHT_TIMEOUT_SECONDS) as resp:
                resp.read(1024)
            return True
        except urllib.error.HTTPError:
            return True  # it answered; a status code is an answer and the process is alive
        except (urllib.error.URLError, OSError):
            return False

    def get(self, path: str, params: dict | None = None, heavy: bool = False) -> object | None:
        url = f"http://127.0.0.1:{self.port}{path}"
        if params:
            url += "?" + urllib.parse.urlencode({k: v for k, v in params.items() if v not in (None, "")})
        timeout = HEAVY_TIMEOUT_SECONDS if heavy else LIGHT_TIMEOUT_SECONDS
        try:
            with urllib.request.urlopen(url, timeout=timeout) as resp:
                body = resp.read(16 << 20)
        except urllib.error.HTTPError as exc:
            # A reached-but-refused route. 404 is "this build/flag does not serve it", 401/403 is
            # a scope refusal — both are states a reader must be told about by name, because the
            # fix differs (restart with --dashboard vs. you are not the tenant).
            self.errors[path] = f"http_{exc.code}"
            return None
        except (urllib.error.URLError, OSError) as exc:
            self.errors[path] = f"unreachable:{type(exc).__name__}"
            return None
        try:
            return json.loads(body)
        except (ValueError, TypeError):
            self.errors[path] = "unparseable"
            return None


def _num(value: object, default: float = 0.0) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return default
    value = float(value)
    if value != value or value in (float("inf"), float("-inf")):
        return default
    return value


def _get_in(obj: object, *path: str) -> object:
    cur = obj
    for key in path:
        if not isinstance(cur, dict):
            return None
        cur = cur.get(key)
    return cur


def _flat(value: object) -> str:
    """One line, no newlines, for the key=value wire format. A field that carries a newline would
    silently become two facts, one of which has no key."""
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, float):
        return f"{value:.4f}".rstrip("0").rstrip(".") or "0"
    return re.sub(r"\s+", " ", str(value)).strip()


def _usd(value: float) -> str:
    """Dollars at a resolution that does not read as zero. A real $0.004 finding printed as $0.00
    looks like a broken meter, and this report's credibility is the only thing it has."""
    if value and abs(value) < 0.01:
        return f"${value:.4f}".rstrip("0")
    return f"${value:,.2f}"


def _tokens(n: float) -> str:
    n = float(n)
    for scale, suffix in ((1e9, "B"), (1e6, "M"), (1e3, "k")):
        if abs(n) >= scale:
            return f"{n / scale:.1f}".rstrip("0").rstrip(".") + suffix
    return f"{n:.0f}"


# ---------------------------------------------------------------------------
# A finding
# ---------------------------------------------------------------------------

class Finding:
    """One thing the reader can act on, with the evidence it rests on and the command that does it.

    The four fields that are never optional are `title`, `evidence`, `fix` and `basis`, and each is
    non-optional for a reason:

      * evidence — the measurement, with its n. A claim without its n is a claim this report is not
        entitled to make.
      * fix — the command. A finding with no fix is an observation, and observations belong in the
        facts block, not in a ranked list of things to do.
      * basis — what the figure IS: measured over the window, projected from it, or a range. This
        is the field that stops a forecast being read as a bill.
    """

    def __init__(self, ident: str, severity: str, title: str, evidence: str, fix: str,
                 basis: str, usd_window: float | None = None, usd_month: float | None = None,
                 usd_lo: float | None = None, usd_hi: float | None = None) -> None:
        self.ident = ident
        self.severity = severity
        self.title = title
        self.evidence = evidence
        self.fix = fix
        self.basis = basis
        self.usd_window = usd_window
        self.usd_month = usd_month
        self.usd_lo = usd_lo
        self.usd_hi = usd_hi

    def rank_money(self) -> float:
        """The figure the ranking uses. The LOW end of a range, never its midpoint: a finding is
        ordered by what it is worth at worst, so a wide interval cannot outrank a smaller, certain
        saving on the strength of its optimistic end."""
        if self.usd_lo is not None:
            return max(0.0, self.usd_lo)
        for candidate in (self.usd_month, self.usd_window):
            if candidate is not None:
                return max(0.0, candidate)
        return 0.0

    def as_facts(self) -> dict:
        out = {
            "id": self.ident,
            "severity": self.severity,
            "title": self.title,
            "evidence": self.evidence,
            "fix": self.fix,
            "basis": self.basis,
        }
        for key, value in (("usd_window", self.usd_window), ("usd_month", self.usd_month),
                           ("usd_lo", self.usd_lo), ("usd_hi", self.usd_hi)):
            if value is not None:
                out[key] = round(value, 4)
        return out


def _rank(findings: list[Finding]) -> list[Finding]:
    return sorted(findings, key=lambda f: (-f.rank_money(),
                                           SEVERITY_ORDER.get(f.severity, 9), f.ident))


class Report:
    """The assembled answer: the environment it was measured in, the facts, and the ranked findings.

    `unavailable` is its own list and not an empty findings list. "We could not measure this" and
    "we measured this and there is nothing wrong" are the two answers a report like this must never
    conflate, and conflating them is how a blinded dashboard reads as a clean bill of health.
    """

    def __init__(self) -> None:
        self.facts: dict[str, object] = {}
        self.findings: list[Finding] = []
        self.unavailable: list[str] = []

    def fact(self, key: str, value: object) -> None:
        if value is not None:
            self.facts[key] = value

    def document(self) -> dict:
        """The assembled report, which is the ONE source every output mode renders.

        Split out from emit so `--report` cannot drift from `--json`: a second traversal of the
        facts to build the page would be a second idea of what the report says, and the two would
        disagree the first time either changed.
        """
        return {
            "result": "ok",
            "facts": self.facts,
            "unavailable": self.unavailable,
            "findings": [f.as_facts() for f in _rank(self.findings)],
        }

    def emit(self, as_json: bool) -> None:
        doc = self.document()
        if as_json:
            print(json.dumps(doc, indent=2, sort_keys=True))
            return
        print("result=ok")
        for key, value in self.facts.items():
            print(f"{key}={_flat(value)}")
        for name in self.unavailable:
            print(f"unavailable={_flat(name)}")
        ranked = _rank(self.findings)
        print(f"findings={len(ranked)}")
        for i, finding in enumerate(ranked, start=1):
            for key, value in finding.as_facts().items():
                print(f"finding.{i}.{key}={_flat(value)}")


# ---------------------------------------------------------------------------
# The window: every projection's denominator
# ---------------------------------------------------------------------------

class Window:
    """The measured window, which every extrapolation in this file divides by.

    It comes from /api/stats' own `since`/`until` — the oldest and newest RETAINED row — and not
    from an install date. Retention and the janitor evict old rows, so an install date would
    overstate the span a figure covers and therefore understate the monthly projection derived
    from it. Both directions of that error are wrong; using the measured span is the only honest
    denominator available.
    """

    def __init__(self, stats: object) -> None:
        since = _num(_get_in(stats, "since"))
        until = _num(_get_in(stats, "until"))
        self.days = 0.0
        if since > 0 and until > since:
            self.days = (until - since) / 86_400_000.0
        self.requests = int(_num(_get_in(stats, "requests")))
        self.sessions = int(_num(_get_in(stats, "sessions")))

    @property
    def projectable(self) -> bool:
        return self.days >= MIN_PROJECTION_DAYS

    def monthly(self, usd_window: float) -> float | None:
        """usd_window scaled to 30 days, or None on a window too short to scale.

        None rather than the unscaled figure: returning the window figure under a field called
        `usd_month` would relabel four hours of traffic as a month of it, which is worse than
        having no monthly figure at all.
        """
        if not self.projectable or usd_window == 0:
            return None
        return usd_window * (DAYS_PER_MONTH / self.days)


# ---------------------------------------------------------------------------
# env: the doctor-shaped header, plus the findings only configuration can produce
# ---------------------------------------------------------------------------

def collect_env(f: Fetcher, rep: Report, stats: object) -> None:
    """Where this is measured and whether it can be measured at all.

    Modelled on `claude doctor`'s header — running version, path, install method, auto-update
    state, the settings file in force — because that block is the thing that makes every line
    under it interpretable. The fields differ completely (there is no useful overlap between "is
    ripgrep working" and "is your dashboard recording"), the SHAPE is the same, and so is the rule
    that each problem carries its own one-line fix.
    """
    port, port_source = _resolve_port()
    opts, opt_sources = _configured_options()
    rep.fact("port", port or "(none)")
    rep.fact("port_source", port_source)
    # The most specific file that contributed anything, plus the file behind each option. One
    # `options_file` line could not describe two installs' files, and "which file set this" is the
    # question a reader asks the moment a value surprises them.
    rep.fact("options_file", next(iter(opt_sources.values()), "(none)"))
    for key in sorted(opt_sources):
        rep.fact(f"options_file.{key}", opt_sources[key])

    preset = str(opts.get("preset") or settings.DEFAULT_PRESET)
    strategy = str(opts.get("cache_strategy") or settings.DEFAULT_STRATEGY)
    rep.fact("preset", preset)
    rep.fact("preset_configured", "preset" in opts)
    rep.fact("cache_strategy", strategy)
    rep.fact("cache_strategy_configured", "cache_strategy" in opts)
    rep.fact("idle_exit", opts.get("idle_exit") or "24h")
    rep.fact("upstream", settings.redact_url(str(opts.get("upstream") or "")) or "(anthropic)")
    rep.fact("pipeline", ",".join(PRESET_PIPELINES.get(preset, ())) or "(none)")

    # The proxy's own liveness and version. /healthz answers without the dashboard, which is what
    # makes "up but blind" distinguishable from "down" — two states whose fixes are different, and
    # telling somebody to restart with --dashboard when nothing is running at all is a wrong answer
    # that looks like a right one.
    if port is None:
        # Nothing on disk routes this directory. Reading a port anyway is how this reported another
        # project's proxy as this account's, so the report stops here and says which question it
        # could not answer.
        rep.unavailable.append("port")
        rep.findings.append(Finding(
            "no-install", "info",
            "context-guru is not installed for this directory, so there is nothing to measure",
            "No settings file in scope configures a port and install-scope.json has no record "
            "that routes this directory. Reporting on the default port would read whichever "
            "install owns it — ports are allocated per project from 8787 upwards — and price "
            "another project's traffic as yours.",
            "Run /context-guru:install here, or run this from a directory that is installed.",
            "configuration, not a measurement"))
        return

    up = f.reachable()
    rep.fact("proxy_up", up)
    state = settings.state_dir()
    try:
        with open(os.path.join(state, "proxy-version"), encoding="utf-8") as fh:
            rep.fact("proxy_version", fh.readline().strip())
    except OSError:
        rep.fact("proxy_version", "(unknown)")

    # ROUTING, and specifically whether an exported shell variable is overriding the settings
    # files. This is doctor's "Environment variables" check with the one variable that matters
    # here: an exported ANTHROPIC_BASE_URL beats every settings file, so a project can be
    # configured perfectly and routed somewhere else entirely, and nothing else in this report
    # would say so.
    shell_base = os.environ.get("ANTHROPIC_BASE_URL", "")
    rep.fact("session_base_url", settings.redact_url(shell_base) or "(unset)")
    ours = re.match(rf"^https?://(?:127\.0\.0\.1|localhost|\[::1\]):{re.escape(port)}/anthropic/?$",
                    shell_base)
    rep.fact("session_routed_through_us", bool(ours))
    if shell_base and not ours:
        rep.findings.append(Finding(
            "routing-not-ours", "high",
            "This session's traffic is not going through your context-guru proxy",
            f"ANTHROPIC_BASE_URL is {settings.redact_url(shell_base)}, which is not "
            f"127.0.0.1:{port}/anthropic. Every figure below therefore describes traffic recorded "
            f"earlier or by another project, not what you are doing right now.",
            "Unset the exported variable and start a new session, or re-run "
            "/context-guru:install to route this project.",
            "configuration, not a measurement"))

    # THE ONE FINDING THAT GATES EVERY OTHER. Without --dashboard there is no store, so there are
    # no tools, no keep-alive ledger and no component rows — and a report that silently omitted
    # them would read as "your setup is clean".
    if stats is None:
        rep.unavailable.append("dashboard")
        if not up:
            rep.findings.append(Finding(
                "proxy-down", "high",
                "No proxy is answering, so there is nothing to read",
                f"Nothing responded on 127.0.0.1:{port}. History already recorded is still on "
                f"disk — this says the process is not running now, not that the measurements are "
                f"gone.",
                "\"${CLAUDE_PLUGIN_ROOT}/scripts/start-proxy.sh\" --unrouted, then run this again. "
                "/context-guru:status diagnoses why it stopped.",
                "configuration, not a measurement"))
        else:
            rep.findings.append(Finding(
                "dashboard-off", "high",
                "The proxy is running but recording nothing, so nothing here can be measured",
                f"/api/stats on port {port} answered {f.errors.get('/api/stats', 'nothing')} while "
                f"/healthz answered normally. The proxy forwards and saves as usual; it just keeps "
                f"no history, so no insight in this report has data behind it.",
                "Stop the proxy and start it again with "
                "\"${CLAUDE_PLUGIN_ROOT}/scripts/start-proxy.sh\" --unrouted, which passes "
                "--dashboard. Do not restart it from inside a session routed through it.",
                "configuration, not a measurement"))
        return

    window = Window(stats)
    rep.fact("window_days", round(window.days, 2))
    rep.fact("window_requests", window.requests)
    rep.fact("window_sessions", window.sessions)
    rep.fact("window_projectable", window.projectable)
    rep.fact("spent_usd", round(_num(_get_in(stats, "cost_usd")), 4))
    rep.fact("net_saved_usd", round(_num(_get_in(stats, "net_saved_usd")), 4))

    if window.requests == 0:
        rep.findings.append(Finding(
            "no-traffic", "info",
            "No requests have been recorded yet",
            "The store is empty. This is the expected state right after an install: routing "
            "applies to the NEXT session, and the cache effect only appears from a session's "
            "second turn onwards.",
            "Use Claude Code for a few sessions, then run /context-guru:insights again.",
            "no measurement yet"))
        return

    if not window.projectable:
        rep.findings.append(Finding(
            "window-too-short", "info",
            "The measured window is too short to project a monthly figure",
            f"{window.days:.1f} days of retained traffic over {window.requests:,} requests. "
            f"Window figures below are real; monthly ones are withheld rather than scaled up "
            f"from less than {MIN_PROJECTION_DAYS:g} days.",
            "Nothing to fix. The projections appear on their own once there is a week of history.",
            "no measurement yet"))

    # A pending configuration change, from the fingerprint start-proxy.sh records. Doctor's
    # equivalent is "Last update attempt"; this is the same idea applied to the thing that actually
    # decides what the proxy does — and it matters here specifically because a reader could
    # otherwise act on a finding about a preset the running proxy is not using.
    fingerprint = os.path.join(state, f"proxy-{port}.fingerprint")
    try:
        with open(fingerprint, encoding="utf-8") as fh:
            recorded = fh.read(512).strip()
        rep.fact("running_fingerprint", recorded)
        wanted_preset = re.search(r"preset=(\S+)", recorded)
        wanted_strategy = re.search(r"strategy=(\S+)", recorded)
        drifted = []
        if wanted_preset and wanted_preset.group(1) != preset:
            drifted.append(f"preset {wanted_preset.group(1)} running vs {preset} configured")
        if wanted_strategy and wanted_strategy.group(1) != strategy:
            drifted.append(f"strategy {wanted_strategy.group(1)} running vs {strategy} configured")
        if drifted:
            rep.findings.append(Finding(
                "config-pending", "medium",
                "The running proxy predates your configuration",
                "; ".join(drifted) + ". Findings below that talk about the configured value "
                "describe what the NEXT session will do, not what produced this history.",
                "Start a new session — the SessionStart hook restarts the proxy when it sees "
                "the difference. Do not restart it from inside a routed session.",
                "configuration, not a measurement"))
    except OSError:
        # Absent is not an error and not a pending change: a proxy started before this was
        # recorded, or by something else, has none. Saying "drifted" here would invent a change.
        rep.fact("running_fingerprint", "(not recorded)")

    # A pending proxy release, read from the record the SessionStart check already wrote. NEVER a
    # fetch: this command is read-only about the network too, and the release check has its own
    # cadence and its own skip state.
    try:
        with open(os.path.join(state, "update-check.yaml"), encoding="utf-8") as fh:
            text = fh.read(4096)
        fields = dict(re.findall(r"^(answer|skipped|latest):\s*(.*)$", text, re.MULTILINE))
        latest = (fields.get("latest") or "").strip()
        installed = str(rep.facts.get("proxy_version", ""))
        if (latest and latest not in ("unknown", (fields.get("skipped") or "").strip())
                and fields.get("answer", "ask").strip() != "never"
                and latest not in (installed, f"v{installed}")):
            rep.findings.append(Finding(
                "proxy-update", "low",
                f"A newer proxy release is available ({latest})",
                f"Installed {installed or '(unknown)'}, latest seen {latest}. Not a cost "
                f"finding — listed because a release can carry a measurement fix, and a stale "
                f"binary is the one thing that makes this report wrong rather than incomplete.",
                "/context-guru:update",
                "configuration, not a measurement"))
    except OSError:
        pass

    # BASH_MAX_OUTPUT_LENGTH is doctor's own environment check, and it is here for a reason doctor
    # has no interest in: it is the single knob that decides how much tool output enters the
    # transcript in the first place, and every offloading component in the pipeline exists to deal
    # with what it lets through. Reported as context for the components finding, never as a fix on
    # its own — shrinking it changes what the agent can SEE, which is a correctness decision and
    # not a cost one.
    if os.environ.get("BASH_MAX_OUTPUT_LENGTH"):
        rep.fact("bash_max_output_length", os.environ["BASH_MAX_OUTPUT_LENGTH"])


# ---------------------------------------------------------------------------
# capabilities: MCP servers, their tools, and skills you carry and never call
# ---------------------------------------------------------------------------

def collect_capabilities(f: Fetcher, rep: Report, window: Window, sessions: int = 0) -> None:
    """What you declare on every request, what you never invoke, and what dropping it is worth.

    The two figures here are different KINDS and are never added: `unused_usd` from /api/tools is
    what carrying a never-invoked declaration has ALREADY cost over the window, and
    `projected_usd` from /api/toolfilter is what removing it WOULD save going forward. The server
    keeps them apart; so does this.

    The qualification rule is the server's and it is not softened here: a removal is only offered
    for something declared in at least `min_sessions` captured sessions spanning at least
    `min_days`, and invoked in none of them. Anything below that is reported as NOT YET QUALIFIED
    with its own counts — because "we have no rows for that session" is not evidence about what
    that session did not use, and letting it stand in for one is how absence of evidence becomes
    authorisation to break somebody's agent.

    `sessions` windows the whole report to the N most recently ACTIVE sessions, and it exists for
    the one case the all-history aggregate genuinely cannot answer: three months of traffic in one
    folder with a plugin installed three weeks ago reports that plugin's skills as nearly free,
    because it is dividing their weight over ninety sessions that never declared them.
    `breakdown=session` rides along with it unconditionally rather than being a flag of its own —
    the per-session rows ARE the point of narrowing the window, and a window with no per-session
    detail is just a smaller aggregate rather than an answer about which sessions paid.
    """
    tools = f.get("/api/tools", heavy=True,
                  params={"sessions": sessions, "breakdown": "session"} if sessions else None)
    if tools is None:
        rep.unavailable.append("capabilities")
        return

    coverage = _get_in(tools, "coverage") or {}
    totals = _get_in(tools, "totals") or {}
    captured = int(_num(coverage.get("captured")))
    not_captured = int(_num(coverage.get("not_captured")))
    rep.fact("decl_sessions_captured", captured)
    rep.fact("decl_sessions_not_captured", not_captured)
    rep.fact("decl_set_tokens", int(_num(totals.get("declared_set_tokens"))))
    rep.fact("decl_unused_pct", round(_num(totals.get("unused_pct")), 1))
    rep.fact("decl_unused_reads", int(_num(totals.get("unused_reads"))))
    rep.fact("decl_unused_usd", round(_num(totals.get("unused_usd")), 4))
    rep.fact("decl_priced", bool(totals.get("priced")))
    rep.fact("system_prompt_tokens", int(_num(_get_in(tools, "prompt", "tokens"))))
    # The request-weighted mean, which is the ONLY one of the three session-length figures a
    # per-session projection may multiply by. The mean is dragged to ~4 by one-request sidechains
    # and the median to 1; the sessions where the money goes run to dozens of turns.
    rep.fact("requests_per_session_typical",
             round(_num(totals.get("requests_per_session_typical")), 1))

    # What the user ASKED for and what the account actually HAS, kept as two facts because on a
    # fresh install they differ and only one of them is a sample. `--sessions 5` against a machine
    # with one session is a report resting on one session, and labelling it with the 5 would claim
    # evidence that does not exist.
    if sessions:
        rep.fact("window_session_limit", sessions)
    rep.fact("window_sessions_analyzed", captured)

    # The decision surface, read HERE rather than after the findings below, because the per-item
    # verdict needs it: the suggestion list IS the observation floor applied — the server offers an
    # item only once it has seen it declared across `min_sessions` sessions spanning `min_days` —
    # and the inventory row carries a session count but no span, so this is the only place the
    # floor can be answered from rather than guessed at. One GET either way; the ordering of the
    # facts in the output is unchanged because Report keys insertion-order and the inventory's keys
    # all start with `item.`/`owner.`.
    doc = f.get("/api/toolfilter", heavy=True)
    if doc is not None:
        rep.fact("toolfilter_enabled", bool(_get_in(doc, "enabled")))
        rep.fact("toolfilter_reason", _get_in(doc, "reason") or "")
        rep.fact("suggest_min_sessions", int(_num(_get_in(doc, "min_sessions"))))
        rep.fact("suggest_min_days", round(_num(_get_in(doc, "min_days")), 1))
        rep.fact("suggest_withheld", int(_num(_get_in(doc, "withheld"))))

    _emit_inventory(rep, tools, _qualified_keys(doc), captured)

    if captured == 0:
        rep.unavailable.append("capabilities:no-captured-inventory")
        rep.findings.append(Finding(
            "inventory-not-captured", "info",
            "No session has recorded what it declared, so unused capabilities cannot be found",
            f"{not_captured} session(s) in the window predate inventory capture or were dropped. "
            f"They are counted separately and NOT folded into a zero: this is 'we cannot tell', "
            f"not 'nothing is unused'.",
            "Nothing to fix. Newer sessions record it automatically; re-run this after a few.",
            "no measurement yet"))
        return

    if not_captured:
        rep.fact("decl_coverage_note",
                 f"{not_captured} of {captured + not_captured} sessions have no inventory and "
                 f"contribute nothing to any figure here")

    # THE HEADLINE, AND IT MUST NOT DEPEND ON A REMOVAL QUALIFYING. The share of what every request
    # carries that is never invoked is measured the moment there is one captured session, while a
    # per-item removal needs 5 sessions across 7 days. Without this finding an account with 78.7%
    # of its prompt going unread reported nothing at all for its first week — the largest measured
    # waste in the corpus, silent, because the safety rule on ACTING was also gating TELLING.
    # Those are different thresholds and this is the one that needs no threshold.
    unused_pct = _num(totals.get("unused_pct"))
    unused_usd = _num(totals.get("unused_usd"))
    unused_tokens = int(_num(totals.get("unused_tokens")))
    priced = bool(totals.get("priced"))
    if unused_pct > 0:
        rep.findings.append(Finding(
            "declarations-unused-share", "high" if unused_pct >= 50 else "medium",
            f"{unused_pct:.0f}% of the capabilities you carry on every request were never invoked",
            f"{_tokens(unused_tokens)} of {_tokens(_num(totals.get('declared_tokens')))} tokens "
            f"per request, re-read by every turn of the session: "
            f"{_tokens(_num(totals.get('unused_reads')))} billed tokens over "
            f"{captured} captured session(s). This counts MCP tools and skills only — Claude "
            f"Code's own tools are excluded because removing one breaks the agent, and they are "
            f"the largest group by weight, so including them would make this number advice to "
            f"break your agent."
            + ("" if priced else " The models these sessions ran on have no rate on the price "
                                 "list, so there is no dollar figure — unpriced is not free."),
            "/context-guru:insights-capabilities lists them per server and per skill. Per-item "
            "removals are offered separately, once an item has been observed unused across "
            "enough sessions and days to be sure — see suggest_min_sessions/suggest_min_days.",
            "measured over the window" if priced else
            "measured in tokens; unpriced, so no dollar figure",
            usd_window=unused_usd if priced and unused_usd else None,
            usd_month=window.monthly(unused_usd) if priced and unused_usd else None))
    if not priced and int(_num(coverage.get("unpriced_sessions"))) > 0:
        rep.fact("decl_unpriced_sessions", int(_num(coverage.get("unpriced_sessions"))))
        models = [str(m.get("model")) for m in (_get_in(tools, "models") or [])
                  if isinstance(m, dict) and not m.get("priced")]
        rep.findings.append(Finding
                            ("models-unpriced", "info",
                             "Some of your traffic cannot be priced, so its waste has no dollar figure",
                             f"{int(_num(coverage.get('unpriced_sessions')))} captured session(s) ran "
                             f"on model(s) with no rate on this proxy's price list"
                             + (": " + ", ".join(sorted(set(models))[:4]) if models else "")
                             + ". Their tokens are counted in every token figure here and their "
                               "dollars are in nobody's total — counted rather than valued at zero, "
                               "because 'unknown' and 'worthless' are different claims.",
                             "Nothing to fix in the plugin. On a gateway deployment the operator "
                             "maintains that price list; token figures above are unaffected.",
                             "coverage, not a measurement"))

    # The skills half, which is the one place the inventory can be UNKNOWN rather than empty: a
    # skills listing is prose in a system-role message, so the parse can fail. `unknown` must
    # never be reported as "you declare no skills".
    skills = _get_in(tools, "skills") or {}
    state = str(skills.get("state") or "absent")
    rep.fact("skills_state", state)
    rep.fact("skills_declared", int(_num(skills.get("declared"))))
    rep.fact("skills_invoked", int(_num(skills.get("invoked"))))
    rep.fact("skills_listing_tokens", int(_num(skills.get("listing_tokens"))))
    rep.fact("skills_listing_unused_usd", round(_num(skills.get("unused_listing_usd")), 4))
    if state == "unknown":
        rep.fact("skills_unknown_sessions", int(_num(skills.get("unknown_sessions"))))
        rep.fact("skills_note", "a skills listing was present and could not be parsed; this is "
                                "not the same fact as declaring no skills")
    declared = int(_num(skills.get("declared")))
    invoked = int(_num(skills.get("invoked")))
    listing = int(_num(skills.get("listing_tokens")))
    listing_usd = _num(skills.get("unused_listing_usd"))
    if state == "ok" and declared > 0 and listing > 0 and invoked * 4 <= declared:
        # The skills listing is ONE INDIVISIBLE BLOCK of prose, so it is waste only in the sessions
        # that invoked NOTHING in it — which is why the dollar figure here is the listing's weight
        # in those sessions and not a per-skill share. Removing one skill from a listing of ninety
        # does not shrink the prompt by a ninetieth of it in any session that used another skill.
        rep.findings.append(Finding(
            "skills-listing-weight", "medium",
            f"You declare {declared} skills and invoked {invoked}; the listing costs "
            f"{_tokens(listing)} tokens on every request",
            f"The listing is prose in a system message, one block, re-read every turn. It is waste "
            f"only in sessions that invoked no skill at all, and that is what "
            f"{_usd(listing_usd)} above measures — not a per-skill share. Installing a plugin adds "
            f"its skills to this block for every project on the machine.",
            "Uninstall plugins whose skills you do not reach for, or move them to project scope so "
            "they only load where they are used: /plugin",
            "measured over the window" if listing_usd else
            "measured in tokens; unpriced, so no dollar figure",
            usd_window=listing_usd or None,
            usd_month=window.monthly(listing_usd) if listing_usd else None))

    # Per-server rollup. The SERVER is the unit, because a server is what a user adds or removes —
    # a server with three tools used between them is used, not "barely used" three separate ways.
    for row in (_get_in(tools, "servers") or []):
        if not isinstance(row, dict):
            continue
        name = str(row.get("server") or "")
        if not name:
            continue
        rep.fact(f"server.{name}.tools", int(_num(row.get("tools"))))
        rep.fact(f"server.{name}.tools_used", int(_num(row.get("tools_used"))))
        rep.fact(f"server.{name}.tokens_per_request", int(_num(row.get("tokens"))))
        rep.fact(f"server.{name}.sessions_declared", int(_num(row.get("sessions_declared"))))
        rep.fact(f"server.{name}.sessions_used", int(_num(row.get("sessions_used"))))
        rep.fact(f"server.{name}.calls", int(_num(row.get("calls"))))
        rep.fact(f"server.{name}.unused_usd", round(_num(row.get("unused_usd")), 4))
        rep.fact(f"server.{name}.priced", bool(row.get("priced")))

    # What the account already did for itself. This is a saving the product caused and it used to
    # be thrown away entirely: once a server stops being declared, no component ran and no filter
    # fired, so the reduction registered nowhere. It is reported and never added to the filter's
    # realized savings, because an account that removed a server locally AND has it in a
    # server-side filter list would otherwise be credited twice for one reduction.
    self_removed = [r for r in (_get_in(tools, "self_removed") or []) if isinstance(r, dict)]
    if self_removed:
        credited = sum(_num(r.get("avoided_usd")) for r in self_removed)
        names = ", ".join(str(r.get("server") or r.get("name")) for r in self_removed[:5])
        rep.fact("self_removed_items", len(self_removed))
        rep.fact("self_removed_usd", round(credited, 4))
        rep.findings.append(Finding(
            "self-removed-credit", "ok",
            f"You already removed {len(self_removed)} capability(ies), and it worked",
            f"{names} stopped being declared partway through the window. The requests after that "
            f"would have re-read them: {_usd(credited)} avoided over "
            f"{window.days:.1f} days. "
            + ("Some of these also appear on a server-side filter list, so the same reduction may "
               "be credited there too — reported, not netted."
               if any(r.get("overlap") for r in self_removed) else ""),
            "Nothing to fix. This is the mechanism the findings above are asking for, working.",
            "measured over the window",
            usd_window=credited, usd_month=window.monthly(credited)))

    # The rest of the decision surface: what excluding something has really saved, and what is
    # safe to offer next. `doc` was read above, where the verdict needed the floor.
    if doc is None:
        rep.unavailable.append("capability-suggestions")
        return

    realized = _get_in(doc, "realized")
    if isinstance(realized, dict) and _num(realized.get("usd")) > 0:
        rep.fact("toolfilter_realized_usd", round(_num(realized.get("usd")), 4))
        rep.fact("toolfilter_realized_reads", int(_num(realized.get("reads"))))

    suggestions = [s for s in (_get_in(doc, "suggestions") or []) if isinstance(s, dict)]
    rep.fact("suggestions", len(suggestions))

    # HOW TO ACTUALLY REMOVE IT. A Suggestion carries `remove_as`, which is the string for
    # toolfilter's own `remove` list — NOT a command a developer runs. The runnable form lives on
    # the matching inventory row as `removal`, so index those by (kind, name) and look it up.
    # Printing `remove_as` as the fix would hand somebody `skill__foo` and call it an instruction.
    removals: dict[tuple[str, str], dict] = {}
    for row in (_get_in(tools, "tools") or []) + (_get_in(tools, "skills", "skills") or []):
        if isinstance(row, dict) and isinstance(row.get("removal"), dict):
            removals[(str(row.get("kind")), str(row.get("name")))] = row["removal"]

    total_projected = 0.0
    for s in suggestions:
        name = str(s.get("name") or "")
        kind = str(s.get("kind") or "")
        removal = removals.get((kind, name)) or {}
        projected = _num(s.get("projected_usd"))
        priced = bool(s.get("priced"))
        total_projected += projected if priced else 0.0
        sessions = int(_num(s.get("sessions")))
        days = _num(s.get("days"))

        # The fix, in the order a developer can act on it: a command if there is one, otherwise the
        # settings fragment and where it goes. `effect` is carried through because some perfectly
        # valid ways to block a capability save nothing — denying a tool the model can no longer
        # call still leaves its schema in the prompt — and a fix that does not shrink the prompt
        # must say so rather than be offered as this finding's answer.
        fix = str(removal.get("command") or "")
        if not fix and removal.get("settings"):
            fix = (f"merge {removal['settings']} into "
                   f"{removal.get('settings_path') or '~/.claude/settings.json'}")
        if not fix:
            fix = (f"remove `{name}` from whatever declares it; toolfilter's own list calls it "
                   f"`{s.get('remove_as') or name}`")
        if removal.get("effect"):
            fix += f" — effect: {removal['effect']}"
        if removal.get("note"):
            fix += f" ({removal['note']})"

        rep.findings.append(Finding(
            f"unused-{kind}-{name}", "high" if projected >= 1 else "medium",
            f"{_label(kind)} `{name}` has never been invoked since it first appeared",
            f"Declared in {sessions} captured session(s) over {days:.1f} days, invoked in none of "
            f"them, and re-read by every turn of each: {_tokens(_num(s.get('unused_reads')))} "
            f"billed tokens spent carrying it. "
            + (f"Weight {_tokens(_num(s.get('tokens')))} tokens on every single request. "
               if _num(s.get("tokens")) else "")
            + (str(s.get("basis") or "")),
            fix,
            "projected from what carrying it has cost" if priced else
            "unpriced: no rate known for the models these sessions ran on",
            usd_window=projected if priced else None,
            usd_month=window.monthly(projected) if priced else None))

    if total_projected > 0:
        rep.fact("suggestions_projected_usd", round(total_projected, 4))
        # The toolfilter counterfactual, and it is a MEASURED one rather than a simulation: this
        # is the exact set of declarations the component would have withheld, priced at the tier
        # the requests that carried them were really billed at. It is the only component-level
        # "would have saved" figure in this whole report that is not an inference.
        if not _get_in(doc, "enabled"):
            rep.findings.append(Finding(
                "toolfilter-off", "high",
                "The component that stops sending unused declarations is not running",
                f"{len(suggestions)} qualified declaration(s) totalling "
                f"{_usd(total_projected)} over the window would have been withheld by "
                f"`toolfilter`. It removes nothing you invoke — the qualification rule above is "
                f"what it acts on — and it needs no per-item decision from you.",
                "/plugin configure context-guru -> preset: conservative (or medium/high/xhigh). "
                "`toolfilter` is in every tier of the picker's ladder except `off`.",
                "measured over the window",
                usd_window=total_projected, usd_month=window.monthly(total_projected)))

    if not suggestions and int(_num(_get_in(doc, "withheld"))) > 0:
        rep.findings.append(Finding(
            "suggestions-withheld", "info",
            "Some capabilities look unused but have not been observed long enough to say so",
            f"{int(_num(_get_in(doc, 'withheld')))} item(s) are unused so far but fall short of "
            f"{int(_num(_get_in(doc, 'min_sessions')))} sessions spanning "
            f"{_num(_get_in(doc, 'min_days')):.0f} days. Five sessions in one afternoon are five "
            f"sessions on one task, and the tool you did not need today is the one tomorrow needs.",
            "Nothing to fix. They qualify on their own once the span is there.",
            "deliberately withheld: insufficient observation"))


def _label(kind: str) -> str:
    return {"mcp_tool": "MCP tool", "mcp_server": "MCP server", "skill": "Skill",
            "tool": "Tool", "server_tool": "Provider tool"}.get(kind, kind or "Capability")


# The scope at which a capability can actually be removed, derived from the inventory row's own
# `removal.kind`. This is NOT cosmetic grouping: the three values are three different commands with
# three different blast radii, and picking the wrong one produces a fix that silently does nothing.
#
#   item   — removable on its own (a standalone skill, a client tool, one MCP tool denied by name)
#   server — the MCP server is the unit; denying one tool leaves the rest declared
#   plugin — the PLUGIN is the unit. `skillOverrides` has no effect on a plugin's skills, so a
#            per-skill instruction here is a setting that changes nothing. This is the case that
#            makes the field load-bearing rather than descriptive.
#   none   — not offered at any scope. Claude Code's own built-ins: the weight is real, and
#            removing one takes away a capability the model is expected to have.
_REMOVAL_SCOPE = {
    "plugin_skill": "plugin",
    "mcp_server": "server",
    "mcp_tool": "server",
    "skill": "item",
    "client_tool": "item",
    "builtin": "none",
}


def _front_matter_description(path: str) -> str:
    """The `description:` from a SKILL.md's YAML front matter, flattened to one line.

    Deliberately NOT a YAML parser: PyYAML is not in the standard library and this file's whole
    transport story is "only what python ships with". What it needs is one scalar from the top of a
    small file, and the shapes that actually occur are a plain value, a quoted value, and a folded
    block (`>` or `|`) whose continuation lines are indented — so those are the three it reads, and
    anything it cannot read it reports as absent rather than guessing.

    Flattening is not cosmetic. A description is prose somebody else wrote: it contains newlines,
    and the output format here is one fact per line, so an unflattened description would split the
    stream and every key after it would be parsed as prose.
    """
    try:
        with open(path, encoding="utf-8", errors="replace") as fh:
            # The front matter is at the top and small; reading the whole of a large SKILL.md to
            # find it would make this scale with the body rather than the header.
            head = fh.read(8192)
    except OSError:
        return ""
    if not head.startswith("---"):
        return ""
    end = head.find("\n---", 3)
    block = head[3:end] if end != -1 else head[3:]

    lines = block.split("\n")
    for i, line in enumerate(lines):
        if not line.startswith("description:"):
            continue
        value = line[len("description:"):].strip()
        # A folded or literal block: the value is the indented lines that follow.
        if value in (">", "|", ">-", "|-", ">+", "|+"):
            parts = []
            for nxt in lines[i + 1:]:
                if nxt.strip() and not nxt.startswith((" ", "\t")):
                    break
                parts.append(nxt.strip())
            value = " ".join(p for p in parts if p)
        else:
            # A plain scalar may still continue onto indented lines.
            parts = [value]
            for nxt in lines[i + 1:]:
                if not nxt.strip() or not nxt.startswith((" ", "\t")):
                    break
                parts.append(nxt.strip())
            value = " ".join(p for p in parts if p)
            if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
                value = value[1:-1]
        return " ".join(value.split())
    return ""


# How much description a reader will actually scan in a row beside three numbers. Not a hard
# truncation point — _summarize cuts at a sentence boundary and only falls back to a hard cut when
# one sentence is longer than this on its own.
DESC_SUMMARY_CHARS = 200


def _summarize(desc: str) -> str:
    """The leading sentence(s) of a description, up to roughly DESC_SUMMARY_CHARS.

    A skill's `description` is a TRIGGER written for a model — "use this when the user says X,
    skip it when Y" — and its informative part is almost always the first sentence. Measured on one
    real machine: median 278 characters, longest 1,099, with the tail being trigger phrases. In a
    table beside tokens and dollars that tail crowds out the numbers, so this keeps the front.

    Cuts on sentence boundaries so the result reads as a sentence rather than a clipped fragment,
    and drops the model-facing trigger tail wherever a description marks it explicitly.
    """
    text = " ".join(desc.split())
    if not text:
        return ""
    # An explicit trigger section is addressed to the model, never to the reader of a cost report.
    for marker in ("Triggers on:", "TRIGGER —", "TRIGGER-", "Trigger:", "Use when", "Use this when"):
        cut = text.find(marker)
        if cut > 40:
            text = text[:cut].strip()
            break
    if len(text) <= DESC_SUMMARY_CHARS:
        return text
    out = ""
    for piece in re.split(r"(?<=[.!?])\s+", text):
        if out and len(out) + 1 + len(piece) > DESC_SUMMARY_CHARS:
            break
        out = f"{out} {piece}".strip() if out else piece
    # A single sentence can be longer than the whole budget — three skills on one real machine run
    # to between 326 and 424 characters without an internal full stop — so the sentence-boundary
    # result is still cut to the budget. Cutting at a word keeps it from ending mid-token, and the
    # ellipsis is load-bearing: an invisible truncation reads as the author's own full stop.
    if len(out) > DESC_SUMMARY_CHARS:
        out = out[:DESC_SUMMARY_CHARS].rsplit(" ", 1)[0] + "…"
    return out


def _skill_descriptions() -> dict[str, str]:
    """Every skill description readable on this machine, keyed as the inventory names it.

    Two layouts, because a skill reaches the prompt through either: a plugin's own directory
    (`plugins/cache/<marketplace>/<plugin>/<version>/skills/<name>`, keyed `<plugin>:<name>` to
    match the inventory) and a marketplace's standalone skills (`plugins/marketplaces/<m>/skills/
    <name>`, keyed `<name>`).

    WHAT IS NOT HERE, and must stay absent rather than be filled in: the skills built into Claude
    Code have no SKILL.md on disk at all. There is nothing to read, so there is no description, and
    the inventory says `desc_unknown` for them. Inventing a plausible sentence would produce a line
    indistinguishable from one this function read out of a file, which is the one thing this whole
    report is built not to do.

    A newer version of the same plugin wins, because that is the one Claude Code loads.
    """
    base = os.environ.get("CLAUDE_CONFIG_DIR") or os.path.join(os.path.expanduser("~"), ".claude")
    out: dict[str, str] = {}

    cache = os.path.join(base, "plugins", "cache")
    for marketplace in _listdir(cache):
        for plugin in _listdir(os.path.join(cache, marketplace)):
            for version in sorted(_listdir(os.path.join(cache, marketplace, plugin))):
                skills = os.path.join(cache, marketplace, plugin, version, "skills")
                for name in _listdir(skills):
                    desc = _front_matter_description(os.path.join(skills, name, "SKILL.md"))
                    if desc:
                        out[f"{plugin}:{name}"] = desc

    markets = os.path.join(base, "plugins", "marketplaces")
    for marketplace in _listdir(markets):
        skills = os.path.join(markets, marketplace, "skills")
        for name in _listdir(skills):
            desc = _front_matter_description(os.path.join(skills, name, "SKILL.md"))
            if desc and name not in out:
                out[name] = desc

    return out


def _listdir(path: str) -> list[str]:
    """Directory entries, or nothing. A missing plugin cache is the normal state of a machine with
    no plugins installed, not an error worth failing a cost report over."""
    try:
        return [e for e in os.listdir(path) if not e.startswith(".")]
    except OSError:
        return []


# This plugin's own name, as it appears in a skill's `<plugin>:<name>` identifier and in the
# `claude plugin disable <plugin>` command. Here rather than inlined because it is the one string
# whose job is to keep the reporter out of its own report.
SELF_PLUGIN = "context-guru"


def _partition_self(rows: list[tuple[dict, bool]]) -> tuple[list[tuple[dict, bool]], dict]:
    """Split this plugin's own never-invoked rows out of the inventory, and total them separately.

    Matched on the `<plugin>:` prefix of a skill's name, which is how the inventory identifies a
    plugin's skills, and not on a substring of the name: a user's own skill called
    `context-guru-notes` is theirs and belongs in the report.
    """
    kept: list[tuple[dict, bool]] = []
    agg = {"items": 0, "tokens": 0, "usd": 0.0, "command": ""}
    for row, aside in rows:
        name = str(row.get("name") or "")
        if not name.startswith(SELF_PLUGIN + ":"):
            kept.append((row, aside))
            continue
        # Only the unused ones are excluded-and-counted; an invoked skill of ours was never in the
        # inventory to begin with, and reporting it as excluded would overstate the omission.
        if _num(row.get("calls")) or _num(row.get("sessions_used")):
            continue
        agg["items"] += 1
        agg["tokens"] += int(_num(row.get("tokens")))
        if row.get("priced"):
            agg["usd"] += _num(row.get("unused_usd"))
        removal = row.get("removal") if isinstance(row.get("removal"), dict) else {}
        if not agg["command"] and removal.get("command"):
            agg["command"] = str(removal["command"])
    return kept, agg


def _qualified_keys(doc: object) -> set[tuple[str, str]]:
    """The (kind, name) of every item the server has seen enough of to offer a removal for.

    THE FLOOR, ANSWERED RATHER THAN REIMPLEMENTED. The rule is "declared across at least
    `min_sessions` captured sessions spanning at least `min_days` days, invoked in none of them",
    and the second clause needs a SPAN. An inventory row carries `sessions_declared` and no dates
    at all, so a floor check written from the row can only test the session count — which passes
    for five sessions in one afternoon, the exact case the day floor exists to reject, and the
    report would read as though it had a week of evidence.
    /api/toolfilter already applies both clauses and its suggestion list is the result, so that
    list IS the answer. Keyed on (kind, name) and not on name alone because the two namespaces
    overlap: `giveinfo:blah-one` the plugin skill and a tool of the same string are different rows.

    An absent or unreadable document yields an EMPTY set, which demotes every verdict to
    `consider`. That is the safe direction and it is deliberate: with no floor to check against,
    the report must not recommend a removal, and failing the other way would hand out
    auto-selectable `remove` verdicts precisely when the evidence for them could not be read.
    """
    return {(str(s.get("kind") or ""), str(s.get("name") or ""))
            for s in (_get_in(doc, "suggestions") or []) if isinstance(s, dict)}


# The longest `why` this will emit. It is rendered in a table cell beside three numbers and read
# aloud by a model summarising the report, so a paragraph here buries the figures it exists to
# support. Enforced by truncation rather than by hope, because the session counts and call counts
# interpolated into it come from the server and nothing here bounds them.
WHY_MAX_CHARS = 159


# How long a capability must go unused, while still being carried, before this report calls it
# abandoned. Mirrors dash.StaleAfter and is pinned against it by
# TestEveryVerdictGoEmitsIsOneThePluginAlsoKnows and the equivalence test.
#
# Fourteen days, and it is a judgement rather than a measurement. Shorter catches a fortnight's
# holiday or a spell on one part of a codebase, neither of which means a tool is finished with;
# much longer and a project that changed phase a month ago still shows its old tooling as current.
STALE_AFTER_DAYS = 14.0


def _verdict(row: dict, aside: bool, scope: str, qualified: bool) -> tuple[str, str]:
    """What this report is entitled to say about one item, and the evidence it rests on.

    Four verdicts, and each boundary between them is a specific way the report could otherwise
    overstate what it measured:

      * `keep` for a built-in, because the question is closed — the weight is real and removing one
        takes away a capability the model is expected to have, so no amount of further observation
        turns it into an offer.
      * `keep` for something invoked, which is the OPPOSITE claim and therefore a different
        sentence: this one earned its place. Collapsing the two into one wording loses the only
        part a reader can act on, since one says "you may not" and the other says "you would not
        want to".
      * `remove` only past the floor. Five sessions in one afternoon are five sessions on one task.
      * `consider` otherwise: unused, removable, and observed too thinly to act on. The item is
        still listed, because the floor gates the ADVICE and never the disclosure.

    `why` states the EVIDENCE and not the verdict. "Remove this" is what the verdict field already
    says; what a reader cannot get anywhere else is the n behind it, which is the only thing that
    makes the verdict checkable.
    """
    calls = int(_num(row.get("calls")))
    used = int(_num(row.get("sessions_used")))
    declared = int(_num(row.get("sessions_declared")))

    # STALE sits between "invoked" and "never invoked", and it is the state this report could not
    # express. A user working three months in one project — planning, then development, now
    # research — has planning skills used heavily in month one and dead since. `calls > 0` filed
    # those under `keep`, next to a tool invoked a minute ago, so they were never mentioned.
    #
    # NOT solved with a session window, because a session is not a unit of time: measured on one
    # real store, spans were 0.0h, 1.1h and 99.1h, with a single session covering four days. The
    # gap between last-used and last-carried is a property of the ITEM, so it holds however
    # anybody structures their work — and it names a date, which a reader can check.
    stale_days = _stale_days(row)
    if stale_days is not None and not aside and scope != "none":
        when = _ts_date(_num(row.get("last_used_ts")))
        return "stale", (f"last used {when} — {stale_days:.0f} days carried on every request "
                         f"since, invoked in none of them")
    if calls or used:
        # Invoked. The call count is the evidence and it is quoted first, because "37 calls" settles
        # the question in a way "used in 4 sessions" does not — a capability invoked once in one
        # session is a weaker keep than one invoked constantly, and the reader can see which.
        detail = f"{calls} call(s) in {used} session(s)" if calls else f"invoked in {used} session(s)"
        return "keep", f"invoked: {detail}, so it earned the weight it carries"
    if aside or scope == "none":
        return "keep", ("built-in to Claude Code: never invoked here, but removing it takes away a "
                        "capability the agent is expected to have")
    if qualified:
        return "remove", (f"never invoked, and declared across {declared} session(s) spanning "
                          f"enough days to qualify under this proxy's own observation floor")
    return "consider", (f"never invoked, but only {declared} session(s) of evidence — below the "
                        f"floor, and sessions on one task say nothing about the next one")


def _stale_days(row: dict) -> float | None:
    """Days this capability has been carried since it was last used, or None if that is not a
    question about it.

    Both timestamps are required: without `last_used_ts` the item was never invoked, which is a
    different verdict, and without `last_carried_ts` there is nothing to measure the gap against.

    The gap runs from the last USE to the last time it was CARRIED, never to now — a report read a
    week after the traffic stopped would otherwise accuse every capability of going stale during
    the gap in which nothing happened at all.
    """
    used = _num(row.get("last_used_ts"))
    carried = _num(row.get("last_carried_ts"))
    if used <= 0 or carried <= 0:
        return None
    days = (carried - used) / 86_400_000.0
    return days if days >= STALE_AFTER_DAYS else None


def _ts_date(ms: float) -> str:
    """An epoch-ms timestamp as a date a reader recognises. Local time, because the reader is at
    this machine and comparing it against their own memory of what they were doing."""
    import datetime
    return datetime.datetime.fromtimestamp(ms / 1000.0).strftime("%-d %b %Y")


def _emit_inventory(rep: Report, tools: object, qualified: set[tuple[str, str]],
                    captured: int) -> None:
    """Name every never-invoked capability, with its weight, its cost and its removal scope.

    WHY THIS EXISTS SEPARATELY FROM THE SUGGESTIONS. /api/toolfilter withholds a removal until an
    item has been seen unused across enough sessions and days to be sure, and that rule is right:
    five sessions in one afternoon are five sessions on one task. But it is a rule about ADVICE,
    and it was being applied to DISCLOSURE — below the floor the report said `suggestions=0` and
    named nothing, so a user asking "what am I carrying and never using" got a count and no list.
    Refusing to recommend is honest. Refusing to say what is there is just unhelpful, and the
    inventory rows the answer needs were already fetched and then dropped.

    So this emits what is MEASURED (declared, never invoked, this much weight, this much spent) and
    leaves what is ADVISED to the suggestion path above. An item here is not a recommendation to
    remove it; `item.<name>.scope` says what removing it would even mean, and `none` means the
    question is closed. `item.<name>.verdict` is what the report is entitled to SAY about it, and
    the one value a caller may act on without further checks is `remove`.

    AN INVOKED ITEM GETS A VERDICT AND NOTHING ELSE. The cost columns here all mean "spent carrying
    this for nothing", and that figure does not exist for something that was used — so an invoked
    row carries `verdict=keep` and its `why`, and no weight, cost or removal command. The verdict
    is emitted for it anyway because a reader looking up a capability by name is entitled to an
    answer rather than silence, and silence here would be indistinguishable from "not declared".

    Rows are indexed by name rather than by position, because a reader greps for the item they are
    asking about and a positional index would renumber under them on the next run.
    """
    rows: list[tuple[dict, bool]] = []
    for row in (_get_in(tools, "tools") or []) + (_get_in(tools, "skills", "skills") or []):
        if isinstance(row, dict):
            rows.append((row, False))
    # `aside` is Claude Code's own tools, held apart by the server because they are the largest
    # group by weight and removing one breaks the agent. They belong in an inventory — hiding them
    # understates what the prompt costs — but never in an offer.
    for row in (_get_in(tools, "aside") or []):
        if isinstance(row, dict):
            rows.append((row, True))

    descriptions = _skill_descriptions()

    # OUR OWN WEIGHT IS NOT A SAVING WE RECOMMEND. Every skill this plugin ships is declared on
    # every request like any other, and on most machines none of them is ever invoked — so left in,
    # context-guru is one of the larger rows in its own report, and the total it tells the reader to
    # act on is inflated by the cost of producing the report. Excluded from the inventory, from
    # every owner rollup and from the removable total; NAMED, counted and priced in facts of its
    # own, because a reader who sees 44 skills in /plugin and 42 here must be able to audit the
    # difference rather than discover it. Hiding the cost would be worse than the cost.
    rows, excluded = _partition_self(rows)
    if excluded["items"]:
        rep.fact("self_excluded_plugin", SELF_PLUGIN)
        rep.fact("self_excluded_items", excluded["items"])
        rep.fact("self_excluded_tokens", excluded["tokens"])
        if excluded["usd"]:
            rep.fact("self_excluded_usd", round(excluded["usd"], 4))
        if excluded["command"]:
            rep.fact("self_excluded_command", excluded["command"])

    listed = 0
    unused_tokens = 0
    unused_usd = 0.0
    removable_window_usd = 0.0
    per_session: dict[str, dict] = {}
    for row, aside in rows:
        name = str(row.get("name") or "")
        if not name:
            continue
        removal = row.get("removal") if isinstance(row.get("removal"), dict) else {}
        scope = "none" if (aside or removal.get("danger")) else \
            _REMOVAL_SCOPE.get(str(removal.get("kind") or ""), "item")
        verdict, why = _verdict(row, aside, scope,
                                (str(row.get("kind") or ""), name) in qualified)
        pre = f"item.{name}"
        rep.fact(f"{pre}.verdict", verdict)
        rep.fact(f"{pre}.why", why[:WHY_MAX_CHARS])

        # Unused means both counters are zero. Either one above zero is a capability that earned
        # its place, and an inventory of waste that included it would be wrong about the one thing
        # it claims to measure — so an invoked item stops here, with the verdict above and none of
        # the cost columns, every one of which means "spent carrying this for nothing".
        if _num(row.get("calls")) or _num(row.get("sessions_used")):
            continue

        rep.fact(f"{pre}.kind", str(row.get("kind") or ""))
        rep.fact(f"{pre}.tokens", int(_num(row.get("tokens"))))
        rep.fact(f"{pre}.reads", int(_num(row.get("unused_reads"))))
        # Priced and unpriced are different claims and are kept apart: an item whose models have no
        # known rate carries tokens and no dollar figure, rather than a zero that reads as free.
        if row.get("priced"):
            rep.fact(f"{pre}.usd", round(_num(row.get("unused_usd")), 4))
        else:
            rep.fact(f"{pre}.unpriced", True)
        rep.fact(f"{pre}.scope", scope)
        # What the user loses by removing it. A skill's own SKILL.md is the source; a tool has no
        # equivalent file here, and the inventory does not invent one. `desc_unknown` states the
        # gap, because a missing description read as "this does nothing" is the misreading that
        # turns a cost report into bad advice.
        if str(row.get("kind")) == "skill":
            desc = descriptions.get(name, "")
            if desc:
                summary = _summarize(desc)
                rep.fact(f"{pre}.desc", summary)
                # The whole text, kept because the summary is for scanning and this is for
                # deciding. A reader about to delete something is entitled to all of it, and a
                # report that truncated with no way back would be the only copy and a lossy one.
                if summary != desc:
                    rep.fact(f"{pre}.desc_full", desc)
            else:
                rep.fact(f"{pre}.desc_unknown", True)
        if row.get("server"):
            rep.fact(f"{pre}.server", str(row.get("server")))
        if scope == "plugin" and ":" in name:
            rep.fact(f"{pre}.plugin", name.split(":", 1)[0])
        if removal.get("command"):
            rep.fact(f"{pre}.command", str(removal["command"]))

        # WHICH SESSIONS PAID FOR IT. Present only when the window was narrowed, because that is
        # the only time the server sends it. "This skill cost you $0.20" is a figure nobody can
        # check; "in these two sessions it cost $0.16 and $0.04, and you never called it" is one a
        # reader can audit against their own memory of what they were doing that day.
        #
        # `requests` is the multiplier and is carried rather than inferred: a 1,200-token tool in
        # an 80-request session cost 96,000 billed tokens and the same tool in a 2-request session
        # cost 2,400, and the per-request weight alone hides the whole of that difference.
        for srow in (row.get("sessions") or []):
            if not isinstance(srow, dict):
                continue
            sid = str(srow.get("session") or "")
            if not sid:
                continue
            rep.fact(f"{pre}.session.{sid}.requests", int(_num(srow.get("requests"))))
            rep.fact(f"{pre}.session.{sid}.reads", int(_num(srow.get("unused_reads"))))
            if srow.get("priced"):
                rep.fact(f"{pre}.session.{sid}.usd", round(_num(srow.get("unused_usd")), 4))
            bucket = per_session.setdefault(sid, {"requests": 0, "reads": 0, "usd": 0.0,
                                                  "items": 0, "model": "", "priced": True})
            # The session's own request count, which is one number per session and NOT a sum over
            # the items reporting it: every declaration that session carried was re-read by the
            # same requests, so adding them would multiply the session's length by its inventory.
            bucket["requests"] = int(_num(srow.get("requests")))
            bucket["reads"] += int(_num(srow.get("unused_reads")))
            bucket["items"] += 1
            if srow.get("priced"):
                bucket["usd"] += _num(srow.get("unused_usd"))
            else:
                bucket["priced"] = False
            if not bucket["model"] and srow.get("model"):
                bucket["model"] = str(srow.get("model"))

        listed += 1
        if scope != "none":
            unused_tokens += int(_num(row.get("tokens")))
            if row.get("priced"):
                unused_usd += _num(row.get("unused_usd"))
                removable_window_usd += _num(row.get("unused_usd"))

    # Per session, across every never-invoked item it carried. The built-in's weight IS in here,
    # because this figure answers "what did that session spend carrying things it never touched"
    # and a session's bill does not shrink because one of its declarations was unremovable. The
    # REMOVABLE subset is `savings_window_usd` below, which is a different question and is why the
    # two are separate facts rather than one reused in both places.
    for sid in sorted(per_session):
        b = per_session[sid]
        pre = f"session.{sid}"
        rep.fact(f"{pre}.requests", b["requests"])
        rep.fact(f"{pre}.unused_reads", b["reads"])
        rep.fact(f"{pre}.unused_usd", round(b["usd"], 4))
        rep.fact(f"{pre}.items", b["items"])
        if b["model"]:
            rep.fact(f"{pre}.model", b["model"])
        rep.fact(f"{pre}.priced", b["priced"])

    _emit_owners(rep, rows)

    rep.fact("inventory_items", listed)
    # The removable subtotal, which is deliberately NOT the headline `decl_unused_usd`: that figure
    # excludes built-ins entirely, while this one excludes them from the OFFER and still counts
    # them in the listing above. Two different questions, two different numbers, named apart.
    rep.fact("inventory_removable_tokens", unused_tokens)
    rep.fact("inventory_removable_usd", round(unused_usd, 4))
    rep.fact("inventory_removable_basis", "measured over the window")

    # THE PROJECT SUMMARY, and the word in it that does the work is `measured`. This is what the
    # removable set ALREADY COST over the window, not a forecast of what dropping it will save —
    # and the two differ, because the window's traffic is the only traffic anyone measured. The
    # basis says so in the one place a reader or a narrating model will look.
    #
    # The per-session average divides by the sessions ANALYZED and not by the N requested: asking
    # for the last five sessions on a machine that has one must not divide a real figure by five
    # and report a fifth of it as what a session costs.
    rep.fact("savings_window_usd", round(removable_window_usd, 4))
    if captured > 0:
        rep.fact("savings_per_session_avg_usd", round(removable_window_usd / captured, 4))
    rep.fact("savings_basis", "measured over the window")


def _emit_owners(rep: Report, rows: list[tuple[dict, bool]]) -> None:
    """Roll each never-invoked item up to the thing that would remove it.

    WHY OWNERS AND NOT JUST ITEMS. The unit of removal is not the item: a plugin's skills go
    together because `skillOverrides` does not touch them, and an MCP server's tools go together
    because there is no per-tool uninstall. So "what does this plugin cost me" is the question that
    decides anything, and leaving the reader to add rows themselves is both work and a trap — this
    report has two figures that must never be summed, and a reader in the habit of adding columns
    will eventually add those.

    AN OWNER IS NOT A NAME. `mcp__giveinfo__info` and `giveinfo:blah-one` can both exist with no
    relationship at all: the first belongs to an MCP server, the second to a plugin, and the shared
    word is a coincidence of naming. They are therefore keyed in separate namespaces
    (`owner.server.*` and `owner.plugin.*`) rather than merged on the string.

    WHAT IS NOT SUMMED HERE. These subtotals re-count tokens already counted per item, one level
    up. They are a view, not an addend: `inventory_removable_usd` is computed over ITEMS, each
    counted once, and nothing in this function contributes to it.
    """
    plugins: dict[str, dict] = {}
    servers: dict[str, dict] = {}

    for row, aside in rows:
        name = str(row.get("name") or "")
        if not name or aside:
            continue
        if _num(row.get("calls")) or _num(row.get("sessions_used")):
            continue
        removal = row.get("removal") if isinstance(row.get("removal"), dict) else {}
        if removal.get("danger"):
            continue
        kind = str(row.get("kind") or "")
        tokens = int(_num(row.get("tokens")))
        usd = _num(row.get("unused_usd")) if row.get("priced") else 0.0

        # A plugin owns an item only where the item itself says so — a `plugin_skill` removal.
        # Deriving ownership from a colon in the name alone would invent a plugin for any skill
        # that happened to contain one, and hand the reader a disable command for something that
        # does not exist.
        if str(removal.get("kind")) == "plugin_skill" and ":" in name:
            bucket = plugins.setdefault(name.split(":", 1)[0],
                                        {"skills": 0, "mcp": 0, "tokens": 0, "usd": 0.0,
                                         "unpriced": 0, "command": ""})
            bucket["skills" if kind == "skill" else "mcp"] += 1
        elif row.get("server"):
            bucket = servers.setdefault(str(row.get("server")),
                                        {"tools": 0, "tokens": 0, "usd": 0.0,
                                         "unpriced": 0, "command": ""})
            bucket["tools"] += 1
        else:
            # Owned by nothing: a standalone skill or a client tool, removable on its own. It has
            # no owner row, because an owner row implies a command that removes the group.
            continue

        bucket["tokens"] += tokens
        bucket["usd"] += usd
        if not row.get("priced"):
            bucket["unpriced"] += 1
        if not bucket["command"] and removal.get("command"):
            bucket["command"] = str(removal["command"])

    for kind, group in (("plugin", plugins), ("server", servers)):
        for owner in sorted(group):
            b = group[owner]
            pre = f"owner.{kind}.{owner}"
            if kind == "plugin":
                rep.fact(f"{pre}.skills", b["skills"])
                rep.fact(f"{pre}.mcp_tools", b["mcp"])
                rep.fact(f"{pre}.items", b["skills"] + b["mcp"])
            else:
                rep.fact(f"{pre}.tools", b["tools"])
                rep.fact(f"{pre}.items", b["tools"])
            rep.fact(f"{pre}.tokens", b["tokens"])
            rep.fact(f"{pre}.usd", round(b["usd"], 4))
            # An owner holding an unpriced child has a subtotal that is short by an unknown
            # amount. Saying how many rather than quietly reporting the partial sum, because a
            # subtotal presented as complete is the kind of number this report refuses to print.
            if b["unpriced"]:
                rep.fact(f"{pre}.unpriced_items", b["unpriced"])
            if b["command"]:
                rep.fact(f"{pre}.command", b["command"])

    rep.fact("owner_plugins", len(plugins))
    rep.fact("owner_servers", len(servers))


# ---------------------------------------------------------------------------
# idle: the gaps you leave, the cache misses they cause, and what a ping is worth
# ---------------------------------------------------------------------------

def collect_idle(f: Fetcher, rep: Report, window: Window, strategy: str) -> None:
    """Idle time, the cache expiries it causes, and the keep-alive counterfactual.

    The chain this reports, in order, because each link is a different measurement:

      1. HOW LONG you actually go idle — the gap distribution, p10/p50/p90. This is the "caused by
         the user" half and it is not a judgement: an idle gap is a developer thinking, reading a
         diff, or in a meeting.
      2. How many of the resulting cache expiries are ADDRESSABLE — i.e. a ping placed inside the
         entry's lifetime could have refreshed it. A gap longer than K pings can reach is not
         addressable at any price, and counting it in a saving would be the single easiest way to
         inflate this figure.
      3. What the pings DID, if they ran: the ledger, net of what the pings themselves cost.
      4. What they WOULD have done, if they did not: the server's own bootstrap interval, as a
         RANGE, or its refusal.

    The refusal is forwarded verbatim rather than replaced with a small number. Every account in
    the production corpus has a 90% interval whose relative half-width is at least 62% and 5 of 12
    cross zero; an account below the admission floor has no interval at all, and inventing one
    would be the exact defect the endpoint's missing point-estimate field exists to prevent.
    """
    ledger = f.get("/api/keepalive")
    if ledger is None:
        rep.unavailable.append("idle")
        return

    rep.fact("keepalive_strategy", strategy)
    rep.fact("keepalive_recorded", _num(_get_in(ledger, "keepalive_recorded_from")) > 0)
    rep.fact("keepalive_pings", int(_num(_get_in(ledger, "pings"))))
    rep.fact("keepalive_ping_usd", round(_num(_get_in(ledger, "ping_usd")), 4))
    rep.fact("keepalive_saved_usd", round(_num(_get_in(ledger, "saved_usd")), 4))
    rep.fact("keepalive_net_usd", round(_num(_get_in(ledger, "net_usd")), 4))
    rep.fact("keepalive_misses_avoided", int(_num(_get_in(ledger, "misses_avoided"))))
    rep.fact("addressable_misses", int(_num(_get_in(ledger, "addressable_misses"))))
    rep.fact("addressable_usd", round(_num(_get_in(ledger, "addressable_usd")), 4))
    rep.fact("keepalive_sessions_touched", int(_num(_get_in(ledger, "sessions_touched"))))
    rep.fact("keepalive_winners", int(_num(_get_in(ledger, "winners"))))
    rep.fact("keepalive_losers", int(_num(_get_in(ledger, "losers"))))

    net = _num(_get_in(ledger, "net_usd"))
    pings = _num(_get_in(ledger, "pings"))
    wrote = _num(_get_in(ledger, "pings_that_wrote"))
    read_nothing = _num(_get_in(ledger, "pings_that_read_nothing"))

    # The gap distribution: what "idle" actually looks like on this account.
    behaviour = f.get("/api/keepalive/behaviour")
    addressable = _num(_get_in(ledger, "addressable_misses"))
    if behaviour is not None:
        rep.fact("idle_gap_p10_hours", round(_num(_get_in(behaviour, "gap_p10")), 3))
        rep.fact("idle_gap_p50_hours", round(_num(_get_in(behaviour, "gap_p50")), 3))
        rep.fact("idle_gap_p90_hours", round(_num(_get_in(behaviour, "gap_p90")), 3))
        rep.fact("prefix_p50_tokens", int(_num(_get_in(behaviour, "prefix_p50"))))
        rep.fact("prefix_above_20k", int(_num(_get_in(behaviour, "prefix_above_20k"))))
        rep.fact("coverage_seconds", round(_num(_get_in(behaviour, "coverage_seconds")), 1))
        for band in (_get_in(behaviour, "gap_bands") or []):
            if isinstance(band, dict) and band.get("label"):
                rep.fact(f"gap_band.{_slug(str(band['label']))}.n", int(_num(band.get("n"))))
                rep.fact(f"gap_band.{_slug(str(band['label']))}.usd", round(_num(band.get("usd")), 4))
        # phantom_ttl_rows is a data-quality count, not a saving. Surfaced because a large one
        # means the expiry attribution behind every figure in this section is weaker than it looks.
        phantom = _num(_get_in(behaviour, "phantom_ttl_rows"))
        if phantom > 0:
            rep.fact("phantom_ttl_rows", int(phantom))
        if addressable == 0:
            addressable = _num(_get_in(behaviour, "addressable_misses"))
    else:
        rep.unavailable.append("idle:gap-distribution")

    # WHAT IS AT RISK RIGHT NOW. This is the one part of the report about the present rather than
    # the window, and it is the most persuasive: a session whose cache entry lapses in ninety
    # seconds has a named dollar cost attached to walking away.
    live = f.get("/api/keepalive/live")
    if live is not None:
        soon = int(_num(_get_in(live, "soon")))
        soon_usd = _num(_get_in(live, "soon_usd"))
        rep.fact("live_sessions_expiring_soon", soon)
        rep.fact("live_soon_usd", round(soon_usd, 4))
        rep.fact("live_potential_usd", round(_num(_get_in(live, "potential_usd")), 4))

    # The K ladder at the current policy's idle interval: what each number of pings would convert,
    # and what it would net. This is where "what strategy would have saved me" becomes a table
    # rather than one number.
    calc = f.get("/api/keepalive/calc")
    if calc is not None:
        rep.fact("calc_prefix_tokens", int(_num(_get_in(calc, "prefix_tokens"))))
        rep.fact("calc_prefix_source", _get_in(calc, "prefix_source") or "")
        rep.fact("calc_model", _get_in(calc, "model") or "")
        rep.fact("calc_priced", bool(_get_in(calc, "priced")))
        rep.fact("calc_ping_usd_each", round(_num(_get_in(calc, "ping_usd_each")), 6))
        rep.fact("calc_avoided_usd_each", round(_num(_get_in(calc, "avoided_usd_each")), 6))
        for row in (_get_in(calc, "rows") or []):
            if not isinstance(row, dict):
                continue
            k = int(_num(row.get("max_pings")))
            rep.fact(f"calc.k{k}.convertible_misses", int(_num(row.get("convertible_misses"))))
            rep.fact(f"calc.k{k}.net_usd", round(_num(row.get("net_usd")), 4))
            rep.fact(f"calc.k{k}.coverage_seconds", round(_num(row.get("coverage_seconds")), 1))
            if row.get("current"):
                rep.fact("calc_current_k", k)

    # The recommendation, or the refusal. Both are answers; neither is a number to smooth over.
    rec = f.get("/api/keepalive/recommend")
    if rec is None:
        rep.unavailable.append("idle:recommendation")
        return
    refused = str(_get_in(rec, "refused") or "")
    rep.fact("recommend_refused", refused)
    rep.fact("recommend_n", int(_num(_get_in(rec, "n"))))
    rep.fact("recommend_sessions", int(_num(_get_in(rec, "sessions"))))

    if refused:
        rep.findings.append(Finding(
            "keepalive-undecidable", "info",
            "Not enough idle history yet to say what keep-alive is worth on your traffic",
            f"The recommender refused: {refused}. It rests on a bootstrap over sessions, and "
            f"below its floor ({int(_num(_get_in(rec, 'n')))} addressable expiries across "
            f"{int(_num(_get_in(rec, 'sessions')))} session(s) so far) the interval it would "
            f"produce is an artefact of the resample rather than a property of your traffic. "
            f"Service-wide the same measurement lands between "
            f"{_usd(_num(_get_in(rec, 'service_lo_usd')))} and "
            f"{_usd(_num(_get_in(rec, 'service_hi_usd')))} over a comparable window — that is "
            f"scale, not your figure.",
            "Nothing to fix. The default `5-min-ping` strategy is already the shipped policy; "
            "re-run this once there is more idle history.",
            "refused: insufficient observation"))
    else:
        lo = _num(_get_in(rec, "lo_usd"))
        hi = _num(_get_in(rec, "hi_usd"))
        idle_s = int(_num(_get_in(rec, "idle_seconds")))
        k = int(_num(_get_in(rec, "max_pings")))
        rep.fact("recommend_idle_seconds", idle_s)
        rep.fact("recommend_max_pings", k)
        rep.fact("recommend_lo_usd", round(lo, 4))
        rep.fact("recommend_hi_usd", round(hi, 4))
        alt = int(_num(_get_in(rec, "alt_max_pings")))
        if alt:
            rep.fact("recommend_alt_max_pings", alt)

        if strategy == "none" and hi > 0:
            rep.findings.append(Finding(
                "keepalive-off", "high",
                "Keep-alive is off, and your idle gaps are costing you cache re-creations",
                f"{int(addressable)} cache expiry(ies) in this window were ADDRESSABLE — a ping "
                f"inside the entry's lifetime would have refreshed it instead of the next turn "
                f"paying the premium cache-WRITE rate to rebuild the whole prefix. Over a window "
                f"like this one, pinging every {idle_s}s up to {k} times is worth "
                f"{_usd(lo)}-{_usd(hi)} (90% interval over "
                f"{int(_num(_get_in(rec, 'n')))} expiries in "
                f"{int(_num(_get_in(rec, 'sessions')))} sessions). There is deliberately no single "
                f"number: the interval is too wide for one to mean anything.",
                "/plugin configure context-guru -> cache strategy: 5-min-ping. It SPENDS your own "
                "credential on the pings; the range above is already net of that. "
                "/context-guru:cache-strategy-picker explains the alternatives.",
                "90% interval over a window like this one", usd_lo=lo, usd_hi=hi))
        elif strategy != "none" and pings > 0 and net <= 0:
            worst = str(_get_in(ledger, "worst_session") or "")
            rep.findings.append(Finding(
                "keepalive-underwater", "medium",
                "Keep-alive is running and has not paid for itself on this traffic",
                f"{int(pings)} ping(s) cost "
                f"{_usd(_num(_get_in(ledger, 'ping_usd')))} and rescued "
                f"{_usd(_num(_get_in(ledger, 'saved_usd')))}: net "
                f"{_usd(net)}. "
                f"{int(_num(_get_in(ledger, 'winners')))} session(s) came out ahead and "
                f"{int(_num(_get_in(ledger, 'losers')))} behind"
                + (f", worst {worst}." if worst else ".")
                + " A negative net is a real outcome, not a measurement error.",
                "/plugin configure context-guru -> cache strategy: none, if this persists. Check "
                "it again after a week: a few long sessions with large prefixes can flip the sign.",
                "measured over the window", usd_window=net))
        elif strategy != "none" and net > 0:
            rep.findings.append(Finding(
                "keepalive-paying", "ok",
                "Keep-alive is paying for itself",
                f"{int(pings)} ping(s) prevented at most "
                f"{int(_num(_get_in(ledger, 'misses_avoided')))} cache miss(es) for a net "
                f"{_usd(net)} over {window.days:.1f} days. 'At most' is the server's own word: "
                f"the provider's cache is keyed on content, so another session sending an "
                f"identical prefix would have refreshed the same entry anyway, and that confound "
                f"cannot be measured from here.",
                "Nothing to fix.", "measured over the window",
                usd_window=net, usd_month=window.monthly(net)))

        if alt and alt != k:
            rep.findings.append(Finding(
                "keepalive-k", "low",
                f"A different ping budget may reach further on your gaps (K={alt})",
                f"Your convertible-expiry count rises by more than the bootstrap's own noise "
                f"between K={k} and K={alt}. Coverage is K x interval plus the entry lifetime, so "
                f"more pings buy REACH across a longer gap rather than a deeper discount.",
                "Only reachable from the hosted control plane today; the plugin's strategies fix "
                "K. Worth knowing before you conclude keep-alive cannot reach your gaps.",
                "measured over the window"))

    # A ping that CREATED a cache entry instead of refreshing one is money spent for nothing. It is
    # a bug signal and is reported as a problem, never folded into the saving.
    if wrote > 0:
        rep.findings.append(Finding(
            "keepalive-wrote", "high",
            "Some keep-alive pings created a cache entry instead of refreshing one",
            f"{int(wrote)} of {int(pings)} ping(s) wrote rather than read. That pays the premium "
            f"cache-write rate to cache something nothing will re-read, which is the one keep-alive "
            f"outcome with no upside at all.",
            "Report it with /context-guru:status --stats output attached. Switching cache strategy "
            "to none stops the spend while it is investigated.",
            "measured over the window"))
    if read_nothing > 0:
        rep.fact("keepalive_pings_that_read_nothing", int(read_nothing))


def _slug(text: str) -> str:
    return re.sub(r"[^a-z0-9]+", "_", text.lower()).strip("_") or "band"


# ---------------------------------------------------------------------------
# components: what earned its place, and what is switched off that would have
# ---------------------------------------------------------------------------

def collect_components(f: Fetcher, rep: Report, window: Window, preset: str) -> None:
    """Which components paid for themselves, and — the harder half — what the ones that never ran
    would have been worth.

    THE HONEST BOUNDARY, stated here because it is the thing most likely to be quietly crossed:
    you cannot measure what a component that never ran would have removed. Simulating it means
    replaying every request through it, which is a benchmark and not a report. So the OFF set is
    reported in three tiers and each is labelled on the wire:

      measured  — `toolfilter` and `cachesplit` only, because the store holds the quantity they
                  would have acted on, priced at the tier the requests really paid.
      related   — the offloaders. No figure for the component, but the measured weight of the waste
                  they target (compaction credit, the cost of the episodes compaction is already
                  paying for) is real evidence that there is something to win here.
      none      — everything else. Named, with what it does, and NO number. A study figure from
                  another corpus presented as this account's saving is how a report like this stops
                  being worth reading.

    The verdict on a component that DID run is `net_usd_with_estimate`, never `saved_usd` alone:
    judging a component that spends on its own (extract_llm) by its saving while ignoring its LLM
    bill is not a verdict, and judging its full spend against the six rows of saving that predate
    the saved_usd column is not one either.
    """
    rows = _get_in(f.get("/api/components", heavy=True), "components")
    pipeline = PRESET_PIPELINES.get(preset)
    rep.fact("preset_known", pipeline is not None)
    if rows is None:
        rep.unavailable.append("components")
        return
    rows = [r for r in rows if isinstance(r, dict)]

    ran: dict[str, dict] = {}
    for row in rows:
        name = str(row.get("component") or "")
        if not name or _num(row.get("runs")) <= 0:
            continue
        ran[name] = row
        net = _num(row.get("net_usd_with_estimate"))
        saved = _num(row.get("saved_usd")) + _num(row.get("saved_usd_estimated"))
        runs = _num(row.get("runs"))
        acted = _num(row.get("acted_tokens"))
        structural = _num(row.get("acted_structural"))
        rep.fact(f"component.{name}.runs", int(runs))
        rep.fact(f"component.{name}.acted_tokens", int(acted))
        rep.fact(f"component.{name}.acted_structural", int(structural))
        rep.fact(f"component.{name}.reverted", int(_num(row.get("reverted"))))
        rep.fact(f"component.{name}.saved_usd", round(saved, 4))
        rep.fact(f"component.{name}.net_usd", round(net, 4))
        rep.fact(f"component.{name}.replay_multiple", round(_num(row.get("replay_multiple")), 2))
        rep.fact(f"component.{name}.unpriced_rows", int(_num(row.get("saved_usd_unpriced_rows"))))

        if net < 0:
            rep.findings.append(Finding(
                f"component-underwater-{name}", "medium",
                f"`{name}` has cost more than it saved over this window",
                f"{_usd(saved)} saved against its own spend, net {_usd(net)} over "
                f"{int(runs)} run(s). "
                + (f"Its saving amortizes over later turns at "
                   f"{_num(row.get('replay_multiple')):.1f}x the first removal, so a short window "
                   f"understates it — but this window is what there is."
                   if _num(row.get("replay_multiple")) > 1 else ""),
                f"Switch to a preset without `{name}`: /plugin configure context-guru -> preset. "
                f"`conservative` has no model-spending component; `off` runs nothing.",
                "measured over the window", usd_window=net))
        elif net > 0:
            rep.findings.append(Finding(
                f"component-earning-{name}", "ok",
                f"`{name}` is earning its place",
                f"net {_usd(net)} over {int(runs)} run(s) and "
                f"{_tokens(acted)} tokens removed"
                + (f"; {int(structural)} run(s) changed the request without removing anything, "
                   f"which is what this component is for" if structural and not acted else ""),
                "Nothing to fix.", "measured over the window",
                usd_window=net, usd_month=window.monthly(net)))
        elif runs > 0 and acted == 0 and structural == 0:
            rep.findings.append(Finding(
                f"component-inert-{name}", "low",
                f"`{name}` ran {int(runs)} time(s) and never acted",
                "It is in the path, costing latency, and finding nothing to do on this traffic. "
                "That is a real answer about your traffic rather than a fault — some components "
                "only fire on shapes you do not send.",
                f"Harmless to leave. A preset without `{name}` removes the latency: "
                f"/plugin configure context-guru -> preset.",
                "measured over the window"))

    rep.fact("components_ran", ",".join(sorted(ran)) or "(none)")

    if pipeline is None:
        rep.fact("components_note",
                 f"preset `{preset}` is not one this plugin offers, so the OFF set cannot be "
                 f"listed; only what actually ran is reported")
        return

    # Tool-output and compaction weight: the measured size of the problem the offloaders exist for.
    # This is the `related` evidence and it is labelled as such everywhere it is used below.
    # This view has NO totals object: it groups by provenance, because a compaction the client did
    # and one the proxy did are different events and core refuses to average them. So the rollup is
    # done here, over `by_provenance`, and the provenance split is kept on the wire so a reader can
    # see which half they are looking at.
    episodes = f.get("/api/components/compaction-episodes")
    compaction_usd = 0.0
    if episodes is not None:
        groups = [g for g in (_get_in(episodes, "by_provenance") or []) if isinstance(g, dict)]
        n = summarizer = net = 0.0
        for g in groups:
            label = _slug(str(g.get("provenance") or "unknown"))
            rep.fact(f"compaction.{label}.episodes", int(_num(g.get("episodes"))))
            rep.fact(f"compaction.{label}.net_usd", round(_num(g.get("net_usd")), 4))
            rep.fact(f"compaction.{label}.cold_credit_usd", round(_num(g.get("cold_credit_usd")), 4))
            n += _num(g.get("episodes"))
            net += _num(g.get("net_usd"))
            summarizer += _num(g.get("summarizer_cost_usd"))
        rep.fact("compaction_episodes", int(n))
        rep.fact("compaction_net_usd", round(net, 4))
        rep.fact("compaction_summarizer_usd", round(summarizer, 4))
        # THE FIGURE THE OFFLOADER ARGUMENT RESTS ON, and it is deliberately this one rather than
        # the credits above: conversations that grew past what the cache held, hit NO compaction
        # episode at all, and paid a cold rebuild anyway. The credits measure compaction that
        # HAPPENED; this measures transcript weight nothing managed, which is the waste an
        # offloading component would have been reducing.
        no_ep = _num(_get_in(episodes, "coverage", "no_episode_cold_usd"))
        rep.fact("compaction_conversations", int(_num(_get_in(episodes, "coverage", "conversations"))))
        rep.fact("compaction_no_episode", int(_num(_get_in(episodes, "coverage", "no_episode"))))
        if no_ep > 0:
            rep.fact("compaction_no_episode_cold_usd", round(no_ep, 4))
            compaction_usd = no_ep
    else:
        rep.unavailable.append("components:compaction")

    off = [c for c in COMPONENTS if c not in pipeline and c not in ran]
    rep.fact("components_off", ",".join(sorted(off)) or "(none)")

    # `toolfilter`'s counterfactual is produced by collect_capabilities, which has the qualified
    # suggestion total. Not repeated here: two findings about one component, from two endpoints,
    # with two slightly different dollar figures is exactly the kind of disagreement this
    # report's credibility cannot survive.
    related = sorted(c for c in off if COMPONENTS[c]["counterfactual"] == "related")
    if related and compaction_usd > 0:
        rep.findings.append(Finding(
            "offloaders-off", "medium",
            "Nothing in your pipeline is managing the weight of tool output",
            f"{_usd(compaction_usd)} over this window went on rebuilding conversation prefixes "
            f"that grew past what the cache held — the waste `"
            + "`, `".join(related[:4])
            + "` exist to reduce. THIS IS NOT A PROJECTED SAVING: it is the measured size of the "
              "problem, and no honest figure exists for what these components would have removed "
              "from traffic they never saw. Simulating them means replaying every request, which "
              "is a benchmark, not a report.",
            "/plugin configure context-guru -> preset: conservative adds `extract` "
            "deterministically; `medium` adds a cheap-model relevance pass; `high`/`xhigh` add a "
            "summarizer and spend on their own. /context-guru:preset-picker explains the trade "
            "in each.",
            "measured size of the problem, NOT a projected saving"))
    elif related:
        rep.fact("offloaders_note",
                 f"{len(related)} offloading component(s) are off and there is no measured "
                 f"compaction waste in this window to argue for them")

    unmeasured = sorted(c for c in off if COMPONENTS[c]["counterfactual"] == "none")
    for name in unmeasured:
        rep.fact(f"off.{name}", COMPONENTS[name]["what"])
    if unmeasured:
        rep.fact("off_unmeasured_note",
                 "these are off and carry NO figure: nothing measured says what they would have "
                 "saved on your traffic, and a number from another corpus is not yours")


# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------

# The template `--report` fills, beside this script inside the plugin. A file rather than a string
# constant so the markup can be edited and reviewed as markup.
REPORT_TEMPLATE = "insights_report.html"

# The one placeholder in that template. Replaced with the SAME json document `--json` prints, so the
# page renders from data rather than from values interpolated into markup — descriptions are prose
# somebody else wrote, and splicing them into HTML would be an injection waiting to happen.
REPORT_PLACEHOLDER = "__CG_DATA__"


def _write_report(doc: dict) -> str:
    """Fill the template with this run's data and write it under the state directory.

    LOCAL, AND ONLY LOCAL. The page is written to this machine and opened from this machine; there
    is no upload, no publish and no remote asset. That is not a default to be relaxed later: the
    report names which tools somebody has installed and what their traffic costs, so a single remote
    font would announce this machine to whoever served it the moment the file was opened.

    The path is RETURNED rather than opened. Launching a browser out of a command somebody ran to
    read numbers is a side effect they did not ask for, and on a headless box it is an error.
    """
    template = os.path.join(os.path.dirname(os.path.abspath(__file__)), REPORT_TEMPLATE)
    try:
        with open(template, encoding="utf-8") as fh:
            html = fh.read()
    except OSError as exc:
        return f"(template unreadable: {exc})"

    # `</script>` inside the JSON would close the data island early and the rest would be parsed as
    # markup. Escaping the slash keeps it a valid JSON string and an inert one.
    payload = json.dumps(doc, sort_keys=True).replace("</", "<\\/")
    html = html.replace(REPORT_PLACEHOLDER, payload)

    out_dir = settings.state_dir()
    try:
        os.makedirs(out_dir, exist_ok=True)
        path = os.path.join(out_dir, "insights-report.html")
        with open(path, "w", encoding="utf-8") as fh:
            fh.write(html)
    except OSError as exc:
        return f"(could not write the report: {exc})"
    return path


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Read-only insights from the local context-guru proxy's own measurements.")
    parser.add_argument("area", choices=("all", "env", "capabilities", "idle", "components"),
                        help="which insight to collect; `all` is every one plus the environment")
    parser.add_argument("--json", action="store_true",
                        help="print the assembled document instead of key=value lines")
    parser.add_argument("--report", action="store_true",
                        help="also write a local HTML report and print its path; nothing is uploaded")
    # Windowing, and the reason it is a flag rather than the default: the all-history aggregate is
    # the right answer for "what does my setup cost" and the wrong one for "what is this plugin I
    # installed last week costing me", because it divides that plugin's weight over every session
    # that predates it. A user who needs the second asks for it; a user who does not keeps the
    # larger, steadier sample.
    parser.add_argument("--sessions", type=int, metavar="N", default=0,
                        help="window the capability report to the N most recently active sessions, "
                             "with the per-session breakdown of what each one paid")
    args = parser.parse_args()
    if args.sessions < 0:
        parser.error("--sessions must be positive; a negative window selects nothing")

    port, _ = _resolve_port()
    # Fetcher is built even with no port, so collect_env stays one code path; nothing is requested
    # in that case, because collect_env returns before the first read and `stats` is left None.
    f = Fetcher(port or "")
    rep = Report()
    rep.fact("area", args.area)

    # /api/stats is read first and unconditionally: it carries the window every projection divides
    # by, and its absence is the finding that gates every other one. Not requested at all when no
    # install routes this directory — a GET to a port nobody claimed is a question about somebody
    # else's proxy.
    stats = f.get("/api/stats", heavy=True) if port else None
    collect_env(f, rep, stats)
    if stats is None:
        # Nothing was measured, so there is nothing to render. A page of blanks and zeroes reads as
        # a clean bill of health, which is the single misreading this whole surface is built to
        # prevent — so the refusal and its reason ARE the output, and the gating finding above
        # already carries the command that fixes it.
        if args.report:
            rep.fact("report_refused", "nothing measured yet: no install routes this directory")
        rep.emit(args.json)
        return 0

    window = Window(stats)
    opts, _ = _configured_options()
    preset = str(opts.get("preset") or settings.DEFAULT_PRESET)
    strategy = str(opts.get("cache_strategy") or settings.DEFAULT_STRATEGY)

    # Nothing below has anything to say about an account with no retained requests, and each
    # collector would say it separately: measured against an empty store they produce
    # "no session recorded what it declared", "not enough idle history" and "no components ran" —
    # three findings that are one fact, stated three ways, on top of collect_env's own `no-traffic`.
    # Observed on a real store whose rows had just aged out under the 7-day retention, which is a
    # state any long-lived install reaches.
    if window.requests > 0:
        if args.area in ("all", "capabilities"):
            collect_capabilities(f, rep, window, args.sessions)
        if args.area in ("all", "idle"):
            collect_idle(f, rep, window, strategy)
        if args.area in ("all", "components"):
            collect_components(f, rep, window, preset)

    # Every endpoint that could not be read, by name. A report that quietly dropped a section
    # would read as a clean bill of health for the thing it could not see.
    for path, why in sorted(f.errors.items()):
        rep.fact(f"error.{path}", why)

    if not rep.findings:
        rep.fact("verdict", "nothing to fix in what could be measured")

    if args.report:
        # Stamped before rendering so the page can say when it was measured. A report read a week
        # later is a report about a week-old window, and nothing else on the page would say so.
        rep.fact("report_generated", _now_local())
        # The script's OWN absolute path, so the page can print a command that runs. Without it
        # the page printed `python3 insights.py …`, which resolves from nowhere: this file lives in
        # a versioned plugin cache directory the reader has never cd'd into, so a bare filename is
        # a command that cannot work from anywhere they are likely to be standing.
        rep.fact("report_script", os.path.abspath(__file__))
        rep.fact("report_area", args.area)
        if args.sessions:
            rep.fact("report_sessions", args.sessions)
        rep.fact("report_path", _write_report(rep.document()))

    rep.emit(args.json)
    return 0


def _now_local() -> str:
    """Local wall-clock time, to the minute. Not UTC: this is read by the person at this machine."""
    import datetime
    return datetime.datetime.now().strftime("%Y-%m-%d %H:%M")


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        sys.exit(130)
