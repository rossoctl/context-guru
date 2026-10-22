// cg-transcript-replay runs the shipped pipeline over request sequences rebuilt from Claude Code
// transcripts (scripts/analysis/cc_transcript_requests.py) and writes, per call, what the pipeline
// changed. It makes no network call: it is the offline half of "what would Context Guru save here".
//
// Every call k of a round is sent as messages[:n_k], in order, through apply.BodyOpts with a
// Tracker, a per-round store and the call's own timestamp — the same inputs the proxy gives it — so
// the cached-prefix boundary, the cold-cache verdict and sticky offloads all behave as they would live.
//
// Per call it records the token count of the original and the forwarded messages, how much of the
// forwarded body is an unchanged prefix of the previous forwarded body (anything less is a cache
// rewrite), the per-component saving, and the per-tool_use_id saving. Pricing is left to the caller.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/internal/tokens"
	"github.com/rossoctl/context-guru/modes"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type round struct {
	Messages []json.RawMessage `json:"messages"`
	Calls    []struct {
		N    int   `json:"n"`
		TsMs int64 `json:"ts_ms"`
	} `json:"calls"`
}

type callOut struct {
	K         int            `json:"k"`
	N         int            `json:"n"`
	OrigTok   int            `json:"orig_tok"`
	OutTok    int            `json:"out_tok"`
	CommonTok int            `json:"common_tok"` // tokens of the forwarded prefix identical to the previous forwarded body
	PrevOut   int            `json:"prev_out_tok"`
	Broken    bool           `json:"broken"` // the forwarded body rewrote a message the previous one sent
	BrokenAt  int            `json:"broken_at"`
	Changed   bool           `json:"changed"`
	Cold      bool           `json:"cold"`
	MaxCached int            `json:"max_cached_idx"`
	ByComp    map[string]int `json:"by_comp,omitempty"`
	ByTool    map[string]int `json:"by_tool,omitempty"`
	Err       string         `json:"err,omitempty"`
}

type rec struct{ comps map[string]int }

func (r *rec) Component(rp components.Report) {
	if rp.Discarded > 0 || rp.Reverted || rp.Skipped {
		return
	}
	if d := rp.TokensBefore - rp.TokensAfter; d != 0 {
		r.comps[rp.Component] += d
	}
}
func (r *rec) Run(components.RunReport) {}

// norm strips cache_control so the breakpoint moving to the newest message is not read as a rewrite.
func norm(raw string) string {
	out := raw
	gjson.Get(raw, "content").ForEach(func(k, b gjson.Result) bool {
		if b.Get("cache_control").Exists() {
			out, _ = sjson.Delete(out, "content."+k.String()+".cache_control")
		}
		return true
	})
	return out
}

func hash(s string) uint64 { h := fnv.New64a(); h.Write([]byte(s)); return h.Sum64() }

// toolResults maps tool_use_id -> token count of its tool_result block.
func toolResults(msgs []gjson.Result) map[string]int {
	m := map[string]int{}
	for _, msg := range msgs {
		msg.Get("content").ForEach(func(_, b gjson.Result) bool {
			if b.Get("type").String() == "tool_result" {
				m[b.Get("tool_use_id").String()] += tokens.Count(b.Get("content").Raw)
			}
			return true
		})
	}
	return m
}

func main() {
	preset := flag.String("preset", "conservative", "shipped preset to run (ignored when -pipeline is set)")
	pipeline := flag.String("pipeline", "", "comma-separated component list to run instead of a preset")
	cfgPath := flag.String("config", "", "full YAML config document to run instead of -preset/-pipeline")
	model := flag.String("model", "claude-opus-5", "model name on every request")
	out := flag.String("out", "", "output directory")
	flag.Parse()
	doc := "preset: " + *preset + "\n"
	if *pipeline != "" {
		doc = "pipeline: [" + *pipeline + "]\n"
	}
	b := []byte(doc)
	if *cfgPath != "" {
		var err error
		if b, err = os.ReadFile(*cfgPath); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	cfg, err := config.LoadBytes(b)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, path := range flag.Args() {
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		if err := replay(cfg, *model, path, filepath.Join(*out, name+".jsonl")); err != nil {
			fmt.Fprintln(os.Stderr, name, err)
			os.Exit(1)
		}
	}
}

func replay(cfg *config.Config, model, in, outPath string) error {
	b, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	var r round
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	em := &rec{}
	pipe, err := cfg.Build(em)
	if err != nil {
		return err
	}
	st := store.NewMemory(cfg.Store)
	tr := modes.NewTracker(0)
	f, err := os.OpenFile(outPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	session := "replay-" + filepath.Base(in)
	var prevHash []uint64
	prevOut := 0
	for k, c := range r.Calls {
		body := `{"model":"` + model + `","max_tokens":32000,"messages":[]}`
		var origTok int
		for i, m := range r.Messages[:c.N] {
			raw := string(m)
			if i == c.N-1 { // Claude Code's trailing breakpoint, on the last block of the newest message
				if n := gjson.Get(raw, "content.#").Int(); n > 0 {
					raw, _ = sjson.SetRaw(raw, fmt.Sprintf("content.%d.cache_control", n-1), `{"type":"ephemeral"}`)
				}
			}
			body, _ = sjson.SetRaw(body, "messages.-1", raw)
			origTok += tokens.Count(norm(string(m)))
		}
		em.comps = map[string]int{}
		res := apply.BodyOpts(context.Background(), pipe, st, apply.Opts{
			Provider: bschemas.Anthropic, Body: []byte(body), Session: session,
			Window: 1_000_000, WindowExact: true, Mode: components.ModeSync, Tracker: tr,
			Now: time.UnixMilli(c.TsMs),
		})
		o := callOut{K: k, N: c.N, OrigTok: origTok, Changed: res.Changed, MaxCached: res.MaxCachedIdx, ByComp: em.comps, PrevOut: prevOut, BrokenAt: -1}
		if res.Run == nil && res.Changed {
			o.Err = "changed without a run report"
		}
		outMsgs := gjson.GetBytes(res.Body, "messages").Array()
		origMsgs := gjson.Get(body, "messages").Array()
		hs, ts := make([]uint64, len(outMsgs)), make([]int, len(outMsgs))
		for i, m := range outMsgs {
			s := norm(m.Raw)
			hs[i], ts[i] = hash(s), tokens.Count(s)
			o.OutTok += ts[i]
		}
		common := 0
		for i := 0; i < len(prevHash) && i < len(hs); i++ {
			if hs[i] != prevHash[i] {
				o.Broken, o.BrokenAt = true, i
				break
			}
			common += ts[i]
		}
		o.CommonTok = common
		ot, nt := toolResults(origMsgs), toolResults(outMsgs)
		for id, t := range ot {
			if d := t - nt[id]; d != 0 {
				if o.ByTool == nil {
					o.ByTool = map[string]int{}
				}
				o.ByTool[id] = d
			}
		}
		o.Cold = res.Run != nil && res.Trace.CacheAware && res.MaxCachedIdx < 0 && k > 0
		prevHash, prevOut = hs, o.OutTok
		if err := enc.Encode(o); err != nil {
			return err
		}
	}
	return nil
}
