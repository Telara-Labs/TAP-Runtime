package bridge

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/bind"
)

// These run against the real clients installed on this machine, through their
// real control channels. They are skipped where a client is not installed.

func TestLiveClaudeInventoryAndDenyRules(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude is not installed")
	}
	// The deny rule is given to this one process. No settings file is changed.
	c, err := NewClaude("--disallowedTools", "mcp__telara__telara_task_list")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	name, version := c.Client()
	t.Logf("client %s %s, tested=%v", name, version, Tested(name, version))

	inv, err := c.Inventory()
	if err != nil {
		t.Fatal(err)
	}
	if len(inv) == 0 {
		t.Skip("this Claude Code has no connected MCP servers")
	}
	counts := map[bind.Effect]int{}
	for _, tool := range inv {
		counts[tool.Annotated]++
	}
	t.Logf("%d tools: %v", len(inv), counts)

	denied, err := c.Denied(bind.Tool{Server: "telara", Name: "telara_task_list"})
	if err != nil {
		t.Fatal(err)
	}
	if !denied {
		t.Fatalf("a tool named in --disallowedTools is not reported as denied; rules seen: %v", c.deny)
	}
	other, err := c.Denied(bind.Tool{Server: "telara", Name: "telara_knowledge_search"})
	if err != nil {
		t.Fatal(err)
	}
	if other {
		t.Fatal("a tool nobody denied is reported as denied")
	}
}

func TestLiveCodexInventory(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex is not installed")
	}
	c, err := NewCodex()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	name, version := c.Client()
	t.Logf("client %s %s, tested=%v", name, version, Tested(name, version))
	if version == "" {
		t.Fatal("no version read from codex")
	}
	inv, err := c.Inventory()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[bind.Effect]int{}
	for _, tool := range inv {
		counts[tool.Annotated]++
	}
	t.Logf("%d tools: %v", len(inv), counts)
	if len(inv) == 0 {
		t.Skip("this Codex has no MCP servers")
	}
}

// Codex answers config/read with its merged configuration, and the
// rules are read from it. This reads the real one and checks only that the
// read works and is consistent with the tools the same Codex lists.
func TestLiveCodexRulesAreReadable(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex is not installed")
	}
	c, err := NewCodex()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	rs, err := c.rules()
	if err != nil {
		t.Fatalf("config/read: %v", err)
	}
	t.Logf("%d MCP servers have rules", len(rs))
	for server := range rs {
		if _, err := c.Denied(bind.Tool{Server: server, Name: "x"}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Asks(bind.Tool{Server: server, Name: "x"}); err != nil {
			t.Fatal(err)
		}
	}
}

var liveVSCode = flag.Bool("live-vscode", false, "launch a real VS Code with the TAP extension (opens a window, then closes it)")

// The VS Code ask rule, through the real extension in a real VS
// Code. Run with: go test ./bridge -run LiveVSCode -live-vscode
func TestLiveVSCodeAskRulesThroughTheRealExtension(t *testing.T) {
	code := "/Applications/Visual Studio Code.app/Contents/MacOS/Code"
	if !*liveVSCode {
		t.Skip("pass -live-vscode to open a real VS Code")
	}
	if _, err := os.Stat(code); err != nil {
		t.Skip("VS Code is not at " + code)
	}
	// A short path: VS Code refuses a user-data directory whose socket path is long.
	root, err := os.MkdirTemp("/tmp", "vsc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	os.MkdirAll(filepath.Join(root, "user", "User"), 0o755)
	os.WriteFile(filepath.Join(root, "user", "User", "settings.json"),
		[]byte(`{"chat.tools.eligibleForAutoApproval":{"probe/search_issues":false,"runTask":false,"other":true}}`), 0o644)
	// A real MCP server in this VS Code, started by a command: starting a
	// server explicitly is what stands in for a person trusting it.
	os.WriteFile(filepath.Join(root, "srv.js"), []byte(liveMCPServer), 0o644)
	os.WriteFile(filepath.Join(root, "user", "User", "mcp.json"),
		[]byte(`{"servers":{"probe":{"type":"stdio","command":"node","args":["`+filepath.Join(root, "srv.js")+`"]}}}`), 0o644)
	os.MkdirAll(filepath.Join(root, "starter"), 0o755)
	os.WriteFile(filepath.Join(root, "starter", "package.json"), []byte(`{"name":"starter","publisher":"t","version":"0.0.1","engines":{"vscode":"^1.100.0"},"main":"./e.js","activationEvents":["*"]}`), 0o644)
	os.WriteFile(filepath.Join(root, "starter", "e.js"), []byte(`const vscode=require("vscode");
exports.activate=async()=>{await new Promise(r=>setTimeout(r,6000));
 for (const id of ["mcp.config.usrlocal.probe","probe"]) { try { await vscode.commands.executeCommand("workbench.mcp.startServer", id); } catch {} } };`), 0o644)
	ext, _ := filepath.Abs(filepath.Join("..", "vscode"))
	cacheDir := filepath.Join(os.Getenv("HOME"), "Library", "Caches", "tap-runtime", "vscode")
	before, _ := filepath.Glob(filepath.Join(cacheDir, "*.sock"))
	cmd := exec.Command(code, "--user-data-dir", filepath.Join(root, "user"), "--extensions-dir", filepath.Join(root, "exts"),
		"--extensionDevelopmentPath", ext, "--extensionDevelopmentPath", filepath.Join(root, "starter"), "--new-window")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); exec.Command("pkill", "-f", filepath.Join(root, "user")).Run() }()
	var v *VSCode
	for i := 0; i < 40 && v == nil; i++ {
		time.Sleep(time.Second)
		socks, _ := filepath.Glob(filepath.Join(cacheDir, "*.sock"))
		for _, s := range socks {
			isNew := true
			for _, b := range before {
				if b == s {
					isNew = false
				}
			}
			if isNew {
				if c, err := NewVSCode(s); err == nil {
					v = c
					break
				}
			}
		}
	}
	if v == nil {
		t.Fatal("the extension's socket did not appear")
	}
	defer v.Close()
	name, version := v.Client()
	t.Logf("client %s %s", name, version)
	var inv []bind.Tool
	for i := 0; i < 30; i++ {
		inv, err = v.Inventory()
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, tool := range inv {
			found = found || tool.Name == "mcp_probe_search_issues"
		}
		if found {
			break
		}
		time.Sleep(time.Second)
	}
	asked := map[string]bool{}
	for _, tool := range inv {
		ask, err := v.Asks(tool)
		if err != nil {
			t.Fatal(err)
		}
		if ask {
			asked[tool.Name] = true
		}
	}
	t.Logf("tools the setting makes ask: %v", asked)
	if !asked["mcp_probe_search_issues"] || asked["mcp_probe_create_issue"] {
		t.Errorf("the setting's probe/search_issues key did not make exactly that MCP tool ask: %v", asked)
	}
	if !asked["run_task"] {
		t.Errorf("the setting's runTask key did not make run_task ask: %v", asked)
	}
	if asked["get_task_output"] || asked["vscode_listCodeUsages"] {
		t.Errorf("a tool the setting does not name asks: %v", asked)
	}
}

const liveMCPServer = `let buf="";process.stdin.on("data",d=>{buf+=d;let i;while((i=buf.indexOf("\n"))>=0){const l=buf.slice(0,i);buf=buf.slice(i+1);if(!l.trim())continue;const m=JSON.parse(l);const r=(res)=>process.stdout.write(JSON.stringify({jsonrpc:"2.0",id:m.id,result:res})+"\n");
if(m.method==="initialize")r({protocolVersion:m.params.protocolVersion,capabilities:{tools:{}},serverInfo:{name:"probe",version:"1"}});
else if(m.method==="tools/list")r({tools:[{name:"search_issues",description:"x",inputSchema:{type:"object",properties:{}},annotations:{readOnlyHint:true}},{name:"create_issue",description:"y",inputSchema:{type:"object",properties:{}}}]});
else if(m.id!==undefined)r({});}});`
