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

func TestWideningCountsUsesChanges(t *testing.T) {
	d1 := strings.Repeat("a1", 32)
	d2 := strings.Repeat("b2", 32)
	use := func(alias, ref, digest, effect string) string {
		return "  - {alias: " + alias + ", ref: " + ref + ", version: 1.0.0, digest: " + digest + ", effect: " + effect + "}\n"
	}
	base := approved + "uses:\n" + use("brief", "dev.telara/brief", d1, "read")
	for name, c := range map[string]struct {
		next string
		want string // a fragment of what widened, or "" for nothing
	}{
		"nothing changed":                     {base, ""},
		"an entry removed":                    {approved, ""},
		"an alias renamed":                    {approved + "uses:\n" + use("summary", "dev.telara/brief", d1, "read"), ""},
		"the effect lowered":                  {approved + "uses:\n" + use("brief", "dev.telara/brief", d1, "read"), ""},
		"a new entry":                         {base + use("other", "dev.telara/other", d1, "read"), "uses dev.telara/other: not used before"},
		"a changed digest":                    {approved + "uses:\n" + use("brief", "dev.telara/brief", d2, "read"), "pinned to a different version"},
		"a higher effect":                     {approved + "uses:\n" + use("brief", "dev.telara/brief", d1, "write"), "now declared write"},
		"a first use on a manifest with none": {base, ""},
	} {
		t.Run(name, func(t *testing.T) {
			from := base
			if name == "the effect lowered" {
				from = approved + "uses:\n" + use("brief", "dev.telara/brief", d1, "write")
			}
			if name == "a first use on a manifest with none" {
				from = approved
				c.want = "uses dev.telara/brief: not used before"
			}
			a, err := Parse([]byte(from))
			if err != nil {
				t.Fatal(err)
			}
			n, err := Parse([]byte(c.next))
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

// Found by the promotion gate's tests: two manifests Widening
// cannot read compared as "nothing widened".
func TestAManifestThatCannotBeComparedWidens(t *testing.T) {
	ok, err := Parse([]byte(approved))
	if err != nil {
		t.Fatal(err)
	}
	v1, err := Parse([]byte("apiVersion: primitives.telara.dev/v1\nkind: Primitive\nmetadata: {publisher: dev.telara, name: p, version: 1.0.0}\nexecution: {entrypoint: main.py}\n"))
	if err != nil {
		t.Fatal(err)
	}
	broken, _ := Parse([]byte(strings.Replace(approved, "alias: search", "alias: Not-An-Alias", 1)))
	for name, c := range map[string]struct {
		approved, next *Manifest
		want           string
	}{
		"two v1 manifests":            {v1, v1, "the approved version is apiVersion"},
		"a v3 version after a v1 one": {v1, ok, "the approved version is apiVersion"},
		"a v1 version after a v3 one": {ok, v1, "the new version is apiVersion"},
		"nothing was approved":        {nil, ok, "the approved version has no manifest"},
		"no new manifest":             {ok, nil, "the new version has no manifest"},
		"a manifest that cannot run":  {ok, broken, "the new version has a manifest that could not run"},
	} {
		t.Run(name, func(t *testing.T) {
			got := strings.Join(Widening(c.approved, c.next), "\n")
			if !strings.Contains(got, c.want) {
				t.Fatalf("got %q, want it to say %q", got, c.want)
			}
		})
	}
	if w := Widening(ok, ok); len(w) != 0 {
		t.Fatalf("control failed: a manifest compared with itself widened: %v", w)
	}
}

// A version that drops a pin binds its tool across every server on the client
// instead of the one server and tool that were approved, so it reaches more
// (TAP-G05).
func TestWideningFlagsARemovedPin(t *testing.T) {
	pinned := strings.Replace(approved, "gmail.threads.search, effect: read}", "gmail.threads.search, effect: read, pin: {server: x, tool: y}}", 1)
	if pinned == approved {
		t.Fatal("the fixture does not contain the tool to pin")
	}
	a, err := Parse([]byte(pinned))
	if err != nil {
		t.Fatal(err)
	}
	n, err := Parse([]byte(approved))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(Widening(a, n), "; ")
	if !strings.Contains(got, "gmail.threads.search") || !strings.Contains(got, "pin") {
		t.Fatalf("a removed pin widened nothing: %q", got)
	}
	if same := Widening(a, a); len(same) != 0 {
		t.Fatalf("an unchanged pin widens: %v", same)
	}
}
