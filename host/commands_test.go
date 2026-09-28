package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func testManifest() *manifest {
	return &manifest{Commands: []command{
		{Command: "git", Globals: []string{"-C <any>", "--no-pager"}, Args: []string{"log"}, Effect: "read"},
		{Command: "kubectl", Globals: []string{"--context minikube", "-n <any>"}, Args: []string{"get"}, Effect: "read"},
		{Command: "kubectl", Globals: []string{"--context minikube"}, Args: []string{"delete"}, Effect: "destructive"},
		{Command: "bash", Globals: []string{"-c <any>"}, Effect: "read"},
		{Command: "docker", Effect: "read"},
	}}
}

func TestResolve(t *testing.T) {
	cases := []struct {
		name       string
		command    string
		args       []string
		declared   bool
		wantEffect string
	}{
		{"declared read", "kubectl", []string{"get", "pods"}, true, "read"},
		{"flags after the subcommand are not bounded", "kubectl", []string{"get", "pods", "-o", "json", "--context", "minikube"}, true, "read"},
		{"declared destructive", "kubectl", []string{"delete", "pod", "x"}, true, "destructive"},
		{"undeclared program", "curl", []string{"https://example.com"}, false, ""},
		{"undeclared subcommand", "kubectl", []string{"apply", "-f", "x"}, false, ""},
		{"bash declared read is reclassified", "bash", []string{"-c", "echo"}, true, "destructive"},
		{"docker run declared read is reclassified", "docker", []string{"run", "img"}, true, "destructive"},
		{"docker ps stays as declared", "docker", []string{"ps"}, true, "read"},

		// Doc 34 section 13.8 finding 3: a leading global flag.
		{"global taking any value", "git", []string{"-C", "/tmp", "log", "--oneline"}, true, "read"},
		{"global taking no value", "git", []string{"--no-pager", "log"}, true, "read"},
		{"two globals", "git", []string{"--no-pager", "-C", "/tmp", "log"}, true, "read"},
		{"global with an allowed literal", "kubectl", []string{"--context", "minikube", "get", "pods"}, true, "read"},
		{"same, written with an equals sign", "kubectl", []string{"--context=minikube", "get", "pods"}, true, "read"},
		{"global with a value that is not allowed", "kubectl", []string{"--context", "prod", "get", "pods"}, false, ""},
		{"same, written with an equals sign", "kubectl", []string{"--context=prod", "get", "pods"}, false, ""},
		{"undeclared global", "kubectl", []string{"--kubeconfig", "/etc/other", "get", "pods"}, false, ""},
		{"global declared for another subcommand", "kubectl", []string{"-n", "kube-system", "delete", "pod", "x"}, false, ""},
		{"global missing its value", "git", []string{"-C"}, false, ""},

		// The subcommand must be first. A read must never be satisfied by a
		// word that appears later in a different action.
		{"declared word appearing after another subcommand", "kubectl", []string{"delete", "pod", "get"}, true, "destructive"},
		{"declared word appearing late in an undeclared action", "kubectl", []string{"apply", "get"}, false, ""},
		{"a value that looks like the subcommand", "git", []string{"-C", "log", "status"}, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			decl, effect := resolve(testManifest(), c.command, c.args)
			if (decl != nil) != c.declared {
				t.Fatalf("declared = %v, want %v", decl != nil, c.declared)
			}
			if effect != c.wantEffect {
				t.Fatalf("effect = %q, want %q", effect, c.wantEffect)
			}
		})
	}
}

func TestEnvironmentIsWithheldUnlessDeclared(t *testing.T) {
	have := map[string]string{"PATH": "/bin", "HOME": "/h", "AWS_SECRET_ACCESS_KEY": "s3cret", "KUBECONFIG": "/k", "GITLAB_TOKEN": "t"}
	lookup := func(k string) (string, bool) { v, ok := have[k]; return v, ok }

	env, names := environFor(&command{Command: "git"}, lookup)
	if !reflect.DeepEqual(names, []string{"HOME", "PATH"}) {
		t.Fatalf("an undeclaring command was given %v", names)
	}
	if strings.Contains(strings.Join(env, " "), "s3cret") {
		t.Fatal("a secret in the runner's environment reached a host program")
	}
	_, names = environFor(&command{Command: "kubectl", Env: []string{"KUBECONFIG", "NOT_SET_ANYWHERE"}}, lookup)
	if !reflect.DeepEqual(names, []string{"HOME", "KUBECONFIG", "PATH"}) {
		t.Fatalf("got %v", names)
	}
}

// Runs real programs: the environment a host program actually sees, the
// directory it runs in, and what it is given on standard input.
func TestRunCommandAgainstRealPrograms(t *testing.T) {
	for _, p := range []string{"env", "pwd", "cat"} {
		if _, err := exec.LookPath(p); err != nil {
			t.Skipf("%s is not on this machine", p)
		}
	}
	t.Setenv("TAP_TEST_CANARY", "must-not-leak")
	t.Setenv("TAP_TEST_DECLARED", "passed-through")
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)

	m := &manifest{Commands: []command{
		{Command: "printenv", Effect: "read", Env: []string{"TAP_TEST_DECLARED"}},
		{Command: "pwd", Effect: "read"},
		{Command: "cat", Effect: "read"},
	}}
	var journal bytes.Buffer

	r := runCommand(m, request{Command: "printenv"}, false, &journal)
	if strings.Contains(r.Stdout, "must-not-leak") {
		t.Fatal("an undeclared variable reached the host program")
	}
	if !strings.Contains(r.Stdout, "TAP_TEST_DECLARED=passed-through") {
		t.Fatalf("a declared variable did not reach the host program:\n%s", r.Stdout)
	}
	if r := runCommand(m, request{Command: "pwd"}, false, &journal); strings.TrimSpace(r.Stdout) != dir {
		t.Fatalf("ran in %q, want %q", strings.TrimSpace(r.Stdout), dir)
	}
	if r := runCommand(m, request{Command: "cat", Stdin: "piped in\n"}, false, &journal); r.Stdout != "piped in\n" {
		t.Fatalf("standard input did not arrive: %q", r.Stdout)
	}

	var first map[string]any
	if err := json.Unmarshal([]byte(strings.SplitN(journal.String(), "\n", 2)[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first["cwd"] != dir || !strings.Contains(strings.Join(toStrings(first["env"]), ","), "TAP_TEST_DECLARED") {
		t.Fatalf("the journal does not say where it ran or what it was given: %v", first)
	}
	if strings.Contains(journal.String(), "passed-through") || strings.Contains(journal.String(), "must-not-leak") {
		t.Fatal("the journal recorded a variable's value; it may record names only")
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}
