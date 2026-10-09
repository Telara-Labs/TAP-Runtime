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
	// A draft under the workspace's .tap/drafts stays out of git, whichever
	// way it is saved.
	if err := author.IgnoreDrafts(dir, false); err != nil {
		s.toolError(id, err.Error())
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
		// Gemini CLI could not save at all: TAP could not ask, and its
		// policy denied the tap discover save command TAP pointed it to.
		// The person's yes is given to the agent, as for any file it
		// writes: an agent whose own settings let it write files unasked
		// saves here, and running the package later is still gated.
		home, _ := os.UserHomeDir()
		s.mu.Lock()
		pid, name := s.agentPid, s.clientName
		s.mu.Unlock()
		// Gemini CLI asks the person itself when TAP's hook requires it
		// (gemini_confirm.go); the call arriving is their yes.
		confirmed, notConfirmed := s.geminiConfirmed("tap_save", raw)
		ok, why := confirmed, "the person allowed it in Gemini CLI's own confirmation"
		if !ok {
			ok, why = agentDoesUnasked(name, home, unaskedWrite, pid)
			if notConfirmed != "" {
				why = notConfirmed + ", and " + why
			}
		}
		if !ok {
			s.toolError(id, fmt.Sprintf("this client cannot ask the person to agree, and %s; they can save it with: %s discover save %s", why, runnerCommand(), dir))
			return
		}
		logf("save       %s without a TAP prompt: %s", ref, why)
	} else if why := s.confirmSave(ref, dir, declares(m)); why != "" {
		s.toolError(id, why+" "+ref)
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

// confirmSave requests a confirmation, not extra form data. The client's
// permission policy may authorize a local save; decline and cancel still
// save nothing. Execution retains its own package and effect gates.
func (s *server) confirmSave(ref, dir, declared string) string {
	if strings.TrimSpace(declared) == "" {
		declared = "nothing beyond computing its output"
	}
	m, ok := s.ask("elicitation/create", map[string]any{
		"message":         fmt.Sprintf("Save the primitive %s from a reviewed snapshot of %s to your TAP collection, so later sessions can find and run it? It declares:\n\n%s\n\nRunning it later still asks before any change it makes.", ref, dir, declared),
		"requestedSchema": map[string]any{"type": "object", "properties": map[string]any{}},
	})
	if !ok || m.Error != nil {
		return "the client could not complete the save confirmation for"
	}
	var r struct {
		Action  string         `json:"action"`
		Content map[string]any `json:"content"`
	}
	if json.Unmarshal(m.Result, &r) != nil {
		return "the client returned an invalid save confirmation for"
	}
	switch r.Action {
	case "accept":
		// Preserve refusal from clients answering the old checkbox schema.
		if approve, present := r.Content["approve"]; present && approve != true {
			return "the client declined the save confirmation for"
		}
		return ""
	case "decline":
		return "the client declined the save confirmation for"
	case "cancel":
		return "the client canceled the save confirmation for"
	default:
		return "the client returned an invalid save confirmation for"
	}
}
