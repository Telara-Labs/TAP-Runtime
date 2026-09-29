package manifest

import (
	"fmt"
	"sort"
	"strings"
)

// Widening compares the declarations of a new version with those of the
// version that was approved, and says what the new one may do that the
// approved one could not. Empty means it widens nothing.
//
// Ruling 24 (doc 34 section 13.16): a new version needs approval again only
// if it widens. The definition applied here is that section's:
//
//   - a tool or capability not declared before, or a higher effect class on
//     one that was;
//   - a command, file, environment or fetch entry not declared before;
//   - a changed pattern in any of those, unless the change only removes an
//     entry. Whether one pattern is contained in another is not attempted:
//     any edit other than a deletion counts as widening;
//   - file access raised from read to write, or a fetch method added;
//   - a move from the contained tier to the uncontained one.
//
// Code is not compared. A version that declares the same things and runs
// different code widens nothing, which is the consequence of the ruling and
// the reason a tenant may require approval of every version instead.
//
// That definition was written to make the ruling buildable and had not been
// reviewed by Luis when this was written.
//
// A manifest that cannot be compared widens. Widening reads v3 declarations,
// and an older manifest has none of them to read: two v1 manifests would
// compare as "nothing widened" and a version be promoted without a person.
// So a missing manifest, one of another apiVersion, or one that could not
// run is reported as a widening, never as nothing.
func Widening(approved, next *Manifest) []string {
	var out []string
	add := func(f string, a ...any) { out = append(out, fmt.Sprintf(f, a...)) }

	for _, side := range []struct {
		name string
		m    *Manifest
	}{{"the approved version", approved}, {"the new version", next}} {
		switch {
		case side.m == nil:
			add("%s has no manifest; it cannot be compared", side.name)
		case side.m.APIVersion != APIVersion:
			add("%s is apiVersion %q, not %s; its declarations cannot be compared", side.name, side.m.APIVersion, APIVersion)
		default:
			if p := side.m.RunProblems(); len(p) > 0 {
				add("%s has a manifest that could not run (%s); it cannot be compared", side.name, p[0])
			}
		}
	}
	if len(out) > 0 {
		return out
	}

	rank := map[string]int{"read": 0, "write": 1, "destructive": 2, "financial": 2, "identity-admin": 2}
	was := map[string]int{}
	for _, t := range approved.Tools {
		name := CapabilityName(t.Capability)
		if r, ok := was[name]; !ok || rank[t.Effect] > r {
			was[name] = rank[t.Effect]
		}
	}
	for _, t := range next.Tools {
		name := CapabilityName(t.Capability)
		before, ok := was[name]
		switch {
		case !ok:
			add("tool %s: capability %s was not declared before", t.Alias, name)
		case rank[t.Effect] > before:
			add("tool %s: capability %s is now declared %s", t.Alias, name, t.Effect)
		}
	}
	// A pin changes which tool a capability reaches.
	pins := map[string]string{}
	for _, t := range approved.Tools {
		pins[CapabilityName(t.Capability)] = pinOf(t)
	}
	for _, t := range next.Tools {
		name := CapabilityName(t.Capability)
		if before, ok := pins[name]; ok && pinOf(t) != before && pinOf(t) != "" {
			add("tool %s: capability %s is now pinned to %s", t.Alias, name, pinOf(t))
		}
	}

	have := map[string]bool{}
	for _, c := range approved.Commands {
		have[commandKey(c)] = true
	}
	for _, c := range next.Commands {
		if !have[commandKey(c)] {
			add("command %s %s (%s): not declared before, or declared differently", c.Command, strings.Join(c.Args, " "), c.Effect)
		}
	}

	files := map[string]string{}
	for _, f := range approved.Files {
		if f.Access == "write" || files[f.Path] == "" {
			files[f.Path] = f.Access
		}
	}
	for _, f := range next.Files {
		before, ok := files[f.Path]
		switch {
		case !ok:
			add("files %s (%s): not declared before", f.Path, f.Access)
		case f.Access == "write" && before != "write":
			add("files %s: access raised from read to write", f.Path)
		}
	}

	methods := map[string]map[string]bool{}
	for _, f := range approved.Fetch {
		o := originKey(f.Origin)
		if methods[o] == nil {
			methods[o] = map[string]bool{}
		}
		for _, m := range methodsOf(f) {
			methods[o][m] = true
		}
	}
	for _, f := range next.Fetch {
		o := originKey(f.Origin)
		if methods[o] == nil {
			add("fetch %s: not declared before", f.Origin)
			continue
		}
		for _, m := range methodsOf(f) {
			if !methods[o][m] {
				add("fetch %s: method %s added", f.Origin, m)
			}
		}
	}

	if approved.Runtime() == RuntimeWasm && next.Runtime() != RuntimeWasm {
		add("execution.runtime: moved from the contained tier to %s", next.Runtime())
	}
	sort.Strings(out)
	return out
}

func pinOf(t Tool) string {
	if t.Pin == nil {
		return ""
	}
	return strings.Join([]string{t.Pin.Client, t.Pin.Server, t.Pin.Tool}, " / ")
}

// commandKey is everything a command declaration says. Two declarations with
// one key allow the same thing; any difference is a different declaration.
func commandKey(c Command) string {
	globals := append([]string{}, c.Globals...)
	env := append([]string{}, c.Env...)
	sort.Strings(globals)
	sort.Strings(env)
	return strings.Join([]string{c.Command, strings.Join(globals, "\x1f"), strings.Join(c.Args, "\x1f"), c.Effect, strings.Join(env, "\x1f")}, "\x1e")
}

func originKey(o string) string { return strings.ToLower(strings.TrimSuffix(o, "/")) }

func methodsOf(f Fetch) []string {
	if len(f.Methods) == 0 {
		return []string{"GET"}
	}
	out := make([]string, len(f.Methods))
	for i, m := range f.Methods {
		out[i] = strings.ToUpper(m)
	}
	return out
}
