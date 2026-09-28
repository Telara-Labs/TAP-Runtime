// Package scaffold implements `tap init`: it writes a package skeleton
// matching the tap-creator skill's own authoring templates (SKILL.md's
// claim that `tap init` emits the skill's rich templates -- CHANGELOG.md v1
// CLI fix item 5, made true at G0; STATUS.md G0 agenda item 5 previously
// found this claim false: the CLI shipped a divergent, minimal echo
// skeleton instead).
//
// The templates are copied byte-for-byte from
// telara-tap/skills/tap-creator/templates/primitive-skeleton-{api,web}/ into
// this package's templates/ directory and embedded via go:embed -- see
// templates/SYNC_NOTE.md for why a single shared source isn't possible
// across the module boundary, and for the manual-sync obligation this
// creates.
//
// Unlike the old echo skeleton, this output is an AUTHORING skeleton: it is
// full of REPLACE_* placeholders the author (human or the tap-creator skill
// driving an agent) fills in, exactly as it does when the skill itself
// scaffolds a package by hand. It does not validate or contract-test clean
// unmodified -- that was true of the old minimal skeleton but is not the
// design point of the rich one; the "next" hint below says so.
package scaffold

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed templates/primitive-skeleton-api
var apiTemplateFS embed.FS

//go:embed templates/primitive-skeleton-web
var webTemplateFS embed.FS

const (
	apiTemplateRoot = "templates/primitive-skeleton-api"
	webTemplateRoot = "templates/primitive-skeleton-web"
)

type Options struct {
	Dir  string // target directory (default: ./<name-local-part>)
	Name string // as given on the command line: "name" or "publisher/name"
	Web  bool
}

// Scaffold writes the package files under opts.Dir, creating it if needed.
// It refuses to overwrite an existing primitive.yaml.
func Scaffold(opts Options) (string, error) {
	publisher, localName := splitName(opts.Name)
	if publisher == "" {
		publisher = "dev.telara" // the templates' own placeholder default
	}
	dir := opts.Dir
	if dir == "" {
		dir = localName
	}
	if _, err := os.Stat(filepath.Join(dir, "primitive.yaml")); err == nil {
		return "", fmt.Errorf("%s/primitive.yaml already exists", dir)
	}

	templateFS, root := apiTemplateFS, apiTemplateRoot
	if opts.Web {
		templateFS, root = webTemplateFS, webTemplateRoot
	}

	err := fs.WalkDir(templateFS, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		content, err := templateFS.ReadFile(path)
		if err != nil {
			return err
		}
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		return os.WriteFile(full, substitutePlaceholders(content, publisher, localName), 0o644)
	})
	if err != nil {
		return "", err
	}
	return dir, nil
}

// substitutePlaceholders fills in the name/publisher the caller already gave
// tap init on the command line -- REPLACE_KEBAB_NAME, REPLACE_NAME, and
// REPLACE_PUBLISHER, plus the template's own "publisher: dev.telara"
// default line. Every other REPLACE_* placeholder (host, slot, tool,
// schema fields, fixture content, ...) is deliberately left for the author:
// that's the authoring skeleton's whole point.
func substitutePlaceholders(content []byte, publisher, localName string) []byte {
	s := string(content)
	s = strings.ReplaceAll(s, "REPLACE_KEBAB_NAME", localName)
	s = strings.ReplaceAll(s, "REPLACE_PUBLISHER/REPLACE_NAME", publisher+"/"+localName)
	s = strings.ReplaceAll(s, "publisher: dev.telara", "publisher: "+publisher)
	return []byte(s)
}

func splitName(name string) (publisher, local string) {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[:i], name[i+1:]
	}
	return "", name
}
