package extract

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	starjson "go.starlark.net/lib/json"
	"go.starlark.net/resolve"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

const (
	starlarkMaxSteps = 50_000_000
	starlarkTimeout  = 2 * time.Second
	// maxSandboxInput caps the body handed to the interpreter. The step/time limits
	// only preempt at Starlark step boundaries, so a single native operation (a giant
	// string concat/repeat) can't be interrupted mid-flight — bounding the input keeps
	// those native ops proportionate. A model-written filter should never need to see
	// more than ~1 MiB; above it we fail open to the deterministic path.
	maxSandboxInput = 1 << 20 // 1 MiB
	// maxSandboxHeapGrowth bounds how much a running program may grow the process heap
	// before the watchdog cancels it — the backstop for a program that balloons memory
	// (e.g. OUTPUT = OUTPUT + OUTPUT in a loop), which the step limit does not catch.
	maxSandboxHeapGrowth = 256 << 20 // 256 MiB
	// sandboxWatchInterval is how often the watchdog samples heap use. Coarse on purpose:
	// runtime.ReadMemStats stops the world, so we keep the sampling rate low.
	sandboxWatchInterval = 200 * time.Millisecond
)

// reBuiltins are the regex helpers injected into the sandbox so a model-written
// filter can trim words/sentences/parts, not just whole lines. Backed by stdlib
// regexp (RE2: linear-time, no catastrophic backtracking, pure-Go). A bad pattern
// returns a Starlark error → the program fails → the caller falls open.
//
//	re_sub(pattern, repl, s) -> string     (regexp.ReplaceAllString)
//	re_findall(pattern, s)   -> [string]   (all non-overlapping matches)
//	re_split(pattern, s)     -> [string]   (split on every match)
//	re_match(pattern, s)     -> bool       (does s contain a match)
func reBuiltins() starlark.StringDict {
	str := func(fn func(*regexp.Regexp, string) starlark.Value, arity3 bool, name string) *starlark.Builtin {
		return starlark.NewBuiltin(name, func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
			var pat, repl, s string
			var err error
			if arity3 {
				err = starlark.UnpackArgs(b.Name(), args, kw, "pattern", &pat, "repl", &repl, "s", &s)
			} else {
				err = starlark.UnpackArgs(b.Name(), args, kw, "pattern", &pat, "s", &s)
			}
			if err != nil {
				return nil, err
			}
			re, err := regexp.Compile(pat)
			if err != nil {
				return nil, err
			}
			if arity3 {
				return starlark.String(re.ReplaceAllString(s, repl)), nil
			}
			return fn(re, s), nil
		})
	}
	toList := func(ss []string) starlark.Value {
		vs := make([]starlark.Value, len(ss))
		for i, s := range ss {
			vs[i] = starlark.String(s)
		}
		return starlark.NewList(vs)
	}
	return starlark.StringDict{
		"re_sub":     str(nil, true, "re_sub"),
		"re_findall": str(func(re *regexp.Regexp, s string) starlark.Value { return toList(re.FindAllString(s, -1)) }, false, "re_findall"),
		"re_split":   str(func(re *regexp.Regexp, s string) starlark.Value { return toList(re.Split(s, -1)) }, false, "re_split"),
		"re_match":   str(func(re *regexp.Regexp, s string) starlark.Value { return starlark.Bool(re.MatchString(s)) }, false, "re_match"),
	}
}

// runStarlark asks the model for a Starlark program whose contract is: read the
// global string INPUT (the full tool output), assign the string OUTPUT (the trimmed
// value) and optionally the string SUMMARY (a one-line digest for the marker). It
// runs sandboxed over the FULL body — no imports, no I/O, step + time limits — and
// returns (OUTPUT, SUMMARY), or ("","") on any failure (fail-open). Containment/
// sanity is verified by the caller (RunExtraction).
func runStarlark(ctx context.Context, body, goal string, keepIDs []string, model Model, rewrite, cacheContext bool, aggro Aggressiveness) (out, summary, reason string) {
	if model == nil {
		return "", "", "no model"
	}
	// Split shape: the invariant contract+examples go in a cacheable system block, the
	// goal/keep-list/output in the user message. Falls back to one message on a client
	// without the capability. Same content either way.
	sys, user := buildCodePromptSplit(body, goal, keepIDs, rewrite, cacheContext, aggro)
	src, err := completeSplit(ctx, model, sys, user)
	if err != nil {
		return "", "", "model call failed: " + err.Error()
	}
	if strings.TrimSpace(src) == "" {
		return "", "", "empty reply"
	}
	prog := stripFences(src)
	out, summary, reason = execStarlarkDetail(ctx, body, prog)
	if !strings.HasPrefix(reason, "syntax error") || !repairSyntax {
		return out, summary, reason
	}
	// ONE repair round-trip. The interpreter's message is actionable on its own
	// (file:line:col plus the token it wanted), and the failure is almost always a
	// Python-ism the model can fix in place — measured, syntax rejections were the
	// single largest cause of a code leg that bought nothing. The repair deliberately
	// does NOT resend the tool output: fixing syntax needs the program and the error,
	// not the data, so the second call costs the (cacheable) preamble plus a few
	// hundred tokens instead of the whole body again. Exactly one retry — a model that
	// cannot fix its own program with the parser's own message will not fix it on the
	// third try either, and every attempt is real money.
	repairTried.Add(1)
	fixed, err := completeSplit(ctx, model, sys, repairUserPart(prog, reason))
	if err != nil || strings.TrimSpace(fixed) == "" {
		return out, summary, reason
	}
	out2, summary2, reason2 := execStarlarkDetail(ctx, body, stripFences(fixed))
	if reason2 != "" {
		return "", "", reason + " (repair: " + reason2 + ")"
	}
	repairFixed.Add(1)
	return out2, summary2, ""
}

// repairSyntax enables the single syntax-repair round-trip. A var, not a const, so the
// diagnostic harness can measure the leg with it off — the retry is a second billed call
// and has to earn its place.
var repairSyntax = true

var repairTried, repairFixed atomic.Int64

// RepairStats reports how many syntax-repair round-trips were attempted and how many
// produced an accepted-by-the-sandbox program. tried-fixed is what the retry cost for
// nothing, and the ratio is the only honest basis for keeping it.
func RepairStats() (tried, fixed int64) { return repairTried.Load(), repairFixed.Load() }

// execStarlark runs a Starlark filter source over the body and returns OUTPUT (or ""
// on failure). Thin wrapper over execStarlarkSummary for callers/tests that don't
// care about the SUMMARY.
func execStarlark(ctx context.Context, body, src string) string {
	out, _ := execStarlarkSummary(ctx, body, src)
	return out
}

// execStarlarkSummary runs a model-written filter over the body and returns
// (OUTPUT, SUMMARY). The program is ALWAYS wrapped in a function body so top-level
// assignments are reassignable locals — this makes the natural multi-step style
// (OUTPUT = filter(...); OUTPUT = re_sub(...)) work directly, so there is no
// "cannot reassign global OUTPUT" error and no retry. OUTPUT defaults to INPUT and
// SUMMARY to "" (so a program that sets neither is a clean no-op miss). Sandboxed:
// json module + regex helpers, no imports, no I/O, step + time limits. Fail-open to
// ("","") on any error/panic.
func execStarlarkSummary(ctx context.Context, body, src string) (out, summary string) {
	out, summary, _ = execStarlarkDetail(ctx, body, src)
	return out, summary
}

// execStarlarkDetail is execStarlarkSummary plus WHY the program produced nothing.
//
// The reason has to escape this function. Collapsing a Starlark SYNTAX error into the same
// empty result as "the model never replied" is what hid the component's single largest
// failure mode: measured on real Claude Code traffic, claude-haiku-4-5 replies with a
// perfectly plausible program every time and 12 of 13 were rejected here, because the model
// writes PYTHON — generator expressions (`any(k in ln for k in ids)`), which Starlark has no
// such thing as. Every one of those surfaced as "no usable program or reply", so the prompt
// looked like it was being ignored when it was being answered and then thrown away.
func execStarlarkDetail(ctx context.Context, body, src string) (out, summary, reason string) {
	defer func() {
		if r := recover(); r != nil {
			out, summary, reason = "", "", fmt.Sprintf("sandbox panic: %v", r)
		}
	}()
	if len(body) > maxSandboxInput {
		// oversized: fail open, let the deterministic path handle it
		return "", "", "input above the sandbox limit"
	}
	ctx, cancel := context.WithTimeout(ctx, starlarkTimeout)
	defer cancel()
	thread := &starlark.Thread{Name: "extract"} // Load==nil => load() disabled
	thread.SetMaxExecutionSteps(starlarkMaxSteps)
	var startHeap uint64
	{
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		startHeap = m.HeapAlloc
	}
	done := make(chan struct{})
	go func() {
		// Cancel on timeout OR on runaway heap growth. thread.Cancel takes effect at the
		// next Starlark step boundary — enough to stop a doubling loop between iterations
		// (each iteration is a step), bounding peak memory instead of letting it OOM.
		t := time.NewTicker(sandboxWatchInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				thread.Cancel(ctx.Err().Error())
				return
			case <-done:
				return
			case <-t.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > startHeap+maxSandboxHeapGrowth {
					thread.Cancel("memory limit exceeded")
					return
				}
			}
		}
	}()
	defer close(done)

	predeclared := starlark.StringDict{
		"json":  starjson.Module,
		"INPUT": starlark.String(body),
	}
	for k, v := range reBuiltins() {
		predeclared[k] = v
	}

	// Wrap: OUTPUT/SUMMARY start as locals with safe defaults, the model's program
	// runs as the function body (any top-level assignment is a local reassignment),
	// and we return both. Indent the source one tab.
	var b strings.Builder
	b.WriteString("def _cg_main():\n\tOUTPUT = INPUT\n\tSUMMARY = \"\"\n")
	for _, ln := range strings.Split(src, "\n") {
		b.WriteString("\t")
		b.WriteString(ln)
		b.WriteString("\n")
	}
	b.WriteString("\treturn (OUTPUT, SUMMARY)\n_CG_RES = _cg_main()\n")

	globals, err := starlark.ExecFile(thread, "extract.star", b.String(), predeclared)
	if err != nil {
		return "", "", classifyExecError(src, err)
	}
	tup, ok := globals["_CG_RES"].(starlark.Tuple)
	if !ok || len(tup) != 2 {
		return "", "", "bad program result: no (OUTPUT, SUMMARY) pair"
	}
	res, ok := tup[0].(starlark.String)
	if !ok {
		return "", "", "bad program result: OUTPUT was not a string"
	}
	if sum, ok := tup[1].(starlark.String); ok {
		summary = clipSummary(string(sum))
	}
	return string(res), summary, ""
}

// maxSummaryRunes bounds the one-line digest spliced in next to the recovery marker.
//
// The prompt asks for ONE short line, and until now that was the only thing enforcing it.
// A model that answered with a paragraph did not produce a long marker — it produced NO
// compaction, because the marker-inclusive never-worse check in components/offload/marker.go
// abandons the whole splice when the result is not smaller than the original. So an
// over-long summary silently cost the call AND the reduction. Clipping keeps the reduction.
const maxSummaryRunes = 120

// clipSummary reduces a SUMMARY to one line of at most maxSummaryRunes runes. Runes, not
// bytes: cutting mid-rune would splice invalid UTF-8 into the transcript we forward.
func clipSummary(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	r := []rune(s)
	if len(r) <= maxSummaryRunes {
		return s
	}
	return strings.TrimSpace(string(r[:maxSummaryRunes-1])) + "\u2026"
}

// classifyExecError turns one interpreter error into a STABLE SLUG, because the rate of
// each cause is the only thing that says what to fix — and collapsing them is precisely
// what hid a 92% syntax-rejection rate for months. Four causes, four fixes:
//
//	truncated reply  the program is incomplete (the reply stopped at the output cap):
//	                 raise the cap or shrink the ask. Detected from the SOURCE, not from
//	                 the parser message, because a cut-off program fails with whatever
//	                 error the cut happens to produce.
//	syntax error     the model wrote Python the sandbox refuses: fix the prompt (and the
//	                 one repair round-trip retries it).
//	runtime error    the program parsed and then failed (bad regex, wrong type, step /
//	                 time / memory limit): nothing the prompt text can do reliably.
func classifyExecError(src string, err error) string {
	if incompleteProgram(src) {
		return "truncated reply: " + err.Error()
	}
	var se syntax.Error
	var rl resolve.ErrorList
	if errors.As(err, &se) || errors.As(err, &rl) {
		return "syntax error: " + err.Error()
	}
	return "runtime error: " + err.Error()
}

// incompleteProgram reports whether src stops in the middle of something — an unclosed
// bracket or an unterminated string literal. That is the signature of a reply CUT OFF at
// the output cap, which produces a partial program and, until now, presented as an
// ordinary syntax error: same slug, same "fix the prompt" conclusion, wrong fix. A
// complete program always balances, so this cannot mistake a well-formed one for a
// truncated one.
func incompleteProgram(src string) bool {
	depth := 0
	for i := 0; i < len(src); i++ {
		switch c := src[i]; c {
		case '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case '\'', '"':
			n, closed := skipString(src, i)
			if !closed {
				return true
			}
			i = n
		}
	}
	return depth > 0
}

// skipString advances past the string literal starting at i (quote char included),
// handling triple quotes, escapes and the r"" prefix. It returns the index of the closing
// quote and whether the literal was closed at all.
func skipString(src string, i int) (int, bool) {
	q := src[i]
	triple := strings.HasPrefix(src[i:], strings.Repeat(string(q), 3))
	end := string(q)
	if triple {
		end = strings.Repeat(string(q), 3)
		i += 3
	} else {
		i++
	}
	for i < len(src) {
		if src[i] == '\\' {
			i += 2
			continue
		}
		if !triple && src[i] == '\n' {
			return i, false // a single-quoted literal cannot span lines
		}
		if strings.HasPrefix(src[i:], end) {
			return i + len(end) - 1, true
		}
		i++
	}
	return i, false
}
