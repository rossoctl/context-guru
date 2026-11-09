package apply_test

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/modes"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// THE #752 REVERSIBILITY CONTRACT, end to end. A tool output the sweep removes on a cold request
// must come back from `expand` byte for byte, last newline included. The release check for v0.6.2
// found a `cat` output one byte short: the client appends a <system-reminder> block after the
// output, normalize splits that block off, and the whitespace between the output and the block
// went with it — so the stash (and every expand of it) lost the output's own trailing newline.
//
// The reminder itself is NOT part of the reversal. It is client metadata, the request still
// carries it after the marker, and the stash holds the output proper: everything before the
// reminder run. That boundary is pinned here too.

// dropAsker answers the sweep's ping-time question: every labeled output is spent, none is
// needed again. Same reply shape as the sweep's own fixtures use.
type dropAsker struct{ asks int }

var askLabelRe = regexp.MustCompile(`\[(\d+)\] `)

func (a *dropAsker) Ask(_ context.Context, _, ask string) (string, components.PrefixUsage, error) {
	a.asks++
	labels := askLabelRe.FindAllStringSubmatch(ask, -1)
	parts := make([]string, 0, len(labels))
	for _, l := range labels {
		parts = append(parts, `{"i":`+l[1]+`,"needed_by":"none","quote":"","verdict":"drop"}`)
	}
	return "[" + strings.Join(parts, ",") + "]", components.PrefixUsage{CacheRead: 19595, Fresh: 40, Output: 90}, nil
}

// sweepOutput is one tool output above the sweep's floor: filler text with no identifier a later
// turn could reference, so the recheck drops it. end is the byte run the case is about.
func sweepOutput(n int, end string) string {
	base := fmt.Sprintf("log file %d header line\n", n) +
		strings.Repeat("filler record line without any identifier\n", 60) +
		fmt.Sprintf("log file %d end of output", n) // no trailing newline of its own
	return base + end
}

// sweepNewlineBody is a Claude Code shaped request: one Bash tool_result per ending, then a
// closing exchange so no output is the newest message. The last message carries the cache
// breakpoint, as the client does.
func sweepNewlineBody(ends []string, grown bool) []byte {
	msgs := []any{map[string]any{"role": "user", "content": "read the log files"}}
	for i, end := range ends {
		id := fmt.Sprintf("call_%d", i)
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []any{map[string]any{
				"type": "tool_use", "id": id, "name": "Bash",
				"input": map[string]any{"command": fmt.Sprintf("cat log%d.txt", i)}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{
				"type": "tool_result", "tool_use_id": id, "content": sweepOutput(i, end)}}},
		)
	}
	msgs = append(msgs,
		map[string]any{"role": "assistant", "content": "I have read every file."},
		map[string]any{"role": "user", "content": []any{map[string]any{
			"type": "text", "text": "please continue",
			"cache_control": map[string]any{"type": "ephemeral"}}}},
	)
	if grown {
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": "continuing"},
			map[string]any{"role": "user", "content": []any{map[string]any{
				"type": "text", "text": "thanks",
				"cache_control": map[string]any{"type": "ephemeral"}}}},
		)
	}
	b, _ := json.Marshal(map[string]any{"model": "claude-sonnet-5", "max_tokens": 64, "messages": msgs})
	return b
}

func TestTheSweepStashKeepsTheOutputsLastNewline(t *testing.T) {
	// The reminder Claude Code appends after a Bash output (#556). One separator newline, the
	// shape the v0.6.2 release check caught: the file's own last newline.
	reminder := "\n<system-reminder>\n<total_tokens>14989288 tokens left</total_tokens>\n</system-reminder>"
	ends := []string{"\n", "\n\n", "\r\n", reminder}
	// What expand must return for each: the whole text, minus the reminder run (case 4 only).
	wantOutput := map[string]string{}
	for i, end := range ends {
		if end == reminder {
			wantOutput[fmt.Sprintf("call_%d", i)] = sweepOutput(i, "\n")
			continue
		}
		wantOutput[fmt.Sprintf("call_%d", i)] = sweepOutput(i, end)
	}

	cfg := pipe(t, "pipeline: [extract_llm_sweep]\ncomponents:\n  extract_llm_sweep: {min_tokens: 200}\nmode: sync\n")
	p, err := cfg.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory(store.Options{})
	tr := modes.NewTracker(0)
	asker := &dropAsker{}
	base := time.Unix(1_700_000_000, 0)
	body := sweepNewlineBody(ends, false)

	opts := apply.Opts{
		Provider: bschemas.Anthropic, Body: body, Session: "sweep-newline",
		CacheMode: "on", Tracker: tr, Now: base,
		PrefixAsk: asker, CanKeepAlive: func(_, _ string) bool { return true },
	}
	// Turn 1 registers the thread's sweep question.
	res1 := apply.BodyOpts(context.Background(), p, st, opts)
	if asker.asks != 0 {
		t.Fatalf("the request itself made %d asks; the question belongs to the ping", asker.asks)
	}
	dispatch, _, reason, ok := offload.SweepPingSubstitute(res1.Session)
	if !ok {
		t.Fatalf("no sweep question offered (reason %q)", reason)
	}
	held := dispatch(10*time.Second, offload.SweepPingWait{WaitingFor: "human"})
	if !held.Held || held.Proposed != len(ends) {
		t.Fatalf("the ping held %+v; want held with %d proposed drops", held, len(ends))
	}

	// Turn 2, past the cache TTL: the cold apply removes the outputs and stashes them.
	opts.Body, opts.Now = sweepNewlineBody(ends, true), base.Add(7*time.Minute)
	res2 := apply.BodyOpts(context.Background(), p, st, opts)
	if !res2.Changed {
		t.Fatal("the cold request removed nothing")
	}

	markerRe := regexp.MustCompile(`<<cg:([0-9a-f]+)>>`)
	dropped := 0
	for _, m := range gjson.GetBytes(res2.Body, "messages").Array() {
		for _, blk := range m.Get("content").Array() {
			if blk.Get("type").String() != "tool_result" {
				continue
			}
			id := blk.Get("tool_use_id").String()
			got := blk.Get("content").String()
			mat := markerRe.FindStringSubmatch(got)
			if mat == nil {
				continue // an output the apply left verbatim
			}
			dropped++
			orig, ok := expand.Resolve(st, mat[1])
			if !ok {
				t.Fatalf("%s: marker %s does not resolve", id, mat[1])
			}
			if orig != wantOutput[id] {
				t.Fatalf("%s: expand gave back %d bytes, want the tool output's exact %d bytes\n got %q\nwant %q",
					id, len(orig), len(wantOutput[id]), clip752(orig), clip752(wantOutput[id]))
			}
			// The reminder is client metadata the request still carries after the marker: once,
			// at the end, and not inside the stashed original (the equality above already
			// proves the stash holds the output proper).
			if want, suffix := id == "call_3", "<system-reminder>\n<total_tokens>14989288 tokens left</total_tokens>\n</system-reminder>"; want {
				if !strings.HasSuffix(got, suffix) || strings.Count(got, "<system-reminder>") != 1 {
					t.Fatalf("%s: the request must still carry its reminder after the marker: %q", id, clip752(got))
				}
			}
		}
	}
	if dropped != len(ends) {
		t.Fatalf("the cold apply removed %d outputs, want %d", dropped, len(ends))
	}
}

func clip752(s string) string {
	if len(s) > 120 {
		return "…" + s[len(s)-120:]
	}
	return s
}
