package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
