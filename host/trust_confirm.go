package main

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strings"

	"golang.org/x/term"
)

// Testing Kilo, which cannot show TAP's prompts: when a primitive was refused,
// the agent ran tap trust from its own shell and so approved its own code.
// tap trust is the person's decision. It asks on the controlling terminal,
// which an agent's shell tool does not have, and refuses without one.

// ttyPath is the controlling terminal; a variable so tests can take it away.
var ttyPath = func() string {
	if runtime.GOOS == "windows" {
		return "CONIN$"
	}
	return "/dev/tty"
}()

// confirmTrust shows what is about to be trusted and asks the person at the
// terminal to type yes. ok is false for anything else; err says there is no
// terminal to ask on.
var confirmTrust = askAtTerminal

func askAtTerminal(summary string) (ok bool, err error) {
	// CONIN$ exists even when a process's stdin is redirected. Opening it in
	// CI or another noninteractive caller would wait for an answer that nobody
	// can provide, so require the caller's actual stdin to be a terminal first.
	if runtime.GOOS == "windows" && !term.IsTerminal(int(os.Stdin.Fd())) {
		return false, fmt.Errorf("standard input is not a terminal")
	}
	tty, err := os.OpenFile(ttyPath, os.O_RDWR, 0)
	if err != nil {
		return false, err
	}
	defer tty.Close()
	out := tty
	if runtime.GOOS == "windows" {
		if o, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
			defer o.Close()
			out = o
		}
	}
	fmt.Fprintf(out, "%s\nTrust it to run without being asked? Type yes: ", summary)
	line, _ := bufio.NewReader(tty).ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(line), "yes"), nil
}
