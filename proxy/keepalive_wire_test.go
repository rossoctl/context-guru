package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/tidwall/gjson"
)

// Reviewer's arm: the WHOLE chain, masked hold -> sweep unmask -> fire -> real sendPing -> wire.
//
// Neither existing test covers this seam. TestRetainedBodyIsMaskedAtRest stops at the k.send
// fake, so it proves what fire HANDS to send, not what is posted. TestSendPingHitsTheUpstream
// starts after masking: it passes a plaintext Authorization and pingBody() output directly, so
// it never exercises an unmask at all. A mask bug between those two points is invisible to both,
// and it is the expensive one — altered wire bytes write a new entry at 12.5x instead of
// refreshing one at 0.1x.
func TestPingWireBytesEndToEndThroughRealSend(t *testing.T) {
	const cred = "Bearer sk-caller-END-TO-END"
	const marker = "MY-PRIVATE-SOURCE-CODE-MARKER"

	// The handler runs on the test server's goroutine, and the HTTP round trip is not a
	// happens-before edge, so what it saw comes back over a buffered channel. k.dispatch is
	// k.fire (inline), so by the time sweep() returns the ping has completed and the value is
	// already queued -- hence a non-blocking receive, which keeps "no ping fired" observable
	// as the zero value rather than turning it into a deadlock.
	type seen struct {
		body []byte
		auth string
	}
	seenCh := make(chan seen, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 1<<20)
		n, _ := r.Body.Read(b)
		seenCh <- seen{append([]byte(nil), b[:n]...), r.Header.Get("Authorization")}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"usage":{"input_tokens":0,"output_tokens":1,` +
			`"cache_read_input_tokens":48576,"cache_creation_input_tokens":0}}`))
	}))
	defer srv.Close()

	// R5: retention needs a real audit sink, so wire one through New() as the fixture does.
	sink, err := dash.NewRecorder(dash.Options{DBPath: ":memory:", BatchSize: 1,
		FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	h := New(nil, nil, metrics.NewAggregator(), Options{Dashboard: sink})
	defer h.Close()
	h.client = srv.Client()
	k := h.keeper
	clock := &testClock{at: time.Now()}
	k.now = clock.now
	// NOTE: k.send is NOT overridden — this goes out over real HTTP through sendPing.
	k.dispatch = k.fire // inline, so the assertions below run after the ping completes

	orig := []byte(strings.Replace(kaBody, `"text":"hi"`, `"text":"`+marker+`"`, 1))
	want, ok := pingBody(orig)
	if !ok {
		t.Fatal("pingBody refused the fixture")
	}
	tn := &Tenancy{ID: "t1", Cache: kaPolicy()}
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
	r.Header.Set("Authorization", cred)
	up := upstream{base: srv.URL, path: "/v1/messages"}
	for i := 0; i < 2; i++ { // two turns: the gate needs turn >= 1
		k.record(tn, "s", clock.now().Add(time.Duration(i)*time.Second), orig, up, r,
			bschemas.Anthropic, "/v1/messages", http.StatusOK, Usage{CacheRead: 48576}, true)
	}
	if k.Stats().Live != 1 {
		t.Fatal("nothing retained, so nothing is being tested")
	}
	// Prove the hold really is masked, or the rest of this proves nothing about unmasking.
	k.mu.Lock()
	held := append([]byte(nil), k.live[kaKey("t1", "s")].body...)
	k.mu.Unlock()
	if bytes.Contains(held, []byte(marker)) {
		t.Fatal("the hold is plaintext, so this test is not exercising an unmask")
	}

	if n := k.sweep(clock.advance(281 * time.Second)); n != 1 {
		t.Fatalf("sweep fired %d pings", n)
	}
	var got seen
	select {
	case got = <-seenCh:
	default:
	}
	if got.body == nil {
		t.Fatal("the upstream received no ping")
	}
	if !bytes.Equal(got.body, want) {
		t.Errorf("WIRE BYTES DIFFER from pingBody(recorded): len got=%d want=%d",
			len(got.body), len(want))
	}
	for _, p := range []string{"model", "tools", "system", "messages", "thinking", "tool_choice"} {
		if a, b := gjson.GetBytes(orig, p).Raw, gjson.GetBytes(got.body, p).Raw; a != b {
			t.Errorf("%s differs on the wire:\n before: %.100s\n  after: %.100s", p, a, b)
		}
	}
	if strings.Count(string(orig), "cache_control") != strings.Count(string(got.body), "cache_control") {
		t.Error("breakpoint count changed on the wire")
	}
	// The credential unmasks to the caller's own value on the wire, and only there.
	if got.auth != cred {
		t.Errorf("upstream saw Authorization %q, want the caller's own key", got.auth)
	}
}
