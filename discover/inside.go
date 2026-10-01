package discover

import (
	"net/url"
	"path"
	"regexp"
	"strings"

	"gitlab.com/telara-labs/tap-runtime/discover/shellparse"
)

// Some calls carry their real work as code or a document: a browser script
// passed to a JavaScript REPL tool, a patch passed to apply_patch. To the
// miner such a call is one step with one opaque text argument, so every
// browser session and every ledger update would look alike. These functions
// open them up into the steps they contain.

var awaitedCallRe = regexp.MustCompile(`await\s+([A-Za-z_$][\w$]*)((?:\s*\.\s*[A-Za-z_$][\w$]*)+)\s*\(`)

// jsSteps returns one step per awaited method call in a script, in order.
// Awaiting is what separates an action (a navigation, a click, a snapshot)
// from a utility (JSON.stringify, console.log). The method path (goto,
// playwright.domSnapshot) is the label. The receiver is a slot: it is fixed
// when every run used the same object (agent, chrome) and an input when it
// was a variable the author named each time (liTab, gmailTab). The first
// argument is a slot too: a string literal as its value, anything else (a
// function, an object) as its source, marked Raw.
func jsSteps(code string) []Step {
	var out []Step
	for _, m := range awaitedCallRe.FindAllStringSubmatchIndex(code, -1) {
		method := strings.Join(strings.Fields(strings.ReplaceAll(code[m[4]:m[5]], ".", " ")), ".")
		if isWait(method) {
			continue
		}
		st := Step{Label: "js:" + method, Skeleton: "js:" + method}
		st.Slots = append(st.Slots, Slot{Key: "recv", Type: SlotWord, Value: code[m[2]:m[3]]})
		rs := []rune(strings.TrimSpace(code[m[1]:]))
		switch {
		case len(rs) == 0 || rs[0] == ')':
		case isQuote(rs[0]):
			v, _ := readJSString(rs, 0)
			st.Slots = append(st.Slots, argSlots("0", v)...)
		default:
			src := strings.TrimSpace(string(rs[:scanJSValue(rs, 0)]))
			src = truncateUTF8(src, 2000)
			st.Slots = append(st.Slots, Slot{Key: "0", Type: SlotText, Value: src, Raw: true})
		}
		out = append(out, st)
	}
	return out
}

var patchFileRe = regexp.MustCompile(`(?m)^\*\*\* (Update|Add|Delete) File: (.+?)\s*$`)

// patchSteps returns one step per file a patch touches: "patch:update",
// "patch:add" or "patch:delete", with the file as a path slot and its base
// name as a separate slot, so a ledger updated every time (activity.log)
// shows as fixed even when it is reached by different paths.
func patchSteps(input string) []Step {
	var out []Step
	for _, m := range patchFileRe.FindAllStringSubmatch(input, -1) {
		label := "patch:" + strings.ToLower(m[1])
		out = append(out, Step{Label: label, Skeleton: label, Slots: []Slot{
			{Key: "file", Type: SlotPath, Value: m[2]},
			{Key: "name", Type: SlotWord, Value: path.Base(m[2])},
		}})
	}
	return out
}

// argSlots types one argument value. A URL also yields its host as a slot of
// its own: the site is usually the fixed part (the same page family every
// time) and the path the parameter. A path also yields its base name.
func argSlots(key, v string) []Slot {
	v = truncateUTF8(v, 200)
	tp := typeOf(shellparse.Word{Text: v, Quoted: strings.ContainsAny(v, " \n")})
	out := []Slot{{Key: key, Type: tp, Value: v}}
	switch tp {
	case SlotURL:
		if u, err := url.Parse(v); err == nil && u.Host != "" {
			out = append(out, Slot{Key: key + ".host", Type: SlotWord, Value: u.Host})
		}
	case SlotPath:
		out = append(out, Slot{Key: key + ".name", Type: SlotWord, Value: path.Base(v)})
	}
	return out
}

// isWait reports a call that only waits (waitForTimeout, waitForLoadState,
// sleep). Waits sit between nearly every browser action; they are timing,
// not work, and would otherwise multiply every browser pattern.
func isWait(method string) bool {
	last := method[strings.LastIndex(method, ".")+1:]
	return strings.HasPrefix(last, "waitFor") || last == "sleep" || last == "delay" || last == "wait"
}
