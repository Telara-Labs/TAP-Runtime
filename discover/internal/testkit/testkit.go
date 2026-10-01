// Package testkit holds the corpus builders and fakes the discover packages' tests share.
package testkit

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/model"
	"gitlab.com/telara-labs/tap-runtime/discover/retrieval"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// Episodes builds n sessions named prefix00.., each one request, four days apart.
// Calls without a recorded outcome are marked ok.
func Episodes(prefix string, n int, text func(i int) string, calls func(i int) []trace.Call) []trace.Session {
	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	var out []trace.Session
	for i := 0; i < n; i++ {
		s := trace.Session{Client: "fake", ID: fmt.Sprintf("%s%02d", prefix, i), Start: t0.AddDate(0, 0, 4*i)}
		s.AddRequest(text(i))
		for _, c := range calls(i) {
			c.Request, c.Time = 0, s.Start
			if c.Outcome == trace.OutcomeUnknown {
				c.Outcome = trace.OutcomeOK
			}
			s.Calls = append(s.Calls, c)
		}
		out = append(out, s)
	}
	return out
}

// Explore is an open-ended investigation: each run reads and searches
// places the agent chose as it went.
func Explore(prefix string, n int) []trace.Session {
	svc := []string{"billing", "gateway", "search", "tenants", "storage", "knowledge", "scheduler", "vault", "agents"}
	return Episodes(prefix, n, func(i int) string { return fmt.Sprintf("why does the %s service fail on startup?", svc[i%len(svc)]) }, func(i int) []trace.Call {
		return []trace.Call{
			{Tool: "Read", Args: map[string]string{"file_path": fmt.Sprintf("src/mod%d/init_%d.go", i*7, i)}},
			ShellCall(fmt.Sprintf("rg -n handler%d src/mod%d", i*3, i*5)),
			{Tool: "Read", Args: map[string]string{"file_path": fmt.Sprintf("src/mod%d/config_%d.go", i*11, i)}},
			ShellCall(fmt.Sprintf("rg -n retry%d internal/x%d", i*13, i)),
		}
	})
}

// RealJQ is the host's jq, used by the stand-ins above.
func RealJQ(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq not available")
	}
	return p
}

func Contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func WriteFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func CompositionNode(ordinal int, action string, args map[string]string, inputs ...model.SpanInput) retrieval.SpanNode {
	if args == nil {
		args = map[string]string{}
	}
	args["integration"] = "jira"
	args["action"] = action
	var slots []trace.Slot
	for key, value := range args {
		slots = append(slots, trace.Slot{Key: key, Value: value, Type: trace.SlotText})
	}
	return retrieval.SpanNode{Ordinal: ordinal, Call: trace.Call{Tool: "mcp:telara_execute_action", Args: args}, Steps: []trace.Step{{Label: "mcp:telara_execute_action", Slots: slots}}, Inputs: inputs}
}

// ToolCorpus: 20 sessions pick up a ticket through two MCP tools, passing the
// same ticket id to both, among random noise.
func ToolCorpus() []trace.Session {
	var out []trace.Session
	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	noise := []string{"ls", "pwd", "date", "whoami", "uptime", "df -h", "id", "hostname"}
	for i := 0; i < 60; i++ {
		s := trace.Session{Client: "fake", ID: fmt.Sprintf("t%02d", i), Start: t0.AddDate(0, 0, 7*(i%20))}
		for j := 0; j < 20; j++ {
			s.Calls = append(s.Calls, trace.Call{Tool: "shell", Command: noise[(i*7+j*3)%len(noise)] + fmt.Sprintf(" %d", j)})
			if i < 20 && j == 8 {
				id := fmt.Sprintf("TENG-%d", 3000+i)
				s.Calls = append(s.Calls,
					trace.Call{Tool: "mcp:telara_task_create", Args: map[string]string{"goal": fmt.Sprintf("work on %s part %d", id, i), "ticket": id}},
					trace.Call{Tool: "mcp:telara_jira_transition_issue", Args: map[string]string{"issue_key": id, "transition_id": "11"}})
			}
		}
		out = append(out, s)
	}
	return out
}

// RequestSessions builds n sessions, each one request running the calls
// make(i) returns, a few days apart.
func RequestSessions(n int, text func(i int) string, calls func(i int) []trace.Call) []trace.Session {
	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	var out []trace.Session
	for i := 0; i < n; i++ {
		s := trace.Session{Client: "fake", ID: fmt.Sprintf("f%02d", i), Start: t0.AddDate(0, 0, 4*i)}
		s.AddRequest(text(i))
		for _, c := range calls(i) {
			c.Request, c.Time = 0, s.Start
			s.Calls = append(s.Calls, c)
		}
		out = append(out, s)
	}
	return out
}

func ShellCall(cmd string) trace.Call { return trace.Call{Tool: "shell", Command: cmd} }

// FakeReader serves synthetic sessions as if they were a client's history.
type FakeReader struct {
	Sessions []trace.Session
	Name     string
}

func (f FakeReader) Client() string {
	if f.Name == "" {
		return "fake"
	}
	return f.Name
}

func (f FakeReader) Read(time.Time) ([]trace.Session, error) { return f.Sessions, nil }

// PlantedCorpus has 60 sessions of random tool calls. Twenty of them also
// run one procedure (build, test, push with a changing branch name) and load
// the "ship" skill. A procedure that is really there must qualify; random
// co-occurrence must not.
func PlantedCorpus() []trace.Session {
	rng := rand.New(rand.NewSource(42))
	noise := []string{"ls", "cat a", "grep x y", "head -3", "wc -l", "tail -5", "sed -n 1p", "find .", "du -sh", "pwd", "whoami", "date"}
	var out []trace.Session
	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 60; i++ {
		s := trace.Session{Client: "fake", ID: fmt.Sprintf("s%02d", i), Start: t0.AddDate(0, 0, 7*(i%20))}
		call := func(cmd string) {
			s.Calls = append(s.Calls, trace.Call{Client: "fake", Session: s.ID, Tool: "shell", Command: cmd, Time: s.Start})
		}
		for j := 0; j < 25; j++ {
			call(noise[rng.Intn(len(noise))] + fmt.Sprintf(" %d", rng.Intn(1000)))
			if i < 20 && j == 10 {
				s.Calls = append(s.Calls, trace.Call{Client: "fake", Session: s.ID, Tool: "Skill", Args: map[string]string{"skill": "ship"}})
				call("make build")
				call("go test ./... -count=1")
				call(fmt.Sprintf("git push origin feature-%d", i))
			}
		}
		out = append(out, s)
	}
	return out
}

func FileReplaceSession(id, file, old, new string) trace.Session {
	body := "path = '" + file + "'\ntext = open(path).read()\nchanged = text.replace('" + old + "', '" + new + "')\nopen(path, 'w').write(changed)\n"
	return NewSession(id, "Replace text in a file", trace.Call{Tool: "shell", Command: "cd project && python3 - <<'PY'\n" + body + "PY\necho done", Outcome: trace.OutcomeOK})
}

// Synthetic credentials only. None of these is real.
const (
	fakeBearer = "Bearer abcDEF1234567890ghiJKL"
	fakeGitlab = "glpat-AbCdEfGhIjKlMnOpQrSt"
	fakeJWT    = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
	fakeKey    = "AKIAABCDEFGHIJKLMNOP"
)

// RequestCorpus: 30 sessions over 10 weeks. Each carries a few requests:
//   - "move TENG-<n> to done": transition then comment on that ticket (the
//     ticket id is in the request: a primitive)
//   - "deploy the gateway": set image to a tag the agent chose, then watch
//     the rollout (the tag is never in the request; the agent chooses it and a primitive takes it as an input)
//   - noise requests of random reads.
func RequestCorpus() []trace.Session {
	rng := rand.New(rand.NewSource(21))
	noise := []string{"ls", "pwd", "date", "uptime", "hostname", "id", "df -h", "du -sh ."}
	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	var out []trace.Session
	for i := 0; i < 30; i++ {
		s := trace.Session{Client: "fake", ID: fmt.Sprintf("r%02d", i), Start: t0.AddDate(0, 0, 2*i)}
		call := func(c trace.Call) {
			c.Request = len(s.Requests) - 1
			c.Time = s.Start
			s.Calls = append(s.Calls, c)
		}
		s.AddRequest("look around the repo")
		for j := 0; j < 4; j++ {
			call(trace.Call{Tool: "shell", Command: noise[rng.Intn(len(noise))] + fmt.Sprint(" ", j)})
		}
		if i%3 != 2 {
			ticket := fmt.Sprintf("TENG-%d", 3100+i)
			s.AddRequest("please move " + ticket + " to done and say it shipped")
			call(trace.Call{Tool: "mcp:telara_jira_transition_issue", Args: map[string]string{"issue_key": ticket, "transition_id": "21"}})
			call(trace.Call{Tool: "mcp:telara_jira_add_comment", Args: map[string]string{"issue_key": ticket, "body": "shipped"}})
		}
		if i%3 != 0 {
			s.AddRequest("deploy the gateway to minikube")
			tag := fmt.Sprintf("teng%d-v%d", 3000+i, rng.Intn(9))
			call(trace.Call{Tool: "shell", Command: "kubectl --context minikube -n telara-middleware set image deploy/gateway gateway=telara/gateway:" + tag})
			call(trace.Call{Tool: "shell", Command: "kubectl --context minikube -n telara-middleware rollout status deploy/gateway"})
		}
		out = append(out, s)
	}
	return out
}

// Synthetic credentials only. None of these is real.
const (
	FakeBearer = "Bearer abcDEF1234567890ghiJKL"
	FakeGitlab = "glpat-AbCdEfGhIjKlMnOpQrSt"
	FakeJWT    = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
	FakeKey    = "AKIAABCDEFGHIJKLMNOP"
)

func CredCorpus() []trace.Session {
	rng := rand.New(rand.NewSource(4))
	noise := []string{"ls", "pwd", "date", "id", "uptime"}
	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	var out []trace.Session
	for i := 0; i < 12; i++ {
		s := trace.Session{Client: "fake", ID: fmt.Sprintf("c%02d", i), Start: t0.AddDate(0, 0, 5*i)}
		s.AddRequest(fmt.Sprintf("sync project %d", i))
		add := func(c trace.Call) {
			c.Request, c.Time = 0, s.Start
			s.Calls = append(s.Calls, c)
		}
		add(trace.Call{Tool: "shell", Command: noise[rng.Intn(len(noise))]})
		add(trace.Call{Tool: "shell", Command: fmt.Sprintf("curl -s -H 'Authorization: %s' https://api.example.com/projects/%d", FakeBearer, i)})
		add(trace.Call{Tool: "mcp:vault_write", Args: map[string]string{"path": "secret/app", "token": FakeGitlab, "data": fmt.Sprintf(`{"db_password": "pw-%d-abcdefgh"}`, i)}, RawArgs: map[string]bool{"data": true}})
		add(trace.Call{Tool: "shell", Command: fmt.Sprintf("git clone https://deploy:hunter2secret@git.example.com/p%d.git", i)})
		out = append(out, s)
	}
	return out
}

var T0 = time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)

func NewSession(id, req string, calls ...trace.Call) trace.Session {
	for i := range calls {
		calls[i].Time = T0.Add(time.Duration(i) * time.Second)
		if calls[i].Outcome == trace.OutcomeUnknown {
			calls[i].Outcome = trace.OutcomeOK
		}
	}
	return trace.Session{Client: "codex", ID: id, Start: T0, Requests: []string{req}, Calls: calls}
}

const StatedPrompt = `Automation: queue monitor. Run %d.
- Read /work/tracker/queue.json and count rows by status.
- Append the pass to /work/tracker/log.jsonl.
- Check that log.jsonl parses.`

func SpanWithCalls(ps []model.SpanProposal, want ...int) *model.SpanProposal {
	for i := range ps {
		if reflect.DeepEqual(ps[i].Calls, want) {
			return &ps[i]
		}
	}
	return nil
}

func SpanRefs(c trace.Call) trace.Call {
	c.OutIDs, c.OutCtx, c.OutPaths = trace.OutputRefsPaths(c.Output)
	return c
}
