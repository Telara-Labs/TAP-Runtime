package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Goose keeps its MCP servers as extensions in ~/.config/goose/config.yaml
// and has no command to add one. The runner merges its entry in
// the way setMCPEntry does for JSON: other keys keep their values, order and
// comments, the file is backed up once before the first change, a file that
// is not YAML is left alone, and writing the same entry again changes
// nothing.

// gooseEntry is the runner's extension entry for Goose.
func gooseEntry(self, name string, env envFlags) map[string]any {
	entry := map[string]any{
		"enabled": true, "type": "stdio", "name": name,
		"cmd": self, "args": []any{"serve", "--name", name}, "timeout": 300,
	}
	if len(env) > 0 {
		entry["envs"] = mcpEntry(self, name, env)["env"]
	}
	return entry
}

// setYAMLEntry adds (or with entry == nil removes) key.name in the YAML file
// at path. It reports whether the file changed.
func setYAMLEntry(path, key, name string, entry any) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	var doc yaml.Node
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return false, fmt.Errorf("%s is not YAML (%v); it was left unchanged", path, err)
		}
	}
	if entry == nil && doc.Kind == 0 {
		return false, nil
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	top := doc.Content[0]
	if top.Kind != yaml.MappingNode {
		return false, fmt.Errorf("%s is not a YAML mapping; it was left unchanged", path)
	}
	servers := mappingValue(top, key)
	if servers == nil {
		if entry == nil {
			return false, nil
		}
		servers = &yaml.Node{Kind: yaml.MappingNode}
		top.Content = append(top.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, servers)
	}
	if servers.Kind != yaml.MappingNode {
		return false, fmt.Errorf("%s: %s is not a mapping; it was left unchanged", path, key)
	}
	i := mappingIndex(servers, name)
	if entry == nil {
		if i < 0 {
			return false, nil
		}
		servers.Content = append(servers.Content[:i], servers.Content[i+2:]...)
	} else {
		var v yaml.Node
		if err := v.Encode(entry); err != nil {
			return false, err
		}
		if i >= 0 {
			var old, want any
			servers.Content[i+1].Decode(&old)
			v.Decode(&want)
			if fmt.Sprint(old) == fmt.Sprint(want) {
				return false, nil
			}
			servers.Content[i+1] = &v
		} else {
			servers.Content = append(servers.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: name}, &v)
		}
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if len(raw) > 0 {
		backup := path + ".tap-backup"
		if _, err := os.Stat(backup); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(backup, raw, 0o600); err != nil {
				return false, err
			}
		}
	}
	return true, os.WriteFile(path, out.Bytes(), 0o600)
}

func mappingIndex(m *yaml.Node, key string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i
		}
	}
	return -1
}

func mappingValue(m *yaml.Node, key string) *yaml.Node {
	if i := mappingIndex(m, key); i >= 0 {
		return m.Content[i+1]
	}
	return nil
}
