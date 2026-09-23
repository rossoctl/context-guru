package main

import "testing"

// Every case below is a reply shape MEASURED on this corpus or on the gateway, not an invented one.
// The taxonomy exists because the first live run of this tool folded all four into "no verdicts" and
// reported failures=0 while 17 of 30 batches never produced a judgement.
func TestClassifyReplySeparatesTheFourOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		reply      string
		candidates int
		want       string
		wantN      int
	}{{
		name:       "a complete verdict array is the only measurement",
		reply:      `[{"i":0,"needed_by":"none","verdict":"drop"},{"i":1,"needed_by":"none","verdict":"keep"}]`,
		candidates: 2, want: replyOK, wantN: 2,
	}, {
		// The shape that produced the 94-row file: prose, no array at all. Remedy is the prompt.
		name:       "prose with no array is unparseable, not truncated",
		reply:      "I have considered the outputs but will not produce a verdict array here.",
		candidates: 3, want: replyUnparseable,
	}, {
		// Remedy is the opposite -- raise max_tokens -- so it must not be reported as unparseable.
		name:       "an array opened and never closed is truncated",
		reply:      `[{"i":0,"needed_by":"none","verdict":"drop"},{"i":1,"needed_by":"a","quote":"I will run the su`,
		candidates: 3, want: replyTruncated,
	}, {
		// The QUIET failure: it parses, so nothing errors, but the omitted candidates would read as
		// deliberate keeps in a scorer that defaults absent to keep.
		name:       "a short but valid array is partial",
		reply:      `[{"i":0,"needed_by":"none","verdict":"drop"}]`,
		candidates: 6, want: replyPartial, wantN: 1,
	}, {
		// A deliberate keep-all. It MUST NOT read as junk: ParseVerdicts documents this distinction
		// as the one a live arm turned on.
		name:       "a closed empty array is a decision, not a failure",
		reply:      `[]`,
		candidates: 0, want: replyOK,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			vs, kind := classifyReply(tc.reply, tc.candidates)
			if kind != tc.want {
				t.Errorf("kind = %q, want %q", kind, tc.want)
			}
			if len(vs) != tc.wantN {
				t.Errorf("verdicts = %d, want %d", len(vs), tc.wantN)
			}
		})
	}
}

// An unparseable reply must yield NO verdicts, so the caller cannot write decisions derived from a
// reply it already classified as junk.
//
// HONEST LIMIT ON THIS TEST: it cannot fail against today's code, and a mutation run confirmed that
// -- changing `return nil, replyUnparseable` to `return vs, replyUnparseable` leaves every test
// green, because `ParseVerdicts` itself returns `nil, false` on failure, so the two lines are
// identical. It is kept as a contract assertion on that callee: if ParseVerdicts is ever changed to
// salvage a partial slice from a reply it rejects, this is what trips. Read it as documentation of a
// dependency, NOT as evidence that classifyReply drops verdicts on its own.
func TestAnUnparseableReplyYieldsNoVerdicts(t *testing.T) {
	for _, r := range []string{
		"no array here",
		`[{"i":0,"needed_by":"none","verdict":"dr`,
	} {
		if vs, kind := classifyReply(r, 4); len(vs) != 0 {
			t.Errorf("%q classified %s but returned %d verdicts", r, kind, len(vs))
		}
	}
}
