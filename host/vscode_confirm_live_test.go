package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	liveVSCodeConfirm = flag.Bool("live-vscode-confirm", false, "open a real VS Code (its own profile) and answer the runner's confirmations in VS Code's own UI")
	liveEvidenceDir   = flag.String("live-evidence", "", "with a live test: copy what each run showed and answered into this directory")
)

// A primitive's change in VS Code is put in front of the person by VS Code
// itself. The runner asks with an MCP elicitation; VS Code's MCP client shows
// it (in the chat that called tap_run, or, with no chat, as a notification
// with a form), and the answer comes back to the runner. Here a real VS Code,
// with the TAP extension and a profile of the test's own, runs primitives
// through the extension's MCP server, and a helper extension answers each
// form through VS Code's own commands: the notification's Respond button,
// then the form's fields. VS Code's confirmation of each tool call itself is
// approved by the profile's chat.tools.global.autoApprove, so only the
// runner's question stands between the primitive and the change.
//
// Run with: go test ./host -run LiveVSCodeConfirm -live-vscode-confirm
func TestLiveVSCodeConfirmsAPrimitivesChangeInItsOwnUI(t *testing.T) {
	code := "/Applications/Visual Studio Code.app/Contents/MacOS/Code"
	if !*liveVSCodeConfirm {
		t.Skip("pass -live-vscode-confirm to open a real VS Code")
	}
	if _, err := os.Stat(code); err != nil {
		t.Skip("VS Code is not at " + code)
	}
	tap := buildTap(t)
	cache, _ := os.UserCacheDir()
	interp := filepath.Join(cache, "tap-runtime", "interpreters")
	// A short path: VS Code refuses a user-data directory whose socket path is long.
	root, err := os.MkdirTemp("/tmp", "vsc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	home, work := filepath.Join(root, "home"), filepath.Join(root, "work")
	for _, d := range []string{home, filepath.Join(work, "out"), filepath.Join(root, "user", "User"), filepath.Join(root, "starter")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// The runner keeps its collection and trust under a home of the test's own.
	wrapper := filepath.Join(root, "tapw")
	os.WriteFile(wrapper, []byte(fmt.Sprintf("#!/bin/sh\ncd %q\nHOME=%q exec %q \"$@\" --interpreters %q\n", work, home, tap, interp)), 0o700)
	save := func(dir string) {
		cmd := exec.Command(tap, "discover", "save", dir)
		cmd.Env = append(os.Environ(), "HOME="+home)
		if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "saved") {
			t.Fatalf("saving %s: %v\n%s", dir, err, out)
		}
	}
	save(authoredPackage(t, "local.me", "note-writer", "Write a short note to out/note.txt",
		"execution: {entrypoint: main.py}\nfiles:\n  - {path: out, access: write}\n",
		"main.py", "import sys\ntap.write(\"out/note.txt\", sys.argv[1] + \"\\n\")\nprint(\"wrote out/note.txt:\", sys.argv[1])\n"))
	smoke, _ := os.ReadFile(filepath.Join(repoRoot, "examples", "browser-smoke", "primitive.yaml"))
	smokeMain, _ := os.ReadFile(filepath.Join(repoRoot, "examples", "browser-smoke", "main.py"))
	smokeDir := authoredPackage(t, "examples.telara.dev", "browser-smoke", "Navigate a local fixture and verify its rendered heading", "", "main.py", string(smokeMain))
	os.WriteFile(filepath.Join(smokeDir, "primitive.yaml"), smoke, 0o600)
	os.WriteFile(filepath.Join(smokeDir, "CHANGELOG.md"), []byte("## 0.1.1\n\n- Example.\n"), 0o600)
	_, npxErr := exec.LookPath("npx")
	if npxErr == nil {
		save(smokeDir)
	}

	os.WriteFile(filepath.Join(root, "user", "User", "settings.json"), []byte(fmt.Sprintf(
		`{"tapRuntime.path":%q,"chat.tools.global.autoApprove":true,"workbench.startupEditor":"none","extensions.ignoreRecommendations":true,"telemetry.telemetryLevel":"off"}`, wrapper)), 0o600)
	os.WriteFile(filepath.Join(root, "user", "User", "mcp.json"), []byte(
		`{"servers":{"playwright":{"type":"stdio","command":"npx","args":["@playwright/mcp@latest","--headless","--isolated"]}}}`), 0o600)
	os.WriteFile(filepath.Join(root, "starter", "package.json"), []byte(`{"name":"starter","publisher":"t","version":"0.0.1","engines":{"vscode":"^1.101.0"},"main":"./e.js","activationEvents":["*"]}`), 0o600)
	os.WriteFile(filepath.Join(root, "starter", "e.js"), []byte(confirmStarter), 0o600)

	// The fixture site the browser primitive checks.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.FileServer(http.Dir(filepath.Join(repoRoot, "examples", "browser-support", "site")))}
	go srv.Serve(ln)
	defer srv.Close()
	base := "http://" + ln.Addr().String()

	ext := filepath.Join(repoRoot, "vscode")
	run := func(name string, plan map[string]any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(plan)
		os.WriteFile(filepath.Join(root, "plan.json"), b, 0o600)
		os.Remove(filepath.Join(root, "result.json"))
		user := filepath.Join(root, "user")
		cmd := exec.Command(code, "--log", "trace", "--user-data-dir", user, "--extensions-dir", filepath.Join(root, "exts"),
			"--extensionDevelopmentPath", ext, "--extensionDevelopmentPath", filepath.Join(root, "starter"), "--new-window", work)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		stop := func() {
			// The next run must start a new VS Code, not hand its arguments
			// to this one; a window can outlive SIGTERM.
			exec.Command("pkill", "-f", user).Run()
			for i := 0; i < 30 && exec.Command("pgrep", "-f", user).Run() == nil; i++ {
				if i == 10 {
					exec.Command("pkill", "-9", "-f", user).Run()
				}
				time.Sleep(time.Second)
			}
			cmd.Process.Kill()
			cmd.Wait()
		}
		defer stop()
		var res map[string]any
		for i := 0; i < 120 && res == nil; i++ {
			time.Sleep(2 * time.Second)
			if b, err := os.ReadFile(filepath.Join(root, "result.json")); err == nil {
				json.Unmarshal(b, &res)
			}
		}
		if res == nil {
			t.Fatalf("%s: VS Code gave no result", name)
		}
		if *liveEvidenceDir != "" {
			out, _ := json.MarshalIndent(res, "", " ")
			os.MkdirAll(*liveEvidenceDir, 0o700)
			os.WriteFile(filepath.Join(*liveEvidenceDir, "vscode-"+name+".json"), out, 0o600)
		}
		t.Logf("%s: %v", name, res["run"])
		return res
	}
	text := func(res map[string]any) string {
		r, _ := res["run"].(map[string]any)
		s, _ := r["text"].(string)
		e, _ := r["error"].(string)
		return s + e
	}
	note := filepath.Join(work, "out", "note.txt")

	res := run("note-approve", map[string]any{"query": "write a note file", "name": "local.me/note-writer", "args": []string{"approved-in-vscode"}, "answers": []string{"approve", "approve"}})
	if b, _ := os.ReadFile(note); strings.TrimSpace(string(b)) != "approved-in-vscode" || len(res["replies"].([]any)) != 2 {
		t.Errorf("approved in VS Code, the note is %q: %s", b, text(res))
	}
	os.Remove(note)
	res = run("note-decline", map[string]any{"query": "write a note file", "name": "local.me/note-writer", "args": []string{"declined-in-vscode"}, "answers": []string{"decline"}})
	if _, err := os.Stat(note); err == nil || !strings.Contains(text(res), "1 refused") {
		t.Errorf("declined in VS Code, the note was written or not refused: %s", text(res))
	}
	if npxErr != nil {
		t.Log("npx is not installed: the Playwright legs were not run")
		return
	}
	input := `{"base_url":"` + base + `"}`
	res = run("browser-approve", map[string]any{"query": "check the browser smoke fixture", "name": "examples.telara.dev/browser-smoke", "args": []string{input}, "answers": []string{"approve", "approve", "approve", "approve"}})
	if !strings.Contains(text(res), `"status": "passed"`) {
		t.Errorf("approved in VS Code, the browser check did not pass: %s", text(res))
	}
	res = run("browser-decline", map[string]any{"query": "check the browser smoke fixture", "name": "examples.telara.dev/browser-smoke", "args": []string{input}, "answers": []string{"decline"}})
	if !strings.Contains(text(res), "0 action(s) run, 1 refused") {
		t.Errorf("declined in VS Code, the browser was still driven: %s", text(res))
	}
}

// authoredPackage writes a package with the AUTHORING.json tap discover save
// needs. body follows the metadata line of primitive.yaml.
func authoredPackage(t *testing.T, publisher, name, goal, body, file, program string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	os.MkdirAll(dir, 0o700)
	contract := map[string]any{}
	for _, f := range []string{"goal", "inputs", "scope", "procedure", "output", "oracle", "failures", "boundary"} {
		contract[f] = map[string]any{"value": goal + ": " + f, "established_by": "the test"}
	}
	a, _ := json.Marshal(map[string]any{"kind": "tap.authoring/v1", "name": name, "publisher": publisher, "author": "host-agent",
		"agent": "vscode", "selection": "selected_task", "sources": []string{"src_0123456789ab"}, "brief_digest": "sha256:00",
		"contract": contract, "interface": map[string]any{"args": []string{"input"}}})
	os.WriteFile(filepath.Join(dir, "AUTHORING.json"), a, 0o600)
	os.WriteFile(filepath.Join(dir, "primitive.yaml"), []byte(fmt.Sprintf(
		"apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: %s, name: %s, version: 0.1.0, description: %s}\n%s", publisher, name, goal, body)), 0o600)
	os.WriteFile(filepath.Join(dir, file), []byte(program), 0o600)
	os.WriteFile(filepath.Join(dir, "CHANGELOG.md"), []byte("## 0.1.0\n\n- First version.\n"), 0o600)
	return dir
}

// confirmStarter runs one primitive through the TAP extension's MCP server
// and answers each of the runner's forms through VS Code's own commands, as
// plan.json says. VS Code's trace log of the server's traffic says when a
// form was sent and what VS Code answered.
const confirmStarter = `const vscode=require("vscode");const fs=require("fs");const path=require("path");
const ROOT=path.join(__dirname,"..");
const plan=JSON.parse(fs.readFileSync(path.join(ROOT,"plan.json"),"utf8"));
const sleep=ms=>new Promise(r=>setTimeout(r,ms));
const text=r=>r.content.map(p=>p.value).join("");
const cmd=(...a)=>vscode.commands.executeCommand(...a);
exports.activate=async(ctx)=>{ const result={answers:[]}; try {
 const dir=path.join(ctx.logUri.fsPath,"..","..");
 const mcpLog=()=>{const f=fs.readdirSync(dir).find(n=>n.startsWith("mcpServer.")&&n.endsWith("TAP Runtime.log"));return f?fs.readFileSync(path.join(dir,f),"utf8"):""};
 // The profile's global auto-approve needs a one-time opt-in; this is the
 // switch VS Code's own tests use to skip that dialog.
 await cmd("setContext","vscode.chat.tools.global.autoApprove.testMode",true);
 let s;
 for (let i=0;i<60&&!s;i++){ await sleep(2000);
   try { await cmd("workbench.mcp.startServer","*",{waitForLiveTools:true}); } catch(e){}
   s=vscode.lm.tools.find(t=>/tap_search$/.test(t.name)); }
 const tool=n=>vscode.lm.tools.find(t=>t.name.startsWith("mcp_tap")&&t.name.endsWith(n)).name;
 const call=async(n,input)=>text(await vscode.lm.invokeTool(tool(n),{input,toolInvocationToken:undefined}));
 const sr=await call("tap_search",{query:plan.query});
 const ref=sr.match(new RegExp("("+plan.name.replace(/[.\/]/g,"\\$&")+"@\\S+)"))[1], digest=sr.match(/Digest: (\w+)/)[1];
 result.ref=ref; result.digest=digest; result.vscode=vscode.version;
 await call("tap_load",{ref,digest});
 await cmd("notifications.clearAll");
 let done; vscode.lm.invokeTool(tool("tap_run"),{input:{ref,digest,args:plan.args},toolInvocationToken:undefined}).then(r=>{done={text:text(r)}},e=>{done={error:String(e)}});
 let seen=0; const answers=plan.answers.slice();
 for (let i=0;i<150&&!done;i++){ await sleep(1000);
   const forms=mcpLog().split("\n").filter(l=>l.includes("[server -> editor]")&&l.includes("elicitation/create"));
   while (seen<forms.length){ const m=JSON.parse(forms[seen].slice(forms[seen].indexOf("{"))); seen++;
     const ans=answers.shift()||"decline";
     await sleep(1500);
     await cmd("notification.acceptPrimaryAction");
     await sleep(1500);
     for (const sch of Object.values(m.params.requestedSchema.properties)) {
       if (sch.type==="boolean") await cmd(ans==="approve"?"quickInput.first":"quickInput.last");
       await sleep(700); await cmd("quickInput.accept"); await sleep(1000);
     }
     result.answers.push({answer:ans,message:m.params.message});
   } }
 result.run=done||{error:"no result within 150 s"};
 result.replies=mcpLog().split("\n").filter(l=>l.includes("[editor -> server]")&&l.includes('"action"')).map(l=>JSON.parse(l.slice(l.indexOf("{"))).result);
} catch(e){ result.error=String(e&&e.stack||e) }
 fs.writeFileSync(path.join(ROOT,"result.json"),JSON.stringify(result,null,1)); };
`
