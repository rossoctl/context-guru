package cheapmodel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/dispatch"
)

// send is the one transport call every cheap-model request makes: build the request, hand it to
// the host's Dispatcher, and return the raw response body on 2xx. The host owns the base URL and
// the credential; this only names the path, the provider dialect and the non-secret headers.
//
// label prefixes the error on a non-2xx status ("", "prefixed ", "prefixed Responses "), so the
// three call shapes keep their own wording without three near-identical error branches.
func send(ctx context.Context, d dispatch.Dispatcher, ref dispatch.Ref, p bschemas.ModelProvider,
	path, label string, hdr http.Header, body []byte) ([]byte, error) {
	if d == nil {
		return nil, errors.New("cheapmodel: no dispatcher")
	}
	resp, err := d.Do(ctx, dispatch.Request{Purpose: dispatch.PurposeCheapModel, Ref: ref,
		Provider: p, Method: http.MethodPost, Path: path, Header: hdr, Body: body})
	if err != nil {
		return nil, err
	}
	if resp.Status != http.StatusOK {
		return nil, fmt.Errorf("cheapmodel: %sstatus %d: %s", label, resp.Status, clipErr(resp.Body))
	}
	return resp.Body, nil
}

// clipErr is clipErrBody for a response the Dispatcher already read in full.
func clipErr(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > errBodyCap {
		s = s[:errBodyCap]
	}
	if s == "" {
		return "(empty body)"
	}
	return strings.Join(strings.Fields(s), " ")
}
