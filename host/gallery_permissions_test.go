package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/contract/glob"
	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

// A package that handles one file must not authorize a second file, or an
// empty argument suffix. A final bare * previously allowed both.
func TestGallerySinglePathCommandsRefuseAdditionalFiles(t *testing.T) {
	for _, pkg := range []string{"desktop-file-inventory", "desktop-image-inspect", "desktop-document-text", "desktop-open-review"} {
		t.Run(pkg, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("..", "examples", pkg, "primitive.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			m, err := mf.Parse(b)
			if err != nil {
				t.Fatal(err)
			}
			decl := m.Commands[len(m.Commands)-1]
			args := append([]string{}, decl.Args[:len(decl.Args)-1]...)
			if !glob.Args(decl.Args, append(args, "/tmp/a file")) {
				t.Fatal("one absolute path was refused")
			}
			for _, suffix := range [][]string{nil, {"/tmp/one", "/tmp/two"}, {"relative-file"}} {
				if glob.Args(decl.Args, append(append([]string{}, args...), suffix...)) {
					t.Fatalf("command %s authorized path suffix %q", decl.Command, suffix)
				}
			}
		})
	}
}
