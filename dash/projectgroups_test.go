package dash

// Grouping recorded folders into the PROJECTS Claude Code itself recognises.
//
// THE DEFECT THIS FIXES, reported against the running dashboard: the project picker offered 40
// entries. Those were not 40 projects — they were every distinct directory Claude Code had ever
// been launched in, including 18 throwaway /private/tmp folders and subdirectories of real
// repositories (notes/server, ai-native-company/docs/adr).
//
// WHERE THE GROUPING COMES FROM, and why not from the paths. Claude Code files every session
// under ~/.claude/projects/<one directory per project>/, and puts a project's subdirectories in
// the same one. So the grouping already exists and is authoritative; deriving it again from the
// path strings cannot work, and two attempts were measured failing:
//
//   - Path nesting ("a recorded path inside another recorded path is its subfolder") collapsed
//     everything into /Users/vitalyz, because that folder had itself been used once.
//   - Shallowest-path-wins resolved EVERY project to /private/tmp, which has fewer segments than
//     any real project path.
//
// Nothing here reads the filesystem for a .git directory or anything else about a folder's
// current state. A project deleted since must keep its history — the statistics are the point.

import (
	"os"
	"testing"
)

// Subdirectories of a project resolve to the project.
func TestProjectGroupsFoldSubdirectoriesIntoTheProject(t *testing.T) {
	home := t.TempDir()
	// One transcript directory, as Claude Code writes it: the project and two subfolders.
	writeJSONL(t, home, "-Users-v-notes", "s1",
		turnLine("s1", "/Users/v/notes", daysAgo(9), 100, 10),
		turnLine("s1", "/Users/v/notes", daysAgo(9), 100, 10),
		turnLine("s1", "/Users/v/notes/server", daysAgo(8), 100, 10),
		turnLine("s1", "/Users/v/notes/ops/compose", daysAgo(7), 100, 10))

	g := ProjectGroups(home)
	for _, sub := range []string{"/Users/v/notes", "/Users/v/notes/server", "/Users/v/notes/ops/compose"} {
		if got := g.Project(sub); got != "/Users/v/notes" {
			t.Errorf("Project(%q) = %q; want /Users/v/notes — Claude Code filed them together, "+
				"so they are one project", sub, got)
		}
	}
	if len(g.Projects()) != 1 {
		t.Errorf("Projects() = %v; want exactly one", g.Projects())
	}
}

// THE PROJECT IS THE DOMINANT FOLDER, NOT THE SHALLOWEST.
//
// Measured and rejected: /private/tmp has fewer path segments than any real project, so a
// shallowest-wins rule resolved every project to /private/tmp. A session that steps out into a
// scratch directory mid-flight must not rename its project.
func TestProjectGroupsPickTheDominantFolderNotTheShallowest(t *testing.T) {
	home := t.TempDir()
	writeJSONL(t, home, "-Users-v-deep", "s1",
		turnLine("s1", "/Users/v/Projects/deep/repo", daysAgo(9), 100, 10),
		turnLine("s1", "/Users/v/Projects/deep/repo", daysAgo(9), 100, 10),
		turnLine("s1", "/Users/v/Projects/deep/repo", daysAgo(9), 100, 10),
		// One excursion into a much shallower scratch folder.
		turnLine("s1", "/private/tmp", daysAgo(8), 100, 10))

	g := ProjectGroups(home)
	if got := g.Project("/private/tmp"); got != "/Users/v/Projects/deep/repo" {
		t.Errorf("Project(/private/tmp) = %q; want the project whose session visited it. A "+
			"shallowest-path rule resolved every project to /private/tmp on real data", got)
	}
	if ps := g.Projects(); len(ps) != 1 || ps[0] != "/Users/v/Projects/deep/repo" {
		t.Errorf("Projects() = %v; want just the dominant folder", ps)
	}
}

// THE DIRECTORY NAME IS NEVER DECODED.
//
// `-Users-v-my-repo` is ambiguous: a literal '-' in a folder name encodes exactly like a
// separator, so decoding yields /Users/v/my/repo. The recorded cwd inside the file is
// unambiguous and is what must be used.
func TestProjectGroupsDoNotDecodeTheDirectoryName(t *testing.T) {
	home := t.TempDir()
	writeJSONL(t, home, "-Users-v-my-repo", "s1",
		turnLine("s1", "/Users/v/my-repo", daysAgo(9), 100, 10))

	g := ProjectGroups(home)
	ps := g.Projects()
	if len(ps) != 1 || ps[0] != "/Users/v/my-repo" {
		t.Errorf("Projects() = %v; want [/Users/v/my-repo]. Decoding the directory name turns "+
			"the hyphen into a separator and invents a folder that never existed", ps)
	}
}

// A FOLDER NOBODY RECORDED STAYS ITSELF, rather than vanishing or being attached to a project
// that never saw it. This is what keeps a measured row whose session has no transcript visible.
func TestProjectGroupsLeaveAnUnknownFolderAlone(t *testing.T) {
	home := t.TempDir()
	writeJSONL(t, home, "-Users-v-notes", "s1", turnLine("s1", "/Users/v/notes", daysAgo(9), 100, 10))

	g := ProjectGroups(home)
	if got := g.Project("/Users/v/somewhere-else"); got != "/Users/v/somewhere-else" {
		t.Errorf("Project(unknown) = %q; an unrecognised folder has to stay itself, or a row "+
			"the transcripts cannot explain disappears from the page", got)
	}
}

// TWO TRANSCRIPT DIRECTORIES CAN NAME ONE PROJECT, and they merge. Measured on a real machine:
// two directories both resolved to OpenSource/context-guru.
func TestProjectGroupsMergeTwoDirectoriesNamingOneProject(t *testing.T) {
	home := t.TempDir()
	writeJSONL(t, home, "-Users-v-proj", "s1", turnLine("s1", "/Users/v/proj", daysAgo(9), 100, 10))
	writeJSONL(t, home, "-Users-v-proj-2", "s2",
		turnLine("s2", "/Users/v/proj", daysAgo(8), 100, 10),
		turnLine("s2", "/Users/v/proj/sub", daysAgo(8), 100, 10))

	g := ProjectGroups(home)
	if ps := g.Projects(); len(ps) != 1 {
		t.Errorf("Projects() = %v; want one — both directories name the same project", ps)
	}
	if got := g.Project("/Users/v/proj/sub"); got != "/Users/v/proj" {
		t.Errorf("Project(sub) = %q; want /Users/v/proj", got)
	}
}

// NO FILESYSTEM LOOKUP. A project deleted from disk keeps its history, because the statistics
// are what the page is for.
func TestProjectGroupsDoNotTouchTheFilesystem(t *testing.T) {
	home := t.TempDir()
	// A path that does not exist and cannot be stat'd.
	writeJSONL(t, home, "-Users-v-gone", "s1",
		turnLine("s1", "/Users/v/deleted-project", daysAgo(9), 100, 10),
		turnLine("s1", "/Users/v/deleted-project/src", daysAgo(9), 100, 10))
	if _, err := os.Stat("/Users/v/deleted-project"); err == nil {
		t.Skip("the fixture path unexpectedly exists on this machine")
	}

	g := ProjectGroups(home)
	if got := g.Project("/Users/v/deleted-project/src"); got != "/Users/v/deleted-project" {
		t.Errorf("Project(src of a deleted project) = %q; want the project. A rule that reads "+
			"the disk discards exactly the history this page exists to show", got)
	}
}
