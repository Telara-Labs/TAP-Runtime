package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/Telara-Labs/TAP-Runtime/bind"
)

// When two connected servers offer a tool that fits one capability equally
// well, the runner does not pick between them. Name alone cannot
// say which server a person trusts, and another server can offer a tool named
// like the one a primitive means. The person chooses once; the choice is kept
// on this machine, per client, and the primitive stays portable because its
// manifest says what it needs and never which server provides it.

// Pick is one choice a person is asked to make.
type Pick struct {
	Primitive  string
	Alias      string
	Capability string
	Client     string
	Servers    []string // every server whose tool fits equally well
}

// Chooser asks the person which server fills a capability. ok is false when
// there is nobody to ask or they declined.
type Chooser func(Pick) (server string, ok bool)

// bindingStore remembers which server a person chose for a capability, per
// client.
type bindingStore interface {
	get(client, capability string) string
	set(client, capability, server string) error
}

// fileBindings keeps the choices in one JSON file in the user's config
// directory: {"<client>": {"<capability>": "<server>"}}.
type fileBindings struct {
	path string
	mu   sync.Mutex
}

func defaultBindingsPath() string {
	dir, err := userConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "tap", "bindings.json")
}

func newFileBindings(path string) *fileBindings { return &fileBindings{path: path} }

func (f *fileBindings) load() map[string]map[string]string {
	all := map[string]map[string]string{}
	if f.path == "" {
		return all
	}
	if b, err := os.ReadFile(f.path); err == nil {
		json.Unmarshal(b, &all)
	}
	return all
}

func (f *fileBindings) get(client, capability string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.load()[client][capability]
}

func (f *fileBindings) set(client, capability, server string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.path == "" {
		return errors.New("this machine has no user config directory to keep the choice in")
	}
	all := f.load()
	if all[client] == nil {
		all[client] = map[string]string{}
	}
	if server == "" {
		delete(all[client], capability)
		if len(all[client]) == 0 {
			delete(all, client)
		}
	} else {
		all[client][capability] = server
	}
	b, _ := json.MarshalIndent(all, "", "  ")
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(f.path, append(b, '\n'), 0o600)
}

// tiedServers returns the distinct servers whose tools score exactly what the
// best candidate scores, best candidate's server first. Candidates arrive best
// first. A single server has no ambiguity to resolve.
func tiedServers(ranked []bind.Candidate) []string {
	if len(ranked) == 0 {
		return nil
	}
	top := ranked[0].Score
	seen := map[string]bool{}
	var servers []string
	for _, c := range ranked {
		if c.Score != top || seen[c.Tool.Server] {
			continue
		}
		seen[c.Tool.Server] = true
		servers = append(servers, c.Tool.Server)
	}
	return servers
}

func hasServer(inv []bind.Tool, server string) bool {
	for _, t := range inv {
		if t.Server == server {
			return true
		}
	}
	return false
}

// renamedPinServer finds the server that offers a pinned tool when the
// pinned server name is not connected here: the same MCP server is often
// connected under another name on another machine ("telara" in a local
// config, "claude.ai Telara" as a claude.ai connector). A Telara pin is also
// served by a server offering Telara's verified dispatcher. One such server
// binds; several are settled once per pinned server name, like any tie.
func renamedPinServer(d toolDecl, inv []bind.Tool, store bindingStore, choose Chooser, client string) (server, refusal string, none bool) {
	_, _, telaraPin := telaraActionFromPin(d.Pin.Server, d.Pin.Tool)
	var servers []string
	seen := map[string]bool{}
	for _, t := range inv {
		if seen[t.Server] {
			continue
		}
		_, dispatcher := findTool(inv, t.Server, "telara_execute_action")
		_, catalog := findTool(inv, t.Server, "telara_tool_search")
		if _, direct := findTool(inv, t.Server, d.Pin.Tool); direct || (telaraPin && dispatcher && catalog) {
			seen[t.Server] = true
			servers = append(servers, t.Server)
		}
	}
	switch len(servers) {
	case 0:
		return "", fmt.Sprintf("the pinned tool %s / %s is not on this client and no connected server offers %s; connect the MCP server that provides it to %s",
			d.Pin.Server, d.Pin.Tool, d.Pin.Tool, client), true
	case 1:
		return servers[0], "", false
	}
	sort.Strings(servers)
	server, refusal = settle(Pick{Alias: d.Alias, Capability: "server:" + d.Pin.Server, Client: client, Servers: servers}, store, choose)
	return server, refusal, false
}

// settle decides between servers that fit equally well. It returns the server
// to use, or a refusal that tells the person how to choose.
func settle(p Pick, store bindingStore, choose Chooser) (server, refusal string) {
	if store != nil {
		if saved := store.get(p.Client, p.Capability); saved != "" {
			for _, s := range p.Servers {
				if s == saved {
					return saved, ""
				}
			}
			// The server they chose is not offering a match now. Ask again.
		}
	}
	if choose != nil {
		if s, ok := choose(p); ok {
			for _, have := range p.Servers {
				if have == s {
					if store != nil {
						if err := store.set(p.Client, p.Capability, s); err != nil {
							logf("binding    could not keep the choice of %s for %s: %v", s, p.Capability, err)
						}
					}
					return s, ""
				}
			}
		}
	}
	names := make([]string, len(p.Servers))
	for i, s := range p.Servers {
		names[i] = fmt.Sprintf("%q", s)
	}
	return "", fmt.Sprintf("%d connected servers offer a tool that fits %s equally well (%s) and the runner does not choose between them. Choose one: tap bind --client %s %s %q",
		len(p.Servers), p.Capability, strings.Join(names, ", "), p.Client, p.Capability, p.Servers[0])
}

// bindCommand is `tap bind`: choose, show or forget which server fills a
// capability on a client.
func bindCommand(args []string, stdout, stderr io.Writer) int {
	store := newFileBindings(defaultBindingsPath())
	client := ""
	var rest []string
	list, forget := false, false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--client":
			if i+1 >= len(args) {
				fmt.Fprintln(stderr, "tap bind: --client needs a name")
				return 2
			}
			client = args[i+1]
			i++
		case "--list":
			list = true
		case "--forget":
			forget = true
		default:
			rest = append(rest, args[i])
		}
	}
	if list {
		all := store.load()
		var clients []string
		for c := range all {
			clients = append(clients, c)
		}
		sort.Strings(clients)
		if len(clients) == 0 {
			fmt.Fprintln(stdout, "no choices are kept")
		}
		for _, c := range clients {
			var caps []string
			for k := range all[c] {
				caps = append(caps, k)
			}
			sort.Strings(caps)
			for _, k := range caps {
				fmt.Fprintf(stdout, "%s\t%s\t%s\n", c, k, all[c][k])
			}
		}
		return 0
	}
	if client == "" || (forget && len(rest) != 1) || (!forget && len(rest) != 2) {
		fmt.Fprintln(stderr, "usage: tap bind --client NAME CAPABILITY SERVER\n       tap bind --client NAME --forget CAPABILITY\n       tap bind --list")
		return 2
	}
	server := ""
	if !forget {
		server = rest[1]
	}
	if err := store.set(client, rest[0], server); err != nil {
		fmt.Fprintln(stderr, "tap bind:", err)
		return 1
	}
	if forget {
		fmt.Fprintf(stdout, "forgot the choice for %s on %s\n", rest[0], client)
	} else {
		fmt.Fprintf(stdout, "%s on %s now uses %q\n", rest[0], client, server)
	}
	return 0
}
