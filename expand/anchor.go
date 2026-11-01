package expand

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Fixed restore (#407): the host half of giving expanded content back WITHOUT changing the
// cached prefix.
//
// Kept-verbatim restores an expanded output by un-compacting the original at its own position,
// which is a byte change deep inside the provider's cached prefix: the whole suffix is
// cache-written again (measured on session e8f16627, request 3556: $0.615 to restore ~300
// tokens). Fixed restore leaves the original compacted and puts the content in a SEPARATE
// message, labelled with the same <<cg:HASH>> marker, at a fixed point right after the turn that
// asked for it — an Anchor. Every later turn re-inserts the same bytes at the same index, so the
// only new bytes are the ones after the expanding turn, which were never cached anyway.
//
// It is needed even where the expand call never reached the client. The in-band continuation
// answers context_guru_expand inside the request (proxy.serve), so the client's next turn holds
// no tool_use and no tool_result: without an inserted copy, the model would be back to the bare
// marker and expand it again on every turn (#201's loop).
//
// An Anchor is stated in the CLIENT's coordinates — the messages (or Responses `input` items) the
// client sends, before any rewrite — because those are the only coordinates that stay put as the
// conversation grows: the client re-sends its history verbatim and appends to it.

// Anchor is one restored original's fixed place in a session's transcript.
type Anchor struct {
	// ID is the marker id the agent expanded; its stash holds the original.
	ID string `json:"id"`
	// At is the client message index the restored copy is inserted BEFORE: the first message
	// after the turn that expanded it. At == len(messages) appends.
	At int `json:"at"`
	// Fp fingerprints the client message at At-1, so a transcript that no longer has that
	// message there (the agent's own compaction, /compact, an edited history) is recognised
	// and the anchor is not used.
	Fp string `json:"fp"`
}

func anchorKey(session string) string { return store.RestorePrefix + session }

// MessagesField is the body field where a dialect keeps its transcript: Responses `input`,
// every other dialect `messages`. The one definition the anchor code and apply share.
func MessagesField(wire string) string {
	if wire == "responses" {
		return "input"
	}
	return "messages"
}

// Fingerprint identifies one wire message independently of what a client changes on an old
// message without changing what the model reads: the hash of its canonical form
// (schema.CanonicalWireMessage, #783).
//
// `cache_control` is excluded because it marks the LAST message of each request, so the message
// an anchor points at carries it on the turn the anchor is recorded and not afterwards. A content
// of one plain text block reads as its string because Claude Code sends that same message as a
// string once a later turn follows it (seen on 2.1.312 in the #485 probes). Everything else must
// match. Keys are sorted, so the client's key order does not count. Numbers are kept as their
// literal text: decoded as float64, two integers above 2^53 that differ would read as one, and so
// would the two messages.
//
// A stored fingerprint is compared with fingerprintMatches, not with ==.
func Fingerprint(msg gjson.Result) string {
	b, ok := schema.CanonicalWireMessage(msg.Raw)
	if !ok {
		return ""
	}
	return fingerprintOf(b)
}

func fingerprintOf(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:12])
}

// fingerprintMatches reports whether msg is the message a stored fingerprint fp names. Besides
// Fingerprint it accepts the two values the rule before #783 (cache_control removed, nothing else)
// gives the same message: msg as it is, and msg with a string content written as one text block.
// The first keeps every anchor recorded before the upgrade over a block that is still a block.
// The second matches an anchor recorded over the block form of a message the client now sends as
// a string, the case the old rule missed. Neither accepts a message Fingerprint would tell apart:
// all three forms are the same message under the canonical rule.
func fingerprintMatches(msg gjson.Result, fp string) bool {
	if fp == "" {
		return false
	}
	if Fingerprint(msg) == fp {
		return true
	}
	for _, raw := range [...]string{msg.Raw, asTextBlock(msg)} {
		if raw == "" {
			continue
		}
		if b, ok := legacyCanonical(raw); ok && fingerprintOf(b) == fp {
			return true
		}
	}
	return false
}

// legacyCanonical is the canonical form before #783: cache_control removed at every depth, keys
// sorted, numbers as their literal text.
func legacyCanonical(raw string) ([]byte, bool) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	b, err := json.Marshal(stripCacheControl(v))
	return b, err == nil
}

// asTextBlock returns msg with a string `content` written as one text block, or "" when its
// content is not a string.
func asTextBlock(msg gjson.Result) string {
	c := msg.Get("content")
	if c.Type != gjson.String {
		return ""
	}
	out, err := sjson.SetRaw(msg.Raw, "content", `[{"type":"text","text":`+c.Raw+`}]`)
	if err != nil {
		return ""
	}
	return out
}

func stripCacheControl(v any) any {
	switch t := v.(type) {
	case map[string]any:
		delete(t, "cache_control")
		for k, x := range t {
			t[k] = stripCacheControl(x)
		}
	case []any:
		for i, x := range t {
			t[i] = stripCacheControl(x)
		}
	}
	return v
}

// AnchorPoint is the anchor for an expand answered INSIDE this request (the in-band
// continuation): right after the request's last message, which is where the model's reply —
// the turn that expanded — will sit in the client's next request. body is the request as the
// client sent it. ok is false when the transcript is not one an anchor can be stated in.
func AnchorPoint(wire string, body []byte) (at int, fp string, ok bool) {
	if wire == "responses" && gjson.GetBytes(body, "previous_response_id").String() != "" {
		return 0, "", false // history held upstream: these items are not the transcript
	}
	msgs := gjson.GetBytes(body, MessagesField(wire)).Array()
	if len(msgs) == 0 || !mayPrecede(wire, msgs[len(msgs)-1]) {
		return 0, "", false
	}
	fp = Fingerprint(msgs[len(msgs)-1])
	return len(msgs), fp, fp != ""
}

// RecordAnchor stores a for session, unless an anchor for the same id is already recorded: the
// FIRST place the content came back is where it stays, so a repeated expand of the same id does
// not move it.
func RecordAnchor(st store.Store, session string, a Anchor) {
	if session == "" || a.ID == "" || a.Fp == "" || a.At < 1 {
		return
	}
	as := Anchors(st, session)
	for _, x := range as {
		if x.ID == a.ID {
			return
		}
	}
	if b, err := json.Marshal(append(as, a)); err == nil {
		st.Put(anchorKey(session), b)
	}
}

// Anchors returns session's recorded anchors in the order they were recorded. An unreadable
// record reads as none — the fail-open direction, kept-verbatim in place.
func Anchors(st store.Store, session string) []Anchor {
	b, ok := st.Get(anchorKey(session))
	if !ok {
		return nil
	}
	var as []Anchor
	if json.Unmarshal(b, &as) != nil {
		return nil
	}
	return as
}

// Usable reports whether a still names the same place in msgs, the client's transcript this
// turn: the message before it is the one fingerprinted, and inserting there keeps the
// transcript well formed.
func (a Anchor) Usable(wire string, msgs []gjson.Result) bool {
	if a.At < 1 || a.At > len(msgs) {
		return false
	}
	if !mayPrecede(wire, msgs[a.At-1]) || !fingerprintMatches(msgs[a.At-1], a.Fp) {
		return false
	}
	return a.At == len(msgs) || mayFollow(wire, msgs[a.At])
}

// mayPrecede: a user-role message may follow m. Not an assistant turn (the inserted message
// would split it from its tool results, or end a prefill) nor a Responses call or reasoning
// item, which belongs to the output that follows it.
func mayPrecede(wire string, m gjson.Result) bool {
	if m.Get("role").String() == "assistant" {
		return false
	}
	switch m.Get("type").String() {
	case "function_call", "custom_tool_call", "reasoning":
		return false
	}
	return true
}

// mayFollow: a user-role message may precede m. Not a tool answer, which must sit right after
// the call it answers.
func mayFollow(wire string, m gjson.Result) bool {
	if m.Get("role").String() == "tool" {
		return false
	}
	switch m.Get("type").String() {
	case "function_call_output", "custom_tool_call_output":
		return false
	}
	return true
}

// CallSite is one context_guru_expand call the CLIENT received and answered (the repair path,
// RepairToolResults): the assistant message holding the call is the turn that expanded, so the
// anchor sits right before it.
type CallSite struct {
	ID string
	// At is the index of the message holding the call.
	At int
	// Answer is what the transcript now carries as that call's result, after the repair.
	Answer string
}

// CallSites lists the expand calls in a transcript, first call per id.
func CallSites(wire string, body []byte) []CallSite {
	msgs := gjson.GetBytes(body, MessagesField(wire)).Array()
	answers := map[string]string{} // call id -> result text
	for _, m := range msgs {
		switch {
		case wire == "responses":
			if t := m.Get("type").String(); t == "function_call_output" || t == "custom_tool_call_output" {
				answers[m.Get("call_id").String()] = m.Get("output").String()
			}
		case wire == "anthropic":
			for _, blk := range m.Get("content").Array() {
				if blk.Get("type").String() == "tool_result" {
					answers[blk.Get("tool_use_id").String()] = blk.Get("content").String()
				}
			}
		default:
			if m.Get("role").String() == "tool" {
				answers[m.Get("tool_call_id").String()] = m.Get("content").String()
			}
		}
	}
	var out []CallSite
	seen := map[string]bool{}
	add := func(at int, callID, id string) {
		if id == "" || callID == "" || seen[id] {
			return
		}
		ans, ok := answers[callID]
		if !ok {
			return
		}
		seen[id] = true
		out = append(out, CallSite{ID: id, At: at, Answer: ans})
	}
	for i, m := range msgs {
		switch {
		case wire == "responses":
			if t := m.Get("type").String(); (t == "function_call" || t == "custom_tool_call") && m.Get("name").String() == ToolName {
				args := m.Get("arguments").String()
				if t == "custom_tool_call" {
					args = m.Get("input").String()
				}
				add(i, m.Get("call_id").String(), gjson.Get(args, "id").String())
			}
		case wire == "anthropic":
			for _, blk := range m.Get("content").Array() {
				if blk.Get("type").String() == "tool_use" && blk.Get("name").String() == ToolName {
					add(i, blk.Get("id").String(), blk.Get("input.id").String())
				}
			}
		default:
			for _, tc := range m.Get("tool_calls").Array() {
				if tc.Get("function.name").String() == ToolName {
					add(i, tc.Get("id").String(), gjson.Get(tc.Get("function.arguments").String(), "id").String())
				}
			}
		}
	}
	return out
}

// RestoredText is the restored copy as the model reads it: the marker it expanded, said to be
// answered, then the original. A pure function of (id, original), so every turn inserts the
// same bytes.
func RestoredText(id, original string) string {
	return "[context-guru: the full original of " + Marker(id) + " follows; it is already expanded, " +
		"do not call " + ToolName + " for it again]\n" + original
}

// restoredMessage is RestoredText as one user-role message in the dialect's wire shape.
func restoredMessage(wire, text string) []byte {
	var v any
	switch wire {
	case "anthropic":
		v = map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}}
	case "responses":
		v = map[string]any{"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": text}}}
	default:
		v = map[string]any{"role": "user", "content": text}
	}
	b, _ := json.Marshal(v)
	return b
}

// Insertion is one restored copy to splice in, at an index of the body it is spliced into.
type Insertion struct {
	At   int
	Text string
}

// InsertRestored splices the restored copies into body's transcript, each before the message at
// its At (or at the end, for At == len). Insertions sharing an index keep their given order.
// Every other message keeps its exact bytes. ok is false, with body unchanged, on any shape it
// cannot splice.
func InsertRestored(wire string, body []byte, ins []Insertion) ([]byte, bool) {
	if len(ins) == 0 {
		return body, true
	}
	field := MessagesField(wire)
	msgs := gjson.GetBytes(body, field)
	if !msgs.IsArray() {
		return body, false
	}
	arr := msgs.Array()
	before := make(map[int][][]byte, len(ins))
	for _, in := range ins {
		if in.At < 0 || in.At > len(arr) {
			return body, false
		}
		before[in.At] = append(before[in.At], restoredMessage(wire, in.Text))
	}
	var buf bytes.Buffer
	buf.WriteByte('[')
	n := 0
	emit := func(b []byte) {
		if n > 0 {
			buf.WriteByte(',')
		}
		buf.Write(b)
		n++
	}
	for i := 0; i <= len(arr); i++ {
		for _, b := range before[i] {
			emit(b)
		}
		if i < len(arr) {
			emit([]byte(arr[i].Raw))
		}
	}
	buf.WriteByte(']')
	out, err := sjson.SetRawBytes(body, field, buf.Bytes())
	if err != nil {
		return body, false
	}
	return out, true
}

// itemSpan reports whether a stashed original is a span of native Responses items rather than
// text (see responsesContentPresent): it cannot be given back as one text message.
func itemSpan(original string) bool {
	r := gjson.Parse(original)
	return r.IsArray() && len(r.Array()) > 0 && r.Array()[0].Get("type").Exists()
}

// Restorable reports whether original can be restored as a text message on wire.
func Restorable(wire, original string) bool {
	return original != "" && !(wire == "responses" && itemSpan(original))
}
