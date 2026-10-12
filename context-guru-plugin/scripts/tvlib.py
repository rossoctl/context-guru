"""Drawing primitives for the terminal views in tv.py. Stdlib only.

One `Env` decides how everything is drawn: colour (off for NO_COLOR and for non-terminals, so a skill's
captured output stays plain), glyph set (Unicode, or ASCII-only), width, and the light/dark ramp. Every
view must stay readable with colour off: colour only repeats what a glyph already says.
"""

from __future__ import annotations

import os
import re
import shutil
import sys
import textwrap

UNI = {"full": "█", "empty": "░", "half": "▒", "spark": "▁▂▃▄▅▆▇█", "hit": "■", "part": "▨", "miss": "✖",
       "ping": "↻", "compact": "▼", "msg": "▪", "busy": "▮", "idle": "·", "ramp": " ░▒▓█", "dot": "●",
       "dim": "◐", "off": "○", "arrow": "→", "up": "▲", "pause": "─"}
ASC = {"full": "#", "empty": "-", "half": "=", "spark": "_.,:-=+#", "hit": "o", "part": "~", "miss": "X",
       "ping": "@", "compact": "v", "msg": ".", "busy": "|", "idle": " ", "ramp": " .:*#", "dot": "*",
       "dim": "+", "off": "-", "arrow": "->", "up": "^", "pause": "-"}

# Basic 16-colour codes: the terminal's own palette adapts them to its light or dark background.
CODES = {"ok": "32", "warn": "33", "bad": "31", "info": "36", "dim": "2", "bold": "1"}
# Heat ramp as 256-colour backgrounds, pale to strong. Dark terminals fade toward black, light toward white.
HEAT = {"dark": [236, 52, 88, 160, 196], "light": [255, 224, 210, 203, 160]}
ANSI = re.compile(r"\x1b\[[0-9;]*m")


def vlen(s: str) -> int:
    return len(ANSI.sub("", s))


class Env:
    def __init__(self, color: bool | None = None, ascii_only: bool | None = None, width: int | None = None,
                 theme: str | None = None, stream=None) -> None:
        stream = stream or sys.stdout
        tty = hasattr(stream, "isatty") and stream.isatty()
        if color is None:  # https://no-color.org: any non-empty NO_COLOR wins; FORCE_COLOR asks for it
            color = False if os.environ.get("NO_COLOR") else (bool(os.environ.get("FORCE_COLOR")) or tty)
        enc = (getattr(stream, "encoding", None) or "").lower()
        if ascii_only is None:
            ascii_only = bool(os.environ.get("CG_ASCII")) or os.environ.get("TERM") == "dumb" or \
                (bool(enc) and "utf" not in enc)
        if width is None:
            width = int(os.environ["COLUMNS"]) if os.environ.get("COLUMNS", "").isdigit() else \
                shutil.get_terminal_size((80, 24)).columns
        self.color, self.ascii = color, ascii_only
        self.width = max(40, min(width, 140))
        self.theme = theme or os.environ.get("CG_THEME") or ("light" if os.environ.get("COLORFGBG", "").endswith(";15") else "dark")
        self.g = ASC if ascii_only else UNI

    # colour -------------------------------------------------------------------------------
    def paint(self, kind: str, s: str) -> str:
        return f"\x1b[{CODES[kind]}m{s}\x1b[0m" if self.color and s else s

    def heat(self, level: int, s: str) -> str:
        """`s` on the heat ramp, level 0..4. Colour off: the density glyph already carries the level."""
        if not self.color:
            return s
        return f"\x1b[48;5;{HEAT[self.theme][level]}m{s}\x1b[0m"

    # shapes -------------------------------------------------------------------------------
    def bar(self, frac: float, n: int = 12, kind: str | None = None, hatch: bool = False) -> str:
        """A fill bar. `hatch` marks an estimate: same length, different texture, so it is never mistaken for observed."""
        k = max(0, min(n, round(max(0.0, min(1.0, frac)) * n)))
        s = self.g["half" if hatch else "full"] * k + self.g["empty"] * (n - k)
        return self.paint(kind, s) if kind else s

    def spark(self, vals: list[float]) -> str:
        sp, top = self.g["spark"], max(vals) if vals else 0
        return "".join(sp[min(7, int(v / top * 7.999))] if top > 0 and v > 0 else sp[0] for v in vals)

    def stack(self, parts: list[tuple[float, str]], n: int) -> str:
        """Stacked bar: parts are (fraction, glyph). Rounded so the total is exactly n cells."""
        out, used, acc = "", 0, 0.0
        for frac, ch in parts:
            acc += frac
            end = round(min(acc, 1.0) * n)
            out += ch * max(0, end - used)
            used = max(used, end)
        return out + self.g["empty"] * (n - used)

    def heat_cell(self, v: float, top: float, w: int = 2) -> str:
        """One heat-map cell, w chars wide: ramp glyph (always) + background colour (when on)."""
        lv = 0 if v <= 0 or top <= 0 else min(4, 1 + int(v / top * 3.999))
        ch = self.g["ramp"][min(lv, 4)]
        return self.heat(lv, " " * w) if self.color else ch * w

    def rule(self, label: str = "") -> str:
        line = self.g["pause"] * (self.width - 1)
        return self.paint("dim", (f"{label} " + line)[:self.width - 1] if label else line)


def usd(x: float) -> str:
    x = float(x or 0)
    s = "-" if x < 0 else ""
    x = abs(x)
    return s + (f"${x:,.0f}" if x >= 100 else f"${x:,.2f}" if x >= 0.005 else "$0")


def dur(sec: float) -> str:
    sec = int(max(sec, 0))
    return f"{sec // 3600}h{sec % 3600 // 60:02d}m" if sec >= 3600 else f"{sec // 60}:{sec % 60:02d}"


def span(sec: float) -> str:
    """A length of time in words that cannot be mistaken for a clock: 45s, 12m, 1h20m."""
    sec = int(max(sec, 0))
    return f"{sec}s" if sec < 60 else f"{sec // 60}m" if sec < 3600 else f"{sec // 3600}h{sec % 3600 // 60:02d}m"


def clip(s: str, width: int, e: Env) -> str:
    """Cut a plain string to width with an ellipsis (ANSI-free input only)."""
    return s if len(s) <= width else s[:max(0, width - 1)] + ("~" if e.ascii else "…")


def fit(lines: list[str], e: Env) -> list[str]:
    """Word-wrap any line wider than the terminal (colour dropped on those lines only), continuation indented."""
    out = []
    for ln in lines:
        if vlen(ln) <= e.width - 1:
            out.append(ln)
            continue
        plain = ANSI.sub("", ln)
        ind = " " * (len(plain) - len(plain.lstrip()))
        out += textwrap.wrap(plain, e.width - 1, subsequent_indent=ind, break_long_words=True) or [""]
    return out


def cols(rows: list[list[str]], aligns: str, gap: int = 2, maxw: int = 0) -> list[str]:
    """Aligned columns measured on visible width, so coloured cells line up. With maxw, the first column
    is cut with an ellipsis until the table fits."""
    if not rows:
        return []
    w = [max(vlen(r[i]) for r in rows) for i in range(len(rows[0]))]
    over = sum(w) + gap * (len(w) - 1) - maxw
    if maxw and over > 0 and w[0] - over >= 8:
        w[0] -= over
        rows = [[c[:w[0] - 1] + "…" if i == 0 and len(c) > w[0] else c for i, c in enumerate(r)] for r in rows]
    pad = lambda c, n, a: c + " " * (n - vlen(c)) if a == "l" else " " * (n - vlen(c)) + c  # noqa: E731
    return [(" " * gap).join(pad(c, n, a) for c, n, a in zip(r, w, aligns)).rstrip() for r in rows]
