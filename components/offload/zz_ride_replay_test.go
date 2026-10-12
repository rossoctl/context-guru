package offload_test

// Offline replay of extract_llm_ride's trigger logic over captured Claude Code trajectories.
// Not part of the suite: gated by CG_RIDE_REPLAY=<capture.jsonl[,more]> and CG_RIDE_CFG=<yaml>.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/internal/extract"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var frac = func() float64 {
	if v, err := strconv.ParseFloat(os.Getenv("CG_RIDE_FRAC"), 64); err == nil && v > 0 {
		return v
	}
	return 1
}()

var globalLive int

var labelRe = regexp.MustCompile(`\[(\d+)\] (\d+) tokens`)

type replayAsker struct {
	live                      *cheapmodel.Anthropic
	prevBody                  []byte
	liveAsks, maxLive         int
	liveItems, liveTok        int
	liveP, liveE, liveK       int
	park                      bool
	asks, targeted, prefixTok int
	itemsPerAsk               []int
}

func (a *replayAsker) Ask(ctx context.Context, s, ask string) (string, components.PrefixUsage, error) {
	return a.AskAs(ctx, s, ask, "user")
}
func (a *replayAsker) AskAs(ctx context.Context, _, ask, _ string) (string, components.PrefixUsage, error) {
	a.asks++
	if a.live != nil {
		if globalLive >= a.maxLive || len(a.prevBody) == 0 {
			return "[]", components.PrefixUsage{CacheRead: a.prefixTok, Fresh: 600, Output: 20}, nil
		}
		a.liveAsks++
		globalLive++
		ls := labelRe.FindAllStringSubmatch(ask, -1)
		a.itemsPerAsk = append(a.itemsPerAsk, len(ls))
		for _, l := range ls {
			t, _ := strconv.Atoi(l[2])
			a.targeted += t
		}
		body := a.prevBody
		if gjson.GetBytes(body, "thinking.display").Exists() {
			body, _ = sjson.SetBytes(body, "thinking.display", "summarized")
		}
		body, _ = sjson.SetBytes(body, "model", a.live.Model)
		warm := *a.live
		warm.MaxTokens = 16
		if _, _, werr := warm.CompletePrefixedAs(ctx, body, "ok", "user"); werr != nil {
			fmt.Printf("LIVEERR warm %v\n", werr)
		}
		reply, u, err := a.live.CompletePrefixedAs(ctx, body, ask, "user")
		if err != nil {
			fmt.Printf("LIVEERR %v\n", err)
			return "[]", components.PrefixUsage{CacheRead: a.prefixTok, Fresh: 600, Output: 20}, nil
		}
		sizes := map[int]int{}
		for _, l := range ls {
			lb, _ := strconv.Atoi(l[1])
			sz, _ := strconv.Atoi(l[2])
			sizes[lb] = sz
		}
		offered, removed := 0, 0
		for _, sz := range sizes {
			offered += sz
		}
		if ds, ok := extract.ParseRideDecisions(reply); ok {
			for _, d := range ds {
				switch d.Action {
				case "park":
					removed += sizes[d.Label]
				case "extract":
					if r := sizes[d.Label] - schema.TextTokens(d.Extract); r > 0 {
						removed += r
					}
				}
				switch d.Action {
				case "park":
					a.liveP++
				case "extract":
					a.liveE++
				default:
					a.liveK++
				}
			}
		}
		fmt.Printf("LIVEMASS offered=%d removed_by_decision=%d\n", offered, removed)
		fmt.Printf("LIVEASK items=%d offered=%d usage read=%d write=%d fresh=%d out=%d\n", len(ls), a.targeted, u.CacheRead, u.CacheWrite, u.Fresh, u.Output)
		return reply, components.PrefixUsage(u), nil
	}
	ls := labelRe.FindAllStringSubmatch(ask, -1)
	a.itemsPerAsk = append(a.itemsPerAsk, len(ls))
	var parts []string
	for k, l := range ls {
		t, _ := strconv.Atoi(l[2])
		a.targeted += t
		act := "keep"
		if a.park && (frac >= 1 || float64((k*7919+a.asks*131)%100) < frac*100) {
			act = "park"
		}
		parts = append(parts, `{"label":`+l[1]+`,"action":"`+act+`"}`)
	}
	return "[" + strings.Join(parts, ",") + "]", components.PrefixUsage{CacheRead: a.prefixTok, Fresh: 600, Output: 250}, nil
}

type rec struct {
	ts   int64
	body []byte
}

func TestRideReplay(t *testing.T) {
	files, cfg := os.Getenv("CG_RIDE_REPLAY"), os.Getenv("CG_RIDE_CFG")
	if files == "" {
		t.Skip("set CG_RIDE_REPLAY")
	}
	park := os.Getenv("CG_RIDE_PARK") == "1"
	liveModel := os.Getenv("CG_RIDE_LIVE")
	maxLive, _ := strconv.Atoi(os.Getenv("CG_RIDE_MAXASKS"))
	sample, _ := strconv.Atoi(os.Getenv("CG_RIDE_SAMPLE"))
	if sample < 1 {
		sample = 1
	}
	sessIdx := -1
	comp, err := components.New("extract_llm_ride", []byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	off := comp.(components.Offload)
	groups := map[string][]rec{}
	for _, f := range strings.Split(files, ",") {
		fh, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		sc := newScanner(fh)
		for sc.Scan() {
			line := sc.Bytes()
			body := gjson.GetBytes(line, "body")
			if !body.Exists() || !gjson.Get(body.Raw, "tools").Exists() {
				continue
			}
			uid := gjson.Get(body.Raw, "metadata.user_id").String()
			sys := gjson.Get(body.Raw, "system.0.text").String()
			if len(sys) > 80 {
				sys = sys[:80]
			}
			k := f + "|" + uid + "|" + fmt.Sprint(len(gjson.Get(body.Raw, "tools").Array())) + "|" + sys
			groups[k] = append(groups[k], rec{gjson.GetBytes(line, "ts_ms").Int(), []byte(body.Raw)})
		}
		fh.Close()
	}
	var sessions, reqs, asksTot, tgtTot, removedTot, longSess, firing int
	var net, gross, askCost, rewrite, rewriteAnch, base float64
	gates := map[string]int{}
	events := map[string]int{}
	coldReqs, appliedReqs, anchoredApplies := 0, 0, 0
	var asksPer []int
	gkeys := make([]string, 0, len(groups))
	for k := range groups {
		gkeys = append(gkeys, k)
	}
	sort.Strings(gkeys)
	for _, gk := range gkeys {
		rs := groups[gk]
		if len(rs) < 8 { // subagent stubs and one-shot calls
			continue
		}
		sort.Slice(rs, func(i, j int) bool { return rs[i].ts < rs[j].ts })
		sessIdx++
		if liveModel != "" && sessIdx%sample != 0 {
			continue
		}
		sessions++
		g0, a0, r0, b0 := gross, askCost, rewrite, base
		st := store.NewMemory(store.Options{MaxEntries: 4000})
		asker := &replayAsker{park: park, maxLive: maxLive}
		if liveModel != "" {
			asker.live = &cheapmodel.Anthropic{BaseURL: os.Getenv("GW_BASE"), APIKey: os.Getenv("GW_KEY"), Model: liveModel, MaxTokens: cheapmodel.PrefixAskMaxTokens, AuthScheme: "bearer", Client: &http.Client{Timeout: 280 * time.Second}}
		}
		prevLen := 0
		var prevTs int64
		asksBefore := 0
		type fireRec struct{ idx, saved, tailW int }
		var prevAfter []string
		removedAt := make([]int, len(rs))
		coldGap := make([]bool, len(rs))
		anchoredW := make([]int, len(rs)) // tokens from the anchor on, when a breakpoint was placed
		var fires []fireRec
		toks := make([]int, len(rs))
		for i, r := range rs {
			norm := apply.NormalizeMessages(bschemas.Anthropic, r.body)
			req := &bschemas.BifrostChatRequest{Provider: bschemas.Anthropic, Input: norm}
			toks[i] = schema.MessagesTokens(req)
			idle := int64(0)
			if prevTs > 0 {
				idle = r.ts - prevTs
			}
			asker.prefixTok = toks[maxi(i-1, 0)]
			if i > 0 {
				asker.prevBody = rs[i-1].body
			}
			c := &components.Ctx{Session: "s", Ctx: context.Background(), Store: st, CtxWindow: 1_000_000,
				ModelName: "claude-opus-5", CacheAware: true, MaxCachedIdx: prevLen - 1,
				IdleMs: idle, CacheTTLMs: 300000, PrefixAsk: asker, ColdCache: idle > 300000}
			rep := &components.Report{Component: "extract_llm_ride"}
			before := asker.asks
			orig := make([]string, len(req.Input))
			for k := range req.Input {
				orig[k] = schema.MessageText(req.Input[k])
			}
			off.Offload(req, rep, c)
			for g, n := range rep.Gates {
				gates[g] += n
			}
			for g, n := range rep.Events {
				events[g] += n
			}
			coldGap[i] = idle > 300000
			if idle > 300000 {
				coldReqs++
			}
			after := make([]string, len(req.Input))
			for k := range req.Input {
				after[k] = schema.MessageText(req.Input[k])
			}
			removedAt[i] = toks[i] - schema.MessagesTokens(req)
			if rep.Events["ride_anchor_placed"] > 0 {
				fd := -1
				for k := 0; k < len(after) && k < len(prevAfter); k++ {
					if after[k] != prevAfter[k] {
						fd = k
						break
					}
				}
				if fd >= 1 {
					for k := fd - 1; k < len(after); k++ {
						anchoredW[i] += schema.TextTokens(after[k])
					}
				}
			}
			if asker.asks > before {
				first := -1
				for k := 0; k < len(after) && k < len(prevAfter); k++ {
					if after[k] != prevAfter[k] {
						first = k
						break
					}
				}
				tail := 0
				if first >= 0 {
					for k := first; k < len(after); k++ {
						tail += schema.TextTokens(after[k])
					}
				}
				newly := removedAt[i]
				if i > 0 {
					newly -= removedAt[i-1]
				}
				fires = append(fires, fireRec{i, newly, tail})
			}
			prevAfter = after
			prevLen, prevTs = len(norm), r.ts
		}
		_ = asksBefore
		if asker.live != nil && asker.liveAsks > 0 {
			fmt.Printf("LIVESESS asks=%d decisions park=%d extract=%d keep=%d\n", asker.liveAsks, asker.liveP, asker.liveE, asker.liveK)
		}
		asksTot += asker.asks
		tgtTot += asker.targeted
		reqs += len(rs)
		asksPer = append(asksPer, asker.asks)
		if asker.asks > 0 {
			firing++
		}
		if len(rs) >= 30 {
			longSess++
		}
		// economics in fresh-input equivalents: read 0.1, write 1.25, fresh 1, output 5; oracle horizon
		// = the session's actual remaining requests. A fire at request i removes `saved` tokens from the
		// request that carries it and, being frozen and replayed, from every later one.
		for i := range rs {
			prev := 0
			if i > 0 {
				prev = toks[i-1]
			}
			base += 0.1*float64(prev) + 1.25*float64(maxi(toks[i]-prev, 0))
		}
		for _, v := range removedAt {
			gross += 0.1 * float64(v)
			removedTot += v
		}
		for k, f := range fires {
			n := 0
			if k < len(asker.itemsPerAsk) {
				n = asker.itemsPerAsk[k]
			}
			fmt.Printf("FIRE %s %d %d %d\n", taskKey(rs[0].body), toks[maxi(f.idx-1, 0)], n, f.saved)
			askCost += 0.1*float64(toks[maxi(f.idx-1, 0)]+13000) + 600 + 250*5
		}
		for i := range rs {
			newly := removedAt[i]
			if i > 0 {
				newly -= removedAt[i-1]
			}
			if newly <= 0 {
				continue
			}
			appliedReqs++
			if coldGap[i] {
				rewrite -= 1.15 * float64(newly) // the request rewrote the whole conversation anyway: a smaller write
			} else {
				if anchoredW[i] > 0 {
					rewrite += 1.15 * float64(anchoredW[i])
					anchoredApplies++
				} else {
					rewrite += 1.15 * float64(toks[i]-removedAt[i])
				}
			}
		}
		fmt.Printf("SESSNET %s %.1f %.1f\n", taskKey(rs[0].body), (gross-g0)-(askCost-a0)-(rewrite-r0), base-b0)
	}
	net = gross - askCost - rewrite
	sort.Ints(asksPer)
	med := 0
	if len(asksPer) > 0 {
		med = asksPer[len(asksPer)/2]
	}
	fmt.Printf("REPLAY park=%v sessions=%d (>=30 req: %d) requests=%d firing_sessions=%d asks=%d (per session median %d, mean %.2f) targeted_tokens=%d removed_tokens=%d\n",
		park, sessions, longSess, reqs, firing, asksTot, med, float64(asksTot)/float64(maxi(sessions, 1)), tgtTot, removedTot)
	fmt.Printf("BASELINE input cost (fresh-equivalents, conversation tokens only) = %.0f; net as share: unanchored %.2f%% anchored %.2f%%\n", base, 100*(gross-askCost-rewrite)/base, 100*(gross-askCost-rewriteAnch)/base)
	fmt.Printf("ANCHORED (a cache breakpoint before the first edited message): rewrite_cost=%.0f net=%.0f\n", rewriteAnch, gross-askCost-rewriteAnch)
	fmt.Printf("ECON (fresh-input equivalents, oracle horizon): gross_read_saving=%.0f ask_cost=%.0f rewrite_cost=%.0f net=%.0f  net_per_session=%.0f\n",
		gross, askCost, rewrite, net, net/float64(maxi(sessions, 1)))
	fmt.Printf("COLD requests (gap>300s): %d; requests that applied new removals: %d (anchored: %d); events %v\n", coldReqs, appliedReqs, anchoredApplies, events)
	keys := make([]string, 0, len(gates))
	for g := range gates {
		keys = append(keys, g)
	}
	sort.Strings(keys)
	for _, g := range keys {
		fmt.Printf("  gate %s %d\n", g, gates[g])
	}
	_ = json.Marshal
}

func maxi(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func taskKey(body []byte) string {
	m := gjson.GetBytes(body, "messages.0.content")
	t := m.String()
	if m.IsArray() {
		t = ""
		for _, b := range m.Array() {
			t += b.Get("text").String()
		}
	}
	if len(t) > 400 {
		t = t[len(t)-400:]
	}
	h := 0
	for _, c := range t {
		h = (h*131 + int(c)) & 0x7fffffff
	}
	return fmt.Sprintf("t%d", h)
}
