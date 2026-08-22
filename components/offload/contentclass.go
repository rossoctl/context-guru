package offload

import "regexp"

// Content classes, and the compression ratio each one ACHIEVED on real traffic.
//
// The economic gate prices a candidate with one pooled compression ratio, learned across
// every kind of tool output at once. MEASURED over 9,763 captured messages from the two
// tenants running this component, that pooling is the reason the gate could not tell a
// paying call from a losing one:
//
//	ls -l listing         65.5%      JSON blob             2.8%
//	prose / other         41.0%      test/eval result log  1.5%
//	markdown doc          36.2%      grep -n / rg output   6.7%
//	multi-file bundle     34.6%      ANSI CLI output       8.0%
//	Read w/ line numbers  29.2%      YAML config          26.1%
//	source code           29.9%
//
// A 23x spread, and JSON plus ANSI CLI output are 31% of the reachable token mass in the two
// classes that compress WORST. That is the direct cause of the flat size-versus-yield relation
// the corpus shows (r = -0.10 between candidate size and reduction ratio): the biggest messages
// in this workload are logs and JSON, so raising a size floor selects for the material that
// cannot pay. Sizing cannot fix a mix problem; only looking at the content can.
//
// So the class's own measured ratio is handed to the gate in place of the pooled one, and the
// existing expected-saving-versus-call-cost comparison does the rest. No new threshold, no new
// gate: at the measured $0.0035 call cost a 1,600-token prose candidate clears break-even and a
// 23,000-token JSON blob does not — and 23,000 is above this workload's 7,399-token ceiling, so
// JSON is refused for the whole corpus, which is the correct answer.
//
// Ratios are deliberately the OBSERVED ones, not targets. Where a class is unrecognised the
// pooled learned ratio governs, unchanged.
//
// ponytail: head-sniffing regexes over the first 1,600 bytes, ported from the analysis script
// that produced the table above so the classes and the numbers cannot drift apart. Upgrade to
// per-class learned tracking only if a measurement shows a class's real ratio has moved.
var contentClasses = []struct {
	name  string
	ratio float64
	// all patterns must match for the class to be chosen
	all []*regexp.Regexp
}{
	{"multi_file_bundle", 0.346, res(`(?m)^(===|##########|########|#{4,} )`, `\.(md|yaml|yml|json|env|go|py|sh)\b`)},
	{"read_with_line_numbers", 0.292, res(`(?m)^\s*\d+[:\t]`)},
	{"grep_output", 0.067, res(`(?m)^[\w./-]+\.\w{1,5}:\d+:`)},
	{"ls_listing", 0.655, res(`(?m)^(total \d+|d[rwx-]{9})`)},
	{"test_result_log", 0.015, res(`\b(PASS|FAIL|ok\s|FAILED|Traceback|assert)\b`, `(?i)\b(test|task|trial)\b`)},
	{"markdown_doc", 0.362, res(`(?m)^(\s*[-*]\s|#{1,3}\s)`)},
	{"json_blob", 0.028, res(`(?s)^\s*[{\[]|(?m)^\s*"\w+":`)},
	{"yaml_config", 0.261, res(`(?m)^\s*\w[\w.-]*:\s`, `(?m)^(apiVersion|kind|metadata|pipeline|components):`)},
	{"ansi_cli_output", 0.080, res(`\x1b\[|\[0m`)},
	{"source_code", 0.299, res(`(?m)^(#!/|package |import |func |def |class )`)},
}

func res(pats ...string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(pats))
	for i, p := range pats {
		out[i] = regexp.MustCompile(p)
	}
	return out
}

// minWindowRatio is the class reduction below which the model-free character WINDOW is not a
// faithful reduction of the content and must not be offered at all.
//
// The window keeps a fixed budget of characters, so on a 16,000-character body it removes ~75%
// whatever the body is. A class whose measured reduction is 6.7% cannot support that: the elided
// part is not redundant with the kept part, it is different facts. MEASURED on a live cold sweep
// through the proxy: a 6,906-token `grep -n` result of Go function signatures came back as its
// first 37 of 158 lines with "22,081 characters elided" — an honest marker on a useless result,
// and one the production analysis had judged a CORRECT rejection when the same shape appeared
// there. Marking a cut stops it being a false statement; it does not make it an extraction.
//
// 0.25 separates the classes cleanly rather than by tuning: everything at or above it (YAML 26.1%,
// Read 29.2%, source 29.9%, bundle 34.6%, markdown 36.2%, ls -l 65.5%) is genuinely redundant
// content where a window is a real reduction, and everything below (ANSI 8.0%, grep 6.7%, JSON
// 2.8%, test logs 1.5%) is fact-dense. That is the same four classes the economic gate refuses on
// expected yield — one measurement answering two questions, which is the reason to trust it.
//
// It matters even though the gate already refuses those four, because the gate can be overridden:
// the exploration budget (2 calls per session) and `economic_gate: false` both reach the extractor
// with the gate's verdict ignored, and that is exactly where this was found.
const minWindowRatio = 0.25

// classHeadBytes is how much of a candidate is sniffed. The shape of a tool output is set by
// its first few lines; scanning more costs time on every candidate of every request and, at the
// sizes this component sees, changed no verdict in the corpus.
const classHeadBytes = 1600

// contentClass names the content class of a tool output and the compression ratio that class
// achieved in production. ok is false when nothing matched, and the caller must then keep using
// the pooled learned ratio — an unrecognised shape is not evidence of a bad one.
//
// Order matters and mirrors the analysis script: a line-numbered Read of a JSON file is a Read,
// a grep hit inside a bundle is a bundle.
func contentClass(content string) (name string, ratio float64, ok bool) {
	head := content
	if len(head) > classHeadBytes {
		head = head[:classHeadBytes]
	}
	// The bundle and multi-file tests look at the very start for a filename, matching the
	// script that measured them.
	short := head
	if len(short) > 400 {
		short = short[:400]
	}
	for _, cl := range contentClasses {
		matched := true
		for i, re := range cl.all {
			hay := head
			if cl.name == "multi_file_bundle" && i == 1 {
				hay = short
			}
			if !re.MatchString(hay) {
				matched = false
				break
			}
		}
		if matched {
			return cl.name, cl.ratio, true
		}
	}
	return "", 0, false
}
