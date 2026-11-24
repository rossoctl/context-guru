package apply_test

import (
	"context"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/store"
)

// context-guru#1354: the messages-array path left Result.Changed false after a head-TTL relabel
// when the count-changing rebuild declined. The body held the 1h label, but Changed said "no change".
func TestHeadTTLRelabelCountsAsChangedOnTheMessagesPath(t *testing.T) {
	pad := strings.Repeat("context ", 30000)
	long := strings.Repeat("some long earlier turn text ", 20)
	// The message with a non-string role has no slot, so the rebuild declines.
	body := `{"model":"aws/claude-opus-5",` +
		`"tools":[{"name":"Bash","description":"` + pad + `","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],` +
		`"system":[{"type":"text","text":"you are claude code","cache_control":{"type":"ephemeral"}},` +
		`{"type":"text","text":"Current branch: main","cache_control":{"type":"ephemeral"}}],` +
		`"messages":[
		{"role":"user","content":"first"},
		{"role":"assistant","content":"` + long + `"},
		{"role":"user","content":"` + long + `"},
		{"role":"assistant","content":"tail one"},
		{"role":123,"content":"x"},
		{"role":"user","content":[{"type":"text","text":"final","cache_control":{"type":"ephemeral"}}]}
	]}`
	p, err := pipe(t, "pipeline: [midsum]\n").Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	res := apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{
		Provider: bschemas.Anthropic, Body: []byte(body), HeadTTL1h: true, HeadTTLMinBytes: 200000,
	})
	if !res.Trace.HeadTTL1h || !strings.Contains(string(res.Body), `"ttl":"1h"`) {
		t.Fatalf("precondition: the body must hold the 1h relabel (trace %v)", res.Trace.HeadTTL1h)
	}
	if !res.Changed {
		t.Fatal("Changed = false, but the body holds the head-TTL relabel")
	}
}
