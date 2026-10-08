// Package sharedwire is the relay side of the shared runner: how a session a
// client started reaches the one runner that serves every session on this
// machine, and how the session's messages pass between them.
//
// It uses only the standard library, so the relay built from it (./relay)
// stays a few megabytes: a client keeps one relay per session for as long as
// the session is open, and some clients keep dozens.
package sharedwire

import (
	"bufio"
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
	"sync"
	"time"
)

// Hello is the first line a relay sends: who the session is.
type Hello struct {
	Key  string   `json:"key"`
	Dir  string   `json:"dir"`
	Env  []string `json:"env"`
	Args []string `json:"args"`
}

// Answer is the runner's reply to a Hello.
type Answer struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// ErrNoShared is returned, wrapped, when no runner can be reached.
var ErrNoShared = errors.New("no shared runner")

// Logf writes a line of the relay's own log, to standard error.
var Logf = func(format string, a ...any) { fmt.Fprintf(os.Stderr, "tap  "+format+"\n", a...) }

// Dir is where the runner keeps its socket, lock and log: private to this
// user. A variable so a test can move it.
var Dir = func() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "tap-runtime", "shared"), nil
}

// Paths are the files of the runner for one key.
type Paths struct{ Dir, Sock, Lock, Log string }

// PathsFor names the runner's files for key.
func PathsFor(key string) (Paths, error) {
	dir, err := Dir()
	if err != nil {
		return Paths{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Paths{}, err
	}
	os.Chmod(dir, 0o700)
	sum := sha256.Sum256([]byte(key))
	name := "r-" + hex.EncodeToString(sum[:5])
	p := Paths{Dir: dir, Sock: filepath.Join(dir, name+".sock"), Lock: filepath.Join(dir, name+".lock"), Log: filepath.Join(dir, "runner.log")}
	// A socket path is limited to about 100 bytes (104 on macOS). A deep
	// cache directory keeps the lock and log, and the socket goes to a short
	// private directory.
	if len(p.Sock) > 100 {
		short, err := shortSocketDir()
		if err != nil {
			return Paths{}, err
		}
		p.Sock = filepath.Join(short, name+".sock")
		if len(p.Sock) > 100 {
			return Paths{}, fmt.Errorf("the socket path %s is too long", p.Sock)
		}
	}
	return p, nil
}

// Dial reaches the runner, starting runner (the tap program) as one when
// none answers.
func Dial(p Paths, runner string) (net.Conn, error) {
	if c, err := net.DialTimeout("unix", p.Sock, time.Second); err == nil {
		return c, nil
	}
	// Sessions often start together. One relay starts the runner; the rest
	// wait for it here and find it running.
	if f, err := os.OpenFile(p.Lock+".start", os.O_CREATE|os.O_RDWR, 0o600); err == nil {
		if LockFile(f) == nil {
			defer f.Close()
			if c, err := net.DialTimeout("unix", p.Sock, time.Second); err == nil {
				return c, nil
			}
		} else {
			f.Close()
		}
	}
	if err := StartRunner(p, runner); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("unix", p.Sock, time.Second); err == nil {
			return c, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("the runner did not start within 10 s (see %s)", p.Log)
}

// StartRunner starts runner shared-runner, detached from this session.
// Several relays may start one at once; all but the first find the runner's
// lock taken and stop.
func StartRunner(p Paths, runner string) error {
	logFile, err := os.OpenFile(p.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(runner, "shared-runner")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Dir = home
	}
	Detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// SendHello opens a session on the runner.
func SendHello(c net.Conn, r *bufio.Reader, h Hello) error {
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
	var a Answer
	if json.Unmarshal(line, &a) != nil {
		return fmt.Errorf("the runner's answer could not be read")
	}
	if !a.OK {
		return fmt.Errorf("the runner refused the session: %s", a.Error)
	}
	return nil
}

// Relay passes one client session, read from in and answered on out, to the
// runner. It returns ErrNoShared, wrapped and having read nothing from in,
// when no runner can be reached.
func Relay(in io.Reader, out io.Writer, p Paths, runner string, h Hello) error {
	connect := func() (net.Conn, *bufio.Reader, error) {
		c, err := Dial(p, runner)
		if err != nil {
			return nil, nil, err
		}
		r := bufio.NewReaderSize(c, 64*1024)
		if err := SendHello(c, r, h); err != nil {
			c.Close()
			return nil, nil, err
		}
		return c, r, nil
	}
	c, r, err := connect()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNoShared, err)
	}
	l := &link{out: out, conn: c, reader: r, connect: connect, inflight: map[string]bool{}}
	return l.run(in)
}

// link is one client session passed through to the runner. If the runner
// stops while the client is still there, the link starts another and opens
// the session again with the client's own initialize, and every request the
// old runner had not answered is answered with an error.
type link struct {
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

type peek struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
}

func (l *link) toClient(b []byte) {
	l.outMu.Lock()
	defer l.outMu.Unlock()
	l.out.Write(b)
}

func (l *link) run(in io.Reader) error {
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
		var m peek
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
func (l *link) pump(r *bufio.Reader) {
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var m peek
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
func (l *link) reconnect() bool {
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
		Logf("runner     stopped three times in a minute; ending this session")
		os.Exit(1)
	}
	lost := l.inflight
	l.inflight = map[string]bool{}
	l.mu.Unlock()
	type rpcError struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	for id := range lost {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "error": rpcError{-32603, "the TAP runner restarted before it answered; call again"}})
		l.toClient(append(b, '\n'))
	}
	c, r, err := l.connect()
	if err != nil {
		Logf("runner     could not be restarted: %v", err)
		os.Exit(1)
	}
	l.mu.Lock()
	l.conn, l.reader = c, r
	replay, done := l.initialize, l.initialized
	if replay != nil {
		var m peek
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
	Logf("runner     restarted; session opened again")
	return true
}
