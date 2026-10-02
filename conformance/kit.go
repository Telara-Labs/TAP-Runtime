// Package conformance holds what a TAP runner is held to.
//
// corpus/ is data: binding, satisfaction and manifest cases with their
// required outcomes, for a runner written by anybody in any language.
//
// The kit in this file tests a runner from outside. It starts the runner as
// an MCP server, plays the client, hands it real packages and looks at what
// it did to a real directory. It knows nothing of how the runner is built.
//
//	go run ./conformance/cmd/tap-conformance -- tap serve
//
// A runner that passes every lane has been shown to refuse what these
// packages attempt. It has not been shown to be correct.
package conformance

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Lane is one thing a runner must do, and whether it did.
type Lane struct {
	Name   string
	Passed bool
	Detail string
}

type session struct {
	cmd   *exec.Cmd
	in    io.WriteCloser
	sc    *bufio.Scanner
	next  int
	asked []string
	// answers are given to elicitations in order; the last repeats.
	answers []map[string]any
}

var (
	yes = map[string]any{"action": "accept", "content": map[string]any{"approve": true, "limit": 100}}
	one = map[string]any{"action": "accept", "content": map[string]any{"approve": true, "limit": 1}}
	no  = map[string]any{"action": "decline"}
)

func start(runner []string, dir string, elicitation bool, answers ...map[string]any) (*session, error) {
	cmd := exec.Command(runner[0], runner[1:]...)
	cmd.Dir = dir
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s := &session{cmd: cmd, in: in, sc: bufio.NewScanner(out), answers: answers}
	s.sc.Buffer(make([]byte, 0, 64<<10), 32<<20)
	caps := map[string]any{}
	if elicitation {
		caps["elicitation"] = map[string]any{"form": map[string]any{}}
	}
	if _, err := s.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": caps,
		"clientInfo": map[string]any{"name": "tap-conformance", "version": "1"}}); err != nil {
		s.stop()
		return nil, err
	}
	return s, nil
}

func (s *session) stop() {
	s.in.Close()
	done := make(chan struct{})
	go func() { s.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		s.cmd.Process.Kill()
	}
}

func (s *session) send(v any) {
	b, _ := json.Marshal(v)
	s.in.Write(append(b, '\n'))
}

func (s *session) call(method string, params any) (map[string]any, error) {
	s.next++
	id := s.next
	s.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	type line struct {
		m  map[string]any
		ok bool
	}
	ch := make(chan line, 1)
	read := func() {
		for s.sc.Scan() {
			var m map[string]any
			if json.Unmarshal(s.sc.Bytes(), &m) == nil {
				ch <- line{m, true}
				return
			}
		}
		ch <- line{nil, false}
	}
	deadline := time.After(90 * time.Second)
	for {
		go read()
		select {
		case l := <-ch:
			if !l.ok {
				return nil, fmt.Errorf("%s: the runner stopped answering", method)
			}
			if l.m["method"] == "elicitation/create" {
				p, _ := l.m["params"].(map[string]any)
				msg, _ := p["message"].(string)
				// The first time a runner is asked to run a package it may ask
				// whether to (TENG-3103). That is not a question about a change,
				// so it is answered yes and not counted among them.
				if strings.Contains(msg, "for the first time on this machine") {
					s.send(map[string]any{"jsonrpc": "2.0", "id": l.m["id"], "result": yes})
					continue
				}
				s.asked = append(s.asked, msg)
				ans := no
				if n := len(s.answers); n > 0 {
					ans = s.answers[min(len(s.asked)-1, n-1)]
				}
				s.send(map[string]any{"jsonrpc": "2.0", "id": l.m["id"], "result": ans})
				continue
			}
			if l.m["id"] == float64(id) {
				if e, ok := l.m["error"]; ok && e != nil {
					return nil, fmt.Errorf("%s: %v", method, e)
				}
				r, _ := l.m["result"].(map[string]any)
				return r, nil
			}
		case <-deadline:
			return nil, fmt.Errorf("%s: no answer in 90 seconds", method)
		}
	}
}

// run hands the runner a package and returns what the program printed.
func (s *session) run(pkg string) (string, error) {
	r, err := s.call("tools/call", map[string]any{"name": "tap_run", "arguments": map[string]any{"package": pkg}})
	if err != nil {
		return "", err
	}
	content, _ := r["content"].([]any)
	if len(content) == 0 {
		return "", nil
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text, nil
}

const head = `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.conformance, name: %s, version: 0.1.0}
execution: {entrypoint: %s}
`

func pkg(root, name, entry, blocks, script string) string {
	dir := filepath.Join(root, "pkg-"+name)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "primitive.yaml"), []byte(fmt.Sprintf(head, name, entry)+blocks), 0o644)
	os.WriteFile(filepath.Join(dir, entry), []byte(script), 0o644)
	return dir
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func content(p string) string { b, _ := os.ReadFile(p); return strings.TrimSpace(string(b)) }

// Run puts a runner through every lane. runner is the command that starts
// it as an MCP server on its standard input and output.
func Run(runner []string, log io.Writer) []Lane {
	var lanes []Lane
	root, err := os.MkdirTemp("", "tap-conformance-")
	if err != nil {
		return []Lane{{"setup", false, err.Error()}}
	}
	defer os.RemoveAll(root)
	root, _ = filepath.EvalSymlinks(root)

	// A canary outside everything any package declares.
	outside := filepath.Join(root, "outside")
	os.MkdirAll(outside, 0o755)
	secret := filepath.Join(outside, "secret.txt")
	os.WriteFile(secret, []byte("CANARY-7f3a"), 0o600)

	lane := func(name string, elicitation bool, answers []map[string]any, build func(work string) string, check func(work, out string, s *session) string) {
		work := filepath.Join(root, "work-"+strings.ReplaceAll(name, " ", "-"))
		os.MkdirAll(work, 0o755)
		p := build(work)
		s, err := start(runner, work, elicitation, answers...)
		if err != nil {
			lanes = append(lanes, Lane{name, false, "the runner did not start: " + err.Error()})
			return
		}
		out, err := s.run(p)
		s.stop()
		detail := ""
		if err != nil {
			detail = err.Error()
		} else {
			detail = check(work, out, s)
		}
		lanes = append(lanes, Lane{name, detail == "", detail})
		mark := "pass"
		if detail != "" {
			mark = "FAIL"
		}
		fmt.Fprintf(log, "%-4s  %s", mark, name)
		if detail != "" {
			fmt.Fprintf(log, "\n      %s", strings.ReplaceAll(detail, "\n", "\n      "))
		}
		fmt.Fprintln(log)
	}

	// ---- containment: a program reaches nothing it did not declare
	lane("a program cannot read a file it did not declare", true, nil,
		func(w string) string {
			return pkg(root, "read-outside", "main.sh", "", "cat "+secret+" || echo blocked\ncat /etc/hosts || echo blocked\n")
		},
		func(w, out string, s *session) string {
			if strings.Contains(out, "CANARY-7f3a") || strings.Contains(out, "localhost") {
				return "the program read a file outside what it declared:\n" + out
			}
			return ""
		})
	lane("a program cannot write a file it did not declare", true, []map[string]any{yes},
		func(w string) string {
			return pkg(root, "write-outside", "main.sh", "files:\n  - {path: out, access: write}\n",
				"echo x > "+filepath.Join(outside, "planted.txt")+" || echo blocked\necho x > ../planted.txt || echo blocked\n")
		},
		func(w, out string, s *session) string {
			if exists(filepath.Join(outside, "planted.txt")) || exists(filepath.Join(filepath.Dir(w), "planted.txt")) {
				return "a file was written outside the declared directory"
			}
			return ""
		})
	lane("a declared directory cannot be left through a symbolic link", true, []map[string]any{yes},
		func(w string) string {
			os.MkdirAll(filepath.Join(w, "out"), 0o755)
			os.Symlink(outside, filepath.Join(w, "out", "link"))
			return pkg(root, "symlink", "main.sh", "files:\n  - {path: out, access: write}\n",
				"echo x > out/link/through-link.txt || echo blocked\ncat out/link/secret.txt || echo blocked\n")
		},
		func(w, out string, s *session) string {
			if exists(filepath.Join(outside, "through-link.txt")) {
				return "a write followed a symbolic link out of the declared directory"
			}
			if strings.Contains(out, "CANARY-7f3a") {
				return "a read followed a symbolic link out of the declared directory"
			}
			return ""
		})
	lane("a program cannot run a command it did not declare", true, []map[string]any{yes},
		func(w string) string {
			return pkg(root, "undeclared-command", "main.sh", "commands:\n  - {command: true, args: [\"*\"], effect: read}\n",
				"touch "+filepath.Join(w, "touched.txt")+" || echo blocked\nsh -c 'echo x > "+filepath.Join(w, "shelled.txt")+"' || echo blocked\n")
		},
		func(w, out string, s *session) string {
			if exists(filepath.Join(w, "touched.txt")) || exists(filepath.Join(w, "shelled.txt")) {
				return "an undeclared command ran"
			}
			return ""
		})
	lane("a program does not see the runner's environment", true, nil,
		func(w string) string {
			return pkg(root, "environment", "main.sh", "", "echo \"home=[$HOME] path=[$PATH] user=[$USER]\"\n")
		},
		func(w, out string, s *session) string {
			if !strings.Contains(out, "home=[] path=[] user=[]") {
				return "the program saw the runner's environment:\n" + out
			}
			return ""
		})

	// ---- bounds on what is declared
	lane("a flag before a subcommand must be declared, with its value", true, nil,
		func(w string) string {
			os.WriteFile(filepath.Join(w, "a.txt"), []byte("alpha\n"), 0o644)
			return pkg(root, "globals", "main.sh", "commands:\n  - {command: grep, globals: [\"-c\"], args: [alpha, \"*\"], effect: read}\n",
				"grep -c alpha a.txt && echo declared-ran\ngrep -r alpha . && echo undeclared-flag-ran\ngrep beta a.txt; echo status=$?\n")
		},
		func(w, out string, s *session) string {
			if !strings.Contains(out, "declared-ran") {
				return "a declared command with a declared flag did not run:\n" + out
			}
			if strings.Contains(out, "undeclared-flag-ran") {
				return "a flag that was not declared was accepted"
			}
			if !strings.Contains(out, "status=126") {
				return "an undeclared subcommand was not refused:\n" + out
			}
			return ""
		})
	web := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			os.WriteFile(filepath.Join(outside, "posted.txt"), []byte(r.Method), 0o644)
		}
		rw.Write([]byte("declared-origin"))
	}))
	defer web.Close()
	other := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { rw.Write([]byte("OTHER-ORIGIN")) }))
	defer other.Close()
	lane("a program can fetch only the origins it declared", true, []map[string]any{yes},
		func(w string) string {
			return pkg(root, "fetch", "main.sh", "fetch:\n  - {origin: \""+web.URL+"\"}\n",
				"tap fetch "+web.URL+"/x && echo\ntap fetch "+other.URL+"/x || echo blocked\ntap fetch "+web.URL+"/x POST || echo post-blocked\n")
		},
		func(w, out string, s *session) string {
			if !strings.Contains(out, "declared-origin") {
				return "a declared origin could not be fetched:\n" + out
			}
			if strings.Contains(out, "OTHER-ORIGIN") {
				return "an undeclared origin was fetched"
			}
			if exists(filepath.Join(outside, "posted.txt")) {
				return "a method that was not declared was sent: " + content(filepath.Join(outside, "posted.txt"))
			}
			return ""
		})

	// A read can carry data out in its address, so a fetch is asked of the
	// person, even a GET. With no yes, nothing leaves (TENG-3099).
	lane("a fetch, even a read, goes nowhere unless a person says yes", true, nil,
		func(w string) string {
			return pkg(root, "fetch-unasked", "main.sh", "fetch:\n  - {origin: \""+web.URL+"\"}\n",
				"tap fetch "+web.URL+"/x && echo fetched || echo refused\n")
		},
		func(w, out string, s *session) string {
			if strings.Contains(out, "declared-origin") || strings.Contains(out, "fetched\n") && !strings.Contains(out, "refused") {
				return "a fetch ran with nobody's yes:\n" + out
			}
			return ""
		})

	// ---- approval
	const writer = "files:\n  - {path: out, access: write}\n"
	const three = "echo 1 > out/a.txt && echo a-written || echo a-refused\necho 2 > out/b.txt && echo b-written || echo b-refused\necho 3 > out/c.txt && echo c-written || echo c-refused\n"
	lane("a change is made when a person says yes", true, []map[string]any{yes},
		func(w string) string {
			// The directory exists already, so a runner with no sandbox
			// and no gate would write into it.
			os.MkdirAll(filepath.Join(w, "out"), 0o755)
			return pkg(root, "yes", "main.sh", writer, three)
		},
		func(w, out string, s *session) string {
			if content(filepath.Join(w, "out", "a.txt")) != "1" || content(filepath.Join(w, "out", "c.txt")) != "3" {
				return "approved changes were not made:\n" + out
			}
			if len(s.asked) == 0 {
				return "changes were made and nobody was asked"
			}
			return ""
		})
	lane("no change is made when a person says no", true, []map[string]any{no},
		func(w string) string {
			// The directory exists already, so a runner with no sandbox
			// and no gate would write into it.
			os.MkdirAll(filepath.Join(w, "out"), 0o755)
			return pkg(root, "no", "main.sh", writer, three)
		},
		func(w, out string, s *session) string {
			if e, _ := os.ReadDir(filepath.Join(w, "out")); len(e) > 0 {
				return fmt.Sprintf("%d change(s) were made after a person said no", len(e))
			}
			if len(s.asked) == 0 {
				return "nobody was asked"
			}
			if !strings.Contains(out, "a-refused") {
				return "the program was not told it was refused:\n" + out
			}
			return ""
		})
	lane("a number allowed is a number not exceeded", true, []map[string]any{one, no},
		func(w string) string {
			// The directory exists already, so a runner with no sandbox
			// and no gate would write into it.
			os.MkdirAll(filepath.Join(w, "out"), 0o755)
			return pkg(root, "ceiling", "main.sh", writer, three)
		},
		func(w, out string, s *session) string {
			e, _ := os.ReadDir(filepath.Join(w, "out"))
			if len(e) != 1 {
				return fmt.Sprintf("one change was allowed and %d were made", len(e))
			}
			if len(s.asked) < 2 {
				return "the person was not asked again when the number was reached"
			}
			return ""
		})
	lane("a client that cannot show a prompt gets no changes", false, []map[string]any{yes},
		func(w string) string {
			// The directory exists already, so a runner with no sandbox
			// and no gate would write into it.
			os.MkdirAll(filepath.Join(w, "out"), 0o755)
			return pkg(root, "no-prompt", "main.sh", writer, three)
		},
		func(w, out string, s *session) string {
			if e, _ := os.ReadDir(filepath.Join(w, "out")); len(e) > 0 {
				return fmt.Sprintf("%d change(s) were made with nobody able to approve them", len(e))
			}
			if len(s.asked) > 0 {
				return "a client that did not advertise elicitation was sent one"
			}
			return ""
		})
	lane("a read needs nobody", false, nil,
		func(w string) string {
			os.MkdirAll(filepath.Join(w, "in"), 0o755)
			os.WriteFile(filepath.Join(w, "in", "x.txt"), []byte("readable\n"), 0o644)
			return pkg(root, "read", "main.sh", "files:\n  - {path: in, access: read}\n", "cat in/x.txt\n")
		},
		func(w, out string, s *session) string {
			if !strings.Contains(out, "readable") {
				return "a declared read did not happen:\n" + out
			}
			return ""
		})

	// ---- the manifest
	lane("a manifest with a field the format does not have is refused", true, []map[string]any{yes},
		func(w string) string {
			return pkg(root, "misspelt", "main.sh", "file:\n  - {path: out, access: write}\n", "echo ran > "+filepath.Join(w, "ran.txt")+"; echo ran\n")
		},
		func(w, out string, s *session) string {
			if strings.HasPrefix(strings.TrimSpace(out), "ran") || exists(filepath.Join(w, "ran.txt")) {
				return "a manifest with a misspelt block was run"
			}
			return ""
		})
	lane("an entrypoint outside the package is refused", true, nil,
		func(w string) string {
			os.WriteFile(filepath.Join(root, "stray.sh"), []byte("echo stray-ran\n"), 0o644)
			p := pkg(root, "stray", "main.sh", "", "echo inside\n")
			os.WriteFile(filepath.Join(p, "primitive.yaml"), []byte(fmt.Sprintf(head, "stray", "../stray.sh")), 0o644)
			return p
		},
		func(w, out string, s *session) string {
			if strings.Contains(out, "stray-ran") {
				return "a program outside the package was run"
			}
			return ""
		})
	lane("a subprocess runtime is not run as if it were contained", true, []map[string]any{yes},
		func(w string) string {
			p := pkg(root, "subprocess", "main.sh", "", "echo x > "+filepath.Join(w, "uncontained.txt")+"\n")
			os.WriteFile(filepath.Join(p, "primitive.yaml"), []byte(strings.Replace(fmt.Sprintf(head, "subprocess", "main.sh"),
				"execution: {entrypoint: main.sh}", "execution: {entrypoint: main.sh, runtime: tap-subprocess-v1}", 1)), 0o644)
			return p
		},
		func(w, out string, s *session) string {
			if exists(filepath.Join(w, "uncontained.txt")) {
				return "an uncontained primitive wrote a file it never declared"
			}
			return ""
		})
	return lanes
}
