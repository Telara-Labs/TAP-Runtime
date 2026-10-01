package discover

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/pack"

	"gitlab.com/telara-labs/tap-runtime/discover/pyparse"

	"gitlab.com/telara-labs/tap-runtime/contract/manifest"
)

//go:embed inline_file_replace_ast.py
var inlineFileReplaceAST []byte

// This parser reads code as data. It never executes a recorded script and
// returns no recorded path or replacement text to the compiler.
func strictInlineFileReplacePy(body string) bool {
	if len(body) > 16<<10 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", "-c", string(inlineFileReplaceAST))
	cmd.Stdin = strings.NewReader(body)
	var out bytes.Buffer
	cmd.Stdout = &out
	if cmd.Run() != nil {
		return false
	}
	return out.String() == "yes\n"
}

func synthesizeInlineFileReplace(g *ProgramGraph, traces []observedTrace) {
	shape := traces[0].span.CodeShape
	sessions := map[string]bool{}
	embedded := false
	for _, tr := range traces {
		if tr.span.CodeShape != shape || len(tr.groups) != 1 || len(tr.groups[0]) != 1 {
			g.Problems = append(g.Problems, "inline code shapes or source call boundaries differ")
			return
		}
		body, isEmbedded, ok := pyparse.InlinePythonBody(tr.groups[0][0].node.call.Command)
		if !ok || !pyparse.StrictInlineFileReplace(body) {
			g.Problems = append(g.Problems, "inline Python is not a proved same-file four-statement read/replace/write")
			return
		}
		sessions[tr.span.Client+"\x00"+tr.span.Session] = true
		embedded = embedded || isEmbedded
	}
	if len(sessions) < 2 {
		g.Problems = append(g.Problems, "inline file transform has fewer than two independent sessions")
		return
	}
	g.InlineFileReplace = &InlineFileReplace{Embedded: embedded}
	g.Inputs = []ProgramInput{
		{Name: "file_path", Type: "string", Source: "caller; same path is read and written"},
		{Name: "old", Type: "string", Source: "caller; nonempty search text"},
		{Name: "new", Type: "string", Source: "caller; replacement text"},
	}
	g.Steps = []ProgramStep{
		{Role: "read file", Tool: "tap.read", Effect: "read", Args: []ProgramArg{{Path: []string{"path"}, Value: ProgramValue{Kind: "input", Input: "file_path"}}}},
		{Role: "replace text", Tool: "python.str.replace", Effect: "none", Args: []ProgramArg{{Path: []string{"old"}, Value: ProgramValue{Kind: "input", Input: "old"}}, {Path: []string{"new"}, Value: ProgramValue{Kind: "input", Input: "new"}}}},
		{Role: "write same file", Tool: "tap.write", Effect: "write", Args: []ProgramArg{{Path: []string{"path"}, Value: ProgramValue{Kind: "input", Input: "file_path"}}}},
	}
}

func generateInlineFileReplace(g *ProgramGraph) (*GeneratedPackage, error) {
	if len(g.Steps) != 3 || len(g.Inputs) != 3 || g.InlineFileReplace == nil {
		return nil, fmt.Errorf("invalid inline file replacement graph")
	}
	name := "discovered-" + strings.TrimPrefix(g.CandidateID, "lc_")
	if !pack.SkillName.MatchString(name) {
		return nil, fmt.Errorf("candidate %q cannot name a package", g.CandidateID)
	}
	m := &manifest.Manifest{APIVersion: manifest.APIVersion, Kind: "Primitive",
		Metadata:  manifest.Metadata{Publisher: "local.discover", Name: name, Version: "0.1.0", Description: "Replace text in a caller-selected file under the invocation directory."},
		Execution: manifest.Execution{Runtime: manifest.RuntimeWasm, Entrypoint: "main.py"},
		Files:     []manifest.File{{Path: ".", Access: "write"}},
		Interface: &manifest.Interface{InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"file_path": map[string]any{"type": "string"}, "old": map[string]any{"type": "string"}, "new": map[string]any{"type": "string"}},
			"required": []string{"file_path", "old", "new"}, "additionalProperties": false}, OutputSchema: map[string]any{"type": "object"}},
	}
	code := `import json
import sys

if len(sys.argv) != 2:
    raise ValueError('pass one JSON object of typed inputs')
inputs = json.loads(sys.argv[1])
if not isinstance(inputs, dict) or set(inputs) != {'file_path', 'old', 'new'}:
    raise ValueError('file_path, old and new are required')
if not all(isinstance(inputs[key], str) for key in ('file_path', 'old', 'new')):
    raise ValueError('all inputs must be strings')
if not inputs['file_path'] or not inputs['old']:
    raise ValueError('file_path and old must be nonempty')
before = tap.read(inputs['file_path'])
if '\ufffd' in before:
    raise ValueError('file is not valid UTF-8')
after = before.replace(inputs['old'], inputs['new'])
tap.write(inputs['file_path'], after)
print(json.dumps({'path': inputs['file_path'], 'replacements': before.count(inputs['old'])}))
`
	readme := "# Discovered file replacement\n\nThis exact inner Python pattern was observed in " + fmt.Sprint(g.Executions) + " execution(s) across " + fmt.Sprint(g.Sessions) + " sessions. It reads one caller-selected text file, replaces every occurrence of nonempty `old` with `new`, and writes the same file. The runtime grants this package file write reach under the invocation working directory (`files: .`); each write still needs runtime approval. It cannot read or write a path outside that directory.\n\nInputs: `file_path`, `old`, `new` (strings). Output: path and replacement count. This code does not run shell prefixes, suffixes, tests, git operations, or any other surrounding commands. It is a generic text replacement; source recurrence alone does not prove user-task usefulness or savings. Review the exact code and reach before private acceptance.\n"
	if g.InlineFileReplace.Embedded {
		readme += "\nEvery supporting snippet was embedded in a larger shell call; only the inner file transform is generated.\n"
	}
	files := map[string][]byte{"primitive.yaml": m.YAML(), "main.py": []byte(code), "README.md": []byte(readme)}
	_, digest, err := pack.PackFiles(files, func(string) bool { return false })
	if err != nil {
		return nil, err
	}
	return &GeneratedPackage{Graph: g, Manifest: m, Files: files, Digest: digest}, nil
}
