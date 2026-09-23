package config

import (
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/all"
)

// A WITHDRAWN ENUM VALUE MUST BE REFUSED THE SAME WAY ON BOTH PATHS, and until Field.Withdrawn existed
// it was not.
//
// `cold` and `pre_expiry_or_cold` were `cache_state` values; they were retired on evidence rather than
// renamed, so an operator carrying one wrote something that was documented and defaulted to. The
// component constructor tells them which surviving value matches what they were buying. The settings
// API validated the same key through a generic enum branch and answered `is not one of any,
// pre_expiry` — a typo message, for the one person who did not make a typo, on the one path where a
// human is watching a form.
//
// REACHABLE ONLY BY A HAND-WRITTEN POST, which is why it was missed: the form's dropdown is built from
// Options and cannot offer a withdrawn value, and normalize() turns "" into the default. That makes it
// a narrow path, not an unreachable one — a saved document from an older release, or anything driving
// the API directly, arrives here.
func TestAWithdrawnEnumValueIsRefusedWithItsReplacementNamedOnTheFormPath(t *testing.T) {
	// The precondition is the whole reason this test can regress silently: if the descriptor stops
	// declaring Withdrawn, validateValue falls through to the generic branch and the assertions below
	// would be checking a message nobody targeted.
	var decl components.Field
	for _, fd := range components.Fields("summarize") {
		if fd.Key == "trigger.cache_state" {
			decl = fd
		}
	}
	if decl.Key == "" {
		t.Fatal("summarize declares no trigger.cache_state field; the rest of this test is vacuous")
	}
	if len(decl.Withdrawn) == 0 {
		t.Fatal("trigger.cache_state declares no Withdrawn values, so the form cannot give a targeted " +
			"refusal and this test would be asserting the generic enum message")
	}

	for _, state := range []string{"cold", "pre_expiry_or_cold"} {
		t.Run(state, func(t *testing.T) {
			f := Form{Components: map[string]map[string]any{
				"summarize": {"trigger.cache_state": state},
			}}
			err := f.validate()
			if err == nil {
				t.Fatalf("the form accepted %q; it has no behaviour any more, so a document carrying "+
					"it must be refused rather than silently kept", state)
			}
			// The generic message is the failure being guarded against, not merely a worse one: it
			// tells this operator to pick from a list without saying that their value was withdrawn
			// or which replacement preserves their intent.
			if strings.Contains(err.Error(), "is not one of") {
				t.Errorf("%s got the generic enum message: %v", state, err)
			}
			if !strings.Contains(err.Error(), "withdrawn") {
				t.Errorf("%s: the error does not say the value was withdrawn: %v", state, err)
			}
			for _, want := range []string{"any", "pre_expiry"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s: the error names no %q replacement: %v", state, want, err)
				}
			}
			// And it has to say WHICH key, because a settings POST can carry many.
			if !strings.Contains(err.Error(), "trigger.cache_state") {
				t.Errorf("%s: the error does not locate the key: %v", state, err)
			}
		})
	}
}

// THE SURVIVING VALUES STILL PASS, which is what keeps the check above from being satisfied by a
// validator that simply refuses every cache_state.
func TestTheSurvivingCacheStatesPassTheFormPath(t *testing.T) {
	for _, state := range append([]string{}, components.CacheStates...) {
		t.Run(state, func(t *testing.T) {
			f := Form{Components: map[string]map[string]any{
				"summarize": {"trigger.cache_state": state},
			}}
			if err := f.validate(); err != nil {
				t.Errorf("the form refused the declared value %q: %v", state, err)
			}
		})
	}
}

// AND A REAL TYPO STILL GETS THE GENERIC MESSAGE. The withdrawal branch must not swallow the case it
// sits in front of — a misspelling is not a withdrawal and the operator needs the option list.
func TestATypoStillGetsTheEnumList(t *testing.T) {
	f := Form{Components: map[string]map[string]any{
		"summarize": {"trigger.cache_state": "pre_expiryy"},
	}}
	err := f.validate()
	if err == nil {
		t.Fatal("the form accepted a misspelled cache_state")
	}
	if !strings.Contains(err.Error(), "is not one of") {
		t.Errorf("a typo did not get the option list: %v", err)
	}
}
