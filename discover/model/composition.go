package model

// SpanComposition is a structural retrieval bucket. It describes observed
// operations and result dependencies, not a safe or useful primitive contract.
// Repetition is evidence about this trace; the key omits its observed count.
type SpanComposition struct {
	Key        string       `json:"key"`
	Actions    []string     `json:"actions"`
	Edges      []string     `json:"edges,omitempty"`
	Repetition []SpanRepeat `json:"repetition,omitempty"`
}

type SpanRepeat struct {
	Action string `json:"action"`
	Count  int    `json:"count"`
	Kind   string `json:"kind"` // for_each, repeated, or dependent_repeat
}

type SpanCompositionGroup struct {
	Key       string       `json:"key"`
	Proposals int          `json:"proposals"`
	Sessions  int          `json:"sessions"`
	Example   SpanProposal `json:"example"`
	Members   []string     `json:"members"`
}
