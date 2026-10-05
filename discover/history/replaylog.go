package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strconv"
)

// Replay logs (plan §3.4.1, envelope ReplayLog): an agent writes a session as
// an initial state followed by changes, one JSON record per line, and the
// session is what replaying them gives. Two dialects are read here, each
// following its writer's own reader:
//
//   - VS Code chat (chatSessions/<id>.jsonl, VS Code >= 1.109): kind 0 is the
//     whole state; kind 1 sets the value at path k; kind 2 pushes the values v
//     onto the array at k, first cutting it to length i when i is given;
//     kind 3 deletes the value at k. (workbench.desktop.main.js, the log
//     reader's _applySet/_applyPush.)
//   - Gemini CLI (chats/session-*.jsonl): a record with an id is a message,
//     added or, for an id already seen, replaced in place; {"$set": {...}}
//     updates metadata and, with "messages", replaces them all;
//     {"$rewindTo": id} drops that message and every later one (all of them
//     when the id is unknown). (gemini-cli loadConversationRecord.)
//
// A line that is not JSON (a torn last line while the agent writes) is
// skipped and counted.

// vscodeOp is one VS Code log record.
type vscodeOp struct {
	Kind int               `json:"kind"`
	K    []json.RawMessage `json:"k"`
	V    json.RawMessage   `json:"v"`
	I    *int              `json:"i"`
}

// ReplayVSCode replays a VS Code chat log into the session state.
func ReplayVSCode(r io.Reader) (state any, skipped int, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 256<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var op vscodeOp
		if json.Unmarshal(line, &op) != nil {
			skipped++
			continue
		}
		var v any
		if len(op.V) > 0 {
			if json.Unmarshal(op.V, &v) != nil {
				skipped++
				continue
			}
		}
		if op.Kind == 0 {
			state = v
			continue
		}
		if state == nil || len(op.K) == 0 {
			skipped++ // a change before any state, or with no path
			continue
		}
		path := make([]any, len(op.K))
		for i, k := range op.K {
			var s string
			var n float64
			switch {
			case json.Unmarshal(k, &s) == nil:
				path[i] = s
			case json.Unmarshal(k, &n) == nil:
				path[i] = int(n)
			default:
				path[i] = string(k)
			}
		}
		var ok bool
		switch op.Kind {
		case 1:
			state, ok = setAt(state, path, v)
		case 2:
			var items []any
			if v != nil {
				items, _ = v.([]any)
			}
			ok = pushAt(state, path, items, op.I)
		case 3:
			ok = deleteAt(state, path)
		}
		if !ok {
			skipped++
		}
	}
	return state, skipped, sc.Err()
}

// parentOf walks to the container holding the last path element.
func parentOf(root any, path []any) (any, bool) {
	cur := root
	for _, p := range path[:len(path)-1] {
		switch c := cur.(type) {
		case map[string]any:
			key, _ := p.(string)
			cur = c[key]
		case []any:
			i, ok := p.(int)
			if !ok || i < 0 || i >= len(c) {
				return nil, false
			}
			cur = c[i]
		default:
			return nil, false
		}
	}
	return cur, cur != nil
}

func setAt(root any, path []any, v any) (any, bool) {
	parent, ok := parentOf(root, path)
	if !ok {
		return root, false
	}
	switch c := parent.(type) {
	case map[string]any:
		key, _ := path[len(path)-1].(string)
		c[key] = v
		return root, true
	case []any:
		i, ok := path[len(path)-1].(int)
		if !ok || i < 0 || i >= len(c) {
			return root, false
		}
		c[i] = v
		return root, true
	}
	return root, false
}

// pushAt replaces the array at path (in its parent) by itself cut to length
// cut, if given, plus items: arrays are values, so the parent is updated.
func pushAt(root any, path []any, items []any, cut *int) bool {
	parent, ok := parentOf(root, path)
	if !ok {
		return false
	}
	var get func() []any
	var put func([]any) bool
	switch c := parent.(type) {
	case map[string]any:
		key, _ := path[len(path)-1].(string)
		get = func() []any { a, _ := c[key].([]any); return a }
		put = func(a []any) bool { c[key] = a; return true }
	case []any:
		i, ok := path[len(path)-1].(int)
		if !ok || i < 0 || i >= len(c) {
			return false
		}
		get = func() []any { a, _ := c[i].([]any); return a }
		put = func(a []any) bool { c[i] = a; return true }
	default:
		return false
	}
	a := get()
	if cut != nil && *cut >= 0 && *cut <= len(a) {
		a = a[:*cut]
	}
	return put(append(a, items...))
}

func deleteAt(root any, path []any) bool {
	parent, ok := parentOf(root, path)
	if !ok {
		return false
	}
	if c, ok := parent.(map[string]any); ok {
		key, _ := path[len(path)-1].(string)
		delete(c, key)
		return true
	}
	return false
}

// GeminiLog is a Gemini CLI session replayed: its metadata and its messages
// in order.
type GeminiLog struct {
	Meta     map[string]json.RawMessage
	Messages []json.RawMessage
}

// ReplayGemini replays a Gemini CLI session log.
func ReplayGemini(r io.Reader) (log GeminiLog, skipped int, err error) {
	log.Meta = map[string]json.RawMessage{}
	var order []string
	byID := map[string]json.RawMessage{}
	put := func(id string, msg json.RawMessage) {
		if _, ok := byID[id]; !ok {
			order = append(order, id)
		}
		byID[id] = msg
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 256<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec map[string]json.RawMessage
		if json.Unmarshal(line, &rec) != nil {
			skipped++
			continue
		}
		str := func(k string) (string, bool) {
			var s string
			if v, ok := rec[k]; ok && json.Unmarshal(v, &s) == nil {
				return s, true
			}
			return "", false
		}
		if id, ok := str("$rewindTo"); ok {
			at := -1
			for i, x := range order {
				if x == id {
					at = i
					break
				}
			}
			if at < 0 {
				at = 0
			}
			for _, x := range order[at:] {
				delete(byID, x)
			}
			order = order[:at]
			continue
		}
		if id, ok := str("id"); ok {
			put(id, line)
			continue
		}
		var set map[string]json.RawMessage
		if v, ok := rec["$set"]; ok && json.Unmarshal(v, &set) == nil {
			if msgs, ok := set["messages"]; ok {
				var list []json.RawMessage
				if json.Unmarshal(msgs, &list) == nil {
					order, byID = nil, map[string]json.RawMessage{}
					for _, m := range list {
						var head struct{ ID string }
						if json.Unmarshal(m, &head) == nil && head.ID != "" {
							put(head.ID, m)
						}
					}
				}
			}
			for k, v := range set {
				if k != "messages" {
					log.Meta[k] = v
				}
			}
			continue
		}
		if _, ok := str("sessionId"); ok {
			for k, v := range rec {
				if k != "messages" {
					log.Meta[k] = v
				}
			}
			continue
		}
		skipped++
	}
	for _, id := range order {
		log.Messages = append(log.Messages, byID[id])
	}
	return log, skipped, sc.Err()
}

// jsonString is a raw JSON string's value, or "".
func jsonString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// itoa avoids importing strconv in every reader.
func itoa(n int) string { return strconv.Itoa(n) }
