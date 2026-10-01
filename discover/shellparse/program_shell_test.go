package shellparse

import (
	"strings"
	"testing"
)

func TestProgramShellWordsLiteralBoundary(t *testing.T) {
	words, err := ProgramShellWords(`git commit -m 'A title with spaces'`)
	if err != nil || strings.Join(words, "|") != "git|commit|-m|A title with spaces" {
		t.Fatalf("literal argv = %q, %v", words, err)
	}
	for _, line := range []string{
		"git status; git push", "git status | cat", "git status > file",
		"git status && git push", "git status $(whoami)", "git status `whoami`",
		"python3 -c 'print(1)'", "cd repo", "git status\ngit push", "X=1 git status",
		"git status # comment", `git commit -m "a\nb"`,
	} {
		words, err := ProgramShellWords(line)
		if err == nil && !ProgramCommandRunsCode(words[0], words[1:]) {
			t.Errorf("non-literal or code-running command passed: %q -> %q", line, words)
		}
	}
}

func TestProgramShellPipelineParser(t *testing.T) {
	commands, err := ProgramShellCommands(`cat 'a | b.txt' | grep -n 'target text'`)
	if err != nil || len(commands) != 2 || strings.Join(commands[0], "|") != "cat|a | b.txt" || strings.Join(commands[1], "|") != "grep|-n|target text" {
		t.Fatalf("pipe = %q, %v", commands, err)
	}
	for _, line := range []string{"cat a || grep b", "cat a |", "cat a | grep $(whoami)", "cat a | grep b > out", "cat a | grep b && wc -l", "cat a & grep b"} {
		if commands, err := ProgramShellCommands(line); err == nil {
			t.Errorf("unsafe pipeline passed: %q -> %q", line, commands)
		}
	}
	plan, err := ProgramShellPlan(`git add 'a && b.txt' && git status --short`)
	if err != nil || len(plan) != 2 || plan[1].Connector != "and" || plan[0].Words[2] != "a && b.txt" {
		t.Fatalf("success chain = %+v, %v", plan, err)
	}
}
