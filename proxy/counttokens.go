package proxy

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
)

// countTokensPath is the upstream route this forwards to, and the suffix Claude Code appends
// to whatever ANTHROPIC_BASE_URL it was given.
const countTokensPath = "/v1/messages/count_tokens"

// countTokens forwards POST /v1/messages/count_tokens to the Anthropic upstream, verbatim.
//
// **Why it has to exist.** Claude Code asks the API how many tokens its context is worth, to
// decide when to compact. Route the client through a gateway that does not serve this endpoint
// and the fallback is not a warning — the client works it out by issuing INFERENCE requests
// instead. On a funnel whose entire pitch is "this makes your sessions cheaper", the absence of
// a cheap endpoint silently adds billed calls. Cheap to add; expensive to leave out.
//
// **Why the body is forwarded UNCHANGED, with no pipeline.** The client is asking about the
// context IT holds, and it uses the answer to manage its own transcript. Handing back a count
// for a compacted body would answer a question nobody asked and would make the client's own
// budgeting wrong in the direction that hurts — it would believe it has more room than it does,
// and only find out at a 400 on the real request. So: no components, no cache split, no
// rewriting. The count is the truth about what the client sent.
//
// The response is relayed through h.stream, which copies status, headers and body byte for byte
// — the same path the chat routes use, and the reason an upstream error here reaches the client
// with the upstream's own wording intact.
func (h *Handler) countTokens(static upstream) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		up := static
		if h.opts.Tenants != nil {
			// Hosted mode: authenticate, meter and resolve the tenant's own upstream, exactly
			// as the catch-all does. Without this the route would be an unmetered open
			// forwarder that also leaked OUR token to the upstream in place of the tenant's.
			tn, err := h.authenticate(r)
			if err != nil {
				h.refuse(w, r, err)
				return
			}
			release, ok := h.meter(w, r, tn)
			defer release()
			if !ok {
				return
			}
			if up, err = h.upstreamFor(r, tn, pickAnthropic, static); err != nil {
				h.refuseRoute(w, r, tn, err)
				return
			}
		}
		if up.base == "" {
			recordRefusal(refuseNoUpstream, "")
			http.Error(w, "no upstream configured", http.StatusBadGateway)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "count_tokens: unreadable request body", http.StatusBadRequest)
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
			up.base+countTokensPath, bytes.NewReader(body))
		if err != nil {
			http.Error(w, "proxy: "+err.Error(), http.StatusBadGateway)
			return
		}
		copyHeaders(req.Header, r.Header)
		setUpstreamAuth(req.Header, up)

		resp, err := h.client.Do(req)
		if err != nil {
			recordRefusal(refuseUpstream, "")
			// The fixed string, not err.Error(): a *url.Error stringifies as
			// `Post "<the full upstream URL>": ...`, which would publish the operator's
			// upstream address — and any userinfo in it — to every caller. Detail goes to
			// the log, where the operator can see it and the caller cannot.
			slog.Warn("context-guru: count_tokens upstream call failed", "err", err)
			http.Error(w, "upstream request failed", http.StatusBadGateway)
			return
		}
		h.stream(w, resp)
	}
}

// anthropicCountTokensUpstream is the single-tenant upstream for the route above: the same
// base and the same credential handling as POST /anthropic/v1/messages, so a proxy configured
// with an API key injects it here too, and one configured without forwards the caller's own.
func (h *Handler) anthropicCountTokensUpstream() upstream {
	return upstream{
		base:   h.opts.AnthropicUpstream,
		path:   countTokensPath,
		setKey: headerKey("x-api-key", h.opts.AnthropicKey),
	}
}
