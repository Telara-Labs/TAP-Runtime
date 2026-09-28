package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var version = "dev"

// installCommand is `host install`: it registers this runner with a client
// as an MCP server, using the client's own command to do it, so the format
// of the client's configuration is the client's business.
//
//	host install --client claude [--scope user] [--print]
//	host install --client codex [--print]
//
// It is the one setup step a person takes. With --print it changes nothing
// and shows what it would run.
func installCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	client := fs.String("client", "", "claude or codex")
	scope := fs.String("scope", "user", "for claude: local, user or project")
	print := fs.Bool("print", false, "show the command and change nothing")
	name := fs.String("name", "tap", "name the client will know the runner by")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		fmt.Fprintln(stderr, "cannot tell where this program is:", err)
		return 1
	}
	argv, err := installArgv(*client, *scope, *name, self)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if *print {
		fmt.Fprintln(stdout, strings.Join(argv, " "))
		return 0
	}
	if _, err := exec.LookPath(argv[0]); err != nil {
		fmt.Fprintf(stderr, "%s is not on this machine\n", argv[0])
		return 1
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func installArgv(client, scope, name, self string) ([]string, error) {
	switch client {
	case "claude":
		switch scope {
		case "local", "user", "project":
		default:
			return nil, fmt.Errorf("scope %q is not one Claude Code has", scope)
		}
		return []string{"claude", "mcp", "add", "--scope", scope, name, "--", self, "serve"}, nil
	case "codex":
		return []string{"codex", "mcp", "add", name, "--", self, "serve"}, nil
	case "":
		return nil, fmt.Errorf("say which client: --client claude or --client codex")
	}
	return nil, fmt.Errorf("client %q cannot lend its connections, so there is nothing to install into", client)
}
