package main

import (
	"encoding/json"
	"regexp"
	"strings"

	"gitlab.com/telara-labs/tap-runtime/bind"
)

// plainWord is a value that can name an operation: a short word, never free
// text, an identifier with spaces, or a structure.
var plainWord = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:-]{0,39}$`)

// nestedTool finds the operation a call dispatches through another tool. A
// call dispatches when its arguments are plain-word values beside exactly one
// parameter object (a gateway's integration and action with their params).
// The operation is the client's own tool whose name holds every word of
// those values with the fewest other words (get_issue, not
// get_issue_comments); a tie finds nothing. Nothing here names a gateway or
// a provider.
func nestedTool(args map[string]any, via bind.Tool, inv []bind.Tool) (bind.Tool, bool) {
	objects := 0
	var words []string
	for _, v := range args {
		switch x := v.(type) {
		case map[string]any:
			objects++
		case string:
			t := strings.TrimSpace(x)
			if strings.HasPrefix(t, "{") && json.Valid([]byte(t)) {
				objects++
			} else if plainWord.MatchString(x) {
				words = append(words, bind.Tokens(x)...)
			}
		}
	}
	if objects != 1 || len(words) == 0 {
		return bind.Tool{}, false
	}
	unique := map[string]bool{}
	for _, w := range words {
		unique[w] = true
	}
	var found []bind.Tool
	fewest := -1
	for _, t := range inv {
		if t.Server == via.Server && t.Name == via.Name {
			continue
		}
		have := map[string]bool{}
		for _, w := range bind.Tokens(t.Name) {
			have[w] = true
		}
		for _, w := range bind.Tokens(t.Server) {
			have[w] = true
		}
		all := true
		for w := range unique {
			all = all && have[w]
		}
		if !all {
			continue
		}
		extra := len(have) - len(unique)
		switch {
		case fewest < 0 || extra < fewest:
			found, fewest = []bind.Tool{t}, extra
		case extra == fewest:
			found = append(found, t)
		}
	}
	if len(found) != 1 {
		return bind.Tool{}, false
	}
	return found[0], true
}

// callEffect is the effect one call is gated by. A dispatched operation the
// client also lists as its own tool contributes its annotation to the gate.
// A read declaration is refused when the operation is effectful; an
// effectful declaration is promoted to the stronger observed effect.
func callEffect(bd *binding, inv []bind.Tool, args map[string]any) (effect, refused, nested string) {
	effect = bd.effective()
	inner, ok := nestedTool(args, bd.tool, inv)
	if !ok || inner.Annotated == bind.Unknown {
		return effect, "", ""
	}
	nested = inner.Server + " / " + inner.Name
	if bd.Declared == string(bind.Read) && inner.Annotated != bind.Read {
		return effect, "the dispatched operation " + nested + " is annotated " + string(inner.Annotated) + " and the primitive declares " + bd.Declared, nested
	}
	if inner.Annotated == bind.Read && effect != string(bind.Read) && !bd.Asked {
		return string(bind.Read), "", nested
	}
	if bind.Rank(inner.Annotated) > bind.Rank(bind.Effect(effect)) {
		effect = string(inner.Annotated)
	}
	return effect, "", nested
}
