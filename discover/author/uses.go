package author

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

// Installed is a saved primitive a brief can propose to use.
type Installed struct {
	Ref      string // publisher/name
	Version  string
	Digest   string // the run digest a uses entry pins
	Manifest *manifest.Manifest
}

// InstalledPrimitives lists the primitives saved under root, the TAP
// collection. A folder that is not a package that may run is left out.
func InstalledPrimitives(root string) []Installed {
	items, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []Installed
	for _, it := range items {
		if !it.IsDir() || strings.HasPrefix(it.Name(), ".") {
			continue
		}
		dir := filepath.Join(root, it.Name())
		digest, m, err := manifest.RunDigest(dir)
		if err != nil || len(m.RunProblems()) != 0 {
			continue
		}
		out = append(out, Installed{Ref: m.Metadata.Publisher + "/" + m.Metadata.Name, Version: m.Metadata.Version, Digest: digest, Manifest: m})
	}
	return out
}

// UseSuggestion is an installed primitive whose whole tool sequence the
// procedure contains, so the primitive could be run from the new one instead
// of copying its steps. It is a proposal the author checks.
type UseSuggestion struct {
	Alias   string `json:"alias"`
	Ref     string `json:"ref"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
	Effect  string `json:"effect"`
	// Steps are the brief's step numbers the primitive would replace.
	Steps []int `json:"steps"`
}

var aliasChars = regexp.MustCompile(`[^a-z0-9_]+`)

// SuggestUses finds the installed primitives whose tool sequence the steps
// contain. A primitive's sequence is its tools in the order declared, each
// pinned to a server and a tool. It matches only when those steps follow one
// another in the procedure with exactly that server and tool, in that order.
// A primitive with a tool that is not pinned has no sequence to compare and is
// never proposed, and no name is compared loosely.
func SuggestUses(steps []BriefStep, installed []Installed) []UseSuggestion {
	var out []UseSuggestion
	for _, p := range installed {
		var seq [][2]string
		complete := len(p.Manifest.Tools) > 0
		for _, t := range p.Manifest.Tools {
			if t.Pin == nil || t.Pin.Server == "" || t.Pin.Tool == "" {
				complete = false
				break
			}
			seq = append(seq, [2]string{t.Pin.Server, t.Pin.Tool})
		}
		if !complete {
			continue
		}
		at := -1
		for i := 0; i+len(seq) <= len(steps) && at < 0; i++ {
			at = i
			for j, want := range seq {
				if steps[i+j].Server != want[0] || steps[i+j].MCPTool != want[1] {
					at = -1
					break
				}
			}
		}
		if at < 0 {
			continue
		}
		s := UseSuggestion{
			Alias: aliasFor(p.Manifest.Metadata.Name), Ref: p.Ref, Version: p.Version, Digest: p.Digest,
			Effect: highestDeclaredEffect(p.Manifest),
		}
		for j := range seq {
			s.Steps = append(s.Steps, steps[at+j].N)
		}
		out = append(out, s)
	}
	return out
}

func aliasFor(name string) string {
	a := aliasChars.ReplaceAllString(strings.ToLower(name), "_")
	a = strings.Trim(a, "_")
	if a == "" || a[0] < 'a' || a[0] > 'z' {
		a = "p_" + a
	}
	if len(a) > 64 {
		a = a[:64]
	}
	return a
}

var effectOrder = map[string]int{"read": 0, "write": 1, "destructive": 2, "financial": 2, "identity-admin": 2}

// highestDeclaredEffect is the highest effect a manifest declares itself: its
// tools, programs, file writes, fetch methods other than GET and HEAD, and
// what it uses.
func highestDeclaredEffect(m *manifest.Manifest) string {
	best := "read"
	raise := func(e string) {
		if effectOrder[e] > effectOrder[best] {
			best = e
		}
	}
	for _, t := range m.Tools {
		raise(t.Effect)
	}
	for _, c := range m.Commands {
		raise(c.Effect)
	}
	for _, f := range m.Files {
		if f.Access == "write" {
			raise("write")
		}
	}
	for _, f := range m.Fetch {
		for _, method := range f.Methods {
			if mth := strings.ToUpper(method); mth != "GET" && mth != "HEAD" {
				raise("write")
			}
		}
	}
	for _, u := range m.Uses {
		raise(u.Effect)
	}
	return best
}

// usesYAML is the uses block that would pin the suggested primitives.
func usesYAML(s []UseSuggestion) string {
	var w strings.Builder
	w.WriteString("uses:\n")
	for _, u := range s {
		fmt.Fprintf(&w, "  - {alias: %s, ref: %s, version: %s, digest: %s, effect: %s}\n", u.Alias, u.Ref, u.Version, u.Digest, u.Effect)
	}
	return w.String()
}
