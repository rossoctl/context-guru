//go:build cg_skeleton

// Local measurement of the skeleton component against captures of real agent traffic.
// Not an assertion and not part of CI — skeleton is LOCAL-ONLY and evaluation-only (see
// docs/components/skeleton.md), and the point of this file is to answer "what would it
// actually remove, from what, and what would the agent no longer know" with numbers
// instead of intuition.
//
//	CG_SKEL_CAPTURE=/home/vpcuser/cg-research/prod/bodies.jsonl \
//	  CGO_ENABLED=1 go test -tags cg_skeleton ./components/offload -run SkeletonCapture -v
//
// Several captures may be given, colon-separated. CG_SKEL_SAMPLE=1 also prints a
// before/after of the largest reduced dump, which is the only honest way to review what
// a lossy transform costs.
//
// Two figures, because they answer different questions:
//
//   - PER DUMP: over every unique (tool call, output) pair in the capture, what does the
//     component remove from the file-dump class. This is the size of the lever.
//   - PER REQUEST: the last (largest) transcript in the capture, rebuilt faithfully and
//     run through Offload once, with the cache-tail gate the proxy actually applies.
//     This is what a live turn would see, and it is much smaller than the lever.

package offload

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// dumpPair is one (producing call, output) pair from a capture.
type dumpPair struct {
	name string // tool name: Read, Bash, …
	args string // raw JSON arguments
	text string // the tool_result text
}

// capturePairs returns the deduplicated (call, output) pairs in a capture, in first-seen
// order. A capture line is a whole request body, so the same tool result recurs on every
// later turn; dedup keys on (tool_use_id, text) so a re-sent output counts once.
func capturePairs(t *testing.T, file string) []dumpPair {
	t.Helper()
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	seen := map[string]bool{}
	var out []dumpPair
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 256<<20)
	for sc.Scan() {
		calls := map[string]dumpPair{}
		gjson.GetBytes(sc.Bytes(), "body.messages.#.content|@flatten").ForEach(func(_, blk gjson.Result) bool {
			switch blk.Get("type").String() {
			case "tool_use":
				calls[blk.Get("id").String()] = dumpPair{name: blk.Get("name").String(), args: blk.Get("input").Raw}
			case "tool_result":
				id := blk.Get("tool_use_id").String()
				txt := blockText(blk)
				if txt == "" || seen[id+"\x00"+txt] {
					return true
				}
				seen[id+"\x00"+txt] = true
				p := calls[id]
				p.text = txt
				out = append(out, p)
			}
			return true
		})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// blockText renders a tool_result's content, which is either a string or an array of
// text parts.
func blockText(blk gjson.Result) string {
	c := blk.Get("content")
	if !c.IsArray() {
		return c.String()
	}
	var b strings.Builder
	c.ForEach(func(_, p gjson.Result) bool { b.WriteString(p.Get("text").String()); return true })
	return b.String()
}

// pairReq builds the request shape one pair produces, plus a synthetic LATER read of the
// same path so the newest-read guard (which protects the agent's current picture of a
// file) does not mask the size of the lever. That guard is measured separately, per
// request, where it belongs.
func pairReq(p dumpPair) *bschemas.BifrostChatRequest {
	id, name := "m0", p.name
	tc := bschemas.ChatAssistantMessageToolCall{ID: &id,
		Function: bschemas.ChatAssistantMessageToolCallFunction{Name: &name, Arguments: p.args}}
	res := tool(p.text)
	res.ChatToolMessage = &bschemas.ChatToolMessage{ToolCallID: &id}
	later := "m1"
	tc2 := tc
	tc2.ID = &later
	res2 := tool("(later read of the same path)")
	res2.ChatToolMessage = &bschemas.ChatToolMessage{ToolCallID: &later}
	return &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		{Role: bschemas.ChatMessageRoleAssistant, ChatAssistantMessage: &bschemas.ChatAssistantMessage{ToolCalls: []bschemas.ChatAssistantMessageToolCall{tc}}},
		res,
		{Role: bschemas.ChatMessageRoleAssistant, ChatAssistantMessage: &bschemas.ChatAssistantMessage{ToolCalls: []bschemas.ChatAssistantMessageToolCall{tc2}}},
		res2,
	}}
}

// rebuildLast reconstructs the LAST capture line as a request: assistant tool_use blocks
// become ToolCalls, tool_result blocks become tool messages, everything else becomes a
// plain text message. Positions are preserved, which is what makes the cache-tail gate
// and the newest-read guard meaningful.
func rebuildLast(t *testing.T, file string) *bschemas.BifrostChatRequest {
	t.Helper()
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 256<<20)
	var last []byte
	for sc.Scan() {
		if len(sc.Bytes()) > len(last) {
			last = append(last[:0], sc.Bytes()...)
		}
	}
	req := &bschemas.BifrostChatRequest{}
	gjson.GetBytes(last, "body.messages").ForEach(func(_, msg gjson.Result) bool {
		role := msg.Get("role").String()
		var calls []bschemas.ChatAssistantMessageToolCall
		var results []bschemas.ChatMessage
		var text strings.Builder
		msg.Get("content").ForEach(func(_, blk gjson.Result) bool {
			switch blk.Get("type").String() {
			case "tool_use":
				id, name := blk.Get("id").String(), blk.Get("name").String()
				calls = append(calls, bschemas.ChatAssistantMessageToolCall{ID: &id,
					Function: bschemas.ChatAssistantMessageToolCallFunction{Name: &name, Arguments: blk.Get("input").Raw}})
			case "tool_result":
				id := blk.Get("tool_use_id").String()
				m := tool(blockText(blk))
				m.ChatToolMessage = &bschemas.ChatToolMessage{ToolCallID: &id}
				results = append(results, m)
			default:
				text.WriteString(blk.Get("text").String())
			}
			return true
		})
		if msg.Get("content").Type == gjson.String {
			text.WriteString(msg.Get("content").String())
		}
		if len(calls) > 0 {
			req.Input = append(req.Input, bschemas.ChatMessage{Role: bschemas.ChatMessageRoleAssistant,
				ChatAssistantMessage: &bschemas.ChatAssistantMessage{ToolCalls: calls}})
		}
		if text.Len() > 0 {
			m := bschemas.ChatMessage{Role: bschemas.ChatMessageRole(role)}
			if role == "assistant" {
				m.ChatAssistantMessage = &bschemas.ChatAssistantMessage{}
			}
			schema.SetMessageText(&m, text.String())
			req.Input = append(req.Input, m)
		}
		req.Input = append(req.Input, results...) // normalize splits packed results, as apply does
		return true
	})
	return req
}

// removedDollars prices removed tokens on THIS corpus. A removed token is worth 0.209x a
// fresh one ($0.412/MTok) because the corpus is ~90% cache_read — pricing at 1.0x
// would overstate every figure here by ~4.8x.
// 0.209x fresh, at the $2.00/MTok this gateway actually billed the capture corpus. An earlier
// value of 0.6265 applied the same multiplier to Anthropic's $3.00 list rate, which overstated
// every dollar below by ~1.26x. Rankings were unaffected; absolute figures were not.
const perMTok = 0.412

func removedDollars(tok int) float64 { return float64(tok) / 1e6 * perMTok }

func TestSkeletonCaptureMeasure(t *testing.T) {
	spec := os.Getenv("CG_SKEL_CAPTURE")
	if spec == "" {
		t.Skip("set CG_SKEL_CAPTURE=/path/capture.jsonl[:more.jsonl] to measure")
	}
	for _, file := range strings.Split(spec, ":") {
		measureCapture(t, file)
	}
}

func measureCapture(t *testing.T, file string) {
	pairs := capturePairs(t, file)
	s := skeletonFor(t, "min_tokens: 80\n")

	var (
		allTok, dumpCand, dumpTok    int
		acted, before, after         int
		gates                        = map[string]int{}
		byTool                       = map[string]int{}
		biggestGain                  int
		bigBefore, bigAfter, bigCall string
	)
	for _, p := range pairs {
		allTok += schema.TextTokens(p.text)
		isDump := dumpGrammar(schema.ToolCall{Name: p.name, Args: p.args}) != ""
		if isDump {
			dumpCand++
			dumpTok += schema.TextTokens(p.text)
		}
		req := pairReq(p)
		rep := &components.Report{}
		if _, err := s.Offload(req, rep, &components.Ctx{Session: "m", Store: store.NewMemory(store.Options{})}); err != nil {
			t.Fatal(err)
		}
		got := schema.MessageText(req.Input[1])
		if got == p.text {
			for g := range rep.Gates {
				gates[g]++
			}
			continue
		}
		acted++
		byTool[p.name]++
		b, a := schema.TextTokens(p.text), schema.TextTokens(got)
		before += b
		after += a
		if b-a > biggestGain {
			biggestGain, bigBefore, bigAfter, bigCall = b-a, p.text, got, p.name+" "+p.args
		}
	}
	pct := func(a, b int) float64 {
		if b == 0 {
			return 0
		}
		return 100 * float64(a) / float64(b)
	}
	fmt.Printf("\n=== %s ===\n", file)
	fmt.Printf("PER DUMP    unique tool outputs=%d (%d tokens)  file-dump candidates=%d (%d tokens, %.1f%% of all tool tokens)\n",
		len(pairs), allTok, dumpCand, dumpTok, pct(dumpTok, allTok))
	fmt.Printf("PER DUMP    acted on=%d/%d candidates (%.1f%% fire rate)  by tool=%v\n",
		acted, dumpCand, pct(acted, dumpCand), sorted(byTool))
	fmt.Printf("PER DUMP    tokens %d -> %d  removed %d (%.1f%% of the dumps it acted on, %.1f%% of ALL tool tokens)  = $%.4f at 0.209x\n",
		before, after, before-after, pct(before-after, before), pct(before-after, allTok), removedDollars(before-after))
	fmt.Printf("PER DUMP    declined: %v\n", sorted(gates))

	// --- per request: the real thing, with the gates a live turn applies ---
	req := rebuildLast(t, file)
	tot := schema.MessagesTokens(req)
	maxCached := len(req.Input) - 3 // the proxy's tail: everything but the last turn is cached
	rep := &components.Report{}
	if _, err := s.Offload(req, rep, &components.Ctx{Session: "r", Store: store.NewMemory(store.Options{}),
		CacheAware: true, MaxCachedIdx: maxCached}); err != nil {
		t.Fatal(err)
	}
	warm := tot - schema.MessagesTokens(req)
	req2 := rebuildLast(t, file)
	rep2 := &components.Report{}
	if _, err := s.Offload(req2, rep2, &components.Ctx{Session: "c", Store: store.NewMemory(store.Options{})}); err != nil {
		t.Fatal(err)
	}
	cold := tot - schema.MessagesTokens(req2)
	fmt.Printf("PER REQUEST largest transcript: %d messages, %d tokens\n", len(req.Input), tot)
	fmt.Printf("PER REQUEST warm (cache-tail gate on, MaxCachedIdx=%d): removed %d tokens = $%.4f   gates=%v\n",
		maxCached, warm, removedDollars(warm), sorted(rep.Gates))
	fmt.Printf("PER REQUEST cold (whole transcript eligible):           removed %d tokens (%.1f%%) = $%.4f   gates=%v\n",
		cold, pct(cold, tot), removedDollars(cold), sorted(rep2.Gates))

	// --- the counterfactual the honest answer needs -----------------------
	//
	// The two figures above disagree by everything, and the reason is ONE rule: the
	// newest Read of a path is never touched. On real traffic an agent reads each file
	// once and never re-reads it, so EVERY Read is the newest of its path and the whole
	// lever is gated away. This measures what a weaker rule — protect the newest Read
	// only while it is still near the tail, where the agent is actually working — would
	// be worth, so the decision not to enable this component rests on a number.
	//
	// It is measurement only. No such knob exists, deliberately: an older Read is still
	// the file's true content, and "the agent stopped looking at it 40 messages ago" is
	// a guess about attention, not a fact about the file (unlike readlifecycle's stale
	// class, which acts only on a Read a LATER edit has provably falsified).
	for _, window := range []int{20, 40} {
		pairs3 := schema.ToolCalls(req2)
		newest3 := newestReads(req2, pairs3)
		s3 := skeletonFor(t, "min_tokens: 80\n")
		n, gain := 0, 0
		for i := range req2.Input {
			if req2.Input[i].Role != bschemas.ChatMessageRoleTool || i >= len(req2.Input)-window {
				continue
			}
			tc := pairs3[i]
			txt := schema.MessageText(req2.Input[i])
			if txt == "" || schema.TextTokens(txt) < 80 || newest3[dumpPath(tc)] != i {
				continue
			}
			out, ok := s3.reduce(txt, tc, &components.Report{})
			if !ok {
				continue
			}
			n++
			gain += schema.TextTokens(txt) - schema.TextTokens(out)
		}
		fmt.Printf("COUNTERFACT newest-read protected only within the last %d messages: %d more dumps reducible, %d tokens = $%.4f\n",
			window, n, gain, removedDollars(gain))
	}

	if os.Getenv("CG_SKEL_SAMPLE") == "1" && bigBefore != "" {
		fmt.Printf("\n--- LARGEST REDUCTION (%s): %d -> %d tokens ---\n", bigCall, schema.TextTokens(bigBefore), schema.TextTokens(bigAfter))
		fmt.Printf("BEFORE (first 40 lines of %d):\n%s\n", strings.Count(bigBefore, "\n")+1, headLines(bigBefore, 40))
		fmt.Printf("AFTER (first 40 lines of %d):\n%s\n", strings.Count(bigAfter, "\n")+1, headLines(bigAfter, 40))
	}
}

func headLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = append(lines[:n], "…")
	}
	return strings.Join(lines, "\n")
}

// sorted renders a counter deterministically (descending count, then key), so two runs
// of the measurement print the same thing.
func sorted(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "%s=%d", k, m[k])
	}
	b.WriteByte('}')
	return b.String()
}
