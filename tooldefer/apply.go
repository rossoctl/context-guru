package tooldefer

import (
	"github.com/rossoctl/context-guru/internal/tokens"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// Env is the state one host keeps for deferral. One per process (or per tenant, as the host
// likes); every field is safe for concurrent use.
type Env struct {
	Store   store.Store
	History History
	Gate    *Gate
	Prober  *Prober
}

// Req is one request, as the host sees it.
type Req struct {
	Tenant, Session, Thread, Project, Model string
	// Orig is the request as the client sent it: the rule and the weights read it.
	Orig []byte
	// Body is the request to rewrite (the pipeline's output). nil means Orig.
	Body []byte
	// Probe names the probe verdict that gates a NEW decision.
	Probe ProbeKey
	// Keep is the account's keep list.
	Keep []string
	// AccountOff is the account opt-out (toolfilter.defer: off).
	AccountOff bool
}

// Outcome is what Apply did.
type Outcome struct {
	Body    []byte
	Changed bool
	// Deferred is the names this request carries with defer_loading, sorted.
	Deferred []string
	// DeferredTokens is their weight in the original request, less the tools a search has already
	// loaded in this transcript. ListTokens is the names list's weight. Our tokenizer's units.
	DeferredTokens, ListTokens int
	// Why says why nothing changed, for /stats and the log: "" when Changed.
	Why string
	// Decided is true on the request that made the thread's decision; Rebuilt when the decision
	// came from Rebuild after a store miss.
	Decided, Rebuilt bool
	// NeedProbe asks the host to start a probe for Req.Probe (the verdict is unknown).
	NeedProbe bool
}

// Apply defers the thread's set on one request. Fail open: on any problem Body is the input.
//
// The off switches are read on every request: the account opt-out, the user's literal
// ENABLE_TOOL_SEARCH=false, and a 400 turn-off for this tenant and model. Each one sends the
// original request. The probe verdict and the history only gate a NEW decision: once a thread has
// a stored set, the set is applied as stored, so a restart that loses the in-memory probe cache
// cannot move a tool.
func Apply(env Env, r Req) (o Outcome) {
	if r.Body == nil {
		r.Body = r.Orig
	}
	o.Body = r.Body
	defer func() {
		if p := recover(); p != nil {
			o = Outcome{Body: r.Body, Why: "panic"}
		}
	}()
	switch {
	case r.Session == "":
		o.Why = "no_session"
		return o
	case env.Store == nil || !env.Store.Persists():
		// Storage off: no thread-fixed set, so nothing is deferred.
		o.Why = "store_off"
		return o
	case r.AccountOff:
		o.Why = "account_off"
		return o
	case !AnthropicModel(r.Model):
		o.Why = "not_anthropic"
		return o
	case env.Gate.SearchOff(r.Session):
		o.Why = "tool_search_false"
		return o
	case env.Gate.Broken(r.Tenant, r.Model):
		o.Why = "turned_off_after_400"
		return o
	case !gjson.GetBytes(r.Orig, "tools").IsArray():
		o.Why = "no_tools"
		return o
	}
	rec, ok := Load(env.Store, r.Session, r.Thread)
	if !ok {
		v := Pass
		if env.Prober != nil {
			v = env.Prober.Verdict(r.Probe)
		}
		in := Input{Tenant: r.Tenant, Project: r.Project, Body: r.Orig, Session: r.Session, Keep: r.Keep}
		switch {
		case !HasAssistantTurn(r.Orig):
			// The thread's first request: it writes the cache anyway, so this is when to decide.
			if v != Pass {
				o.NeedProbe = v == Unknown
				o.Why = "probe_" + v.String()
				rec = Record{} // decided: nothing, for this whole thread
			} else {
				rec = Record{Deferred: Decide(env.History, in)}
			}
			o.Decided = true
		case v == Pass || hasSearchTurn(r.Orig):
			// A store miss mid-thread (a restart with no disk copy): rebuild.
			rec = Record{Deferred: Rebuild(env.History, in)}
			o.Decided, o.Rebuilt = true, true
		default:
			// Mid-thread, no record and no proof the provider supports deferral: send the original.
			o.NeedProbe = v == Unknown
			o.Why = "probe_" + v.String()
			rec = Record{}
			o.Decided = true
		}
		Save(env.Store, r.Session, r.Thread, rec)
		if env.Gate != nil {
			env.Gate.Decisions.Add(1)
			if o.Rebuilt {
				env.Gate.Rebuilds.Add(1)
			}
		}
	}
	if len(rec.Deferred) == 0 {
		if o.Why == "" {
			o.Why = "nothing_to_defer"
		}
		return o
	}
	set := make(map[string]bool, len(rec.Deferred))
	for _, n := range rec.Deferred {
		set[n] = true
	}
	out, names, changed := Rewrite(r.Body, set)
	if !changed {
		o.Why = "rewrite_declined"
		return o
	}
	loaded := Loaded(r.Orig)
	weigh := map[string]bool{}
	for _, n := range names {
		if !loaded[n] {
			weigh[n] = true
		}
	}
	gjson.GetBytes(r.Orig, "tools").ForEach(func(_, t gjson.Result) bool {
		if weigh[t.Get("name").String()] {
			o.DeferredTokens += tokens.Count(t.Raw)
		}
		return true
	})
	o.Body, o.Changed, o.Deferred = out, true, names
	o.ListTokens = tokens.Count(NamesList(names))
	if env.Gate != nil {
		env.Gate.Deferred.Add(1)
	}
	return o
}

// SearchesIn reads one response message (an Anthropic message object): how many server-side
// searches ran, and which tools they loaded.
func SearchesIn(msg []byte) (searches int, loaded []string) {
	gjson.GetBytes(msg, "content").ForEach(func(_, b gjson.Result) bool {
		switch b.Get("type").String() {
		case "server_tool_use":
			if b.Get("name").String() == SearchName {
				searches++
			}
		case "tool_search_tool_result":
			b.Get("content.tool_references").ForEach(func(_, r gjson.Result) bool {
				loaded = append(loaded, r.Get("tool_name").String())
				return true
			})
		}
		return true
	})
	return searches, loaded
}

// LoadedWeight is the weight, in the original request, of the named tools.
func LoadedWeight(body []byte, names []string) int {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	n := 0
	gjson.GetBytes(body, "tools").ForEach(func(_, t gjson.Result) bool {
		if want[t.Get("name").String()] {
			n += tokens.Count(t.Raw)
		}
		return true
	})
	return n
}
