package main

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	agents "github.com/Telara-Labs/TAP-Runtime/discover/client"
)

// authorSkill tells a connected agent when a procedure is worth saving as a
// primitive and how to write, check and save one. Without it an agent on a
// fresh machine finds no primitive, does the work by hand, and never learns
// that it could have saved the procedure.
//
//go:embed skills/tap-author/SKILL.md
var authorSkill string

const (
	authorSkillName   = "tap-author"
	authorSkillMarker = "tap-owned: tap-author"
)

// syncAuthorSkill writes, refreshes or (with remove) deletes the tap-author
// skill in c's skills folder. A folder of that name that TAP did not write is
// left alone and reported.
func syncAuthorSkill(c agents.Client, home string, print, remove bool, stdout, stderr io.Writer) int {
	root, err := c.SkillsDir(false, home, "")
	if err != nil {
		fmt.Fprintf(stderr, "%s: authoring skill: %v\n", c.Name, err)
		return 1
	}
	dir := filepath.Join(root, authorSkillName)
	file := filepath.Join(dir, "SKILL.md")
	existing, err := os.ReadFile(file)
	switch {
	case err == nil && !strings.Contains(string(existing), authorSkillMarker):
		fmt.Fprintf(stdout, "%s: kept %s, which TAP did not write\n", c.Name, dir)
		return 0
	case err != nil && !os.IsNotExist(err):
		fmt.Fprintf(stderr, "%s: authoring skill: %v\n", c.Name, err)
		return 1
	}
	owned := err == nil
	switch {
	case remove && !owned:
		return 0
	case remove && print:
		fmt.Fprintf(stdout, "%s: would remove the authoring skill at %s\n", c.Name, dir)
		return 0
	case remove:
		if err := os.RemoveAll(dir); err != nil {
			fmt.Fprintf(stderr, "%s: authoring skill: %v\n", c.Name, err)
			return 1
		}
		fmt.Fprintf(stdout, "%s: removed the authoring skill at %s\n", c.Name, dir)
		return 0
	case owned && string(existing) == authorSkill:
		return 0
	case print:
		fmt.Fprintf(stdout, "%s: would write the authoring skill at %s\n", c.Name, dir)
		return 0
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(stderr, "%s: authoring skill: %v\n", c.Name, err)
		return 1
	}
	if err := os.WriteFile(file, []byte(authorSkill), 0o600); err != nil {
		fmt.Fprintf(stderr, "%s: authoring skill: %v\n", c.Name, err)
		return 1
	}
	fmt.Fprintf(stdout, "%s: wrote the authoring skill at %s\n", c.Name, dir)
	return 0
}
