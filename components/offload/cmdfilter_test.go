package offload

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/components/dsl"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
	"regexp"

	"gopkg.in/yaml.v3"
)

func builtinDoc(t *testing.T) dsl.File {
	t.Helper()
	var f dsl.File
	if err := yaml.Unmarshal([]byte(builtinFilters), &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// The builtins load (which also RUNS every inline test — see dsl.Registry.Load).
func TestBuiltinFiltersLoad(t *testing.T) {
	var r dsl.Registry
	if err := r.Load([]byte(builtinFilters)); err != nil {
		t.Fatal(err)
	}
	if r.Len() < 20 {
		t.Fatalf("expected the full ported set, got %d filters", r.Len())
	}
}

// Guardrail (rtk's build.rs): every shipped filter must carry at least one test,
// and every test's input must actually ROUTE to its own filter. The second half is
// the check that makes the selector rewrite verifiable — an rtk `match_command`
// regex copied over would compile fine and simply never fire.
func TestEveryBuiltinFilterHasTestsAndRoutes(t *testing.T) {
	f := builtinDoc(t)
	var r dsl.Registry
	if err := r.Load([]byte(builtinFilters)); err != nil {
		t.Fatal(err)
	}
	for name := range f.Filters {
		cases := f.Tests[name]
		if len(cases) == 0 {
			t.Errorf("filter %q ships no tests", name)
			continue
		}
		for _, tc := range cases {
			if strings.TrimSpace(tc.Input) == "" {
				continue // an empty-input case has no selector to route
			}
			got := r.Match(selectorKey(tc.Input))
			if got == nil {
				t.Errorf("filter %q test %q: selector %q matches NO filter", name, tc.Name, selectorKey(tc.Input))
				continue
			}
			if got.Name != name {
				t.Errorf("filter %q test %q: selector %q routes to %q instead", name, tc.Name, selectorKey(tc.Input), got.Name)
			}
		}
	}
}

// Every success-collapse rule must carry an unless guard: in a proxy the agent
// cannot re-run the command to find the warning a bare collapse swallowed.
// Collapsing output to a one-line summary is the dangerous operation in this package: in a
// proxy the agent cannot re-run the command to find the warning that got swallowed. TWO
// mechanisms collapse and they are safe for DIFFERENT reasons, so each needs its own
// assertion — checking only `match_output` (as this test originally did) leaves the 12
// `on_empty` filters, including the heaviest-firing one, entirely unguarded by any test.
//
//   - match_output: guarded. Every rule names the diagnostics that veto the collapse.
//   - on_empty: structural. It fires only when strip_lines_matching removed EVERYTHING, and
//     every strip list is an explicit allow-list of known boilerplate with no catch-all — so
//     an unrecognised diagnostic is not stripped, the output is not empty, and the collapse
//     never fires. An `unless` guard would be redundant.
//
// The on_empty half is why this test exists. Adding `.*` to a strip list would silently void
// the guarantee and NOTHING else would catch it: the filter's own inline tests would still
// pass, because they exercise the boilerplate it was written for.
func TestEveryMatchOutputRuleIsGuarded(t *testing.T) {
	// Matches a pattern that can consume ANY line: `.*`, `.+`, and the anchored forms.
	catchAll := regexp.MustCompile(`^\^?\.[*+]\$?$`)

	guarded, structural := 0, 0
	for name, def := range builtinDoc(t).Filters {
		for i, rule := range def.MatchOutput {
			if rule.Unless == "" {
				t.Errorf("filter %q match_output[%d] (%q) has no unless guard", name, i, rule.Pattern)
			}
			guarded++
		}
		if def.OnEmpty == nil {
			continue
		}
		structural++
		for _, pat := range def.StripLinesMatching {
			if catchAll.MatchString(pat) {
				t.Errorf("filter %q strips with catch-all %q while on_empty collapses to %q: an "+
					"unrecognised diagnostic would be stripped, leaving the output empty, and the "+
					"collapse would then hide it. Strip lists must stay explicit allow-lists",
					name, pat, *def.OnEmpty)
			}
		}
	}
	if structural == 0 {
		t.Fatal("no on_empty filters found; the structural half of the invariant is untested")
	}
	t.Logf("collapse rules: %d guarded (match_output), %d structural (on_empty)", guarded, structural)
}

// Every filter must declare a family, else its savings land in "other" and the
// per-family attribution in /stats is useless.
func TestEveryBuiltinFilterHasFamily(t *testing.T) {
	for name, def := range builtinDoc(t).Filters {
		if def.Family == "" {
			t.Errorf("filter %q has no family", name)
		}
	}
}

// A tail cut is recoverable more cheaply than a whole-blob loss, and the hint must
// say so rather than pushing the agent to the expensive path for both.
func TestRecoveryHintDistinguishesTailFromWhole(t *testing.T) {
	if got := recoveryHint(dsl.LossNone, 3); got != "" {
		t.Fatalf("no loss must emit no hint, got %q", got)
	}
	tail, whole := recoveryHint(dsl.LossTail, 42), recoveryHint(dsl.LossWhole, 42)
	if tail == whole {
		t.Fatal("LossTail and LossWhole must not share one hint")
	}
	if !strings.Contains(tail, "42") || !strings.Contains(tail, "truncated") {
		t.Fatalf("tail hint should name the cut point: %q", tail)
	}
	if !strings.Contains(whole, expand.ToolName) {
		t.Fatalf("whole-blob hint must name the expand tool: %q", whole)
	}
}

func cmdToolMsg(s string) schemas.ChatMessage {
	c := s
	return schemas.ChatMessage{Role: schemas.ChatMessageRoleTool, Content: &schemas.ChatMessageContent{ContentStr: &c}}
}

func newFilterComp(t *testing.T, cfg string) components.Offload {
	t.Helper()
	comp, err := newCmdfilter([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	return comp.(components.Offload)
}

func runFilter(t *testing.T, f components.Offload, text string) (string, *components.Report) {
	t.Helper()
	req := &schemas.BifrostChatRequest{Provider: schemas.Anthropic, Input: []schemas.ChatMessage{cmdToolMsg(text)}}
	c := &components.Ctx{Ctx: context.Background(), Session: "s", Store: store.NewMemory(store.Options{}), MaxCachedIdx: -1}
	rep := &components.Report{}
	if _, err := f.Offload(req, rep, c); err != nil {
		t.Fatal(err)
	}
	return schema.MessageText(req.Input[0]), rep
}

// Below the size floor the marker routinely costs more than the filter saves, so
// nothing is touched at all (no stash, no marker).
func TestSizeFloorSkipsSmallOutputs(t *testing.T) {
	small := "make[1]: Entering directory '/x'\ngcc -O2 foo.c\nmake[1]: Leaving directory '/x'\n"
	if len(small) >= defaultMinSize {
		t.Fatalf("fixture is not below the floor (%d bytes)", len(small))
	}
	out, rep := runFilter(t, newFilterComp(t, ""), small)
	if out != small || !rep.Skipped {
		t.Fatalf("output below the size floor must be untouched: %q skipped=%v", out, rep.Skipped)
	}
	out2, _ := runFilter(t, newFilterComp(t, "min_size: 1\n"), small)
	if strings.Contains(out2, "Entering directory") {
		t.Fatalf("with min_size: 1 the chatter should be stripped: %q", out2)
	}
}

// The floor sits at 400 because it was MEASURED there, not inherited from rtk's 500.
// This is the case that decided it: a real captured pip output that is nothing but the
// two fixed advisories — 400 <= len < 500, so the old floor refused it while the
// marker-inclusive never-worse guard accepts it. If someone raises the floor back
// toward 500 this is the saving that silently disappears, so pin it behaviorally
// rather than asserting the constant against itself.
func TestSizeFloorAdmitsTheMeasuredBand(t *testing.T) {
	const pipNag = "WARNING: Running pip as the 'root' user can result in broken permissions and " +
		"conflicting behaviour with the system package manager, possibly rendering your system " +
		"unusable. It is recommended to use a virtual environment instead: " +
		"https://pip.pypa.io/warnings/venv. Use the --root-user-action option if you know what " +
		"you are doing and want to suppress this warning.\n\n" +
		"[notice] A new release of pip is available: 25.2 -> 26.2.1\n" +
		"[notice] To update, run: pip install --upgrade pip\n"
	if len(pipNag) < defaultMinSize || len(pipNag) >= 500 {
		t.Fatalf("fixture must sit in the 400..499 band the sweep opened up, got %d bytes", len(pipNag))
	}
	out, rep := runFilter(t, newFilterComp(t, ""), pipNag)
	if rep.Skipped || !strings.Contains(out, "pip: install ok") {
		t.Fatalf("output in the measured band must be filtered at the default floor: %q skipped=%v", out, rep.Skipped)
	}
	if !expand.HasPlaceholder(out) {
		t.Fatalf("the rewrite must stay reversible: %q", out)
	}
	// And the floor still refuses the same content when configured back to 500.
	out500, rep500 := runFilter(t, newFilterComp(t, "min_size: 500\n"), pipNag)
	if out500 != pipNag || !rep500.Skipped {
		t.Fatalf("min_size: 500 must still refuse it: %q skipped=%v", out500, rep500.Skipped)
	}
	if rep500.Gates["below_min_size"] != 1 {
		t.Fatalf("the refusal must be attributed to the floor, gates=%v", rep500.Gates)
	}
}

func planFixture() string {
	var b strings.Builder
	b.WriteString("Acquiring state lock. This may take a few moments...\n")
	for i := 0; i < 40; i++ {
		b.WriteString("Refreshing state... [id=subnet-0000000" + string(rune('a'+i%26)) + "]\n")
	}
	b.WriteString("\nTerraform will perform the following actions:\n\n  # aws_instance.web will be created\n  # (14 unchanged attributes hidden)\n\nPlan: 1 to add, 0 to change, 0 to destroy.\n")
	return b.String()
}

// Compression floors rtk asserts for its own equivalents (terraform-plan, make).
func TestCompressionFloors(t *testing.T) {
	var r dsl.Registry
	if err := r.Load([]byte(builtinFilters)); err != nil {
		t.Fatal(err)
	}
	var mk strings.Builder
	for i := 0; i < 30; i++ {
		mk.WriteString("make[1]: Entering directory '/src/pkg'\ncc -c -O2 file.c\nmake[1]: Leaving directory '/src/pkg'\n")
	}
	for _, tc := range []struct{ name, input string }{
		{"terraform-plan", planFixture()},
		{"make", mk.String()},
	} {
		c := r.Match(selectorKey(tc.input))
		if c == nil || c.Name != tc.name {
			t.Fatalf("%s fixture routed to %v", tc.name, c)
		}
		out, _ := dsl.Apply(c, tc.input)
		saved := 1 - float64(schema.TextTokens(out))/float64(schema.TextTokens(tc.input))
		if saved < 0.60 {
			t.Errorf("%s saved only %.1f%% (floor 60%%)", tc.name, saved*100)
		}
	}
}

// The per-family ledger is what lets /stats say which families pay off.
type fakeStats struct {
	acts   []string
	misses []string
}

func (f *fakeStats) FilterAct(family, filter, key string, saved int) {
	f.acts = append(f.acts, family+"/"+filter)
}
func (f *fakeStats) FilterMiss(sel string) { f.misses = append(f.misses, sel) }

func TestFamilyMetricsAndSelectorMisses(t *testing.T) {
	fs := &fakeStats{}
	req := &schemas.BifrostChatRequest{Provider: schemas.Anthropic, Input: []schemas.ChatMessage{
		cmdToolMsg(planFixture()),
		cmdToolMsg("Totally unrecognized first line\n" + strings.Repeat("filler line to clear the size floor\n", 30)),
	}}
	c := &components.Ctx{Ctx: context.Background(), Session: "s", Store: store.NewMemory(store.Options{}), MaxCachedIdx: -1, FilterStats: fs}
	if _, err := newFilterComp(t, "").Offload(req, &components.Report{}, c); err != nil {
		t.Fatal(err)
	}
	if len(fs.acts) != 1 || fs.acts[0] != "iac/terraform-plan" {
		t.Fatalf("expected one iac/terraform-plan act, got %v", fs.acts)
	}
	if len(fs.misses) != 1 || fs.misses[0] != "Totally unrecognized first line" {
		t.Fatalf("expected the unmatched selector to be logged, got %v", fs.misses)
	}
}

// The per-filter ledger is an ENFORCED-namespace field with no potential_* counterpart, so
// an observe-mode run populating it reports savings that never happened — and a consumer
// cannot tell them apart from real ones. #31 named that as its primary correctness risk: a
// mislabelled hypothetical is worse than no number, because it inflates the product's own
// headline claim.
//
// The gate lives on Ctx.Stats() rather than at this component's two call sites, so it also
// covers the next sink added to Ctx — a component author reaching for c.FilterStats has no
// reason to think about modes. Asserting that sync DOES record proves the gate rather than a
// dead sink.
func TestObserveModeDoesNotRecordFilterStats(t *testing.T) {
	const aptOut = "Reading package lists...\nSetting up git (1:2.43.0-1ubuntu7.3) ...\n" +
		"Processing triggers for libc-bin (2.39-0ubuntu8.6) ...\n"

	for _, tc := range []struct {
		mode       components.Mode
		wantRecord bool
	}{{components.ModeSync, true}, {components.ModeObserve, false}} {
		sink := &recordingSink{}
		f := newFilterComp(t, "min_size: 1\n")
		req := &schemas.BifrostChatRequest{Provider: schemas.Anthropic,
			Input: []schemas.ChatMessage{cmdToolMsg(aptOut)}}
		c := &components.Ctx{Ctx: context.Background(), Session: "s",
			Store: store.NewMemory(store.Options{}), MaxCachedIdx: -1,
			Mode: tc.mode, FilterStats: sink}
		if _, err := f.Offload(req, &components.Report{}, c); err != nil {
			t.Fatalf("mode=%s: %v", tc.mode, err)
		}
		if got := sink.acts + sink.misses; (got > 0) != tc.wantRecord {
			t.Errorf("mode=%s: %d ledger events (acts=%d misses=%d), wantRecord=%v",
				tc.mode, got, sink.acts, sink.misses, tc.wantRecord)
		}
	}
}

type recordingSink struct{ acts, misses int }

func (r *recordingSink) FilterAct(_, _, _ string, _ int) { r.acts++ }
func (r *recordingSink) FilterMiss(string)               { r.misses++ }

// maxMissKeys bounds how MANY keys the selector-miss ledger holds, not how big they are, and
// selectorKey runs on whatever the tool returned. So on multimodal traffic the top slots filled
// with base64 image payloads: the ledger exists to answer "which filter is worth writing next",
// and 200 image blobs answer nothing while sitting in the aggregator under its lock and
// shipping in every /stats scrape.
func TestMissLedgerKeysAreBoundedAndTextOnly(t *testing.T) {
	long := "docker build -t " + strings.Repeat("x", 400) + " ."
	if got := firstLine(long); len(got) > maxMissKeyLen {
		t.Errorf("key not bounded: %d bytes (cap %d)", len(got), maxMissKeyLen)
	} else if !strings.HasPrefix(got, "docker build") {
		t.Errorf("truncation lost the identifying prefix: %q", got)
	}

	// A cut must not split a multi-byte rune, or the key ships as invalid UTF-8 in the payload.
	if got := firstLine("build " + strings.Repeat("é", 200)); !utf8.ValidString(got) {
		t.Errorf("truncated key is not valid UTF-8: %q", got)
	}

	// Non-text blocks carry no output shape a filter could ever match, so they are dropped
	// rather than truncated — a truncated blob is still a useless ledger entry.
	for _, blob := range []string{
		`[{"type":"image","source":{"type":"base64","data":"iVBORw0KGgoAAAANSUhEUgAAAoAAAAKACAIAAACDr150AACQZUlEQVR4nO3dd3wUZf4H8O"}}]`,
		`{"type": "image", "source": {"data": "` + strings.Repeat("A", 120) + `"}}`,
	} {
		if got := firstLine(blob); got != "" {
			t.Errorf("non-text block recorded as a miss shape: %q", got)
		}
	}

	// A real command banner must still survive intact — the bound must not cost the signal.
	for _, ok := range []string{"Reading package lists...", "> Task :app:compileDebugKotlin", "make[1]: Entering directory '/src'"} {
		if got := firstLine(ok); got != ok {
			t.Errorf("real selector altered: %q -> %q", ok, got)
		}
	}
}

// A REAL pip install output, captured verbatim from a Terminal-Bench trial (Claude
// Agent SDK traffic). It is the top-ranked unmatched shape in the selector-miss ledger
// for a Python workload — 24 builtin filters covered pytest, npm, poetry, uv, composer,
// bundler, apt and brew, but not pip itself, the one installer nearly every Python task
// runs. The three trailing advisory lines are pure noise: the root-user warning and the
// pip-upgrade notices are fixed text the agent can never act on, while the
// "Successfully installed" manifest is the result and must survive.
const capturedPipInstall = "Successfully installed contourpy-1.3.3 cycler-0.12.1 " +
	"fonttools-4.63.0 kiwisolver-1.5.0 matplotlib-3.10.7 numpy-2.3.4 packaging-25.0 " +
	"pillow-12.0.0 pyparsing-3.2.5 python-dateutil-2.9.0.post0 six-1.17.0\n" +
	"WARNING: Running pip as the 'root' user can result in broken permissions and " +
	"conflicting behaviour with the system package manager. It is recommended to use a " +
	"virtual environment instead: https://pip.pypa.io/warnings/venv\n" +
	"\n[notice] A new release of pip is available: 25.2 -> 26.2.1\n" +
	"[notice] To update, run: pip install --upgrade pip\n"

func TestPipInstallOutputIsFiltered(t *testing.T) {
	var r dsl.Registry
	if err := r.Load([]byte(builtinFilters)); err != nil {
		t.Fatal(err)
	}
	filt := r.Match(selectorKey(capturedPipInstall))
	if filt == nil {
		t.Fatalf("no builtin filter matches a pip install output; selector was %q",
			selectorKey(capturedPipInstall))
	}
	out, _ := dsl.Apply(filt, capturedPipInstall)
	if schema.TextTokens(out) >= schema.TextTokens(capturedPipInstall) {
		t.Errorf("filter %q did not shrink the output: %d -> %d tokens",
			filt.Name, schema.TextTokens(capturedPipInstall), schema.TextTokens(out))
	}
	// The result manifest is the whole point of the output — it must survive.
	if !strings.Contains(out, "Successfully installed contourpy-1.3.3") {
		t.Errorf("the install manifest was dropped: %q", out)
	}
	// The unactionable advisories must be gone.
	for _, noise := range []string{"root' user", "[notice]"} {
		if strings.Contains(out, noise) {
			t.Errorf("advisory noise %q survived: %q", noise, out)
		}
	}
}

// A REAL pdflatex run, captured verbatim from a Terminal-Bench trial. Second-ranked
// unmatched shape in the selector-miss ledger (13 occurrences of a ~614-token log).
// The signal is the Overfull/Underfull warnings and the output summary; the engine
// banner and the absolute texlive package paths are fixed boilerplate.
const capturedPdflatex = "/usr/bin/pdflatex\n" +
	"This is pdfTeX, Version 3.141592653-2.6-1.40.25 (TeX Live 2023/Debian) (preloaded format=pdflatex)\n" +
	" restricted \\write18 enabled.\nentering extended mode\n(./main.tex\n" +
	"LaTeX2e <2023-11-01> patch level 1\nL3 programming layer <2024-01-22>\n" +
	"(/usr/share/texlive/texmf-dist/tex/latex/base/article.cls\n" +
	"Document Class: article 2023/05/17 v1.4n Standard LaTeX document class\n" +
	"(/usr/share/texlive/texmf-dist/tex/latex/base/size10.clo))\n" +
	"(/usr/share/texlive/texmf-dist/tex/latex/l3backend/l3backend-pdftex.def)\n" +
	"No file main.aux.\n(./input.tex\n" +
	"Overfull \\hbox (0.10312pt too wide) in paragraph at lines 5--6\n" +
	"\\OT1/cmr/m/n/10 many cu-ri-ous na-tures to me and also made\n\n" +
	"[1{/var/lib/texmf/fonts/map/pdftex/updmap/pdftex.map}]\n" +
	"[5] (./main.aux) )\n(see the transcript file for additional information)" +
	"</usr/share/texlive/texmf-dist/fonts/type1/public/amsfonts/cm/cmr10.pfb>\n" +
	"Output written on main.pdf (5 pages, 29584 bytes).\n" +
	"Transcript written on main.log.\n"

func TestPdflatexOutputIsFiltered(t *testing.T) {
	var r dsl.Registry
	if err := r.Load([]byte(builtinFilters)); err != nil {
		t.Fatal(err)
	}
	filt := r.Match(selectorKey(capturedPdflatex))
	if filt == nil {
		t.Fatalf("no builtin filter matches a pdflatex log; selector was %q",
			selectorKey(capturedPdflatex))
	}
	out, _ := dsl.Apply(filt, capturedPdflatex)
	if schema.TextTokens(out) >= schema.TextTokens(capturedPdflatex) {
		t.Errorf("filter %q did not shrink the log: %d -> %d tokens",
			filt.Name, schema.TextTokens(capturedPdflatex), schema.TextTokens(out))
	}
	// The diagnostics and the result are the signal — they must survive verbatim.
	for _, keep := range []string{
		"Overfull \\hbox (0.10312pt too wide) in paragraph at lines 5--6",
		"Output written on main.pdf (5 pages, 29584 bytes).",
	} {
		if !strings.Contains(out, keep) {
			t.Errorf("dropped signal %q from %q", keep, out)
		}
	}
	// Fixed engine/package boilerplate must be gone.
	for _, noise := range []string{"This is pdfTeX", "texmf-dist/tex/latex/base/article.cls",
		"Transcript written on", "entering extended mode"} {
		if strings.Contains(out, noise) {
			t.Errorf("boilerplate %q survived: %q", noise, out)
		}
	}

	// The line budget must not eat the RESULT. A TeX log's most important line sits at
	// the very end, so a small cap class silently truncates the one line the agent needs
	// — measured on a real 39-line run under `cap: list`, which dropped the summary.
	var long strings.Builder
	long.WriteString("This is pdfTeX, Version 3.141592653 (TeX Live 2023/Debian)\nentering extended mode\n(./main.tex\n")
	for i := 0; i < 30; i++ {
		long.WriteString("Overfull \\hbox (1.0pt too wide) in paragraph at lines 1--2\n")
		long.WriteString("\\OT1/cmr/m/n/10 some hy-phen-ated words here\n")
	}
	long.WriteString("Output written on main.pdf (5 pages, 29584 bytes).\nTranscript written on main.log.\n")
	lout, _ := dsl.Apply(filt, long.String())
	if !strings.Contains(lout, "Output written on main.pdf") {
		t.Errorf("the line budget truncated the output summary off a long log:\n%s", lout)
	}
}
