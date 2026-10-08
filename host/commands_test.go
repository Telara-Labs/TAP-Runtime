package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/Telara-Labs/TAP-Runtime/bridge"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testManifest() *manifest {
	any := []string{"*"}
	return &manifest{Commands: []command{
		{Command: "git", Globals: []string{"-C <any>", "--no-pager"}, Args: []string{"log", "*"}, Effect: "read"},
		// A command's arguments as a pattern.
		{Command: "kubectl", Globals: []string{"--context minikube", "-n <any>"}, Args: []string{"get", "pods", "*"}, Effect: "read"},
		{Command: "kubectl", Globals: []string{"--context minikube"}, Args: []string{"delete", "pod", "tmp-*"}, Effect: "destructive"},
		{Command: "bash", Globals: []string{"-c <any>"}, Args: any, Effect: "read"},
		{Command: "docker", Args: any, Effect: "read"},
		{Command: "docker", Args: []string{"ps", "*"}, Effect: "read"},
		{Command: "date", Args: []string{}, Effect: "read"},
		{Command: "find", Args: []string{"*"}, Effect: "read"},
		{Command: "python3", Args: []string{"*"}, Effect: "read"},
		{Command: "ssh", Args: []string{"*"}, Effect: "read"},
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
		{"an interpreter declared read is reclassified", "python3", []string{"-c", "print(1)"}, true, "destructive"},
		{"ssh declared read is reclassified", "ssh", []string{"host", "uptime"}, true, "destructive"},
		{"find stays as declared", "find", []string{".", "-name", "*.go"}, true, "read"},
		{"find -exec is reclassified, wherever it appears", "find", []string{".", "-name", "*.go", "-exec", "rm", "{}", ";"}, true, "destructive"},
		{"find -execdir too", "find", []string{"/tmp", "-execdir", "sh", "-c", "x", ";"}, true, "destructive"},
		{"a pattern of no words allows no arguments", "date", nil, true, "read"},
		{"and refuses any", "date", []string{"-u"}, false, ""},
		{"with no globals declared, the patterns cover flags too", "docker", []string{"--context", "x", "ps"}, true, "read"},

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
	// A declaration may be a pattern.
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

	r := runCommand(context.Background(), bridge.Proc{}, m, request{Command: "printenv"}, false, &journal)
	if strings.Contains(r.Stdout, "must-not-leak") {
		t.Fatal("an undeclared variable reached the host program")
	}
	if !strings.Contains(r.Stdout, "TAP_TEST_DECLARED=passed-through") {
		t.Fatalf("a declared variable did not reach the host program:\n%s", r.Stdout)
	}
	if r := runCommand(context.Background(), bridge.Proc{}, m, request{Command: "pwd"}, false, &journal); strings.TrimSpace(r.Stdout) != dir {
		t.Fatalf("ran in %q, want %q", strings.TrimSpace(r.Stdout), dir)
	}
	if r := runCommand(context.Background(), bridge.Proc{}, m, request{Command: "cat", Stdin: "piped in\n"}, false, &journal); r.Stdout != "piped in\n" {
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

// The list of invocations that run code the manifest cannot
// describe was short. A command declared with args ["*"] and effect read ran
// `git -c alias.x=!sh ...` with no approval (threat-model probe P6).
func TestInvocationsThatRunCodeAreRecognized(t *testing.T) {
	for _, c := range []struct {
		argv []string
		code bool
	}{
		{[]string{"git", "-c", "alias.x=!sh -c id", "x"}, true},
		{[]string{"git", "-C", "/repo", "-c", "core.pager=sh", "log"}, true},
		{[]string{"git", "--config-env=core.sshCommand=VAR", "fetch"}, true},
		{[]string{"git", "--exec-path=/tmp/x", "status"}, true},
		{[]string{"git", "fetch", "--upload-pack=/tmp/x", "origin"}, true},
		{[]string{"git", "rebase", "--exec", "make test"}, true},
		{[]string{"git", "bisect", "run", "./t.sh"}, true},
		{[]string{"git", "submodule", "foreach", "id"}, true},
		{[]string{"git", "difftool"}, true},
		{[]string{"make", "all"}, true},
		{[]string{"awk", "BEGIN{system(\"id\")}"}, true},
		{[]string{"npm", "run", "build"}, true},
		{[]string{"npx", "pkg"}, true},
		{[]string{"go", "run", "."}, true},
		{[]string{"cargo", "build"}, true},
		{[]string{"tar", "-xf", "a.tar", "--to-command=sh"}, true},
		{[]string{"rsync", "-e", "sh", "a", "b"}, true},
		{[]string{"timeout", "5", "id"}, true},
		{[]string{"nohup", "id"}, true},
		// Ordinary reads stay reads.
		{[]string{"git", "log", "-c"}, false},
		{[]string{"git", "status", "--short"}, false},
		{[]string{"git", "-C", "/repo", "diff"}, false},
		{[]string{"git", "--no-pager", "log"}, false},
		{[]string{"go", "list", "./..."}, false},
		{[]string{"npm", "ls"}, false},
		{[]string{"kubectl", "get", "pods"}, false},
		{[]string{"tar", "-tf", "a.tar"}, false},
		{[]string{"cat", "a.txt"}, false},
	} {
		if got := runsArbitraryCode(c.argv[0], c.argv[1:]); got != c.code {
			t.Errorf("%v: runsArbitraryCode = %v, want %v", c.argv, got, c.code)
		}
	}
}

func TestGitDashCDeclaredAsAReadIsRaisedToDestructive(t *testing.T) {
	m := &manifest{Commands: []command{{Command: "git", Args: []string{"*"}, Effect: "read"}}}
	if _, effect := resolve(m, "git", []string{"-c", "alias.x=!id", "x"}); effect != "destructive" {
		t.Fatalf("git -c ran as %q", effect)
	}
	if _, effect := resolve(m, "git", []string{"log", "-5"}); effect != "read" {
		t.Fatalf("git log ran as %q", effect)
	}
}

func TestCommandClassificationIncludesDeclaredGlobals(t *testing.T) {
	for _, tc := range []struct {
		name          string
		globals, args []string
		want          string
	}{
		{"configuration flag", []string{"-c <any>"}, []string{"-c", "core.pager=cat", "log"}, "destructive"},
		{"configuration flag inline", []string{"--config-env <any>"}, []string{"--config-env=core.pager=TEST", "log"}, "destructive"},
		{"program location", []string{"--exec-path <any>"}, []string{"--exec-path=/tmp/owned-programs", "status"}, "destructive"},
		{"ordinary read globals", []string{"-C <any>", "--no-pager"}, []string{"-C", "/tmp", "--no-pager", "log"}, "read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &manifest{Commands: []command{{Command: "git", Globals: tc.globals, Args: []string{"*"}, Effect: "read"}}}
			if declaration, effect := resolve(m, "git", tc.args); declaration == nil || effect != tc.want {
				t.Fatalf("declared command classification: declaration=%v effect=%q, want %q", declaration, effect, tc.want)
			}
		})
	}
}

func TestCommandClassificationStillRecognizesLaunchersAfterGlobals(t *testing.T) {
	for _, tc := range []struct {
		command       string
		globals, args []string
		want          string
	}{
		{"docker", []string{"--context <any>"}, []string{"--context", "owned", "run", "fixture"}, "destructive"},
		{"kubectl", []string{"--context <any>"}, []string{"--context", "owned", "exec", "fixture"}, "destructive"},
		{"docker", []string{"--context <any>"}, []string{"--context", "owned", "ps"}, "read"},
		{"kubectl", []string{"--context <any>"}, []string{"--context", "owned", "get", "pods"}, "read"},
	} {
		m := &manifest{Commands: []command{{Command: tc.command, Globals: tc.globals, Args: []string{"*"}, Effect: "read"}}}
		if declaration, effect := resolve(m, tc.command, tc.args); declaration == nil || effect != tc.want {
			t.Errorf("%s globals: declaration=%v effect=%q, want %q", tc.command, declaration, effect, tc.want)
		}
	}
}

func TestAHostProgramStopsWithItsRunContext(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	m := &manifest{Commands: []command{{Command: "sleep", Args: []string{"5"}, Effect: "read"}}}
	start := time.Now()
	var journal bytes.Buffer
	r := runCommand(ctx, bridge.Proc{}, m, request{Command: "sleep", Args: []string{"5"}}, false, &journal)
	if r.Exit == 0 || !strings.Contains(r.Stderr, "stopped this program with the primitive") {
		t.Fatalf("canceled command: %+v", r)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("canceled local program kept running: %s", elapsed)
	}
	if !strings.Contains(journal.String(), `"outcome":"ran"`) || !strings.Contains(journal.String(), `"effect":"read"`) {
		t.Fatalf("canceled dispatched program lost its journal receipt: %s", journal.String())
	}
}
