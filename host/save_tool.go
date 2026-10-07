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
	"github.com/Telara-Labs/TAP-Runtime/discover/pack"
)

// saveTool saves a package an agent wrote into the person's TAP collection.
// An agent's shell may not be able to write there: Codex runs commands in a
// sandbox that can write only inside the workspace, and the save failed with
// a read-only file system. This server is started by the client outside that
// sandbox, so it saves, but only after the person agrees in the client's own
// prompt; a client that cannot show one is told to run tap discover save.
var saveTool = localTool("tap_save", "Save a primitive package folder you wrote (primitive.yaml, versioned CHANGELOG.md, program, AUTHORING.json; compiled packages also need current BUILD.json) to the person's TAP collection, so later sessions find and run it. The person is asked to agree first. Use it when the tap-author skill says to save, and always where your shell cannot write outside the workspace.", map[string]any{
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
	// Freeze the reviewed bytes before asking. An agent editing its draft while
	// the prompt is open must not silently replace the approved program.
	archive, _, err := author.PackageDir(dir)
	if err != nil {
		s.toolError(id, err.Error())
		return
	}
	snapshot, err := os.MkdirTemp("", "tap-save-review-")
	if err != nil {
		s.toolError(id, err.Error())
		return
	}
	defer os.RemoveAll(snapshot)
	if err := pack.Unpack(archive, snapshot); err != nil {
		s.toolError(id, err.Error())
		return
	}
	root, err := pack.CollectionDir()
	if err != nil {
		s.toolError(id, err.Error())
		return
	}
	if _, err := author.CheckPackage(snapshot, root); err != nil {
		s.toolError(id, err.Error()+"; fix it and call tap_save again")
		return
	}
	m, err := mf.Load(snapshot)
	if err != nil {
		s.toolError(id, err.Error())
		return
	}
	ref := m.Metadata.Publisher + "/" + m.Metadata.Name + "@" + m.Metadata.Version
	s.mu.Lock()
	client := clientFor(s.clientName)
	s.mu.Unlock()
	if why := unlendableTools(client, m); why != "" {
		s.toolError(id, why+"; fix it and call tap_save again")
		return
	}
	if !canElicit {
		s.toolError(id, fmt.Sprintf("this client cannot ask the person to agree; they can save it with: %s discover save %s", runnerCommand(), dir))
		return
	}
	if !s.confirmSave(ref, dir, declares(m)) {
		s.toolError(id, "the person did not agree to save "+ref)
		return
	}
	var out, errOut bytes.Buffer
	code := discover.Command([]string{"save", snapshot}, strings.NewReader(""), &out, &errOut)
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
		"message": fmt.Sprintf("Save the primitive %s from a reviewed snapshot of %s to your TAP collection, so later sessions can find and run it? It declares:\n\n%s\n\nRunning it later still asks before any change it makes.", ref, dir, declared),
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
