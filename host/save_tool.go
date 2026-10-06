package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
	"github.com/Telara-Labs/TAP-Runtime/discover"
	"github.com/Telara-Labs/TAP-Runtime/discover/author"
)

// saveTool saves a package an agent wrote into the person's TAP collection.
// An agent's shell may not be able to write there: Codex runs commands in a
// sandbox that can write only inside the workspace, and the save failed with
// a read-only file system. This server is started by the client outside that
// sandbox, so it saves, but only after the person agrees in the client's own
// prompt; a client that cannot show one is told to run tap discover save.
var saveTool = localTool("tap_save", "Save a primitive package folder you wrote (primitive.yaml, program, AUTHORING.json) to the person's TAP collection, so later sessions find and run it. The person is asked to agree first. Use it when the tap-author skill says to save, and always where your shell cannot write outside the workspace.", map[string]any{
	"package": map[string]any{"type": "string", "description": "Absolute path of the package folder."},
}, []string{"package"}, false)

func (s *server) handleSave(id *json.RawMessage, raw json.RawMessage, canElicit bool) {
	var a struct {
		Package string `json:"package"`
	}
	if err := readToolArgs(raw, &a); err != nil || a.Package == "" {
		s.toolError(id, "tap_save needs package, the absolute path of the package folder")
		return
	}
	dir := filepath.Clean(a.Package)
	if !filepath.IsAbs(dir) {
		s.toolError(id, "tap_save needs an absolute package path")
		return
	}
	m, err := mf.Load(dir)
	if err != nil {
		s.toolError(id, fmt.Sprintf("%s is not a primitive package: %v", dir, err))
		return
	}
	if _, err := os.Stat(filepath.Join(dir, "AUTHORING.json")); err != nil {
		s.toolError(id, "the package has no AUTHORING.json; the tap-author skill says how to write it from tap discover brief")
		return
	}
	// Checked before the person is asked, so they are never asked to agree
	// to a save that then fails (Codex's first save did).
	if _, err := author.ReadAuthoring(dir); err != nil {
		s.toolError(id, err.Error()+"; fix it and call tap_save again")
		return
	}
	ref := m.Metadata.Publisher + "/" + m.Metadata.Name + "@" + m.Metadata.Version
	if !canElicit {
		s.toolError(id, fmt.Sprintf("this client cannot ask the person to agree; they can save it with: %s discover save %s", runnerCommand(), dir))
		return
	}
	if !s.confirmSave(ref, dir, declares(m)) {
		s.toolError(id, "the person did not agree to save "+ref)
		return
	}
	var out, errOut bytes.Buffer
	code := discover.Command([]string{"save", dir}, strings.NewReader(""), &out, &errOut)
	text := strings.TrimSpace(out.String() + "\n" + errOut.String())
	if code != 0 {
		s.toolError(id, "saving failed: "+text)
		return
	}
	s.reply(id, map[string]any{"content": []any{map[string]any{"type": "text", "text": text + "\nFind it with tap_search and run it with tap_run."}}})
}

// confirmSave asks the person whether to save a package. Anything but an
// explicit yes is a no.
func (s *server) confirmSave(ref, dir, declared string) bool {
	if strings.TrimSpace(declared) == "" {
		declared = "nothing beyond computing its output"
	}
	m, ok := s.ask("elicitation/create", map[string]any{
		"message": fmt.Sprintf("Save the primitive %s from %s to your TAP collection, so later sessions can find and run it? It declares:\n\n%s\n\nRunning it later still asks before any change it makes.", ref, dir, declared),
		"requestedSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"approve": map[string]any{"type": "boolean", "title": "Save this primitive", "default": false}},
			"required":   []string{"approve"},
		},
	})
	if !ok || m.Error != nil {
		return false
	}
	var r struct {
		Action  string `json:"action"`
		Content struct {
			Approve bool `json:"approve"`
		} `json:"content"`
	}
	return json.Unmarshal(m.Result, &r) == nil && r.Action == "accept" && r.Content.Approve
}
