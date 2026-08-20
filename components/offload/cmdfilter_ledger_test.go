// Mining the FilterMiss ledger, and measuring the filters written from it, against
// captures of REAL agent traffic with the repo's own tokenizer (internal/tokens via
// schema.TextTokens — never bytes/4).
//
// Captures hold real traffic and are never committed, so everything here skips unless
// pointed at one:
//
//	CG_CORPUS='/home/vpcuser/cg-research/bench/*.jsonl' \
//	  go test ./components/offload -run 'Ledger|AgentFilter' -v
//
// The in-process ledger (metrics.Aggregator.FilterMiss) ranks unmatched shapes by
// COUNT and holds only the first line of the selector. That is enough to notice a shape
// but not to prioritise: the tokens are what cost money, and the COMMAND is what a
// filter keys on. This harness replays the same selector logic offline and ranks by
// tokens per command, which is the ledger the filter set was actually written from.
package offload

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/components/dsl"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
	"github.com/tidwall/gjson"
)

// ledgerPair is one captured tool result with the command that produced it.
type ledgerPair struct {
	cmd, out string
	tokens   int
}

// ledgerCorpus returns the DISTINCT (command, output) pairs across every capture
// matched by CG_CORPUS. Distinct, because a transcript re-sends every earlier result on
// every later turn; counting each turn would scale every number by the session length.
func ledgerCorpus(t *testing.T) []ledgerPair {
	t.Helper()
	spec := os.Getenv("CG_CORPUS")
	if spec == "" {
		t.Skip("set CG_CORPUS to captured-traffic jsonl globs")
	}
	seen := map[string]bool{}
	var out []ledgerPair
	for _, g := range strings.Split(spec, ",") {
		files, _ := filepath.Glob(strings.TrimSpace(g))
		sort.Strings(files)
		for _, file := range files {
			f, err := os.Open(file)
			if err != nil {
				continue
			}
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 0, 1<<20), 256<<20)
			for sc.Scan() {
				cmdByID := map[string]string{}
				gjson.GetBytes(sc.Bytes(), "body.messages.#.content|@flatten").ForEach(func(_, blk gjson.Result) bool {
					switch blk.Get("type").String() {
					case "tool_use":
						cmd := blk.Get("input.command").String()
						if cmd == "" {
							cmd = blk.Get("name").String()
							if p := blk.Get("input.file_path").String(); p != "" {
								cmd += " " + p
							} else if p := blk.Get("input.pattern").String(); p != "" {
								cmd += " " + p
							}
						}
						cmdByID[blk.Get("id").String()] = cmd
					case "tool_result":
						txt := blk.Get("content").String()
						if c := blk.Get("content"); c.IsArray() {
							var b strings.Builder
							c.ForEach(func(_, p gjson.Result) bool { b.WriteString(p.Get("text").String()); return true })
							txt = b.String()
						}
						cmd := cmdByID[blk.Get("tool_use_id").String()]
						if txt == "" || seen[cmd+"\x00"+txt] {
							return true
						}
						seen[cmd+"\x00"+txt] = true
						out = append(out, ledgerPair{cmd: cmd, out: txt, tokens: schema.TextTokens(txt)})
					}
					return true
				})
			}
			f.Close()
		}
	}
	if len(out) == 0 {
		t.Skipf("CG_CORPUS %q matched no tool outputs", spec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].cmd != out[j].cmd {
			return out[i].cmd < out[j].cmd
		}
		return out[i].out < out[j].out
	})
	return out
}

// cmdClass buckets a command line into the thing a filter would key on: the program,
// plus a subcommand for the multiplexers where the subcommand decides the output shape.
// Pipelines are classed by their LAST stage, which is what shapes the output.
var (
	ledgerEnvPrefix = regexp.MustCompile(`^(\s*(sudo|env|time|nohup|[A-Z_][A-Z0-9_]*=\S*)\s+)+`)
	ledgerSubcmd    = map[string]bool{"git": true, "go": true, "cargo": true, "npm": true, "docker": true, "kubectl": true, "python": true, "python3": true, "uv": true, "pip": true, "yarn": true, "pnpm": true, "make": true, "gh": true}
)

func cmdClass(cmd string) string {
	if cmd == "" {
		return "(unpaired)"
	}
	seg := ledgerEnvPrefix.ReplaceAllString(strings.TrimSpace(lastStage(cmd)), "")
	f := strings.Fields(seg)
	if len(f) == 0 {
		return "(empty)"
	}
	prog := filepath.Base(f[0])
	if ledgerSubcmd[prog] {
		for _, a := range f[1:] {
			if !strings.HasPrefix(a, "-") {
				return prog + " " + filepath.Base(a)
			}
		}
	}
	return prog
}

// lastStage returns the final stage of a pipeline/chain. It has to be quote-aware:
// splitting naively on `|` classed `grep -n "a|b" x` under the class `b"`, and real agent
// traffic is full of alternation-bearing grep patterns — the first version of this ledger
// had a dozen such phantom classes crowding out the real ones.
func lastStage(cmd string) string {
	start, i := 0, 0
	for i < len(cmd) {
		switch c := cmd[i]; c {
		case '\'', '"':
			for i++; i < len(cmd) && cmd[i] != c; i++ {
				if cmd[i] == '\\' && c == '"' {
					i++
				}
			}
		case '|', ';', '&':
			for i < len(cmd) && (cmd[i] == '|' || cmd[i] == ';' || cmd[i] == '&') {
				i++
			}
			start = i
			continue
		}
		i++
	}
	return cmd[start:]
}

// TestLedgerRankedMisses is the deliverable: which commands' output reaches cmdfilter
// and matches nothing, ranked by TOKENS, not by count.
func TestLedgerRankedMisses(t *testing.T) {
	pairs := ledgerCorpus(t)
	reg := &dsl.Registry{}
	if err := reg.Load([]byte(builtinFilters)); err != nil {
		t.Fatal(err)
	}
	type row struct {
		class            string
		hits, miss       int
		hitTok, missTok  int
		belowFloor       int
		belowFloorTokens int
	}
	by := map[string]*row{}
	total, reach := 0, 0
	for _, p := range pairs {
		total += p.tokens
		r := by[cmdClass(p.cmd)]
		if r == nil {
			r = &row{class: cmdClass(p.cmd)}
			by[cmdClass(p.cmd)] = r
		}
		if len(p.out) < defaultMinSize {
			r.belowFloor++
			r.belowFloorTokens += p.tokens
			continue
		}
		reach += p.tokens
		if reg.Match(matchKey(schema.ToolCall{Name: "Bash", Args: `{"command":` + jsonQuote(p.cmd) + `}`}, selectorKey(p.out))) != nil {
			r.hits++
			r.hitTok += p.tokens
		} else {
			r.miss++
			r.missTok += p.tokens
		}
	}
	rows := make([]*row, 0, len(by))
	for _, r := range by {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].missTok != rows[j].missTok {
			return rows[i].missTok > rows[j].missTok
		}
		return rows[i].class < rows[j].class
	})
	t.Logf("corpus: %d distinct (command,output) pairs, %d tokens; %d tokens reach the filter (>= %d bytes)",
		len(pairs), total, reach, defaultMinSize)
	t.Logf("%-28s %8s %8s %8s %8s %8s", "command class", "missTok", "misses", "hitTok", "hits", "<floor")
	for i, r := range rows {
		if i >= 30 && r.missTok == 0 {
			break
		}
		t.Logf("%-28s %8d %8d %8d %8d %8d", r.class, r.missTok, r.miss, r.hitTok, r.hits, r.belowFloor)
	}
}

func jsonQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// TestLedgerBiggestMisses prints the largest individual unmatched outputs with their
// command and head, so a filter is written against what the corpus actually holds.
func TestLedgerBiggestMisses(t *testing.T) {
	pairs := ledgerCorpus(t)
	reg := &dsl.Registry{}
	if err := reg.Load([]byte(builtinFilters)); err != nil {
		t.Fatal(err)
	}
	var miss []ledgerPair
	for _, p := range pairs {
		if len(p.out) < defaultMinSize {
			continue
		}
		if reg.Match(matchKey(schema.ToolCall{Name: "Bash", Args: `{"command":` + jsonQuote(p.cmd) + `}`}, selectorKey(p.out))) == nil {
			miss = append(miss, p)
		}
	}
	sort.SliceStable(miss, func(i, j int) bool { return miss[i].tokens > miss[j].tokens })
	n := 25
	if len(miss) < n {
		n = len(miss)
	}
	for _, p := range miss[:n] {
		head := strings.SplitN(p.out, "\n", 4)
		if len(head) > 3 {
			head = head[:3]
		}
		for i := range head {
			if len(head[i]) > 90 {
				head[i] = head[i][:90]
			}
		}
		t.Logf("%6d tok | %-40.40s | %s", p.tokens, cmdClass(p.cmd)+"  ("+p.cmd+")", strings.Join(head, " ⏎ "))
	}
}

// ledgerPrice is the effective $/token a REMOVED input token is worth on this traffic.
//
// It is not the fresh-input rate. The captured corpus is 90.54% cache_read, and a provider
// bills cache_read at 0.1x fresh and cache_write at 1.25x — so the blend is
// 0.9054*0.1 + 0.0946*1.25 = 0.2088 of fresh. At Sonnet's $3/Mtok input that is
// $0.412/Mtok. Quoting the fresh rate instead would overstate every number below by 4.8x,
// which is the convention error this repo refuses (docs/results, cg-measurement-conventions).
const ledgerPrice = 3.0 / 1e6 * (0.9054*0.1 + 0.0946*1.25)

// TestAgentFilterCorpusMeasure is the per-filter measurement: fire rate and tokens
// removed for each agent_filters mode, marker-inclusive and never-worse-gated exactly as
// cmdfilter applies them. Run it per arm to get per-arm numbers:
//
//	for a in short long mixed cold; do CG_CORPUS="/home/vpcuser/cg-research/bench/$a.jsonl" \
//	  go test ./components/offload -run AgentFilterCorpusMeasure -v; done
func TestAgentFilterCorpusMeasure(t *testing.T) {
	pairs := ledgerCorpus(t)
	total := 0
	for _, p := range pairs {
		total += p.tokens
	}
	for _, mode := range []string{"off", "safe", "lossy"} {
		reg := &dsl.Registry{}
		if err := reg.Load([]byte(builtinFilters)); err != nil {
			t.Fatal(err)
		}
		docs, err := agentFilterDocs(mode)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range docs {
			if err := reg.Load([]byte(d)); err != nil {
				t.Fatal(err)
			}
		}
		type agg struct{ fired, before, saved int }
		by := map[string]*agg{}
		sum := 0
		for _, p := range pairs {
			if len(p.out) < defaultMinSize {
				continue
			}
			c := reg.Match(matchKey(schema.ToolCall{Name: "Bash", Args: `{"command":` + jsonQuote(p.cmd) + `}`}, selectorKey(p.out)))
			if c == nil {
				continue
			}
			out, loss := dsl.Apply(c, p.out)
			if out == p.out {
				continue
			}
			newText := out + "\n" + expand.Marker("0123456789abcdef") + recoveryHint(loss, len(strings.Split(out, "\n")))
			before, after := schema.TextTokens(p.out), schema.TextTokens(newText)
			if after >= before {
				continue
			}
			a := by[c.Name]
			if a == nil {
				a = &agg{}
				by[c.Name] = a
			}
			a.fired++
			a.before += before
			a.saved += before - after
			sum += before - after
		}
		names := make([]string, 0, len(by))
		for n := range by {
			names = append(names, n)
		}
		sort.Slice(names, func(i, j int) bool {
			if by[names[i]].saved != by[names[j]].saved {
				return by[names[i]].saved > by[names[j]].saved
			}
			return names[i] < names[j]
		})
		t.Logf("=== agent_filters: %s — %d tokens removed (%.2f%% of %d), $%.4f", mode, sum,
			100*float64(sum)/float64(total), total, float64(sum)*ledgerPrice)
		for _, n := range names {
			a := by[n]
			t.Logf("  %-18s fired %4d/%-4d (%5.2f%%)  saved %6d tok (%.2f%% of corpus, %4.1f%% of what it saw)  $%.4f",
				n, a.fired, len(pairs), 100*float64(a.fired)/float64(len(pairs)), a.saved,
				100*float64(a.saved)/float64(total), 100*float64(a.saved)/float64(a.before), float64(a.saved)*ledgerPrice)
		}
	}
}
