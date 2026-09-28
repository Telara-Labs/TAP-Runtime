package testrunner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"telara.dev/tap/internal/model"
	"telara.dev/tap/internal/schemautil"
	"telara.dev/tap/internal/star"
)

// caseError is a sentinel abort carrying one of the taxonomy error codes
// used across both example packages' contract tests: schema_invalid,
// egress_blocked, origin_blocked. execution_error is tap's own fallback for
// a genuine harness/fixture problem (missing fixture data, a starlark
// exception, etc.) that the case did not expect.
type caseError struct {
	Code string
	Msg  string
}

func (e *caseError) Error() string { return e.Code + ": " + e.Msg }

type Options struct {
	Binds      map[string]string
	CaseFilter string
}

type CaseResult struct {
	Name          string
	Passed        bool
	Failures      []string
	StepsExecuted int
	Skipped       bool
}

type RunReport struct {
	Dir   string
	Cases []CaseResult
}

func (r RunReport) Passed() int {
	n := 0
	for _, c := range r.Cases {
		if c.Passed {
			n++
		}
	}
	return n
}

func (r RunReport) Failed() int { return len(r.Cases) - r.Passed() }

// Run loads tests/contract.test.yaml under pkg.Dir and executes every case.
func Run(pkg *model.Package, opts Options) (*RunReport, error) {
	if pkg.Workflow == nil {
		return nil, fmt.Errorf("workflow.yaml did not load: %v", pkg.WorkflowErr)
	}
	contractPath := filepath.Join(pkg.Dir, "tests", "contract.test.yaml")
	if _, err := os.Stat(contractPath); err != nil {
		return nil, fmt.Errorf("no tests/contract.test.yaml in %s", pkg.Dir)
	}
	cf, err := LoadContractFile(contractPath)
	if err != nil {
		return nil, err
	}
	binds := MergeBindings(cf.Bindings, opts.Binds)
	testsDir := filepath.Dir(contractPath)

	report := &RunReport{Dir: pkg.Dir}
	for _, tc := range cf.Cases {
		if opts.CaseFilter != "" && !strings.Contains(tc.Name, opts.CaseFilter) {
			continue
		}
		report.Cases = append(report.Cases, runCase(pkg, testsDir, tc, binds))
	}
	return report, nil
}

func runCase(pkg *model.Package, testsDir string, tc TestCase, binds map[string]string) CaseResult {
	res := CaseResult{Name: tc.Name}

	var fixtures map[string]interface{}
	if tc.Fixtures != "" {
		f, err := LoadFixtures(filepath.Join(testsDir, tc.Fixtures))
		if err != nil {
			res.Failures = append(res.Failures, fmt.Sprintf("loading fixtures: %v", err))
			return res
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
	if tc.Now != "" {
		// CHANGELOG.md v1 ruling 3: a test case may pin the reserved `now`
		// input (ISO-8601 datetime the runtime auto-stamps in production)
		// so deterministic staleness/monitoring logic is testable offline.
		if input == nil {
			input = map[string]interface{}{}
		}
		input["now"] = tc.Now
	}

	// Gate: schema validation, before any step runs (04-cli.md taxonomy: schema).
	if cErr := validateInputSchema(pkg, input); cErr != nil {
		return finishAborted(res, tc, cErr, 0)
	}

	ctx := NewEvalContext(input)
	stepsExecuted := 0
	leaseInvoked := false
	var leaseMaxTokens []int
	var leaseInputChars []int
	paginationPagesWalked := 0
	driftEventEmitted := false

	for _, step := range pkg.Workflow.Steps {
		switch {
		case step.API != nil:
			if step.HasWhen {
				ok, err := evalCondition(step.When, ctx)
				if err != nil {
					return finishAborted(res, tc, &caseError{Code: "execution_error", Msg: err.Error()}, stepsExecuted)
				}
				if !ok {
					ctx.Steps[step.ID] = nil
					continue
				}
			}
			if tc.OverrideStep != nil && tc.OverrideStep.ID == step.ID && tc.OverrideStep.EgressHost != "" {
				if !egressHostAllowed(pkg.Manifest.Requirements.Network.EgressHosts, tc.OverrideStep.EgressHost, binds) {
					return finishAborted(res, tc, &caseError{Code: "egress_blocked", Msg: fmt.Sprintf(
						"step %q would call undeclared egress host %q (requirements.network.egressHosts: %v)",
						step.ID, tc.OverrideStep.EgressHost, pkg.Manifest.Requirements.Network.EgressHosts)}, stepsExecuted)
				}
			}
			rec, ok := fixtures[step.API.Tool]
			if !ok {
				return finishAborted(res, tc, &caseError{Code: "execution_error", Msg: fmt.Sprintf("no fixture entry for tool %q (step %q)", step.API.Tool, step.ID)}, stepsExecuted)
			}
			out, pages := buildAPIOutput(rec)
			if step.API.Paginate != "" {
				paginationPagesWalked += pages
			}
			ctx.Steps[step.ID] = out
			stepsExecuted++

		case step.Browser != nil:
			if step.HasWhen {
				ok, err := evalCondition(step.When, ctx)
				if err != nil {
					return finishAborted(res, tc, &caseError{Code: "execution_error", Msg: err.Error()}, stepsExecuted)
				}
				if !ok {
					ctx.Steps[step.ID] = nil
					continue
				}
			}
			if urlVal, err := ctx.ResolveValue(step.Browser.URL); err == nil {
				if urlStr, ok := urlVal.(string); ok && urlStr != "" && step.Browser.Origin.Slot != "" {
					if boundOrigin, ok := binds[step.Browser.Origin.Slot]; ok {
						if same, serr := sameOrigin(urlStr, boundOrigin); serr == nil && !same {
							return finishAborted(res, tc, &caseError{Code: "origin_blocked", Msg: fmt.Sprintf(
								"url %q is outside the bound origin for slot %q (%s)", urlStr, step.Browser.Origin.Slot, boundOrigin)}, stepsExecuted)
						}
					}
				}
			}
			rec, ok := lookupBrowserFixture(fixtures, step.ID, tc.FixtureVariant)
			if !ok {
				return finishAborted(res, tc, &caseError{Code: "execution_error", Msg: fmt.Sprintf("no browser fixture for step %q", step.ID)}, stepsExecuted)
			}
			ctx.Steps[step.ID] = rec
			if d, _ := rec["drift"].(bool); d {
				driftEventEmitted = true
			}
			stepsExecuted++

		case step.Transform != nil:
			if step.HasWhen {
				ok, err := evalCondition(step.When, ctx)
				if err != nil {
					return finishAborted(res, tc, &caseError{Code: "execution_error", Msg: err.Error()}, stepsExecuted)
				}
				if !ok {
					ctx.Steps[step.ID] = nil
					continue
				}
			}
			result, err := runTransform(pkg, step, ctx)
			if err != nil {
				return finishAborted(res, tc, &caseError{Code: "execution_error", Msg: err.Error()}, stepsExecuted)
			}
			ctx.Steps[step.ID] = result
			stepsExecuted++

		case step.Reason != nil:
			if step.HasWhen {
				ok, err := evalCondition(step.When, ctx)
				if err != nil {
					return finishAborted(res, tc, &caseError{Code: "execution_error", Msg: err.Error()}, stepsExecuted)
				}
				if !ok {
					ctx.Steps[step.ID] = nil
					continue
				}
			}
			if tc.LeaseMock == "" {
				// Reached a reason: step with no lease_mock supplied: the
				// case isn't exercising this step's result (e.g. the
				// anti-bot drift case only cares that drift was detected,
				// not what the fallback lease would have said), so leave
				// its output null rather than hard-failing the case.
				ctx.Steps[step.ID] = nil
				stepsExecuted++
				continue
			}
			mockVal, ok := fixtures[tc.LeaseMock]
			if !ok {
				return finishAborted(res, tc, &caseError{Code: "execution_error", Msg: fmt.Sprintf("lease_mock %q not found in fixtures", tc.LeaseMock)}, stepsExecuted)
			}
			ctx.Steps[step.ID] = mockVal
			leaseInvoked = true
			leaseMaxTokens = append(leaseMaxTokens, step.Reason.MaxTokens)
			if inputVal, err := ctx.ResolveValue(step.Reason.Input); err == nil {
				if s, ok := inputVal.(string); ok {
					leaseInputChars = append(leaseInputChars, len(s))
				}
			}
			stepsExecuted++

		default:
			// branch/approval/code/primitive: not exercised by the two
			// golden packages; treat as a no-op skip rather than a hard
			// failure so future packages using them still run.
			ctx.Steps[step.ID] = nil
		}
	}

	output := map[string]interface{}{}
	for name, v := range pkg.Workflow.Outputs {
		rv, err := ctx.ResolveValue(v)
		if err != nil {
			res.Failures = append(res.Failures, fmt.Sprintf("resolving output %q: %v", name, err))
			continue
		}
		output[name] = rv
	}

	res.StepsExecuted = stepsExecuted

	if expectErr, ok := tc.Expect.stringField("error"); ok {
		res.Failures = append(res.Failures, fmt.Sprintf("expected error %q but the case completed normally", expectErr))
	}

	checkExpectations(&res, pkg, tc, output, leaseInvoked, leaseMaxTokens, leaseInputChars, paginationPagesWalked, driftEventEmitted, stepsExecuted)

	res.Passed = len(res.Failures) == 0
	return res
}

func finishAborted(res CaseResult, tc TestCase, cErr *caseError, stepsExecuted int) CaseResult {
	res.StepsExecuted = stepsExecuted
	expectErr, hasExpect := tc.Expect.stringField("error")
	if !hasExpect {
		res.Failures = append(res.Failures, fmt.Sprintf("unexpected abort: %v", cErr))
	} else if expectErr != cErr.Code {
		res.Failures = append(res.Failures, fmt.Sprintf("expected error %q, got %q (%s)", expectErr, cErr.Code, cErr.Msg))
	}
	if want, ok := tc.Expect.intField("steps_executed"); ok && want != stepsExecuted {
		res.Failures = append(res.Failures, fmt.Sprintf("steps_executed: expected %d, got %d", want, stepsExecuted))
	}
	res.Passed = len(res.Failures) == 0
	return res
}

func validateInputSchema(pkg *model.Package, input map[string]interface{}) *caseError {
	_, schema, err := schemautil.LoadSchemaDoc(pkg.Manifest.InputSchemaPath(), pkg.Manifest.Interface.InputSchemaInline)
	if err != nil {
		return &caseError{Code: "execution_error", Msg: err.Error()}
	}
	if err := schemautil.Validate(schema, input); err != nil {
		return &caseError{Code: "schema_invalid", Msg: err.Error()}
	}
	return nil
}

func runTransform(pkg *model.Package, step model.Step, ctx *EvalContext) (interface{}, error) {
	ts := step.Transform
	var program string
	if ts.ExpressionFrom != "" {
		b, err := os.ReadFile(pkg.ResolvePath(ts.ExpressionFrom))
		if err != nil {
			return nil, fmt.Errorf("transform step %q: %w", step.ID, err)
		}
		program = string(b)
	} else {
		program = ts.Expression
		if !strings.Contains(program, "result") {
			program = "result = (\n" + program + "\n)"
		}
	}

	plain, fileRefs := model.FileRefParams(ts.Params)
	globals, err := ctx.ResolveParams(plain)
	if err != nil {
		return nil, fmt.Errorf("transform step %q: %w", step.ID, err)
	}
	for globalName, rel := range fileRefs {
		v, err := loadFileRefGlobal(pkg, globalName, rel)
		if err != nil {
			return nil, fmt.Errorf("transform step %q: %w", step.ID, err)
		}
		globals[globalName] = v
	}

	filename := ts.ExpressionFrom
	if filename == "" {
		filename = step.ID + ".star"
	}
	return star.Exec(filename, program, globals)
}

// loadFileRefGlobal loads a `<name>_from: path/to/file.yaml` data file and
// unwraps it if it's a single-key map with a key matching the global name
// (the gitlab-pipeline-triage convention: `signatures_from: src/signatures.yaml`
// binds the global `signatures` to the value of that file's top-level
// `signatures:` key).
func loadFileRefGlobal(pkg *model.Package, globalName, rel string) (interface{}, error) {
	raw, _, err := model.LoadYAMLFile(pkg.ResolvePath(rel))
	if err != nil {
		return nil, err
	}
	if len(raw) == 1 {
		if v, ok := raw[globalName]; ok {
			return v, nil
		}
	}
	return raw, nil
}
