package tooldefer

import (
	"strings"

	"github.com/rossoctl/context-guru/internal/tokens"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// Env is the state one host keeps for deferral. One per process (or per tenant, as the host
// likes); every field is safe for concurrent use.
type Env struct {
	// Store is the session store. Records live there when it is durable (store.Durable).
	Store store.Store
	// Fallback holds the records when Store is not durable: tool deferral's own file (File.Store).
	// nil means there is none.
	Fallback store.Store
	History  History
	Gate     *Gate
	Prober   *Prober
}

// RecordStore is where this Env keeps its records: a durable session store, else the durable
// fallback, else nil (nothing is deferred: a record that dies with the process would move tools
// inside live cached prefixes on the next start).
func (e Env) RecordStore() store.Store {
	switch {
	case store.Durable(e.Store):
		return e.Store
	case store.Durable(e.Fallback):
		return e.Fallback
	}
	return nil
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
	// NeedProbe asks the host to start a probe for Req.Probe (no verdict, or a stale one).
	NeedProbe bool
}

// Apply defers the thread's set on one request. Fail open: on any problem Body is the input.
//
// The off switches are read on every request: the account opt-out and a 400 turn-off for this
// tenant and model. Each one sends the original request. The user's ENABLE_TOOL_SEARCH opt-out
// blocks new decisions only. The probe verdict and the history only gate a NEW decision: once a thread has
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
	case env.RecordStore() == nil:
		// No durable place for the record: nothing is deferred (see RecordStore).
		o.Why = "no_durable_store"
		return o
	case r.AccountOff:
		o.Why = "account_off"
		return o
	case !AnthropicModel(r.Model):
		o.Why = "not_anthropic"
		return o
	case env.Gate.Broken(r.Tenant, r.Model):
		o.Why = "turned_off_after_400"
		return o
	case !gjson.GetBytes(r.Orig, "tools").IsArray():
		o.Why = "no_tools"
		return o
	}
	rs := env.RecordStore()
	if l, isL := rs.(interface{ LoadSession(string) }); isL {
		l.LoadSession(r.Session) // the fallback file's rows for this session; a no-op once loaded
	}
	searchOff := SearchOff(rs, r.Session) || env.Gate.TakePending(r.Tenant, r.Session, rs)
	rec, ok := Load(rs, r.Session, r.Thread)
	if !ok && searchOff {
		// The user's tool search opt-out. It blocks a NEW decision only: a thread that already has a
		// record keeps it (below), so a flag that lands after the thread's first request, as with
		// `claude -p`, never moves a tool inside the thread's cached prefix.
		o.Why = "tool_search_false"
		Save(rs, r.Session, r.Thread, Record{})
		return o
	}
	if !ok {
		v := Pass
		if env.Prober != nil {
			v = env.Prober.Verdict(r.Probe)
			// No verdict, or one older than ProbeTTL (a Pass or a Fail): ask the host to re-probe. A
			// stale verdict is still used until the new one lands; a Fail must not last forever.
			o.NeedProbe = v == Unknown || env.Prober.Stale(r.Probe)
		}
		in := Input{Tenant: r.Tenant, Project: r.Project, Body: r.Orig, Session: r.Session, Keep: r.Keep}
		switch {
		case !HasAssistantTurn(r.Orig) || !hasSearchTurn(r.Orig):
			// The thread's first request: it writes the cache anyway, so this is when to decide.
			// A thread with assistant turns but no record and no search of ours (a fork, or a resume
			// under a new session id) is a first request too: the records are durable, so a missing
			// one means a new thread, and only our own search turn proves an earlier set.
			if v != Pass {
				o.Why = "probe_" + v.String()
				rec = Record{} // decided: nothing, for this whole thread
			} else {
				rec = Record{Deferred: Decide(env.History, in)}
			}
			o.Decided = true
		default:
			// A record lost mid-thread (evicted at the pin cap) in a thread our search has loaded
			// from: rebuild. Our search could only load a deferred tool, so the provider supported it.
			set, ok := Rebuild(env.History, in)
			if !ok {
				// The rule defers a tool the transcript does not prove: the set is unknown. Send
				// the original and save nothing, so the next request checks again.
				o.Why = "rebuild_unproven"
				if env.Gate != nil {
					env.Gate.Unproven.Add(1)
				}
				return o
			}
			rec = Record{Deferred: set}
			o.Decided, o.Rebuilt = true, true
		}
		Save(rs, r.Session, r.Thread, rec)
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
	r := ReadSearches(msg)
	return r.Searches, r.Loaded
}

// SearchRead is what one response's server-side searches did.
type SearchRead struct {
	// Searches is the calls to our search tool; Results the tool_search_tool_result blocks.
	Searches, Results int
	// Loaded is the tools the results loaded.
	Loaded []string
}

// Dropped is how many searches have no result: the API does not run a search sent in the same
// response as another tool call, and the client drops the call (#866, N8).
func (r SearchRead) Dropped() int {
	if r.Searches > r.Results {
		return r.Searches - r.Results
	}
	return 0
}

// ReadSearches is SearchesIn with the result count.
func ReadSearches(msg []byte) (r SearchRead) {
	gjson.GetBytes(msg, "content").ForEach(func(_, b gjson.Result) bool {
		r.add(b)
		return true
	})
	return r
}

// add reads one content block.
func (r *SearchRead) add(b gjson.Result) {
	switch b.Get("type").String() {
	case "server_tool_use":
		if b.Get("name").String() == SearchName {
			r.Searches++
		}
	case "tool_search_tool_result":
		r.Results++
		b.Get("content.tool_references").ForEach(func(_, t gjson.Result) bool {
			r.Loaded = append(r.Loaded, t.Get("tool_name").String())
			return true
		})
	}
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

// SearchesInSSE is SearchesIn for a raw event stream (or a window of one): it reads the
// content_block_start events of server_tool_use and tool_search_tool_result blocks.
func SearchesInSSE(raw []byte) (searches int, loaded []string) {
	r := ReadSearchesSSE(raw)
	return r.Searches, r.Loaded
}

// ReadSearchesSSE is SearchesInSSE with the result count.
func ReadSearchesSSE(raw []byte) (r SearchRead) {
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "data:") || !strings.Contains(line, "content_block_start") {
			continue
		}
		r.add(gjson.Get(strings.TrimSpace(line[5:]), "content_block"))
	}
	return r
}
