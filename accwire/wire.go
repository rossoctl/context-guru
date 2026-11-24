// Package accwire reads a request body for the accountant, one adapter per wire.
//
// The accountant library is wire-agnostic: it takes a Body of hashed positions, cache-mark
// breakpoints and operations. This package turns a provider request into those. Each wire reads
// its own shape in four places, and they all sit behind one Wire value, chosen once per request:
//
//   - Body and Parse cut a body into tools, system and messages (the hash space of the positions);
//   - Breakpoints finds the cache marks;
//   - EnvelopeOps and MessageOps report the edits that no pipeline component reports;
//   - WireIndex maps a pipeline slot path to the wire position it sits in.
//
// A new wire (accountant#17) adds one kind here, and no other place. The package is a library:
// it imports no host package (proxy, proxyapp, dash).
//
// The write side stays out. apply's slot types rewrite the request that goes upstream, so only
// this read side sits behind the interface (#1164).
package accwire

import (
	"strconv"
	"strings"
	"unsafe"

	"github.com/tidwall/gjson"

	"github.com/rossoctl/accountant"
)

// Wire is the accountant's reader for one provider wire.
type Wire interface {
	// Name is "messages" or "responses".
	Name() string
	// Body hashes a request body in the provider's order: tools, system, messages. A body without a
	// conversation (or with invalid JSON) gives an empty Body.
	Body(raw []byte) accountant.Body
	// Parse is Body and Breakpoints on one parse of the body. count prices each thinking block the
	// proxy saw a model make (accountant design D16); nil gives every block the fixed price. The
	// hashes and sizes do not depend on it.
	Parse(raw []byte, count accountant.OpaqueCount) (accountant.Body, []accountant.Breakpoint)
	// Breakpoints lists the cache marks as positions in the same space as Body.
	Breakpoints(raw []byte) []accountant.Breakpoint
	// EnvelopeOps lists the operations that explain the tools and system sections' difference
	// between the client body and the forwarded body. It is nil when either has no conversation.
	EnvelopeOps(client, forwarded []byte) []accountant.Operation
	// MessageOps explains a change to the messages that an edit made AFTER the pipeline, with the
	// edit's own operator.
	MessageOps(operator string, before, after []byte) []accountant.Operation
	// WireIndex maps a normalized slot path, such as "messages.3.content.1.text" or
	// "input.3.content.1.text", to the wire position it sits in. ok is false when the path names
	// no position of this wire.
	WireIndex(slotPath string) (index int, ok bool)
}

// kind is the one implementation of Wire. The wires differ only in how they cut a body into
// sections (scan) and in how a slot path names a position (WireIndex).
type kind int

const (
	// messages is Anthropic Messages and Chat Completions. Both keep the conversation in a
	// "messages" array, and every read below treats them alike.
	messages kind = iota
	// responses is the OpenAI Responses wire: instructions is the system section and input holds
	// the messages (#899).
	responses
)

var (
	// Messages reads Anthropic Messages and Chat Completions bodies.
	Messages Wire = messages
	// Responses reads OpenAI Responses bodies.
	Responses Wire = responses
)

// For chooses the wire of a request body, once. A body with no messages array and an input (an
// array of items, or one string) is Responses. Every other body is Messages, which gives an empty
// Body when there is no conversation.
func For(raw []byte) Wire {
	// gjson.Get on the bytes read in place: ParseBytes would copy the whole body, and the answer is
	// only a type, so nothing keeps a pointer into raw. The scan is read-only.
	var s string
	if len(raw) > 0 {
		s = unsafe.String(&raw[0], len(raw))
	}
	if gjson.Get(s, "messages").IsArray() {
		return messages
	}
	if in := gjson.Get(s, "input"); in.IsArray() || in.Type == gjson.String {
		return responses
	}
	return messages
}

func (k kind) Name() string {
	if k == responses {
		return "responses"
	}
	return "messages"
}

func (k kind) Body(raw []byte) accountant.Body { return k.body(raw) }

func (k kind) Parse(raw []byte, count accountant.OpaqueCount) (accountant.Body, []accountant.Breakpoint) {
	return k.parse(raw, count)
}

func (k kind) Breakpoints(raw []byte) []accountant.Breakpoint { return k.breakpoints(raw) }

func (k kind) EnvelopeOps(client, forwarded []byte) []accountant.Operation {
	return k.envelopeOps(client, forwarded)
}

func (k kind) MessageOps(operator string, before, after []byte) []accountant.Operation {
	return k.messageOps(operator, before, after)
}

// WireIndex of a messages wire: "messages.<i>" and anything below it name message i.
// Of the Responses wire: "input" is the one position of a string input, "input.<i>..." is item i.
// The instructions are the system section, not a position, so they name none.
func (k kind) WireIndex(path string) (int, bool) {
	if k == responses {
		if path == "input" {
			return 0, true
		}
		parts := strings.SplitN(path, ".", 3)
		if len(parts) < 2 || parts[0] != "input" {
			return 0, false
		}
		i, err := strconv.Atoi(parts[1])
		return i, err == nil && i >= 0
	}
	s, found := strings.CutPrefix(path, "messages.")
	if !found {
		return 0, false
	}
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		s = s[:dot]
	}
	i, err := strconv.Atoi(s)
	return i, err == nil
}
