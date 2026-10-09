package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Opt in explicitly because this test contacts the user's live MCP integrations.
// Read mode always declines prompts. Controlled-write mode requires an exact
// prepared plan and a separate human-created one-action decision file.
var liveClientAcceptance = flag.Bool("live-client-acceptance", false, "run the same TAP primitive through real Claude Code and Codex MCP front doors")
var liveClientAcceptanceHTTPURL = flag.String("live-client-acceptance-http-url", "", "exercise an existing HTTP TAP MCP frontend instead of local stdio")
var liveClientAcceptanceTokenFile = flag.String("live-client-acceptance-token-file", "", "file containing the HTTP frontend bearer token; never logged")
var liveClientAcceptanceServerEvidence = flag.String("live-client-acceptance-server-evidence-dir", "", "server-side directory containing journal.jsonl and runs/ for HTTP acceptance")
var liveClientAcceptanceServerName = flag.String("live-client-server-name", "tap", "Codex MCP server name to call (HTTP mode can use an isolated named entry)")
var liveClientAcceptanceWritePlan = flag.String("live-client-acceptance-write-plan", "", "opt in to a controlled write described by this prepared plan")
var liveClientAcceptanceApprovalDir = flag.String("live-client-acceptance-approval-dir", "", "private directory for real elicitation requests and explicit human decision files")

const clientAcceptancePrimitive = "discovered-255de3afb326"
const clientAcceptanceRef = "local.discover/discovered-255de3afb326@0.1.0"

func TestLiveClientAcceptanceClaudeCodeAndCodex(t *testing.T) {
	if !*liveClientAcceptance {
		t.Skip("pass -live-client-acceptance to call live Telara Jira MCP read tools")
	}
	writeMode := *liveClientAcceptanceWritePlan != ""
	var approvalExchangeDir string
	if writeMode && *liveClientAcceptanceApprovalDir == "" {
		t.Fatal("controlled write requires -live-client-acceptance-approval-dir")
	}
	if !writeMode && *liveClientAcceptanceApprovalDir != "" {
		t.Fatal("approval-dir requires -live-client-acceptance-write-plan")
	}
	httpMode := *liveClientAcceptanceHTTPURL != ""
	if writeMode && !httpMode {
		t.Fatal("controlled-write acceptance requires the configured HTTP frontend and server-side evidence")
	}
	var httpAuthorization string
	if httpMode {
		if *liveClientAcceptanceTokenFile == "" || *liveClientAcceptanceServerEvidence == "" {
			t.Fatal("HTTP mode requires -live-client-acceptance-token-file and -live-client-acceptance-server-evidence-dir")
		}
		token, err := os.ReadFile(*liveClientAcceptanceTokenFile)
		if err != nil {
			t.Fatalf("read HTTP frontend token file: %v", err)
		}
		httpAuthorization = strings.TrimSpace(string(token))
		if httpAuthorization == "" {
			t.Fatal("HTTP frontend token file is empty")
		}
		if !strings.HasPrefix(strings.ToLower(httpAuthorization), "bearer ") {
			httpAuthorization = "Bearer " + httpAuthorization
		}
	} else if *liveClientAcceptanceTokenFile != "" || *liveClientAcceptanceServerEvidence != "" {
		t.Fatal("token-file and server-evidence flags require -live-client-acceptance-http-url")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	primitive := clientAcceptancePrimitive
	refExpected := clientAcceptanceRef
	digestExpected := ""
	catalogRoot := filepath.Join(home, "Library", "Application Support", "tap", "primitives")
	if writeMode {
		var plan struct {
			Status string         `json:"status"`
			Ref    string         `json:"ref"`
			Digest string         `json:"digest"`
			Inputs map[string]any `json:"inputs"`
		}
		b, err := os.ReadFile(*liveClientAcceptanceWritePlan)
		if err != nil {
			t.Fatalf("read controlled-write plan: %v", err)
		}
		if err := json.Unmarshal(b, &plan); err != nil {
			t.Fatalf("decode controlled-write plan: %v", err)
		}
		if plan.Status != "prepared; not executed" || plan.Ref != "local.discover/discovered-get-issue-jira-telara-add-comment-e62f3f@0.1.0" || plan.Digest != "7537ab73ff8fbdb47c8b06348b35c333efe3f3e6c5f2e5e4970fa02faa10cb0f" {
			t.Fatalf("controlled-write plan identity or state changed (status=%q ref=%q digest=%q)", plan.Status, plan.Ref, plan.Digest)
		}
		if plan.Inputs["step_1_params_issue_key"] != "TENG-3159" || plan.Inputs["step_2_params_body"] != "TAP MCP cross-client controlled write verification. Test only." {
			t.Fatal("controlled-write plan inputs changed; refusing to execute")
		}
		primitive = "discovered-get-issue-jira-telara-add-comment-e62f3f"
		refExpected = plan.Ref
		digestExpected = plan.Digest
		if exchange, err := os.MkdirTemp(*liveClientAcceptanceApprovalDir, "controlled-write-"); err != nil {
			t.Fatalf("create approval exchange directory: %v", err)
		} else {
			approvalExchangeDir = exchange
			t.Logf("real elicitation requests and human decision files: %s", approvalExchangeDir)
		}
	}
	packageDir := filepath.Join(catalogRoot, primitive)
	digest, manifest, err := packageDigest(packageDir)
	if err != nil {
		t.Fatalf("read installed primitive: %v", err)
	}
	ref := manifest.Metadata.Publisher + "/" + manifest.Metadata.Name + "@" + manifest.Metadata.Version
	if ref != refExpected {
		t.Fatalf("installed primitive identity changed: %s", ref)
	}
	if digestExpected != "" && digest != digestExpected {
		t.Fatalf("installed controlled-write digest changed: got %s, want %s", digest, digestExpected)
	}
	if manifest.Interface.InputSchema == nil {
		t.Fatal("primitive has no declared input schema")
	}
	main, err := os.ReadFile(filepath.Join(packageDir, manifest.Execution.Entrypoint))
	if err != nil {
		t.Fatal(err)
	}
	if writeMode {
		if !strings.Contains(string(main), `"add_comment"`) || !strings.Contains(string(main), `"get_issue"`) {
			t.Fatal("controlled-write primitive no longer has the planned read-then-comment workflow")
		}
	} else {
		// The generated follow-up is a write to. Keep its driving list
		// empty while enabling the TENG-3159 comment-list read below.
		if !strings.Contains(string(main), `"step_4"`) || !strings.Contains(string(main), `telara_task_create_items`) {
			t.Fatal("expected task-create follow-up step is absent; inspect the current primitive before running")
		}
		if !strings.Contains(string(main), `"list_comments"`) {
			t.Fatal("expected Jira comment-read step is absent; inspect the current primitive before running")
		}
	}

	claude := filepath.Join(home, ".local", "bin", "claude")
	if _, err := os.Stat(claude); err != nil {
		t.Fatalf("native Claude Code executable is unavailable: %v", err)
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Fatalf("Codex executable is unavailable: %v", err)
	}
	nativeVersion, err := exec.Command(claude, "--version").Output()
	if err != nil {
		t.Fatalf("read native Claude Code version: %v", err)
	}
	pathClaude, pathErr := exec.LookPath("claude")
	pathVersion := "unavailable"
	if pathErr == nil {
		if b, versionErr := exec.Command(pathClaude, "--version").Output(); versionErr == nil {
			pathVersion = strings.TrimSpace(string(b))
		}
	}
	t.Logf("Claude bridge resolver selects %s (%s); PATH resolves %s (%s)", claude, strings.TrimSpace(string(nativeVersion)), pathClaude, pathVersion)

	serverName := "tap"
	if httpMode {
		serverName = *liveClientAcceptanceServerName
		if serverName == "" {
			t.Fatal("-live-client-server-name must not be empty")
		}
	}
	artifactDir, err := os.MkdirTemp("", "tap-client-acceptance-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("raw client transcripts and journals: %s", artifactDir)
	bin := ""
	if !httpMode {
		bin = filepath.Join(artifactDir, "tap")
		build := exec.Command("go", "build", "-o", bin, "./host")
		build.Dir = repoRoot
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("building TAP runner: %v\n%s", err, out)
		}
	}

	inputJSON := `{"step_1_params_issue_key":"TENG-3159","telara_jira_list_comments_items":[{}],"telara_task_create_items":[]}`
	if writeMode {
		inputJSON = `{"step_1_params_issue_key":"TENG-3159","step_2_params_body":"TAP MCP cross-client controlled write verification. Test only."}`
	}
	args := map[string]any{"ref": ref, "digest": digest, "args": []string{inputJSON}}
	for _, client := range []string{"claude", "codex"} {
		t.Run(client, func(t *testing.T) {
			clientServerName := serverName
			if client == "claude" {
				clientServerName = "tap"
			}
			work := filepath.Join(artifactDir, client)
			if err := os.MkdirAll(work, 0o700); err != nil {
				t.Fatal(err)
			}
			journalPath := filepath.Join(work, "journal.jsonl")
			configDir := filepath.Join(work, "config")
			serverArgs := []string{"serve", "--catalog-root", catalogRoot, "--config-dir", configDir,
				"--runs", filepath.Join(work, "runs"), "--journal", journalPath}
			clientLog, err := os.OpenFile(filepath.Join(work, "mcp-transcript.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer clientLog.Close()

			var caller liveAcceptanceCaller
			if client == "claude" {
				caller, err = startClaudeAcceptance(t, claude, bin, serverArgs, work, clientLog, clientServerName, *liveClientAcceptanceHTTPURL, httpAuthorization)
			} else {
				caller, err = startCodexAcceptance(t, bin, serverArgs, work, clientLog, clientServerName, httpMode)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer caller.Close()
			if writeMode {
				caller.SetApprovalExchange(approvalExchangeDir)
			}

			search, err := caller.Call("tap_search", map[string]any{"query": primitive, "detail": true})
			if err != nil {
				t.Fatalf("tap_search: %v", err)
			}
			var found struct {
				Matches []struct {
					Ref    string `json:"ref"`
					Digest string `json:"digest"`
				} `json:"matches"`
			}
			if err := json.Unmarshal([]byte(search), &found); err != nil {
				t.Fatalf("decode tap_search result %q: %v", search, err)
			}
			if len(found.Matches) != 1 || found.Matches[0].Ref != ref || found.Matches[0].Digest != digest {
				t.Fatalf("tap_search did not return the installed identity and digest: %s", search)
			}
			loaded, err := caller.Call("tap_load", map[string]any{"ref": ref, "digest": digest, "detail": true})
			if err != nil {
				t.Fatalf("tap_load: %v", err)
			}
			if writeMode {
				var declarations struct {
					Tools []struct {
						Alias  string `json:"alias"`
						Effect string `json:"effect"`
					} `json:"tools"`
				}
				if err := json.Unmarshal([]byte(loaded), &declarations); err != nil {
					t.Fatalf("decode tap_load declarations: %v", err)
				}
				effects := map[string]string{}
				for _, tool := range declarations.Tools {
					effects[tool.Alias] = tool.Effect
				}
				if effects["step_1"] != "read" || effects["step_2"] != "write" {
					t.Fatalf("tap_load did not expose the planned read/write declarations: %s", loaded)
				}
			} else if !strings.Contains(loaded, "telara_task_create") || !strings.Contains(loaded, "telara_jira_list_comments") {
				t.Fatalf("tap_load did not expose expected read and write declarations: %s", loaded)
			}
			result, err := caller.Call("tap_run", args)
			if err != nil {
				if httpMode {
					t.Fatalf("tap_run: %v (transcript: %s; HTTP backend evidence: %s)", err,
						filepath.Join(work, "mcp-transcript.jsonl"), *liveClientAcceptanceServerEvidence)
				}
				t.Fatalf("tap_run: %v (transcript: %s)", err, filepath.Join(work, "mcp-transcript.jsonl"))
			}
			if writeMode {
				var output map[string]json.RawMessage
				jsonResult := result
				if status := strings.Index(jsonResult, "\n["); status >= 0 {
					jsonResult = jsonResult[:status]
				}
				if err := json.Unmarshal([]byte(strings.TrimSpace(jsonResult)), &output); err != nil {
					t.Fatalf("decode controlled-write result %q: %v", result, err)
				}
				var issue struct {
					Key string `json:"key"`
				}
				if err := json.Unmarshal(output["step_1"], &issue); err != nil || issue.Key != "TENG-3159" {
					t.Fatalf("controlled write did not first read TENG-3159: %s", result)
				}
				var receipt map[string]any
				if err := json.Unmarshal(output["step_2"], &receipt); err != nil {
					t.Fatalf("decode add_comment receipt: %v", err)
				}
				commentID := findCommentID(receipt)
				if commentID == "" {
					t.Fatalf("add_comment result has no comment id: %s", result)
				}
				t.Logf("controlled write receipt: run result includes comment id %s; independently governed get_comment verification remains required", commentID)
				if httpMode {
					runID, err := acceptanceRunID(result)
					if err != nil {
						t.Fatalf("extract remote TAP run ID: %v", err)
					}
					if err := verifyHTTPControlledWriteAcceptance(*liveClientAcceptanceServerEvidence, runID); err != nil {
						t.Fatal(err)
					}
					t.Logf("HTTP backend write run ID=%s; server evidence=%s", runID, filepath.Join(*liveClientAcceptanceServerEvidence, "runs", runID, "index.jsonl"))
				}
				t.Logf("primitive=%s digest=%s; search/load/one run completed through %s", ref, digest, client)
				return
			}
			var readOutput struct {
				Step1 struct {
					Key string `json:"key"`
				} `json:"step_1"`
				Step3 []struct {
					Comments []json.RawMessage `json:"comments"`
				} `json:"step_3"`
			}
			jsonResult := result
			if status := strings.Index(jsonResult, "\n["); status >= 0 {
				jsonResult = jsonResult[:status]
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(jsonResult)), &readOutput); err != nil {
				t.Fatalf("decode real read output %q: %v", result, err)
			}
			if readOutput.Step1.Key != "TENG-3159" || len(readOutput.Step3) != 1 {
				t.Fatalf("run output does not prove issue and comments read: %.1200s", result)
			}
			if httpMode {
				runID, err := acceptanceRunID(result)
				if err != nil {
					t.Fatalf("extract remote TAP run ID: %v", err)
				}
				if err := verifyHTTPReadOnlyAcceptance(*liveClientAcceptanceServerEvidence, runID); err != nil {
					t.Fatal(err)
				}
				t.Logf("HTTP backend run ID=%s; server evidence=%s", runID, filepath.Join(*liveClientAcceptanceServerEvidence, "runs", runID, "index.jsonl"))
			} else if err := verifyReadOnlyAcceptanceJournal(journalPath); err != nil {
				t.Fatal(err)
			}
			t.Logf("primitive=%s digest=%s; search/load/run completed through %s; result: %.800s", ref, digest, client, result)
		})
	}
}

func verifyReadOnlyAcceptanceJournal(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4096), 2<<20)
	var rows []map[string]any
	for sc.Scan() {
		var row map[string]any
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			return fmt.Errorf("invalid journal row: %w", err)
		}
		rows = append(rows, row)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return verifyReadOnlyAcceptanceRows(rows, path)
}

func acceptanceRunID(result string) (string, error) {
	marker := "[run "
	i := strings.LastIndex(result, marker)
	if i < 0 {
		return "", errors.New("tap_run result has no run receipt")
	}
	rest := result[i+len(marker):]
	end := strings.IndexByte(rest, ']')
	if end <= 0 {
		return "", errors.New("tap_run result has a malformed run receipt")
	}
	runID := rest[:end]
	for _, r := range runID {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return "", errors.New("tap_run result has an invalid run ID")
		}
	}
	return runID, nil
}

func verifyHTTPReadOnlyAcceptance(root, runID string) error {
	runPath := filepath.Join(root, "runs", runID, "index.jsonl")
	f, err := os.Open(runPath)
	if err != nil {
		return fmt.Errorf("open HTTP backend run record: %w", err)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4096), 2<<20)
	var started, finished time.Time
	var headerRunID string
	var finishOutcome string
	for sc.Scan() {
		var row struct {
			Phase   string    `json:"phase"`
			At      time.Time `json:"at"`
			Outcome string    `json:"outcome"`
			Header  *struct {
				RunID   string    `json:"run_id"`
				Started time.Time `json:"started"`
			} `json:"header"`
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			f.Close()
			return fmt.Errorf("decode HTTP run record: %w", err)
		}
		if row.Phase == "header" && row.Header != nil {
			headerRunID, started = row.Header.RunID, row.Header.Started
		}
		if row.Phase == "finish" {
			finished, finishOutcome = row.At, row.Outcome
		}
	}
	scanErr := sc.Err()
	_ = f.Close()
	if scanErr != nil {
		return scanErr
	}
	if headerRunID != runID || started.IsZero() || finished.IsZero() || finishOutcome != "completed" {
		return fmt.Errorf("HTTP run record %s is incomplete or mismatched (header=%s, outcome=%s)", runID, headerRunID, finishOutcome)
	}

	journalPath := filepath.Join(root, "journal.jsonl")
	f, err = os.Open(journalPath)
	if err != nil {
		return fmt.Errorf("open HTTP backend action journal: %w", err)
	}
	defer f.Close()
	sc = bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4096), 2<<20)
	var actionRows []map[string]any
	for sc.Scan() {
		var row map[string]any
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			return fmt.Errorf("decode HTTP action journal: %w", err)
		}
		ts, _ := row["ts"].(string)
		at, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			continue
		}
		if !at.Before(started) && !at.After(finished) {
			actionRows = append(actionRows, row)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if len(actionRows) != 2 {
		return fmt.Errorf("HTTP server journal has %d calls during run %s, want exactly two read calls", len(actionRows), runID)
	}
	return verifyReadOnlyAcceptanceRows(actionRows, journalPath)
}

func verifyHTTPControlledWriteAcceptance(root, runID string) error {
	runPath := filepath.Join(root, "runs", runID, "index.jsonl")
	f, err := os.Open(runPath)
	if err != nil {
		return fmt.Errorf("open HTTP backend run record: %w", err)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4096), 2<<20)
	var started, finished time.Time
	var headerRunID, finishOutcome string
	var calls []map[string]any
	for sc.Scan() {
		var row map[string]any
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			f.Close()
			return fmt.Errorf("decode HTTP run record: %w", err)
		}
		phase, _ := row["phase"].(string)
		if phase == "header" {
			header, _ := row["header"].(map[string]any)
			headerRunID, _ = header["run_id"].(string)
			started, _ = time.Parse(time.RFC3339Nano, fmt.Sprint(header["started"]))
		}
		if phase == "begin" || phase == "end" {
			calls = append(calls, row)
		}
		if phase == "finish" {
			finished, _ = time.Parse(time.RFC3339Nano, fmt.Sprint(row["at"]))
			finishOutcome, _ = row["outcome"].(string)
		}
	}
	scanErr := sc.Err()
	_ = f.Close()
	if scanErr != nil {
		return scanErr
	}
	if headerRunID != runID || started.IsZero() || finished.IsZero() || finishOutcome != "completed" {
		return fmt.Errorf("HTTP run record %s is incomplete or mismatched (header=%s, outcome=%s)", runID, headerRunID, finishOutcome)
	}
	if len(calls) != 4 {
		return fmt.Errorf("HTTP run record has %d call records, want begin/end for exactly two actions", len(calls))
	}
	for i, alias := range []string{"step_1", "step_2"} {
		begin, end := calls[i*2], calls[i*2+1]
		if begin["phase"] != "begin" || end["phase"] != "end" || begin["id"] != end["id"] {
			return errors.New("HTTP run action records are not paired begin/end calls")
		}
		if outcome, _ := end["outcome"].(string); outcome != "ran" {
			return fmt.Errorf("HTTP run action %s ended with outcome %q, want ran", alias, outcome)
		}
	}

	journalPath := filepath.Join(root, "journal.jsonl")
	f, err = os.Open(journalPath)
	if err != nil {
		return fmt.Errorf("open HTTP backend action journal: %w", err)
	}
	defer f.Close()
	sc = bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4096), 2<<20)
	byAlias := map[string]map[string]any{}
	for sc.Scan() {
		var row map[string]any
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			return fmt.Errorf("decode HTTP action journal: %w", err)
		}
		ts, _ := row["ts"].(string)
		at, err := time.Parse(time.RFC3339Nano, ts)
		if err == nil && !at.Before(started) && !at.After(finished) {
			alias, _ := row["alias"].(string)
			byAlias[alias] = row
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if len(byAlias) != 2 {
		return fmt.Errorf("HTTP server journal has %d action rows, want step_1 read and step_2 write", len(byAlias))
	}
	if !journalActionIs(byAlias["step_1"], "read") || !journalActionIs(byAlias["step_2"], "write") {
		return fmt.Errorf("HTTP server journal does not prove one read and one write: step_1=%v step_2=%v", byAlias["step_1"], byAlias["step_2"])
	}
	return nil
}

func journalActionIs(row map[string]any, effect string) bool {
	if row == nil {
		return false
	}
	got, _ := row["effect"].(string)
	outcome, _ := row["outcome"].(string)
	return got == effect && outcome == "ran"
}

func findCommentID(value any) string {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			if key == "id" || key == "comment_id" {
				if id, ok := item.(string); ok && strings.TrimSpace(id) != "" {
					return id
				}
				if id, ok := item.(float64); ok && id > 0 {
					return fmt.Sprintf("%.0f", id)
				}
			}
		}
		for _, item := range v {
			if id := findCommentID(item); id != "" {
				return id
			}
		}
	case []any:
		for _, item := range v {
			if id := findCommentID(item); id != "" {
				return id
			}
		}
	}
	return ""
}

func TestHumanDecisionRequiresExplicitOneActionApproval(t *testing.T) {
	cases := []struct {
		name string
		json string
		want string
	}{
		{"explicit one action", `{"action":"accept","content":{"approve":true,"limit":1}}`, "accept"},
		{"decline", `{"action":"decline"}`, "decline"},
		{"missing limit", `{"action":"accept","content":{"approve":true}}`, "decline"},
		{"unbounded limit", `{"action":"accept","content":{"approve":true,"limit":0}}`, "decline"},
		{"malformed", `{`, "decline"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := acceptanceDecision([]byte(tc.json))["action"]
			if got != tc.want {
				t.Fatalf("decision action = %v, want %q", got, tc.want)
			}
		})
	}
	if !controlledWriteElicitation(map[string]any{"request": map[string]any{"message": "Add comment to TENG-3159"}}) {
		t.Fatal("planned Jira comment elicitation was not recognized")
	}
	if controlledWriteElicitation(map[string]any{"request": map[string]any{"message": "Allow access to TENG-3159"}}) {
		t.Fatal("unrelated elicitation was accepted as the planned write")
	}
}

func acceptanceDecision(b []byte) map[string]any {
	decline := map[string]any{"action": "decline"}
	var decision map[string]any
	if json.Unmarshal(b, &decision) != nil {
		return decline
	}
	action, _ := decision["action"].(string)
	if action == "decline" {
		return decline
	}
	content, _ := decision["content"].(map[string]any)
	approve, _ := content["approve"].(bool)
	limit, _ := content["limit"].(float64)
	if action != "accept" || !approve || limit != 1 {
		return decline
	}
	return map[string]any{"action": "accept", "content": map[string]any{"approve": true, "limit": 1}}
}

func controlledWriteElicitation(event map[string]any) bool {
	b, _ := json.Marshal(event)
	text := strings.ToLower(string(b))
	return strings.Contains(text, "teng-3159") && strings.Contains(text, "comment")
}

func verifyReadOnlyAcceptanceRows(rows []map[string]any, path string) error {
	seen := map[string]string{}
	var ran []string
	for _, row := range rows {
		alias, _ := row["alias"].(string)
		outcome, _ := row["outcome"].(string)
		dispatch, _ := row["dispatches"].(string)
		if outcome == "ran" && (alias == "step_1" || alias == "step_3") {
			seen[alias] = dispatch
		}
		if outcome == "ran" {
			ran = append(ran, alias)
		}
		tool, _ := row["tool"].(string)
		if alias == "step_4" || tool == "telara_task_create" {
			return errors.New("journal shows an unexpected write/follow-up action")
		}
		if strings.Contains(dispatch, "task_create") || strings.Contains(dispatch, "create_task") {
			return errors.New("journal shows an unexpected write/follow-up tool call")
		}
	}
	if len(rows) == 0 {
		return errors.New("TAP journal is empty; execution evidence is missing")
	}
	if !oneOf(seen["step_1"], "jira/get_issue", "telara / telara_jira_get_issue") ||
		!oneOf(seen["step_3"], "jira/list_comments", "telara / telara_jira_list_comments") {
		return fmt.Errorf("journal does not prove both real read calls (got %v); inspect %s", seen, path)
	}
	if len(ran) != 2 {
		return fmt.Errorf("journal records %d actual calls, want exactly the issue and comments reads; inspect %s", len(ran), path)
	}
	return nil
}

func oneOf(got string, values ...string) bool {
	for _, value := range values {
		if got == value {
			return true
		}
	}
	return false
}

type liveAcceptanceCaller interface {
	Call(tool string, args map[string]any) (string, error)
	SetApprovalExchange(string)
	Close()
}

type acceptanceClient struct {
	cmd         *exec.Cmd
	in          io.WriteCloser
	lines       chan map[string]any
	log         io.Writer
	client      string
	serverName  string
	approvalDir string
	seq         int
	thread      string
}

func startClaudeAcceptance(t *testing.T, claude, bin string, serverArgs []string, work string, log io.Writer, serverName, httpURL, authorization string) (*acceptanceClient, error) {
	t.Helper()
	cfg := filepath.Join(work, "mcp.json")
	var server map[string]any
	if httpURL != "" {
		server = map[string]any{"type": "http", "url": httpURL, "headers": map[string]string{"Authorization": authorization}}
	} else {
		args, _ := json.Marshal(serverArgs)
		server = map[string]any{"command": bin, "args": json.RawMessage(args)}
	}
	config := map[string]any{"mcpServers": map[string]any{serverName: server}}
	body, _ := json.Marshal(config)
	if err := os.WriteFile(cfg, body, 0o600); err != nil {
		return nil, err
	}
	cmd := exec.Command(claude, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--mcp-config", cfg, "--strict-mcp-config")
	cmd.Dir = work
	return startAcceptanceClient(cmd, "claude", serverName, log)
}

func startCodexAcceptance(t *testing.T, bin string, serverArgs []string, work string, log io.Writer, serverName string, httpMode bool) (*acceptanceClient, error) {
	t.Helper()
	var cmd *exec.Cmd
	if httpMode {
		// The caller's normal config contains the separately named private HTTP
		// entry; never place its bearer token in argv or a new environment var.
		cmd = exec.Command("codex", "app-server")
	} else {
		args, _ := json.Marshal(serverArgs)
		cmd = exec.Command("codex", "-c", fmt.Sprintf("mcp_servers.%s.command=%q", serverName, bin),
			"-c", fmt.Sprintf("mcp_servers.%s.args=%s", serverName, string(args)), "app-server")
	}
	cmd.Dir = work
	c, err := startAcceptanceClient(cmd, "codex", serverName, log)
	if err != nil {
		return nil, err
	}
	if _, err := c.request("initialize", map[string]any{"clientInfo": map[string]string{"name": "tap-acceptance", "version": "1"}}); err != nil {
		c.Close()
		return nil, err
	}
	r, err := c.request("thread/start", map[string]any{"ephemeral": true, "cwd": work})
	if err != nil {
		c.Close()
		return nil, err
	}
	thread, _ := r["thread"].(map[string]any)
	c.thread, _ = thread["id"].(string)
	if c.thread == "" {
		c.Close()
		return nil, errors.New("Codex app-server did not return a thread id")
	}
	return c, nil
}

func startAcceptanceClient(cmd *exec.Cmd, client, serverName string, log io.Writer) (*acceptanceClient, error) {
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &acceptanceClient{cmd: cmd, in: in, lines: make(chan map[string]any, 128), log: log, client: client, serverName: serverName}
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 64*1024), 64<<20)
		for sc.Scan() {
			line := append([]byte(nil), sc.Bytes()...)
			_, _ = log.Write(append(line, '\n'))
			var msg map[string]any
			if json.Unmarshal(line, &msg) == nil {
				c.lines <- msg
			}
		}
		close(c.lines)
	}()
	return c, nil
}

func (c *acceptanceClient) Call(tool string, args map[string]any) (string, error) {
	var r map[string]any
	var err error
	if c.client == "claude" {
		r, err = c.request("mcp_call", map[string]any{"tool": "mcp__" + c.serverName + "__" + tool, "arguments": args})
	} else {
		r, err = c.request("mcpServer/tool/call", map[string]any{"server": c.serverName, "tool": tool, "arguments": args, "threadId": c.thread})
	}
	if err != nil {
		return "", err
	}
	if e, _ := r["isError"].(bool); e {
		return "", fmt.Errorf("client returned MCP isError: %v", r)
	}
	return acceptanceText(r), nil
}

func (c *acceptanceClient) SetApprovalExchange(dir string) { c.approvalDir = dir }

func acceptanceText(result map[string]any) string {
	content, _ := result["content"].([]any)
	var out []string
	for _, item := range content {
		if m, ok := item.(map[string]any); ok {
			if s, ok := m["text"].(string); ok {
				out = append(out, s)
			}
		}
	}
	return strings.Join(out, "\n")
}

func (c *acceptanceClient) request(method string, params map[string]any) (map[string]any, error) {
	c.seq++
	id := c.seq
	var msg any
	if c.client == "claude" {
		fields := map[string]any{"subtype": method}
		for k, v := range params {
			fields[k] = v
		}
		msg = map[string]any{"type": "control_request", "request_id": fmt.Sprintf("tap-accept-%d", id), "request": fields}
	} else {
		msg = map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	}
	if err := c.send(msg); err != nil {
		return nil, err
	}
	requestWait := 90 * time.Second
	if c.approvalDir != "" {
		requestWait = 16 * time.Minute
	}
	deadline := time.NewTimer(requestWait)
	defer deadline.Stop()
	for {
		select {
		case next, ok := <-c.lines:
			if !ok {
				return nil, errors.New("client closed its output")
			}
			if c.client == "claude" {
				if next["type"] == "control_request" {
					request, _ := next["request"].(map[string]any)
					if request["subtype"] == "elicitation" {
						answer := c.humanDecision(next)
						_ = c.send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": next["request_id"], "response": answer}})
						continue
					}
				}
				if next["type"] != "control_response" {
					continue
				}
				response, _ := next["response"].(map[string]any)
				if response["request_id"] != fmt.Sprintf("tap-accept-%d", id) {
					continue
				}
				if response["subtype"] == "error" {
					return nil, fmt.Errorf("%s: %v", method, response["error"])
				}
				r, _ := response["response"].(map[string]any)
				return r, nil
			}
			if next["method"] == "mcpServer/elicitation/request" {
				answer := c.humanDecision(next)
				_ = c.send(map[string]any{"jsonrpc": "2.0", "id": next["id"], "result": answer})
				continue
			}
			if next["id"] == float64(id) && next["method"] == nil {
				if next["error"] != nil {
					return nil, fmt.Errorf("%s: %v", method, next["error"])
				}
				r, _ := next["result"].(map[string]any)
				return r, nil
			}
		case <-deadline.C:
			return nil, fmt.Errorf("%s: timed out waiting for the native client", method)
		}
	}
}

// humanDecision records the native client's real elicitation and waits for a
// separate, human-created response file. No response is synthesized here.
func (c *acceptanceClient) humanDecision(event map[string]any) map[string]any {
	if c.approvalDir == "" {
		return map[string]any{"action": "decline"}
	}
	if !controlledWriteElicitation(event) {
		return map[string]any{"action": "decline"}
	}
	requestPath := filepath.Join(c.approvalDir, c.client+".request.json")
	decisionPath := filepath.Join(c.approvalDir, c.client+".decision.json")
	request := map[string]any{"client": c.client, "received_at": time.Now().UTC().Format(time.RFC3339Nano), "event": event,
		"decision_file":    decisionPath,
		"allowed_decision": map[string]any{"action": "accept", "content": map[string]any{"approve": true, "limit": 1}},
		"decline_decision": map[string]any{"action": "decline"}}
	b, err := json.MarshalIndent(request, "", "  ")
	if err != nil {
		return map[string]any{"action": "decline"}
	}
	f, err := os.OpenFile(requestPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return map[string]any{"action": "decline"}
	}
	_, writeErr := f.Write(append(b, '\n'))
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		return map[string]any{"action": "decline"}
	}
	_, _ = fmt.Fprintf(os.Stderr, "TAP %s elicitation recorded at %s; awaiting a human decision file at %s\n", c.client, requestPath, decisionPath)

	deadline := time.Now().Add(15 * time.Minute)
	for time.Now().Before(deadline) {
		decisionBytes, err := os.ReadFile(decisionPath)
		if err == nil {
			return acceptanceDecision(decisionBytes)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return map[string]any{"action": "decline"}
		}
		time.Sleep(500 * time.Millisecond)
	}
	_, _ = fmt.Fprintf(os.Stderr, "TAP %s elicitation approval wait expired; declining safely\n", c.client)
	return map[string]any{"action": "decline"}
}

func (c *acceptanceClient) send(value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = c.in.Write(append(b, '\n'))
	return err
}

func (c *acceptanceClient) Close() {
	_ = c.in.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
}
