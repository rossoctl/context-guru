package apply

// Offline replay: does a cache breakpoint before the first changed message make deep (unrestricted)
// deterministic edits pay? Gated by CG_HOUSE_REPLAY=<capture.jsonl,...>; CG_HOUSE_PIPE=<component[,component]|house>.
// Costs are in fresh-input equivalents (read 0.1, write 1.25, fresh 1), conversation tokens only.

import (
	"bufio"
	"context"
	"crypto/sha1"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

type hrecEv struct {
	ts   int64
	body []byte
}

func msgsOfEv(body []byte) ([]string, []int) {
	norm := NormalizeMessages(bschemas.Anthropic, body)
	txt := make([]string, len(norm))
	tok := make([]int, len(norm))
	for i := range norm {
		txt[i] = schema.MessageText(norm[i])
		tok[i] = schema.TextTokens(txt[i])
	}
	return txt, tok
}

func sumEv(a []int, from int) int {
	s := 0
	for i := from; i < len(a); i++ {
		s += a[i]
	}
	return s
}

func buildPipeEv(t *testing.T, spec string) *components.Pipeline {
	var cfg *config.Config
	var err error
	if strings.HasPrefix(spec, "preset:") {
		cfg, err = config.LoadBytes([]byte("preset: " + strings.TrimPrefix(spec, "preset:") + "\n"))
	} else if spec == "house" {
		cfg, err = config.LoadBytes([]byte("preset: house\n"))
	} else {
		cfg, err = config.LoadBytes([]byte("pipeline: [" + spec + "]\n"))
	}
	if err != nil {
		t.Fatal(err)
	}
	p, err := cfg.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEvalAnchorReplay(t *testing.T) {
	files, spec := os.Getenv("CG_HOUSE_REPLAY"), os.Getenv("CG_HOUSE_PIPE")
	if files == "" {
		t.Skip("set CG_HOUSE_REPLAY")
	}
	groups := map[string][]hrecEv{}
	for _, f := range strings.Split(files, ",") {
		fh, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 0, 1<<20), 256<<20)
		for sc.Scan() {
			b := gjson.GetBytes(sc.Bytes(), "body")
			if !b.Exists() || !gjson.Get(b.Raw, "tools").Exists() {
				continue
			}
			sys := gjson.Get(b.Raw, "system.0.text").String()
			if len(sys) > 80 {
				sys = sys[:80]
			}
			k := f + "|" + gjson.Get(b.Raw, "metadata.user_id").String() + "|" + sys
			groups[k] = append(groups[k], hrecEv{gjson.GetBytes(sc.Bytes(), "ts_ms").Int(), []byte(b.Raw)})
		}
		fh.Close()
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	kBatch, _ := strconv.Atoi(os.Getenv("CG_HOUSE_K"))
	lb, _ := strconv.Atoi(os.Getenv("EV_LB"))
	ageMs, _ := strconv.Atoi(os.Getenv("EV_AGE_MS"))
	if lb == 0 {
		lb = 1 << 30
	}
	if ageMs == 0 {
		ageMs = 1 << 30
	}
	var cRaw, cTail, cNo, cAn float64
	var removedTail, removedUnres, nChange, nAnch, reqs, sessions int
	for si, k := range keys {
		rs := groups[k]
		if len(rs) < 8 {
			continue
		}
		sort.Slice(rs, func(i, j int) bool { return rs[i].ts < rs[j].ts })
		sessions++
		r0, n0, a0, t0 := cRaw, cNo, cAn, cTail
		pipeT, pipeU := buildPipeEv(t, spec), buildPipeEv(t, spec)
		stT, stU := store.NewMemory(store.Options{MaxEntries: 20000}), store.NewMemory(store.Options{MaxEntries: 20000})
		var prevRaw, prevT, prevU int
		var prevUtxt []string
		var curTxt []string
		var curTok []int
		ends := map[int]string{} // request end index -> hash of the forwarded prefix up to it, as written
		endTs := map[int]int64{}
		for i, r := range rs {
			sid := fmt.Sprintf("hs%d", si)
			resT := BodyOpts(context.Background(), pipeT, stT, Opts{Provider: bschemas.Anthropic, Body: r.body, Session: sid + "t", CacheMode: "auto"})
			resU := BodyOpts(context.Background(), pipeU, stU, Opts{Provider: bschemas.Anthropic, Body: r.body, Session: sid + "u", CacheMode: "off"})
			bt, bu := resT.Body, resU.Body
			if bt == nil {
				bt = r.body
			}
			if bu == nil {
				bu = r.body
			}
			_, tokR := msgsOfEv(r.body)
			_, tokT := msgsOfEv(bt)
			txtU, tokU := msgsOfEv(bu)
			if kBatch > 1 {
				// apply the unrestricted result only every kBatch-th request; in between keep the last applied
				// prefix and append the new raw messages (the tail pipeline is a no-op for this component)
				rawTxt, rawTok := msgsOfEv(r.body)
				if i%kBatch == 0 || curTxt == nil {
					curTxt, curTok = txtU, tokU
				} else {
					nt := make([]string, len(rawTxt))
					nk := make([]int, len(rawTxt))
					for k := range rawTxt {
						if k < len(curTxt) {
							nt[k], nk[k] = curTxt[k], curTok[k]
						} else {
							nt[k], nk[k] = rawTxt[k], rawTok[k]
						}
					}
					txtU, tokU = nt, nk
					curTxt, curTok = nt, nk
				}
			}
			ph := prefixHashesEv(txtU)
			tr, tt, tu := sumEv(tokR, 0), sumEv(tokT, 0), sumEv(tokU, 0)
			reqs++
			cRaw += 0.1*float64(prevRaw) + 1.25*float64(max0Ev(tr-prevRaw))
			cTail += 0.1*float64(prevT) + 1.25*float64(max0Ev(tt-prevT))
			removedTail += tr - tt
			removedUnres += tr - tu
			// unrestricted forwarded history: where did it change versus the previous forwarded one?
			p := -1
			for k := 0; k < len(prevUtxt) && k < len(txtU); k++ {
				if prevUtxt[k] != txtU[k] {
					p = k
					break
				}
			}
			if p < 0 {
				cNo += 0.1*float64(prevU) + 1.25*float64(max0Ev(tu-prevU))
				cAn += 0.1*float64(prevU) + 1.25*float64(max0Ev(tu-prevU))
			} else {
				nChange++
				cNo += 1.25 * float64(tu) // only the system head survives
				best := -1
				for e := p - 2; e >= 0 && p-2-e <= lb; e-- {
					if h, ok := ends[e]; ok && h == ph[e] && r.ts-endTs[e] <= int64(ageMs) {
						best = e
						break
					}
				}
				if best >= 0 {
					nAnch++
					head := sumEv(tokU[:best+1], 0)
					cAn += 0.1*float64(head) + 1.25*float64(tu-head)
				} else {
					cAn += 1.25 * float64(tu)
				}
			}
			for e := range ends {
				if e >= len(ph) || ends[e] != ph[e] {
					delete(ends, e) // that entry no longer describes our prefix; a later request rewrote it
				}
			}
			ends[len(txtU)-1] = ph[len(txtU)-1]
			endTs[len(txtU)-1] = r.ts
			prevRaw, prevT, prevU, prevUtxt = tr, tt, tu, txtU
			_ = i
		}
		fmt.Printf("SESSH %s %.1f %.1f %.1f %.1f\n", k[strings.LastIndex(k, "|")+1:][:0]+fmt.Sprint(si), cRaw-r0, cTail-t0, cNo-n0, cAn-a0)
	}
	fmt.Printf("HOUSE pipe=%s sessions=%d requests=%d | raw %.0f | tail-only(status quo) %.0f (%.2f%% vs raw) | unrestricted no-anchor %.0f (%.2f%%) | unrestricted anchor %.0f (%.2f%%) | changed-prefix requests %d (anchorable %d) | removed token-requests tail %d unrestricted %d\n",
		spec, sessions, reqs, cRaw, cTail, 100*(cRaw-cTail)/cRaw, cNo, 100*(cRaw-cNo)/cRaw, cAn, 100*(cRaw-cAn)/cRaw, nChange, nAnch, removedTail, removedUnres)
}

func max0Ev(a int) int {
	if a < 0 {
		return 0
	}
	return a
}

func prefixHashesEv(txt []string) []string {
	h := sha1.New()
	out := make([]string, len(txt))
	for i, t := range txt {
		h.Write([]byte(t))
		h.Write([]byte{0})
		out[i] = fmt.Sprintf("%x", h.Sum(nil))[:12]
	}
	return out
}
