package compile

import (
	"testing"

	agentspb "gitlab.com/telara-labs/telara-proto/protos-go/agents/agent_service"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"telara.dev/tap/internal/model"
)

// TestProto_RoundTripCorpus proves the compiler's JSON dialect lifts cleanly
// onto the published proto message and survives a protojson round-trip with
// zero loss: compile -> ToProto -> protojson.Marshal -> protojson.Unmarshal ->
// proto.Equal. A rename/removal in telara-proto breaks ToProto at Go compile
// time; a semantic drift (a field the marshaler drops, an enum that doesn't
// re-parse) breaks proto.Equal here. Runs over the whole corpus; asserts at
// least two packages actually exercised the path.
func TestProto_RoundTripCorpus(t *testing.T) {
	pkgs := corpus(t)
	exercised := 0
	for _, p := range pkgs {
		t.Run(p.name, func(t *testing.T) {
			pkg, err := model.LoadPackage(p.dir)
			if err != nil {
				t.Fatalf("load package: %v", err)
			}
			def, cf, err := Compile(pkg)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			if cf.HasErrors() {
				t.Fatalf("compile errors: %v", cf.Errors())
			}

			// Dialect -> proto message.
			msg, err := ToProto(def)
			if err != nil {
				t.Fatalf("ToProto: %v", err)
			}
			if len(msg.GetNodes()) != len(def.Nodes) {
				t.Fatalf("node count drift: proto has %d, dialect has %d", len(msg.GetNodes()), len(def.Nodes))
			}

			// Marshal via protojson, then parse back into a fresh message.
			data, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(msg)
			if err != nil {
				t.Fatalf("protojson.Marshal: %v", err)
			}
			var back agentspb.WorkflowDefinition
			if err := protojson.Unmarshal(data, &back); err != nil {
				t.Fatalf("protojson.Unmarshal: %v\npayload: %s", err, string(data))
			}
			if !proto.Equal(msg, &back) {
				t.Fatalf("round-trip not equal for %s", p.name)
			}
		})
		exercised++
	}
	if exercised < 2 {
		t.Fatalf("round-trip exercised only %d package(s); need >= 2", exercised)
	}
}

// TestProto_TransformNodePreserved specifically asserts that a compiled
// TRANSFORM node keeps its enum + config across the proto round-trip (the B1
// addition this whole wire-up exists for).
func TestProto_TransformNodePreserved(t *testing.T) {
	def := &Definition{
		Name:        "rt-transform",
		EntryNodeID: "t1",
		Nodes: []Node{
			{
				ID:   "t1",
				Type: NodeTransform,
				Transform: &TransformConfig{
					Language: "starlark",
					Program:  "result = {'x': 1}",
				},
			},
		},
	}
	msg, err := ToProto(def)
	if err != nil {
		t.Fatalf("ToProto: %v", err)
	}
	data, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back agentspb.WorkflowDefinition
	if err := protojson.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back.GetNodes()) != 1 {
		t.Fatalf("expected 1 node, got %d", len(back.GetNodes()))
	}
	n := back.GetNodes()[0]
	if n.GetType() != agentspb.WorkflowNodeType_WORKFLOW_NODE_TYPE_TRANSFORM {
		t.Fatalf("type not preserved: %v", n.GetType())
	}
	if n.GetTransform().GetLanguage() != "starlark" || n.GetTransform().GetProgram() != "result = {'x': 1}" {
		t.Fatalf("transform config not preserved: %+v", n.GetTransform())
	}
}
