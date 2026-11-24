package keepalive

import (
	"context"
	"reflect"
	"sync"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/modes"
	"github.com/rossoctl/context-guru/store"
)

// The 1-hour touch has ONE record, the store's cg:seen: value (#1365). A keeper test that looks at a touch
// therefore needs a store, as the proxy always has one. storeOf gives each test keeper its own.
var testStores sync.Map // *Keeper -> store.Store

func storeOf(k *Keeper) store.Store {
	st, _ := testStores.LoadOrStore(k, store.NewMemory(store.Options{}))
	return st.(store.Store)
}

// realRequestClock runs the real apply for a real request at the given time, against the keeper's store, as the
// proxy does before the forward: it writes the request's own clock (and carries a standing 1-hour ping). It
// returns the alias the clock was written under and its time, which the host hands to Record as TouchAlias and TouchAtMs.
func realRequestClock(k *Keeper, key Key, body string, at time.Time) (string, int64) {
	res := apply.BodyOpts(context.Background(), components.NewPipeline(nil, nil), storeOf(k), apply.Opts{
		Provider: bschemas.Anthropic, Body: []byte(body), Tracker: modes.NewTracker(0), Now: at, Tenant: key.Tenant,
	})
	return res.TouchAlias, res.TouchAtMs
}

func reflectTypeOfThread() reflect.Type { return reflect.TypeOf(oneHourThread{}) }
