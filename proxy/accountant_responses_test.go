package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/dash"
)

// The Responses wire keeps the conversation in input and the system prompt in instructions. accBodies
// reads them as the messages and system sections: one position per input item, and instructions as
// one block. Each position's size is its wire size.
func TestAccBodies_ResponsesBody(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.5","tools":[{"type":"function","name":"bash"}],"instructions":"be brief","input":[` +
		`{"role":"user","content":"run it"},` +
		`{"type":"function_call","call_id":"c1","name":"bash","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"done"}]}`)
	b := accBodies(raw)
	if len(b.Pos) != 5 {
		t.Fatalf("%d positions, want 5 (1 tool, 1 instructions, 3 items)", len(b.Pos))
	}
	want := []string{gjson.GetBytes(raw, "tools.0").Raw, gjson.GetBytes(raw, "instructions").Raw}
	for _, it := range gjson.GetBytes(raw, "input").Array() {
		want = append(want, it.Raw)
	}
	for i, w := range want {
		if int(b.Pos[i].Bytes) != len(w) {
			t.Errorf("position %d is %d bytes, want %d", i, b.Pos[i].Bytes, len(w))
		}
	}
	// The sections: tools from 0, system (instructions) from 1, messages (input) from 2.
	if fmt.Sprint(b.Start) != fmt.Sprint([3]int{0, 1, 2}) {
		t.Errorf("section starts %v, want [0 1 2]", b.Start)
	}
	if bps := accBreakpoints(raw); len(bps) != 0 {
		t.Errorf("breakpoints %+v: the Responses wire has no cache marks", bps)
	}
}

// A string input is one position.
func TestAccBodies_ResponsesStringInput(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.5","input":"hello there"}`)
	b := accBodies(raw)
	if len(b.Pos) != 1 || int(b.Pos[0].Bytes) != len(`"hello there"`) {
		t.Fatalf("positions %+v, want one of %d bytes", b.Pos, len(`"hello there"`))
	}
}

// messages wins over input, so a Messages or Chat Completions body is read exactly as before.
func TestAccBodies_MessagesWinOverInput(t *testing.T) {
	a := accBodies([]byte(`{"messages":[{"role":"user","content":"x"}],"input":[{"role":"user","content":"y"},{"role":"user","content":"z"}]}`))
	b := accBodies([]byte(`{"messages":[{"role":"user","content":"x"}]}`))
	if fmt.Sprint(a.Pos) != fmt.Sprint(b.Pos) {
		t.Fatalf("input changed a messages body:\n got %+v\nwant %+v", a.Pos, b.Pos)
	}
}

// An envelope change of the instructions (the tool filter takes a skill out of them) is a system rewrite,
// owned by toolfilter. It is not a head-TTL relabel: no cache mark changed, and a headttl: ChangeID would put
// the saving on the headttl row of an OpenAI account, where that lever never acts (review of #1150).
func TestEnvelopeOps_ResponsesInstructionsRewrite(t *testing.T) {
	client := []byte(`{"instructions":"long instructions with a skill list","input":[{"role":"user","content":"x"}]}`)
	fwd := []byte(`{"instructions":"long instructions","input":[{"role":"user","content":"x"}]}`)
	ops := envelopeOps(client, fwd)
	if len(ops) != 1 {
		t.Fatalf("ops = %+v, want one system rewrite", ops)
	}
	o := ops[0]
	cl, fw := gjson.GetBytes(client, "instructions").Raw, gjson.GetBytes(fwd, "instructions").Raw
	if o.At != (accountant.Position{Section: accountant.SectionSystem, Index: 0}) ||
		o.SizeBefore.Bytes != len(cl) || o.SizeAfter.Bytes != len(fw) {
		t.Fatalf("op = %+v, want system 0, %d -> %d bytes", o, len(cl), len(fw))
	}
	if o.Operator != "context-guru.toolfilter" || !strings.HasPrefix(o.ChangeID, "cg:toolfilter:") {
		t.Fatalf("op owned by %s (ChangeID %s), want context-guru.toolfilter with a cg:toolfilter: ChangeID", o.Operator, o.ChangeID)
	}
}

// The head-TTL relabel of a system block keeps its headttl owner and ChangeID: the library books the 1h premium
// only through a headttl: ChangeID.
func TestEnvelopeOps_SystemRelabelStaysHeadTTL(t *testing.T) {
	client := []byte(`{"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"x"}]}`)
	fwd := []byte(`{"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":"x"}]}`)
	ops := envelopeOps(client, fwd)
	if len(ops) != 1 || ops[0].Operator != "context-guru.headttl" || !strings.HasPrefix(ops[0].ChangeID, "headttl:") {
		t.Fatalf("ops = %+v, want one headttl relabel", ops)
	}
}

// A tools change on the Responses wire is reported as on the other wires.
func TestEnvelopeOps_ResponsesToolRemoved(t *testing.T) {
	client := []byte(`{"tools":[{"type":"function","name":"a"},{"type":"function","name":"b"}],"input":[{"role":"user","content":"x"}]}`)
	fwd := []byte(`{"tools":[{"type":"function","name":"a"}],"input":[{"role":"user","content":"x"}]}`)
	ops := envelopeOps(client, fwd)
	if len(ops) != 1 || ops[0].At != (accountant.Position{Section: accountant.SectionTools, Index: 1}) || ops[0].SizeAfter.Bytes != 0 {
		t.Fatalf("ops = %+v, want one removal at tools 1", ops)
	}
}

// messageOps reads input on the Responses wire: an item inserted after the pipeline is a splice.
func TestMessageOps_ResponsesInsertedItem(t *testing.T) {
	before := []byte(`{"input":[{"role":"user","content":"a"},{"role":"user","content":"b"}]}`)
	after := []byte(`{"input":[{"role":"user","content":"a"},{"role":"developer","content":"names"},{"role":"user","content":"b"}]}`)
	ops := messageOps("tooldefer", before, after)
	if len(ops) != 1 || ops[0].MessageSplice == nil || ops[0].MessageSplice.At != 1 || ops[0].MessageSplice.Inserted != 1 {
		t.Fatalf("ops = %+v, want one insertion at input 1", ops)
	}
	if want := len(gjson.GetBytes(after, "input.1").Raw); ops[0].SizeAfter.Bytes != want {
		t.Fatalf("SizeAfter %d, want %d", ops[0].SizeAfter.Bytes, want)
	}
}

// respUpstream answers a Responses request and bills a quarter of the forwarded bytes, half of it cached.
func respUpstream() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"resp_1","object":"response","status":"completed",`+
			`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],`+
			`"usage":{"input_tokens":%d,"output_tokens":5,"input_tokens_details":{"cached_tokens":%d}}}`, len(b)/4, len(b)/8)
	}))
}

// postN posts each body to path in one session and waits for the rows.
func postN(t *testing.T, srv *httptest.Server, rec *dash.Recorder, path, session string, bodies []string) {
	t.Helper()
	for _, body := range bodies {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("x-context-guru-session", session)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	waitForRows(t, rec, int64(len(bodies)))
}

// agentPrefix is a stable prompt as large as a real agent's, so the removed tool output is a part of
// the request and not nearly all of it (with nearly all of it removed, any ratio error trips the
// implausible_size bound).
var agentPrefix = strings.Repeat("You are a careful coding agent. Read the files before you edit them. ", 120)

// assertPriced fails unless every event was priced by the accountant, and the booked ones save above zero.
func assertPriced(t *testing.T, evs []dash.Event) {
	t.Helper()
	if len(evs) == 0 {
		t.Fatal("no events")
	}
	booked := 0
	for i, e := range evs {
		if fb := e.AccountantFallback(); fb != "" {
			t.Errorf("request %d: fallback %q, want the accountant's figure", i+1, fb)
		}
		r, ok := e.AccountantResult()
		if !ok {
			t.Errorf("request %d: no library Result", i+1)
			continue
		}
		for _, c := range r.Checks {
			if c.Fatal() {
				t.Errorf("request %d: fatal check %s", i+1, c.Check)
			}
		}
		// The first request of a thread has no history to rule out a cache break against, so its figure
		// is held out of the totals and kept as an estimate (#915), on every wire. The later ones book it.
		if e.EstimateHeld {
			if e.EstimatedSavedUSD <= 0 {
				t.Errorf("request %d: held estimate %g, want above zero", i+1, e.EstimatedSavedUSD)
			}
			continue
		}
		var saved float64
		for _, c := range e.Components {
			saved += c.SavedUSD
		}
		if saved <= 0 {
			t.Errorf("request %d: saving %g, want above zero", i+1, saved)
		}
		booked++
	}
	if booked == 0 {
		t.Error("no request booked a saving")
	}
}

// The Responses wire is priced: a request whose tool output a component cut books a saving above
// zero, and the library's sum check passes (#899).
func TestResponsesWireBooksASaving(t *testing.T) {
	up := respUpstream()
	defer up.Close()
	h, rec, tap := accHandler(t, up.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	var bodies []string
	for i := 0; i < 3; i++ {
		bodies = append(bodies, `{"model":"gpt-5","instructions":"`+agentPrefix+`","input":[`+
			`{"role":"user","content":"fix"},`+
			`{"type":"function_call","call_id":"c1","name":"bash","arguments":"{}"},`+
			`{"type":"function_call_output","call_id":"c1","output":"`+strings.Repeat("Traceback line here\\n", 300+i)+`"},`+
			`{"role":"user","content":"again"}]}`)
	}
	postN(t, srv, rec, "/openai/v1/responses", "sess-resp", bodies)
	assertPriced(t, tap.all())
}

// Chat Completions is priced too. The review of #768 saw implausible_size here; that came from a
// fixture whose removed tool output was 98% of the request, and the Anthropic wire gives the same
// result on it. With a real agent's prefix the request prices.
func TestChatCompletionsWireBooksASaving(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":%d,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":%d}}}`, len(b)/4, len(b)/8)
	}))
	defer up.Close()
	h, rec, tap := accHandler(t, up.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	var bodies []string
	for i := 0; i < 3; i++ {
		bodies = append(bodies, `{"model":"gpt-5","messages":[{"role":"system","content":"`+agentPrefix+`"},`+
			`{"role":"user","content":"fix"},`+
			`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"bash","arguments":"{}"}}]},`+
			`{"role":"tool","tool_call_id":"c1","content":"`+strings.Repeat("Traceback line here\\n", 300+i)+`"},`+
			`{"role":"user","content":"again"}]}`)
	}
	postN(t, srv, rec, "/openai/v1/chat/completions", "sess-chat", bodies)
	assertPriced(t, tap.all())
}

// A request with previous_response_id carries only this turn: the provider holds the history, and the
// proxy cannot see it. The record would compare a short delta with the last full body, so the request
// books no saving and says why.
//
// conversation and an item_reference input item do the same: the provider adds stored items the body does not
// carry (review of #1150).
func TestResponsesServerHeldHistoryBooksNoSavingAndSaysWhy(t *testing.T) {
	tool := `{"type":"function_call_output","call_id":"c1","output":"` + strings.Repeat("Traceback line here\\n", 300) + `"}`
	for name, c := range map[string]struct{ field, item string }{
		"previous_response_id": {`"previous_response_id":"resp_0",`, ""},
		"conversation id":      {`"conversation":"conv_0",`, ""},
		"conversation object":  {`"conversation":{"id":"conv_0"},`, ""},
		"item_reference":       {"", `{"type":"item_reference","id":"msg_0"},`},
	} {
		t.Run(name, func(t *testing.T) {
			up := respUpstream()
			defer up.Close()
			h, rec, tap := accHandler(t, up.URL)
			srv := httptest.NewServer(h.Mux())
			defer srv.Close()
			body := `{"model":"gpt-5",` + c.field + `"instructions":"` + agentPrefix + `","input":[` + c.item + tool + `]}`
			postN(t, srv, rec, "/openai/v1/responses", "sess-held", []string{body, body})
			for i, e := range tap.all() {
				if fb := e.AccountantFallback(); fb != dash.FallbackServerHeldHistory {
					t.Errorf("request %d: fallback %q, want %q", i+1, fb, dash.FallbackServerHeldHistory)
				}
				if _, ok := e.AccountantResult(); ok {
					t.Errorf("request %d: got a library Result", i+1)
				}
				if saved := e.BaselineCostUSD - e.CostUSD; saved > 1e-12 {
					t.Errorf("request %d: booked a saving of %g", i+1, saved)
				}
			}
		})
	}
}

// A Responses request that falls back stores its reason: the row keeps acc_fallback, so the Overview can
// count the request. previous_response_id makes the history server-held, which the proxy cannot price.
func TestResponsesFallbackStoresItsReason(t *testing.T) {
	up := respUpstream()
	defer up.Close()
	h, rec, tap := accHandler(t, up.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	tool := `{"type":"function_call_output","call_id":"c1","output":"` + strings.Repeat("Traceback line here\\n", 300) + `"}`
	body := `{"model":"gpt-5","previous_response_id":"resp_0","instructions":"` + agentPrefix + `","input":[` + tool + `]}`
	postN(t, srv, rec, "/openai/v1/responses", "sess-fallback-stored", []string{body, body})
	evs := tap.all()
	if len(evs) == 0 {
		t.Fatal("no events")
	}
	for i, e := range evs {
		if e.AccFallback != dash.FallbackServerHeldHistory {
			t.Errorf("request %d: stored AccFallback %q, want %q", i+1, e.AccFallback, dash.FallbackServerHeldHistory)
		}
	}
}

// The rule itself: what marks a Responses body as one whose history the provider holds.
func TestResponsesServerHeldHistoryRule(t *testing.T) {
	for body, want := range map[string]bool{
		`{"input":[{"role":"user","content":"x"}]}`:                                    false,
		`{"previous_response_id":"","input":"x"}`:                                      false,
		`{"conversation":null,"input":"x"}`:                                            false,
		`{"conversation":"","input":"x"}`:                                              false,
		`{"previous_response_id":"resp_0","input":"x"}`:                                true,
		`{"conversation":"conv_0","input":"x"}`:                                        true,
		`{"conversation":{"id":"conv_0"},"input":"x"}`:                                 true,
		`{"input":[{"role":"user","content":"x"},{"type":"item_reference","id":"a"}]}`: true,
	} {
		if got := responsesServerHeldHistory([]byte(body)); got != want {
			t.Errorf("%s: %v, want %v", body, got, want)
		}
	}
}
