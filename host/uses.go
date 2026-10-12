package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

// A primitive may run other primitives it lists under uses. Each one is pinned
// by the run digest of its package, so the digest of the primitive that uses
// them pins every one beneath it. A child runs under its own manifest only: it
// is admitted, bound, gated and granted on its own digest, and it inherits
// nothing from the primitive that started it. The primitive that started it
// receives its output and nothing else.

const (
	// maxUseDepth is how many levels of primitives a run may go down. The
	// primitive that was asked for is level 0.
	maxUseDepth = 4
	// maxConcurrentRuns is how many run requests one primitive may have in
	// flight; more wait.
	maxConcurrentRuns = 4
	// childStderrTail is how much of a failed child's stderr its parent is given.
	childStderrTail = 4 << 10
	// exitBadOutput is the exit a parent sees when a child's output does not
	// match what the child declared.
	exitBadOutput = 70
)

// useNode is one primitive a manifest uses, found in the local catalog by its
// digest.
type useNode struct {
	Alias, Ref, Version, Digest string
	// Declared is the effect the user of this primitive declared for it.
	// Effect is the highest effect the package itself declares, counting
	// what it uses in turn.
	Declared, Effect string
	Path             string
	Manifest         *mf.Manifest
	Uses             []*useNode
}

var effectRank = map[string]int{"read": 0, "write": 1, "destructive": 2, "financial": 2, "identity-admin": 2}

// refName is a catalog ref without its version: publisher/name.
func refName(ref string) string {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		return ref[:i]
	}
	return ref
}

func shortDigest(d string) string {
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// resolveUses finds every primitive m uses, and what each uses in turn, in the
// catalog. A primitive is found by its digest only, and nothing is fetched. self
// is the digest of m. Anything missing, cyclic, too deep or under-declared is
// refused here, before anything runs.
func resolveUses(entries []catalogEntry, m *mf.Manifest, self string) ([]*useNode, error) {
	return resolveUsesAt(entries, m, []string{self})
}

func resolveUsesAt(entries []catalogEntry, m *mf.Manifest, chain []string) ([]*useNode, error) {
	var out []*useNode
	for _, u := range m.Uses {
		if len(chain) > maxUseDepth {
			return nil, fmt.Errorf("%s: primitives may use others only %d levels down", u.Alias, maxUseDepth)
		}
		var found *catalogEntry
		var sameRef []string
		for i := range entries {
			e := &entries[i]
			if strings.EqualFold(e.Digest, u.Digest) && refName(e.Ref) == u.Ref {
				found = e
				break
			}
			if refName(e.Ref) == u.Ref {
				sameRef = append(sameRef, shortDigest(e.Digest))
			}
		}
		if found == nil {
			why := fmt.Sprintf("%s needs %s@%s (%s), which is not installed here. Save or pull that version first.", u.Alias, u.Ref, u.Version, shortDigest(u.Digest))
			if len(sameRef) > 0 {
				why = fmt.Sprintf("%s needs %s@%s (%s), but the version installed here is a different one (%s). Save or pull the pinned version, or re-pin the use.", u.Alias, u.Ref, u.Version, shortDigest(u.Digest), strings.Join(sameRef, ", "))
			}
			return nil, fmt.Errorf("%s", why)
		}
		for _, d := range chain {
			if strings.EqualFold(d, found.Digest) {
				return nil, fmt.Errorf("%s: %s uses itself, directly or through the primitives beneath it", u.Alias, u.Ref)
			}
		}
		kids, err := resolveUsesAt(entries, found.Manifest, append(append([]string{}, chain...), found.Digest))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", u.Alias, err)
		}
		effect, why := highestEffect(found.Manifest, kids)
		if effectRank[effect] > effectRank[u.Effect] {
			return nil, fmt.Errorf("%s is declared %s but %s can %s (%s). Declare effect: %s.", u.Alias, u.Effect, u.Ref, effect, why, effect)
		}
		out = append(out, &useNode{
			Alias: u.Alias, Ref: u.Ref, Version: u.Version, Digest: strings.ToLower(u.Digest),
			Declared: u.Effect, Effect: effect, Path: found.Path, Manifest: found.Manifest, Uses: kids,
		})
	}
	return out, nil
}

// highestEffect is the most a package declares it may do, and the first
// declaration that says so: the highest of its tools, its host programs, its
// files (write is a write) and its fetches (anything but GET and HEAD is a
// write), and of what it uses.
func highestEffect(m *mf.Manifest, uses []*useNode) (effect, why string) {
	effect = "read"
	raise := func(e, because string) {
		if effectRank[e] > effectRank[effect] {
			effect, why = e, because
		}
	}
	for _, t := range m.Tools {
		raise(t.Effect, "tool "+mf.CapabilityName(t.Capability))
	}
	for _, c := range m.Commands {
		raise(c.Effect, "program "+c.Command)
	}
	for _, f := range m.Files {
		if f.Access == "write" {
			raise("write", "files "+f.Path)
		}
	}
	for _, f := range m.Fetch {
		for _, method := range f.Methods {
			if mth := strings.ToUpper(method); mth != "GET" && mth != "HEAD" {
				raise("write", "fetch "+mth+" "+f.Origin)
			}
		}
	}
	for _, u := range uses {
		raise(u.Effect, "use "+u.Alias)
	}
	return effect, why
}

// treeNode is a resolved node with the name it is shown under.
type treeNode struct {
	Name string // alias path from the root, such as fetch_release/inner
	*useNode
}

// flattenUses lists every node beneath a set of uses, depth first.
func flattenUses(nodes []*useNode, prefix string) []treeNode {
	var out []treeNode
	for _, n := range nodes {
		name := prefix + n.Alias
		out = append(out, treeNode{Name: name, useNode: n})
		out = append(out, flattenUses(n.Uses, name+"/")...)
	}
	return out
}

// usesProblems says what would stop m from running its uses: the problems
// resolveUses finds, for the commands that check a package without running it.
func usesProblems(entries []catalogEntry, m *mf.Manifest, self string) []string {
	if len(m.Uses) == 0 {
		return nil
	}
	if _, err := resolveUses(entries, m, self); err != nil {
		return []string{err.Error()}
	}
	return nil
}

// treeShared is what the primitives of one run share. Everything that grants
// authority is kept out of it: it holds counters, the clock and the way to
// ask a person.
type treeShared struct {
	mu         sync.Mutex
	dispatches int
	max        int
	// askMu serializes the questions put to a person, because one approver
	// serves the whole tree.
	askMu sync.Mutex
}

// runTree is the place of one primitive in the tree of a run.
type runTree struct {
	*treeShared
	depth     int
	path      []string // the primitive's name, then the aliases down to this one
	digests   []string // from the root to this primitive
	ancestors []*budget
	// parentRun and parentReq name the run and request that started this one.
	parentRun, parentReq string
	// runID is the id this run is created under, when its parent chose it.
	runID string
	// timeCap, when set, is the least time that is left to any ancestor.
	timeCap time.Duration
	// uses are the primitives this one may run.
	uses []*useNode
}

func newRootTree(max int, name, digest string) *runTree {
	return &runTree{treeShared: &treeShared{max: max}, path: []string{name}, digests: []string{digest}}
}

// below is the place of a child of this primitive.
func (t *runTree) below(n *useNode, ancestors []*budget, parentRun, parentReq, runID string, timeCap time.Duration) *runTree {
	return &runTree{
		treeShared: t.treeShared, depth: t.depth + 1,
		path:      append(append([]string{}, t.path...), n.Alias),
		digests:   append(append([]string{}, t.digests...), n.Digest),
		ancestors: ancestors, parentRun: parentRun, parentReq: parentReq, runID: runID,
		timeCap: timeCap, uses: n.Uses,
	}
}

// asker is the name a person is shown for the primitive asking: the path
// down from the one they started.
func (t *runTree) asker() string { return strings.Join(t.path, " › ") }

// countDispatch counts one request against the whole tree, and says whether it
// is over what the root allows.
func (t *runTree) countDispatch() (over bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dispatches++
	return t.dispatches > t.max
}

// childAuth is what a child is given beyond its own manifest: the fetch
// origins and read fetches that were kept for its own digest. The primitive
// that started it supplies none of it.
type childAuth struct {
	Origins, Grants []string
	Remember        func(kind string)
}

// ChildRun is what one child run in the tree did.
type ChildRun struct {
	Alias, Ref, Digest, RunID                  string
	Exit, Calls, FailedCalls, Refused, Unknown int
	Elapsed                                    time.Duration
	Children                                   []ChildRun
}

// childInfo carries what running a child learned back to the place that
// records it.
type childInfo struct {
	err     error
	unknown int
	run     *ChildRun
}

// A child's output is stored in its own finish record, so a parent that
// crashed before it heard the answer can read it.
type storedResult struct {
	Exit    int    `json:"exit"`
	Stdout  string `json:"stdout"`
	Stderr  string `json:"stderr"`
	Unknown int    `json:"unknown,omitempty"`
}

// clipTail keeps the last n bytes of s.
func clipTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// childReply is what the primitive that started a child is told. A child
// that failed, or whose output does not fit what it declared, is a result to
// inspect and not a failure of the runner.
func childReply(n *useNode, r storedResult) reply {
	out := strings.TrimRight(r.Stdout, "\n")
	switch {
	case r.Unknown > 0:
		return reply{Unknown: true, Result: out, Refused: fmt.Sprintf("%s ended with %d change(s) whose outcome is unknown: an earlier run stopped while they were in progress", n.Alias, r.Unknown)}
	case r.Exit != 0:
		return reply{Exit: r.Exit, Stderr: clipTail(r.Stderr, childStderrTail), Result: out}
	}
	if why := checkOutput(n.Manifest, out); why != "" {
		return reply{Exit: exitBadOutput, Stderr: "output did not match the declared outputSchema: " + why, Result: out}
	}
	return reply{Result: out}
}

// checkOutput holds a child's output to its declared outputSchema as far as
// this runner checks it: that it is an object with the required keys, and
// that the keys the schema types are of those types.
func checkOutput(m *mf.Manifest, out string) string {
	if m == nil || m.Interface == nil {
		return ""
	}
	schema := m.Interface.OutputSchema
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 && len(schemaStrings(schema["required"])) == 0 {
		return ""
	}
	var obj map[string]any
	if json.Unmarshal([]byte(out), &obj) != nil || obj == nil {
		return "the output is not a JSON object"
	}
	return checkObject(schema, obj)
}

// checkObject holds an object to a schema's required keys and to the types of
// its top-level properties. Full validation is not attempted.
func checkObject(schema map[string]any, obj map[string]any) string {
	for _, k := range schemaStrings(schema["required"]) {
		if _, ok := obj[k]; !ok {
			return fmt.Sprintf("the required field %q is missing", k)
		}
	}
	props, _ := schema["properties"].(map[string]any)
	for k, v := range obj {
		p, _ := props[k].(map[string]any)
		if p == nil {
			continue
		}
		var want []string
		switch t := p["type"].(type) {
		case string:
			want = []string{t}
		case []any:
			for _, x := range t {
				if s, ok := x.(string); ok {
					want = append(want, s)
				}
			}
		}
		if len(want) == 0 {
			continue
		}
		got := jsonKind(v)
		ok := false
		for _, w := range want {
			if w == got || (w == "number" && got == "integer") {
				ok = true
			}
		}
		if !ok {
			return fmt.Sprintf("the field %q is %s, not %s", k, got, strings.Join(want, " or "))
		}
	}
	return ""
}

func jsonKind(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64:
		if t == float64(int64(t)) {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

// localEntries is the catalog a run looks for its children in.
func localEntries(f func() ([]catalogEntry, error)) ([]catalogEntry, error) {
	if f != nil {
		return f()
	}
	return localCatalog()
}

// usesCheckDir is the problem, if any, with the primitives a package in dir
// uses: one that is not installed, a cycle, too many levels, or an effect
// declared lower than the primitive's own.
func usesCheckDir(dir string, catalog func() ([]catalogEntry, error)) string {
	digest, m, err := packageDigest(dir)
	if err != nil || len(m.Uses) == 0 {
		return ""
	}
	entries, err := localEntries(catalog)
	if err != nil {
		return err.Error()
	}
	if p := usesProblems(entries, m, digest); len(p) > 0 {
		return strings.Join(p, "; ")
	}
	return ""
}

// usesChanges lists what changed in the primitives a package uses: the ones
// added, the ones removed, and the ones pinned to a different version.
func usesChanges(old, next []mf.Use) []string {
	was := map[string]mf.Use{}
	for _, u := range old {
		was[u.Ref] = u
	}
	now := map[string]bool{}
	var out []string
	for _, u := range next {
		now[u.Ref] = true
		switch before, ok := was[u.Ref]; {
		case !ok:
			out = append(out, fmt.Sprintf("uses %s: added (%s, %s)", u.Ref, shortDigest(u.Digest), u.Effect))
		case before.Digest != u.Digest:
			out = append(out, fmt.Sprintf("uses %s: re-pinned from %s to %s", u.Ref, shortDigest(before.Digest), shortDigest(u.Digest)))
		case before.Effect != u.Effect:
			out = append(out, fmt.Sprintf("uses %s: effect changed from %s to %s", u.Ref, before.Effect, u.Effect))
		}
	}
	for _, u := range old {
		if !now[u.Ref] {
			out = append(out, fmt.Sprintf("uses %s: removed", u.Ref))
		}
	}
	return out
}
