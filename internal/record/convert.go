package record

// Offline conversion: exported OTLP spans -> claude-record corpus files.
//
// The proxy exports OTLP and nothing else. The corpus shape (otel.go) is what Inference-Perf's
// OTel trace replay and the analysis scripts read, so it is made here, after the fact, from the
// exported spans. Two pieces of work moved here from the proxy:
//
//   - the corpus rendering itself: original session id as trace_id, tool-call arguments as
//     strings, service.name claude-code, ISO times;
//   - turn reconstruction. It needs every call of a session at once, which a streaming
//     exporter never has, and the spans already carry the request bodies it reads.
//
// The OTLP-only attributes (session.id, gen_ai.provider.name, the OpenInference llm.* and
// input/output.* keys) are dropped, so a converted file carries exactly the attributes the
// corpus has always had, and the analysis side sees no difference.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
)

// otlpOnly are the attributes ToOTLP adds on top of the corpus attributes; the converter drops
// them again.
var otlpOnly = map[string]bool{
	AttrSessionID: true, AttrProviderName: true,
	AttrOISpanKind: true, AttrOIModelName: true, AttrOIProvider: true,
	AttrOITokenPrompt: true, AttrOITokenComplete: true, AttrOITokenTotal: true,
	AttrOITokenCacheRead: true, AttrOITokenCacheNew: true,
	AttrOIInputValue: true, AttrOIInputMime: true, AttrOIOutputValue: true, AttrOIOutputMime: true,
}

// Session is one session's spans, grouped by the converter.
type Session struct {
	ID       string // the original session key, from session.id
	TenantID string // from the resource, "" single-tenant
	Spans    []ReadSpan
}

// GroupSessions groups spans by session.id (falling back to the OTLP trace id for a span that
// has none), in order of first appearance, with each session's spans sorted by start time.
// Spans that are not recorded turns (no gen_ai.operation.name) are skipped: a collector's file
// can hold other services' spans too.
func GroupSessions(spans []ReadSpan) []Session {
	idx := map[string]int{}
	var out []Session
	for _, s := range spans {
		if _, ok := s.Attributes[AttrOperation]; !ok {
			continue
		}
		id, _ := s.Attributes[AttrSessionID].(string)
		if id == "" {
			id = s.TraceID
		}
		tenant, _ := s.Resource[ResTenant].(string)
		key := tenant + "\x00" + id
		i, ok := idx[key]
		if !ok {
			i = len(out)
			idx[key] = i
			out = append(out, Session{ID: id, TenantID: tenant})
		}
		out[i].Spans = append(out[i].Spans, s)
	}
	for i := range out {
		sp := out[i].Spans
		sort.SliceStable(sp, func(a, b int) bool { return sp[a].Start.Before(sp[b].Start) })
	}
	return out
}

// CorpusSpan maps one exported span back to the corpus shape.
func CorpusSpan(sessionID string, s ReadSpan) Span {
	at := make(map[string]any, len(s.Attributes))
	for k, v := range s.Attributes {
		if otlpOnly[k] {
			continue
		}
		// OTLP JSON carries every integer as int64; the corpus writes plain ints. Same value,
		// and json.Marshal renders both identically, but keep the type the corpus code uses.
		if n, ok := v.(int64); ok {
			v = int(n)
		}
		at[k] = v
	}
	for _, k := range []string{AttrInputMsgs, AttrOutputMsgs} {
		if v, ok := at[k].(string); ok {
			at[k] = CorpusMessages(v)
		}
	}
	st := Status{Code: StatusOK}
	if s.StatusError != "" {
		st = Status{Code: StatusError, Message: s.StatusError}
	}
	return Span{
		TraceID:      sessionID,
		SpanID:       s.SpanID,
		ParentSpanID: s.ParentSpanID,
		Name:         s.Name,
		Kind:         SpanKindClient,
		StartTime:    FormatTime(s.Start),
		EndTime:      FormatTime(s.End),
		Attributes:   at,
		ResourceAttr: map[string]any{"service.name": "claude-code"},
		Status:       st,
	}
}

// ConvertSession renders one session as a corpus file: its observed spans, then one derived
// span per reconstructed human turn.
//
// Fails open, as the proxy-side reconstruction did: if reconstruction panics, the file holds
// the observed spans only. A file missing its derived turns is a degraded file; a missing file
// is a lost session.
func ConvertSession(sess Session, collectedAt time.Time) File {
	spans := make([]Span, 0, len(sess.Spans))
	for _, s := range sess.Spans {
		spans = append(spans, CorpusSpan(sess.ID, s))
	}
	return NewFile(sess.ID, withDerivedTurns(sess.ID, spans), collectedAt)
}

func withDerivedTurns(sessionID string, spans []Span) (out []Span) {
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
		// The span's start time sorts chronologically, and it is also the closest thing to a
		// timestamp a reconstructed turn can honestly carry.
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
			continue // FirstSeen is a start time written above; skip rather than invent one
		}
		res = append(res, DerivedTurnSpan(sessionID, derivedSpanID(sessionID, t.Sequence, t.Root), "", t, start))
	}
	return res
}

// derivedSpanID is a stable 8-byte id for one reconstructed turn, so converting a session twice
// gives the same ids.
func derivedSpanID(sessionID string, seq int, root string) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + root + "\x00" + strconv.Itoa(seq)))
	return hex.EncodeToString(sum[:8])
}

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
	if s == "" || strings.TrimLeft(s, ".") == "" {
		return "cg-unkeyed"
	}
	return s
}
