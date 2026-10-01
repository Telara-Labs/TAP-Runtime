package model

import (
	"errors"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// DraftStep is one step as the review shows it.
type DraftStep struct {
	N      int    `json:"n"`
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Line   string `json:"line"`
	Effect string `json:"effect,omitempty"`
	Note   string `json:"note,omitempty"`
}

// DraftInput is one argument of the drafted primitive: $Position in main.sh.
type DraftInput struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// Raw inputs are passed as JSON (a number, a boolean), not as a string.
	Raw bool `json:"raw,omitempty"`
	// Example is one recorded value, redacted, for the person reviewing on
	// this machine. It is never written into a generated file.
	Example string `json:"-"`
	// Sensitive inputs are credentials: the caller supplies them from its
	// own configuration and no recorded value is kept.
	Sensitive bool `json:"sensitive,omitempty"`
	// DerivedFrom is the step (1-based) whose output held this value in most
	// runs: the program must take it from there, not from the caller.
	DerivedFrom int `json:"derived_from,omitempty"`
	// Extract, when set, is how the program takes this value from step
	// DerivedFrom's output: a grep pattern for the text before it and the
	// value itself. The caller does not supply it and it has no Position.
	Extract string `json:"extract,omitempty"`
	// Binding says how Extract reads the value: "json_path" (a jq path into
	// a JSON result) or "text_anchor" (the text before it).
	Binding string `json:"binding,omitempty"`
	// List marks a caller-given list the program loops over (a JSON array).
	List bool `json:"list,omitempty"`
	// Position is the argument number the caller passes it as ($1, $2...);
	// 0 for an extracted value.
	Position int    `json:"position"`
	From     string `json:"from"`
	// strip is what Extract's match starts with, removed to leave the value.
	Strip string `json:"-"`
	// pos is the template position of the step that takes it.
	Pos int `json:"-"`
}

// Draft is a drafted package and what the review needs to show it.
type Draft struct {
	Name      string            `json:"name"`
	Publisher string            `json:"publisher"`
	Inputs    []DraftInput      `json:"inputs"`
	Steps     []DraftStep       `json:"steps"`
	Files     map[string][]byte `json:"-"`
	// Blocked lists every place a generated file still looks like it holds a
	// credential. While it is non-empty the draft cannot be saved, packaged
	// or published.
	Blocked []string `json:"blocked,omitempty"`
	// Problems is what manifest.PublishProblems reports; empty means the
	// package passes the same checks the registry runs before accepting it.
	Problems   []string `json:"problems"`
	HumanSteps int      `json:"human_steps"`
	// Derived counts inputs an earlier step's output supplied in the
	// recorded runs that the draft could not extract itself: they need
	// authoring. Extracted counts those it takes from that output.
	Derived   int `json:"derived"`
	Extracted int `json:"extracted"`
	// FixedSteps counts steps that pin something: a subcommand, an MCP tool,
	// a constant argument or browser object. None means every command and
	// argument varied: exploration, not a procedure.
	FixedSteps int `json:"fixed_steps"`
	// FixedShare is how much of the routine is fixed: each replayed step's
	// tool or command plus every argument that never changed, over that plus
	// the inputs. A procedure is mostly fixed; an investigation, where the
	// agent chose most values as it went, is mostly inputs.
	FixedShare float64          `json:"fixed_share"`
	Values     []map[int]string `json:"-"`
	// listLoop names the steps drafted as a loop over a caller list.
	ListLoop map[string]bool `json:"-"`
	// humanPos are template positions drafted as human steps.
	HumanPos map[int]bool `json:"-"`
	// priorLoop names steps that loop over a list an earlier result held.
	PriorLoop map[string]bool `json:"-"`
	// firstRun is the first drafted run's steps, for evidence lookups.
	FirstRun []trace.Step `json:"-"`
	PosStep  map[int]int  `json:"-"`
	// RuntimeUnsupported names steps the TAP guest runtime would not run
	// as recorded (a built-in that ignores file arguments, a file read the
	// manifest cannot declare). They block a structurally complete draft.
	RuntimeUnsupported []string `json:"runtime_unsupported,omitempty"`
}

// DraftOptions are the review's answers.
type DraftOptions struct {
	Publisher string
	// ReadOnly lists step numbers (1-based) the user confirmed change nothing.
	ReadOnly map[int]bool
	// loops are steps each run made once per item of a list the request
	// gave: the varying argument and, per occurrence, its values in order.
	Loops map[string]LoopSpec `json:"-"`
}

type LoopSpec struct {
	Key    string     `json:"-"`
	Values [][]string `json:"-"` // per occurrence (index into occ)
	// source is where the list came from: "caller" (the request gave it)
	// or "prior_output" (an earlier step's result held every item).
	Source string `json:"-"`
}

// ErrBlocked is returned when a draft still holds something credential-shaped.
var ErrBlocked = errors.New("the draft still contains credential-shaped values; it cannot be saved or published until they are removed")
