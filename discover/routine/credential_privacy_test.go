package routine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/discover/model"
	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

func TestCompoundDraftParameterizesConstantCamelCredentialFlags(t *testing.T) {
	for _, name := range []string{"dbPassword", "oauthToken", "clientCredentials", "passwordValue"} {
		t.Run(name, func(t *testing.T) {
			// The same short synthetic value in two distinct sessions must
			// still be an input, rather than a constant in the package.
			command := "tool --" + name + " q --tokenCount 5 --tokenizer ordinary-model --issue_key ABC-12 && tool status"
			step := trace.Step{Label: "sh:tool", Raw: command, Compound: true, Session: "synthetic-one"}
			other := step
			other.Session = "synthetic-two"
			draft := BuildDraft(model.Candidate{Items: []int{0}, Steps: []model.StepTemplate{{Label: step.Label}}, Sessions: 2}, [][]trace.Step{{step}, {other}}, model.DraftOptions{})
			if len(draft.Inputs) != 1 || !draft.Inputs[0].Sensitive || draft.Inputs[0].Example != "" {
				t.Fatalf("credential input not preserved: %+v", draft.Inputs)
			}
			main := string(draft.Files["main.sh"])
			if strings.Contains(main, "--"+name+" q") || !strings.Contains(main, "--"+name+` "${1}"`) {
				t.Fatalf("constant credential not replaced by an input: %s", main)
			}
			for _, normal := range []string{"--tokenCount 5", "--tokenizer ordinary-model", "--issue_key ABC-12", "&& tool status"} {
				if !strings.Contains(main, normal) {
					t.Fatalf("ordinary command changed: %s", main)
				}
			}
			for file, raw := range draft.Files {
				if strings.Contains(string(raw), "--"+name+" q") {
					t.Errorf("%s retains the recorded credential", file)
				}
			}
		})
	}
}

func TestToolDraftSuppliesNestedCredentialConfigAsRawJSON(t *testing.T) {
	secretConfig := `{"rows":[{"env":{"dbPassword":"q"},"region":"west"}],"enabled":true}`
	benignConfig := `{"tokenizer":"ordinary-model","token_count":5,"password_length":24,"issue_key":"ABC-12"}`
	step := trace.Step{Label: "mcp:synthetic_inspect", Session: "synthetic-one", Slots: []trace.Slot{
		{Key: "config", Type: trace.SlotText, Value: secretConfig, Raw: true},
		{Key: "options", Type: trace.SlotText, Value: benignConfig, Raw: true},
	}}
	other := step
	other.Session = "synthetic-two"
	draft := BuildDraft(model.Candidate{Items: []int{0}, Steps: []model.StepTemplate{{Label: step.Label}}, Sessions: 2}, [][]trace.Step{{step}, {other}}, model.DraftOptions{})
	if len(draft.Inputs) != 1 || !draft.Inputs[0].Sensitive || !draft.Inputs[0].Raw || draft.Inputs[0].Example != "" {
		t.Fatalf("nested credential not supplied as raw caller JSON: %+v", draft.Inputs)
	}
	if len(draft.Blocked) != 0 {
		t.Fatalf("benign tokenizer configuration blocks the package: %v", draft.Blocked)
	}
	main := string(draft.Files["main.sh"])
	if !strings.Contains(main, `a1_1=$(json_raw "${1}")`) || !strings.Contains(main, `'"config":'"$a1_1"`) || !strings.Contains(main, benignConfig) {
		t.Fatalf("JSON parameter typing or benign configuration changed: %s", main)
	}
	for file, raw := range draft.Files {
		if strings.Contains(string(raw), `"dbPassword":"q"`) {
			t.Errorf("%s retains a credential default", file)
		}
	}
	// Caller values retain the entire object, array and boolean structure,
	// including ordinary fields; only the generated default is forbidden.
	values := DraftInputValues(draft, 0)
	if values[0] != secretConfig || !json.Valid([]byte(values[0])) {
		t.Fatalf("caller input cannot replay the complete recorded shape: %v", values)
	}
}
