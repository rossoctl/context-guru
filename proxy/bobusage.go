package proxy

import (
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/tidwall/gjson"
)

// bobUsage corrects the shared parser's reading of a usage block from Bob's model route.
//
// Bob's gateway answers in OpenAI chat shape, but its block also carries the cache writes:
// prompt_tokens_details.cache_write_tokens and the per-TTL split under
// prompt_tokens_details.cache_creation_token_details. providerusage.Parse reads that shape as
// plain OpenAI chat, so it books every written token as fresh input and sees no 1h write.
// The correction stays here, on Bob's route only, so no other provider's reading moves.
//
// base is what the shared parser produced. Any field that is absent or does not add up
// returns base unchanged (fail open).
func bobUsage(u gjson.Result, base Usage) Usage {
	if !u.IsObject() {
		return base
	}
	prompt, ok := bobCount(u.Get("prompt_tokens"))
	if !ok {
		return base
	}
	var write int64
	found := false
	for _, p := range []string{"prompt_tokens_details.cache_write_tokens",
		"prompt_tokens_details.cache_creation_tokens", "cache_creation_input_tokens"} {
		if v := u.Get(p); v.Exists() {
			if write, ok = bobCount(v); !ok {
				return base
			}
			found = true
			break
		}
	}
	if !found {
		return base
	}
	var write1h int64
	if v := u.Get("prompt_tokens_details.cache_creation_token_details.ephemeral_1h_input_tokens"); v.Exists() {
		if write1h, ok = bobCount(v); !ok || write1h > write {
			return base
		}
	}
	if base.CacheRead+write > prompt {
		return base
	}
	out := base
	out.CacheWrite, out.CacheWrite1h = write, write1h
	out.FreshInput = prompt - base.CacheRead - write
	return out
}

// bobCount is a non-negative whole token count, or ok=false.
func bobCount(v gjson.Result) (int64, bool) {
	if v.Type != gjson.Number || v.Num < 0 || v.Num != float64(int64(v.Num)) {
		return 0, false
	}
	return int64(v.Num), true
}

// Bob's model route is served by at least 2 backends that count the same request differently (a
// chat's first request: about 21,000 tokens on A, 13,000 on B), so a comparison is valid only on
// one backend. Two marks that do not depend on each other tell them apart (9 replies on 2026-11-09,
// 58 on 2026-11-10):
//
//	A: the id starts with "chatcmpl-", and usage.prompt_tokens_details has cache_write_tokens.
//	B: the id does not start with "chatcmpl-", and the block has neither cache_write_tokens nor
//	   cache_creation_token_details. B ids vary in length and characters, so neither is checked.
//
// Both backends read the cache. IBM can change either mark at any time. So a reply where the
// marks disagree is "other" and never A or B. A reply with no id or no usage block (Bob's router
// helper calls) is "" and is no alarm.
const bobIDPrefixA = "chatcmpl-"

// bobMarks are the facts about one reply that tell Bob's backends apart. The id stays in memory
// for one call only: only the label from bobBackend is kept.
type bobMarks struct {
	id              string
	mixedIDs        bool // the chunks of one stream did not all carry the same id
	usage           bool
	cacheWrite      bool // usage.prompt_tokens_details.cache_write_tokens is present
	creationDetails bool // usage.prompt_tokens_details.cache_creation_token_details is present
	providerFields  bool // the top-level provider_specific_fields is present
	helper          bool // the reply names one of Bob's helper models (security, router)
}

// bobBackend is the backend label for a reply's marks: "A", "B", "other" or "".
func bobBackend(m bobMarks) string {
	if m.id == "" || !m.usage || m.helper {
		return ""
	}
	if m.mixedIDs {
		return "other"
	}
	if strings.HasPrefix(m.id, bobIDPrefixA) {
		if m.cacheWrite {
			return "A"
		}
		return "other"
	}
	// B ids vary in length and characters (22 and 23 chars, some with - or _, seen on 2026-11-10),
	// so only what B lacks is checked: the chatcmpl- start (above) and the cache-write fields.
	if !m.cacheWrite && !m.creationDetails {
		return "B"
	}
	return "other"
}

func bobAlnum(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9') {
			return false
		}
	}
	return true
}

// bobMarksOf reads a reply's usage block and its marks in one pass: the top level of a JSON body,
// or the `data:` chunks of an OpenAI chat event stream (the id of the first chunk that has one,
// and the last usage block). Anything it cannot read gives no marks (fail open).
func bobMarksOf(contentType string, body []byte) (gjson.Result, bobMarks) {
	var m bobMarks
	var usage gjson.Result
	note := func(doc gjson.Result) {
		if id := doc.Get("id"); id.Type == gjson.String && id.Str != "" {
			if m.id == "" {
				m.id = id.Str
			} else if id.Str != m.id {
				m.mixedIDs = true
			}
		}
		if doc.Get("provider_specific_fields").Exists() {
			m.providerFields = true
		}
		// Bob's helper calls name their own model. They are named here, not the coding tiers: a
		// user may pick premium, standard or fast, and every one of those must keep its label.
		switch doc.Get("model").String() {
		case "security", "router":
			m.helper = true
		}
		if u := doc.Get("usage"); usagePresent(u) {
			usage = u
		}
	}
	if !strings.Contains(contentType, "event-stream") {
		if doc := gjson.ParseBytes(body); doc.IsObject() {
			note(doc)
		}
	} else {
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			if doc := gjson.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:"))); doc.IsObject() {
				note(doc)
			}
		}
	}
	if usage.Exists() {
		m.usage = true
		m.cacheWrite = usage.Get("prompt_tokens_details.cache_write_tokens").Exists()
		m.creationDetails = usage.Get("prompt_tokens_details.cache_creation_token_details").Exists()
	}
	return usage, m
}

// The /stats counts of each label, process-wide like UsageGaps. "none" is the "" label.
var bobBackendA, bobBackendB, bobBackendOther, bobBackendNone atomic.Int64

// bobBackendIDNoUsage counts the replies that have an id but no usage block. They get the "" label,
// as router calls do, and log no WARN. The count is kept apart only to measure how often it happens
// before deciding how to label them: a backend that stopped sending usage would show here.
var bobBackendIDNoUsage atomic.Int64

// BobBackendCounts is /stats' bob_backend: how many replies on Bob's model route got each label.
func BobBackendCounts() map[string]int64 {
	return map[string]int64{"A": bobBackendA.Load(), "B": bobBackendB.Load(),
		"other": bobBackendOther.Load(), "none": bobBackendNone.Load(),
		"id_no_usage": bobBackendIDNoUsage.Load()}
}

// One WARN per new "other" shape, at most bobShapeMax shapes per process, so a backend that
// changed its marks gives a few lines and not one per request.
const bobShapeMax = 8

var (
	bobShapeMu   sync.Mutex
	bobShapeSeen = map[string]struct{}{}
)

// resetBobBackendForTest forgets the shapes logged so far. For TESTS only.
func resetBobBackendForTest() {
	bobShapeMu.Lock()
	defer bobShapeMu.Unlock()
	bobShapeSeen = map[string]struct{}{}
}

// bobNoteBackend counts the reply's label and returns it. On "other" it logs each kind of shape
// once: the first 4 characters of the id, its length and the marks it saw. Never the whole id.
func bobNoteBackend(m bobMarks) string {
	label := bobBackend(m)
	switch label {
	case "A":
		bobBackendA.Add(1)
	case "B":
		bobBackendB.Add(1)
	case "":
		bobBackendNone.Add(1)
		if m.id != "" && !m.usage {
			bobBackendIDNoUsage.Add(1)
		}
	default:
		bobBackendOther.Add(1)
		prefix := bobWarnPrefix(m.id)
		// The kind of shape: only the facts the rule uses. Not the id's letters, length or characters:
		// backend B ids vary in all 3, so a key on them would make every reply of one changed backend
		// a "new" shape and use up every slot. The WARN line still shows length and characters.
		key := strings.Join([]string{
			strconv.FormatBool(strings.HasPrefix(m.id, bobIDPrefixA)), strconv.FormatBool(m.cacheWrite),
			strconv.FormatBool(m.creationDetails), strconv.FormatBool(m.mixedIDs),
		}, "|")
		bobShapeMu.Lock()
		_, dup := bobShapeSeen[key]
		fresh := !dup && len(bobShapeSeen) < bobShapeMax
		if fresh {
			bobShapeSeen[key] = struct{}{}
		}
		bobShapeMu.Unlock()
		if fresh {
			slog.Default().Warn("cg.bob_backend_unknown", "prefix", prefix, "len", len(m.id),
				"alnum", bobAlnum(m.id), "cache_write", m.cacheWrite,
				"creation_details", m.creationDetails, "mixed_ids", m.mixedIDs,
				"provider_fields", m.providerFields)
		}
	}
	return label
}

// bobResponseUsage applies bobUsage to a whole response: the top-level `usage` of a JSON body,
// or the last `usage` chunk of an OpenAI chat event stream. ok is whether the shared parser read
// the usage; with ok false, base is returned unchanged. It also sets base.Backend (bobBackend),
// with or without usage.
func bobResponseUsage(contentType string, body []byte, base Usage, ok bool) Usage {
	usage, m := bobMarksOf(contentType, body)
	base.Backend = bobNoteBackend(m)
	if !ok {
		return base
	}
	return bobUsage(usage, base)
}

// bobWarnPrefix is the part of an id a WARN may log: at most 4 bytes, and always fewer than the whole
// id, so a short id is never logged in full. A non-ASCII byte is shown escaped, never raw.
func bobWarnPrefix(id string) string {
	n := min(4, len(id)-1)
	if n <= 0 {
		return ""
	}
	q := strconv.QuoteToASCII(id[:n])
	return q[1 : len(q)-1]
}
