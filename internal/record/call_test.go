package record

import (
	"strings"
	"testing"
)

// JoinIncomplete must not duplicate a reason it already carries, or a long multi-round
// response would accumulate "tee_cap_reached,tee_cap_reached,...".
func TestJoinIncompleteComposesWithoutDuplicating(t *testing.T) {
	got := JoinIncomplete(IncompleteNone, IncompleteRetainCap)
	if got != IncompleteRetainCap {
		t.Errorf("first reason = %q, want %q", got, IncompleteRetainCap)
	}
	got = JoinIncomplete(got, IncompleteTruncated)
	if got != IncompleteRetainCap+","+IncompleteTruncated {
		t.Errorf("second reason did not append: %q", got)
	}
	if again := JoinIncomplete(got, IncompleteRetainCap); again != got {
		t.Errorf("a repeated reason must not be appended twice: %q", again)
	}
	if none := JoinIncomplete(got, IncompleteNone); none != got {
		t.Errorf("IncompleteNone must be a no-op, got %q", none)
	}
}

// A turn whose body is a spliced mix of reasons keeps the Turn method and JoinIncomplete in step.
func TestMarkIncompleteUsesJoinIncomplete(t *testing.T) {
	var turn Turn
	for _, r := range []string{IncompleteTeeCap, IncompleteNoBody, IncompleteTeeCap, IncompleteNone} {
		turn.MarkIncomplete(r)
	}
	want := JoinIncomplete(JoinIncomplete(IncompleteNone, IncompleteTeeCap), IncompleteNoBody)
	if turn.Incomplete != want || turn.Incomplete != IncompleteTeeCap+","+IncompleteNoBody {
		t.Errorf("Incomplete = %q, want %q", turn.Incomplete, want)
	}
}

// The trace id comes from Claude Code's own session id, then the host's resolver, then a stable
// per-process key. It is never empty.
func TestTraceIDOrder(t *testing.T) {
	const uuid = "0f8fad5b-d9cb-469f-a165-70867728950e"
	withUser := []byte(`{"metadata":{"user_id":"` + uuid + `"}}`)
	calls := 0
	derived := func() string { calls++; return "t1:abc" }

	if got := TraceID(withUser, derived); got != uuid || calls != 0 {
		t.Errorf("explicit id: got %q (derived calls %d), want %q and no call", got, calls, uuid)
	}
	if got := TraceID([]byte(`{}`), derived); got != "cg-derived-t1:abc" || calls != 1 {
		t.Errorf("derived id: got %q", got)
	}
	un := TraceID([]byte(`{}`), nil)
	if !strings.HasPrefix(un, "cg-unkeyed-") || len(un) == len("cg-unkeyed-") {
		t.Errorf("unkeyed id = %q, want a non-empty suffix", un)
	}
	if again := TraceID([]byte(`{}`), func() string { return "" }); again != un {
		t.Errorf("a resolver that finds nothing must give the same unkeyed id: %q vs %q", again, un)
	}
}

const callReq = `{"model":"claude-sonnet-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
const callResp = `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn"}`

func TestTurnFromCallCopiesTheFactsTheHostGives(t *testing.T) {
	turn := TurnFromCall(ProxiedCall{
		TraceID: "tr", SpanID: "sp", ParentID: "pa", Provider: "anthropic",
		ReqBody: []byte(callReq), RespBody: []byte(callResp), RespSize: 99,
		FreshInput: 1, CacheRead: 2, CacheWrite: 3, Output: 4, StopReason: "end_turn", UsageOK: true,
		UpstreamMS: 7, FirstByteMS: 5, Rounds: 2, Err: "boom",
	})
	if turn.SessionID != "tr" || turn.SpanID != "sp" || turn.ParentID != "pa" || turn.System != "anthropic" {
		t.Errorf("ids = %+v", turn)
	}
	if turn.FreshInput != 1 || turn.CacheRead != 2 || turn.CacheWrite != 3 || turn.Output != 4 ||
		turn.StopReason != "end_turn" || turn.UpstreamMS != 7 || turn.FirstByteMS != 5 || turn.Rounds != 2 ||
		turn.ResponseSize != 99 || turn.Err != "boom" {
		t.Errorf("scalars = %+v", turn)
	}
	if turn.Model != "claude-sonnet-5" || turn.MaxTokens != 64 {
		t.Errorf("request meta = %q %d", turn.Model, turn.MaxTokens)
	}
	if turn.RequestBody == "" || turn.ResponseBody == "" || turn.Incomplete != IncompleteNone {
		t.Errorf("bodies = %q / %q, incomplete = %q", turn.RequestBody, turn.ResponseBody, turn.Incomplete)
	}
}

func TestTurnFromCallSaysWhenTheTurnIsNotWhole(t *testing.T) {
	sse := []byte("data: {\"type\":\"message_stop\"}\n\n")
	for _, tc := range []struct {
		name string
		call ProxiedCall
		want string
	}{
		{"no usage was read", ProxiedCall{Provider: "anthropic", ReqBody: []byte(callReq), RespBody: []byte(callResp)}, IncompleteNoUsage},
		{"no response body", ProxiedCall{Provider: "anthropic", ReqBody: []byte(callReq), UsageOK: true}, IncompleteNoBody},
		{"a stream of another provider cannot be translated", ProxiedCall{Provider: "openai", ReqBody: []byte(callReq), RespBody: sse, RespIsSSE: true, UsageOK: true}, IncompleteNoBody},
		{"the host's own reason comes first", ProxiedCall{Provider: "anthropic", ReqBody: []byte(callReq), Incomplete: IncompleteTeeCap}, IncompleteTeeCap + "," + IncompleteNoBody + "," + IncompleteNoUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := TurnFromCall(tc.call).Incomplete; got != tc.want {
				t.Errorf("Incomplete = %q, want %q", got, tc.want)
			}
		})
	}
}
