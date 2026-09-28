package model

import (
	"fmt"
	"path/filepath"
)

// Workflow is the typed projection of workflow.yaml (09-workflow-spec.md).
type Workflow struct {
	Path       string
	Dir        string
	Raw        map[string]interface{}
	APIVersion string
	Kind       string

	Inputs     map[string]ParamSpec
	InputOrder []string

	Steps []Step

	Outputs     map[string]interface{}
	OutputOrder []string
}

// ParamSpec mirrors the catalog ParamSpec fields verbatim (09 §1.2 / §3).
type ParamSpec struct {
	Type        string
	Format      string
	Required    bool
	Default     interface{}
	HasDefault  bool
	Description string
	Examples    []interface{}
	Min         interface{}
	Max         interface{}
	Enum        []interface{}
	Aliases     []string
	Raw         map[string]interface{}
}

// Step is one workflow.yaml step; exactly one of the type-specific pointers
// below should be non-nil (validated separately).
type Step struct {
	ID        string
	API       *APIStep
	Browser   *BrowserStep
	Code      *CodeStep
	Primitive *PrimitiveStep
	Transform *TransformStep
	Reason    *ReasonStep
	Branch    *BranchStep
	Approval  map[string]interface{}
	When      string
	HasWhen   bool
	Retry     *RetrySpec
	After     []string
	// Single is the `single: true` step modifier (CHANGELOG.md v1 ruling 9,
	// "ratified as-implemented"): sugar for "this step selects one item and
	// must error rather than silently proceed on an empty result." Accepted
	// by validate (closed-key allowlist + parsed here); compiler enforcement
	// of the actual error-on-empty behavior is a B2 deliverable.
	Single bool
	Raw    map[string]interface{}
}

// TypeName returns which step-type key was present, for diagnostics.
func (s Step) TypeName() string {
	switch {
	case s.API != nil:
		return "api"
	case s.Browser != nil:
		return "browser"
	case s.Code != nil:
		return "code"
	case s.Primitive != nil:
		return "primitive"
	case s.Transform != nil:
		return "transform"
	case s.Reason != nil:
		return "reason"
	case s.Branch != nil:
		return "branch"
	case s.Approval != nil:
		return "approval"
	}
	return "unknown"
}

type APIStep struct {
	Integration string
	Tool        string
	Credential  string
	Params      map[string]interface{}
	Paginate    string
	Raw         map[string]interface{}
}

type OriginRef struct {
	Slot    string
	Literal string
}

type BrowserStep struct {
	Action   string
	Origin   OriginRef
	URL      interface{}
	PlanFrom string
	OnDrift  string
	Raw      map[string]interface{}
}

type CodeStep struct{ Raw map[string]interface{} }
type PrimitiveStep struct {
	Name string
	Raw  map[string]interface{}
}

type TransformStep struct {
	Language       string
	Expression     string
	ExpressionFrom string
	Params         map[string]interface{}
	Raw            map[string]interface{}
}

type ReasonStep struct {
	Purpose      string
	Input        interface{}
	InputSchema  map[string]interface{}
	OutputSchema map[string]interface{}
	MaxTokens    int
	Capabilities []string
	DataClasses  []string
	Raw          map[string]interface{}
}

type BranchCase struct {
	When    string
	Default bool
	Raw     map[string]interface{}
}

type BranchStep struct {
	Cases []BranchCase
	Raw   map[string]interface{}
}

type RetrySpec struct {
	MaxAttempts           int
	InitialBackoffSeconds int
	RetryOn               []string
	RetryOnKeyPresent     bool
	Raw                   map[string]interface{}
}

// LoadWorkflow parses workflow.yaml at path.
func LoadWorkflow(path string) (*Workflow, error) {
	raw, _, err := LoadYAMLFile(path)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, fmt.Errorf("%s: empty document", path)
	}
	w := &Workflow{
		Path: path,
		Dir:  filepath.Dir(path),
		Raw:  raw,
	}
	w.APIVersion = StringVal(raw, "apiVersion")
	w.Kind = StringVal(raw, "kind")

	if inputs, ok := AsMap(raw["inputs"]); ok {
		w.Inputs = map[string]ParamSpec{}
		for name, v := range inputs {
			pm, ok := AsMap(v)
			if !ok {
				continue
			}
			ps := ParamSpec{
				Type:        stringifyType(pm["type"]),
				Format:      StringVal(pm, "format"),
				Required:    BoolVal(pm, "required", false),
				Description: StringVal(pm, "description"),
				Aliases:     StringSliceVal(pm, "aliases"),
				Raw:         pm,
			}
			if d, ok := pm["default"]; ok {
				ps.Default = d
				ps.HasDefault = true
			}
			if ex, ok := AsSlice(pm["examples"]); ok {
				ps.Examples = ex
			}
			if mn, ok := pm["min"]; ok {
				ps.Min = mn
			}
			if mx, ok := pm["max"]; ok {
				ps.Max = mx
			}
			if en, ok := AsSlice(pm["enum"]); ok {
				ps.Enum = en
			}
			w.Inputs[name] = ps
			w.InputOrder = append(w.InputOrder, name)
		}
	}

	if steps, ok := AsSlice(raw["steps"]); ok {
		for _, sv := range steps {
			sm, ok := AsMap(sv)
			if !ok {
				continue
			}
			step := Step{
				ID:  StringVal(sm, "id"),
				Raw: sm,
			}
			if v, ok := sm["when"]; ok {
				if s, ok := AsString(v); ok {
					step.When = s
					step.HasWhen = true
				}
			}
			step.After = StringSliceVal(sm, "after")
			step.Single = BoolVal(sm, "single", false)
			if rm, ok := AsMap(sm["retry"]); ok {
				rs := &RetrySpec{
					MaxAttempts:           IntVal(rm, "max_attempts", 0),
					InitialBackoffSeconds: IntVal(rm, "initial_backoff_seconds", 0),
					Raw:                   rm,
				}
				if _, present := rm["retry_on"]; present {
					rs.RetryOnKeyPresent = true
					rs.RetryOn = StringSliceVal(rm, "retry_on")
				}
				step.Retry = rs
			}

			if am, ok := AsMap(sm["api"]); ok {
				step.API = &APIStep{
					Integration: StringVal(am, "integration"),
					Tool:        StringVal(am, "tool"),
					Credential:  StringVal(am, "credential"),
					Paginate:    StringVal(am, "paginate"),
					Raw:         am,
				}
				if p, ok := AsMap(am["params"]); ok {
					step.API.Params = p
				}
			}
			if bm, ok := AsMap(sm["browser"]); ok {
				bs := &BrowserStep{
					Action:   StringVal(bm, "action"),
					PlanFrom: StringVal(bm, "plan_from"),
					OnDrift:  StringVal(bm, "on_drift"),
					Raw:      bm,
				}
				if om, ok := AsMap(bm["origin"]); ok {
					bs.Origin = OriginRef{Slot: StringVal(om, "slot")}
				} else if os, ok := AsString(bm["origin"]); ok {
					bs.Origin = OriginRef{Literal: os}
				}
				bs.URL = bm["url"]
				step.Browser = bs
			}
			if cm, ok := AsMap(sm["code"]); ok {
				step.Code = &CodeStep{Raw: cm}
			}
			if pm, ok := AsMap(sm["primitive"]); ok {
				step.Primitive = &PrimitiveStep{Name: StringVal(pm, "name"), Raw: pm}
			}
			if tm, ok := AsMap(sm["transform"]); ok {
				ts := &TransformStep{
					Language:       StringVal(tm, "language"),
					Expression:     StringVal(tm, "expression"),
					ExpressionFrom: StringVal(tm, "expression_from"),
					Raw:            tm,
				}
				if p, ok := AsMap(tm["params"]); ok {
					ts.Params = p
				}
				step.Transform = ts
			}
			if rm, ok := AsMap(sm["reason"]); ok {
				rs := &ReasonStep{
					Purpose:      StringVal(rm, "purpose"),
					MaxTokens:    IntVal(rm, "maxTokens", 0),
					Capabilities: StringSliceVal(rm, "capabilities"),
					DataClasses:  StringSliceVal(rm, "dataClasses"),
					Raw:          rm,
				}
				rs.Input = rm["input"]
				if is, ok := AsMap(rm["inputSchema"]); ok {
					rs.InputSchema = is
				}
				if os, ok := AsMap(rm["outputSchema"]); ok {
					rs.OutputSchema = os
				}
				step.Reason = rs
			}
			if bm, ok := AsMap(sm["branch"]); ok {
				bs := &BranchStep{Raw: bm}
				if cases, ok := AsSlice(bm["cases"]); ok {
					for _, cv := range cases {
						cm, ok := AsMap(cv)
						if !ok {
							continue
						}
						bs.Cases = append(bs.Cases, BranchCase{
							When:    StringVal(cm, "when"),
							Default: BoolVal(cm, "default", false),
							Raw:     cm,
						})
					}
				}
				step.Branch = bs
			}
			if am, ok := AsMap(sm["approval"]); ok {
				step.Approval = am
			}

			w.Steps = append(w.Steps, step)
		}
	}

	if outputs, ok := AsMap(raw["outputs"]); ok {
		w.Outputs = outputs
		for name := range outputs {
			w.OutputOrder = append(w.OutputOrder, name)
		}
	}

	return w, nil
}

// StepByID looks up a step by id.
func (w *Workflow) StepByID(id string) (Step, bool) {
	for _, s := range w.Steps {
		if s.ID == id {
			return s, true
		}
	}
	return Step{}, false
}

func stringifyType(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}
