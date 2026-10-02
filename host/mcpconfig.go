package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Agents that keep their MCP servers in a JSON file (Cursor, Windsurf) get
// one entry merged in by the runner (TENG-3114). Everything else in the file
// stays as it was: other keys keep their values and their order, the file is
// backed up once before the first change, a file that is not JSON is left
// alone, and writing the same entry again changes nothing.

// setMCPEntry adds (or with entry == nil removes) servers[name] under key in
// the JSON file at path. It reports whether the file changed.
func setMCPEntry(path, key, name string, entry any) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		if entry == nil {
			return false, nil
		}
		raw = []byte("{}")
	}
	top, err := orderedObject(raw)
	if err != nil {
		return false, fmt.Errorf("%s is not plain JSON (%v); it was left unchanged", path, err)
	}
	serversRaw := top.get(key)
	if serversRaw == nil {
		serversRaw = []byte("{}")
	}
	servers, err := orderedObject(serversRaw)
	if err != nil {
		return false, fmt.Errorf("%s: %s is not an object; it was left unchanged", path, key)
	}
	if entry == nil {
		if servers.get(name) == nil {
			return false, nil
		}
		servers.del(name)
	} else {
		b, err := json.Marshal(entry)
		if err != nil {
			return false, err
		}
		if old := servers.get(name); old != nil && jsonEqual(old, b) {
			return false, nil
		}
		servers.set(name, b)
	}
	top.set(key, servers.encode())
	var out bytes.Buffer
	if err := json.Indent(&out, top.encode(), "", "  "); err != nil {
		return false, err
	}
	out.WriteByte('\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if len(raw) > 0 && !bytes.Equal(raw, []byte("{}")) {
		backup := path + ".tap-backup"
		if _, err := os.Stat(backup); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(backup, raw, 0o600); err != nil {
				return false, err
			}
		}
	}
	return true, os.WriteFile(path, out.Bytes(), 0o600)
}

// object is a JSON object that keeps its keys in order and its values as
// they were written.
type object struct {
	keys []string
	vals map[string]json.RawMessage
}

func orderedObject(raw []byte) (*object, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("not an object")
	}
	o := &object{vals: map[string]json.RawMessage{}}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, _ := tok.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		if _, dup := o.vals[k]; !dup {
			o.keys = append(o.keys, k)
		}
		o.vals[k] = v
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("trailing data")
	}
	return o, nil
}

func (o *object) get(k string) json.RawMessage { return o.vals[k] }

func (o *object) set(k string, v json.RawMessage) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *object) del(k string) {
	delete(o.vals, k)
	for i, x := range o.keys {
		if x == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

func (o *object) encode() json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		var c bytes.Buffer
		if json.Compact(&c, o.vals[k]) == nil {
			b.Write(c.Bytes())
		} else {
			b.Write(o.vals[k])
		}
	}
	b.WriteByte('}')
	return b.Bytes()
}

func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return bytes.Equal(xa, ya)
}
