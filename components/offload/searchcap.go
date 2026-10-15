package offload

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
)

func init() { components.Register("searchcap", newSearchcap) }

// Searchcap is the REVERSIBLE form of rtk's per-file match cap: it keeps the first N hit rows
// of each file in a `grep -n`/`rg` listing and replaces the rest with one `path: +K more
// matches` line. The original is stashed and a <<cg:HASH>> marker names it, so the agent can
// call the expand tool and get every dropped row back. Nothing is lost, only deferred.
//
// It is an Offload (lossy until expanded), unlike searchfold, which is a lossless Reformat.
// List it BEFORE searchfold in a pipeline: the rows it keeps are still `path:line:text`, so
// searchfold folds them further. Output with no hit-row shape, or in which no file exceeds
// the cap, is left byte-identical.
//
// Cache safety is the same argument linecap makes: the rewrite is a pure function of the
// message's own text, so a message re-sent next turn rewrites to the same bytes.
type Searchcap struct {
	perFile  int
	maxFiles int
	keepLast bool
	minSize  int
	mode     markerMode
}

type searchcapConfig struct {
	PerFile    *int   `yaml:"per_file"`
	MaxFiles   *int   `yaml:"max_files"`
	KeepLast   *bool  `yaml:"keep_last"`
	MinSize    *int   `yaml:"min_size"`
	MarkerMode string `yaml:"marker_mode"`
}

const defaultSearchcapPerFile = 15

func newSearchcap(raw []byte) (components.Component, error) {
	var cfg searchcapConfig
	if err := components.Decode(raw, &cfg); err != nil {
		return nil, err
	}
	sc := &Searchcap{perFile: defaultSearchcapPerFile, keepLast: true,
		minSize: defaultMinSize, mode: parseMarkerMode(cfg.MarkerMode)}
	if cfg.PerFile != nil {
		sc.perFile = *cfg.PerFile
	}
	if cfg.MaxFiles != nil {
		sc.maxFiles = *cfg.MaxFiles
	}
	if cfg.KeepLast != nil {
		sc.keepLast = *cfg.KeepLast
	}
	if cfg.MinSize != nil {
		sc.minSize = *cfg.MinSize
	}
	return sc, nil
}

func (Searchcap) Name() string { return "searchcap" }

func (sc *Searchcap) Enabled(*components.Ctx) bool { return sc.perFile > 0 || sc.maxFiles > 0 }

func (sc *Searchcap) Offload(req *schemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) ([]string, error) {
	var keys []string
	changed := 0
	for i := range req.Input {
		m := &req.Input[i]
		if m.Role != schemas.ChatMessageRoleTool {
			continue
		}
		if !schema.Rewritable(*m) {
			rep.Gate("non_text_blocks")
			continue
		}
		content := schema.MessageText(*m)
		if len(content) < sc.minSize {
			rep.Gate("below_min_size")
			continue
		}
		if gate, skip := skipReduce(c, content); skip {
			rep.Gate(gate)
			continue
		}
		out, ok := CapSearchOutput(content, sc.perFile, sc.maxFiles, sc.keepLast)
		if !ok {
			rep.Gate("no_capped_matches")
			continue
		}
		newText, key, eff, ok := tryMark(c, sc.mode, content,
			" [full output: call "+expand.ToolName+"]",
			func(tok string) string {
				if tok == "" {
					return out
				}
				return out + "\n" + tok
			})
		if !ok {
			rep.Gate("marker_no_win")
			continue
		}
		if !commitMark(c, rep, eff, key, content) {
			continue
		}
		schema.SetMessageText(m, newText)
		if key != "" {
			keys = append(keys, key)
		}
		changed++
	}
	if changed == 0 {
		rep.Skipped = true
	}
	return keys, nil
}

// hitRow matches a `path:LINE:text` match row; ctxRow a `path-LINE-text` context row.
var (
	hitRow = regexp.MustCompile(`^([^\s:][^:]*):\d+:`)
	ctxRow = regexp.MustCompile(`^(.+?)-\d+-`)
)

// rowPath returns the file a row belongs to and whether it is a match (not context) row.
func rowPath(l string) (path string, hit, ok bool) {
	if m := hitRow.FindStringSubmatch(l); m != nil {
		return m[1], true, true
	}
	if m := ctxRow.FindStringSubmatch(l); m != nil {
		return m[1], false, true
	}
	return "", false, false
}

// CapSearchOutput keeps the first perFile match rows of every file group (plus the last one
// when keepLast, so the line RANGE of the file stays visible; context rows trailing that last
// match are dropped uncounted, recoverable via expand) and, when maxFiles > 0, only the
// first maxFiles groups. Each elision is one visible line: `path: +K more matches` and
// `+K more files (M matches)`. It returns ok=false for output that is not a hit listing
// (under half the non-blank lines are rows) or in which nothing needed capping.
// Exported so a measurement harness can run it over captured output.
func CapSearchOutput(s string, perFile, maxFiles int, keepLast bool) (string, bool) {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	type group struct {
		path       string
		start, end int // lines[start:end]
	}
	var groups []group
	rows, nonblank := 0, 0
	broken := true // a non-row, non-`--` line since the last row ends the group
	for i, l := range lines {
		if strings.TrimSpace(l) != "" && l != "--" {
			nonblank++
		}
		p, _, ok := rowPath(l)
		if !ok {
			broken = broken || l != "--"
			continue
		}
		rows++
		if n := len(groups); n > 0 && groups[n-1].path == p && !broken {
			groups[n-1].end = i + 1
		} else {
			groups = append(groups, group{p, i, i + 1})
		}
		broken = false
	}
	if rows < 4 || rows*2 < nonblank {
		return s, false
	}
	var out []string
	prev := 0
	dropped := 0
	files, hidden, hiddenFiles := 0, 0, 0
	for _, g := range groups {
		out = append(out, lines[prev:g.start]...) // separators / non-row lines between groups
		prev = g.end
		if maxFiles > 0 && files >= maxFiles {
			hiddenFiles++
			for _, l := range lines[g.start:g.end] {
				if _, hit, _ := rowPath(l); hit {
					hidden++
				}
			}
			continue
		}
		files++
		var hits []int
		for i := g.start; i < g.end; i++ {
			if _, hit, _ := rowPath(lines[i]); hit {
				hits = append(hits, i)
			}
		}
		if perFile <= 0 || len(hits) <= perFile || (keepLast && len(hits) == perFile+1) {
			out = append(out, lines[g.start:g.end]...)
			continue
		}
		cut := hits[perFile] // first dropped match row
		last := -1
		if keepLast {
			last = hits[len(hits)-1]
		}
		out = append(out, lines[g.start:cut]...)
		k := len(hits) - perFile
		if last >= 0 {
			k--
		}
		out = append(out, g.path+": +"+strconv.Itoa(k)+" more matches")
		dropped += k
		if last >= 0 {
			out = append(out, lines[last])
		}
	}
	out = append(out, lines[prev:]...)
	if hiddenFiles > 0 {
		out = append(out, "+"+strconv.Itoa(hiddenFiles)+" more files ("+strconv.Itoa(hidden)+" matches)")
	}
	if dropped == 0 && hiddenFiles == 0 {
		return s, false
	}
	return strings.Join(out, "\n"), true
}

func init() {
	components.RegisterFields("searchcap", searchcapConfig{}, []components.Field{
		{Key: "per_file", Type: components.FieldInt, Default: defaultSearchcapPerFile,
			Hint: "Keep the first N match rows of each file in a grep/rg listing; the rest become one '+K more matches' line. 0 disables the per-file cap."},
		{Key: "max_files", Type: components.FieldInt, Default: 0,
			Hint: "Keep only the first N files of a listing, with a '+K more files' line. 0 keeps all files."},
		{Key: "keep_last", Type: components.FieldBool, Default: true,
			Hint: "Also keep the last match of a capped file, so the span of lines it matches stays visible."},
		{Key: "min_size", Type: components.FieldInt, Default: defaultMinSize,
			Hint: "Only rewrite an output at least this many bytes long — below it a marker usually costs more than the saving."},
		markerModeField(),
	})
}
