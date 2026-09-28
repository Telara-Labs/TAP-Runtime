// guest-sh is a bash-compatible interpreter built for GOOS=wasip1.
//
// It is the spike for doc 34 section 13.3/13.4: a script is the primitive, the
// interpreter ships with the runner, and every external command the script
// names is handed to the host instead of being started here. This program
// cannot start a process: the target has no such call.
//
// Wire: one JSON object per line. The host sends {"script": ...} first. The
// guest sends {"method":"exec"|"return", ...} and reads one reply per exec.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"github.com/itchyny/gojq"
	"gitlab.com/telara-labs/tap-runtime/third_party/sh/expand"
	"gitlab.com/telara-labs/tap-runtime/third_party/sh/interp"
	"gitlab.com/telara-labs/tap-runtime/third_party/sh/syntax"
)

type initMsg struct {
	Script string   `json:"script"`
	Args   []string `json:"args"`
}

type request struct {
	Method     string         `json:"method"`
	Alias      string         `json:"alias,omitempty"`
	Arguments  map[string]any `json:"arguments,omitempty"`
	Command    string         `json:"command,omitempty"`
	Args       []string       `json:"args,omitempty"`
	Stdout     string         `json:"stdout,omitempty"`
	Stderr     string         `json:"stderr,omitempty"`
	Exit       int            `json:"exit"`
	Stdin      string         `json:"stdin,omitempty"`
	Path       string         `json:"path,omitempty"`
	URL        string         `json:"url,omitempty"`
	HTTPMethod string         `json:"http_method,omitempty"`
}

type reply struct {
	Refused string   `json:"refused,omitempty"`
	Result  string   `json:"result,omitempty"`
	Tools   []string `json:"tools,omitempty"`
	Stdout  string   `json:"stdout"`
	Stderr  string   `json:"stderr"`
	Exit    int      `json:"exit"`
	Status  int      `json:"status,omitempty"`
}

var (
	hostIn  = bufio.NewReaderSize(os.Stdin, 1<<20)
	hostOut = os.Stdout
)

func send(r request) {
	b, _ := json.Marshal(r)
	hostOut.Write(append(b, '\n'))
}

func recv(v any) error {
	line, err := hostIn.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return err
	}
	return json.Unmarshal(line, v)
}

func main() {
	var in initMsg
	if err := recv(&in); err != nil {
		send(request{Method: "return", Stderr: "guest: no init: " + err.Error(), Exit: 2})
		return
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(in.Script), "main.sh")
	if err != nil {
		send(request{Method: "return", Stderr: "guest: parse: " + err.Error(), Exit: 2})
		return
	}
	var stdout, stderr bytes.Buffer
	runner, err := interp.New(
		interp.StdIO(nil, &stdout, &stderr),
		interp.Env(expand.ListEnviron()),
		interp.Params(append([]string{"--"}, in.Args...)...),
		interp.ExecHandlers(execMiddleware),
		interp.OpenHandler(openHandler),
	)
	if err != nil {
		send(request{Method: "return", Stderr: "guest: runner: " + err.Error(), Exit: 2})
		return
	}
	exit := 0
	if err := runner.Run(context.Background(), file); err != nil {
		var es interp.ExitStatus
		if errors.As(err, &es) {
			exit = int(es)
		} else {
			stderr.WriteString("guest: " + err.Error() + "\n")
			exit = 1
		}
	}
	send(request{Method: "return", Stdout: stdout.String(), Stderr: stderr.String(), Exit: exit})
}

// openHandler sends every file the script opens to the host, which allows it
// only inside what the manifest declares. Reading fetches the content at
// open; writing sends it at close. Appending is refused: the host is given
// whole files.
func openHandler(ctx context.Context, path string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
	if path == "/dev/null" {
		return devNull{}, nil
	}
	// A relative path is sent as written. The host takes it from the
	// directory the runner was started in; the sandbox has no directory.
	if flag&os.O_APPEND != 0 {
		return nil, &fs.PathError{Op: "open", Path: path, Err: errors.New("appending is not supported; write the whole file")}
	}
	if flag&(os.O_WRONLY|os.O_RDWR) != 0 {
		send(request{Method: "canwrite", Path: path})
		var rp reply
		if err := recv(&rp); err != nil {
			return nil, &fs.PathError{Op: "open", Path: path, Err: err}
		}
		if rp.Refused != "" {
			return nil, &fs.PathError{Op: "open", Path: path, Err: errors.New("REFUSED by host: " + rp.Refused)}
		}
		return &hostFile{path: path, writing: true}, nil
	}
	send(request{Method: "read", Path: path})
	var rp reply
	if err := recv(&rp); err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	if rp.Refused != "" {
		return nil, &fs.PathError{Op: "open", Path: path, Err: errors.New("REFUSED by host: " + rp.Refused)}
	}
	if rp.Exit != 0 {
		return nil, &fs.PathError{Op: "open", Path: path, Err: errors.New(rp.Stderr)}
	}
	return &hostFile{path: path, r: strings.NewReader(rp.Result)}, nil
}

type hostFile struct {
	path    string
	writing bool
	r       *strings.Reader
	w       bytes.Buffer
}

func (f *hostFile) Read(p []byte) (int, error) {
	if f.r == nil {
		return 0, io.EOF
	}
	return f.r.Read(p)
}
func (f *hostFile) Write(p []byte) (int, error) { return f.w.Write(p) }
func (f *hostFile) Close() error {
	if !f.writing {
		return nil
	}
	send(request{Method: "write", Path: f.path, Stdin: f.w.String()})
	var rp reply
	if err := recv(&rp); err != nil {
		return err
	}
	if rp.Refused != "" {
		return &fs.PathError{Op: "write", Path: f.path, Err: errors.New("REFUSED by host: " + rp.Refused)}
	}
	if rp.Exit != 0 {
		return &fs.PathError{Op: "write", Path: f.path, Err: errors.New(rp.Stderr)}
	}
	return nil
}

type devNull struct{}

func (devNull) Read([]byte) (int, error)    { return 0, io.EOF }
func (devNull) Write(p []byte) (int, error) { return len(p), nil }
func (devNull) Close() error                { return nil }

func execMiddleware(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		hc := interp.HandlerCtx(ctx)
		switch args[0] {
		case "jq":
			return runJQ(hc, args[1:])
		case "head":
			return runHead(hc, args[1:])
		case "wc":
			return runWC(hc, args[1:])
		case "tap":
			return runTap(hc, args[1:])
		case "cat":
			return runCat(ctx, hc, args[1:])
		}
		// Anything else is a host program. Never call next: the default
		// handler would try to start a process.
		// A program at the receiving end of a pipe is given what was piped.
		stdin := ""
		if hc.Stdin != nil {
			b, _ := io.ReadAll(hc.Stdin)
			stdin = string(b)
		}
		send(request{Method: "exec", Command: args[0], Args: args[1:], Stdin: stdin})
		var rp reply
		if err := recv(&rp); err != nil {
			fmt.Fprintf(hc.Stderr, "%s: host did not answer: %v\n", args[0], err)
			return interp.ExitStatus(125)
		}
		if rp.Refused != "" {
			fmt.Fprintf(hc.Stderr, "%s: REFUSED by host: %s\n", args[0], rp.Refused)
			return interp.ExitStatus(126)
		}
		io.WriteString(hc.Stdout, rp.Stdout)
		io.WriteString(hc.Stderr, rp.Stderr)
		if rp.Exit != 0 {
			return interp.ExitStatus(uint8(rp.Exit))
		}
		return nil
	}
}

func runJQ(hc interp.HandlerContext, args []string) error {
	raw := false
	filter := "."
	for _, a := range args {
		switch {
		case a == "-r":
			raw = true
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(hc.Stderr, "jq: unsupported flag %s\n", a)
			return interp.ExitStatus(2)
		default:
			filter = a
		}
	}
	q, err := gojq.Parse(filter)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "jq: %v\n", err)
		return interp.ExitStatus(3)
	}
	if hc.Stdin == nil {
		fmt.Fprintln(hc.Stderr, "jq: no input")
		return interp.ExitStatus(2)
	}
	dec := json.NewDecoder(hc.Stdin)
	dec.UseNumber()
	for {
		var v any
		if err := dec.Decode(&v); err == io.EOF {
			return nil
		} else if err != nil {
			fmt.Fprintf(hc.Stderr, "jq: input: %v\n", err)
			return interp.ExitStatus(2)
		}
		it := q.Run(normalize(v))
		for {
			out, ok := it.Next()
			if !ok {
				break
			}
			if e, isErr := out.(error); isErr {
				fmt.Fprintf(hc.Stderr, "jq: %v\n", e)
				return interp.ExitStatus(5)
			}
			if s, isStr := out.(string); isStr && raw {
				fmt.Fprintln(hc.Stdout, s)
				continue
			}
			b, _ := gojq.Marshal(out)
			fmt.Fprintln(hc.Stdout, string(b))
		}
	}
}

// normalize turns json.Number into the numeric types gojq accepts.
func normalize(v any) any {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return int(i)
		}
		f, _ := t.Float64()
		return f
	case map[string]any:
		for k, e := range t {
			t[k] = normalize(e)
		}
	case []any:
		for i, e := range t {
			t[i] = normalize(e)
		}
	}
	return v
}

func runHead(hc interp.HandlerContext, args []string) error {
	n := 10
	for i := 0; i < len(args); i++ {
		if args[i] == "-n" && i+1 < len(args) {
			n, _ = strconv.Atoi(args[i+1])
			i++
		}
	}
	if hc.Stdin == nil {
		return nil
	}
	sc := bufio.NewScanner(hc.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for i := 0; i < n && sc.Scan(); i++ {
		fmt.Fprintln(hc.Stdout, sc.Text())
	}
	io.Copy(io.Discard, hc.Stdin)
	return nil
}

func runWC(hc interp.HandlerContext, args []string) error {
	if hc.Stdin == nil {
		fmt.Fprintln(hc.Stdout, 0)
		return nil
	}
	b, _ := io.ReadAll(hc.Stdin)
	fmt.Fprintln(hc.Stdout, bytes.Count(b, []byte{'\n'}))
	return nil
}

// runTap is the shell's form of the SDK:
//
//	tap tools                      the aliases that are bound, one per line
//	tap call <alias> [json]        call a tool; its result goes to stdout
func runTap(hc interp.HandlerContext, args []string) error {
	if len(args) == 1 && args[0] == "tools" {
		send(request{Method: "tools"})
		var rp reply
		if err := recv(&rp); err != nil {
			return interp.ExitStatus(125)
		}
		for _, t := range rp.Tools {
			fmt.Fprintln(hc.Stdout, t)
		}
		return nil
	}
	if len(args) >= 2 && args[0] == "fetch" {
		method := "GET"
		if len(args) > 2 {
			method = args[2]
		}
		body := ""
		if method != "GET" && method != "HEAD" && hc.Stdin != nil {
			b, _ := io.ReadAll(hc.Stdin)
			body = string(b)
		}
		send(request{Method: "fetch", URL: args[1], HTTPMethod: method, Stdin: body})
		var rp reply
		if err := recv(&rp); err != nil {
			return interp.ExitStatus(125)
		}
		if rp.Refused != "" {
			fmt.Fprintf(hc.Stderr, "tap fetch: REFUSED by host: %s\n", rp.Refused)
			return interp.ExitStatus(126)
		}
		if rp.Exit != 0 {
			io.WriteString(hc.Stderr, rp.Stderr+"\n")
			return interp.ExitStatus(uint8(rp.Exit))
		}
		io.WriteString(hc.Stdout, rp.Result)
		if rp.Status >= 400 {
			return interp.ExitStatus(22) // what curl --fail returns
		}
		return nil
	}
	if len(args) < 2 || args[0] != "call" {
		fmt.Fprintln(hc.Stderr, "usage: tap tools | tap call <alias> [json] | tap fetch <url> [method]")
		return interp.ExitStatus(2)
	}
	arguments := map[string]any{}
	if len(args) > 2 {
		if err := json.Unmarshal([]byte(args[2]), &arguments); err != nil {
			fmt.Fprintf(hc.Stderr, "tap call: arguments are not a JSON object: %v\n", err)
			return interp.ExitStatus(2)
		}
	}
	send(request{Method: "call", Alias: args[1], Arguments: arguments})
	var rp reply
	if err := recv(&rp); err != nil {
		fmt.Fprintf(hc.Stderr, "tap call: host did not answer: %v\n", err)
		return interp.ExitStatus(125)
	}
	if rp.Refused != "" {
		fmt.Fprintf(hc.Stderr, "tap call %s: REFUSED by host: %s\n", args[1], rp.Refused)
		return interp.ExitStatus(126)
	}
	if rp.Exit != 0 {
		io.WriteString(hc.Stderr, rp.Stderr+"\n")
		return interp.ExitStatus(uint8(rp.Exit))
	}
	fmt.Fprintln(hc.Stdout, rp.Result)
	return nil
}

// runCat copies its standard input, or the files it names, to its output.
// Files go through openHandler, so they are bounded like any other.
func runCat(ctx context.Context, hc interp.HandlerContext, args []string) error {
	if len(args) == 0 {
		if hc.Stdin != nil {
			io.Copy(hc.Stdout, hc.Stdin)
		}
		return nil
	}
	for _, a := range args {
		f, err := openHandler(ctx, a, os.O_RDONLY, 0)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "cat: %v\n", err)
			return interp.ExitStatus(1)
		}
		io.Copy(hc.Stdout, f)
		f.Close()
	}
	return nil
}
