package author

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// saved writes a primitive into a collection root the way a save leaves it.
func saved(t *testing.T, root, name, tools string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: local.me, name: " + name + ", version: 0.1.0}\nexecution: {entrypoint: main.py}\n" + tools
	os.WriteFile(filepath.Join(dir, "primitive.yaml"), []byte(manifest), 0o600)
	os.WriteFile(filepath.Join(dir, "main.py"), []byte("print('x')\n"), 0o600)
}

// A recorded session of one request that made these MCP calls, then a shell one.
func session() trace.Session {
	call := func(server, tool string) trace.Call {
		return trace.Call{Client: "claude", Session: "s1", Tool: "mcp:" + tool, MCPServer: server, MCPTool: tool, Request: 0}
	}
	return trace.Session{Client: "claude", ID: "s1", Requests: []string{"check the release and tell the team"}, Calls: []trace.Call{
		call("tracker", "search_issues"),
		call("chat", "find_channel"),
		call("chat", "post_message"),
		{Client: "claude", Session: "s1", Tool: "shell", Command: "git log -1", Request: 0},
	}}
}

func TestABriefProposesAnInstalledPrimitiveWhoseWholeToolSequenceItContains(t *testing.T) {
	root := t.TempDir()
	// Both tools, pinned, in this order: contained.
	saved(t, root, "notify", "tools:\n  - {alias: find, capability: chat.channels.find, effect: read, pin: {server: chat, tool: find_channel}}\n  - {alias: post, capability: chat.messages.post, effect: write, pin: {server: chat, tool: post_message}}\n")
	// The same tools in the other order: not the same sequence.
	saved(t, root, "reversed", "tools:\n  - {alias: post, capability: chat.messages.post, effect: write, pin: {server: chat, tool: post_message}}\n  - {alias: find, capability: chat.channels.find, effect: read, pin: {server: chat, tool: find_channel}}\n")
	// One of the sequence, with a tool the calls do not make: not contained.
	saved(t, root, "extra", "tools:\n  - {alias: find, capability: chat.channels.find, effect: read, pin: {server: chat, tool: find_channel}}\n  - {alias: del, capability: chat.messages.delete, effect: destructive, pin: {server: chat, tool: delete_message}}\n")
	// Right tool names on another server: not the same tools.
	saved(t, root, "other-server", "tools:\n  - {alias: find, capability: chat.channels.find, effect: read, pin: {server: elsewhere, tool: find_channel}}\n")
	// A tool without a pin has no sequence to compare.
	saved(t, root, "unpinned", "tools:\n  - {alias: find, capability: chat.channels.find, effect: read}\n")
	// Not contiguous in the calls: the search sits between nothing here, so
	// a pair that skips a call is not contained.
	saved(t, root, "gappy", "tools:\n  - {alias: search, capability: tracker.issues.search, effect: read, pin: {server: tracker, tool: search_issues}}\n  - {alias: post, capability: chat.messages.post, effect: write, pin: {server: chat, tool: post_message}}\n")
	// A folder that is not a package.
	os.MkdirAll(filepath.Join(root, "notes"), 0o700)

	b, err := NewBrief(session(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := SuggestUses(b.Evidence.Steps, InstalledPrimitives(root))
	if len(got) != 1 {
		t.Fatalf("proposed %+v, want only notify", got)
	}
	u := got[0]
	if u.Ref != "local.me/notify" || u.Alias != "notify" || u.Effect != "write" || u.Version != "0.1.0" || len(u.Digest) != 64 {
		t.Fatalf("the proposal is %+v", u)
	}
	if len(u.Steps) != 2 || u.Steps[0] != 2 || u.Steps[1] != 3 {
		t.Fatalf("it covers steps %v, want 2 and 3", u.Steps)
	}
	// The brief says so, with the block to paste.
	b.Uses = got
	md := b.Markdown()
	for _, want := range []string{"Installed primitives that already do part of this", "uses:", "ref: local.me/notify", "digest: " + u.Digest, "effect: write"} {
		if !strings.Contains(md, want) {
			t.Errorf("BRIEF.md does not contain %q", want)
		}
	}
	// A brief with nothing to propose does not mention it.
	b.Uses = nil
	if strings.Contains(b.Markdown(), "Installed primitives") {
		t.Error("a brief with no proposal mentions installed primitives")
	}
}

func TestNoPrimitiveIsProposedForCallsThatShareOnlyAName(t *testing.T) {
	root := t.TempDir()
	saved(t, root, "lookalike", "tools:\n  - {alias: s, capability: tracker.issues.search, effect: read, pin: {server: tracker, tool: search_issue}}\n")
	b, _ := NewBrief(session(), 0, nil)
	if got := SuggestUses(b.Evidence.Steps, InstalledPrimitives(root)); len(got) != 0 {
		t.Fatalf("a near name was matched: %+v", got)
	}
}
