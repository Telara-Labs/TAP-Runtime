// host is the runner. Doc 34 section 13. It loads a package whose
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
	"flag"
	"fmt"
	"io"
	"os"
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
	Entrypoint string      `yaml:"entrypoint"`
	Tools      []toolDecl  `yaml:"tools"`
	Commands   []command   `yaml:"commands"`
	Files      []fileDecl  `yaml:"files"`
	Fetch      []fetchDecl `yaml:"fetch"`
}

type request struct {
	Method     string            `json:"method"`
	Alias      string            `json:"alias"`
	Arguments  map[string]any    `json:"arguments"`
	Command    string            `json:"command"`
	Args       []string          `json:"args"`
	Stdout     string            `json:"stdout"`
	Stderr     string            `json:"stderr"`
	Exit       int               `json:"exit"`
	Stdin      string            `json:"stdin"`
	Path       string            `json:"path"`
	URL        string            `json:"url"`
	HTTPMethod string            `json:"http_method"`
	Headers    map[string]string `json:"headers"`
}

type reply struct {
	Refused string   `json:"refused,omitempty"`
	Result  string   `json:"result,omitempty"`
	Tools   []string `json:"tools,omitempty"`
	Stdout  string   `json:"stdout"`
	Stderr  string   `json:"stderr"`
	Exit    int      `json:"exit"`
	Stdin   string   `json:"stdin"`
	Status  int      `json:"status,omitempty"`
	// Gated is set when the only thing missing is a person's agreement.
	Gated bool `json:"gated,omitempty"`
}

// Approver decides one gated action. It is asked once for each distinct
// action in a run, and told what exactly would happen.
type Approver func(ask Ask) bool

// Ask describes one action that needs a person's agreement.
type Ask struct {
	Primitive string
	Effect    string // write or destructive
	Action    string // one line: exactly what would be done
}

// Options is everything a run is given.
type Options struct {
	Package     string
	Args        []string
	Approve     Approver
	Journal     io.Writer
	InterpDir   string
	CacheDir    string
	PyLib       string
	Client      string        // claude, codex, or empty to detect
	Bridge      bridge.Bridge // set by the server, which already knows the client
	ReceiptPath string
}

// Result is what a run produced.
type Result struct {
	Exit      int
	Stdout    string
	Stderr    string
	Ran       int
	Refused   int
	Admission *admission
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		if err := serve(os.Stdin, os.Stdout, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "host  fatal:", err)
			os.Exit(1)
		}
		return
	}
	approve := flag.Bool("approve", false, "approve write and destructive actions for this run")
	journalPath := flag.String("journal", "", "append one JSON line per action")
	interpDir := flag.String("interpreters", "", "interpreter store; default is the user cache directory")
	cacheDir := flag.String("cache", "", "directory for the compiled-interpreter cache")
	client := flag.String("client", "", "client whose connections to borrow: claude or codex; detected when empty")
	receiptPath := flag.String("receipt", "", "write the admission record as JSON")
	pyLib := flag.String("pylib", "", "python standard library directory, mounted read-only")
	flag.Parse()
	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: host [--approve] [--journal FILE] <package-dir> [args...]\n       host serve")
		os.Exit(2)
	}
	var journal io.Writer = io.Discard
	if *journalPath != "" {
		f, err := os.OpenFile(*journalPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		must(err)
		defer f.Close()
		journal = f
	}
	yes := *approve
	res, err := Run(context.Background(), Options{
		Package: flag.Arg(0), Args: flag.Args()[1:], Journal: journal,
		Approve:   func(Ask) bool { return yes },
		InterpDir: *interpDir, CacheDir: *cacheDir, PyLib: *pyLib, Client: *client, ReceiptPath: *receiptPath,
	})
	must(err)
	if res.Stderr != "" {
		logf("script stderr:\n%s", strings.TrimRight(res.Stderr, "\n"))
	}
	fmt.Printf("RESULT (exit %d)\n%s", res.Exit, res.Stdout)
	os.Exit(res.Exit)
}

// Run admits a package, runs it in the sandbox and serves its requests.
func Run(ctx context.Context, o Options) (*Result, error) {
	raw, err := os.ReadFile(filepath.Join(o.Package, "primitive.yaml"))
	if err != nil {
		return nil, err
	}
	var m manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("primitive.yaml: %w", err)
	}
	if err := validateCapabilities(&m); err != nil {
		return nil, err
	}
	script, err := os.ReadFile(filepath.Join(o.Package, m.Entrypoint))
	if err != nil {
		return nil, err
	}
	store, err := storeDir(o.InterpDir)
	if err != nil {
		return nil, err
	}
	wasmBytes, in, sum, err := obtain(store, m.Entrypoint)
	if err != nil {
		return nil, err
	}
	kind := in.Kind
	journal := o.Journal
	if journal == nil {
		journal = io.Discard
	}

	logf("package    %s  entrypoint %s (source, not compiled)", m.Metadata.Name, m.Entrypoint)
	logf("interpreter %s (%d bytes) sha256:%s", in.File, len(wasmBytes), sum[:12])
	for _, c := range m.Commands {
		logf("declared   %-8s %-18s %s", c.Command, strings.Join(c.Args, " "), c.Effect)
	}

	var adm *admission
	br := o.Bridge
	if len(m.Tools) > 0 {
		if br == nil {
			c := o.Client
			if c == "" {
				c = detectClient()
			}
			br, err = openBridge(c)
			if err != nil {
				return nil, err
			}
			defer br.Close()
		}
		adm, err = admit(m.Tools, br)
		if err != nil {
			return nil, err
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
		if o.ReceiptPath != "" {
			j, _ := json.MarshalIndent(adm, "", "  ")
			if err := os.WriteFile(o.ReceiptPath, append(j, '\n'), 0o600); err != nil {
				return nil, err
			}
		}
	}

	rc := wazero.NewRuntimeConfig()
	if o.CacheDir != "" {
		cache, err := wazero.NewCompilationCacheWithDir(o.CacheDir)
		if err != nil {
			return nil, err
		}
		defer cache.Close(ctx)
		rc = rc.WithCompilationCache(cache)
	}
	rt := wazero.NewRuntimeWithConfig(ctx, rc)
	defer rt.Close(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)

	t0 := time.Now()
	compiled, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		return nil, err
	}
	logf("compiled interpreter in %s", time.Since(t0).Round(time.Millisecond))

	// OS pipes, not io.Pipe: io.Pipe is unbuffered, so a guest flushing stdout
	// while the host writes its reply deadlocks both sides.
	toGuestR, toGuestW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	fromGuestR, fromGuestW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	var guestErr bytes.Buffer
	cfg := guestConfig(kind, o.PyLib, string(script), o.Args).
		WithStdin(toGuestR).WithStdout(fromGuestW).WithStderr(&guestErr)

	done := make(chan error, 1)
	go func() {
		_, err := rt.InstantiateModule(ctx, compiled, cfg)
		fromGuestW.Close()
		done <- err
	}()

	enc := json.NewEncoder(toGuestW)
	if kind == "sh" {
		if err := enc.Encode(map[string]any{"script": string(script), "args": o.Args}); err != nil {
			return nil, err
		}
	}

	// decided remembers the answer for an action already asked about, so a
	// loop does not ask a person the same question on every pass.
	decided := map[string]bool{}
	gate := func(op func(approve bool) reply, describe func() string, effect string) reply {
		rp := op(false)
		if !rp.Gated {
			return rp
		}
		action := describe()
		ok, asked := decided[action]
		if !asked {
			ok = o.Approve != nil && o.Approve(Ask{Primitive: m.Metadata.Name, Effect: effect, Action: action})
			decided[action] = ok
			b, _ := json.Marshal(map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano),
				"outcome": map[bool]string{true: "approved", false: "declined"}[ok], "action": action, "effect": effect})
			journal.Write(append(b, '\n'))
		}
		if !ok {
			return rp
		}
		return op(true)
	}

	res := &Result{Admission: adm}
	rd := bufio.NewReaderSize(fromGuestR, 1<<20)
	var final *request
	for {
		line, rerr := rd.ReadBytes('\n')
		if len(line) > 0 {
			var rq request
			if jerr := json.Unmarshal(line, &rq); jerr != nil {
				logf("guest      non-protocol output: %s", strings.TrimSpace(string(line)))
			} else if rq.Method == "return" {
				final = &rq
				break
			} else {
				var rp reply
				counted := true
				switch rq.Method {
				case "tools":
					rp.Tools = []string{}
					if adm != nil {
						rp.Tools = adm.aliases()
					}
					counted = false
				case "call":
					effect, action := "write", "call "+rq.Alias
					if adm != nil {
						if bd := adm.byAlias[rq.Alias]; bd != nil {
							effect = bd.effective()
							args, _ := json.Marshal(rq.Arguments)
							action = fmt.Sprintf("call %s / %s with %s", bd.Server, bd.Tool, args)
						}
					}
					rp = gate(func(a bool) reply { return callTool(adm, br, rq, a, journal) }, func() string { return action }, effect)
				case "exec":
					_, effect := resolve(&m, rq.Command, rq.Args)
					rp = gate(func(a bool) reply { return runCommand(&m, rq, a, journal) },
						func() string { return "run " + strings.TrimSpace(rq.Command+" "+strings.Join(rq.Args, " ")) }, effect)
				case "read", "write", "canwrite":
					rp = gate(func(a bool) reply { return fileOp(&m, rq, a, journal) },
						func() string { return "write the file " + rq.Path }, "write")
					counted = rq.Method != "canwrite"
				case "fetch":
					rp = gate(func(a bool) reply { return fetchOp(&m, rq, a, journal) },
						func() string { return "send " + strings.ToUpper(rq.HTTPMethod) + " to " + rq.URL }, "write")
				default:
					rp.Refused = "unknown request"
				}
				if rp.Refused != "" {
					res.Refused++
				} else if counted {
					res.Ran++
				}
				if err := enc.Encode(rp); err != nil {
					return nil, err
				}
			}
		}
		if rerr != nil {
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
	logf("%d call(s) and command(s) run, %d refused, in %s", res.Ran, res.Refused, time.Since(t0).Round(time.Millisecond))
	if final == nil {
		return nil, fmt.Errorf("the primitive ended without a result")
	}
	res.Exit, res.Stdout, res.Stderr = final.Exit, final.Stdout, final.Stderr
	return res, nil
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
    def _cap(self, o):
        _out.write(json.dumps(o) + "\n"); _out.flush()
        r = json.loads(_in.readline())
        if r.get("refused"): raise PermissionError(r["refused"])
        if r.get("exit"): raise OSError(r.get("stderr") or "failed")
        return r
    def read(self, path): return self._cap({"method": "read", "path": path}).get("result", "")
    def write(self, path, text): self._cap({"method": "write", "path": path, "stdin": text})
    def fetch(self, url, method="GET", body="", headers=None):
        r = self._cap({"method": "fetch", "url": url, "http_method": method, "stdin": body, "headers": headers or {}})
        return {"status": r.get("status"), "body": r.get("result", "")}
    def tools(self):
        _out.write(json.dumps({"method": "tools"}) + "\n"); _out.flush()
        return json.loads(_in.readline()).get("tools") or []
    def exec(self, command, args=(), stdin=""):
        _out.write(json.dumps({"method": "exec", "command": command, "args": list(args), "stdin": stdin}) + "\n"); _out.flush()
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
  _cap(o) { const r = _ask(o); if (r.refused) throw new Error(r.refused); if (r.exit) throw new Error(r.stderr || "failed"); return r; },
  read(path) { return this._cap({ method: "read", path }).result ?? ""; },
  write(path, text) { this._cap({ method: "write", path, stdin: text }); },
  fetch(url, method = "GET", body = "", headers = {}) { const r = this._cap({ method: "fetch", url, http_method: method, stdin: body, headers }); return { status: r.status, body: r.result ?? "" }; },
  exec(command, args = [], stdin = "") { return _ask({ method: "exec", command, args, stdin }); },
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

func logf(f string, a ...any) { fmt.Fprintf(os.Stderr, "host  "+f+"\n", a...) }

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "host  fatal:", err)
		os.Exit(1)
	}
}
