#!/usr/bin/env python3
"""Classify EVERY heredoc in the scenario arms, and compile the ones that feed python3.

`bash -n` treats a heredoc as opaque text, so a syntax error inside one is invisible until the
interpreter runs it — which for these arms is after an hour of paid gateway time.

# THE ASSERTION IS OVER THE POPULATION, NOT OVER WHAT THE MATCHER FOUND

This is the fourth version of this check. Each of the first three looked complete and each had a hole,
and the first two shared one cause — they counted the heredocs their own pattern liked, and reported
success on the count:

  1. Matched the literal tag `PY`. An arm using `<<'EOF' | python3` was skipped in silence.
  2. Keyed on the python3 invocation instead, which fixed that instance and left the shape intact:
     a hyphenated tag (`<<'PY-BODY'`, legal bash) fell out of the tag character class, and a line
     continuation between `python3` and the `<<` put them on different physical lines. Both dropped
     6 blocks to 5 and printed it as a success.

`found == 0` cannot see 6 → 5. That is the shape that ships, because everything still says ok.

  3. Enumerated every heredoc, which closed the missing direction and opened the opposite one:
     classification word-searched the whole line and nothing stripped a trailing comment, so
     `cat > x.yaml <<YAML   # not python3` was filed as python. It could no longer MISS a heredoc; it
     could MIS-FILE one, and on a body that happens to be valid Python it did so silently while
     printing a census that was wrong in the same run. See classify().

So this enumerates every heredoc redirection in these files and requires each one to be CLASSIFIED by
the COMMAND WORD that owns it:

  - python  -> compile the body, and a syntax error fails
  - cat/tee -> a data file (lib.sh writes config.yaml this way); allowed, not compiled
  - anything else -> FAILURE, named as unclassified

An unmatched tag, a continuation, a new interpreter, a heredoc nobody thought about: all surface as
"I could not classify this" rather than as absence, which is what versions 1 and 2 reported — and none
of them can be mis-filed by a comment, which is what version 3 did.
"""

import glob
import os
import py_compile
import re
import sys
import tempfile

# A heredoc redirection. The tag may be quoted or bare; `<<-` strips leading tabs from the terminator.
# The tag class is deliberately permissive — hyphens and dots are legal, and being narrow here is
# exactly how version 2 lost a block. `<<<` (herestring) is excluded: it carries no body.
REDIR = re.compile(r"<<(-?)\s*(?!<)(?:'([^']+)'|\"([^\"]+)\"|([A-Za-z_][\w.-]*))")


def logical_lines(lines):
    """Join backslash continuations, yielding (text, first_physical_index, last_physical_index).

    Version 2 required `python3` and the `<<` on the same PHYSICAL line, so splitting them across a
    continuation hid the block. These invocation lines are already long enough that wrapping one is
    the natural next edit.
    """
    i = 0
    while i < len(lines):
        start = i
        text = lines[i]
        while text.endswith("\\") and i + 1 < len(lines):
            i += 1
            text = text[:-1] + " " + lines[i]
        yield text, start, i
        i += 1


def is_comment(text):
    return text.lstrip().startswith("#")


# `VAR=value` env prefixes, which precede the command word.
ASSIGN = re.compile(r"^[A-Za-z_]\w*=")


def strip_comment(text):
    """Remove a trailing shell comment, leaving quoted # alone."""
    out, quote = [], None
    for ch in text:
        if quote:
            out.append(ch)
            if ch == quote:
                quote = None
            continue
        if ch in "'\"":
            quote = ch
            out.append(ch)
            continue
        if ch == "#":
            break
        out.append(ch)
    return "".join(out)


def classify(text):
    """What consumes this heredoc's body? None means 'cannot tell'.

    KEYED ON THE COMMAND WORD, NOT ON A WORD-SEARCH OF THE LINE, and that distinction is a defect this
    function shipped with. Version 3 asked whether "python" appeared anywhere in the logical line, and
    nothing stripped a trailing comment — so

        cat > "$d/config.yaml" <<YAML   # not python3

    classified as PYTHON. Two outcomes, both wrong and one silent: our real config body fails to
    compile and the run aborts blaming a python syntax error in a YAML file, and a body that happens to
    be valid Python (`key: value` is an annotated assignment) compiles clean and is counted in the
    "python compiled" total. The partition line added so nobody has to re-derive the census was then
    wrong in the same run that printed it.

    Version 3 could no longer MISS a heredoc; it could MIS-FILE one. Same family, opposite direction.

    So: strip comments, take the segment that actually owns the redirection (a heredoc on `a | b <<T`
    belongs to `b`), skip `VAR=value` prefixes, and read the command word. Anything unrecognised is
    unclassified rather than guessed at.
    """
    before = strip_comment(text).split("<<")[0]
    # The heredoc attaches to the last command in a pipeline or list.
    segment = re.split(r"\|\||&&|[|;]", before)[-1]
    for token in segment.split():
        if ASSIGN.match(token) or token in ("!", "time", "exec", "env", "command", "then", "do", "else"):
            continue
        base = os.path.basename(token.strip("\"'()"))
        if re.fullmatch(r"python3?(\.\d+)?", base):
            return "python"
        if base in ("cat", "tee"):
            return "data"
        return None
    return None


def heredocs(path):
    """Yield (kind, tag, body, physical_line, text, quoted) for every heredoc in path.

    body is None when the terminator is never found. quoted says whether the TAG was quoted, which is
    what decides whether the shell expands the body before its consumer sees it — read off the match
    rather than re-sniffed from the text, which is how the previous version got it wrong for `<<-`.
    """
    lines = open(path, encoding="utf-8").read().splitlines()
    consumed_to = -1
    for text, first, last in logical_lines(lines):
        if last <= consumed_to or is_comment(text):
            continue
        for m in REDIR.finditer(text):
            dash = m.group(1)
            tag = m.group(2) or m.group(3) or m.group(4)
            body, j = [], last + 1
            found_end = False
            while j < len(lines):
                candidate = lines[j].lstrip("\t") if dash else lines[j]
                if candidate.strip() == tag:
                    found_end = True
                    break
                body.append(lines[j])
                j += 1
            consumed_to = j
            quoted = bool(m.group(2) or m.group(3))
            yield (classify(text), tag, "\n".join(body) if found_end else None, first + 1, text, quoted)


def compile_body(body):
    tmp = None
    try:
        with tempfile.NamedTemporaryFile("w", suffix=".py", delete=False, encoding="utf-8") as f:
            f.write(body)
            tmp = f.name
        py_compile.compile(tmp, doraise=True, cfile=tmp + "c")
        return None
    except py_compile.PyCompileError as e:
        return str(e)
    finally:
        for p in (tmp, tmp and tmp + "c"):
            if p and os.path.exists(p):
                os.unlink(p)


def main(root):
    paths = sorted(glob.glob(os.path.join(root, "*.sh")))
    if not paths:
        print("no .sh files under %s — nothing to check is itself a failure" % root)
        return 1

    compiled = data = bad = 0
    for path in paths:
        name = os.path.basename(path)
        for kind, tag, body, line, text, quoted in heredocs(path):
            if body is None:
                print("UNTERMINATED: %s:%d heredoc <<%s never closed" % (name, line, tag))
                bad += 1
                continue
            if kind is None:
                # THE POINT OF THE REWRITE. Not skipped, not counted as fine: named and failed.
                print("UNCLASSIFIED: %s:%d heredoc <<%s — cannot tell what consumes this body.\n"
                      "  %s\n"
                      "  If it is python, make the invocation recognisable; if it is data, pipe it "
                      "through cat; otherwise teach classify() about it." % (name, line, tag, text.strip()))
                bad += 1
                continue
            if kind == "data":
                data += 1
                continue
            if not quoted:
                print("WARNING: %s:%d feeds python an UNQUOTED heredoc (<<%s); the shell expands it "
                      "first, so this check sees different text than python will" % (name, line, tag))
            err = compile_body(body)
            if err:
                print("SYNTAX (python): %s:%d\n  %s" % (name, line, err))
                bad += 1
            else:
                compiled += 1

    total = compiled + data + bad
    if total == 0:
        print("NO heredocs found under %s. Every arm reads its results through one, so this means the "
              "scanner has stopped matching — not that there is nothing to check." % root)
        return 1
    if compiled == 0:
        print("NO python heredocs found among %d heredoc(s). Every arm reads its results in python, so "
              "this means classification has broken, not that there is nothing to compile." % total)
        return 1
    print("heredocs: %d python compiled, %d data, %d problem(s)" % (compiled, data, bad))
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1] if len(sys.argv) > 1 else os.path.dirname(os.path.abspath(__file__))))
