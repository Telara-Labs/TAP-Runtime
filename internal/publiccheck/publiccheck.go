package publiccheck

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var forbiddenPublicText = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bTENG-[0-9]+\b`),
	regexp.MustCompile(`(?i)\bruling\s+(?:#|no\.?\s*)?[0-9]+\b`),
	regexp.MustCompile(`(?i)\bdecision\s+[0-9]+\b`),
	regexp.MustCompile(`(?i)\bCLAUDE[-_]EXECUTION[-_]PLAN\b`),
	regexp.MustCompile(`(?i)\btap-discover-review-[0-9]{4}-[0-9]{2}-[0-9]{2}\b`),
	regexp.MustCompile(`(?i)\bClaude[- ]Session\b`),
	regexp.MustCompile(`(?i)\bhttps?://(?:claude\.ai/(?:share|code|chat)/|chatgpt\.com/(?:c|share)/|chat\.openai\.com/(?:c|share)/|gemini\.google\.com/(?:app|share)/)[^\s<>()]+`),
	regexp.MustCompile(`(?i)\bCo-authored-by\s*:\s*(?:GitHub Copilot|OpenAI Codex|Amazon Q|Replit AI|Roo Code|Kilo Code|Claude|Copilot|ChatGPT|Codex|Gemini|Cursor|Windsurf|Aider|Devin|OpenHands|Cline|OpenAI|Anthropic|GPT(?:[- ]?\d+(?:\.\d+)*)?|Grok|DeepSeek|Mistral|Llama|Perplexity)\b`),
}

// FindForbiddenReferences returns internal ticket, decision, session, and AI
// attribution markers found in public-facing text.
func FindForbiddenReferences(text string) []string {
	var found []string
	body := []byte(text)
	for _, pattern := range forbiddenPublicText {
		for _, loc := range pattern.FindAllIndex(body, -1) {
			line := 1 + bytes.Count(body[:loc[0]], []byte("\n"))
			found = append(found, fmt.Sprintf("line %d: %s", line, string(bytes.TrimSpace(body[loc[0]:loc[1]]))))
		}
	}
	return found
}

// CheckCommitRange checks only commits in base..head. Both revisions must
// resolve to commits in the repository; an unavailable CI base is an error.
func CheckCommitRange(ctx context.Context, repo, base, head string) ([]string, error) {
	baseSHA, err := resolveCommit(ctx, repo, base)
	if err != nil {
		return nil, fmt.Errorf("resolve commit base %q: %w", base, err)
	}
	headSHA, err := resolveCommit(ctx, repo, head)
	if err != nil {
		return nil, fmt.Errorf("resolve commit head %q: %w", head, err)
	}
	ancestor := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", baseSHA, headSHA)
	ancestor.Dir = repo
	if output, err := ancestor.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("commit base %s is not an ancestor of head %s: %w %s", baseSHA, headSHA, err, strings.TrimSpace(string(output)))
	}
	cmd := exec.CommandContext(ctx, "git", "log", "--no-show-signature", "--format=%H%x00%B%x00", baseSHA+".."+headSHA)
	cmd.Dir = repo
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read commit range %s..%s: %w", baseSHA, headSHA, err)
	}
	fields := bytes.Split(output, []byte{0})
	var violations []string
	for i := 0; i+1 < len(fields); i += 2 {
		hash := strings.TrimSpace(string(fields[i]))
		message := string(fields[i+1])
		for _, finding := range FindForbiddenReferences(message) {
			violations = append(violations, fmt.Sprintf("%s: %s", hash, finding))
		}
	}
	return violations, nil
}

func resolveCommit(ctx context.Context, repo, revision string) (string, error) {
	if strings.TrimSpace(revision) == "" {
		return "", fmt.Errorf("revision is empty")
	}
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "--quiet", "--end-of-options", revision+"^{commit}")
	cmd.Dir = repo
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse failed: %w", err)
	}
	sha := strings.TrimSpace(string(output))
	if sha == "" {
		return "", fmt.Errorf("git rev-parse returned no commit")
	}
	return sha, nil
}

// RepositoryRoot resolves the Git worktree root from an arbitrary working
// directory, or returns a useful error when invoked outside a checkout.
func RepositoryRoot(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("find Git worktree root: %w", err)
	}
	root := strings.TrimSpace(string(output))
	if root == "" {
		return "", fmt.Errorf("git rev-parse returned no worktree root")
	}
	return filepath.Clean(root), nil
}
