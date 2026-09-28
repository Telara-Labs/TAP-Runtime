package compile

// proto.go is the thin adapter that lifts the compiler's JSON-dialect
// Definition onto the REAL published proto message
// (gitlab.com/telara-labs/telara-proto agents.agent_service.WorkflowDefinition,
// v0.0.0-20260714021153-e7532666f779). The proto module is already a CLI
// dependency (cmd/tap/publish.go's gRPC client), so this adds no new module —
// only a field-by-field mapping. Marshaling the result with protojson is the
// point of `tap compile --proto`: every field the compiler emits must land on
// a real proto field, so a rename/removal in the proto surfaces here as a Go
// compile error rather than as silent drift discovered at wire time.

import (
	"fmt"

	agentspb "gitlab.com/telara-labs/telara-proto/protos-go/agents/agent_service"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
)

// ToProto converts a compiled Definition into the wire proto message.
func ToProto(def *Definition) (*agentspb.WorkflowDefinition, error) {
	if def == nil {
		return nil, fmt.Errorf("nil definition")
	}
	out := &agentspb.WorkflowDefinition{
		Name:        def.Name,
		Description: def.Description,
		EntryNodeId: def.EntryNodeID,
		Metadata:    def.Metadata,
	}
	for i := range def.Nodes {
		n, err := nodeToProto(&def.Nodes[i])
		if err != nil {
			return nil, fmt.Errorf("node %q: %w", def.Nodes[i].ID, err)
		}
		out.Nodes = append(out.Nodes, n)
	}
	for i := range def.Edges {
		e := def.Edges[i]
		out.Edges = append(out.Edges, &agentspb.WorkflowEdge{
			Id:                  e.ID,
			FromNodeId:          e.FromNodeID,
			ToNodeId:            e.ToNodeID,
			ConditionExpression: e.ConditionExpression,
			DefaultEdge:         e.DefaultEdge,
			Metadata:            e.Metadata,
		})
	}
	for i := range def.Inputs {
		in := def.Inputs[i]
		dv, err := toValue(in.DefaultValue)
		if err != nil {
			return nil, fmt.Errorf("input %q default_value: %w", in.Name, err)
		}
		out.Inputs = append(out.Inputs, &agentspb.WorkflowInput{
			Name:         in.Name,
			Type:         in.Type,
			Description:  in.Description,
			Required:     in.Required,
			DefaultValue: dv,
		})
	}
	for i := range def.Outputs {
		o := def.Outputs[i]
		out.Outputs = append(out.Outputs, &agentspb.WorkflowOutput{
			Name:             o.Name,
			Type:             o.Type,
			Description:      o.Description,
			SourceExpression: o.SourceExpression,
		})
	}
	return out, nil
}

func nodeToProto(n *Node) (*agentspb.WorkflowNode, error) {
	nt, err := nodeTypeToProto(n.Type)
	if err != nil {
		return nil, err
	}
	pn := &agentspb.WorkflowNode{
		Id:             n.ID,
		Name:           n.Name,
		Type:           nt,
		Description:    n.Description,
		TimeoutSeconds: int32(n.TimeoutSeconds),
		EffectClass:    n.EffectClass,
		Metadata:       n.Metadata,
	}
	for i := range n.InputBindings {
		b, err := bindingToProto(n.InputBindings[i])
		if err != nil {
			return nil, err
		}
		pn.InputBindings = append(pn.InputBindings, b)
	}
	for i := range n.OutputBindings {
		b, err := bindingToProto(n.OutputBindings[i])
		if err != nil {
			return nil, err
		}
		pn.OutputBindings = append(pn.OutputBindings, b)
	}
	if n.Tool != nil {
		sp, err := toStruct(n.Tool.StaticParameters)
		if err != nil {
			return nil, fmt.Errorf("tool.static_parameters: %w", err)
		}
		pn.Tool = &agentspb.ToolNodeConfig{
			IntegrationType:  n.Tool.IntegrationType,
			ToolName:         n.Tool.ToolName,
			CredentialId:     n.Tool.CredentialID,
			StaticParameters: sp,
		}
	}
	if n.Model != nil {
		os, err := toStruct(n.Model.OutputSchema)
		if err != nil {
			return nil, fmt.Errorf("model.output_schema: %w", err)
		}
		pn.Model = &agentspb.ModelNodeConfig{
			Prompt:          n.Model.Prompt,
			Task:            n.Model.Task,
			OutputSchema:    os,
			MaxIterations:   int32(n.Model.MaxIterations),
			RoutingProfile:  n.Model.RoutingProfile,
			LeaseConfigJson: n.Model.LeaseConfigJSON,
		}
	}
	if n.Transform != nil {
		pn.Transform = &agentspb.TransformNodeConfig{
			Language: n.Transform.Language,
			Program:  n.Transform.Program,
		}
	}
	if n.Branch != nil {
		pn.Branch = &agentspb.BranchNodeConfig{
			ConditionExpression: n.Branch.ConditionExpression,
			DefaultNodeId:       n.Branch.DefaultNodeID,
		}
	}
	if n.Approval != nil {
		pn.Approval = &agentspb.ApprovalNodeConfig{
			Prompt:         n.Approval.Prompt,
			TimeoutMinutes: int32(n.Approval.TimeoutMinutes),
		}
	}
	if n.Subflow != nil {
		sub := &agentspb.SubflowNodeConfig{
			WorkflowDefinitionId: n.Subflow.WorkflowDefinitionID,
		}
		if n.Subflow.WorkflowDefinition != nil {
			nested, err := ToProto(n.Subflow.WorkflowDefinition)
			if err != nil {
				return nil, fmt.Errorf("subflow.workflow_definition: %w", err)
			}
			sub.WorkflowDefinition = nested
		}
		pn.Subflow = sub
	}
	if n.RetryPolicy != nil {
		pn.RetryPolicy = &agentspb.RetryPolicy{
			MaxAttempts:           int32(n.RetryPolicy.MaxAttempts),
			InitialBackoffSeconds: int32(n.RetryPolicy.InitialBackoffSeconds),
			RetryOn:               n.RetryPolicy.RetryOn,
		}
	}
	return pn, nil
}

func bindingToProto(b Binding) (*agentspb.WorkflowBinding, error) {
	lv, err := toValue(b.LiteralValue)
	if err != nil {
		return nil, fmt.Errorf("binding %q literal_value: %w", b.Name, err)
	}
	return &agentspb.WorkflowBinding{
		Name:             b.Name,
		TargetPath:       b.TargetPath,
		SourceExpression: b.SourceExpression,
		LiteralValue:     lv,
		Required:         b.Required,
	}, nil
}

func nodeTypeToProto(t NodeType) (agentspb.WorkflowNodeType, error) {
	switch t {
	case NodeTool:
		return agentspb.WorkflowNodeType_WORKFLOW_NODE_TYPE_TOOL, nil
	case NodeModel:
		return agentspb.WorkflowNodeType_WORKFLOW_NODE_TYPE_MODEL, nil
	case NodeTransform:
		return agentspb.WorkflowNodeType_WORKFLOW_NODE_TYPE_TRANSFORM, nil
	case NodeBranch:
		return agentspb.WorkflowNodeType_WORKFLOW_NODE_TYPE_BRANCH, nil
	case NodeApproval:
		return agentspb.WorkflowNodeType_WORKFLOW_NODE_TYPE_APPROVAL, nil
	case NodeSubflow:
		return agentspb.WorkflowNodeType_WORKFLOW_NODE_TYPE_SUBFLOW, nil
	case NodeJoin:
		return agentspb.WorkflowNodeType_WORKFLOW_NODE_TYPE_JOIN, nil
	case NodeNoop:
		return agentspb.WorkflowNodeType_WORKFLOW_NODE_TYPE_NOOP, nil
	case NodeEnd:
		return agentspb.WorkflowNodeType_WORKFLOW_NODE_TYPE_END, nil
	default:
		return agentspb.WorkflowNodeType_WORKFLOW_NODE_TYPE_UNSPECIFIED, fmt.Errorf("unmapped node type %q", t)
	}
}

func toStruct(m map[string]interface{}) (*structpb.Struct, error) {
	if m == nil {
		return nil, nil
	}
	return structpb.NewStruct(m)
}

func toValue(v interface{}) (*structpb.Value, error) {
	if v == nil {
		return nil, nil
	}
	return structpb.NewValue(v)
}

// MarshalProtoJSON renders the Definition through the real proto message using
// protojson (deterministic, 2-space indent). This is what `tap compile --proto`
// emits.
func MarshalProtoJSON(def *Definition) ([]byte, error) {
	msg, err := ToProto(def)
	if err != nil {
		return nil, err
	}
	return protojson.MarshalOptions{Multiline: true, Indent: "  ", UseProtoNames: true}.Marshal(msg)
}
