package manifest

import (
	"strings"
	"testing"
)

const approved = `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.telara, name: p, version: 1.0.0}
execution: {entrypoint: main.py}
tools:
  - {alias: search, capability: gmail.threads.search, effect: read}
  - {alias: draft, capability: gmail.drafts.create, effect: write}
commands:
  - {command: kubectl, args: [get, pods, "*"], effect: read, env: [KUBECONFIG]}
files:
  - {path: in, access: read}
  - {path: out, access: write}
fetch:
  - {origin: "https://api.github.com"}
  - {origin: "https://*.atlassian.net", methods: [GET, POST]}
`

func TestWidening(t *testing.T) {
	for name, c := range map[string]struct {
		from, to string
		want     string // a fragment of what widened, or "" for nothing
	}{
		"nothing changed":         {"", "", ""},
		"only the version":        {"version: 1.0.0", "version: 1.1.0", ""},
		"only the code":           {"entrypoint: main.py", "entrypoint: main2.py", ""},
		"a tool removed":          {"  - {alias: draft, capability: gmail.drafts.create, effect: write}\n", "", ""},
		"a tool renamed":          {"alias: search", "alias: find", ""},
		"a tool's effect lowered": {"gmail.drafts.create, effect: write", "gmail.drafts.create, effect: read", ""},
		"a command removed":       {"  - {command: kubectl, args: [get, pods, \"*\"], effect: read, env: [KUBECONFIG]}\n", "  - {command: true, args: [], effect: read}\n", "command true"},
		"a fetch origin removed":  {"  - {origin: \"https://api.github.com\"}\n", "", ""},
		"a fetch method removed":  {"methods: [GET, POST]", "methods: [GET]", ""},
		"file access lowered":     {"{path: out, access: write}", "{path: out, access: read}", ""},

		"a new capability":                                     {"gmail.threads.search", "gmail.messages.send", "was not declared before"},
		"a tool's effect raised":                               {"gmail.threads.search, effect: read", "gmail.threads.search, effect: write", "now declared write"},
		"a tool newly pinned":                                  {"gmail.threads.search, effect: read}", "gmail.threads.search, effect: read, pin: {server: x, tool: y}}", "now pinned"},
		"an argument pattern changed":                          {"args: [get, pods, \"*\"]", "args: [get, \"*\"]", "command kubectl get *"},
		"an argument pattern narrowed, which is still an edit": {"args: [get, pods, \"*\"]", "args: [get, pods]", "command kubectl get pods"},
		"a command's effect changed":                           {"effect: read, env: [KUBECONFIG]", "effect: write, env: [KUBECONFIG]", "command kubectl"},
		"an environment name added":                            {"env: [KUBECONFIG]", "env: [KUBECONFIG, \"AWS_*\"]", "command kubectl"},
		"a file path changed":                                  {"{path: in, access: read}", "{path: \"in/**\", access: read}", "files in/**"},
		"file access raised":                                   {"{path: in, access: read}", "{path: in, access: write}", "raised from read to write"},
		"a fetch origin added":                                 {"https://api.github.com", "https://uploads.github.com", "not declared before"},
		"a fetch origin widened":                               {"https://api.github.com", "https://*.github.com", "not declared before"},
		"a fetch method added":                                 {"{origin: \"https://api.github.com\"}", "{origin: \"https://api.github.com\", methods: [GET, DELETE]}", "method DELETE added"},
		"a move out of the sandbox":                            {"execution: {entrypoint: main.py}", "execution: {entrypoint: main.py, runtime: tap-subprocess-v1}", "contained tier"},
	} {
		t.Run(name, func(t *testing.T) {
			next := approved
			if c.from != "" {
				next = strings.Replace(approved, c.from, c.to, 1)
				if next == approved {
					t.Fatalf("the fixture does not contain %q", c.from)
				}
			}
			a, err := Parse([]byte(approved))
			if err != nil {
				t.Fatal(err)
			}
			n, err := Parse([]byte(next))
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Join(Widening(a, n), "; ")
			if c.want == "" && got != "" {
				t.Fatalf("widens: %s", got)
			}
			if c.want != "" && !strings.Contains(got, c.want) {
				t.Fatalf("want %q, got %q", c.want, got)
			}
		})
	}
}
