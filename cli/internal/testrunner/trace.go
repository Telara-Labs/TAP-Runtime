package testrunner

import (
	"fmt"
	"path/filepath"

	"telara.dev/tap/internal/model"
)

// TraceStep is one step's record in a `tap dev` fixture-replay trace.
type TraceStep struct {
	StepID       string                 `json:"step_id"`
	Type         string                 `json:"type"`
	Skipped      bool                   `json:"skipped"`
	Bindings     map[string]interface{} `json:"bindings,omitempty"`
	Requirements []string               `json:"requirements,omitempty"`
	Output       interface{}            `json:"output,omitempty"`
	Error        string                 `json:"error,omitempty"`
}

type TraceReport struct {
	Dir      string                 `json:"dir"`
	CaseName string                 `json:"case_name"`
	Input    map[string]interface{} `json:"input"`
	Steps    []TraceStep            `json:"steps"`
	Output   map[string]interface{} `json:"output"`
}

// Trace runs one contract-test case (by name, or the first case if name is
// "") and records a step-by-step trace: resolved bindings and which
// manifest requirements each step exercised. Used by `tap dev`.
func Trace(pkg *model.Package, caseName string, binds map[string]string) (*TraceReport, error) {
	if pkg.Workflow == nil {
		return nil, fmt.Errorf("workflow.yaml did not load: %v", pkg.WorkflowErr)
	}
	contractPath := filepath.Join(pkg.Dir, "tests", "contract.test.yaml")
	cf, err := LoadContractFile(contractPath)
	if err != nil {
		return nil, err
	}
	binds = MergeBindings(cf.Bindings, binds)

	var tc *TestCase
	for i := range cf.Cases {
		if caseName == "" || cf.Cases[i].Name == caseName {
			tc = &cf.Cases[i]
			break
		}
	}
	if tc == nil {
		return nil, fmt.Errorf("no case named %q in %s", caseName, contractPath)
	}
	testsDir := filepath.Dir(contractPath)

	rep := &TraceReport{Dir: pkg.Dir, CaseName: tc.Name}

	var fixtures map[string]interface{}
	if tc.Fixtures != "" {
		f, err := LoadFixtures(filepath.Join(testsDir, tc.Fixtures))
		if err != nil {
			return nil, err
		}
		fixtures = f
	} else {
		fixtures = map[string]interface{}{}
	}

	var input map[string]interface{}
	if tc.HasInput {
		input = tc.Input
	} else {
		input = deriveDefaultInput(pkg, binds)
	}
	rep.Input = input

	ctx := NewEvalContext(input)

	for _, step := range pkg.Workflow.Steps {
		ts := TraceStep{StepID: step.ID, Type: step.TypeName()}

		if step.HasWhen {
			ok, err := evalCondition(step.When, ctx)
			if err != nil {
				ts.Error = err.Error()
				rep.Steps = append(rep.Steps, ts)
				continue
			}
			if !ok {
				ts.Skipped = true
				ctx.Steps[step.ID] = nil
				rep.Steps = append(rep.Steps, ts)
				continue
			}
		}

		switch {
		case step.API != nil:
			ts.Requirements = []string{"credential:" + step.API.Credential, "integration:" + step.API.Integration}
			plain, _ := model.FileRefParams(step.API.Params)
			b, _ := ctx.ResolveParams(plain)
			ts.Bindings = b
			rec, ok := fixtures[step.API.Tool]
			if !ok {
				ts.Error = fmt.Sprintf("no fixture for tool %q", step.API.Tool)
				break
			}
			out, _ := buildAPIOutput(rec)
			ctx.Steps[step.ID] = out
			ts.Output = out
		case step.Browser != nil:
			req := []string{}
			if step.Browser.Origin.Slot != "" {
				req = append(req, "origin-slot:"+step.Browser.Origin.Slot)
			}
			ts.Requirements = req
			urlVal, _ := ctx.ResolveValue(step.Browser.URL)
			ts.Bindings = map[string]interface{}{"url": urlVal}
			rec, ok := lookupBrowserFixture(fixtures, step.ID, tc.FixtureVariant)
			if !ok {
				ts.Error = "no browser fixture found"
				break
			}
			ctx.Steps[step.ID] = rec
			ts.Output = rec
		case step.Transform != nil:
			plain, fileRefs := model.FileRefParams(step.Transform.Params)
			b, _ := ctx.ResolveParams(plain)
			for name, rel := range fileRefs {
				b[name+"_from"] = rel
			}
			ts.Bindings = b
			result, err := runTransform(pkg, step, ctx)
			if err != nil {
				ts.Error = err.Error()
				break
			}
			ctx.Steps[step.ID] = result
			ts.Output = result
		case step.Reason != nil:
			ts.Requirements = []string{"reasoning-lease"}
			inputVal, _ := ctx.ResolveValue(step.Reason.Input)
			ts.Bindings = map[string]interface{}{"input": inputVal, "maxTokens": step.Reason.MaxTokens}
			if tc.LeaseMock == "" {
				ts.Skipped = true
				ctx.Steps[step.ID] = nil
				break
			}
			mockVal, ok := fixtures[tc.LeaseMock]
			if !ok {
				ts.Error = fmt.Sprintf("lease_mock %q not found", tc.LeaseMock)
				break
			}
			ctx.Steps[step.ID] = mockVal
			ts.Output = mockVal
		default:
			ts.Skipped = true
			ctx.Steps[step.ID] = nil
		}

		rep.Steps = append(rep.Steps, ts)
	}

	output := map[string]interface{}{}
	for name, v := range pkg.Workflow.Outputs {
		rv, _ := ctx.ResolveValue(v)
		output[name] = rv
	}
	rep.Output = output

	return rep, nil
}
