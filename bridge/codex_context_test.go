package bridge

import (
	"bufio"
	"encoding/json"
	"io"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/bind"
)

type codexContextWriter func([]byte) (int, error)

func (w codexContextWriter) Write(b []byte) (int, error) { return w(b) }
func (w codexContextWriter) Close() error                { return nil }

func TestCodexShellContextUsesHostTurnWithoutLoadingItems(t *testing.T) {
	for _, available := range []bool{true, false} {
		c := &Codex{pending: map[int]chan map[string]any{}}
		c.in = codexContextWriter(func(b []byte) (int, error) {
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatal(err)
			}
			p := m["params"].(map[string]any)
			if m["method"] != "thread/turns/list" || p["threadId"] != "original-session" || p["limit"] != float64(1) || p["itemsView"] != "notLoaded" || p["sortDirection"] != "desc" {
				t.Fatalf("unexpected context lookup: %v", m)
			}
			r := map[string]any{"result": map[string]any{"data": []any{map[string]any{"id": "host-turn"}}}}
			if !available {
				r = map[string]any{"error": map[string]any{"message": "method unavailable"}}
			}
			c.pending[int(m["id"].(float64))] <- r
			return len(b), nil
		})
		c.resolveCallerMeta(Proc{Env: []string{"CODEX_THREAD_ID=original-session"}})
		if !available {
			if len(c.toolMeta) != 0 {
				t.Fatal("invented context when host lookup failed")
			}
			continue
		}
		var meta map[string]any
		if err := json.Unmarshal(c.toolMeta, &meta); err != nil {
			t.Fatal(err)
		}
		context := meta["x-codex-turn-metadata"].(map[string]any)
		if context["session_id"] != "original-session" || context["turn_id"] != "host-turn" {
			t.Fatalf("context=%v", meta)
		}
	}
}

func TestCodexIncomingContextIsNeverReplacedFromAnotherSession(t *testing.T) {
	for _, raw := range []string{`{"x-codex-turn-metadata":{"session_id":"incoming","turn_id":"incoming-turn"}}`, `{"x-codex-turn-metadata":{"session_id":"incoming"}}`} {
		c := &Codex{in: codexContextWriter(func([]byte) (int, error) { t.Fatal("looked up another session"); return 0, nil })}
		meta := json.RawMessage(raw)
		c.setToolMeta(meta)
		meta[0] = ' '
		c.resolveCallerMeta(Proc{Env: []string{"CODEX_THREAD_ID=other-session"}})
		if string(c.toolMeta) != raw {
			t.Fatalf("host context replaced: %s", c.toolMeta)
		}
	}
}

// Exercise the actual bridge wire call, including host metadata. Metadata
// must live with this bridge, never in a process-wide session variable.
func TestCodexCallPreservesCallerToolMetadata(t *testing.T) {
	for _, raw := range []string{`{"x-codex-turn-metadata":{"session_id":"caller-session","turn_id":"caller-turn"}}`, ""} {
		t.Run(raw, func(t *testing.T) {
			r, w := io.Pipe()
			defer r.Close()
			defer w.Close()
			c := &Codex{in: w, thread: "bridge-thread", pending: map[int]chan map[string]any{}}
			// The constructor receives the host's per-request context.
			c.setToolMeta(json.RawMessage(raw))
			done := make(chan error, 1)
			go func() { _, err := c.Call(bind.Tool{Server: "ui", Name: "read"}, map[string]any{}); done <- err }()
			sc := bufio.NewScanner(r)
			if !sc.Scan() {
				t.Fatal("bridge sent no call")
			}
			var msg map[string]any
			if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
				t.Fatal(err)
			}
			params := msg["params"].(map[string]any)
			meta, has := params["_meta"]
			if raw == "" && has {
				t.Fatalf("context leaked into a context-free call: %v", params)
			}
			if raw != "" {
				envelope, ok := meta.(map[string]any)
				if !ok {
					t.Fatalf("invalid envelope: %v", meta)
				}
				m, ok := envelope["x-codex-turn-metadata"].(map[string]any)
				if !ok || m["session_id"] != "caller-session" || m["turn_id"] != "caller-turn" {
					t.Errorf("caller context lost: %v", params)
				}
			}
			c.mu.Lock()
			ch := c.pending[int(msg["id"].(float64))]
			c.mu.Unlock()
			ch <- map[string]any{"result": map[string]any{"content": []any{}}}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
