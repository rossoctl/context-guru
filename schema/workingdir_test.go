package schema

import (
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

// WorkingDirOf reads the agent's launch directory out of a request, which is what lets the
// dashboard report cost per PROJECT instead of blending every folder into one total.
//
// Every case below is a shape measured in real traffic (589 marker occurrences across local
// transcripts), not an invented one. Two of them are the reason this function is not a one-line
// strings.TrimPrefix:
//
//   - The marker frequently carries a ` (was /old/path)` suffix, written when the working
//     directory changes mid-session. Keeping it would make one project fragment into many, since
//     "/a (was /b)" and "/a" would be different keys.
//   - 47 of the 589 matches are not paths at all. The prefix appears inside prose and inside a
//     table of instructions, so a parser that trusts the marker records garbage as a project.
func TestWorkingDirOfReadsTheLaunchDirectoryAndRefusesEverythingElse(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{
			name: "a bare path, the common case",
			text: "# Environment\nPrimary working directory: /Users/v/OpenSource/context-guru\nIs a git repository: true",
			want: "/Users/v/OpenSource/context-guru",
		},
		{
			// THE ONE THAT FRAGMENTS A PROJECT. Measured 57 times for one folder alone.
			name: "the (was ...) suffix of a mid-session cd is stripped",
			text: "Primary working directory: /Users/v/OpenSource/context-guru (was /private/tmp)",
			want: "/Users/v/OpenSource/context-guru",
		},
		{
			name: "a trailing slash is normalised away, so one folder is one key",
			text: "Primary working directory: /Users/v/proj/",
			want: "/Users/v/proj",
		},
		{
			// Real text from an instructions table that happens to contain the prefix.
			name: "prose that is not a path is refused",
			text: "Primary working directory:   no  Contents of /                YES  # Environment",
			want: "",
		},
		{
			name: "a regex fragment captured from a transcript is refused",
			text: "Primary working directory: [^",
			want: "",
		},
		{
			// THE CASE THE ABSOLUTE-PATH RULE ALONE DOES NOT CATCH, and the reason the whitespace
			// guard exists. Measured in real traffic: the marker appears inside a table of
			// instructions whose next column begins with a slash, so the captured value both
			// starts with "/" and is plainly prose. Without the interior-whitespace check this is
			// recorded as a project folder and shows up in the picker as one.
			name: "prose beginning with a slash is refused",
			text: "Primary working directory: /                YES  # Environment                no",
			want: "",
		},
		{
			name: "a sentence that happens to start with a path is refused",
			text: "Primary working directory: /Users/v/proj in messages[1] of 8/9 requests",
			want: "",
		},
		{
			name: "a placeholder is refused",
			text: "Primary working directory: <path>",
			want: "",
		},
		{
			name: "a relative path is refused: only an absolute launch directory identifies a project",
			text: "Primary working directory: OpenSource/context-guru",
			want: "",
		},
		{
			name: "no marker at all",
			text: "You are a helpful assistant.",
			want: "",
		},
		{
			name: "the marker with nothing after it",
			text: "Primary working directory: ",
			want: "",
		},
		{
			// The root directory is a legitimate launch point and must not be mistaken for junk.
			name: "the filesystem root",
			text: "Primary working directory: /",
			want: "/",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs := []bschemas.ChatMessage{
				{Role: bschemas.ChatMessageRoleSystem,
					Content: &bschemas.ChatMessageContent{ContentStr: strp("you are an agent")}},
				{Role: bschemas.ChatMessageRoleUser,
					Content: &bschemas.ChatMessageContent{ContentStr: strp(tc.text)}},
			}
			if got := WorkingDirOf(msgs); got != tc.want {
				t.Errorf("WorkingDirOf() = %q; want %q", got, tc.want)
			}
		})
	}
}

// The marker is in messages[1] in real traffic, but nothing guarantees that: a host may inject
// another message first, and the environment block is sometimes in the system prompt. Scanning
// every message costs one pass over a transcript the caller has already normalized, and the
// alternative is a dimension that silently stops being recorded when a client reorders its
// preamble.
func TestWorkingDirOfDoesNotDependOnWhichMessageCarriesTheMarker(t *testing.T) {
	const want = "/Users/v/Research/notes"
	for _, where := range []int{0, 1, 2} {
		msgs := make([]bschemas.ChatMessage, 3)
		for i := range msgs {
			body := "filler"
			if i == where {
				body = "Primary working directory: " + want
			}
			role := bschemas.ChatMessageRoleUser
			if i == 0 {
				role = bschemas.ChatMessageRoleSystem
			}
			msgs[i] = bschemas.ChatMessage{Role: role,
				Content: &bschemas.ChatMessageContent{ContentStr: strp(body)}}
		}
		if got := WorkingDirOf(msgs); got != want {
			t.Errorf("marker in message %d: got %q, want %q", where, got, want)
		}
	}
}

// The FIRST marker wins. A transcript accumulates: this session's own history contains earlier
// turns quoting other directories, and one of them is this very test file. Taking the last match
// would attribute a request to whatever folder was most recently mentioned in conversation.
func TestWorkingDirOfTakesTheFirstMarkerNotTheLast(t *testing.T) {
	msgs := []bschemas.ChatMessage{
		{Role: bschemas.ChatMessageRoleSystem,
			Content: &bschemas.ChatMessageContent{ContentStr: strp(
				"Primary working directory: /Users/v/real-project")}},
		{Role: bschemas.ChatMessageRoleUser,
			Content: &bschemas.ChatMessageContent{ContentStr: strp(
				"earlier I said Primary working directory: /Users/v/something-else")}},
	}
	if got := WorkingDirOf(msgs); got != "/Users/v/real-project" {
		t.Errorf("got %q; want the first marker /Users/v/real-project", got)
	}
}

// A path is CLIENT-CHOSEN free text of unbounded length. Without a cap it reaches the database and
// the SSE feed as-is, so the length limit is a storage property rather than a cosmetic one.
func TestWorkingDirOfRefusesAnAbsurdlyLongPath(t *testing.T) {
	long := "/" + strings.Repeat("a", 5000)
	msgs := []bschemas.ChatMessage{{Role: bschemas.ChatMessageRoleUser,
		Content: &bschemas.ChatMessageContent{ContentStr: strp(
			"Primary working directory: " + long)}}}
	if got := WorkingDirOf(msgs); got != "" {
		t.Errorf("got a %d-character path; want it refused", len(got))
	}
}
