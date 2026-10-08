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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/bridge"
	"github.com/Telara-Labs/TAP-Runtime/internal/sharedwire"
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
		// Where it can, this process becomes the small relay, and the whole
		// runner does not stay in memory for each session a client keeps
		// open. becomeRelay returns only when it could not.
		if err := becomeRelay(args); err != nil && len(relayBinary) > 0 {
			logf("relay      relaying in this process: %v", err)
		}
		err := relayToShared(in, out, args)
		if !errors.Is(err, sharedwire.ErrNoShared) {
			return err
		}
		logf("runner     answering in this process: %v", err)
	}
	return serve(in, out, args)
}

// relayToShared passes one client session to the shared runner. It returns
// an error wrapping sharedwire.ErrNoShared, having read nothing from in, when
// no runner can be reached.
func relayToShared(in io.Reader, out io.Writer, args []string) error {
	key := sharedKey()
	p, err := sharedwire.PathsFor(key)
	if err != nil {
		return fmt.Errorf("%w: %v", sharedwire.ErrNoShared, err)
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("%w: %v", sharedwire.ErrNoShared, err)
	}
	dir, _ := os.Getwd()
	return sharedwire.Relay(in, out, p, exe, sharedwire.Hello{Key: key, Dir: dir, Env: os.Environ(), Args: args, Parent: os.Getppid()})
}

// sharedRunnerCommand is `tap shared-runner`: the runner every relay talks
// to. It is started by a relay, not by a person.
func sharedRunnerCommand(args []string, stderr io.Writer) int {
	key := sharedKey()
	p, err := sharedwire.PathsFor(key)
	if err != nil {
		fmt.Fprintln(stderr, "tap:", err)
		return 1
	}
	lock, err := os.OpenFile(p.Lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fmt.Fprintln(stderr, "tap:", err)
		return 1
	}
	defer lock.Close()
	if sharedwire.TryLockFile(lock) != nil {
		// Another runner holds it: this one is not needed.
		return 0
	}
	// The lock file names the runner, for a person or a test to find it.
	lock.Truncate(0)
	fmt.Fprintf(lock, "%d\n", os.Getpid())
	if info, err := os.Stat(p.Log); err == nil && info.Size() > 8<<20 {
		os.Truncate(p.Log, 0)
	}
	os.Remove(p.Sock)
	ln, err := net.Listen("unix", p.Sock)
	if err != nil {
		fmt.Fprintln(stderr, "tap:", err)
		return 1
	}
	defer os.Remove(p.Sock)
	os.Chmod(p.Sock, 0o600)
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
	answer := func(a sharedwire.Answer) {
		b, _ := json.Marshal(a)
		c.Write(append(b, '\n'))
	}
	var h sharedwire.Hello
	if err != nil || json.Unmarshal(bytes.TrimSpace(line), &h) != nil {
		answer(sharedwire.Answer{Error: "the session could not be read"})
		return
	}
	if h.Key != r.key {
		answer(sharedwire.Answer{Error: "this runner is " + r.key + ", the relay is " + h.Key})
		return
	}
	if !sharedEligible(h.Args) {
		answer(sharedwire.Answer{Error: "these flags need a process of their own"})
		return
	}
	s, _, done, err := newServer(c, h.Args)
	if err != nil {
		answer(sharedwire.Answer{Error: err.Error()})
		return
	}
	defer done()
	s.proc = bridge.Proc{Dir: h.Dir, Env: h.Env}
	s.agentPid = h.Parent
	s.histories = r.histories
	answer(sharedwire.Answer{OK: true})
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
// A minute missed a repeat: the second ask's session was given a read made
// before the first ask was written, and was told the task was new. A read
// started no more than currentSessionSlack before a session holds every
// session that is earlier for it.
const historyReuse = currentSessionSlack

func (p *historyPool) load(client string) *historyLoad {
	if p == nil {
		return loadHistory(client)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if h := p.loads[client]; h != nil && time.Since(h.started) < historyReuse {
		return &historyLoad{started: time.Now(), done: h.done, from: h}
	}
	h := loadHistory(client)
	p.loads[client] = h
	return h
}
