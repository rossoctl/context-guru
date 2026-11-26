package dispatchtest

import (
	"context"
	"errors"
	"testing"

	"github.com/rossoctl/context-guru/dispatch"
)

func TestFakeRecordsAndRefusesUnpinnedRefs(t *testing.T) {
	f := NewFake(func(dispatch.Request) (dispatch.Response, error) {
		return dispatch.Response{Status: 200, Body: []byte(`{}`)}, nil
	})
	if _, err := f.Do(context.Background(), dispatch.Request{Ref: "nope"}); !errors.Is(err, dispatch.ErrNotHeld) {
		t.Fatalf("unpinned ref: err = %v, want ErrNotHeld", err)
	}
	ref := f.Pin()
	body := []byte("abc")
	if _, err := f.Do(context.Background(), dispatch.Request{Ref: ref, Path: "/p", Body: body}); err != nil {
		t.Fatal(err)
	}
	body[0] = 'x'
	sent := f.Sent()
	if len(sent) != 1 || string(sent[0].Body) != "abc" || sent[0].Path != "/p" {
		t.Fatalf("Sent = %+v, want one deep copy of the request", sent)
	}
	f.Release(ref)
	if _, err := f.Do(context.Background(), dispatch.Request{Ref: ref}); !errors.Is(err, dispatch.ErrNotHeld) {
		t.Fatalf("released ref: err = %v, want ErrNotHeld", err)
	}
	if _, ok := f.Price(context.Background(), "", "m", 0); ok {
		t.Fatal("a Fake with no Prices must price nothing")
	}
}
