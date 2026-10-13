package publiccheck

import (
	"bytes"
	"context"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestPublicMarkdownAndSchemaHaveNoInternalReferencesOrAIAttribution(t *testing.T) {
	root := repositoryRoot(t)
	var violations []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && (ignoredDirectory(entry.Name()) || (entry.Name() == "bin" && filepath.Dir(path) == root)) {
				return filepath.SkipDir
			}
			return nil
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".md") || name == "AGENTS.md" || name == "CLAUDE.md" || isFixtureBody(root, path) {
			return nil
		}
		if strings.Contains(path, string(filepath.Separator)+".git"+string(filepath.Separator)) ||
			strings.Contains(path, string(filepath.Separator)+"third_party"+string(filepath.Separator)) {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		violations = append(violations, matches(root, path, markdownProse(body))...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"contract/manifest/manifest.v3.schema.json"} {
		full := filepath.Join(root, path)
		body, err := os.ReadFile(full)
		if err != nil {
			t.Fatal(err)
		}
		violations = append(violations, matches(root, full, body)...)
	}
	if len(violations) != 0 {
		t.Fatalf("public docs contain internal process references or AI coauthor attribution:\n%s", strings.Join(violations, "\n"))
	}
}

func TestGoCommentsHaveNoInternalReferencesOrAIAttribution(t *testing.T) {
	root := repositoryRoot(t)
	var violations []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && (ignoredDirectory(entry.Name()) || (entry.Name() == "bin" && filepath.Dir(path) == root)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), ".pb.go") || isFixtureBody(root, path) {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		for _, group := range file.Comments {
			violations = append(violations, matches(root, path, []byte(group.Text()))...)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("Go comments contain internal process references or AI coauthor attribution:\n%s", strings.Join(violations, "\n"))
	}
}

func TestPublicTextRejectsInternalIdsAndCoauthorTrailers(t *testing.T) {
	for _, text := range []string{
		"This public note mentions TENG-1234.",
		"A 2026 ruling 42 changed the wording.",
		"Co-authored-by: Claude Example <claude@example.invalid>",
		"Co-authored-by: Gemini <gemini@example.invalid>",
		"Co-authored-by: Codex <codex@example.invalid>",
		"Co-authored-by: GPT-6 <gpt@example.invalid>",
		"Claude-Session: https://example.invalid/session/123",
	} {
		if got := matches(".", "sample.md", []byte(text)); len(got) == 0 {
			t.Errorf("matches(%q) = none; want a hygiene violation", text)
		}
	}
}

func TestFixtureBodiesAndLocalArtifactsAreExcluded(t *testing.T) {
	root := filepath.Clean("/repo")
	for _, path := range []string{
		"/repo/discover/history/testdata/aider/work/.aider.chat.history.md",
		"/repo/host/testdata/session.md",
		"/repo/.tmp/export/README.md",
		"/repo/.tap/cache/notes.md",
		"/repo/.cache/tool/README.md",
		"/repo/cache/tool/README.md",
		"/repo/bin/tap",
		"/repo/dist/docs/README.md",
	} {
		if !isFixtureBody(root, path) && strings.Contains(path, "testdata") {
			t.Errorf("isFixtureBody(%q) = false; want fixture exclusion", path)
		}
		if !strings.Contains(path, "testdata") {
			found := false
			for _, part := range strings.Split(filepath.ToSlash(path), "/") {
				found = found || ignoredDirectory(part) || (part == "bin" && filepath.Dir(path) == root+string(filepath.Separator)+"bin")
			}
			if !found {
				t.Errorf("path %q has no ignored artifact directory", path)
			}
		}
	}
	if isFixtureBody(root, "/repo/discover/history/testdata/SYNTHETIC.md") {
		t.Fatal("isFixtureBody(SYNTHETIC.md) = true; want authored fixture note to remain checked")
	}
}

func TestPublicTextAllowsClientNamesAndSyntheticIssueSamples(t *testing.T) {
	text := markdownProse([]byte("Claude Code, Codex, and Gemini CLI are supported clients.\n\n```sh\ntap --client codex examples/triage '{\"jql\":\"key = TENG-1234\"}'\n```\n\nA different query can use `TENG-5678` as a synthetic example value.\n"))
	if got := matches(".", "sample.md", text); len(got) != 0 {
		t.Fatalf("matches(%q) = %v; want synthetic code examples and client names to remain allowed", text, got)
	}
}

func TestCommitMessageGuardCatchesPrivateRefsAndAIAttribution(t *testing.T) {
	for _, message := range []string{
		"TENG-1234: update docs",
		"Record ruling #42",
		"Decision 5: close the question",
		"Research from tap-discover-review-2026-09-29",
		"See https://claude.ai/code/session_123 for details",
		"Co-authored-by: Claude <claude@example.invalid>",
		"Co-authored-by: Gemini <gemini@example.invalid>",
		"Co-authored-by: Codex <codex@example.invalid>",
	} {
		if got := FindForbiddenReferences(message); len(got) == 0 {
			t.Errorf("FindForbiddenReferences(%q) = none; want a violation", message)
		}
	}
	for _, message := range []string{
		"Document the Codex and Gemini integrations",
		"Co-authored-by: Ada Lovelace <ada@example.invalid>",
	} {
		if got := FindForbiddenReferences(message); len(got) != 0 {
			t.Errorf("FindForbiddenReferences(%q) = %v; want no violation", message, got)
		}
	}
}

func TestCommitRangeChecksOnlyIncomingMessagesAndFailsOnMissingBase(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.name", "Public Check Test")
	git(t, repo, "config", "user.email", "public-check@example.invalid")
	file := filepath.Join(repo, "file.txt")
	if err := os.WriteFile(file, []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "file.txt")
	git(t, repo, "commit", "-q", "-m", "Historical TENG-1000 reference")
	base := git(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(file, []byte("clean incoming change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "file.txt")
	git(t, repo, "commit", "-q", "-m", "Document Codex integration", "-m", "Co-authored-by: Ada Lovelace <ada@example.invalid>")
	head := git(t, repo, "rev-parse", "HEAD")
	violations, err := CheckCommitRange(context.Background(), repo, base, head)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("CheckCommitRange() = %v; want historical base excluded", violations)
	}
	if violations, err := CheckCommitRange(context.Background(), repo, head, head); err != nil || len(violations) != 0 {
		t.Fatalf("CheckCommitRange(head, head) = %v, %v; want an accepted empty range", violations, err)
	}
	if _, err := CheckCommitRange(context.Background(), repo, "missing-baseline", head); err == nil {
		t.Fatal("CheckCommitRange() accepted an unresolved baseline")
	}
	if _, err := CheckCommitRange(context.Background(), repo, head, base); err == nil {
		t.Fatal("CheckCommitRange() accepted a reversed range")
	}
	if err := os.WriteFile(file, []byte("incoming violation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "file.txt")
	git(t, repo, "commit", "-q", "-m", "Add integration", "-m", "Co-authored-by: Gemini <gemini@example.invalid>")
	head = git(t, repo, "rev-parse", "HEAD")
	violations, err = CheckCommitRange(context.Background(), repo, base, head)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) == 0 {
		t.Fatal("CheckCommitRange() found no violation in incoming commit message")
	}
	git(t, repo, "switch", "--orphan", "disconnected")
	if err := os.WriteFile(file, []byte("disconnected history\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "file.txt")
	git(t, repo, "commit", "-q", "-m", "Independent root")
	disconnected := git(t, repo, "rev-parse", "HEAD")
	if _, err := CheckCommitRange(context.Background(), repo, base, disconnected); err == nil {
		t.Fatal("CheckCommitRange() accepted a disconnected range")
	}
}

func git(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func ignoredDirectory(name string) bool {
	switch name {
	case ".cache", ".git", ".playwright-mcp", ".tap", ".tmp", "cache", "dist", "node_modules", "out", "third_party", "vendor":
		return true
	default:
		return false
	}
}

func isFixtureBody(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i, part := range parts {
		if part != "testdata" {
			continue
		}
		// Keep the explicitly authored synthetic-fixture note under hygiene checks,
		// while avoiding transcript and payload bodies that intentionally contain
		// arbitrary captured text.
		return !(i == len(parts)-2 && parts[len(parts)-1] == "SYNTHETIC.md")
	}
	return false
}

func markdownProse(body []byte) []byte {
	var out bytes.Buffer
	inFence := false
	for _, line := range bytes.Split(body, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, []byte("```")) || bytes.HasPrefix(trimmed, []byte("~~~")) {
			inFence = !inFence
			out.WriteByte('\n')
			continue
		}
		if inFence {
			out.WriteByte('\n')
			continue
		}
		out.Write(inlineCode.ReplaceAll(line, []byte(" ")))
		out.WriteByte('\n')
	}
	return out.Bytes()
}

var inlineCode = regexp.MustCompile("`+[^`]*`+")

func matches(root, path string, body []byte) []string {
	var found []string
	file := filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator)))
	for _, finding := range FindForbiddenReferences(string(body)) {
		finding = strings.TrimPrefix(finding, "line ")
		line, detail, ok := strings.Cut(finding, ": ")
		if !ok {
			line, detail = strconv.Itoa(1), finding
		}
		found = append(found, file+":"+line+": "+detail)
	}
	return found
}
