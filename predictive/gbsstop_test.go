package predictive

import "testing"

// The user-turn and tool-streak features are a MODEL INPUT. The model was trained with
// scripts/campaign/gbsfeat.py, which counts a tool turn only when the stop reason is exactly
// "tool_use" (lines 54 and 56). A live row of an OpenAI client, with tool_calls, reads as a user
// turn there. The live value must stay equal to the training value. This test pins it, so that a
// later "one list of tool stop reasons" cleanup does not change the input without a retrain.
func TestGBSToolTurnIsTheTrainingValue(t *testing.T) {
	for _, c := range []struct {
		stop         string
		wantUserTurn float64 // m_is_user_turn of the request that follows
		wantStreak   float64 // m_tool_streak of the response itself
	}{
		{"tool_use", 0, 1},
		{"tool_calls", 1, 0},
		{"function_call", 1, 0},
		{"end_turn", 1, 0},
		{"", 1, 0},
	} {
		tr := NewTracker()
		r1 := Request{Tenant: "t", Session: "s", Thread: "th", Model: "m", TsMs: 1_000_000}
		r2 := r1
		r2.TsMs = 1_020_000
		a1 := tr.Arrive(r1)
		resp1 := &Response{Request: r1, EndMs: 1_004_000, DecMs: 1_004_000, Prefix: 1000, Fresh: 5, Output: 10, StopReason: c.stop}
		row1 := NewRow()
		tr.Ended(a1, resp1)
		tr.Features(a1, resp1, row1)
		if got := row1.F[GBSBase+indexOfGBS("m_tool_streak")]; got != c.wantStreak {
			t.Errorf("stop %q: m_tool_streak %v, want %v", c.stop, got, c.wantStreak)
		}
		a2 := tr.Arrive(r2)
		resp2 := &Response{Request: r2, EndMs: 1_024_000, DecMs: 1_024_000, Prefix: 1000, Fresh: 5, Output: 10, StopReason: "end_turn"}
		row2 := NewRow()
		tr.Ended(a2, resp2)
		tr.Features(a2, resp2, row2)
		if got := row2.F[GBSBase+indexOfGBS("m_is_user_turn")]; got != c.wantUserTurn {
			t.Errorf("after stop %q: m_is_user_turn %v, want %v", c.stop, got, c.wantUserTurn)
		}
	}
}
