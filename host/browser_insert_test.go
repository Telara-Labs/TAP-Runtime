package main

import (
	"strings"
	"testing"
)

// TestInsertTextReachesEveryBackend checks that the insert_text action is a
// gated write, that it needs its text, and that each backend receives the
// text: Codex through its Playwright locator's fill, Claude in Chrome
// through the page function the runner runs.
func TestInsertTextReachesEveryBackend(t *testing.T) {
	text := "Hey Omar, quick update.\n\nSecond paragraph with \"quotes\" and a link: https://example.com/x?a=1&b=2"

	c := &browserClient{fakeBridge: fakeBridge{inv: codexInv()}}
	a, _ := admit(browserDecl(), c)
	call(a, c, true, map[string]any{"op": "navigate", "url": "http://localhost:4173/counter.html"})
	if r := call(a, c, false, map[string]any{"op": "act", "action": "insert_text", "selector": "#note", "text": text}); !r.Gated {
		t.Fatalf("an unapproved insert_text ran: %+v", r)
	}
	if r := call(a, c, true, map[string]any{"op": "act", "action": "insert_text", "selector": "#note"}); !strings.Contains(r.Refused+r.Result+r.Stderr, "needs text") {
		t.Fatalf("insert_text without text = %+v", r)
	}
	call(a, c, true, map[string]any{"op": "act", "action": "insert_text", "selector": "#note", "index": float64(1), "text": text})
	code := c.args[len(c.args)-1]["code"].(string)
	if want := `.playwright.locator("#note").nth(1).fill(` + jsString(text) + `)`; !strings.Contains(code, want) {
		t.Fatalf("Codex call %q has no %q", code, want)
	}
	a.closeBrowsers()

	ch := chromeClient(func(string) string { return encoded(true) })
	a, _ = admit(browserDecl(), ch)
	call(a, ch, true, map[string]any{"op": "navigate", "url": "http://localhost:4173/counter.html"})
	if r := call(a, ch, true, map[string]any{"op": "act", "action": "insert_text", "selector": "#note", "text": text}); r.Refused != "" || r.Result != `{"ok":true}` {
		t.Fatalf("Chrome insert_text: %+v", r)
	}
	program := ch.args[len(ch.args)-1]["text"].(string)
	if !strings.Contains(program, "execCommand('insertText'") {
		t.Fatalf("Chrome program does not insert text: %s", program)
	}
	a.closeBrowsers()
}
