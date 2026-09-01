// Package schema holds the helpers this project layers over bifrost's message schema:
// token counting, deep-clone, MessageText/SetMessageText, Rewritable, ToolCalls (which
// pairs a tool result with the call that produced it), SessionHead, and static
// message-SHAPE validation.
//
// # Message-shape validation
//
// ValidateShapeFor checks a message list against the provider's message-SHAPE rules — the
// rules that make a request well-formed regardless of its content.
//
// WHY THIS EXISTS. Four separate shape violations shipped in `summarize` and every one of
// them was found REACTIVELY, by a live provider rejection or a benchmark failure, each
// masked by the one before it:
//
//	2edb9d4  400 messages.1: role 'system' must precede an 'assistant' message or end the array
//	fb5c460  400 messages.N.content.M: unexpected `tool_use_id` found in `tool_result` blocks
//	e7d1aa8  400 messages.N: `tool_use` ids were found without `tool_result` blocks immediately after
//	e9bf3a7  panic: index out of range [-1]   (a short transcript; see NOT CHECKED below)
//
// None was findable by the project's existing methods. No test asserted the shape of what a
// component emitted, and every offline measurement replayed through the `/compact` endpoint,
// which runs the pipeline and returns the rewritten body WITHOUT forwarding it upstream — so
// no provider ever validated it. Replay can tell you what a component removed; it is
// structurally incapable of telling you whether the result is a sendable request.
//
// The first three are checkable statically, with no provider and no model, because they are
// properties of the message list alone. That is what this is for: a component that mutates a
// message list can be asserted well-formed in a unit test, closing the blind spot without
// paying for live traffic on every change.
//
// WHERE IT RUNS. Unit tests, over the pipeline's NORMALIZED view of a transcript — the shape
// apply.normalize produces and components mutate, where an Anthropic `tool_use` block has
// been lifted into bifrost's ToolCalls and each `tool_result` block is its own synthetic
// role=tool message.
//
// It is deliberately NOT on the request hot path, but NOT because of cost. Measured on the
// eval box:
//
//	BenchmarkValidateShape166-16     25543 ns/op    9495 B/op   121 allocs/op
//	BenchmarkValidateShape500-16     72796 ns/op   35492 B/op   357 allocs/op
//	BenchmarkValidateShape5000-16   801821 ns/op  307919 B/op  3508 allocs/op
//
// 73 µs on a 500-message transcript is ~0.007% of a one-second provider call, and less than
// this package's own tokenizer plus apply.normalize already cost per request. Nor is
// "fail-open" a reason to stay off: fail-open IS a decision — validate the compacted body,
// revert to the original on violation, forward that. It converts a guaranteed provider 400
// into a silently-lost saving, which is the trade this repo makes everywhere else.
//
// The real blocker is the DIALECT GATE, and it is why ValidateShapeFor takes a provider.
// system-position is an Anthropic rule; OpenAI imposes no positional constraint on
// system/developer messages, and `/compact` defaults to OpenAI (proxy/proxy.go:566). Wired to
// the hot path with that rule ungated, every OpenAI request whose client re-injects a system
// message mid-array would be reverted and its saving silently lost — a savings regression
// dressed as a safety check. With the gate in place the remaining work is a post-pipeline
// check in apply.Body that reverts on violation. That is a separate change.
//
// NOT CHECKED, and why — so nobody "fixes" these:
//
//   - CONSECUTIVE SAME-ROLE messages. Must NOT be checked. `summarize`'s own legal output is
//     [msgs[0], summary(user), user-tail...] — consecutive user messages. An alternation rule
//     would reject the very output this validator exists to bless, and Anthropic accepts it.
//   - e9bf3a7's PANIC inside boundary arithmetic. No check on the output list can see it: by
//     the time there is a list to inspect the panic has already happened (and
//     pipeline.runOne swallowed it into verdict=reverted). Shape validation is not a
//     substitute for exercising a component on transcripts shorter than its own thresholds.
//   - role="tool" reaching the Anthropic wire. Structurally invisible here, because
//     apply.normalize maps a legal wire and one carrying the illegal role onto the IDENTICAL
//     normalized list. It is a raw-BYTES property, so it is checked on the bytes, by
//     apply/toolrole_wire_test.go and by the role predicate in apply/shape_validate_test.go.
//   - ORPHANED `<<cg:HASH>>` markers. Deciding one needs the expansion Store, and a Store
//     dependency does not belong in this package.
//   - FIRST message not user, and a TRAILING assistant message. Neither is a hard provider
//     rejection, and prefill (a trailing assistant turn) is a supported Anthropic feature.
//   - Token limits, model names, sampling parameters, or anything content-dependent. This is
//     a shape validator, not a request validator.
package schema
