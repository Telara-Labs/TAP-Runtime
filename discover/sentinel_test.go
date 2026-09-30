package discover

// Sentinel set (plan section 8): at least ten supported positives across at
// least three task families that discovery must recommend as useful. It
// guards against passing by abstaining on everything. The cases are fresh
// variants (different tools, names and values) of the shapes the
// recognizer declares it supports, not copies of the C01-C24 fixtures.

import (
	"fmt"
	"strings"
	"testing"
)

type sentinel struct {
	name, family string
	sessions     []Session
	// want are step labels the recommended routine must contain.
	want []string
}

func sentinels() []sentinel {
	var out []sentinel
	// Family: code review (read-only).
	out = append(out, sentinel{"changed files between tags", "review", eps("s1", 6, func(i int) string {
		return fmt.Sprintf("list files that changed between v1.%d.0 and v1.%d.1", i, i)
	}, func(i int) []Call {
		a, b := fmt.Sprintf("v1.%d.0", i), fmt.Sprintf("v1.%d.1", i)
		return []Call{sh("git diff --name-only " + a + " " + b), sh("git log --oneline " + a + ".." + b)}
	}), []string{"sh:git diff", "sh:git log"}})
	out = append(out, sentinel{"blame a file at a revision", "review", eps("s2", 5, func(i int) string {
		return fmt.Sprintf("who last changed internal/auth/token%d.go at rev%d", i, i)
	}, func(i int) []Call {
		f := fmt.Sprintf("internal/auth/token%d.go", i)
		return []Call{sh("git log -1 --format=%an -- " + f), sh("git blame -L 1,40 " + f)}
	}), []string{"sh:git log", "sh:git blame"}})
	// Family: files (loops over a caller list).
	out = append(out, sentinel{"line counts of listed files", "files", eps("s3", 9, func(i int) string {
		var fs []string
		for f := 0; f < 2+i%3; f++ {
			fs = append(fs, fmt.Sprintf("cfg/a%d-%d.yaml", i, f))
		}
		return "count lines in " + strings.Join(fs, " ")
	}, func(i int) []Call {
		var cs []Call
		for f := 0; f < 2+i%3; f++ {
			cs = append(cs, sh(fmt.Sprintf("wc -l cfg/a%d-%d.yaml", i, f)))
		}
		return cs
	}), []string{"sh:wc"}})
	// Family: CI (tool calls, parameterized by the caller).
	out = append(out, sentinel{"pipeline and its jobs", "ci", eps("s4", 6, func(i int) string {
		return fmt.Sprintf("what failed in build %d", 900400+i)
	}, func(i int) []Call {
		id := fmt.Sprint(900400 + i)
		return []Call{{Tool: "mcp:ci_get_build", Args: map[string]string{"build_id": id}}, {Tool: "mcp:ci_list_build_steps", Args: map[string]string{"build_id": id}}}
	}), []string{"mcp:ci_get_build", "mcp:ci_list_build_steps"}})
	// Family: CI with an output dependency (JSON path).
	out = append(out, sentinel{"latest run then its log", "ci", eps("s5", 7, func(i int) string {
		return fmt.Sprintf("show the log of the latest run of workflow deploy-%d", i)
	}, func(i int) []Call {
		run := fmt.Sprintf("77%06d", 1000+i)
		list := Call{Tool: "mcp:ci_list_runs", Args: map[string]string{"workflow": fmt.Sprintf("deploy-%d", i)},
			Output: fmt.Sprintf(`{"runs":[{"id":"%s","status":"failure"}]}`, run)}
		list.OutIDs, list.OutCtx, list.OutPaths = outputRefsPaths(list.Output)
		return []Call{list, {Tool: "mcp:ci_get_run_log", Args: map[string]string{"run_id": run}}}
	}), []string{"mcp:ci_list_runs", "mcp:ci_get_run_log"}})
	// Family: cluster operations (reads, scope, writes).
	out = append(out, sentinel{"describe and events of a deployment", "cluster", eps("s6", 6, func(i int) string {
		return fmt.Sprintf("why is deployment billing-%d not ready", i)
	}, func(i int) []Call {
		d := fmt.Sprintf("billing-%d", i)
		return []Call{sh("kubectl --context kind-dev -n billing describe deploy " + d), sh("kubectl --context kind-dev -n billing get events --field-selector involvedObject.name=" + d)}
	}), []string{"sh:kubectl describe", "sh:kubectl get"}})
	out = append(out, sentinel{"scale a deployment", "cluster", eps("s7", 6, func(i int) string {
		return fmt.Sprintf("scale worker-%d to %d replicas", i, 2+i)
	}, func(i int) []Call {
		d := fmt.Sprintf("worker-%d", i)
		return []Call{sh(fmt.Sprintf("kubectl --context kind-dev -n jobs scale deploy/%s --replicas=%d", d, 2+i)), sh("kubectl --context kind-dev -n jobs rollout status deploy/" + d)}
	}), []string{"sh:kubectl scale", "sh:kubectl rollout"}})
	// Family: tickets (writes with caller inputs).
	out = append(out, sentinel{"close a ticket with a note", "tickets", eps("s8", 6, func(i int) string {
		return fmt.Sprintf("close OPS-%d as fixed", 510+i)
	}, func(i int) []Call {
		k := fmt.Sprintf("OPS-%d", 510+i)
		return []Call{{Tool: "mcp:tracker_transition", Args: map[string]string{"key": k, "to": "Done"}}, {Tool: "mcp:tracker_comment", Args: map[string]string{"key": k, "text": "fixed"}}}
	}), []string{"mcp:tracker_transition", "mcp:tracker_comment"}})
	out = append(out, sentinel{"label a ticket and assign it", "tickets", eps("s9", 6, func(i int) string {
		return fmt.Sprintf("mark OPS-%d as a regression and assign it to the on-call", 700+i)
	}, func(i int) []Call {
		k := fmt.Sprintf("OPS-%d", 700+i)
		return []Call{{Tool: "mcp:tracker_add_label", Args: map[string]string{"key": k, "label": "regression"}}, {Tool: "mcp:tracker_assign", Args: map[string]string{"key": k, "assignee": "on-call"}}}
	}), []string{"mcp:tracker_add_label", "mcp:tracker_assign"}})
	// Family: records with a text-derived id.
	out = append(out, sentinel{"open a record then attach it", "records", eps("s10", 7, func(i int) string {
		return fmt.Sprintf("open an incident record for outage %d and attach the timeline", i)
	}, func(i int) []Call {
		id := fmt.Sprintf("%08x-aa01-4847-a933-187b18ef2985", 0x51000000+i)
		open := Call{Tool: "mcp:incidents_open", Args: map[string]string{"title": fmt.Sprintf("outage %d", i)},
			Output: "Opened.\n\n- **Incident:** `" + id + "`\n"}
		open.OutIDs, open.OutCtx, open.OutPaths = outputRefsPaths(open.Output)
		return []Call{open, {Tool: "mcp:incidents_attach", Args: map[string]string{"incident": id, "file": "timeline.md"}}}
	}), []string{"mcp:incidents_open", "mcp:incidents_attach"}})
	return out
}

func TestSentinelSupportedPositivesAreRecommended(t *testing.T) {
	cases := sentinels()
	families := map[string]bool{}
	for _, c := range cases {
		families[c.family] = true
	}
	if len(cases) < 10 || len(families) < 3 {
		t.Fatalf("sentinel set too small: %d cases, %d families", len(cases), len(families))
	}
	found := 0
	for _, c := range cases {
		rep := runOn(t, c.sessions)
		ok := false
		for _, r := range rep.Routines {
			if r.Suitability != SuitUseful || r.MergedInto != "" {
				continue
			}
			all := true
			for _, l := range c.want {
				if !hasStep(r, l) {
					all = false
				}
			}
			if all {
				ok = true
			}
		}
		if ok {
			found++
		} else {
			t.Errorf("sentinel %q (%s) not recommended:\n%s", c.name, c.family, dump(rep))
		}
	}
	t.Logf("sentinels found: %d of %d across %d families", found, len(cases), len(families))
}
