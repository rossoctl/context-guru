#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Render paper.md to a standalone, self-contained paper.html with every figure inlined.

No external assets, no CDN: the output opens from disk offline. Reuses the research
site's stylesheet so the two read consistently.
"""
import os
import re
import sys
import html as H

HERE = os.path.dirname(os.path.abspath(__file__))
SITE_GEN = "/home/vpcuser/cg-night-0928/site/gen"
sys.path.insert(0, SITE_GEN)
# stylesheet is read from the research site generator if present; see paper2/render.py

FIGDIR = os.path.join(HERE, "figures")


def inline_figure(name):
    """Return the SVG source for a figure, stripped of any XML prolog."""
    path = os.path.join(FIGDIR, name)
    if not os.path.exists(path):
        return '<p class="cap">[figure not found: %s]</p>' % H.escape(name)
    svg = open(path, encoding="utf-8").read()
    i = svg.find("<svg")
    return svg[i:] if i >= 0 else svg


def inline_spans(t):
    """Bold, italic, code, links. Applied to already-escaped text."""
    t = re.sub(r"`([^`]+)`", r"<code>\1</code>", t)
    t = re.sub(r"\*\*([^*]+)\*\*", r"<b>\1</b>", t)
    t = re.sub(r"(?<!\*)\*([^*]+)\*(?!\*)", r"<i>\1</i>", t)
    return t


def render(md):
    out, i, lines = [], 0, md.split("\n")
    while i < len(lines):
        ln = lines[i]

        # fenced code
        if ln.startswith("```"):
            body = []
            i += 1
            while i < len(lines) and not lines[i].startswith("```"):
                body.append(lines[i]); i += 1
            i += 1
            out.append("<pre><code>%s</code></pre>" % H.escape("\n".join(body)))
            continue

        # horizontal rule
        if ln.strip() == "---":
            out.append("<hr>"); i += 1; continue

        # figure reference -> inline the SVG
        m = re.match(r"\*\*Figure (\d+) — `figures/([^`]+)`\*\*", ln)
        if m:
            out.append("<figure>" + inline_figure(m.group(2)) +
                       '<figcaption class="cap"><b>Figure %s.</b> %s</figcaption></figure>'
                       % (m.group(1), H.escape(m.group(2))))
            i += 1; continue

        # headings
        m = re.match(r"^(#{1,4})\s+(.*)$", ln)
        if m:
            lvl = len(m.group(1))
            out.append("<h%d>%s</h%d>" % (lvl, inline_spans(H.escape(m.group(2))), lvl))
            i += 1; continue

        # tables
        if ln.startswith("|") and i + 1 < len(lines) and set(lines[i + 1]) <= set("|-: "):
            hdr = [c.strip() for c in ln.strip("|").split("|")]
            i += 2
            rows = []
            while i < len(lines) and lines[i].startswith("|"):
                rows.append([c.strip() for c in lines[i].strip("|").split("|")]); i += 1
            th = "".join("<th>%s</th>" % inline_spans(H.escape(c)) for c in hdr)
            body = ""
            for r in rows:
                body += "<tr>" + "".join(
                    '<td>%s</td>' % inline_spans(H.escape(c)) for c in r) + "</tr>"
            out.append('<div class="scroll"><table><thead><tr>%s</tr></thead><tbody>%s'
                       '</tbody></table></div>' % (th, body))
            continue

        # lists
        if re.match(r"^\s*[-*]\s+", ln) or re.match(r"^\s*\d+\.\s+", ln):
            ordered = bool(re.match(r"^\s*\d+\.\s+", ln))
            items = []
            while i < len(lines) and (re.match(r"^\s*[-*]\s+", lines[i])
                                     or re.match(r"^\s*\d+\.\s+", lines[i])
                                     or (lines[i].startswith("   ") and lines[i].strip())):
                s = re.sub(r"^\s*(?:[-*]|\d+\.)\s+", "", lines[i])
                if lines[i].startswith("   ") and not re.match(r"^\s*(?:[-*]|\d+\.)\s", lines[i]) \
                        and items:
                    items[-1] += " " + s.strip()
                else:
                    items.append(s)
                i += 1
            tag = "ol" if ordered else "ul"
            out.append('<%s class="plain">%s</%s>'
                       % (tag, "".join("<li>%s</li>" % inline_spans(H.escape(x)) for x in items),
                          tag))
            continue

        # blank
        if not ln.strip():
            i += 1; continue

        # paragraph
        para = []
        while i < len(lines) and lines[i].strip() and not lines[i].startswith(("#", "|", "```")) \
                and lines[i].strip() != "---" and not re.match(r"^\s*[-*]\s+", lines[i]) \
                and not re.match(r"^\s*\d+\.\s+", lines[i]):
            para.append(lines[i].strip()); i += 1
        out.append('<p class="plain">%s</p>' % inline_spans(H.escape(" ".join(para))))

    return "\n".join(out)


def main():
    md = open(os.path.join(HERE, "paper.md"), encoding="utf-8").read()
    body = render(md)
    doc = """<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Keeping a Prompt Cache Warm Is Not a Prediction Problem</title>
<style>%s
.wrap{max-width:900px}
figure{margin:22px 0}
figcaption{margin-top:8px}
hr{border:none;border-top:1px solid var(--line);margin:34px 0}
h1{font-size:27px;margin-top:0}
h3{margin-top:26px}
</style></head>
<body><div class="wrap">
%s
<p class="meta">Regenerated from paper.md by paper2/render.py &middot; all figures inlined,
no external assets &middot; every result is replayed or observed on a read-only snapshot;
none is measured under a live control.</p>
</div></body></html>
""" % (chrome.CSS, body)
    dst = os.path.join(HERE, "paper.html")
    open(dst, "w", encoding="utf-8").write(doc)
    print("wrote %s (%d bytes)" % (dst, len(doc)))
    print("figures inlined: %d" % doc.count("<figure>"))


if __name__ == "__main__":
    main()
