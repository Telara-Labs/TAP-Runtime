package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Windsurf keeps a conversation's transcript only while a
// post_cascade_response_with_transcript hook is configured, and only the 100
// newest (docs.devin.ai/desktop/cascade/hooks). `tap hook windsurf` is that
// hook: it copies each transcript Windsurf names on standard input to
// ~/.tap/windsurf/transcripts, where discover reads it after
// Windsurf has pruned its own copy. It never blocks Windsurf: any problem
// is reported on standard error and the hook exits 0.

// windsurfTranscriptHook is the hook entry tap install adds to
// ~/.codeium/windsurf/hooks.json.
const windsurfTranscriptHook = "post_cascade_response_with_transcript"

func windsurfHook(stdin io.Reader, stderr io.Writer, home string) int {
	var in struct {
		Action   string `json:"agent_action_name"`
		ToolInfo struct {
			TranscriptPath string `json:"transcript_path"`
		} `json:"tool_info"`
	}
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&in); err != nil {
		fmt.Fprintln(stderr, "tap hook windsurf:", err)
		return 0
	}
	if in.Action != windsurfTranscriptHook {
		return 0
	}
	if err := archiveWindsurfTranscript(in.ToolInfo.TranscriptPath, home); err != nil {
		fmt.Fprintln(stderr, "tap hook windsurf:", err)
	}
	return 0
}

// archiveWindsurfTranscript copies one transcript, which must be a regular
// .jsonl file in Windsurf's own transcripts folder.
func archiveWindsurfTranscript(path, home string) error {
	src := filepath.Join(home, ".windsurf", "transcripts")
	clean := filepath.Clean(path)
	if filepath.Dir(clean) != src || !strings.HasSuffix(clean, ".jsonl") {
		return fmt.Errorf("%s is not a Windsurf transcript", path)
	}
	info, err := os.Lstat(clean)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	b, err := os.ReadFile(clean)
	if err != nil {
		return err
	}
	dst := filepath.Join(home, ".tap", "windsurf", "transcripts")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dst, ".transcript-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dst, filepath.Base(clean)))
}

// addWindsurfHook adds the transcript hook to Windsurf's hooks.json (or
// removes it), keeping every other hook. Running it twice leaves one entry.
func addWindsurfHook(path, self string, remove bool) (bool, error) {
	command := shellQuote(self) + " hook windsurf"
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	doc := map[string]any{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return false, fmt.Errorf("%s is not plain JSON (%v); it was left unchanged", path, err)
		}
	}
	hooks, _ := doc["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	var kept []any
	present := false
	list, _ := hooks[windsurfTranscriptHook].([]any)
	for _, h := range list {
		if m, ok := h.(map[string]any); ok && strings.HasSuffix(fmt.Sprint(m["command"]), " hook windsurf") {
			present = true
			continue
		}
		kept = append(kept, h)
	}
	if remove {
		if !present {
			return false, nil
		}
	} else {
		if present && len(kept) == len(list)-1 {
			for _, h := range list {
				if m, ok := h.(map[string]any); ok && m["command"] == command {
					return false, nil // already there, pointing at this runner
				}
			}
		}
		kept = append(kept, map[string]any{"command": command, "show_output": false})
	}
	if len(kept) == 0 {
		delete(hooks, windsurfTranscriptHook)
	} else {
		hooks[windsurfTranscriptHook] = kept
	}
	doc["hooks"] = hooks
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if len(raw) > 0 {
		if _, err := os.Stat(path + ".tap-backup"); os.IsNotExist(err) {
			if err := os.WriteFile(path+".tap-backup", raw, 0o600); err != nil {
				return false, err
			}
		}
	}
	return true, os.WriteFile(path, append(out, '\n'), 0o600)
}
