package trace

import (
	"slices"
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
	steps := JsSteps(code)
	_, recv := labelsAndSlot(steps, "recv")
	if !slices.Equal(recv, []string{"liTab", "liTab", "chrome"}) {
		t.Fatalf("receivers = %q", recv)
	}
	ls, hosts := labelsAndSlot(steps, "0.host")
	want := []string{"js:goto", "js:playwright.domSnapshot", "js:tabs.new"}
	if !slices.Equal(ls, want) {
		t.Fatalf("labels = %q, want %q (waits and un-awaited utilities such as JSON.stringify must not appear)", ls, want)
	}
	if len(hosts) != 1 || hosts[0] != "www.linkedin.com" {
		t.Fatalf("hosts = %q", hosts)
	}
}

func TestPatchStepsNameEachFile(t *testing.T) {
	in := "*** Begin Patch\n*** Update File: /x/telara/activity.log\n@@\n+line\n*** Add File: notes/memory.md\n+# m\n*** End Patch"
	ls, names := labelsAndSlot(PatchSteps(in), "name")
	if !slices.Equal(ls, []string{"patch:update", "patch:add"}) || !slices.Equal(names, []string{"activity.log", "memory.md"}) {
		t.Fatalf("labels %q names %q", ls, names)
	}
}
