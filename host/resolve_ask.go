package main

import (
	"encoding/json"
	"strings"
)

// toolAsker is how a run started through tap_run asks its client which tool
// fills a capability: an elicitation, when the client takes one. A client
// that takes none is asked in the refusal instead (blockedError).
func (s *server) toolAsker(canElicit bool) ToolAsker {
	if !canElicit {
		return nil
	}
	return s.askTool
}

// askTool asks one question with one free-text answer: a tool name, or
// none. It offers no list to pick from. The answer is checked by the caller
// before anything binds; declining, cancelling or "none" is no answer.
func (s *server) askTool(q ToolQuestion) (string, bool) {
	m, ok := s.ask("elicitation/create", map[string]any{
		"message": q.Text() + " The answer is checked before it is used and kept on this machine for " + q.Client + ".\n\nNothing is done until you answer.",
		"requestedSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tool": map[string]any{"type": "string", "title": "Tool name, or none"},
			},
			"required": []string{"tool"},
		},
	})
	if !ok || m.Error != nil {
		return "", false
	}
	var r struct {
		Action  string `json:"action"`
		Content struct {
			Tool string `json:"tool"`
		} `json:"content"`
	}
	if json.Unmarshal(m.Result, &r) != nil || r.Action != "accept" {
		return "", false
	}
	t := strings.TrimSpace(r.Content.Tool)
	if t == "" || strings.EqualFold(t, "none") {
		return "", false
	}
	return t, true
}
