// Command tap-relay is what tap serve becomes, on systems that let a process
// replace its program: a relay of a few megabytes that passes one agent
// session to the runner every session shares. tap serve starts it the moment
// it starts, so the full program does not stay in memory for each session a
// client keeps open. It is built into tap's releases and is not run by hand.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/Telara-Labs/TAP-Runtime/internal/sharedwire"
)

func main() {
	fs := flag.NewFlagSet("tap-relay", flag.ExitOnError)
	key := fs.String("key", "", "the runner's key, from tap serve")
	runner := fs.String("runner", "", "the tap program that answers when no runner can")
	fs.Parse(os.Args[1:])
	if *key == "" || *runner == "" {
		fmt.Fprintln(os.Stderr, "tap-relay is started by tap serve, not by hand")
		os.Exit(2)
	}
	args := fs.Args()
	p, err := sharedwire.PathsFor(*key)
	if err == nil {
		dir, _ := os.Getwd()
		err = sharedwire.Relay(os.Stdin, os.Stdout, p, *runner, sharedwire.Hello{Key: *key, Dir: dir, Env: os.Environ(), Args: args})
		if err == nil {
			return
		}
	}
	if !errors.Is(err, sharedwire.ErrNoShared) && p.Sock != "" {
		sharedwire.Logf("relay      %v", err)
		os.Exit(1)
	}
	// No runner: the session is answered by tap itself, in this process.
	sharedwire.Logf("runner     answering in this process: %v", err)
	err = replace(*runner, append([]string{*runner, "serve", "--own-process"}, args...))
	sharedwire.Logf("relay      could not start %s: %v", *runner, err)
	os.Exit(1)
}
