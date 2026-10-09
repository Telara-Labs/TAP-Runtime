//go:build darwin

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

var (
	liveCopilotCLI    = flag.Bool("live-copilot-cli", false, "drive the real GitHub Copilot CLI in a terminal of the test's own (spends Copilot requests; needs its sign-in)")
	copilotGatewayHdr = flag.String("copilot-gateway-headers", "", "with -live-copilot-cli: a file of header lines for the Telara gateway; adds a Jira comment leg")
	copilotJiraIssue  = flag.String("copilot-jira-issue", "", "with -copilot-gateway-headers: the issue the Jira leg comments on")
	copilotLegs       = flag.String("copilot-legs", "save,write,jira,browser", "with -live-copilot-cli: which legs to run")
)

// GitHub Copilot CLI shows the runner's questions in its own terminal UI. It
// advertises MCP elicitation (form and url); in interactive mode it renders a
// form ("<server> needs information", enter accept, ctrl+d decline), and in
// -p mode it declines every one, so nothing that needs a yes happens there.
// Before that, Copilot asks the person itself before each call of a tool that
// is not read-only ("Do you want to use this tool?").
//
// The test starts Copilot in a terminal of its own and answers both, as a
// person at the keyboard would: it saves a primitive with tap_save, then in
// new sessions finds and runs it, approving its write once and declining it
// once. With -copilot-gateway-headers it does the same for a Jira comment
// sent through the Telara gateway, and with npx installed it runs a browser
// primitive against Playwright. The runner reaches those through --mcp-url:
// Copilot CLI does not lend its own connections to an MCP server it starts.
//
// Run with: go test ./host -run LiveCopilotCLI -live-copilot-cli
func TestLiveCopilotCLIConfirmsAPrimitivesChangeInItsOwnUI(t *testing.T) {
	if !*liveCopilotCLI {
		t.Skip("pass -live-copilot-cli to drive the real Copilot CLI")
	}
	copilot, err := exec.LookPath("copilot")
	if err != nil {
		t.Skip("copilot is not installed")
	}
	tap := buildTap(t)
	cache, _ := os.UserCacheDir()
	interp := filepath.Join(cache, "tap-runtime", "interpreters")
	root := t.TempDir()
	home := filepath.Join(root, "home")
	collection := filepath.Join(home, "Library", "Application Support", "tap", "primitives")
	os.MkdirAll(home, 0o700)
	evidence := func(name string, b []byte) {
		if *liveEvidenceDir != "" {
			os.MkdirAll(*liveEvidenceDir, 0o700)
			os.WriteFile(filepath.Join(*liveEvidenceDir, name), b, 0o600)
		}
	}
	// The runner keeps its collection and trust under a home of the test's own.
	config := func(extra ...string) string {
		args := append([]string{"serve", "--interpreters", interp}, extra...)
		b, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"taptest": map[string]any{
			"type": "local", "command": tap, "args": args, "env": map[string]string{"HOME": home}, "tools": []string{"*"}}}})
		return string(b)
	}
	session := func(name, cfg, prompt string, policy map[string]string, done func() bool) *ptySession {
		t.Helper()
		work := filepath.Join(root, "work-"+name)
		os.MkdirAll(filepath.Join(work, "out"), 0o700)
		s := startPTY(t, work, copilot, "--additional-mcp-config", cfg, "--disable-mcp-server", "tap", "-i", prompt)
		defer s.close()
		s.drive(t, policy, done, 6*time.Minute)
		evidence("copilot-"+name+".txt", []byte(s.transcript()))
		return s
	}
	runsDone := func() func() bool {
		runs := filepath.Join(home, "Library", "Caches", "tap-runtime", "runs")
		before := countFiles(runs)
		return func() bool { return countFiles(runs) > before }
	}

	leg := func(name string) bool { return strings.Contains(","+*copilotLegs+",", ","+name+",") }
	note := authoredPackage(t, "local.me", "note-writer", "Write a short note to out/note.txt",
		"execution: {entrypoint: main.py}\nfiles:\n  - {path: out, access: write}\n",
		"main.py", "import sys\ntap.write(\"out/note.txt\", sys.argv[1] + \"\\n\")\nprint(\"wrote out/note.txt:\", sys.argv[1])\n")

	var s *ptySession
	if leg("save") || leg("write") {
		// Save: Copilot asks before tap_save, then the runner asks to save.
		s = session("save", config(), fmt.Sprintf("Call the taptest tool tap_search with query \"write a note file\", then call the taptest tool tap_save with package %q. Report exactly what each returned.", note),
			map[string]string{"save": "approve"}, func() bool { _, err := os.Stat(filepath.Join(collection, "note-writer")); return err == nil })
		if _, err := os.Stat(filepath.Join(collection, "note-writer")); err != nil {
			t.Fatalf("tap_save through Copilot did not save: %v\n%s", err, s.tail(3000))
		}
		if !s.saw["tool"] || !s.saw["save"] {
			t.Errorf("Copilot did not show both its tool prompt and the save form: %v", s.saw)
		}

		reuse := "Write the note %s to out/note.txt. Use the taptest TAP tools: tap_search for a saved primitive that writes a note, tap_load it, then tap_run it with the text as its only argument. Then say what tap_run returned."
		s = session("write-approve", config(), fmt.Sprintf(reuse, "approved-in-copilot"),
			map[string]string{"trust": "approve", "change": "approve"}, runsDone())
		if b, _ := os.ReadFile(filepath.Join(root, "work-write-approve", "out", "note.txt")); strings.TrimSpace(string(b)) != "approved-in-copilot" {
			t.Errorf("approved in Copilot, the note is %q\n%s", b, s.tail(3000))
		}
		s = session("write-decline", config(), fmt.Sprintf(reuse, "declined-in-copilot"),
			map[string]string{"trust": "approve", "change": "decline"}, runsDone())
		if _, err := os.Stat(filepath.Join(root, "work-write-decline", "out", "note.txt")); err == nil || !s.saw["change"] {
			t.Errorf("declined in Copilot, the note was written or never asked: %v\n%s", s.saw, s.tail(3000))
		}
	}

	if leg("jira") && *copilotGatewayHdr != "" && *copilotJiraIssue != "" {
		comment := authoredPackage(t, "local.me", "jira-comment", "Add one plain-text comment to a Jira issue",
			"execution: {entrypoint: main.py, timeoutSeconds: 120}\ntools:\n  - {alias: comment, capability: jira.comments.add, effect: write}\n",
			"main.py", "import sys, json\nr = tap.call(\"comment\", {\"issue_key\": sys.argv[1], \"body\": sys.argv[2]})\nprint(\"comment result:\", json.dumps(r)[:600] if not isinstance(r, str) else r[:600])\n")
		save := exec.Command(tap, "discover", "save", comment)
		save.Env = append(os.Environ(), "HOME="+home)
		if out, err := save.CombinedOutput(); err != nil {
			t.Fatalf("saving the Jira primitive: %v\n%s", err, out)
		}
		cfg := config("--mcp-url", "https://api.telara.dev/v1/mcp", "--mcp-header-file", *copilotGatewayHdr)
		ask := "Add a comment to Jira issue %s with the body: %s. Do not ask me to confirm in the chat: TAP asks me itself before anything is posted. Use the taptest TAP tools: tap_search for a saved primitive that adds a Jira comment, tap_load it, then tap_run it with two arguments, the issue key and the body. Then say what tap_run returned."
		stamp := time.Now().UTC().Format(time.RFC3339)
		for _, leg := range []string{"approve", "decline"} {
			body := fmt.Sprintf("TAP acceptance %s: a comment written through GitHub Copilot CLI only after the person answered %s in Copilot's terminal.", stamp, leg)
			s = session("jira-"+leg, cfg, fmt.Sprintf(ask, *copilotJiraIssue, body), map[string]string{"trust": "approve", "change": leg}, runsDone())
			t.Logf("jira %s: asked=%v\n%s", leg, s.saw, s.tail(1500))
			if !s.saw["change"] {
				t.Errorf("the Jira %s leg was never asked", leg)
			}
		}
	}

	if _, err := exec.LookPath("npx"); err == nil && leg("browser") {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: http.FileServer(http.Dir(filepath.Join(repoRoot, "examples", "browser-support", "site")))}
		go srv.Serve(ln)
		defer srv.Close()
		pwPort := freePort(t)
		pw := exec.Command("npx", "@playwright/mcp@latest", "--headless", "--isolated", "--port", pwPort)
		if err := pw.Start(); err != nil {
			t.Fatal(err)
		}
		defer pw.Process.Kill()
		for i := 0; i < 60; i++ {
			if c, err := net.Dial("tcp", "localhost:"+pwPort); err == nil {
				c.Close()
				break
			}
			time.Sleep(time.Second)
		}
		smokeMain, _ := os.ReadFile(filepath.Join(repoRoot, "examples", "browser-smoke", "main.py"))
		smoke := authoredPackage(t, "examples.telara.dev", "browser-smoke", "Navigate a local fixture and verify its rendered heading", "", "main.py", string(smokeMain))
		y, _ := os.ReadFile(filepath.Join(repoRoot, "examples", "browser-smoke", "primitive.yaml"))
		os.WriteFile(filepath.Join(smoke, "primitive.yaml"), y, 0o600)
		os.WriteFile(filepath.Join(smoke, "CHANGELOG.md"), []byte("## 0.1.1\n\n- Example.\n"), 0o600)
		save := exec.Command(tap, "discover", "save", smoke)
		save.Env = append(os.Environ(), "HOME="+home)
		if out, err := save.CombinedOutput(); err != nil {
			t.Fatalf("saving the browser primitive: %v\n%s", err, out)
		}
		// Playwright answers only requests addressed to localhost.
		cfg := config("--mcp-url", "http://localhost:"+pwPort+"/mcp", "--mcp-server-name", "Playwright")
		prompt := fmt.Sprintf("Check the browser smoke fixture at %s. Use the taptest TAP tools: tap_search for a saved primitive that checks the browser smoke fixture, tap_load it, then tap_run it with one argument, the JSON object {\"base_url\":\"http://%s\"}. Then say what tap_run returned.", ln.Addr(), ln.Addr())
		s = session("browser", cfg, prompt, map[string]string{"trust": "approve", "change": "approve"}, runsDone())
		if !strings.Contains(s.transcript(), "passed") {
			t.Errorf("the browser primitive did not pass through Copilot:\n%s", s.tail(3000))
		}
	}
}

func countFiles(dir string) int {
	n := 0
	filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}

func freePort(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
}

// ptySession is a program running in a terminal of the test's own.
type ptySession struct {
	cmd    *exec.Cmd
	master *os.File
	mu     sync.Mutex
	raw    bytes.Buffer
	from   int // where the next prompt is looked for in the plain text
	saw    map[string]bool
}

func startPTY(t *testing.T, dir, prog string, args ...string) *ptySession {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	fd := m.Fd()
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, unix.TIOCPTYGRANT, 0); e != 0 {
		t.Fatal(e)
	}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, unix.TIOCPTYUNLK, 0); e != 0 {
		t.Fatal(e)
	}
	var name [128]byte
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); e != 0 {
		t.Fatal(e)
	}
	slave, err := os.OpenFile(string(bytes.TrimRight(name[:], "\x00")), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 45, Col: 140})
	cmd := exec.Command(prog, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	slave.Close()
	s := &ptySession{cmd: cmd, master: m, saw: map[string]bool{}}
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := m.Read(buf)
			s.mu.Lock()
			s.raw.Write(buf[:n])
			s.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return s
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b[\]P][^\x07\x1b]*(\x07|\x1b\\)|\x1b[()][0-9A-Za-z]|\x1b[=>78]`)

func (s *ptySession) transcript() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ansi.ReplaceAllString(s.raw.String(), "")
}

func (s *ptySession) tail(n int) string {
	t := s.transcript()
	if len(t) > n {
		t = t[len(t)-n:]
	}
	return t
}

func (s *ptySession) send(keys ...string) {
	for _, k := range keys {
		time.Sleep(800 * time.Millisecond)
		s.master.WriteString(k)
	}
}

// drive answers each prompt as it appears, until done says the work is
// finished. A prompt's kind is the policy's key: "tool" (Copilot's own
// question before a tool call; always yes), "save", "trust" and "change"
// (the runner's forms). The answer is "approve" or "decline".
func (s *ptySession) drive(t *testing.T, policy map[string]string, done func() bool, limit time.Duration) {
	t.Helper()
	const up, enter, ctrlD = "\x1b[A", "\r", "\x04"
	deadline := time.Now().Add(limit)
	finished := time.Time{}
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
		if done() {
			if finished.IsZero() {
				finished = time.Now()
			}
			if time.Since(finished) > 20*time.Second {
				return
			}
		}
		text := s.transcript()
		if s.from > len(text) {
			s.from = 0
		}
		rest := text[s.from:]
		kind, at := "", -1
		for k, re := range map[string]*regexp.Regexp{
			"folder": regexp.MustCompile(`Do you trust the files in this folder`),
			"tool":   regexp.MustCompile(`Do you want to use this tool`),
			"save":   regexp.MustCompile(`Save the primitive [^\n]*`),
			"trust":  regexp.MustCompile(`for the first time on this machine`),
			"change": regexp.MustCompile(`wants to: `),
		} {
			if loc := re.FindStringIndex(rest); loc != nil && (at < 0 || loc[0] < at) {
				kind, at = k, loc[0]
			}
		}
		if kind == "" {
			continue
		}
		// Wait for the prompt to finish drawing.
		time.Sleep(2 * time.Second)
		s.saw[kind] = true
		ans := policy[kind]
		t.Logf("prompt %s -> %s", kind, map[bool]string{true: "approve", false: ans}[ans == ""])
		switch {
		case kind == "folder" || kind == "tool":
			// The number picks the option at once. An Enter after it would
			// land on whatever is drawn next, such as the runner's form.
			s.send("1")
		case ans == "decline":
			s.send(ctrlD)
		case kind == "save":
			s.send(enter)
		default:
			// A yes/no field that defaults to no: move to yes and see it
			// selected. Enter then goes to the next field, if there is one
			// (the change form's count, which defaults to 1), and Enter on
			// the last field sends the form.
			if !s.sendUntil(up, "❯ Yes", 3) {
				t.Errorf("the %s form never showed yes selected", kind)
				continue
			}
			if kind == "change" {
				s.sendUntil(enter, "How many times", 3)
			}
			s.send(enter)
		}
		s.from = len(s.transcript())
	}
	t.Errorf("the session did not finish within %s:\n%s", limit, s.tail(3000))
}

// sendUntil sends keys until the screen drawn after them shows want.
func (s *ptySession) sendUntil(keys, want string, tries int) bool {
	for i := 0; i < tries; i++ {
		mark := len(s.transcript())
		s.send(keys)
		for j := 0; j < 10; j++ {
			time.Sleep(500 * time.Millisecond)
			if text := s.transcript(); len(text) > mark && strings.Contains(text[mark:], want) {
				return true
			}
		}
	}
	return false
}

func (s *ptySession) close() {
	s.master.WriteString("\x03")
	time.Sleep(500 * time.Millisecond)
	s.master.WriteString("\x03")
	done := make(chan struct{})
	go func() { s.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		s.cmd.Process.Kill()
		<-done
	}
	s.master.Close()
}
