package record

// Helpers shared by the proxy's recorder and anyone reading its output offline.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// SafeFileName makes a session id safe as a file name on every platform, NTFS included: a ":"
// there silently creates an alternate data stream instead of a file.
func SafeFileName(id string) string {
	id = strings.TrimSpace(id)
	var b strings.Builder
	for _, ch := range id {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9',
			ch == '.', ch == '_', ch == '-':
			b.WriteRune(ch)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	// "." and ".." are directory entries, not names; an empty id would yield a hidden file
	// that also merges every unkeyed session into one.
	if s == "" || strings.TrimLeft(s, ".") == "" {
		return "cg-unkeyed"
	}
	return s
}

// WithDerivedTurns returns a session's observed spans followed by one derived span per human turn
// rebuilt from their request bodies. Wire does not store turns; this is how a reader of wire gets
// them, with the same reconstruction the proxy used to run inline.
//
// Turn ids come from the session, the conversation root and the sequence number, so rebuilding a
// session twice gives the same ids. Fails open: if reconstruction panics, the observed spans are
// returned unchanged.
func WithDerivedTurns(sessionID string, spans []Span) (out []Span) {
	out = spans
	defer func() {
		if recover() != nil {
			out = spans
		}
	}()
	calls := make([]Call, 0, len(spans))
	for _, sp := range spans {
		raw, _ := sp.Attributes[AttrInputMsgs].(string)
		if raw == "" {
			continue
		}
		var msgs []Message
		if err := json.Unmarshal([]byte(raw), &msgs); err != nil {
			continue
		}
		defs, _ := sp.Attributes[AttrToolDefs].(string)
		calls = append(calls, Call{Order: sp.StartTime, Messages: msgs, ToolDefs: defs})
	}
	turns := ReconstructTurns(calls)
	if len(turns) == 0 {
		return spans
	}
	res := make([]Span, 0, len(spans)+len(turns))
	res = append(res, spans...)
	for _, t := range turns {
		start, err := time.Parse(time.RFC3339Nano, t.FirstSeen)
		if err != nil {
			continue // FirstSeen is a span start time; skip rather than invent one
		}
		res = append(res, DerivedTurnSpan(sessionID, derivedSpanID(sessionID, t.Sequence, t.Root), "", t, start))
	}
	return res
}

func derivedSpanID(sessionID string, seq int, root string) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + root + "\x00" + strconv.Itoa(seq)))
	return hex.EncodeToString(sum[:8])
}
