package counterfactual

import "time"

// RequestOps is the operations of one request, or of one call made outside a request.
type RequestOps struct {
	Version   int               `json:"version"`
	RequestID string            `json:"requestId"`
	Session   string            `json:"session"`
	Thread    Thread            `json:"thread"`
	Model     string            `json:"model,omitempty"`  // the model the request was sent for
	Params    map[string]string `json:"params,omitempty"` // other settings that change the cost: effort, thinking, max_output, ...
	At        time.Time         `json:"at"`
	// Billing prices every operation of the request, a spend included (R41, R42).
	Billing Billing `json:"billing"`
	// Ops is in-process only: input to Compute for this request, never recorded (R20).
	Ops []Operation `json:"ops"` // in pipeline order
}

// RequestMeta is what the host knows about the request before the pass: the fields of RequestOps
// that are not operations.
type RequestMeta struct {
	RequestID string            `json:"requestId"`
	Session   string            `json:"session"`
	Model     string            `json:"model,omitempty"`
	Params    map[string]string `json:"params,omitempty"`
	At        time.Time         `json:"at"`
	Billing   Billing           `json:"billing"`
}

// Billing is a fact the host reads from the request's credential, not a price.
type Billing struct {
	Mode BillingMode `json:"mode"`
}

// ModelCall is one upstream call an operator or the keeper made. The host's model client fills it
// from the call's own response, so the usage is what the provider billed.
type ModelCall struct {
	Host     string            `json:"host,omitempty"`
	Model    string            `json:"model"`
	Params   map[string]string `json:"params,omitempty"`   // the call's own settings: effort, thinking, ...
	Usage    CallUsage         `json:"usage"`              // everything the call's response bills, as billed
	Detached bool              `json:"detached,omitempty"` // finished after the request it served was answered
}

// CallUsage is the whole bill of one call, as its response states it. CostUSD is a fact the
// provider or gateway reported, not a rate of ours (R1, R49).
type CallUsage struct {
	Usage           // input, cache read, cache write (and its 1h part)
	Output  int     `json:"output,omitempty"`  // output tokens, including reasoning or thinking tokens
	CostUSD float64 `json:"costUSD,omitempty"` // the cost the response stated; 0 when it stated none
}

// Usage is the provider's billed prompt usage. CacheWrite1h is the part of CacheWrite billed at
// the one-hour rate, never an addition to it.
type Usage struct {
	Input        int `json:"input"`
	CacheRead    int `json:"cacheRead"`
	CacheWrite   int `json:"cacheWrite"`
	CacheWrite1h int `json:"cacheWrite1h"`
}

// Thread identifies one conversation inside a session: the main thread, a subagent, or a fork.
// Each thread has its own provider cache entry.
type Thread struct {
	ID     string `json:"id"`               // "main" for the primary thread
	Parent string `json:"parent,omitempty"` // the thread this one was forked from; "" if none
}

// Operation is one change unit a context operator reports.
type Operation struct {
	ID       string `json:"id,omitempty"` // unique inside the request only; across requests, use ChangeID (R6)
	Kind     Kind   `json:"kind"`
	Operator string `json:"operator,omitempty"` // "context-guru.mask", "tool-prune"
	DryRun   bool   `json:"dryRun,omitempty"`   // set by the framework: computed but not sent (R12)

	At         Position `json:"at"`              // first position touched, in this operator's output
	Spans      []Span   `json:"spans,omitempty"` // every touched range, when the change is not contiguous; nil otherwise
	SizeBefore Size     `json:"sizeBefore"`      // what the operator received over the touched ranges; the sum over Spans
	SizeAfter  Size     `json:"sizeAfter"`       // what it sent on over the same ranges

	ChangeID string `json:"changeId,omitempty"` // the same on every request that carries this change

	// kind = spend
	Call *ModelCall `json:"call,omitempty"`

	// kind = setting: the key is the setting name (model, host, or a key of Params).
	Settings map[string]SettingChange `json:"settings,omitempty"`

	// a change to the message list, for mapping positions
	MessageSplice *MessageSplice `json:"messageSplice,omitempty"`
}

// Position is a place in the prompt. The provider hashes tools, then system, then messages.
type Position struct {
	Section Section `json:"section"`
	Index   int     `json:"index"` // block or message index in that section
}

// Span is a range of positions in one section: [From, To], both in the operator's output.
type Span struct {
	Section    Section `json:"section"`
	From       int     `json:"from"`
	To         int     `json:"to"`
	SizeBefore Size    `json:"sizeBefore"`
	SizeAfter  Size    `json:"sizeAfter"`
}

// SettingChange is one setting going from one value to another. A nil pointer means "not set".
//
// At is the first position of the prefix that the provider renders this setting into, so the first
// position whose cache entry the change can invalidate (cached-prefix.md). A model change starts at
// tools[0]; an effort change on a model that renders it into the messages starts at messages[0].
// The body does not differ where a setting differs, so the library cannot find this position by
// comparing hashes (§5.2). Nil means the reporter does not know: the library then falls back to the
// read the usage shows.
type SettingChange struct {
	From *string   `json:"from,omitempty"`
	To   *string   `json:"to,omitempty"`
	At   *Position `json:"at,omitempty"`
}

// Size is a token count and the tokenizer that produced it.
type Size struct {
	Bytes     int    `json:"bytes,omitempty"`     // required on rewrite; the sum check compares it with the Body sizes (§6.5)
	Tokens    int    `json:"tokens"`              // required on rewrite (rule 7)
	Tokenizer string `json:"tokenizer,omitempty"` // "bytes-estimate" when estimated
}

// MessageSplice describes a change to the message list.
type MessageSplice struct {
	At       int `json:"at"`
	Removed  int `json:"removed"`
	Inserted int `json:"inserted"`
}

// CacheState is the state of the thread's provider cache entry.
type CacheState struct {
	CachePhase CachePhase `json:"phase,omitempty"`
	Extent     *Position  `json:"extent,omitempty"` // the last position the entry covers; nil when nothing is covered
	TTL        string     `json:"ttl,omitempty"`    // "5m" | "1h"; empty when cold or unknown
}

// EntryFacts are the facts that decide the cache phase and that only the host knows. The host
// passes them in EndRequest. The library finds the two gaps itself (§5.2), from
// ThreadState.PrevPrint.At, LastPing and the time of the request.
type EntryFacts struct {
	TTL time.Duration `json:"ttl"` // lifetime of the entry: 5m or 1h, from the breakpoint that wrote it
	// Lifetime says whether TTL is a proven expiry (exact) or a guaranteed floor (minimum) (R45).
	Lifetime LifetimeKind `json:"lifetime"`
	// The last keep-alive ping of this thread since its previous request: when it started, the
	// extent it read, and the ChangeID of its keep-alive window. Zero values when no ping was sent.
	LastPing   time.Time `json:"lastPing,omitzero"`
	PingRead   *Position `json:"pingRead,omitempty"`
	PingWindow string    `json:"pingWindow,omitempty"`
}

// The enums are strings, not integers, so an unknown value from a newer operator survives a
// decode and can be reported as unpriced (R19).
type (
	Kind         string // rewrite | spend | setting
	CachePhase   string // hot | cold | unknown
	LifetimeKind string // exact | minimum | unknown
	BillingMode  string // api | subscription | unknown
	Section      string // tools | system | messages
)

const (
	KindRewrite Kind = "rewrite"
	KindSpend   Kind = "spend"
	KindSetting Kind = "setting"

	PhaseHot     CachePhase = "hot"
	PhaseCold    CachePhase = "cold"
	PhaseUnknown CachePhase = "unknown"

	LifetimeExact   LifetimeKind = "exact"
	LifetimeMinimum LifetimeKind = "minimum"
	LifetimeUnknown LifetimeKind = "unknown"

	BillingAPI          BillingMode = "api"
	BillingSubscription BillingMode = "subscription"
	BillingUnknown      BillingMode = "unknown"

	SectionTools    Section = "tools"
	SectionSystem   Section = "system"
	SectionMessages Section = "messages"
)
