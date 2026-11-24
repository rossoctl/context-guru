// Package lifecycle is the order of the steps around one request, for every host.
//
// A host (the proxy, the bifrost adapter, the Cortex plugin) reads a request, calls the
// pipeline through apply, forwards the request and writes the answer. Several steps come before
// the pipeline, between it and the upstream call, and after the answer. Their ORDER matters:
// expand repair must run before the pipeline reads the transcript, the keep-alive arrival must
// run after the thread is known, tool deferral must see the final tools array. This package
// holds that order in code, so a host calls one function and cannot get it wrong.
//
// The host keeps the HTTP read, the upstream call, the response write and the credentials. It
// gives this package function values for the steps that need its own state (the keeper, the
// dashboard capture, the observe pool, tool deferral).
//
// It never imports proxy, proxyapp or dash. Every step fails open: Prepare recovers a panic
// and gives back the original body.
package lifecycle
