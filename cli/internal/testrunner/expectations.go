package testrunner

import (
	"fmt"

	"telara.dev/tap/internal/model"
	"telara.dev/tap/internal/schemautil"
)

func checkExpectations(res *CaseResult, pkg *model.Package, tc TestCase, output map[string]interface{},
	leaseInvoked bool, leaseMaxTokens []int, leaseInputChars []int, pagesWalked int, driftEventEmitted bool, stepsExecuted int) {

	exp := tc.Expect

	if want, ok := exp.boolField("output_schema_valid"); ok {
		_, schema, err := schemautil.LoadSchemaDoc(pkg.Manifest.OutputSchemaPath(), pkg.Manifest.Interface.OutputSchemaInline)
		if err != nil {
			res.Failures = append(res.Failures, fmt.Sprintf("output_schema_valid: could not load output schema: %v", err))
		} else {
			verr := schemautil.Validate(schema, output)
			valid := verr == nil
			if valid != want {
				msg := fmt.Sprintf("output_schema_valid: expected %v, got %v", want, valid)
				if verr != nil {
					msg += ": " + verr.Error()
				}
				res.Failures = append(res.Failures, msg)
			}
		}
	}

	if want, ok := exp.boolField("lease_invoked"); ok && want != leaseInvoked {
		res.Failures = append(res.Failures, fmt.Sprintf("lease_invoked: expected %v, got %v", want, leaseInvoked))
	}

	if want, ok := exp.intField("lease_max_tokens_respected"); ok {
		found := false
		for _, mt := range leaseMaxTokens {
			if mt == want {
				found = true
			}
		}
		if !found {
			res.Failures = append(res.Failures, fmt.Sprintf("lease_max_tokens_respected: no executed reason step declared maxTokens=%d (declared: %v)", want, leaseMaxTokens))
		}
	}

	if want, ok := exp.intField("lease_input_max_chars"); ok {
		for _, n := range leaseInputChars {
			if n > want {
				res.Failures = append(res.Failures, fmt.Sprintf("lease_input_max_chars: reason input was %d chars, exceeds %d", n, want))
			}
		}
	}

	if want, ok := exp.intField("pagination_pages_walked"); ok && want != pagesWalked {
		res.Failures = append(res.Failures, fmt.Sprintf("pagination_pages_walked: expected %d, got %d", want, pagesWalked))
	}

	if want, ok := exp.boolField("drift_event_emitted"); ok && want != driftEventEmitted {
		res.Failures = append(res.Failures, fmt.Sprintf("drift_event_emitted: expected %v, got %v", want, driftEventEmitted))
	}

	if want, ok := exp.intField("browser_retries"); ok {
		got := 0 // fixture-replay never simulates a transient retry loop; see README DECISION
		if want != got {
			res.Failures = append(res.Failures, fmt.Sprintf("browser_retries: expected %d, got %d", want, got))
		}
	}

	if want, ok := exp.intField("steps_executed"); ok && want != stepsExecuted {
		res.Failures = append(res.Failures, fmt.Sprintf("steps_executed: expected %d, got %d", want, stepsExecuted))
	}

	for path, want := range exp.outputMap() {
		got, ok := GetPath(output, path)
		if !ok {
			res.Failures = append(res.Failures, fmt.Sprintf("output.%s: path not found in output", path))
			continue
		}
		if !DeepEqualLoose(got, want) {
			res.Failures = append(res.Failures, fmt.Sprintf("output.%s: expected %v, got %v", path, want, got))
		}
	}

	for _, line := range exp.assertList() {
		if err := evalAssert(output, line); err != nil {
			res.Failures = append(res.Failures, err.Error())
		}
	}
}
