// Package compile implements `tap compile`: it turns a validated TAP
// workflow.yaml (09-workflow-spec.md) into a WorkflowDefinition emitted as the
// lowercase-friendly JSON dialect (the Go-model tag form used by
// telara-orchestrator/services/workflow-engine and telara-interface/src/types/
// agents.ts). It deliberately does NOT import telara-proto: the field names
// below are the documented wire names, which keeps the compiler unblocked by
// the pending proto publish (STATUS.md B1 checkpoint) while staying byte-for-
// byte compatible with the executor's own JSON unmarshal path.
//
// The types here mirror workflow_graph.go's JSON tags exactly for every field
// the executor consumes, and add a small number of forward-looking keys
// (transform node config, model routing_profile / lease_config_json, node
// effect_class) named per B1's already-merged proto additions so that a
// proto-native re-emission later is a rename, not a redesign. Unknown keys are
// ignored by the executor's json.Unmarshal, so the extra keys never break
// today's runtime.
package compile

// Definition is the compiled WorkflowDefinition (lowercase dialect).
type Definition struct {
	Name        string            `json:"name,omitempty"`
	Description string            `json:"description,omitempty"`
	EntryNodeID string            `json:"entry_node_id,omitempty"`
	Nodes       []Node            `json:"nodes"`
	Edges       []Edge            `json:"edges,omitempty"`
	Inputs      []Input           `json:"inputs,omitempty"`
	Outputs     []Output          `json:"outputs,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// NodeType values match models.WorkflowNodeType (workflow_graph.go).
type NodeType string

const (
	NodeTool      NodeType = "tool"
	NodeModel     NodeType = "model"
	NodeTransform NodeType = "transform" // B1 proto enum 11 / §7 T1 target
	NodeBranch    NodeType = "branch"
	NodeApproval  NodeType = "approval"
	NodeSubflow   NodeType = "subflow"
	NodeJoin      NodeType = "join"
	NodeNoop      NodeType = "noop"
	NodeEnd       NodeType = "end"
)

type Node struct {
	ID             string            `json:"id"`
	Name           string            `json:"name,omitempty"`
	Type           NodeType          `json:"type"`
	Description    string            `json:"description,omitempty"`
	InputBindings  []Binding         `json:"input_bindings,omitempty"`
	OutputBindings []Binding         `json:"output_bindings,omitempty"`
	Tool           *ToolConfig       `json:"tool,omitempty"`
	Model          *ModelConfig      `json:"model,omitempty"`
	Transform      *TransformConfig  `json:"transform,omitempty"`
	Branch         *BranchConfig     `json:"branch,omitempty"`
	Approval       *ApprovalConfig   `json:"approval,omitempty"`
	Subflow        *SubflowConfig    `json:"subflow,omitempty"`
	RetryPolicy    *RetryPolicy      `json:"retry_policy,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	EffectClass    string            `json:"effect_class,omitempty"` // B1 proto node field 19
	Metadata       map[string]string `json:"metadata,omitempty"`
}

// Binding matches models.WorkflowBinding. The executor merges static params
// first then applies bindings (bindings override statics), and a binding uses
// literal_value XOR source_expression (never both) per 09 §5.
type Binding struct {
	Name             string      `json:"name,omitempty"`
	TargetPath       string      `json:"target_path,omitempty"`
	SourceExpression string      `json:"source_expression,omitempty"`
	LiteralValue     interface{} `json:"literal_value,omitempty"`
	Required         bool        `json:"required,omitempty"`
}

type Input struct {
	Name         string      `json:"name"`
	Type         string      `json:"type,omitempty"`
	Description  string      `json:"description,omitempty"`
	Required     bool        `json:"required,omitempty"`
	DefaultValue interface{} `json:"default_value,omitempty"`
}

type Output struct {
	Name             string `json:"name"`
	Type             string `json:"type,omitempty"`
	Description      string `json:"description,omitempty"`
	SourceExpression string `json:"source_expression,omitempty"`
}

type Edge struct {
	ID                  string            `json:"id,omitempty"`
	FromNodeID          string            `json:"from_node_id"`
	ToNodeID            string            `json:"to_node_id"`
	ConditionExpression string            `json:"condition_expression,omitempty"`
	DefaultEdge         bool              `json:"default_edge,omitempty"`
	Metadata            map[string]string `json:"metadata,omitempty"`
}

type ToolConfig struct {
	IntegrationType  string                 `json:"integration_type,omitempty"`
	ToolName         string                 `json:"tool_name,omitempty"` // UNPREFIXED (executor adds <integration>_)
	CredentialID     string                 `json:"credential_id,omitempty"`
	StaticParameters map[string]interface{} `json:"static_parameters,omitempty"`
}

// ModelConfig matches models.ModelNodeConfig plus B1's routing_profile (proto
// field 7) and lease_config_json (field 8) — the L1 lease additions. The
// executor ignores the two extra keys today.
type ModelConfig struct {
	Prompt          string                 `json:"prompt,omitempty"`
	Task            string                 `json:"task,omitempty"`
	OutputSchema    map[string]interface{} `json:"output_schema,omitempty"`
	MaxIterations   int                    `json:"max_iterations,omitempty"`
	RoutingProfile  string                 `json:"routing_profile,omitempty"`
	LeaseConfigJSON string                 `json:"lease_config_json,omitempty"`
}

// TransformConfig is the §7 T1 TransformNodeConfig{language, program}. The
// param values arrive as the node's input (static params as literal bindings,
// from: refs as source_expression bindings) exactly like every other node, so
// a shared workflow-engine Starlark runner reuses buildWorkflowNodeInput.
type TransformConfig struct {
	Language string `json:"language,omitempty"`
	Program  string `json:"program,omitempty"`
}

type BranchConfig struct {
	ConditionExpression string `json:"condition_expression,omitempty"`
	DefaultNodeID       string `json:"default_node_id,omitempty"`
}

type ApprovalConfig struct {
	Prompt         string `json:"prompt,omitempty"`
	TimeoutMinutes int    `json:"timeout_minutes,omitempty"`
}

type SubflowConfig struct {
	WorkflowDefinitionID string      `json:"workflow_definition_id,omitempty"`
	WorkflowDefinition   *Definition `json:"workflow_definition,omitempty"`
}

// RetryPolicy matches models.RetryPolicy. retry_on is ALWAYS non-empty in
// compiled output (§6): an empty retry_on retries every error in the executor.
type RetryPolicy struct {
	MaxAttempts           int      `json:"max_attempts,omitempty"`
	InitialBackoffSeconds int      `json:"initial_backoff_seconds,omitempty"`
	RetryOn               []string `json:"retry_on,omitempty"`
}

// DefaultRetryOn is the curated transient list the compiler substitutes when a
// retry: block omits retry_on (09 §4 sharp edge).
var DefaultRetryOn = []string{"timeout", "unavailable", "rate_limit"}
