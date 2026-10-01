package discover

import (
	"encoding/json"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/discover/history"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

func TestCodexPatchInputsAreRead(t *testing.T) {
	// Standalone apply_patch (freeform input) and tools.apply_patch(`...`)
	// inside an exec body were both recorded without their patch before.
	calls := history.JsToolCalls("await tools.apply_patch(`*** Begin Patch\n*** Update File: a/activity.log\n@@\n*** End Patch`);")
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	var in string
	_ = json.Unmarshal(calls[0].Args["input"], &in)
	steps := trace.StepsOf(trace.Call{Tool: "apply_patch", Args: map[string]string{"input": in}}, nil)
	if len(steps) != 1 || steps[0].Label != "patch:update" {
		t.Fatalf("steps = %+v", steps)
	}
}

func TestSkillMentionInsideADocumentIsNotALoad(t *testing.T) {
	long := "*** Begin Patch\n*** Update File: docs/x.md\n+see skills/push-to-prod/SKILL.md for the steps\n" + string(make([]byte, 400))
	if sk := trace.SkillOf(trace.Call{Tool: "apply_patch", Args: map[string]string{"input": long}}); sk != "" {
		t.Fatalf("a patch mentioning a SKILL.md counted as loading %q", sk)
	}
	if sk := trace.SkillOf(trace.Call{Tool: "shell", Command: "cat ~/.codex/skills/icp-outreach/SKILL.md"}); sk != "icp-outreach" {
		t.Fatalf("reading SKILL.md = %q", sk)
	}
}

func TestJSStepsKeepFunctionArguments(t *testing.T) {
	steps := trace.JsSteps("await tab.playwright.evaluate(() => document.title); await agent.browsers.get('extension');")
	if len(steps) != 2 {
		t.Fatalf("steps = %d", len(steps))
	}
	arg := steps[0].Slots[1]
	if arg.Key != "0" || arg.Value != "() => document.title" || !arg.Raw {
		t.Fatalf("function argument = %+v", arg)
	}
	if steps[1].Slots[1].Value != "extension" || steps[1].Slots[1].Raw {
		t.Fatalf("string argument = %+v", steps[1].Slots[1])
	}
}
