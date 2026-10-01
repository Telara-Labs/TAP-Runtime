package model

// SpanTaskReview is a deterministic admission check for the human review
// queue. Ready means the recorded trace has a task-shaped contract, not that
// it is useful, safe, or a validated primitive.
type SpanTaskReview struct {
	Ready     bool     `json:"ready"`
	Component bool     `json:"component"`
	Source    string   `json:"source"`
	Input     string   `json:"input"`
	Output    string   `json:"output"`
	Stop      string   `json:"stop"`
	Reasons   []string `json:"reasons,omitempty"`
}
