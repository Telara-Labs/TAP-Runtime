// Command tap is the authoring-side CLI for TAP (Trusted Agent Primitives).
// See telara-documentation/architecture/tap/04-cli.md for the command
// surface this implements.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "init":
		err = runInit(args)
	case "validate":
		err = runValidate(args)
	case "compile":
		err = runCompile(args)
	case "test":
		err = runTest(args)
	case "diff":
		err = runDiff(args)
	case "dev":
		err = runDev(args)
	case "explain":
		err = runExplain(args)
	case "publish":
		err = runPublish(args)
	case "get":
		err = runGet(args)
	case "list":
		err = runList(args)
	case "adopt":
		err = runAdopt(args)
	case "execute":
		err = runExecute(args)
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "tap: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		if ce, ok := err.(*cliError); ok {
			fmt.Fprintln(os.Stderr, ce.Error())
			os.Exit(ce.Code)
		}
		fmt.Fprintln(os.Stderr, "tap:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `tap -- authoring CLI for Trusted Agent Primitives (TAP)

Usage:
  tap init <name> [--web] [--dir <path>]     Scaffold a package
  tap validate [dir] [--json]                Lint manifest + workflow against the spec
  tap compile [dir] [--out file] [--strict]  Compile workflow.yaml to a WorkflowDefinition (JSON)
  tap test [dir] [--json] [--bind slot=origin ...] [-k <case>]
                                              Run contract tests against fixtures
  tap diff <dirA> <dirB> [--json]            Interface + permission diff
  tap dev [dir] [--bind slot=origin ...]     Fixture-replay with a step-by-step trace
  tap explain [dir] [--json]                 Render the manifest as a permission panel
  tap publish [dir] [--seal] [--json]        Publish to the TAP registry (B4); runs the 5-stage
                                              pipeline server-side. See TAP_REGISTRY_* env vars.
  tap get <publisher>/<name>[@version] [--json]
                                              Fetch a published version (GetPrimitive)
  tap list [--status <status>] [--json]      List registry entries (ListPrimitives)
  tap adopt <publisher>/<name>@<version> --bind slot=credential_id --mcp-config-id X [--json]
                                              Run the handshake (AdoptPrimitive)
  tap execute <publisher>/<name>[@version] --mcp-config-id X --idempotency-key K [--inputs-json '{...}'] [--json]
                                              Execute a primitive's compiled workflow (ExecutePrimitive)

Exit codes: 0 ok, 1 findings/failures, 2 usage error, 3 internal error.
`)
}

// cliError carries a process exit code alongside a message, so subcommands
// can signal "findings found" (1) vs "bad invocation" (2) vs "internal
// error" (3) distinctly, per the failure-taxonomy exit-code contract.
type cliError struct {
	Code int
	Msg  string
}

func (e *cliError) Error() string { return e.Msg }

func newCliError(code int, format string, a ...interface{}) *cliError {
	return &cliError{Code: code, Msg: fmt.Sprintf(format, a...)}
}
