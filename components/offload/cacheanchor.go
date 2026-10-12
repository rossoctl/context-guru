package offload

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

func init() { components.Register("cacheanchor", newCacheAnchor) }

// CacheAnchor keeps an edit inside the cached history from costing a rewrite of everything after the
// system prompt. When a request differs from the thread's previous forwarded request at message p, the
// provider can read only up to the last cache entry that still describes this request's prefix. With the
// client's single breakpoint on its last message there is none before p inside the 20-block look-back, so
// the whole conversation is written again. CacheAnchor puts one cache_control mark on the assistant turn
// that follows the last earlier request end e whose forwarded prefix is unchanged (e <= p-2): that
// breakpoint looks back one block, finds the entry the request ending at e wrote, and reads the prefix up
// to it; only what follows is rewritten.
//
// It must run AFTER every component that edits history, because it compares what is about to be sent.
// It adds at most one breakpoint, never beyond the provider's cap of four counting the client's own
// (Ctx.ExistingBreakpoints), skips a cold request (the whole prefix is rewritten anyway), and adds no
// mark when p is the previous end or later (an append: the client's own mark already reads the prefix).
type CacheAnchor struct{}

type anchorEnd struct {
	H string `json:"h"` // prefix hash written at this request end
	T int64  `json:"t"` // unix ms when the entry was last written or read
}

type anchorState struct {
	Prefix []string          `json:"p"` // prefix hash after each message of the previous forwarded request
	Ends   map[int]anchorEnd `json:"e"` // request end index -> entry
}

// anchorNow is the clock; a test replaces it.
var anchorNow = time.Now

// anchorEntryTTL is how long an entry is trusted to be alive: the provider keeps one five minutes after
// it was last written or read, and a margin is kept for clock and network skew.
const anchorEntryTTL = 270 * time.Second

const anchorMaxEnds = 64

func newCacheAnchor(raw []byte) (components.Component, error) {
	if err := components.Decode(raw, &struct{}{}); err != nil {
		return nil, err
	}
	return CacheAnchor{}, nil
}

func (CacheAnchor) Name() string                 { return "cacheanchor" }
func (CacheAnchor) Enabled(*components.Ctx) bool { return true }

func anchorKey(c *components.Ctx) string {
	return store.SweepKeepPrefix + "anchor:" + thread.Key(c.Session, c.Thread)
}

func prefixHashes(msgs []bschemas.ChatMessage) []string {
	h := sha1.New()
	out := make([]string, len(msgs))
	for i := range msgs {
		h.Write([]byte(msgs[i].Role))
		h.Write([]byte{0})
		h.Write([]byte(schema.MessageText(msgs[i])))
		h.Write([]byte{0})
		out[i] = hex.EncodeToString(h.Sum(nil))[:12]
	}
	return out
}

func (CacheAnchor) Offload(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) ([]string, error) {
	rep.Skipped = true
	if c == nil || c.Store == nil || len(req.Input) == 0 {
		return nil, nil
	}
	ph := prefixHashes(req.Input)
	var st anchorState
	if b, ok := c.Store.Get(anchorKey(c)); ok {
		_ = json.Unmarshal(b, &st)
	}
	if st.Ends == nil {
		st.Ends = map[int]anchorEnd{}
	}
	// first message whose prefix differs from the previous forwarded request
	p := -1
	for k := 0; k < len(st.Prefix) && k < len(ph); k++ {
		if st.Prefix[k] != ph[k] {
			p = k
			break
		}
	}
	if p < 0 && len(ph) < len(st.Prefix) {
		p = len(ph) // the transcript got shorter: everything after is new
	}
	now := anchorNow().UnixMilli()
	if p >= 2 && !c.ColdCache {
		// the latest earlier request end, strictly before p-1, whose entry still describes our prefix
		best := -1
		for e := p - 2; e >= 0; e-- {
			if en, ok := st.Ends[e]; ok && e < len(ph) && en.H == ph[e] &&
				now-en.T < anchorEntryTTL.Milliseconds() {
				best = e
				break
			}
		}
		if best < 0 {
			rep.Gate("anchor_no_valid_entry")
		} else if idx := best + 1; !anchorMarkable(&req.Input[idx]) {
			rep.Gate("anchor_not_markable")
		} else if anchorBreakpoints(req, c) >= 4 {
			rep.Gate("anchor_no_free_slot")
		} else {
			last := &req.Input[idx].Content.ContentBlocks[len(req.Input[idx].Content.ContentBlocks)-1]
			last.CacheControl = &bschemas.CacheControl{Type: bschemas.CacheControlTypeEphemeral}
			st.Ends[best] = anchorEnd{H: st.Ends[best].H, T: now} // our read refreshes it
			rep.Event("anchor_placed")
			rep.Skipped = false
		}
	}
	// remember what we send: entries whose prefix no longer matches are stale, and this request's end is new
	for e, en := range st.Ends {
		if e >= len(ph) || en.H != ph[e] || now-en.T >= anchorEntryTTL.Milliseconds() {
			delete(st.Ends, e)
		}
	}
	st.Ends[len(ph)-1] = anchorEnd{H: ph[len(ph)-1], T: now}
	if len(st.Ends) > anchorMaxEnds {
		lo := len(ph)
		for e := range st.Ends {
			if e < lo {
				lo = e
			}
		}
		delete(st.Ends, lo)
	}
	st.Prefix = ph
	if b, err := json.Marshal(st); err == nil {
		c.Store.Put(anchorKey(c), b)
	}
	return nil, nil
}

// anchorMarkable: an assistant turn with a block array whose last block carries no mark yet. (A tool
// result message normalises to a string and cannot carry a block-level mark through the pipeline.)
func anchorMarkable(m *bschemas.ChatMessage) bool {
	if m.Role != bschemas.ChatMessageRoleAssistant || m.Content == nil || len(m.Content.ContentBlocks) == 0 {
		return false
	}
	return m.Content.ContentBlocks[len(m.Content.ContentBlocks)-1].CacheControl == nil
}

// anchorBreakpoints counts the breakpoints on the request: the host's raw count (system, tools and marks
// no component sees) or the visible ones, whichever is larger.
func anchorBreakpoints(req *bschemas.BifrostChatRequest, c *components.Ctx) int {
	visible := 0
	for i := range req.Input {
		if req.Input[i].Content == nil {
			continue
		}
		for _, b := range req.Input[i].Content.ContentBlocks {
			if b.CacheControl != nil {
				visible++
			}
		}
	}
	if c.ExistingBreakpoints > visible {
		return c.ExistingBreakpoints
	}
	return visible
}

func init() {
	components.RegisterFields("cacheanchor", struct{}{}, nil)
}
