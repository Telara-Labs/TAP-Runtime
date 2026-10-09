package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/bind"
)

// The TAP relay is a plugin inside the person's own OpenCode or Kilo
// session (host/relayplugin/tap-relay.js). It listens on a Unix socket in
// /tmp/tap-relay-<uid>, a directory only this user can open, and forwards
// the session's own server routes the Kilo bridge uses: configuration, MCP
// status, health and call-tool. Reaching the session through it needs no
// second server, port, password or environment variable, and every call
// runs on the session's own MCP connections.

// errNoSessionRelay is returned when no relay of a running session fits.
var errNoSessionRelay = errors.New("no TAP relay plugin is running in this session")

// relayDir is where relays put their sockets.
func relayDir() string { return fmt.Sprintf("/tmp/tap-relay-%d", os.Getuid()) }

type relayMeta struct {
	PID       int    `json:"pid"`
	Directory string `json:"directory"`
	Socket    string `json:"socket"`
}

// FindSessionRelay returns the socket of the relay in the session that
// started this runner: the relay whose process is one of this process's
// ancestors, else one whose session directory is the runner's working
// directory. The directory and socket must belong to this user and be
// private to them.
func FindSessionRelay(wd string) (string, error) {
	if runtime.GOOS == "windows" {
		return "", errNoSessionRelay
	}
	dir := relayDir()
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() || st.Mode().Perm()&0o077 != 0 || !ownedByMe(st) {
		return "", errNoSessionRelay
	}
	metas, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	var live []relayMeta
	for _, m := range metas {
		b, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		var r relayMeta
		if json.Unmarshal(b, &r) != nil || r.PID <= 0 || filepath.Dir(r.Socket) != dir {
			continue
		}
		if !processAlive(r.PID) {
			os.Remove(m)
			os.Remove(r.Socket)
			continue
		}
		if s, err := os.Lstat(r.Socket); err != nil || s.Mode()&os.ModeSocket == 0 || !ownedByMe(s) {
			continue
		}
		live = append(live, r)
	}
	ancestors := map[int]bool{}
	for pid, i := os.Getppid(), 0; pid > 1 && i < 12; i++ {
		ancestors[pid] = true
		pid = parentOf(pid)
	}
	realWd, _ := filepath.EvalSymlinks(wd)
	var byDir []relayMeta
	for _, r := range live {
		if ancestors[r.PID] {
			return r.Socket, nil
		}
		if d, _ := filepath.EvalSymlinks(r.Directory); realWd != "" && d == realWd {
			byDir = append(byDir, r)
		}
	}
	if len(byDir) == 1 {
		return byDir[0].Socket, nil
	}
	return "", errNoSessionRelay
}

// parentOf reads a process's parent from ps, which macOS and Linux both
// have; 0 when it cannot be read.
func parentOf(pid int) int {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

// newKiloOverRelay is the Kilo bridge over a session relay: the same routes,
// reached through the relay's socket instead of a server the bridge starts.
func newKiloOverRelay(sock, wd string) (*Kilo, error) {
	k := &Kilo{base: "http://tap-relay", dir: wd, client: &http.Client{
		Timeout: 120 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}},
	}}
	b, err := k.get("/global/health")
	if err != nil {
		return nil, fmt.Errorf("the TAP relay in this session did not answer: %w", err)
	}
	var h struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(b, &h) == nil {
		k.version = h.Version
	}
	return k, nil
}

// SessionRelay reaches the person's running OpenCode or Kilo through the TAP
// relay plugin in its session. It lists every tool of the servers the
// session configured, with the annotations and schemas the servers give
// (the relay connects to each server itself), and calls one through the
// relay. The person's tool switches and permission rules in the session's
// configuration are honoured as the Kilo bridge honours them.
type SessionRelay struct {
	k      *Kilo
	client string
}

// NewSessionRelay finds the relay in the session that started this runner.
// client is "kilo" or "opencode".
func NewSessionRelay(client string, p Proc) (*SessionRelay, error) {
	sock, err := FindSessionRelay(p.Wd())
	if err != nil {
		return nil, err
	}
	k, err := newKiloOverRelay(sock, p.Wd())
	if err != nil {
		return nil, err
	}
	return &SessionRelay{k: k, client: client}, nil
}

func (r *SessionRelay) Client() (string, string) { return r.client, r.k.version }
func (r *SessionRelay) HasSchemas() bool         { return true }
func (r *SessionRelay) Close()                   {}

// Inventory lists each enabled server's tools. A server the relay could not
// reach is left out, and said why in the runner's log.
func (r *SessionRelay) Inventory() ([]bind.Tool, error) {
	b, err := r.k.get("/tap/tools")
	if err != nil {
		return nil, err
	}
	var got struct {
		Servers map[string][]struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"inputSchema"`
			Annotations map[string]any `json:"annotations"`
		} `json:"servers"`
		Unavailable map[string]string `json:"unavailable"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		return nil, fmt.Errorf("the session relay's tool list: %w", err)
	}
	var out []bind.Tool
	for server, tools := range got.Servers {
		if server == "tap" {
			continue
		}
		for _, t := range tools {
			out = append(out, bind.Tool{Server: server, Name: t.Name, Annotated: codexEffect(t.Annotations), Schema: t.InputSchema})
		}
	}
	return out, nil
}

// Denied applies the session's own tool switches and permission rules.
func (r *SessionRelay) Denied(t bind.Tool) (bool, error) { return r.k.Denied(t) }

// Call runs one tool through the relay's connection to the server.
func (r *SessionRelay) Call(t bind.Tool, args map[string]any) (string, error) {
	return r.k.Call(t, args)
}

// ConfiguredServers is the session's configured servers.
func (r *SessionRelay) ConfiguredServers() ([]ConfiguredServer, error) {
	return r.k.ConfiguredServers()
}

// IsNoSessionRelay says the error is that no relay was found.
func IsNoSessionRelay(err error) bool { return errors.Is(err, errNoSessionRelay) }
