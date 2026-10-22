package dash

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Grouping the folders a session was launched in into the PROJECTS Claude Code recognises.
//
// WHY THIS IS NOT DERIVED FROM THE PATHS. The dashboard records a working directory per request,
// and on a real machine that produced 40 distinct folders: five or six actual projects, their
// subdirectories, and eighteen throwaway /private/tmp directories. Nothing in the path strings
// distinguishes `Research/notes` (a project) from `Research/notes/server` (a folder in it), and
// two attempts to find a rule were measured failing:
//
//   - Nesting ("a recorded path inside another recorded path is its subfolder") collapsed every
//     project into /Users/vitalyz, because that folder had itself been used once.
//   - Shallowest-path-wins resolved EVERY project to /private/tmp, which has fewer segments than
//     any real project path.
//
// CLAUDE CODE HAS ALREADY DECIDED. It files every session under
// ~/.claude/projects/<one directory per project>/, and a project's subdirectories land in the
// same one. So the grouping is read rather than inferred.
//
// NOTHING HERE TOUCHES THE FILESYSTEM. An earlier design looked for a .git directory to find the
// project root; that was wrong twice over — it answered a question Claude Code had already
// answered better, and it made the page depend on today's disk, so a project deleted since would
// vanish along with exactly the history this surface exists to show.

// ProjectGroup maps recorded folders onto projects.
type ProjectGroup struct {
	// of maps every folder seen in a transcript to its project.
	of map[string]string
	// projects is the distinct set, for a picker.
	projects []string
}

// ProjectGroups reads the transcript directories under `home` and builds the mapping.
//
// `home` overrides the home directory, for tests. An empty result is the normal state of a
// machine with no transcripts, and of a HOSTED proxy, which cannot see the user's disk at all —
// the same limit skillDescriptions documents. Project() then returns each folder unchanged,
// which degrades to today's behaviour rather than to an empty page.
func ProjectGroups(home string) *ProjectGroup {
	g := &ProjectGroup{of: map[string]string{}}
	if home == noHome {
		return g
	}
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return g
		}
		home = h
	}
	dirs, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*"))
	seen := map[string]bool{}
	for _, dir := range dirs {
		// Per transcript DIRECTORY, which is Claude Code's own unit of "project". The directory
		// NAME is deliberately not decoded: `-Users-v-my-repo` is ambiguous, because a literal
		// '-' in a folder name encodes exactly like a separator, so decoding it invents
		// /Users/v/my/repo. The cwd recorded inside the files is unambiguous.
		files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
		turnsPer := map[string]int{}
		for _, f := range files {
			for p, n := range readCwdCounts(f) {
				turnsPer[p] += n
			}
		}
		if len(turnsPer) == 0 {
			continue
		}
		// THE DOMINANT FOLDER IS THE PROJECT, not the shallowest. A session steps out into a
		// scratch directory mid-flight — measured, one project's sessions touched 13 outside
		// folders — and /private/tmp has fewer path segments than any real project, so a
		// shallowest-wins rule renamed every project to it.
		//
		// Ties break on the path itself, so the answer does not depend on map iteration order.
		root := ""
		best := -1
		for p, n := range turnsPer {
			if n > best || (n == best && p < root) {
				root, best = p, n
			}
		}
		for p := range turnsPer {
			// First writer wins when two directories claim one folder: both resolve to the same
			// project in practice (measured: two directories both named OpenSource/context-guru),
			// and a later one overwriting an earlier would make the result order-dependent.
			if _, ok := g.of[p]; !ok {
				g.of[p] = root
			}
		}
		if !seen[root] {
			seen[root] = true
			g.projects = append(g.projects, root)
		}
	}
	sort.Strings(g.projects)
	return g
}

// Project resolves one recorded folder to its project.
//
// A FOLDER NO TRANSCRIPT MENTIONS STAYS ITSELF. It is not dropped and not attached to a project
// that never saw it: a measured row whose session has no transcript on disk still describes
// traffic, and attributing it elsewhere would be a guess presented as a measurement.
func (g *ProjectGroup) Project(folder string) string {
	if g == nil {
		return folder
	}
	if p, ok := g.of[folder]; ok {
		return p
	}
	return folder
}

// Projects is the distinct set, sorted.
func (g *ProjectGroup) Projects() []string {
	if g == nil {
		return nil
	}
	return g.projects
}

// FoldersOf is every recorded folder that rolls up to the given project, including the project
// itself.
//
// WHAT A PER-PROJECT QUERY HAS TO MATCH. Invocations are stored against the folder they happened
// in, so a query matching only the project's own path would exclude the work done in its
// subdirectories — and selecting a project would then show LESS than the unscoped view. Measured
// shape: one real project folded in 14 subfolders.
//
// A project nothing was recorded against returns just itself, so a caller always has at least
// one value to match and never builds an empty IN clause.
func (g *ProjectGroup) FoldersOf(project string) []string {
	if g == nil {
		return []string{project}
	}
	out := []string{project}
	for folder, root := range g.of {
		if root == project && folder != project {
			out = append(out, folder)
		}
	}
	sort.Strings(out)
	return out
}

// noHome is the home sentinel for a deployment that must not read the server's disk. A hosted,
// multi-tenant proxy has no business reading $HOME: the plugin names and project paths there
// belong to the server's user, not to the tenant asking.
const noHome = "\x00none"

// readCwdCounts counts, per working directory, the assistant entries of one transcript. Lighter
// than ReadTranscript: it needs only the folder, so it skips the usage decode and the dedupe.
// Only a regular file is read.
func readCwdCounts(path string) map[string]int {
	if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), transcriptMaxLine)
	out := map[string]int{}
	for sc.Scan() {
		b := sc.Bytes()
		if !bytes.Contains(b, []byte(`"type":"assistant"`)) || !bytes.Contains(b, []byte(`"cwd"`)) {
			continue
		}
		var e struct {
			Type string `json:"type"`
			Cwd  string `json:"cwd"`
		}
		if json.Unmarshal(b, &e) != nil || e.Type != "assistant" {
			continue
		}
		if p := ProjectPath(e.Cwd); p != "" {
			out[p]++
		}
	}
	return out
}

// projectGroupsTTL is how long a grouping is reused. Short enough that a new project shows up
// within a minute, long enough that scope() on every request does not reparse ~90 MB.
const projectGroupsTTL = 60 * time.Second

var pgCache struct {
	sync.Mutex
	m map[string]pgEntry
}

type pgEntry struct {
	g  *ProjectGroup
	at time.Time
}

// CachedProjectGroups is ProjectGroups behind a per-home TTL cache, shared by every call site.
func CachedProjectGroups(home string) *ProjectGroup {
	if home == noHome {
		return ProjectGroups(home)
	}
	pgCache.Lock()
	defer pgCache.Unlock()
	if e, ok := pgCache.m[home]; ok && time.Since(e.at) < projectGroupsTTL {
		return e.g
	}
	g := ProjectGroups(home)
	if pgCache.m == nil {
		pgCache.m = map[string]pgEntry{}
	}
	pgCache.m[home] = pgEntry{g, time.Now()}
	return g
}
