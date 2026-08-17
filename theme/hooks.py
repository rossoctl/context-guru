"""MkDocs build hooks for the context-guru theme.

One job: give every generated table its own horizontal scroll container.

Python-Markdown's `tables` extension emits a bare ``<table>``. A results table
with eight numeric columns is wider than the content column on a laptop and much
wider than a phone, and an unwrapped one drags the whole page body sideways --
the single worst responsive failure a docs site can have. Styling the ``<table>``
itself with ``overflow-x: auto`` does not work: overflow needs a block container,
and making the table a block collapses its column sizing.

So wrap it here, once, for every page, instead of asking 55 Markdown files to
remember an ``md_in_html`` wrapper by hand.
"""

from __future__ import annotations

import re

# `<table` only ever starts a table tag, and Python-Markdown emits `</table>` on
# its own with no nesting (Markdown tables cannot contain tables), so a literal
# pair-wise replace is exact here -- no HTML parser required.
_OPEN = re.compile(r"<table(?=[\s>])")


def on_page_content(html: str, page, config, files) -> str:
    if "<table" not in html:
        return html
    html = _OPEN.sub('<div class="table-wrap" tabindex="0" role="region" aria-label="Scrollable table"><table', html)
    return html.replace("</table>", "</table></div>")


def _self_check() -> None:
    """Smallest thing that fails if the wrapping logic breaks."""
    out = on_page_content('<p>x</p><table class="t"><tr><td>1</td></tr></table>', None, None, None)
    assert out.count('class="table-wrap"') == 1, out
    assert out.endswith("</table></div>"), out
    assert out.startswith("<p>x</p><div class=\"table-wrap\""), out
    # two tables on one page
    two = on_page_content("<table><tr></tr></table><table><tr></tr></table>", None, None, None)
    assert two.count("table-wrap") == 2, two
    assert two.count("</div>") == 2, two
    # nothing to do
    assert on_page_content("<p>no tables</p>", None, None, None) == "<p>no tables</p>"
    print("ok")


if __name__ == "__main__":
    _self_check()
