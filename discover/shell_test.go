package discover

import (
	"reflect"
	"testing"
)

func texts(cmds [][]word) [][]string {
	out := make([][]string, len(cmds))
	for i, ws := range cmds {
		for _, w := range ws {
			out[i] = append(out[i], w.Text)
		}
	}
	return out
}

func TestSimpleCommands(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want [][]string
	}{
		{"cd and env prefix dropped", `cd /x && GOWORK=off go test ./... -count=1`, [][]string{{"go", "test", "./...", "-count=1"}}},
		{"pipeline split, redirect dropped", `git log --oneline -5 2>&1 | head -3`, [][]string{{"git", "log", "--oneline", "-5"}, {"head", "-3"}}},
		{"quoted text is one word", `git commit -m "fix the thing" && git push`, [][]string{{"git", "commit", "-m", "fix the thing"}, {"git", "push"}}},
		{"heredoc body with multibyte text", "cat > f <<'EOF'\nstep → next — done ✓\nEOF\nls x", [][]string{{"cat"}, {"ls", "x"}}},
		{"two heredocs, last unterminated", "a <<X\nbody\nX\nb <<Y\nnever ends", [][]string{{"a"}, {"b"}}},
		{"heredoc body dropped", "python3 - <<'PY'\nimport os\nprint(1)\nPY\nls done", [][]string{{"python3", "-"}, {"ls", "done"}}},
		{"wrappers removed", `sudo timeout 30 kubectl get pods`, [][]string{{"kubectl", "get", "pods"}}},
		{"program base name", `/usr/local/bin/tap-run x`, [][]string{{"tap-run", "x"}}},
		{"set and export dropped", `set -euo pipefail; export A=1; make build`, [][]string{{"make", "build"}}},
		{"redirect target dropped", `jq . a.json > /tmp/out.txt`, [][]string{{"jq", ".", "a.json"}}},
		{"unterminated quote at end", `grep -n "abc`, [][]string{{"grep", "-n", "abc"}}},
		{"lone trailing quote", `ls "`, [][]string{{"ls", ""}}},
		{"command substitution stays in its word", `eval "$(minikube docker-env)"`, [][]string{{"eval", "$(minikube docker-env)"}}},
		{"loop grammar removed, body kept", "for repo in a b; do\n  git -C $repo status --short\ndone", [][]string{{"git", "-C", "$repo", "status", "--short"}}},
		{"if grammar removed, condition and body kept", `if [ -d x ]; then make; else echo no; fi`, [][]string{{"[", "-d", "x", "]"}, {"make"}}},
		{"echo and printf dropped", `echo "== $r"; printf '%s\n' x; ls`, [][]string{{"ls"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := texts(simpleCommands(c.in)); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("simpleCommands(%q)\n got %q\nwant %q", c.in, got, c.want)
			}
		})
	}
}

func TestTypeOf(t *testing.T) {
	cases := map[string]struct {
		w    word
		want string
	}{
		"flag":        {word{Text: "--context"}, SlotFlag},
		"url":         {word{Text: "https://x.dev/a"}, SlotURL},
		"quoted text": {word{Text: "fix the thing", Quoted: true}, SlotText},
		"number":      {word{Text: "30"}, SlotNumber},
		"duration":    {word{Text: "30s"}, SlotNumber},
		"path":        {word{Text: "./cmd/server"}, SlotPath},
		"file":        {word{Text: "main.go"}, SlotPath},
		"ticket":      {word{Text: "TENG-3054"}, SlotID},
		"sha":         {word{Text: "6cecfc9"}, SlotID},
		"word":        {word{Text: "status"}, SlotWord},
	}
	for name, c := range cases {
		if got := typeOf(c.w); got != c.want {
			t.Errorf("%s: typeOf(%q) = %s, want %s", name, c.w.Text, got, c.want)
		}
	}
}

func TestSubcommandSkipsFlagValues(t *testing.T) {
	for in, want := range map[string]string{
		"kubectl --context minikube get pods": "get",
		"git -C /x status":                    "status",
		"go test ./...":                       "test",
		"grep -rn foo .":                      "",
		"ls":                                  "",
	} {
		if got := subcommandOf(simpleCommands(in)[0]); got != want {
			t.Errorf("subcommandOf(%q) = %q, want %q", in, got, want)
		}
	}
}
