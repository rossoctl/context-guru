package cheapmodel

import (
	"context"
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
