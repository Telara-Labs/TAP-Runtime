package discover

import "testing"

// The parsers read whatever agents wrote. Both crashes found on the first
// live run (a heredoc with multibyte text, a stray closing bracket in a Codex
// exec body) were parser bugs on real input, so they are fuzzed: any input
// must return without panicking. A hang shows as the run exceeding -timeout,
// which is how go test reports a fuzz input that never returns.

func FuzzSimpleCommands(f *testing.F) {
	for _, s := range []string{
		"cd /x && git status --short 2>&1 | head -3",
		"cat > f <<'EOF'\nstep → next\nEOF\nls",
		`for r in a b; do git -C "$r" diff --stat; done`,
		`eval "$(minikube docker-env)"; echo "`,
		"a <<X\nb <<-Y\n",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) { _ = simpleCommands(s) })
}

func FuzzJSToolCalls(f *testing.F) {
	for _, s := range []string{
		`const r = await tools.exec_command({cmd:"git status",workdir:"/x"}); text(r.output);`,
		`tools.mcp__telara__telara_task_list({query: 'x', n: [1, {a: 2}], ...rest})`,
		`tools.x({a: )})`,
		"tools.y({t: `a ${b} c`, 'k': \"v\\\"w\"})",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) { _ = jsToolCalls(s) })
}
