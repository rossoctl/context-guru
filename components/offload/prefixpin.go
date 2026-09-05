package offload

import (
	"encoding/json"
	"strconv"
	"strings"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"gopkg.in/yaml.v3"
)

func init() { components.Register("prefixpin", newPrefixpin) }

// Prefixpin restores prefix stability when an agent REWRITES an early message on
// every turn. It is the counterpart to cacheinject: cacheinject optimises where
// the cache boundary goes, prefixpin fixes the case where no boundary can help.
//
// Why this exists, from measurement rather than theory. Across 1,955 real Bob
// requests on SWE-bench, 98.0% of turns were append-only and cached fine. The
// other 2% cost 5,796,220 tokens — 71.8% of ALL uncached input — because a single
// early message mutated each turn. On one task the agent re-emitted a running
// <scratchpad>/<state_snapshot> at message index 1, re-rendering an iteration
// counter in ~20 places ("THIRTY-SECOND" -> "THIRTY-THIRD", "thirty-second" ->
// "thirty-third", "32" -> "33"): 152 changed characters out of 6,024, i.e. 98.5%
// identical content, sitting ~1,374 tokens into a 181k-token prefix. Only 0.76% of
// the prefix survived and the cache hit rate collapsed from 98% to 5.7%.
//
// The economics are lopsided. A prompt-cache read costs 0.1x base input and
// uncached input costs 1.0x, so a mutation below the cache boundary makes every
// token above it TEN TIMES more expensive. Compare placement tuning, which only
// ever moves tokens between read (0.1x) and cache-write (1.25x). That is why this
// is worth ~31% of Bob's input cost while every placement change measured ~0%.
//
// How: for each early message, remember the first text seen for its (index, role)
// slot in this session. On a later turn, if that slot's text has CHANGED but is
// still recognisably the same content (same shape, high similarity), re-send the
// FIRST rendering so the prefix stays byte-identical.
//
// LOSSINESS — this is an Offload, deliberately, not a Reformat. The model sees the
// pinned (older) text rather than what the agent just wrote, so information IS
// withheld: a counter reads stale. That is a real behavioural change and the
// reason for the guards below. The original is stashed so `expand` can recover it.
//
// Guards, each closing a way this could do harm:
//   - only messages at index < MaxPinIndex (an early, structural slot; never the
//     working tail the agent is actively reasoning about)
//   - only when similarity >= MinSimilarity (a genuinely rewritten-in-place block,
//     not a different message that happens to occupy the slot)
//   - only after the slot has churned RepeatThreshold times, so a one-off edit is
//     never pinned — only a per-turn churn pattern
//   - never on the newest message, never on tool results
type Prefixpin struct {
	// MaxPinIndex bounds pinning to structurally-early messages. 0 disables.
	MaxPinIndex int `yaml:"max_pin_index"`
	// MinSimilarity is the character-level overlap required to treat a changed slot
	// as the same content rewritten in place.
	MinSimilarity float64 `yaml:"min_similarity"`
	// RepeatThreshold is how many times a slot must churn before pinning starts.
	RepeatThreshold int `yaml:"repeat_threshold"`
	// MinTokens skips slots too small to be worth the behavioural risk.
	MinTokens int `yaml:"min_tokens"`
}

func newPrefixpin(raw []byte) (components.Component, error) {
	p := &Prefixpin{MaxPinIndex: 4, MinSimilarity: 0.80, RepeatThreshold: 2, MinTokens: 200}
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, p); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (Prefixpin) Name() string { return "prefixpin" }

// Enabled everywhere: the failure mode is provider-independent. It bites hardest on
// implicit-cache backends (Gemini/Bob, OpenAI) where there is no cache_control to
// place and prefix stability is the ONLY available lever.
func (p *Prefixpin) Enabled(c *components.Ctx) bool { return p.MaxPinIndex > 0 }

func (p *Prefixpin) Offload(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) ([]string, error) {
	if c == nil || c.Store == nil || c.Session == "" || len(req.Input) < 2 {
		rep.Skipped = true
		return nil, nil
	}

	limit := p.MaxPinIndex
	if limit > len(req.Input)-1 {
		limit = len(req.Input) - 1 // never the newest message
	}

	var keys []string
	acted := false
	for i := 0; i < limit; i++ {
		m := &req.Input[i]
		if !schema.Rewritable(*m) || m.Role == bschemas.ChatMessageRoleTool {
			continue
		}
		cur := schema.MessageText(*m)
		if cur == "" || schema.TextTokens(cur) < p.MinTokens {
			continue
		}
		if skipReduce(c, cur) {
			continue // already offloaded, or the agent expanded it
		}

		st := p.loadSlot(c, i, string(m.Role))
		if st == nil {
			p.saveSlot(c, i, string(m.Role), &slotState{First: cur, Churn: 0})
			continue
		}
		if st.First == cur {
			continue // stable already: nothing to do, and nothing to pay for
		}
		if similarity(st.First, cur) < p.MinSimilarity {
			// A different message now occupies this slot (the transcript was
			// restructured, not rewritten). Re-baseline rather than pin the wrong text.
			p.saveSlot(c, i, string(m.Role), &slotState{First: cur, Churn: 0})
			continue
		}

		st.Churn++
		p.saveSlot(c, i, string(m.Role), st)
		if st.Churn < p.RepeatThreshold {
			continue // one-off edit; not yet evidence of per-turn churn
		}

		// Pin: re-send the first rendering so the prefix hashes identically. The
		// current text is stashed under the marker key so expand can recover it.
		k := pinKey(c.Session, i)
		c.Store.Put(k, []byte(cur))
		schema.SetMessageText(m, st.First)
		keys = append(keys, k)
		acted = true
	}

	if !acted {
		rep.Skipped = true
	}
	return keys, nil
}

// --------------------------------------------------------------------------- //

type slotState struct {
	First string `json:"first"`
	Churn int    `json:"churn"`
}

func pinKey(session string, i int) string {
	return "cg:pin:" + session + ":" + strconv.Itoa(i)
}

func slotKey(session string, i int, role string) string {
	return "cg:pinslot:" + session + ":" + strconv.Itoa(i) + ":" + role
}

func (p *Prefixpin) loadSlot(c *components.Ctx, i int, role string) *slotState {
	b, ok := c.Store.Get(slotKey(c.Session, i, role))
	if !ok || len(b) == 0 {
		return nil
	}
	var s slotState
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	return &s
}

func (p *Prefixpin) saveSlot(c *components.Ctx, i int, role string, s *slotState) {
	if b, err := json.Marshal(s); err == nil {
		c.Store.Put(slotKey(c.Session, i, role), b)
	}
}

// similarity estimates content overlap in [0,1] via line-shingle containment.
//
// A prefix+suffix overlap ratio is NOT adequate here, and the traces show exactly
// why: the real churning block differed by 152 characters out of 6,024 (98.5%
// identical by edit distance) but the edits were scattered over 20 separate hunks
// — a repeated counter rendered in several places ("THIRTY-SECOND"/"thirty-second"
// /"32"). Any one early hunk truncates the common prefix and any one late hunk
// truncates the common suffix, so that measure scored it 0.075 and the guard
// rejected the very case it exists to catch.
//
// Line shingles are immune to scattered edits (only the lines containing an edit
// are lost), are O(n) with a bounded map, and need no quadratic edit distance on a
// message that may be 100k tokens.
func similarity(a, b string) float64 {
	if a == b {
		return 1
	}
	if a == "" || b == "" {
		return 0
	}
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	// Guard the map size on pathological input (one enormous line, or a million
	// lines): both extremes fall back to the cheap character ratio, which is only
	// used to reject wildly different content.
	if len(al) < 4 || len(bl) < 4 {
		la, lb := float64(len(a)), float64(len(b))
		if lb > la {
			la, lb = lb, la
		}
		return lb / la
	}
	set := make(map[string]int, len(al))
	for _, l := range al {
		l = strings.TrimSpace(l)
		if l != "" {
			set[l]++
		}
	}
	hit, total := 0, 0
	for _, l := range bl {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		total++
		if set[l] > 0 {
			set[l]--
			hit++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(hit) / float64(total)
}
