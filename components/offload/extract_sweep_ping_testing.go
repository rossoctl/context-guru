package offload

import (
	"context"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/store"
)

// RegisterSweepPingForTest runs the sweep's own Offload once over msgs, as the request path does, so
// the thread registers its ping-time question exactly the way production registers it. For the
// keeper's tests in another package, which cannot reach the unexported constructor.
//
// cfgYAML is the component's config block ("" is the shipped defaults). model is the request's model
// id. The registry is a package global keyed by thread.Key(session, threadID); a test must call
// ClearKeepAliveCandidate for it when done.
func RegisterSweepPingForTest(session, threadID string, st store.Store, asker components.PrefixAsker,
	model string, msgs []bschemas.ChatMessage, cfgYAML string) error {

	built, err := newExtractSweep([]byte("shadow: true\n" + cfgYAML)) // the ping asks only in shadow mode until PR C
	if err != nil {
		return err
	}
	c := &components.Ctx{Ctx: context.Background(), Session: session, Thread: threadID, Store: st,
		PrefixAsk: asker, ModelName: model, CtxWindow: 1_000_000, MaxCachedIdx: -1}
	_, err = built.(*ExtractSweep).Offload(&bschemas.BifrostChatRequest{Input: msgs},
		&components.Report{Component: "extract_llm_sweep"}, c)
	return err
}

// HeldSweepAnswerForTest reports a thread's held answer in the shape a test can assert on, without
// exporting the record: how many outputs the question judged, how many it proposed to drop, and the
// wait cause stored with it.
func HeldSweepAnswerForTest(st store.Store, threadKey string) (asked, proposed int, waitingFor string, ok bool) {
	h, ok := getHeld(st, threadKey)
	if !ok {
		return 0, 0, "", false
	}
	n, _ := h.proposed()
	return len(h.Outputs), n, h.WaitingFor, true
}
