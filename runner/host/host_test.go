package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testManifest() *manifest {
	return &manifest{Commands: []command{
		{Command: "git", Args: []string{"log"}, Effect: "read"},
		{Command: "kubectl", Args: []string{"get"}, Effect: "read"},
		{Command: "kubectl", Args: []string{"delete"}, Effect: "destructive"},
		{Command: "bash", Effect: "read"},
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
		{"declared destructive", "kubectl", []string{"delete", "pod", "x"}, true, "destructive"},
		{"undeclared program", "curl", []string{"https://example.com"}, false, ""},
		{"undeclared subcommand", "kubectl", []string{"apply", "-f", "x"}, false, ""},
		{"bash declared read is reclassified", "bash", []string{"-c", "echo"}, true, "destructive"},
		{"docker run declared read is reclassified", "docker", []string{"run", "img"}, true, "destructive"},
		{"docker ps stays as declared", "docker", []string{"ps"}, true, "read"},
		// Known defect of prefix matching, doc 34 section 13.8 finding 3. This
		// asserts today's behaviour so a fix has to change the test.
		{"leading global flag is refused", "git", []string{"-C", "/tmp", "log"}, false, ""},
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

func TestObtainRefusesAlteredInterpreter(t *testing.T) {
	store := t.TempDir()
	in := interpreters[".js"]
	if err := os.WriteFile(filepath.Join(store, in.File), []byte("not the pinned file"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := obtain(store, "main.js")
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("altered interpreter was not refused: %v", err)
	}
}

func TestObtainNamesTheBuildForAnUnpublishedInterpreter(t *testing.T) {
	_, _, _, err := obtain(t.TempDir(), "main.sh")
	if err == nil || !strings.Contains(err.Error(), "go build") {
		t.Fatalf("want an error naming the build command, got: %v", err)
	}
}

func TestObtainRefusesUnknownEntrypoint(t *testing.T) {
	if _, _, _, err := obtain(t.TempDir(), "main.rb"); err == nil {
		t.Fatal("an entrypoint with no listed interpreter was accepted")
	}
}

func TestEveryPublishedInterpreterIsPinned(t *testing.T) {
	for ext, in := range interpreters {
		if in.URL != "" && len(in.SHA256) != 64 {
			t.Errorf("%s has a URL and no sha256", ext)
		}
		if in.URL == "" && in.Build == "" {
			t.Errorf("%s has neither a URL nor a build command", ext)
		}
	}
}
