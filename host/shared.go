package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/bridge"
)

// One runner serves every agent session on this machine.
//
// An agent starts its MCP servers once per session, and some start dozens of
// sessions at a time: one agent started 41 copies of tap serve within a
// minute, and together they held 28 GB. So tap serve, as a client starts it,
// is a relay of a few megabytes. It passes the client's messages to one
// shared runner over a socket private to this user, with the directory and
// environment the client started it in, and the runner answers each session
// as that session: its folder, its environment, its approvals. The relay
// starts the runner when none is running; the runner stops after a while
// with no sessions.
//
// Nothing changes for a client: it starts tap serve as before. A relay that
// cannot reach a runner answers the client itself, as tap serve always has.

// sharedIdle is how long the runner waits with no session before it stops.
const sharedIdle = 10 * time.Minute

// sharedHello is the first line a relay sends: who the session is.
type sharedHello struct {
	Key  string   `json:"key"`
	Dir  string   `json:"dir"`
	Env  []string `json:"env"`
	Args []string `json:"args"`
}

type sharedAnswer struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

var errNoShared = errors.New("no shared runner")

// sharedKey names this exact program: its version and the file it runs
// from. An upgrade is a different program, and its relays start a runner of
// their own instead of talking to the old one.
func sharedKey() string {
	exe, err := os.Executable()
	if err != nil {
		return version
	}
	id := exe
	if info, err := os.Stat(exe); err == nil {
		id = fmt.Sprintf("%s|%d|%d", exe, info.Size(), info.ModTime().UnixNano())
	}
	sum := sha256.Sum256([]byte(id))
	return version + "-" + hex.EncodeToString(sum[:6])
}

// sharedDir is where the runner keeps its socket, lock and log: private to
// this user. A variable so a test can move it.
var sharedDir = func() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "tap-runtime", "shared"), nil
}

type sharedPaths struct{ dir, sock, lock, log string }

func sharedPathsFor(key string) (sharedPaths, error) {
	dir, err := sharedDir()
	if err != nil {
		return sharedPaths{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return sharedPaths{}, err
	}
	os.Chmod(dir, 0o700)
	sum := sha256.Sum256([]byte(key))
	name := "r-" + hex.EncodeToString(sum[:5])
	p := sharedPaths{dir: dir, sock: filepath.Join(dir, name+".sock"), lock: filepath.Join(dir, name+".lock"), log: filepath.Join(dir, "runner.log")}
	// A socket path is limited to about 100 bytes (104 on macOS). A deep
	// cache directory keeps the lock and log, and the socket goes to a short
	// private directory.
	if len(p.sock) > 100 {
		short, err := shortSocketDir()
		if err != nil {
			return sharedPaths{}, err
		}
		p.sock = filepath.Join(short, name+".sock")
		if len(p.sock) > 100 {
			return sharedPaths{}, fmt.Errorf("the socket path %s is too long", p.sock)
		}
	}
	return p, nil
}

// sharedEligible reports whether a session started with these flags can be
// answered by the shared runner. A transport of its own (--http-listen), a
// configuration directory of its own (--config-dir, which the runner would
// apply to every session), or --own-process keep it in this process.
func sharedEligible(args []string) bool {
	for _, a := range args {
		name := strings.TrimLeft(a, "-")
		name, _, _ = strings.Cut(name, "=")
		if a != name && (name == "http-listen" || name == "config-dir" || name == "own-process") {
			return false
		}
	}
	return true
}

// serveEntry is tap serve as a client starts it.
func serveEntry(in io.Reader, out io.Writer, args []string) error {
	if sharedEligible(args) {
		err := relayToShared(in, out, args)
		if !errors.Is(err, errNoShared) {
			return err
		}
		logf("runner     answering in this process: %v", err)
	}
	return serve(in, out, args)
}

// dialShared reaches the runner, starting one when none answers.
func dialShared(p sharedPaths, key string) (net.Conn, error) {
	if c, err := net.DialTimeout("unix", p.sock, time.Second); err == nil {
		return c, nil
	}
	// Sessions often start together. One relay starts the runner; the rest
	// wait for it here and find it running.
	if f, err := os.OpenFile(p.lock+".start", os.O_CREATE|os.O_RDWR, 0o600); err == nil {
		if lockFile(f) == nil {
			defer f.Close()
			if c, err := net.DialTimeout("unix", p.sock, time.Second); err == nil {
				return c, nil
			}
		} else {
			f.Close()
		}
	}
	if err := startShared(p); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("unix", p.sock, time.Second); err == nil {
			return c, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("the runner did not start within 10 s (see %s)", p.log)
}

// startShared starts the runner, detached from this session. Several relays
// may start one at once; all but the first find the lock taken and stop.
func startShared(p sharedPaths) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(p.log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(exe, "shared-runner")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Dir = home
	}
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// hello opens a session on the runner.
func hello(c net.Conn, r *bufio.Reader, h sharedHello) error {
	b, _ := json.Marshal(h)
	c.SetDeadline(time.Now().Add(10 * time.Second))
	defer c.SetDeadline(time.Time{})
	if _, err := c.Write(append(b, '\n')); err != nil {
		return err
	}
	line, err := r.ReadBytes('\n')
	if err != nil {
		return err
	}
	var a sharedAnswer
	if json.Unmarshal(line, &a) != nil {
		return fmt.Errorf("the runner's answer could not be read")
	}
	if !a.OK {
		return fmt.Errorf("the runner refused the session: %s", a.Error)
	}
	return nil
}

// relayToShared passes one client session to the shared runner. It returns
// errNoShared, having read nothing from in, when no runner can be reached.
func relayToShared(in io.Reader, out io.Writer, args []string) error {
	key := sharedKey()
	p, err := sharedPathsFor(key)
	if err != nil {
		return fmt.Errorf("%w: %v", errNoShared, err)
	}
	dir, _ := os.Getwd()
	h := sharedHello{Key: key, Dir: dir, Env: os.Environ(), Args: args}
	connect := func() (net.Conn, *bufio.Reader, error) {
		c, err := dialShared(p, key)
		if err != nil {
			return nil, nil, err
		}
		r := bufio.NewReaderSize(c, 64*1024)
		if err := hello(c, r, h); err != nil {
			c.Close()
			return nil, nil, err
		}
		return c, r, nil
	}
	c, r, err := connect()
	if err != nil {
		return fmt.Errorf("%w: %v", errNoShared, err)
	}
	rl := &relayLink{out: out, conn: c, reader: r, connect: connect, inflight: map[string]bool{}}
	return rl.run(in)
}

// relayLink is one client session passed through to the runner. If the
// runner stops while the client is still there, the link starts another and
// opens the session again with the client's own initialize, and every
// request the old runner had not answered is answered with an error.
type relayLink struct {
	out     io.Writer
	connect func() (net.Conn, *bufio.Reader, error)

	mu          sync.Mutex
	conn        net.Conn
	reader      *bufio.Reader
	initialize  []byte // the client's initialize, replayed on a new runner
	initialized []byte
	replayID    string // the replayed initialize's id; its answer is dropped
	inflight    map[string]bool
	clientDone  bool
	restarts    []time.Time
	outMu       sync.Mutex
}

type relayPeek struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
}

func (l *relayLink) toClient(b []byte) {
	l.outMu.Lock()
	defer l.outMu.Unlock()
	l.out.Write(b)
}

func (l *relayLink) run(in io.Reader) error {
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			l.mu.Lock()
			r := l.reader
			l.mu.Unlock()
			l.pump(r)
			if !l.reconnect() {
				return
			}
		}
	}()
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	for sc.Scan() {
		line := append(append([]byte(nil), sc.Bytes()...), '\n')
		var m relayPeek
		json.Unmarshal(line, &m)
		l.mu.Lock()
		switch {
		case m.Method == "initialize":
			l.initialize = line
		case m.Method == "notifications/initialized":
			l.initialized = line
		}
		if m.Method != "" && len(m.ID) > 0 {
			l.inflight[string(m.ID)] = true
		}
		c := l.conn
		l.mu.Unlock()
		// A failed write is the runner gone: the reader side notices and
		// reconnects, and this request is answered with an error there.
		c.Write(line)
	}
	l.mu.Lock()
	l.clientDone = true
	c := l.conn
	l.mu.Unlock()
	// The client is gone. Let the runner finish what it was asked, then end.
	if uc, ok := c.(*net.UnixConn); ok {
		uc.CloseWrite()
	} else {
		c.Close()
	}
	<-readerDone
	return sc.Err()
}

// pump copies the runner's messages to the client until the runner closes.
func (l *relayLink) pump(r *bufio.Reader) {
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var m relayPeek
			json.Unmarshal(line, &m)
			drop := false
			if m.Method == "" && len(m.ID) > 0 {
				l.mu.Lock()
				delete(l.inflight, string(m.ID))
				if l.replayID != "" && string(m.ID) == l.replayID {
					l.replayID, drop = "", true
				}
				l.mu.Unlock()
			}
			if !drop {
				l.toClient(line)
			}
		}
		if err != nil {
			return
		}
	}
}

// reconnect opens the session on a new runner after the old one stopped. It
// gives up when the client has gone, or after three restarts in a minute.
func (l *relayLink) reconnect() bool {
	l.mu.Lock()
	l.conn.Close()
	if l.clientDone {
		l.mu.Unlock()
		return false
	}
	now := time.Now()
	recent := l.restarts[:0]
	for _, t := range l.restarts {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	l.restarts = append(recent, now)
	if len(l.restarts) > 3 {
		l.mu.Unlock()
		logf("runner     stopped three times in a minute; ending this session")
		os.Exit(1)
	}
	lost := l.inflight
	l.inflight = map[string]bool{}
	l.mu.Unlock()
	for id := range lost {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "error": rpcError{-32603, "the TAP runner restarted before it answered; call again"}})
		l.toClient(append(b, '\n'))
	}
	c, r, err := l.connect()
	if err != nil {
		logf("runner     could not be restarted: %v", err)
		os.Exit(1)
	}
	l.mu.Lock()
	l.conn, l.reader = c, r
	replay, done := l.initialize, l.initialized
	if replay != nil {
		var m relayPeek
		json.Unmarshal(replay, &m)
		l.replayID = string(m.ID)
	}
	l.mu.Unlock()
	if replay != nil {
		c.Write(replay)
	}
	if done != nil {
		c.Write(done)
	}
	logf("runner     restarted; session opened again")
	return true
}

// sharedRunnerCommand is `tap shared-runner`: the runner every relay talks
// to. It is started by a relay, not by a person.
func sharedRunnerCommand(args []string, stderr io.Writer) int {
	key := sharedKey()
	p, err := sharedPathsFor(key)
	if err != nil {
		fmt.Fprintln(stderr, "tap:", err)
		return 1
	}
	lock, err := os.OpenFile(p.lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fmt.Fprintln(stderr, "tap:", err)
		return 1
	}
	defer lock.Close()
	if tryLockFile(lock) != nil {
		// Another runner holds it: this one is not needed.
		return 0
	}
	// The lock file names the runner, for a person or a test to find it.
	lock.Truncate(0)
	fmt.Fprintf(lock, "%d\n", os.Getpid())
	if info, err := os.Stat(p.log); err == nil && info.Size() > 8<<20 {
		os.Truncate(p.log, 0)
	}
	os.Remove(p.sock)
	ln, err := net.Listen("unix", p.sock)
	if err != nil {
		fmt.Fprintln(stderr, "tap:", err)
		return 1
	}
	defer os.Remove(p.sock)
	os.Chmod(p.sock, 0o600)
	r := &sharedRunner{key: key, histories: &historyPool{loads: map[string]*historyLoad{}}}
	r.touch()
	logf("runner     %s started, pid %d", key, os.Getpid())
	go func() {
		for range time.Tick(15 * time.Second) {
			if r.active.Load() == 0 && time.Since(time.Unix(0, r.last.Load())) > sharedIdle {
				logf("runner     no sessions for %s; stopping", sharedIdle)
				ln.Close()
				return
			}
		}
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			return 0
		}
		go r.session(c)
	}
}

type sharedRunner struct {
	key       string
	histories *historyPool
	active    atomic.Int64
	last      atomic.Int64 // when a session last began or ended, UnixNano
}

func (r *sharedRunner) touch() { r.last.Store(time.Now().UnixNano()) }

func (r *sharedRunner) session(c net.Conn) {
	defer c.Close()
	r.active.Add(1)
	r.touch()
	defer func() { r.active.Add(-1); r.touch() }()
	br := bufio.NewReaderSize(c, 64*1024)
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := br.ReadBytes('\n')
	c.SetReadDeadline(time.Time{})
	answer := func(a sharedAnswer) {
		b, _ := json.Marshal(a)
		c.Write(append(b, '\n'))
	}
	var h sharedHello
	if err != nil || json.Unmarshal(bytes.TrimSpace(line), &h) != nil {
		answer(sharedAnswer{Error: "the session could not be read"})
		return
	}
	if h.Key != r.key {
		answer(sharedAnswer{Error: "this runner is " + r.key + ", the relay is " + h.Key})
		return
	}
	if !sharedEligible(h.Args) {
		answer(sharedAnswer{Error: "these flags need a process of their own"})
		return
	}
	s, _, done, err := newServer(c, h.Args)
	if err != nil {
		answer(sharedAnswer{Error: err.Error()})
		return
	}
	defer done()
	s.proc = bridge.Proc{Dir: h.Dir, Env: h.Env}
	s.histories = r.histories
	answer(sharedAnswer{OK: true})
	logf("session    opened in %s (%d open)", h.Dir, r.active.Load())
	s.serveStream(br)
}

// historyPool shares one read of an agent's history among the sessions of
// the shared runner, instead of one read held by each.
type historyPool struct {
	mu    sync.Mutex
	loads map[string]*historyLoad
}

// historyReuse is how old a read can be and still be given to a new session.
const historyReuse = time.Minute

func (p *historyPool) load(client string) *historyLoad {
	if p == nil {
		return loadHistory(client)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if h := p.loads[client]; h != nil && time.Since(h.started) < historyReuse {
		return h
	}
	h := loadHistory(client)
	p.loads[client] = h
	return h
}
