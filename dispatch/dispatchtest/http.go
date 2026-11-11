package dispatchtest

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/rossoctl/context-guru/dispatch"
)

// HTTPHost is the smallest real host: it sends to the targets pinned on it, with the headers
// pinned with them. For tests (and live test tools) that talk to an httptest server or a real
// endpoint. It holds credentials in the clear and prices nothing — a production host masks what
// it holds and owns a rate table.
type HTTPHost struct {
	client *http.Client
	mu     sync.Mutex
	next   int
	held   map[dispatch.Ref]target
}

type target struct {
	base string
	auth http.Header
}

// NewHTTPHost returns an empty host whose client never follows a redirect.
func NewHTTPHost() *HTTPHost {
	return &HTTPHost{held: map[dispatch.Ref]target{},
		client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}}
}

// HTTP is NewHTTPHost plus one pinned target: the common test shape.
func HTTP(base string, auth http.Header) (*HTTPHost, dispatch.Ref) {
	h := NewHTTPHost()
	return h, h.Pin(base, auth)
}

// Pin holds base and auth until Release, and returns their Ref.
func (h *HTTPHost) Pin(base string, auth http.Header) dispatch.Ref {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	ref := dispatch.Ref("http:" + strconv.Itoa(h.next))
	h.held[ref] = target{base: strings.TrimRight(base, "/"), auth: auth.Clone()}
	return ref
}

// AnthropicAuth is the header an Anthropic-dialect key goes in: x-api-key, or a bearer token when
// scheme is "bearer" (gateways).
func AnthropicAuth(key, scheme string) http.Header {
	if scheme == "bearer" {
		return BearerAuth(key)
	}
	return http.Header{"X-Api-Key": {key}}
}

// BearerAuth is Authorization: Bearer key.
func BearerAuth(key string) http.Header { return http.Header{"Authorization": {"Bearer " + key}} }

func (h *HTTPHost) Do(ctx context.Context, req dispatch.Request) (dispatch.Response, error) {
	h.mu.Lock()
	t, ok := h.held[req.Ref]
	h.mu.Unlock()
	if !ok {
		return dispatch.Response{}, dispatch.ErrNotHeld
	}
	method := req.Method
	if method == "" {
		method = http.MethodPost
	}
	// A private copy: the caller zeroes req.Body when Do returns, and the transport may still be
	// reading.
	hreq, err := http.NewRequestWithContext(ctx, method, t.base+req.Path,
		bytes.NewReader(append([]byte(nil), req.Body...)))
	if err != nil {
		return dispatch.Response{}, err
	}
	for name, vs := range req.Header {
		hreq.Header[name] = append([]string(nil), vs...)
	}
	for name, vs := range t.auth {
		hreq.Header[name] = append([]string(nil), vs...)
	}
	resp, err := h.client.Do(hreq)
	if err != nil {
		return dispatch.Response{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, dispatch.MaxResponseBytes))
	if err != nil {
		return dispatch.Response{}, err
	}
	return dispatch.Response{Status: resp.StatusCode, Header: resp.Header.Clone(), Body: body}, nil
}

func (h *HTTPHost) Release(ref dispatch.Ref) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.held, ref)
}

// Price knows no model: a test that needs rates uses Fake.
func (h *HTTPHost) Price(context.Context, string, string, int64) (dispatch.Price, bool) {
	return dispatch.Price{}, false
}
