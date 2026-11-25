// Package inband is the in-band loop: the host answers a call to one of our own tools
// (expand, adjudicate, tool search) itself, so the client never sees the call.
//
// Known no-op: Responses over a WebSocket. Some clients send Responses over a WebSocket.
// Codex does (codex-api/src/endpoint/responses_websocket.rs, openai/codex f64f003). A
// WebSocket connection does not go through the HTTP host. On that path nothing in this package
// runs: no tool is injected, nothing is offloaded, and no call is expanded. This is safe,
// because nothing lossy happens there. A marker needs an offload, and an offload needs the
// HTTP host. The client works as it would without Context Guru.
//
// Through the proxy, the case does not arise for Codex: the proxy answers a WebSocket upgrade
// on the Responses route with 426, and Codex falls back to HTTPS (see Handler.Mux in proxy).
// The no-op applies when a client reaches the provider by a route that has no HTTP host.
package inband
