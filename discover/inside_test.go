package discover

import (
	"encoding/json"
	"testing"
)

func labelsAndSlot(steps []Step, key string) ([]string, []string) {
	var ls, vs []string
	for _, s := range steps {
		ls = append(ls, s.Label)
		for _, sl := range s.Slots {
			if sl.Key == key {
				vs = append(vs, sl.Value)
			}
		}
	}
	return ls, vs
}

func TestJSStepsAreAwaitedCalls(t *testing.T) {
	// Shape taken from a real Codex outreach session.
	code := "await liTab.goto(\"https://www.linkedin.com/in/someone/\"); await liTab.playwright.waitForTimeout(800); " +
		"var ss = await liTab.playwright.domSnapshot(); nodeRepl.write(JSON.stringify(ss)); await chrome.tabs.new();"
	steps := jsSteps(code)
	_, recv := labelsAndSlot(steps, "recv")
	if !equal(recv, []string{"liTab", "liTab", "chrome"}) {
		t.Fatalf("receivers = %q", recv)
	}
	ls, hosts := labelsAndSlot(steps, "0.host")
	want := []string{"js:goto", "js:playwright.domSnapshot", "js:tabs.new"}
	if !equal(ls, want) {
		t.Fatalf("labels = %q, want %q (waits and un-awaited utilities such as JSON.stringify must not appear)", ls, want)
	}
	if len(hosts) != 1 || hosts[0] != "www.linkedin.com" {
		t.Fatalf("hosts = %q", hosts)
	}
}

func TestPatchStepsNameEachFile(t *testing.T) {
	in := "*** Begin Patch\n*** Update File: /x/telara/activity.log\n@@\n+line\n*** Add File: notes/memory.md\n+# m\n*** End Patch"
	ls, names := labelsAndSlot(patchSteps(in), "name")
	if !equal(ls, []string{"patch:update", "patch:add"}) || !equal(names, []string{"activity.log", "memory.md"}) {
		t.Fatalf("labels %q names %q", ls, names)
	}
}

func TestCodexPatchInputsAreRead(t *testing.T) {
	// Standalone apply_patch (freeform input) and tools.apply_patch(`...`)
	// inside an exec body were both recorded without their patch before.
	calls := jsToolCalls("await tools.apply_patch(`*** Begin Patch\n*** Update File: a/activity.log\n@@\n*** End Patch`);")
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	var in string
	_ = json.Unmarshal(calls[0].args["input"], &in)
	steps := stepsOf(Call{Tool: "apply_patch", Args: map[string]string{"input": in}}, nil)
	if len(steps) != 1 || steps[0].Label != "patch:update" {
		t.Fatalf("steps = %+v", steps)
	}
}

func TestSkillMentionInsideADocumentIsNotALoad(t *testing.T) {
	long := "*** Begin Patch\n*** Update File: docs/x.md\n+see skills/push-to-prod/SKILL.md for the steps\n" + string(make([]byte, 400))
	if sk := skillOf(Call{Tool: "apply_patch", Args: map[string]string{"input": long}}); sk != "" {
		t.Fatalf("a patch mentioning a SKILL.md counted as loading %q", sk)
	}
	if sk := skillOf(Call{Tool: "shell", Command: "cat ~/.codex/skills/icp-outreach/SKILL.md"}); sk != "icp-outreach" {
		t.Fatalf("reading SKILL.md = %q", sk)
	}
}

func TestJSStepsKeepFunctionArguments(t *testing.T) {
	steps := jsSteps("await tab.playwright.evaluate(() => document.title); await agent.browsers.get('extension');")
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
