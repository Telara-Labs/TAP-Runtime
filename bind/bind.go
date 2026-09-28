// Package bind chooses which of a client's tools a declared capability binds
// to when the client gives no input schema to check a contract against.
//
// Doc 34 section 13.11, rulings 16 to 20. Where a schema IS available the
// satisfaction rule of section 11.3 decides and this package is not used.
//
// Everything here is normative: two runners must reach the same choice from
// the same inventory, so nothing depends on map order, inventory order or
// locale. Change the arithmetic and you change what every primitive binds to.
package bind

import (
	"sort"
	"strings"
	"unicode"
)

// Effect is what a primitive declares for a tool, and what a tool's own
// server says about it. Unknown is only ever the second.
type Effect string

const (
	Read        Effect = "read"
	Write       Effect = "write"
	Destructive Effect = "destructive"
	Unknown     Effect = "unknown"
)

var rank = map[Effect]int{Read: 0, Write: 1, Destructive: 2}

// Floor is the minimum score that binds. Chosen from measurement against two
// clients' real inventories; see bind_test.go and doc 34 section 13.12.
const Floor = 0.60

// Tool is one entry of a client's inventory, as its bridge reports it.
type Tool struct {
	Server    string
	Name      string
	Annotated Effect // what the server says; Unknown when it says nothing
}

// Choice is the outcome for one declared capability.
type Choice struct {
	Bound         *Tool
	Score         float64
	RunnerUp      *Tool // nil when there was no second candidate
	RunnerUpScore float64
	// Gated is set when the bound tool's server did not say what it does. The
	// runner treats it as a write whatever the primitive declared (ruling 20).
	Gated bool
	// Refused says why nothing bound. Empty when Bound is set.
	Refused string
}

// verbs groups words that mean the same action. It is about English verbs,
// not about any connector, which is why it may be a fixed list.
var verbs = map[string]string{
	"search": "search", "find": "search", "query": "search",
	"list": "list",
	"get":  "get", "read": "get", "fetch": "get", "retrieve": "get",
	"create": "create", "add": "create", "new": "create", "insert": "create",
	"update": "update", "edit": "update", "modify": "update", "patch": "update",
	"delete": "delete", "remove": "delete", "trash": "delete",
	"send": "send",
}

// Tokens splits a name into lower-case words on every non-alphanumeric rune
// and on a lower-to-upper case change, then reduces each word to a singular
// and each verb to its group's first word.
func Tokens(name string) []string {
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words = append(words, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	var prev rune
	for _, r := range name {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
		case unicode.IsUpper(r) && unicode.IsLower(prev):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
		prev = r
	}
	flush()
	for i, w := range words {
		w = singular(w)
		if v, ok := verbs[w]; ok {
			w = v
		}
		words[i] = w
	}
	return words
}

func singular(w string) string {
	switch {
	case len(w) > 4 && strings.HasSuffix(w, "ies"):
		return w[:len(w)-3] + "y"
	case len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss"):
		return w[:len(w)-1]
	}
	return w
}

func set(words []string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

// Resolve binds one capability against an inventory.
//
// A capability is written provider.resource.verb, for example
// gmail.threads.search. Its first part is the provider and its last the verb.
func Resolve(capability string, declared Effect, inventory []Tool) Choice {
	parts := strings.Split(capability, ".")
	if len(parts) < 2 {
		return Choice{Refused: "capability " + capability + " is not written provider.resource.verb"}
	}
	provider := Tokens(parts[0])
	want := Tokens(strings.Join(parts[1:], "."))
	if len(provider) == 0 || len(want) == 0 {
		return Choice{Refused: "capability " + capability + " is not written provider.resource.verb"}
	}
	verb := want[len(want)-1]
	wantSet := set(want)

	type scored struct {
		tool   Tool
		score  float64
		recall float64
		words  int
	}
	var cands []scored
	sawProvider, sawEffectMismatch := false, false
	for _, t := range inventory {
		serverWords := set(Tokens(t.Server))
		toolWords := Tokens(t.Name)
		all := set(toolWords)
		for w := range serverWords {
			all[w] = true
		}
		// Ruling 19: the provider must appear in the server or tool name.
		ok := true
		for _, p := range provider {
			if !all[p] {
				ok = false
			}
		}
		if !ok {
			continue
		}
		sawProvider = true
		// Words that only say whose tool this is carry no meaning about what
		// it does: drop the provider and the server's own words.
		have := map[string]bool{}
		for _, w := range toolWords {
			if serverWords[w] || contains(provider, w) {
				continue
			}
			have[w] = true
		}
		if !have[verb] {
			continue // a different action is never the same capability
		}
		// Ruling 20: a tool its server says is more dangerous than the
		// primitive declared never binds.
		if t.Annotated != Unknown && rank[t.Annotated] > rank[declared] {
			sawEffectMismatch = true
			continue
		}
		matched := 0
		for w := range wantSet {
			if have[w] {
				matched++
			}
		}
		recall := float64(matched) / float64(len(wantSet))
		precision := float64(matched) / float64(len(have))
		score := 0.0
		if recall+precision > 0 {
			score = 2 * recall * precision / (recall + precision)
		}
		cands = append(cands, scored{t, score, recall, len(have)})
	}
	// Ruling 18: the top score binds. Equal scores are separated by recall,
	// then by the shorter name, then by server and name in byte order, so the
	// order the client listed its tools in never decides anything.
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if a.recall != b.recall {
			return a.recall > b.recall
		}
		if a.words != b.words {
			return a.words < b.words
		}
		if a.tool.Server != b.tool.Server {
			return a.tool.Server < b.tool.Server
		}
		return a.tool.Name < b.tool.Name
	})
	if len(cands) == 0 || cands[0].score < Floor {
		c := Choice{}
		switch {
		case !sawProvider:
			c.Refused = "no connector for " + parts[0] + " was found"
		case sawEffectMismatch && len(cands) == 0:
			c.Refused = "the only matching tools do more than the declared " + string(declared)
		default:
			c.Refused = "no " + parts[0] + " tool is close enough to " + capability
		}
		if len(cands) > 0 {
			t := cands[0].tool
			c.RunnerUp, c.RunnerUpScore = &t, cands[0].score
		}
		return c
	}
	top := cands[0].tool
	c := Choice{Bound: &top, Score: cands[0].score, Gated: top.Annotated == Unknown}
	if len(cands) > 1 {
		r := cands[1].tool
		c.RunnerUp, c.RunnerUpScore = &r, cands[1].score
	}
	return c
}

func contains(words []string, w string) bool {
	for _, x := range words {
		if x == w {
			return true
		}
	}
	return false
}
