// host is the spike runner for doc 34 section 13. It loads a package whose
// entrypoint is a SOURCE FILE, runs it inside an interpreter that is itself a
// wasm module, and executes each declared host command on the guest's behalf.
//
// The guest gets no filesystem, no environment and no network. NO WithFSConfig.
// NO WithEnv. The one exception is the Python interpreter, which needs its
// standard library: it gets a READ-ONLY mount of that directory and nothing
// else (see guestConfig).
//
// Interpreters are downloaded on first use against a pinned digest and are
// never compiled into this binary (interpreters.go).
//
//	host [--approve] [--journal FILE] <package-dir> [script args...]
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"gopkg.in/yaml.v3"

	"gitlab.com/telara-labs/tap-runtime/bridge"
)

type manifest struct {
	APIVersion string `yaml:"apiVersion"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Entrypoint string     `yaml:"entrypoint"`
	Tools      []toolDecl `yaml:"tools"`
	Commands   []command  `yaml:"commands"`
}

type command struct {
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
	Effect  string   `yaml:"effect"`
}

type request struct {
	Method    string         `json:"method"`
	Alias     string         `json:"alias"`
	Arguments map[string]any `json:"arguments"`
	Command   string         `json:"command"`
	Args      []string       `json:"args"`
	Stdout    string         `json:"stdout"`
	Stderr    string         `json:"stderr"`
	Exit      int            `json:"exit"`
}

type reply struct {
	Refused string   `json:"refused,omitempty"`
	Result  string   `json:"result,omitempty"`
	Tools   []string `json:"tools,omitempty"`
	Stdout  string   `json:"stdout"`
	Stderr  string   `json:"stderr"`
	Exit    int      `json:"exit"`
}

// arbitraryCode lists invocations that run code the manifest cannot describe.
// Whatever the author declared, these are treated as destructive. This is a
// hand-maintained list, which doc 34 section 13.4 names as a weakness.
var arbitraryCode = [][]string{
	{"bash"}, {"sh"}, {"zsh"}, {"env"}, {"xargs"},
	{"docker", "run"}, {"docker", "exec"},
	{"kubectl", "exec"}, {"kubectl", "run"},
}

func hasPrefix(argv, prefix []string) bool {
	if len(argv) < len(prefix) {
		return false
	}
	for i := range prefix {
		if argv[i] != prefix[i] {
			return false
		}
	}
	return true
}

// resolve finds the declared command an invocation falls under. The longest
// matching args prefix wins, so `kubectl delete` does not fall under a
// declared bare `kubectl`.
func resolve(m *manifest, name string, args []string) (*command, string) {
	var best *command
	for i := range m.Commands {
		c := &m.Commands[i]
		if c.Command != name || !hasPrefix(args, c.Args) {
			continue
		}
		if best == nil || len(c.Args) > len(best.Args) {
			best = c
		}
	}
	if best == nil {
		return nil, ""
	}
	effect := best.Effect
	argv := append([]string{name}, args...)
	for _, p := range arbitraryCode {
		if hasPrefix(argv, p) {
			effect = "destructive"
		}
	}
	return best, effect
}

func main() {
	approve := flag.Bool("approve", false, "approve write and destructive commands for this run")
	journalPath := flag.String("journal", "", "append one JSON line per host command")
	interpDir := flag.String("interpreters", "", "interpreter store; default is the user cache directory")
	cacheDir := flag.String("cache", "", "directory for the compiled-interpreter cache")
	client := flag.String("client", "", "client whose connections to borrow: claude or codex; detected when empty")
	receiptPath := flag.String("receipt", "", "write the admission record as JSON")
	pyLib := flag.String("pylib", "", "python standard library directory, mounted read-only")
	flag.Parse()
	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: host [--approve] [--journal FILE] <package-dir> [args...]")
		os.Exit(2)
	}
	pkg := flag.Arg(0)
	raw, err := os.ReadFile(filepath.Join(pkg, "primitive.yaml"))
	must(err)
	var m manifest
	must(yaml.Unmarshal(raw, &m))
	script, err := os.ReadFile(filepath.Join(pkg, m.Entrypoint))
	must(err)
	store, err := storeDir(*interpDir)
	must(err)
	wasmBytes, in, sum, err := obtain(store, m.Entrypoint)
	must(err)
	kind := in.Kind

	logf("package    %s  entrypoint %s (source, not compiled)", m.Metadata.Name, m.Entrypoint)
	logf("interpreter %s (%d bytes) sha256:%s", in.File, len(wasmBytes), sum[:12])
	for _, c := range m.Commands {
		logf("declared   %-8s %-18s %s", c.Command, strings.Join(c.Args, " "), c.Effect)
	}

	var adm *admission
	var br bridge.Bridge
	if len(m.Tools) > 0 {
		c := *client
		if c == "" {
			c = detectClient()
		}
		br, err = openBridge(c)
		must(err)
		defer br.Close()
		adm, err = admit(m.Tools, br)
		if err != nil {
			br.Close()
			must(err)
		}
		logf("client     %s %s", adm.Client, adm.Version)
		if !adm.Tested {
			logf("WARNING    this runner was not run against %s %s; proceeding (recorded on the receipt)", adm.Client, adm.Version)
		}
		for _, b := range adm.Bindings {
			note := "contract not checked: the client gives no schema"
			if br.HasSchemas() {
				note = "contract not checked: schema satisfaction is not built"
			}
			if b.Pinned {
				note = "pinned"
			}
			logf("bound      %-10s %-26s -> %s / %s  score %.3f  declared %s, annotated %s  (%s)", b.Alias, b.Capability, b.Server, b.Tool, b.Score, b.Declared, b.Annotated, note)
			if b.Gated {
				logf("           %-10s its server says nothing about what it does; treated as a write", b.Alias)
			}
		}
		if *receiptPath != "" {
			j, _ := json.MarshalIndent(adm, "", "  ")
			must(os.WriteFile(*receiptPath, append(j, '\n'), 0o600))
		}
	}

	var journal io.Writer = io.Discard
	if *journalPath != "" {
		f, err := os.OpenFile(*journalPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		must(err)
		defer f.Close()
		journal = f
	}

	ctx := context.Background()
	rc := wazero.NewRuntimeConfig()
	if *cacheDir != "" {
		cache, err := wazero.NewCompilationCacheWithDir(*cacheDir)
		must(err)
		defer cache.Close(ctx)
		rc = rc.WithCompilationCache(cache)
	}
	rt := wazero.NewRuntimeWithConfig(ctx, rc)
	defer rt.Close(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)

	t0 := time.Now()
	compiled, err := rt.CompileModule(ctx, wasmBytes)
	must(err)
	logf("compiled interpreter in %s", time.Since(t0).Round(time.Millisecond))

	// OS pipes, not io.Pipe: io.Pipe is unbuffered, so a guest flushing stdout
	// while the host writes its reply deadlocks both sides.
	toGuestR, toGuestW, err := os.Pipe()
	must(err)
	fromGuestR, fromGuestW, err := os.Pipe()
	must(err)
	var guestErr bytes.Buffer

	cfg := guestConfig(kind, *pyLib, string(script), flag.Args()[1:]).
		WithStdin(toGuestR).WithStdout(fromGuestW).WithStderr(&guestErr)

	done := make(chan error, 1)
	go func() {
		_, err := rt.InstantiateModule(ctx, compiled, cfg)
		fromGuestW.Close()
		done <- err
	}()

	enc := json.NewEncoder(toGuestW)
	if kind == "sh" {
		must(enc.Encode(map[string]any{"script": string(script), "args": flag.Args()[1:]}))
	}

	rd := bufio.NewReaderSize(fromGuestR, 1<<20)
	dispatched, refused := 0, 0
	var final *request
	for {
		line, err := rd.ReadBytes('\n')
		if len(line) > 0 {
			var rq request
			if jerr := json.Unmarshal(line, &rq); jerr != nil {
				logf("guest      non-protocol output: %s", strings.TrimSpace(string(line)))
			} else if rq.Method == "return" {
				final = &rq
				break
			} else if rq.Method == "call" {
				rp := callTool(adm, br, rq, *approve, journal)
				if rp.Refused != "" {
					refused++
				} else {
					dispatched++
				}
				must(enc.Encode(rp))
			} else if rq.Method == "tools" {
				rp := reply{Tools: []string{}}
				if adm != nil {
					rp.Tools = adm.aliases()
				}
				must(enc.Encode(rp))
			} else if rq.Method == "exec" {
				rp := runCommand(&m, rq, *approve, journal)
				if rp.Refused != "" {
					refused++
				} else {
					dispatched++
				}
				must(enc.Encode(rp))
			}
		}
		if err != nil {
			break
		}
	}
	toGuestW.Close()
	go io.Copy(io.Discard, fromGuestR)
	if err := <-done; err != nil {
		logf("guest exit %v", err)
	}
	if guestErr.Len() > 0 {
		logf("guest stderr:\n%s", strings.TrimRight(guestErr.String(), "\n"))
	}
	logf("%d call(s) and command(s) run, %d refused, in %s", dispatched, refused, time.Since(t0).Round(time.Millisecond))
	if final == nil {
		logf("guest ended without a result")
		os.Exit(1)
	}
	if final.Stderr != "" {
		logf("script stderr:\n%s", strings.TrimRight(final.Stderr, "\n"))
	}
	fmt.Printf("RESULT (exit %d)\n%s", final.Exit, final.Stdout)
	os.Exit(final.Exit)
}

// guestConfig is the whole of what a guest is given. Kept as one function so a
// test can assert against the real object.
// The preludes are the "SDK preloaded into the interpreter" of doc 34 section
// 13.3. The author's file never contains protocol code: it calls tap.exec and
// prints. print is captured, because stdout is the wire.
const pyPrelude = `
import sys, json, io
_in, _out = sys.stdin, sys.stdout
class _Tap:
    def call(self, alias, arguments=None):
        _out.write(json.dumps({"method": "call", "alias": alias, "arguments": arguments or {}}) + "\n"); _out.flush()
        r = json.loads(_in.readline())
        if r.get("refused"): raise PermissionError(r["refused"])
        if r.get("exit"): raise RuntimeError(r.get("stderr") or "tool call failed")
        try: return json.loads(r.get("result") or "null")
        except ValueError: return r.get("result")
    def tools(self):
        _out.write(json.dumps({"method": "tools"}) + "\n"); _out.flush()
        return json.loads(_in.readline()).get("tools") or []
    def exec(self, command, args=()):
        _out.write(json.dumps({"method": "exec", "command": command, "args": list(args)}) + "\n"); _out.flush()
        return json.loads(_in.readline())
tap = _Tap()
_buf, _err, _exit = io.StringIO(), io.StringIO(), 0
sys.stdout, sys.stderr = _buf, _err
try:
    exec(compile(%s, "main.py", "exec"), {"tap": tap, "__name__": "__main__"})
except SystemExit as e:
    _exit = int(e.code or 0)
except BaseException as e:
    _err.write(type(e).__name__ + ": " + str(e) + "\n"); _exit = 1
_out.write(json.dumps({"method": "return", "stdout": _buf.getvalue(), "stderr": _err.getvalue(), "exit": _exit}) + "\n"); _out.flush()
`

const jsPrelude = `
import * as std from "qjs:std";
let _buf = "", _err = "", _exit = 0;
const _line = (a) => a.map((x) => (typeof x === "string" ? x : JSON.stringify(x))).join(" ") + "\n";
globalThis.print = (...a) => { _buf += _line(a); };
globalThis.console = { log: (...a) => { _buf += _line(a); }, error: (...a) => { _err += _line(a); } };
const _ask = (o) => { std.out.puts(JSON.stringify(o) + "\n"); std.out.flush(); return JSON.parse(std.in.getline()); };
globalThis.tap = {
  call(alias, args = {}) {
    const r = _ask({ method: "call", alias, arguments: args });
    if (r.refused) throw new Error(r.refused);
    if (r.exit) throw new Error(r.stderr || "tool call failed");
    try { return JSON.parse(r.result ?? "null"); } catch (e) { return r.result; }
  },
  tools() { return _ask({ method: "tools" }).tools ?? []; },
  exec(command, args = []) {
    std.out.puts(JSON.stringify({ method: "exec", command, args }) + "\n"); std.out.flush();
    return JSON.parse(std.in.getline());
  },
};
globalThis.std = std;
try { (0, eval)(%s); } catch (e) { _err += String(e) + "\n"; _exit = 1; }
std.out.puts(JSON.stringify({ method: "return", stdout: _buf, stderr: _err, exit: _exit }) + "\n"); std.out.flush();
`

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

func guestConfig(kind, pyLib, script string, args []string) wazero.ModuleConfig {
	cfg := wazero.NewModuleConfig().WithSysWalltime().WithSysNanotime()
	switch kind {
	case "py":
		fsc := wazero.NewFSConfig()
		if pyLib != "" {
			fsc = fsc.WithReadOnlyDirMount(pyLib, "/usr")
		}
		cfg = cfg.WithFSConfig(fsc).
			WithArgs(append([]string{"python", "-c", fmt.Sprintf(pyPrelude, quote(script))}, args...)...)
	case "js":
		cfg = cfg.WithArgs(append([]string{"qjs", "--module", "-e", fmt.Sprintf(jsPrelude, quote(script))}, args...)...)
	default:
		cfg = cfg.WithArgs("sh")
	}
	return cfg
}

func runCommand(m *manifest, rq request, approve bool, journal io.Writer) reply {
	line := strings.TrimSpace(rq.Command + " " + strings.Join(rq.Args, " "))
	decl, effect := resolve(m, rq.Command, rq.Args)
	entry := map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano), "command": rq.Command, "args": rq.Args}
	record := func(outcome string, extra map[string]any) {
		entry["outcome"] = outcome
		for k, v := range extra {
			entry[k] = v
		}
		b, _ := json.Marshal(entry)
		journal.Write(append(b, '\n'))
	}
	if decl == nil {
		logf("  REFUSED  %s  (not declared)", line)
		record("refused_undeclared", nil)
		return reply{Refused: "command not declared in primitive.yaml"}
	}
	entry["effect"] = effect
	if effect != "read" && !approve {
		logf("  GATED    %s  (%s, no approval)", line, effect)
		record("gated", nil)
		return reply{Refused: effect + " command needs approval"}
	}
	path, err := exec.LookPath(rq.Command)
	if err != nil {
		logf("  ABSENT   %s  (program not on this machine)", line)
		record("refused_absent", nil)
		return reply{Refused: "program not installed on this machine"}
	}
	t0 := time.Now()
	cmd := exec.Command(path, rq.Args...)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	exit := 0
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		} else {
			exit = 127
			se.WriteString(err.Error())
		}
	}
	logf("  run      %s  [%s] exit=%d out=%dB %s", line, effect, exit, so.Len(), time.Since(t0).Round(time.Millisecond))
	record("ran", map[string]any{"exit": exit, "stdout_bytes": so.Len(), "ms": time.Since(t0).Milliseconds()})
	return reply{Stdout: so.String(), Stderr: se.String(), Exit: exit}
}

func logf(f string, a ...any) { fmt.Fprintf(os.Stderr, "host  "+f+"\n", a...) }

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "host  fatal:", err)
		os.Exit(1)
	}
}
