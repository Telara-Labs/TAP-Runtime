package author

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// DraftsDir is where an agent drafts briefs, packages, cases and receipts:
// inside the workspace it works in, relative to it. Agents that may only
// touch their workspace (Kilo, Crush and OpenCode run modes refuse or ask
// about paths outside it) can then author and save without a prompt nobody
// is there to answer. Saving copies the package into the TAP collection;
// the runner writes the collection, never the agent.
const DraftsDir = ".tap/drafts"

// draftsIgnore keeps everything under the drafts folder, the ignore file
// included, out of git: a brief quotes session history and a draft is not
// the project's code.
const draftsIgnore = "# Written by tap: drafts and briefs are private and never committed.\n*\n"

// IgnoreDrafts makes sure nothing at path ends up in a git commit. A brief
// folder gets its own ignore file; when path lies under a .tap/drafts folder,
// that folder gets one too, so the packages, cases and receipts beside the
// brief are ignored as well. An existing ignore file is left as it is.
func IgnoreDrafts(path string, brief bool) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if brief {
		if err := writeIgnore(abs); err != nil {
			return err
		}
	}
	if root := draftsRoot(abs); root != "" {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return err
		}
		return writeIgnore(root)
	}
	return nil
}

// draftsRoot is the nearest .tap/drafts folder holding abs (or abs itself),
// or "" when abs is not under one.
func draftsRoot(abs string) string {
	for p := abs; ; {
		if filepath.Base(p) == "drafts" && filepath.Base(filepath.Dir(p)) == ".tap" {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return ""
		}
		p = parent
	}
}

func writeIgnore(dir string) error {
	name := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(name); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.WriteFile(name, []byte(draftsIgnore), 0o600)
}
