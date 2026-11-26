package cheapmodel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/context-guru/dispatch/dispatchtest"
)

func TestSendReturnsTheBodyOn200(t *testing.T) {
	f := dispatchtest.NewFake(func(dispatch.Request) (dispatch.Response, error) {
		return dispatch.Response{Status: 200, Body: []byte(`{"ok":true}`)}, nil
	})
	ref := f.Pin()
	body, err := send(context.Background(), f, ref, bschemas.Anthropic, "/v1/messages", "", nil, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"ok":true}` {
		t.Fatalf("body = %q", body)
	}
}

func TestSendErrorsOnNon200WithTheLabelAndClippedBody(t *testing.T) {
	f := dispatchtest.NewFake(func(dispatch.Request) (dispatch.Response, error) {
		return dispatch.Response{Status: 401, Body: []byte("unauthorized")}, nil
	})
	ref := f.Pin()
	_, err := send(context.Background(), f, ref, bschemas.Anthropic, "/v1/messages", "prefixed ", nil, []byte(`{}`))
	if err == nil {
		t.Fatal("want an error on 401")
	}
	if !strings.Contains(err.Error(), "prefixed status 401") || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("err = %v, want the label, the status and the body", err)
	}
}

func TestSendWithNoDispatcherErrors(t *testing.T) {
	if _, err := send(context.Background(), nil, "ref", bschemas.Anthropic, "/v1/messages", "", nil, []byte(`{}`)); err == nil {
		t.Fatal("want an error with no Dispatcher")
	}
}

// send puts the answer's headers on the sink scoping ctx, and on every sink it nests inside, so a
// keep-alive substitute can report them on its ping row (#1016). The newest answer wins.
func TestSendNotesTheResponseHeaderOnTheSink(t *testing.T) {
	n := 0
	f := dispatchtest.NewFake(func(dispatch.Request) (dispatch.Response, error) {
		n++
		h := http.Header{}
		h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", fmt.Sprintf("0.%d", n))
		return dispatch.Response{Status: 200, Header: h, Body: []byte(`{}`)}, nil
	})
	ref := f.Pin()
	ctx, outer := WithDetachedSink(context.Background())
	ctx, inner := WithCallSink(ctx)
	for i := 0; i < 2; i++ {
		if _, err := send(ctx, f, ref, bschemas.Anthropic, "/v1/messages", "", nil, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	for name, s := range map[string]*Sink{"inner": inner, "outer": outer} {
		if got := s.Header().Get("anthropic-ratelimit-unified-5h-utilization"); got != "0.2" {
			t.Errorf("%s sink header = %q, want 0.2 (the newest answer)", name, got)
		}
	}
}

// An error answer carries the limit headers too (a 429 says "rejected"), so send notes them before
// it returns the error. A transport error has no answer and notes nothing.
func TestSendNotesTheHeaderOfAnErrorAnswerButNotOfATransportError(t *testing.T) {
	f := dispatchtest.NewFake(func(dispatch.Request) (dispatch.Response, error) {
		return dispatch.Response{Status: 429, Header: http.Header{"X-A": {"1"}}, Body: []byte("slow down")}, nil
	})
	ref := f.Pin()
	ctx, s := WithDetachedSink(context.Background())
	if _, err := send(ctx, f, ref, bschemas.Anthropic, "/v1/messages", "", nil, []byte(`{}`)); err == nil {
		t.Fatal("want an error on 429")
	}
	if s.Header().Get("X-A") != "1" {
		t.Errorf("sink header = %v, want the 429 answer's header", s.Header())
	}

	g := dispatchtest.NewFake(func(dispatch.Request) (dispatch.Response, error) {
		return dispatch.Response{}, errors.New("connection reset")
	})
	gref := g.Pin()
	ctx, s2 := WithDetachedSink(context.Background())
	if _, err := send(ctx, g, gref, bschemas.Anthropic, "/v1/messages", "", nil, []byte(`{}`)); err == nil {
		t.Fatal("want the transport error")
	}
	if s2.Header() != nil {
		t.Errorf("sink header = %v, want nil after a transport error", s2.Header())
	}
	// No sink in ctx: send still works and notes nothing anywhere.
	if _, err := send(context.Background(), f, ref, bschemas.Anthropic, "/v1/messages", "", nil, []byte(`{}`)); err == nil {
		t.Fatal("want an error on 429")
	}
}
