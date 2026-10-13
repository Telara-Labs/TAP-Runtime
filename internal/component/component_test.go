package component

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/sys"
)

// buildRust compiles one Rust source file for a WASI target with cargo. These
// tests run real components produced by a real toolchain; they skip, saying
// why, where Rust or its WASI target is not installed.
func buildRust(t *testing.T, src, target string) []byte {
	t.Helper()
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("cargo is not installed; install Rust to run component tests")
	}
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte("[package]\nname = \"probe\"\nversion = \"0.1.0\"\nedition = \"2021\"\n[profile.release]\nopt-level = \"s\"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "src", "main.rs"), []byte(src), 0o644)
	cmd := exec.Command("cargo", "build", "--offline", "--release", "--target", target)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CARGO_TARGET_DIR="+filepath.Join(dir, "target"))
	if out, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "target may not be installed") || strings.Contains(string(out), "can't find crate for `std`") {
			t.Skipf("Rust target %s is not installed (rustup target add %s)", target, target)
		}
		t.Fatalf("cargo build: %v\n%s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(dir, "target", target, "release", "probe.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const probeSource = `
use std::io::{BufRead, Write};
fn main() {
    let args: Vec<String> = std::env::args().collect();
    let mode = args.get(1).cloned().unwrap_or_default();
    match mode.as_str() {
        "probe" => {
            let env = std::env::vars().count();
            let fs = match std::fs::read(&args[2]) { Ok(_) => "read".to_string(), Err(e) => format!("{:?}", e.kind()) };
            let dir = match std::fs::read_dir("/") { Ok(_) => "listed".to_string(), Err(e) => format!("{:?}", e.kind()) };
            let tcp = match std::net::TcpStream::connect("127.0.0.1:9") { Ok(_) => "connected".to_string(), Err(e) => format!("{:?}", e.kind()) };
            let udp = match std::net::UdpSocket::bind("127.0.0.1:0") { Ok(_) => "bound".to_string(), Err(e) => format!("{:?}", e.kind()) };
            let cwd = std::env::current_dir().is_ok();
            let t0 = std::time::Instant::now();
            std::thread::sleep(std::time::Duration::from_millis(20));
            let slept = t0.elapsed().as_millis() >= 20;
            let wall = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap().as_secs() > 1_600_000_000;
            println!("env={} fs={} dir={} tcp={} udp={} cwd={} slept={} wall={}", env, fs, dir, tcp, udp, cwd, slept, wall);
        }
        "echo" => {
            let mut line = String::new();
            std::io::stdin().lock().read_line(&mut line).unwrap();
            print!("got {}", line);
            std::io::stdout().flush().unwrap();
            eprintln!("to stderr");
        }
        "fail" => std::process::exit(3),
        "panic" => panic!("boom"),
        _ => println!("args={:?}", &args[1..]),
    }
}
`

type run struct {
	stdout, stderr bytes.Buffer
	err            error
}

func runComponent(t *testing.T, wasm []byte, stdin string, args ...string) *run {
	t.Helper()
	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCloseOnContextDone(true))
	defer rt.Close(ctx)
	p, err := Load(ctx, rt, wasm)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := &run{}
	r.err = p.Run(ctx, Config{
		Args:  append([]string{"primitive"}, args...),
		Stdin: strings.NewReader(stdin), Stdout: &r.stdout, Stderr: &r.stderr,
		Walltime: func() (int64, int32) { n := time.Now(); return n.Unix(), int32(n.Nanosecond()) },
		Nanotime: func() int64 { return time.Now().UnixNano() },
	})
	return r
}

func TestDetect(t *testing.T) {
	for _, c := range []struct {
		in   []byte
		want Format
		err  bool
	}{
		{[]byte("\x00asm\x01\x00\x00\x00"), Module, false},
		{[]byte("\x00asm\x0d\x00\x01\x00"), Component, false},
		{[]byte("\x00asm\x0c\x00\x01\x00"), "", true},
		{[]byte("\x00asm\x02\x00\x00\x00"), "", true},
		{[]byte("#!/bin/sh"), "", true},
		{nil, "", true},
	} {
		got, err := Detect(c.in)
		if got != c.want || (err != nil) != c.err {
			t.Errorf("Detect(%q) = %q, %v", c.in, got, err)
		}
	}
}

func TestARealRustComponentRunsWithNoAmbientAuthority(t *testing.T) {
	wasm := buildRust(t, probeSource, "wasm32-wasip2")
	if f, _ := Detect(wasm); f != Component {
		t.Fatalf("cargo built %q, not a component", f)
	}
	secret := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(secret, []byte("host data"), 0o600)
	t.Setenv("TAP_COMPONENT_TEST_SECRET", "leak")

	r := runComponent(t, wasm, "", "probe", secret)
	if r.err != nil {
		t.Fatalf("run: %v\n%s", r.err, r.stderr.String())
	}
	got := strings.TrimSpace(r.stdout.String())
	for _, want := range []string{"env=0", "tcp=PermissionDenied", "udp=PermissionDenied", "slept=true", "wall=true"} {
		if !strings.Contains(got, want) {
			t.Errorf("probe output %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "fs=read") || strings.Contains(got, "dir=listed") {
		t.Errorf("the component reached the host filesystem: %s", got)
	}

	t.Run("arguments", func(t *testing.T) {
		r := runComponent(t, wasm, "", "a b", `{"x":1}`)
		if r.err != nil || strings.TrimSpace(r.stdout.String()) != `args=["a b", "{\"x\":1}"]` {
			t.Fatalf("%v %q", r.err, r.stdout.String())
		}
	})
	t.Run("standard streams", func(t *testing.T) {
		r := runComponent(t, wasm, "hello\n", "echo")
		if r.err != nil || r.stdout.String() != "got hello\n" || r.stderr.String() != "to stderr\n" {
			t.Fatalf("%v %q %q", r.err, r.stdout.String(), r.stderr.String())
		}
	})
	t.Run("failure exit", func(t *testing.T) {
		r := runComponent(t, wasm, "", "fail")
		var ee *sys.ExitError
		if !errors.As(r.err, &ee) || ee.ExitCode() == 0 {
			t.Fatalf("exit: %v", r.err)
		}
	})
	t.Run("panic", func(t *testing.T) {
		r := runComponent(t, wasm, "", "panic")
		if r.err == nil || !strings.Contains(r.stderr.String(), "boom") {
			t.Fatalf("panic: %v %q", r.err, r.stderr.String())
		}
	})
}

func TestAProtocolExchangeOverPipes(t *testing.T) {
	wasm := buildRust(t, probeSource, "wasm32-wasip2")
	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCloseOnContextDone(true))
	defer rt.Close(ctx)
	p, err := Load(ctx, rt, wasm)
	if err != nil {
		t.Fatal(err)
	}
	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- p.Run(ctx, Config{Args: []string{"primitive", "echo"}, Stdin: inR, Stdout: outW, Stderr: io.Discard,
			Walltime: func() (int64, int32) { return 0, 0 }, Nanotime: func() int64 { return 0 }})
		outW.Close()
	}()
	inW.Write([]byte("over a pipe\n"))
	line, _ := bufio.NewReader(outR).ReadString('\n')
	if line != "got over a pipe\n" {
		t.Fatalf("%q", line)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestACancelledRunStops(t *testing.T) {
	wasm := buildRust(t, `fn main() { loop { std::hint::black_box(0); } }`, "wasm32-wasip2")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCloseOnContextDone(true))
	defer rt.Close(context.Background())
	p, err := Load(ctx, rt, wasm)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = p.Run(ctx, Config{Args: []string{"primitive"}, Walltime: func() (int64, int32) { return 0, 0 }, Nanotime: func() int64 { return 0 }})
	if err == nil || time.Since(start) > 10*time.Second {
		t.Fatalf("loop not stopped: %v", err)
	}
}

func TestLoadRefusesWhatTAPDoesNotProvide(t *testing.T) {
	for _, c := range []struct{ name, want string }{
		{"wasi:http/outgoing-handler@0.2.0", "no direct network access"},
		{"wasi:keyvalue/store@0.2.0", "does not provide"},
		{"example:custom/thing", "does not provide"},
		{"wasi:io/streams@0.3.0", "needs WASI Preview 3"},
	} {
		var err error
		func() {
			defer catch(&err)
			interfaceName(c.name)
		}()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	for _, ok := range []string{"wasi:io/streams@0.2.3", "wasi:sockets/tcp@0.2.0", "wasi:cli/environment"} {
		var err error
		func() {
			defer catch(&err)
			interfaceName(ok)
		}()
		if err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
}

func TestLoadRefusesMalformedInput(t *testing.T) {
	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	defer rt.Close(ctx)
	head := string(componentHeader)
	for name, b := range map[string]string{
		"core module":         string(moduleHeader),
		"truncated section":   head + "\x07\x05\x01",
		"huge vector":         head + "\x07\x05\xff\xff\xff\xff\x0f",
		"unknown section":     head + "\x63\x00",
		"start function":      head + "\x09\x00",
		"no run export":       head,
		"bad type":            head + "\x07\x02\x01\x01",
		"trailing bytes":      head + "\x07\x03\x01\x7f\x00",
		"out of range alias":  head + "\x06\x06\x01\x01\x00\x05\x01a",
		"core instance index": head + "\x02\x04\x01\x00\x07\x00",
	} {
		if _, err := Load(ctx, rt, []byte(b)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Loading untrusted bytes must fail cleanly, never crash the runner: every
// prefix of a real component, and copies with corrupted bytes, are loaded.
func TestLoadNeverPanicsOnTruncatedOrCorruptedInput(t *testing.T) {
	wasm := buildRust(t, probeSource, "wasm32-wasip2")
	ctx := context.Background()
	load := func(b []byte) {
		rt := wazero.NewRuntime(ctx)
		defer rt.Close(ctx)
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Load or Preview3 panicked: %v", r)
			}
		}()
		Preview3(b)
		Load(ctx, rt, b)
	}
	step := max(1, len(wasm)/400)
	for n := 0; n < len(wasm); n += step {
		load(wasm[:n])
	}
	seed := uint32(1)
	for i := 0; i < 300; i++ {
		b := append([]byte(nil), wasm...)
		for k := 0; k < 4; k++ {
			seed = seed*1664525 + 1013904223
			b[8+int(seed)%(len(b)-8)] ^= byte(seed >> 24)
		}
		load(b)
	}
}

func TestLoadRefusesMoreThanOneLinearMemory(t *testing.T) {
	m := parseCoreModule([]byte("\x00asm\x01\x00\x00\x00\x05\x05\x02\x00\x01\x00\x01"))
	if m.memories != 2 {
		t.Fatalf("memories %d", m.memories)
	}
	one := parseCoreModule([]byte("\x00asm\x01\x00\x00\x00\x05\x03\x01\x00\x01"))
	check := func(mods []*coreModule, instances ...int) (err error) {
		defer catch(&err)
		s := &space{coreModules: mods}
		for _, i := range instances {
			s.coreInstances = append(s.coreInstances, &coreInstance{real: true, module: i})
		}
		checkMemories(s)
		return nil
	}
	if err := check([]*coreModule{one}, 0); err != nil {
		t.Fatal(err)
	}
	if err := check([]*coreModule{m}, 0); err == nil || !strings.Contains(err.Error(), "2 linear memories") {
		t.Fatalf("%v", err)
	}
	if err := check([]*coreModule{one}, 0, 0); err == nil {
		t.Fatal("a module instantiated twice gives two memories")
	}
}
