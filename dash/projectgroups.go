package dash

import (
	"os"
	"path/filepath"
	"sort"
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
			turns, err := ReadTranscript(f)
			if err != nil {
				continue
			}
			for _, tn := range turns {
				if tn.Project != "" {
					turnsPer[tn.Project]++
				}
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
