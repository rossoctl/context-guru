package dash

// Static checks over the Overview refresh timer's pacing.
//
// These are source-level assertions for the same reason the keep-alive and inventory ones are:
// the bug they guard is invisible on screen. A dashboard whose retry pacing has broken looks
// exactly like a dashboard that is working — the reader sees an error or a stale number either
// way — and the damage lands on the SERVER, as a request rate nobody watching the page can see.
//
// The outage that earned them: the timer ticks every 5s and gated on `now - state.loadedAt`,
// which advances only when a load SUCCEEDS. So the first failing load left `age` growing without
// bound, both backoff guards permanently satisfied, and every open tab re-firing two expensive
// queries every 5 seconds instead of every 5 minutes — for hours. Because reads were also
// uncancellable server-side, each of those abandoned requests kept a pooled connection and ran
// its query to completion, which took the process to its memory cap and made every subsequent
// query slower, producing more failures. One line of client pacing sat at the head of that loop.

import (
	"regexp"
	"strings"
	"testing"
)

// The timer must pace on the last ATTEMPT and must not stack overlapping loads.
func TestOverviewRefreshPacesOnAttemptsNotSuccesses(t *testing.T) {
	src := readUI(t, "ui/app.js")

	// The tick body, from the interval callback to its 5000ms period. Scoping the assertions to
	// it matters: `loadedAt` legitimately appears elsewhere (paintFreshness reports it to the
	// reader), so a whole-file search would pass on the wrong occurrence.
	tick := regexp.MustCompile(`(?s)setInterval\(\(\) => \{\s*const every = refreshMs\(\);.*?\}, 5000\);`).
		FindString(src)
	if tick == "" {
		t.Fatal("could not find the Overview refresh tick (setInterval ... refreshMs ... 5000);\n" +
			"if the timer was restructured, this check needs rewriting against whatever replaced it")
	}

	if !strings.Contains(tick, "state.loadingOverview") {
		t.Error("the refresh tick does not check state.loadingOverview.\n" +
			"Without it a load slower than the 5s tick has another started on top of it every tick. " +
			"A failing load is also a slow one, so the failure case stacks hardest, and an " +
			"abandoned request still costs the server a connection and a whole query.")
	}
	if !strings.Contains(tick, "state.lastTryAt") {
		t.Error("the refresh tick does not consult state.lastTryAt.\n" +
			"Pacing on state.loadedAt alone is the outage: it advances only on success, so a " +
			"failing dashboard polls every 5s forever instead of backing off to the reader's interval.")
	}
	// The guard has to be the MAXIMUM of the two. Pacing on lastTryAt alone would be correct for
	// backoff but would stop honouring a successful load's freshness; taking loadedAt alone is
	// the original bug.
	if !regexp.MustCompile(`Math\.max\(state\.loadedAt, state\.lastTryAt\)`).MatchString(tick) {
		t.Error("the tick's age is not Math.max(state.loadedAt, state.lastTryAt).\n" +
			"Both matter: loadedAt is freshness of what is on screen, lastTryAt is when we last " +
			"tried. Dropping either one reintroduces a poll rate no reader asked for.")
	}
}

// lastTryAt must be advanced on EVERY exit from loadOverview, which in practice means `finally`.
//
// Asserted separately from the tick because this is the half that is easy to get subtly wrong:
// setting it at the end of the try block, or in the catch, leaves the abort path (`if
// (aborted(err)) return;`) and any future early return recording no attempt — and one uncounted
// exit is enough to restore the 5s hammer.
func TestLoadOverviewRecordsEveryAttempt(t *testing.T) {
	src := readUI(t, "ui/app.js")
	body := regexp.MustCompile(`(?s)async function loadOverview\(opts = \{\}\) \{.*?\n\}\n`).FindString(src)
	if body == "" {
		t.Fatal("could not find loadOverview(); this check needs rewriting against whatever replaced it")
	}
	fin := strings.LastIndex(body, "} finally {")
	if fin < 0 {
		t.Fatal("loadOverview has no `finally` block.\n" +
			"state.lastTryAt and state.loadingOverview must be settled on every exit, including the " +
			"abort early-return inside the catch. Only `finally` covers all of them.")
	}
	tail := body[fin:]
	for _, want := range []string{"state.loadingOverview = false", "state.lastTryAt = Date.now()"} {
		if !strings.Contains(tail, want) {
			t.Errorf("loadOverview's `finally` does not contain %q.\n"+
				"Recording the attempt anywhere else leaves an exit path that tells the timer no "+
				"attempt was made, and the 5s retry storm comes back.", want)
		}
	}
	if !strings.Contains(body, "state.loadingOverview = true") {
		t.Error("loadOverview never sets state.loadingOverview = true, so the tick's " +
			"anti-stacking guard can never fire.")
	}
}
