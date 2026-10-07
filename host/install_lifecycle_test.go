package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	agents "github.com/Telara-Labs/TAP-Runtime/discover/client"
	"github.com/Telara-Labs/TAP-Runtime/discover/pack"
)

func TestInstallDoesNotLinkInterruptedHiddenSave(t *testing.T) {
	home, collection := t.TempDir(), t.TempDir()
	active := saveSetupPrimitive(t, collection)
	if err := os.Rename(active, filepath.Join(collection, ".greeting.saving-interrupted")); err != nil {
		t.Fatal(err)
	}
	codex, _ := agents.Lookup("codex")
	var out, errOut bytes.Buffer
	if rc := syncPointers(home, collection, []pack.Target{{Client: codex}}, false, true, &out, &errOut); rc != 0 {
		t.Fatalf("%d: %s", rc, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "skills", "greeting")); !os.IsNotExist(err) {
		t.Fatalf("linked interrupted save: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("temporary package counted as saved: %s", out.String())
	}
}
