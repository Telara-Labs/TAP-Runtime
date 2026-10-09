package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/bridge"
)

var liveBrowserAnyClient = flag.Bool("live-browser-any-client", false, "run one browser primitive on every browser the installed clients lend (opens tabs in the person's browsers; the Codex leg spends model tokens)")

// browserAnyClientManifest declares only the runner's browser: no pin, no
// per-client tool, nothing that names Claude in Chrome, Codex or Playwright.
const browserAnyClientManifest = `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.test, name: browser-any-client, version: 0.1.0}
interface:
  inputSchema:
    type: object
    properties:
      base_url: {type: string}
      backend: {type: string}
    required: [base_url, backend]
execution: {entrypoint: main.py, timeoutSeconds: 180}
tools:
  - {alias: browser, capability: tap.browser.use, effect: write}
`

// browserAnyClientProgram reads the counter page, presses its button through
// the runner's act operation, and reads the page again. The backend is an
// input only so one run can be pinned to each browser a client lends; the
// program has no code for any of them.
const browserAnyClientProgram = `import json, sys
a = json.loads(sys.argv[1])
def b(**kw):
    v = tap.call('browser', kw)
    return json.loads(v) if isinstance(v, str) else v
read = "() => ({heading: document.querySelector('h1').textContent, count: document.getElementById('count').textContent})"
# A cold browser can take a while to start: allow a minute, and never read a
# page the ready check did not confirm.
nav = b(op='navigate', url=a['base_url'] + '/counter.html', backend=a['backend'],
        ready="() => document.getElementById('count') ? true : null", ready_timeout_ms=60000)
if nav.get('ready') is not True:
    b(op='close')
    raise RuntimeError('the page never became ready: ' + json.dumps(nav))
try:
    before = b(op='read', function=read)
    b(op='act', action='click', selector='#add')
    after = b(op='read', function=read)
finally:
    b(op='close')
print('RESULT ' + json.dumps({'backend': nav.get('backend'), 'ready': nav.get('ready'), 'before': before, 'after': after}))
`

// counterResult is the RESULT line the program prints.
type counterResult struct {
	Backend string `json:"backend"`
	Ready   any    `json:"ready"`
	Before  struct {
		Heading string `json:"heading"`
		Count   string `json:"count"`
	} `json:"before"`
	After struct {
		Heading string `json:"heading"`
		Count   string `json:"count"`
	} `json:"after"`
}

// The result object ends with the "after" object, so it ends "}}"; stop
// there, since clients may put more text on the same line.
var counterResultLine = regexp.MustCompile(`RESULT (\{.*?\}\})`)

// checkCounterRun says why a run's output does not show the page read, the
// button pressed and the page read again on the backend that was asked for.
func checkCounterRun(out, backend string) error {
	m := counterResultLine.FindAllStringSubmatch(strings.ReplaceAll(out, `\"`, `"`), -1)
	if len(m) == 0 {
		return fmt.Errorf("the program printed no RESULT line")
	}
	var r counterResult
	if err := json.Unmarshal([]byte(m[len(m)-1][1]), &r); err != nil {
		return fmt.Errorf("the RESULT line is not JSON: %v", err)
	}
	if r.Backend != backend {
		return fmt.Errorf("ran on %q, asked for %q", r.Backend, backend)
	}
	if r.Before.Heading != "TAP Browser Counter" || r.After.Heading != "TAP Browser Counter" {
		return fmt.Errorf("the heading read %q then %q", r.Before.Heading, r.After.Heading)
	}
	b, err1 := strconv.Atoi(r.Before.Count)
	a, err2 := strconv.Atoi(r.After.Count)
	if err1 != nil || err2 != nil || a != b+1 {
		return fmt.Errorf("the count read %q then %q; the click did not add one", r.Before.Count, r.After.Count)
	}
	return nil
}

// notLent is how the runner says a client does not lend a browser, or not
// the one asked for. A run that ends this way was not run on that backend.
func notLent(text string) bool {
	return strings.Contains(text, "this client lends no browser") || strings.Contains(text, "is not lent by this client")
}

// browserFixtureBase serves the counter fixture on the port the browser
// examples use, or reuses a server already there when it serves the same
// page. Claude in Chrome's site permission is per origin, port included.
func browserFixtureBase(t *testing.T) string {
	t.Helper()
	base := "http://localhost:" + strconv.Itoa(browserFixturePort)
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(browserFixturePort))
	if err != nil {
		c := http.Client{Timeout: 3 * time.Second}
		resp, gerr := c.Get(base + "/counter.html")
		if gerr != nil {
			t.Skipf("port %d is in use and does not serve the fixture: %v", browserFixturePort, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), "<title>TAP Browser Counter</title>") {
			t.Skipf("port %d is in use by something other than the fixture", browserFixturePort)
		}
		return base
	}
	srv := &http.Server{Handler: http.FileServer(http.Dir(filepath.Join(repoRoot, "examples", "browser-support", "site")))}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return base
}

func writeBrowserAnyClientPackage(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "primitive.yaml"), []byte(browserAnyClientManifest), 0o600)
	os.WriteFile(filepath.Join(dir, "main.py"), []byte(browserAnyClientProgram), 0o600)
}

// TestLiveBrowserPrimitiveRunsOnEveryClientBrowser runs one browser
// primitive, unchanged, on every browser each installed client lends: Claude
// Code with Claude in Chrome, Claude Code with a Playwright MCP server, and
// Codex with its own browser, which answers only inside a Codex turn, so
// that leg is started as a real turn that calls tap_run. Each leg asks for
// one backend and checks that the run happened there. A backend the client
// does not lend on this machine is reported as not run (skipped), never as
// passed.
//
// It drives the person's real browsers against a localhost fixture: one tab
// is opened and closed per leg. The Codex leg spends model tokens.
func TestLiveBrowserPrimitiveRunsOnEveryClientBrowser(t *testing.T) {
	if !*liveBrowserAnyClient {
		t.Skip("pass -live-browser-any-client to drive the person's real browsers")
	}
	store := interpreterStore(t)
	if _, _, _, err := obtain(store, "main.py"); err != nil {
		t.Skipf("the Python interpreter could not be obtained: %v", err)
	}
	base := browserFixtureBase(t)

	for _, backend := range []string{backendChrome, backendPlaywright} {
		t.Run("claude/"+backend, func(t *testing.T) {
			if _, err := exec.LookPath("claude"); err != nil {
				t.Skip("not run: claude is not installed")
			}
			inDir(t)
			runInClient(t, "claude", store, base, backend)
		})
	}

	// Kilo lends whatever MCP servers its configuration names. The person's
	// Kilo has no browser here, so this run adds a Playwright MCP server in
	// a project kilo.json in the run's own directory; the person's
	// configuration is not changed.
	t.Run("kilo/"+backendPlaywright, func(t *testing.T) {
		if _, err := exec.LookPath("kilo"); err != nil {
			t.Skip("not run: kilo is not installed")
		}
		if _, err := exec.LookPath("npx"); err != nil {
			t.Skip("not run: npx is not installed, so Playwright MCP cannot start")
		}
		dir := inDir(t)
		cfg := `{"$schema": "https://app.kilo.ai/config.json", "mcp": {"playwright": {"type": "local", "command": ["npx", "@playwright/mcp@latest"]}}}`
		if err := os.WriteFile(filepath.Join(dir, "kilo.json"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		runInClient(t, "kilo", store, base, backendPlaywright)
	})

	// Goose lends its enabled extensions. This run adds a Playwright MCP
	// extension through Goose's own additional-configuration setting, for
	// this test process only; the person's config.yaml is not changed.
	t.Run("goose/"+backendPlaywright, func(t *testing.T) {
		if _, err := exec.LookPath("goose"); err != nil {
			t.Skip("not run: goose is not installed")
		}
		if _, err := exec.LookPath("npx"); err != nil {
			t.Skip("not run: npx is not installed, so Playwright MCP cannot start")
		}
		extra := filepath.Join(t.TempDir(), "playwright.yaml")
		// Goose opens no session without a model provider, and with its
		// claude-code provider it loads no MCP extension. The runner never
		// asks the model anything, so a provider with no model is enough.
		cfg := "GOOSE_PROVIDER: openai\nGOOSE_MODEL: none\nextensions:\n  playwright:\n    name: playwright\n    type: stdio\n    cmd: npx\n    args: ['@playwright/mcp@latest']\n    enabled: true\n    timeout: 300\n"
		if err := os.WriteFile(extra, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GOOSE_ADDITIONAL_CONFIG_FILES", extra)
		inDir(t)
		runInClient(t, "goose", store, base, backendPlaywright)
	})

	// OpenCode lends the MCP servers its configuration names, through its
	// server's call-tool route. This run adds Playwright in a project
	// opencode.json in the run's own directory. An OpenCode without the route
	// is reported as not run.
	t.Run("opencode/"+backendPlaywright, func(t *testing.T) {
		if _, err := exec.LookPath("opencode"); err != nil {
			t.Skip("not run: opencode is not installed")
		}
		if _, err := exec.LookPath("npx"); err != nil {
			t.Skip("not run: npx is not installed, so Playwright MCP cannot start")
		}
		dir := inDir(t)
		cfg := `{"$schema": "https://opencode.ai/config.json", "mcp": {"playwright": {"type": "local", "command": ["npx", "@playwright/mcp@latest"]}}}`
		if err := os.WriteFile(filepath.Join(dir, "opencode.json"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		runInClient(t, "opencode", store, base, backendPlaywright)
	})

	// Crush has no way to run a tool for another program: the runner
	// connects to the servers in Crush's own configuration that carry no
	// secret. This run adds Playwright in a project crush.json in the run's
	// own directory.
	t.Run("crush/"+backendPlaywright, func(t *testing.T) {
		if _, err := exec.LookPath("crush"); err != nil {
			t.Skip("not run: crush is not installed")
		}
		if _, err := exec.LookPath("npx"); err != nil {
			t.Skip("not run: npx is not installed, so Playwright MCP cannot start")
		}
		dir := inDir(t)
		cfg := `{"mcp": {"playwright": {"type": "stdio", "command": "npx", "args": ["@playwright/mcp@latest"]}}}`
		if err := os.WriteFile(filepath.Join(dir, "crush.json"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		runInClient(t, "crush", store, base, backendPlaywright)
	})

	// Copilot CLI lends the MCP servers in its configuration directory. This
	// run gives it a configuration directory of its own (Copilot's
	// COPILOT_HOME) whose only server is Playwright; the person's ~/.copilot
	// is not read or changed.
	t.Run("copilot/"+backendPlaywright, func(t *testing.T) {
		if _, err := exec.LookPath("copilot"); err != nil {
			t.Skip("not run: copilot is not installed")
		}
		if _, err := exec.LookPath("npx"); err != nil {
			t.Skip("not run: npx is not installed, so Playwright MCP cannot start")
		}
		home := t.TempDir()
		cfg := `{"mcpServers": {"playwright": {"type": "local", "command": "npx", "args": ["@playwright/mcp@latest"], "tools": ["*"]}}}`
		if err := os.WriteFile(filepath.Join(home, "mcp-config.json"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("COPILOT_HOME", home)
		inDir(t)
		runInClient(t, "copilot", store, base, backendPlaywright)
	})

	// Gemini lends its tools through the runner's relay hook. Gemini signs
	// in from a Gemini home of the test's own (GEMINI_CLI_HOME), with a
	// service account through Vertex AI or an API key; the person's own
	// settings are not read or changed. This leg spends one model run.
	t.Run("gemini/"+backendPlaywright, func(t *testing.T) {
		gemini, err := exec.LookPath("gemini")
		if err != nil {
			t.Skip("not run: gemini is not installed")
		}
		if _, err := exec.LookPath("npx"); err != nil {
			t.Skip("not run: npx is not installed, so Playwright MCP cannot start")
		}
		bin := buildRunner(t)
		work, _ := filepath.EvalSymlinks(t.TempDir())
		catalogRoot := filepath.Join(work, "catalog")
		runArgs := stageBrowserAnyClient(t, catalogRoot, base, backendPlaywright)
		journal := filepath.Join(work, "journal.jsonl")
		settings := map[string]any{
			"mcpServers": map[string]any{
				"tap": map[string]any{"command": bin, "args": []string{"serve", "--name", "tap", "--interpreters", store,
					"--runs", filepath.Join(work, "runs"), "--journal", journal, "--catalog-root", catalogRoot}},
				"playwright": map[string]any{"command": "npx", "args": []string{"@playwright/mcp@latest"}},
			},
			"hooks": map[string]any{"AfterTool": []any{map[string]any{"matcher": ".*", "hooks": []any{map[string]any{
				"name": "tap", "type": "command", "command": geminiHookCommand(bin), "timeout": 600000}}}}},
		}
		geminiHome := filepath.Join(t.TempDir(), "gemini-home")
		env := append(os.Environ(), "GEMINI_CLI_HOME="+geminiHome)
		switch sa, key := os.Getenv("GEMINI_SERVICE_ACCOUNT_JSON"), os.Getenv("GEMINI_API_KEY"); {
		case sa != "":
			var acct struct {
				ProjectID string `json:"project_id"`
			}
			if err := json.Unmarshal([]byte(sa), &acct); err != nil || acct.ProjectID == "" {
				t.Fatal("GEMINI_SERVICE_ACCOUNT_JSON is not a service account key")
			}
			keyFile := filepath.Join(t.TempDir(), "sa.json")
			os.WriteFile(keyFile, []byte(sa), 0o600)
			settings["security"] = map[string]any{"auth": map[string]any{"selectedType": "vertex-ai"}}
			env = append(env, "GOOGLE_APPLICATION_CREDENTIALS="+keyFile, "GOOGLE_GENAI_USE_VERTEXAI=true",
				"GOOGLE_CLOUD_PROJECT="+acct.ProjectID, "GOOGLE_CLOUD_LOCATION=us-central1")
		case key != "":
			settings["security"] = map[string]any{"auth": map[string]any{"selectedType": "gemini-api-key"}}
		default:
			t.Skip("not run: set GEMINI_SERVICE_ACCOUNT_JSON or GEMINI_API_KEY for Gemini to sign in")
		}
		// Gemini and the runner both read the servers from Gemini's home.
		b, _ := json.MarshalIndent(settings, "", "  ")
		os.MkdirAll(filepath.Join(geminiHome, ".gemini"), 0o700)
		os.WriteFile(filepath.Join(geminiHome, ".gemini", "settings.json"), b, 0o600)

		prompt := "Call the tool tap_run exactly once with these arguments: " + string(runArgs) +
			". If it hands back a run handle, call tap_result with it until the run finishes. Use no other tool. Then reply with exactly the text the run returned."
		cmd := exec.Command(gemini, "-p", prompt, "-m", "gemini-2.5-flash", "--approval-mode", "yolo", "--skip-trust", "-o", "json")
		cmd.Dir = work
		cmd.Env = env
		done := make(chan struct{})
		var out []byte
		go func() { out, err = cmd.CombinedOutput(); close(done) }()
		select {
		case <-done:
		case <-time.After(8 * time.Minute):
			cmd.Process.Kill()
			<-done
			t.Fatalf("gemini did not finish in 8 minutes:\n%.3000s", out)
		}
		text := string(out)
		j, _ := os.ReadFile(journal)
		t.Logf("gemini (%v):\n%.3000s\njournal:\n%.3000s", err, text, j)
		if strings.Contains(text, "Error authenticating") {
			t.Fatalf("Gemini did not sign in with the credential given")
		}
		if notLent(text) {
			t.Fatalf("Gemini has a Playwright MCP server, but the runner found no browser through it")
		}
		if err := checkCounterRun(text, backendPlaywright); err != nil {
			t.Fatal(err)
		}
	})

	// VS Code lends its language model tools, the MCP servers the person
	// connected included, through the TAP extension. This leg opens a VS Code
	// window with a throwaway profile whose only MCP server is Playwright,
	// and runs the primitive through the extension's bridge.
	t.Run("vscode/"+backendPlaywright, func(t *testing.T) {
		code := "/Applications/Visual Studio Code.app/Contents/MacOS/Code"
		if _, err := os.Stat(code); err != nil {
			t.Skip("not run: VS Code is not at " + code)
		}
		if _, err := exec.LookPath("npx"); err != nil {
			t.Skip("not run: npx is not installed, so Playwright MCP cannot start")
		}
		v := openVSCodeWithPlaywright(t, code)
		inDir(t)
		pkg := filepath.Join(t.TempDir(), "pkg")
		writeBrowserAnyClientPackage(t, pkg)
		args, _ := json.Marshal(map[string]string{"base_url": base, "backend": backendPlaywright})
		res, err := Run(context.Background(), Options{
			Package: pkg, Args: []string{string(args)}, Client: "vscode", Bridge: v,
			Journal: io.Discard, InterpDir: store, RunsDir: t.TempDir(),
			Approve: func(Ask) Grant { return Grant{OK: true, Limit: Unlimited} },
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("run %s, %d calls in %s\n%s\n%s", res.RunID, res.Calls, res.Elapsed.Round(time.Millisecond), res.Stdout, res.Stderr)
		if err := checkCounterRun(res.Stdout, backendPlaywright); err != nil {
			t.Fatalf("%v\nstdout:\n%s\nstderr:\n%s", err, res.Stdout, res.Stderr)
		}
	})

	// Codex lends its MCP servers through the runner's bridge; its own
	// browser (cua_repl) needs a review Codex runs only in the person's own
	// session process, so that leg is expected to be refused. A browser MCP
	// server configured in Codex (Playwright) is lent like any other.
	for _, codexBackend := range []string{backendCodex, backendPlaywright} {
		t.Run("codex/"+codexBackend, func(t *testing.T) {
			if _, err := exec.LookPath("codex"); err != nil {
				t.Skip("not run: codex is not installed")
			}
			bin := buildRunner(t)
			work, _ := filepath.EvalSymlinks(t.TempDir())
			catalogRoot := filepath.Join(work, "catalog")
			runArgs := stageBrowserAnyClient(t, catalogRoot, base, codexBackend)

			serveArgs, _ := json.Marshal([]string{"serve", "--interpreters", store, "--runs", filepath.Join(work, "runs"),
				"--config-dir", filepath.Join(work, "config"), "--catalog-root", catalogRoot})
			// The runner is registered for this one process; no configuration
			// file is changed.
			cmd := exec.Command("codex", "-c", fmt.Sprintf("mcp_servers.tap.command=%q", bin),
				"-c", "mcp_servers.tap.args="+string(serveArgs), "app-server")
			cmd.Dir = work
			in, _ := cmd.StdinPipe()
			stdout, _ := cmd.StdoutPipe()
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { in.Close(); cmd.Process.Kill(); cmd.Wait() }()
			lines := make(chan map[string]any, 256)
			go func() {
				sc := bufio.NewScanner(stdout)
				sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
				for sc.Scan() {
					var m map[string]any
					if json.Unmarshal(sc.Bytes(), &m) == nil {
						lines <- m
					}
				}
				close(lines)
			}()
			send := func(v any) { b, _ := json.Marshal(v); in.Write(append(b, '\n')) }

			var toolResults []string
			var prompts []string
			turnDone := false
			// handle answers what Codex asks of its client and keeps what the
			// turn reports about TAP's tools.
			handle := func(m map[string]any) {
				method, _ := m["method"].(string)
				p, _ := m["params"].(map[string]any)
				switch {
				case method == "mcpServer/elicitation/request":
					msg, _ := p["message"].(string)
					prompts = append(prompts, fmt.Sprintf("from %v: %s", p["serverName"], strings.ReplaceAll(msg, "\n", " | ")))
					send(map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": map[string]any{"action": "accept", "content": acceptForm(p["requestedSchema"])}})
				case strings.HasSuffix(method, "requestApproval") || method == "execCommandApproval" || method == "applyPatchApproval":
					// The turn needs no commands or file changes; refuse them.
					prompts = append(prompts, "declined "+method)
					send(map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": map[string]any{"decision": "decline"}})
				case m["id"] != nil && method != "":
					prompts = append(prompts, "unanswered "+method)
					send(map[string]any{"jsonrpc": "2.0", "id": m["id"], "error": map[string]any{"code": -32601, "message": "not supported by this test"}})
				case method == "item/completed":
					item, _ := p["item"].(map[string]any)
					if item["type"] == "mcpToolCall" && item["server"] == "tap" {
						b, _ := json.Marshal(item["result"])
						e, _ := json.Marshal(item["error"])
						toolResults = append(toolResults, fmt.Sprintf("%v: %s %s", item["tool"], b, e))
					}
				case method == "turn/completed":
					turnDone = true
				}
			}
			n := 0
			call := func(method string, params any, within time.Duration) (map[string]any, error) {
				n++
				id := float64(n + 1000)
				send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
				deadline := time.After(within)
				for {
					select {
					case m, ok := <-lines:
						if !ok {
							return nil, fmt.Errorf("%s: codex exited", method)
						}
						if m["id"] == id && m["method"] == nil {
							if e, ok := m["error"]; ok && e != nil {
								return nil, fmt.Errorf("%s: %v", method, e)
							}
							r, _ := m["result"].(map[string]any)
							return r, nil
						}
						handle(m)
					case <-deadline:
						return nil, fmt.Errorf("%s: no answer within %s", method, within)
					}
				}
			}

			if _, err := call("initialize", map[string]any{"clientInfo": map[string]string{"name": "tap-test", "version": "0"}}, time.Minute); err != nil {
				t.Fatal(err)
			}
			// Approvals go to the person (this test answers them). Under
			// auto_review, Codex's reviewer refuses the browser call the runner
			// makes for the turn and asks for the person's explicit approval.
			r, err := call("thread/start", map[string]any{"ephemeral": true, "cwd": work, "approvalsReviewer": "user"}, 2*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			th, _ := r["thread"].(map[string]any)
			thread, _ := th["id"].(string)
			prompt := "Call the tool tap_run on the MCP server named tap with exactly these arguments, and nothing else:\n" + string(runArgs) +
				"\nIf it hands back a run handle instead of a result, call tap_result with that handle until the run finishes. " +
				"Then reply with the program's output exactly as returned. Use no other tool."
			if _, err := call("turn/start", map[string]any{"threadId": thread, "input": []map[string]any{{"type": "text", "text": prompt}}}, time.Minute); err != nil {
				t.Fatal(err)
			}
			deadline := time.After(6 * time.Minute)
			for !turnDone {
				select {
				case m, ok := <-lines:
					if !ok {
						t.Fatal("codex exited before the turn completed")
					}
					handle(m)
				case <-deadline:
					t.Fatal("the Codex turn did not complete within 6 minutes")
				}
			}
			for _, p := range prompts {
				t.Logf("prompt: %.300s", p)
			}
			all := strings.Join(toolResults, "\n")
			t.Logf("TAP tool results through Codex:\n%.4000s", all)
			if len(toolResults) == 0 {
				t.Fatal("the Codex turn did not call tap_run")
			}
			if notLent(all) {
				t.Skipf("not run: Codex does not lend its browser to the runner here")
			}
			if codexBackend == backendCodex && strings.Contains(all, "Automated review of this operation failed") {
				// Codex reviews every call to its own browser, and that review
				// runs only in the person's session process, which a runner the
				// session starts cannot reach. A known limit of Codex.
				t.Skipf("not run: Codex's own browser cannot be lent to the runner (its automated review runs only in the session's process)")
			}
			if err := checkCounterRun(all, codexBackend); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// openVSCodeWithPlaywright opens VS Code with a throwaway profile, the TAP
// extension from this checkout and one MCP server, Playwright, which a small
// extension starts (starting it stands in for the person trusting it). It
// returns the bridge to that window once Playwright's tools are listed.
func openVSCodeWithPlaywright(t *testing.T, code string) *bridge.VSCode {
	t.Helper()
	// A short path: VS Code refuses a user-data directory whose socket path is long.
	root, err := os.MkdirTemp("/tmp", "vsc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	user := filepath.Join(root, "user", "User")
	os.MkdirAll(user, 0o755)
	os.WriteFile(filepath.Join(user, "mcp.json"), []byte(`{"servers":{"playwright":{"type":"stdio","command":"npx","args":["@playwright/mcp@latest"]}}}`), 0o644)
	// A fresh profile opens VS Code's onboarding dialog (sign in to Copilot,
	// VS Code 1.141), which blocks every tool call until someone dismisses
	// it; this profile turns it off.
	//
	// The extension's calls carry no chat invocation token, so VS Code
	// confirms a tool that is not read-only (browser_navigate) in a modal
	// dialog, which nobody here can answer and cancellation does not close.
	// This throwaway profile approves VS Code's own tool confirmations, and
	// the starter sets the context key VS Code's own tests use to skip the
	// one-time opt-in dialog that the setting raises. It leaves the runner's
	// question in place: Approve below stands in for it, as the person's
	// answer to the runner's form does in real use, and VS Code's auto-approval
	// never answers that form.
	os.WriteFile(filepath.Join(user, "settings.json"), []byte(`{
  "workbench.welcomePage.experimentalOnboarding": false,
  "workbench.welcome.enabled": false,
  "workbench.startupEditor": "none",
  "chat.welcomePage.signIn.enabled": false,
  "chat.tools.global.autoApprove": true
}`), 0o644)
	starter := filepath.Join(root, "starter")
	os.MkdirAll(starter, 0o755)
	os.WriteFile(filepath.Join(starter, "package.json"), []byte(`{"name":"starter","publisher":"t","version":"0.0.1","engines":{"vscode":"^1.100.0"},"main":"./e.js","activationEvents":["*"]}`), 0o644)
	os.WriteFile(filepath.Join(starter, "e.js"), []byte(`const vscode=require("vscode");
exports.activate=async()=>{await vscode.commands.executeCommand("setContext","vscode.chat.tools.global.autoApprove.testMode",true);
 await new Promise(r=>setTimeout(r,6000));
 for (const id of ["mcp.config.usrlocal.playwright","playwright"]) { try { await vscode.commands.executeCommand("workbench.mcp.startServer", id); } catch {} } };`), 0o644)
	cacheDir := filepath.Join(os.Getenv("HOME"), "Library", "Caches", "tap-runtime", "vscode")
	before, _ := filepath.Glob(filepath.Join(cacheDir, "*.sock"))
	cmd := exec.Command(code, "--user-data-dir", filepath.Join(root, "user"), "--extensions-dir", filepath.Join(root, "exts"),
		"--extensionDevelopmentPath", filepath.Join(repoRoot, "vscode"), "--extensionDevelopmentPath", starter, "--new-window")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); exec.Command("pkill", "-f", filepath.Join(root, "user")).Run() })
	var v *bridge.VSCode
	for i := 0; i < 40 && v == nil; i++ {
		time.Sleep(time.Second)
		socks, _ := filepath.Glob(filepath.Join(cacheDir, "*.sock"))
		for _, s := range socks {
			old := false
			for _, b := range before {
				old = old || b == s
			}
			if !old {
				if c, err := bridge.NewVSCode(s); err == nil {
					v = c
					break
				}
			}
		}
	}
	if v == nil {
		t.Fatal("the TAP extension's socket did not appear")
	}
	t.Cleanup(v.Close)
	var names []string
	for i := 0; i < 60; i++ {
		inv, err := v.Inventory()
		if err != nil {
			t.Fatal(err)
		}
		names = names[:0]
		for _, tool := range inv {
			if strings.Contains(tool.Name, "browser_navigate") || strings.Contains(tool.Name, "browser_evaluate") {
				names = append(names, tool.Server+"/"+tool.Name)
			}
		}
		if len(names) >= 2 {
			t.Logf("VS Code lends: %v", names)
			return v
		}
		time.Sleep(time.Second)
	}
	t.Fatal("VS Code never listed Playwright's browser_navigate and browser_evaluate tools")
	return nil
}

// buildRunner builds the runner from this checkout for a client to start.
func buildRunner(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "tap")
	build := exec.Command("go", "build", "-o", bin, "./host")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the runner: %v\n%s", err, out)
	}
	return bin
}

// stageBrowserAnyClient installs the primitive in catalogRoot and returns
// the tap_run arguments that run it on backend.
func stageBrowserAnyClient(t *testing.T, catalogRoot, base, backend string) []byte {
	t.Helper()
	sum := sha256.Sum256([]byte(browserAnyClientManifest + browserAnyClientProgram))
	dest := filepath.Join(catalogRoot, hex.EncodeToString(sum[:8]))
	writeBrowserAnyClientPackage(t, dest)
	digest, manifest, err := packageDigest(dest)
	if err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(map[string]string{"base_url": base, "backend": backend})
	runArgs, _ := json.Marshal(map[string]any{
		"ref":    manifest.Metadata.Publisher + "/" + manifest.Metadata.Name + "@" + manifest.Metadata.Version,
		"digest": digest, "args": []string{string(input)},
	})
	return runArgs
}

// runInClient runs the primitive through the runner's bridge to client, from
// the current directory, asking for backend, and checks the result. Every
// approval the runner asks for is given: the page is a local fixture the
// test owns.
func runInClient(t *testing.T, client, store, base, backend string) {
	t.Helper()
	pkg := filepath.Join(t.TempDir(), "pkg")
	writeBrowserAnyClientPackage(t, pkg)
	args, _ := json.Marshal(map[string]string{"base_url": base, "backend": backend})
	res, err := Run(context.Background(), Options{
		Package: pkg, Args: []string{string(args)}, Client: client,
		Journal: io.Discard, InterpDir: store, RunsDir: t.TempDir(),
		Approve: func(Ask) Grant { return Grant{OK: true, Limit: Unlimited} },
	})
	if err != nil {
		if notLent(err.Error()) {
			t.Skipf("not run: %s does not lend %s here: %v", client, backend, err)
		}
		if strings.Contains(err.Error(), "cannot run a tool for another program") || strings.Contains(err.Error(), "TAP relay plugin") {
			// OpenCode lends its tools only from inside a running session
			// (TestLiveSessionRelay covers it).
			t.Skipf("not run: %v", err)
		}
		t.Fatal(err)
	}
	t.Logf("run %s, %d calls in %s\n%s\n%s", res.RunID, res.Calls, res.Elapsed.Round(time.Millisecond), res.Stdout, res.Stderr)
	if notLent(res.Stderr) {
		t.Skipf("not run: %s does not lend %s here", client, backend)
	}
	if strings.Contains(res.Stdout+res.Stderr, "cannot run a tool for another program") {
		t.Skipf("not run: this %s cannot run a tool for another program", client)
	}
	if backend == backendChrome && strings.Contains(res.Stderr, "Permission denied") {
		t.Skipf("not run: Claude in Chrome was not allowed on localhost; allow it in the extension's site permissions")
	}
	if err := checkCounterRun(res.Stdout, backend); err != nil {
		t.Fatalf("%v\nstdout:\n%s\nstderr:\n%s", err, res.Stdout, res.Stderr)
	}
}

// acceptForm fills an elicitation form with a yes: every boolean ticked,
// every integer at its maximum, or 100 when it has none. The fixture is a
// local page the test owns.
func acceptForm(schema any) map[string]any {
	out := map[string]any{}
	s, _ := schema.(map[string]any)
	props, _ := s["properties"].(map[string]any)
	for k, v := range props {
		p, _ := v.(map[string]any)
		switch p["type"] {
		case "boolean":
			out[k] = true
		case "integer", "number":
			if max, ok := p["maximum"].(float64); ok {
				out[k] = max
			} else {
				out[k] = 100
			}
		case "string":
			if enum, ok := p["enum"].([]any); ok && len(enum) > 0 {
				out[k] = enum[0]
			}
		}
	}
	return out
}
