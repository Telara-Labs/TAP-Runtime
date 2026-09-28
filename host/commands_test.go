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
	any := []string{"*"}
	return &manifest{Commands: []command{
		{Command: "git", Globals: []string{"-C <any>", "--no-pager"}, Args: []string{"log", "*"}, Effect: "read"},
		// The example of ruling 31.
		{Command: "kubectl", Globals: []string{"--context minikube", "-n <any>"}, Args: []string{"get", "pods", "*"}, Effect: "read"},
		{Command: "kubectl", Globals: []string{"--context minikube"}, Args: []string{"delete", "pod", "tmp-*"}, Effect: "destructive"},
		{Command: "bash", Globals: []string{"-c <any>"}, Args: any, Effect: "read"},
		{Command: "docker", Args: any, Effect: "read"},
		{Command: "docker", Args: []string{"ps", "*"}, Effect: "read"},
		{Command: "date", Args: []string{}, Effect: "read"},
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
		{"the rest is open where the pattern ends in a star", "kubectl", []string{"get", "pods", "-n", "app", "-o", "json"}, true, "read"},
		{"a resource that was not declared", "kubectl", []string{"get", "secrets"}, false, ""},
		{"a resource that was not declared, with more after it", "kubectl", []string{"get", "secrets", "pods"}, false, ""},
		{"declared destructive, name matching its pattern", "kubectl", []string{"delete", "pod", "tmp-7"}, true, "destructive"},
		{"declared destructive, name outside its pattern", "kubectl", []string{"delete", "pod", "payments"}, false, ""},
		{"an exact pattern refuses more words", "kubectl", []string{"delete", "pod", "tmp-7", "--all"}, false, ""},
		{"undeclared program", "curl", []string{"https://example.com"}, false, ""},
		{"undeclared subcommand", "kubectl", []string{"apply", "-f", "x"}, false, ""},
		{"bash declared read is reclassified", "bash", []string{"-c", "echo"}, true, "destructive"},
		{"docker run declared read is reclassified", "docker", []string{"run", "img"}, true, "destructive"},
		{"docker ps stays as declared", "docker", []string{"ps"}, true, "read"},
		{"a pattern of no words allows no arguments", "date", nil, true, "read"},
		{"and refuses any", "date", []string{"-u"}, false, ""},

		{"global taking any value", "git", []string{"-C", "/tmp", "log", "--oneline"}, true, "read"},
		{"global taking no value", "git", []string{"--no-pager", "log"}, true, "read"},
		{"two globals", "git", []string{"--no-pager", "-C", "/tmp", "log"}, true, "read"},
		{"global with an allowed literal", "kubectl", []string{"--context", "minikube", "get", "pods"}, true, "read"},
		{"same, written with an equals sign", "kubectl", []string{"--context=minikube", "get", "pods"}, true, "read"},
		{"global with a value that is not allowed", "kubectl", []string{"--context", "prod", "get", "pods"}, false, ""},
		{"same, written with an equals sign", "kubectl", []string{"--context=prod", "get", "pods"}, false, ""},
		{"undeclared global", "kubectl", []string{"--kubeconfig", "/etc/other", "get", "pods"}, false, ""},
		{"global declared for another subcommand", "kubectl", []string{"-n", "kube-system", "delete", "pod", "tmp-1"}, false, ""},
		{"global missing its value", "git", []string{"-C"}, false, ""},
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

// When several declarations match, the one that says most decides.
func TestTheMostSpecificDeclarationDecides(t *testing.T) {
	m := &manifest{Commands: []command{
		{Command: "kubectl", Args: []string{"*"}, Effect: "read"},
		{Command: "kubectl", Args: []string{"delete", "*"}, Effect: "destructive"},
		{Command: "kubectl", Args: []string{"delete", "pod", "*"}, Effect: "write"},
	}}
	for args, want := range map[string]string{
		"get pods":           "read",
		"delete ns payments": "destructive",
		"delete pod x":       "write",
	} {
		if _, got := resolve(m, "kubectl", strings.Fields(args)); got != want {
			t.Errorf("kubectl %s: %s, want %s", args, got, want)
		}
	}
}

func TestEnvironmentIsWithheldUnlessDeclared(t *testing.T) {
	environ := []string{"PATH=/bin", "HOME=/h", "AWS_SECRET_ACCESS_KEY=s3cret", "AWS_REGION=eu-west-1",
		"KUBECONFIG=/k", "GITLAB_TOKEN=t", "XAWS_OTHER=x", "EMPTY="}

	env, names := environFor(&command{Command: "git"}, environ)
	if !reflect.DeepEqual(names, []string{"HOME", "PATH"}) {
		t.Fatalf("an undeclaring command was given %v", names)
	}
	if strings.Contains(strings.Join(env, " "), "s3cret") {
		t.Fatal("a secret in the runner's environment reached a host program")
	}
	_, names = environFor(&command{Command: "kubectl", Env: []string{"KUBECONFIG", "NOT_SET_ANYWHERE"}}, environ)
	if !reflect.DeepEqual(names, []string{"HOME", "KUBECONFIG", "PATH"}) {
		t.Fatalf("got %v", names)
	}
	// Ruling 32: a declaration may be a pattern.
	_, names = environFor(&command{Command: "aws", Env: []string{"AWS_*"}}, environ)
	if !reflect.DeepEqual(names, []string{"AWS_REGION", "AWS_SECRET_ACCESS_KEY", "HOME", "PATH"}) {
		t.Fatalf("AWS_* gave %v", names)
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
