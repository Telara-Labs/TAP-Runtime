package discover

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/discover/model"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

func compositionNode(ordinal int, action string, args map[string]string, inputs ...model.SpanInput) spanNode {
	if args == nil {
		args = map[string]string{}
	}
	args["integration"] = "jira"
	args["action"] = action
	var slots []trace.Slot
	for key, value := range args {
		slots = append(slots, trace.Slot{Key: key, Value: value, Type: trace.SlotText})
	}
	return spanNode{ordinal: ordinal, call: trace.Call{Tool: "mcp:telara_execute_action", Args: args}, steps: []trace.Step{{Label: "mcp:telara_execute_action", Slots: slots}}, inputs: inputs}
}

func TestWriteSpanProposalsUsesCompositionGroups(t *testing.T) {
	p := model.SpanProposal{ID: "sp_example", Composition: model.SpanComposition{Actions: []string{"jira.create_issue", "jira.transition_issue"}}}
	r := &model.Report{SpanProposals: []model.SpanProposal{p}, SpanGroups: []model.SpanGroup{{Proposals: 1}}, CompositionGroups: []model.SpanCompositionGroup{{Proposals: 1, Sessions: 1, Example: p}}}
	var out bytes.Buffer
	WriteSpanProposals(&out, r, 1)
	if !strings.Contains(out.String(), "1 composition groups") || !strings.Contains(out.String(), "jira.create_issue > jira.transition_issue") {
		t.Fatalf("composition review index absent from CLI output: %s", out.String())
	}
}

func TestSpanCompositionFoldsIndependentRepeatedSteps(t *testing.T) {
	single := []spanNode{
		compositionNode(1, "create_issue", nil),
		compositionNode(2, "transition_issue", nil, model.SpanInput{Key: "issue_key", Type: trace.SlotID, Source: "prior_result", FromCall: 1}),
	}
	single[0].call.OutIDs = []string{"TENG-1"}
	single[1].steps[0].Slots = append(single[1].steps[0].Slots, trace.Slot{Key: "issue_key", Type: trace.SlotID, Value: "TENG-1"})
	repeated := []spanNode{
		compositionNode(1, "create_issue", nil),
		compositionNode(2, "create_issue", nil),
		compositionNode(3, "transition_issue", nil, model.SpanInput{Key: "issue_key", Type: trace.SlotID, Source: "prior_result", FromCall: 1}),
		compositionNode(4, "transition_issue", nil, model.SpanInput{Key: "issue_key", Type: trace.SlotID, Source: "prior_result", FromCall: 2}),
	}
	repeated[0].call.OutIDs = []string{"TENG-1"}
	repeated[1].call.OutIDs = []string{"TENG-2"}
	repeated[2].steps[0].Slots = append(repeated[2].steps[0].Slots, trace.Slot{Key: "issue_key", Type: trace.SlotID, Value: "TENG-1"})
	repeated[3].steps[0].Slots = append(repeated[3].steps[0].Slots, trace.Slot{Key: "issue_key", Type: trace.SlotID, Value: "TENG-2"})
	a := spanComposition(single, []int{0, 1})
	b := spanComposition(repeated, []int{0, 1, 2, 3})
	if a.Key != b.Key || !reflect.DeepEqual(a.Actions, []string{"jira.create_issue", "jira.transition_issue"}) || len(b.Repetition) != 2 {
		t.Fatalf("composition did not fold same-role fanout: %+v / %+v", a, b)
	}
	for _, repeat := range b.Repetition {
		if repeat.Action == "jira.transition_issue" && repeat.Kind != "for_each" {
			t.Fatalf("transitions of separate created issues should form a loop: %+v", b)
		}
	}
	interleaved := []spanNode{repeated[0], repeated[2], repeated[1], repeated[3]}
	for i := range interleaved {
		interleaved[i].ordinal = i + 1
	}
	interleaved[1].inputs[0].FromCall = 1
	interleaved[3].inputs[0].FromCall = 3
	c := spanComposition(interleaved, []int{0, 1, 2, 3})
	if c.Key != a.Key {
		t.Fatalf("repeated whole composition should share skeleton: %+v / %+v", a, c)
	}
}

func TestSpanCompositionDoesNotFoldRetryOnSameTarget(t *testing.T) {
	first := compositionNode(1, "transition_issue", map[string]string{"transition_id": "11"})
	second := compositionNode(2, "transition_issue", map[string]string{"transition_id": "11"})
	for _, n := range []*spanNode{&first, &second} {
		n.steps[0].Slots = append(n.steps[0].Slots, trace.Slot{Key: "issue_key", Type: trace.SlotID, Value: "TENG-1"})
	}
	c := spanComposition([]spanNode{first, second}, []int{0, 1})
	if len(c.Actions) != 2 || len(c.Repetition) != 1 || c.Repetition[0].Kind == "for_each" {
		t.Fatalf("retry on one issue must remain a visible repeated step: %+v", c)
	}
}

func TestSpanCompositionDoesNotFoldMotifWithRepeatedTarget(t *testing.T) {
	nodes := []spanNode{
		compositionNode(1, "create_issue", nil),
		compositionNode(2, "create_issue", nil),
		compositionNode(3, "transition_issue", nil),
		compositionNode(4, "create_issue", nil),
		compositionNode(5, "transition_issue", nil),
	}
	for i, id := range []string{"TENG-1", "TENG-2", "", "TENG-3"} {
		if id != "" {
			nodes[i].call.OutIDs = []string{id}
		}
	}
	for _, i := range []int{2, 4} {
		nodes[i].steps[0].Slots = append(nodes[i].steps[0].Slots, trace.Slot{Key: "issue_key", Type: trace.SlotID, Value: "TENG-2"})
	}
	c := spanComposition(nodes, []int{0, 1, 2, 3, 4})
	if len(c.Actions) != 4 {
		t.Fatalf("same transition target in second motif was incorrectly folded: %+v", c)
	}
}

func TestSpanCompositionKeepsOrderedTransitionsAndDependencies(t *testing.T) {
	base := []spanNode{
		compositionNode(1, "create_issue", nil),
		compositionNode(2, "transition_issue", map[string]string{"status": "In Progress"}, model.SpanInput{Key: "issue_key", Type: trace.SlotID, Source: "prior_result", FromCall: 1}),
		compositionNode(3, "transition_issue", map[string]string{"status": "Done"}, model.SpanInput{Key: "issue_key", Type: trace.SlotID, Source: "prior_result", FromCall: 1}),
	}
	c := spanComposition(base, []int{0, 1, 2})
	if len(c.Actions) != 3 || c.Actions[1] == c.Actions[2] {
		t.Fatalf("different state transitions were folded: %+v", c)
	}
	sequential := []spanNode{
		compositionNode(1, "create_issue", nil),
		compositionNode(2, "create_issue", nil, model.SpanInput{Key: "parent_key", Type: trace.SlotID, Source: "prior_result", FromCall: 1}),
	}
	d := spanComposition(sequential, []int{0, 1})
	if len(d.Actions) != 2 || !strings.Contains(d.Key, "jira.create_issue -> jira.create_issue") {
		t.Fatalf("dependent same-action chain was collapsed: %+v", d)
	}
}

func TestSpanCompositionForEachNeedsDistinctSourcedItems(t *testing.T) {
	first := compositionNode(1, "transition_issue", nil, model.SpanInput{Key: "issue_key", Type: trace.SlotID, Source: "caller"})
	second := compositionNode(2, "transition_issue", nil, model.SpanInput{Key: "issue_key", Type: trace.SlotID, Source: "caller"})
	first.steps[0].Slots = append(first.steps[0].Slots, trace.Slot{Key: "issue_key", Type: trace.SlotID, Value: "TENG-1"})
	second.steps[0].Slots = append(second.steps[0].Slots, trace.Slot{Key: "issue_key", Type: trace.SlotID, Value: "TENG-2"})
	c := spanComposition([]spanNode{first, second}, []int{0, 1})
	if len(c.Repetition) != 1 || c.Repetition[0].Kind != "for_each" {
		t.Fatalf("distinct caller-supplied items should be an observed for_each: %+v", c)
	}
	second.steps[0].Slots[len(second.steps[0].Slots)-1].Value = "TENG-1"
	c = spanComposition([]spanNode{first, second}, []int{0, 1})
	if c.Repetition[0].Kind == "for_each" {
		t.Fatalf("same item is not an iteration over a list: %+v", c)
	}
}

func TestSpanCompositionFoldsLinksSharingOneCreatedIssue(t *testing.T) {
	makeLink := func(ordinal int, related string) spanNode {
		n := compositionNode(ordinal, "create_issue_link", nil,
			model.SpanInput{Key: "inward_issue_key", Type: trace.SlotID, Source: "prior_result", FromCall: 1},
			model.SpanInput{Key: "outward_issue_key", Type: trace.SlotID, Source: "caller"})
		n.steps[0].Slots = append(n.steps[0].Slots,
			trace.Slot{Key: "inward_issue_key", Type: trace.SlotID, Value: "TENG-1"},
			trace.Slot{Key: "outward_issue_key", Type: trace.SlotID, Value: related})
		return n
	}
	created := compositionNode(1, "create_issue", nil)
	created.call.OutIDs = []string{"TENG-1"}
	one := spanComposition([]spanNode{created, makeLink(2, "TENG-2")}, []int{0, 1})
	many := spanComposition([]spanNode{created, makeLink(2, "TENG-2"), makeLink(3, "TENG-3")}, []int{0, 1, 2})
	if !reflect.DeepEqual(many.Actions, one.Actions) || len(many.Repetition) != 1 || many.Repetition[0].Kind != "for_each" {
		t.Fatalf("one created issue linking distinct related IDs should be a loop: one=%+v many=%+v", one, many)
	}
	a, _, _ := logicShape(one)
	b, _, _ := logicShape(many)
	if a != b {
		t.Fatalf("one and many links should share a primitive identity: one=%+v many=%+v", one, many)
	}
}

func TestSpanCompositionParameterizesIssueTypeAndNestedGatewayKey(t *testing.T) {
	direct := []spanNode{
		compositionNode(1, "create_issue", nil),
		compositionNode(2, "transition_issue", nil, model.SpanInput{Key: "issue_key", Type: trace.SlotID, Source: "prior_result", FromCall: 1}),
	}
	nested := []spanNode{
		compositionNode(1, "create_issue", map[string]string{"issue_type": "Task"}),
		compositionNode(2, "transition_issue", nil, model.SpanInput{Key: "params/issue_key", Type: trace.SlotID, Source: "prior_result", FromCall: 1}),
	}
	if a, b := spanComposition(direct, []int{0, 1}), spanComposition(nested, []int{0, 1}); a.Key != b.Key {
		t.Fatalf("equivalent direct/nested flow or variable issue type split: %q vs %q", a.Key, b.Key)
	}
}

func TestSpanCompositionSeparatesExplicitAuthorityScope(t *testing.T) {
	makeNode := func(scope string) spanNode {
		return spanNode{ordinal: 1, call: trace.Call{Tool: "shell"}, label: "sh:kubectl get", steps: []trace.Step{{Label: "sh:kubectl get", Slots: []trace.Slot{{Key: "--context=", Value: scope, Type: trace.SlotWord}}}}}
	}
	prod := spanComposition([]spanNode{makeNode("prod")}, []int{0})
	stage := spanComposition([]spanNode{makeNode("staging")}, []int{0})
	if prod.Key == stage.Key || !strings.Contains(prod.Key, "prod") {
		t.Fatalf("explicit cluster context was erased: %+v / %+v", prod, stage)
	}
}
