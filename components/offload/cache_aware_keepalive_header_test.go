package offload

import (
	"testing"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/thread"
)

// The keep-alive summary passes its answer's headers to the keeper only when the call uses the
// user's own credential: the incoming client or the PrefixAsk path. With `model.source: config`,
// or when `incoming` falls back to the configured model, the operator's endpoint answers and its
// limit headers are the operator's, so the candidate is marked and KeepAliveSubstitute clears the
// result's Header (#1016 review). The clearing itself is checked in keepalive's
// TestConfigSourceSummarySubstituteReportHasNoHeader, with a real client on a fake host.
//
// Fake models here, not a real cheapmodel client: a real call adds to cheapmodel's process totals,
// which the extract economics tests in this package read.
func TestKeepAliveSummaryCandidateMarksTheUsersOwnCredential(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		incoming     bool // the host resolved an incoming client
		prefixAsk    bool // no MessagesModel at all; only PrefixAsk
		want         bool
	}{
		{name: "incoming", source: "incoming", incoming: true, want: true},
		{name: "config", source: "config", incoming: true, want: false},
		{name: "incoming falls back to config", source: "incoming", incoming: false, want: false},
		{name: "prefix ask", source: "incoming", prefixAsk: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newCacheAware(t, caBaseCfg+"instruction_role: user\nmodel:\n  source: "+tc.source+"\n")
			c := caCtx("ca-ka-cred-" + tc.source + "-" + tc.name)
			if tc.prefixAsk {
				c.PrefixAsk = &fakePrefixAsker{out: "<summary>ok</summary>"}
			} else {
				spec := components.ModelSpec{Static: &capturingModel{out: "<summary>ok</summary>"}}
				if tc.incoming {
					spec.Incoming = &capturingModel{out: "<summary>ok</summary>"}
				}
				c.Model = spec
			}
			t.Cleanup(func() { ClearKeepAliveCandidate(c.Session) })
			caTurn(t, s, c, caFixture())
			keepAliveCandMu.Lock()
			cand := keepAliveCand[thread.Key(c.Session, c.Thread)]
			keepAliveCandMu.Unlock()
			if cand == nil {
				t.Fatal("no keep-alive candidate registered")
			}
			if cand.ownCredential != tc.want {
				t.Errorf("ownCredential = %v, want %v", cand.ownCredential, tc.want)
			}
		})
	}
}
