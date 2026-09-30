package discover

import (
	"strings"
	"testing"
)

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

func FuzzWordSpans(f *testing.F) {
	for _, s := range []string{
		`cd "dir x" && go test ./... 2>&1 | tail -20`,
		"python3 - <<'PY'\nprint(1)\nPY\nls",
		`echo $(date) 'a' "b\"c" \x`,
		"a <<X",
		`'unterminated`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, w := range wordSpans(s) {
			if w.s < 0 || w.e > len(s) || w.s > w.e {
				t.Fatalf("span %+v out of range for %q", w, s)
			}
		}
		_ = shapeOf(s, wordSpans(s))
	})
}

func FuzzOutputRefs(f *testing.F) {
	for _, seed := range []string{"Task ID: `90991e90-de01-4847-a933-187b18ef2985`", `{"id":"18c0000000000abc9"}`, "é\nTENG-1 x", "\xff\xfeABC-12"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		ids, ctx := outputRefs(text)
		if len(ids) != len(ctx) {
			t.Fatalf("%d ids, %d contexts", len(ids), len(ctx))
		}
		for k, c := range ctx {
			before, _, ok := strings.Cut(c, "\x00")
			if !ok || strings.Contains(before, "\n") || len(before) > 32 {
				t.Fatalf("context %q for %q", c, ids[k])
			}
		}
	})
}
