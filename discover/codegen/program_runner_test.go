package codegen_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/discover/codegen"
)

// This exercises a locally generated package through the shipped TAP host,
// including MCP admission and result flow. The fake server is the external
// MCP provider; no Telara service is mocked or contacted.
func TestGeneratedPackageRunsThroughHostWithFreshInputs(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the TAP host")
	}
	g := &codegen.ProgramGraph{
		CandidateID: "lc_host",
		Inputs: []codegen.ProgramInput{
			{Name: "name", Type: "string", Source: "caller"},
			{Name: "priority", Type: "string", Optional: true, Source: "caller"},
			{Name: "targets", Type: "string", List: true, Source: "caller"},
		},
		Steps: []codegen.ProgramStep{
			{Role: "create", Tool: "mcp:create", Binding: &codegen.ProgramToolBinding{Server: "test", Tool: "create"}, Effect: "write",
				OptionalProfiles: [][]string{{}, {"priority"}},
				Args: []codegen.ProgramArg{
					{Path: []string{"name"}, Value: codegen.ProgramValue{Kind: "input", Input: "name"}},
					{Path: []string{"priority"}, Optional: true, Value: codegen.ProgramValue{Kind: "input", Input: "priority"}},
				}},
			{Role: "link", Tool: "mcp:link", Binding: &codegen.ProgramToolBinding{Server: "test", Tool: "link"}, Effect: "write", Loop: "targets",
				Args: []codegen.ProgramArg{
					{Path: []string{"root"}, Value: codegen.ProgramValue{Kind: "result", Step: 1, ResultPath: ".id"}},
					{Path: []string{"target"}, Value: codegen.ProgramValue{Kind: "item", Input: "targets"}},
				}},
		},
	}
	p, err := codegen.GenerateProgramPackage(g)
	if err != nil {
		t.Fatal(err)
	}
	pkg := t.TempDir()
	for name, data := range p.Files {
		if err := os.WriteFile(filepath.Join(pkg, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var calls []struct {
		Name string
		Args map[string]any
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)
		if msg.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{"serverInfo": map[string]any{"name": "test", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{
				map[string]any{"name": "create"},
				map[string]any{"name": "link"},
			}}
		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(msg.Params, &params)
			mu.Lock()
			calls = append(calls, struct {
				Name string
				Args map[string]any
			}{params.Name, params.Arguments})
			mu.Unlock()
			text := `{"linked":true}`
			if params.Name == "create" {
				text = `{"id":"NEW-1"}`
			}
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
		default:
			http.Error(w, "unexpected method", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": result})
	}))
	defer server.Close()
	bin := filepath.Join(t.TempDir(), "tap")
	build := exec.Command("go", "build", "-o", bin, "./host")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build TAP host: %v\n%s", err, output)
	}
	run := exec.Command(bin, "--approve", "--mcp-url", server.URL, "--runs", t.TempDir(),
		pkg, `{"name":"fresh","priority":"high","targets":["A","B"]}`)
	output, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("generated package failed through host: %v\n%s", err, output)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 3 || calls[0].Name != "create" || calls[1].Name != "link" || calls[2].Name != "link" {
		t.Fatalf("wrong MCP call order: %+v\n%s", calls, output)
	}
	if calls[0].Args["name"] != "fresh" || calls[0].Args["priority"] != "high" {
		t.Fatalf("optional create arguments lost through TAP: %+v", calls[0].Args)
	}
	for i, want := range []string{"A", "B"} {
		args := calls[i+1].Args
		if args["root"] != "NEW-1" || args["target"] != want {
			t.Fatalf("link %d arguments: %+v\n%s", i, args, output)
		}
	}
	pos := bytes.LastIndex(output, []byte("RESULT (exit 0)\n"))
	if pos < 0 {
		t.Fatalf("missing successful runner result: %s", output)
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(output[pos+len("RESULT (exit 0)\n"):]), &result); err != nil {
		t.Fatalf("invalid runner result: %v\n%s", err, output)
	}
	var links []map[string]bool
	if err := json.Unmarshal(result["step_2"], &links); err != nil || len(links) != 2 || !links[0]["linked"] || !links[1]["linked"] {
		t.Fatalf("missing output from generated loop: %+v %v\n%s", links, err, output)
	}
}

func TestGeneratedCollectionSubsetRunsThroughHost(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the TAP host")
	}
	g := &codegen.ProgramGraph{CandidateID: "lc_hostsubset", Inputs: []codegen.ProgramInput{{Name: "positions", Type: "integer", List: true, Source: "caller"}}, Steps: []codegen.ProgramStep{
		{Role: "list", Tool: "mcp:list", Binding: &codegen.ProgramToolBinding{Server: "test", Tool: "list"}, Effect: "read"},
		{Role: "act", Tool: "mcp:act", Binding: &codegen.ProgramToolBinding{Server: "test", Tool: "act"}, Effect: "write", Loop: "positions", DistinctLoopSelections: true,
			Args: []codegen.ProgramArg{{Path: []string{"id"}, Value: codegen.ProgramValue{Kind: "collection_index_item", Step: 1, CollectionPath: ".items", ResultPath: ".id", Input: "positions"}}}},
	}}
	p, err := codegen.GenerateProgramPackage(g)
	if err != nil {
		t.Fatal(err)
	}
	pkg := t.TempDir()
	for name, data := range p.Files {
		if err := os.WriteFile(filepath.Join(pkg, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var calls []struct {
		Name string
		ID   string
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)
		if msg.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{"serverInfo": map[string]any{"name": "test", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "list"}, map[string]any{"name": "act"}}}
		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(msg.Params, &params)
			id, _ := params.Arguments["id"].(string)
			mu.Lock()
			calls = append(calls, struct {
				Name string
				ID   string
			}{params.Name, id})
			mu.Unlock()
			text := `{"ok":true}`
			if params.Name == "list" {
				text = `{"items":[{"id":"FRESH-A"},{"id":"FRESH-B"},{"id":"FRESH-C"}]}`
			}
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
		default:
			http.Error(w, "unexpected method", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": result})
	}))
	defer server.Close()
	bin := filepath.Join(t.TempDir(), "tap")
	build := exec.Command("go", "build", "-o", bin, "./host")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build TAP host: %v\n%s", err, output)
	}
	run := func(input string) ([]byte, error) {
		cmd := exec.Command(bin, "--approve", "--mcp-url", server.URL, "--runs", t.TempDir(), pkg, input)
		return cmd.CombinedOutput()
	}
	output, err := run(`{"positions":[2,0]}`)
	if err != nil || !bytes.Contains(output, []byte("RESULT (exit 0)")) {
		t.Fatalf("fresh subset failed through host: %v\n%s", err, output)
	}
	mu.Lock()
	got := append([]struct {
		Name string
		ID   string
	}(nil), calls...)
	calls = nil
	mu.Unlock()
	if len(got) != 3 || got[0].Name != "list" || got[1].Name != "act" || got[1].ID != "FRESH-C" || got[2].Name != "act" || got[2].ID != "FRESH-A" {
		t.Fatalf("wrong fresh selection or order: %+v\n%s", got, output)
	}
	output, _ = run(`{"positions":[0,4]}`)
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0].Name != "list" {
		t.Fatalf("out-of-range index made downstream effects: %+v\n%s", calls, output)
	}
}

func TestGeneratedRepeatedProducerJoinRunsThroughHost(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the TAP host")
	}
	g := &codegen.ProgramGraph{CandidateID: "lc_hostjoin", Executions: 2, Sessions: 2,
		Inputs: []codegen.ProgramInput{{Name: "records", Type: "object", List: true, Source: "caller", Fields: []codegen.ProgramInputField{
			{Name: "source_id", Path: []string{"source_id"}, Type: "string"},
			{Name: "name", Path: []string{"name"}, Type: "string"},
		}}},
		Steps: []codegen.ProgramStep{
			{Role: "create", Tool: "mcp:create", Binding: &codegen.ProgramToolBinding{Server: "test", Tool: "create"}, Effect: "write", Loop: "records", Args: []codegen.ProgramArg{
				{Path: []string{"source_id"}, Value: codegen.ProgramValue{Kind: "item", Input: "records", ResultPath: ".source_id"}},
				{Path: []string{"name"}, Value: codegen.ProgramValue{Kind: "item", Input: "records", ResultPath: ".name"}},
			}},
			{Role: "transition", Tool: "mcp:transition", Binding: &codegen.ProgramToolBinding{Server: "test", Tool: "transition"}, Effect: "write", LoopResultStep: 1, Args: []codegen.ProgramArg{
				{Path: []string{"record_id"}, Value: codegen.ProgramValue{Kind: "item_result", Step: 1, ResultPath: ".id"}},
			}},
		}}
	p, err := codegen.GenerateProgramPackage(g)
	if err != nil {
		t.Fatal(err)
	}
	pkg := t.TempDir()
	for name, data := range p.Files {
		if err := os.WriteFile(filepath.Join(pkg, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	type invocation struct {
		Name string
		Args map[string]any
	}
	var mu sync.Mutex
	var calls []invocation
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)
		if msg.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{"serverInfo": map[string]any{"name": "test", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "create"}, map[string]any{"name": "transition"}}}
		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(msg.Params, &params)
			mu.Lock()
			calls = append(calls, invocation{params.Name, params.Arguments})
			mu.Unlock()
			body := `{"updated":true}`
			if params.Name == "create" {
				source, _ := params.Arguments["source_id"].(string)
				body = `{"id":"NEW-` + source + `"}`
			}
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": body}}}
		default:
			http.Error(w, "unexpected method", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": result})
	}))
	defer server.Close()
	bin := filepath.Join(t.TempDir(), "tap")
	build := exec.Command("go", "build", "-o", bin, "./host")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build TAP host: %v\n%s", err, output)
	}
	run := exec.Command(bin, "--approve", "--mcp-url", server.URL, "--runs", t.TempDir(), pkg,
		`{"records":[{"source_id":"A","name":"alpha"},{"source_id":"B","name":"beta"}]}`)
	output, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("generated join failed through host: %v\n%s", err, output)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 4 {
		t.Fatalf("want two creates then two transitions: %+v\n%s", calls, output)
	}
	for i, want := range []string{"A", "B"} {
		if calls[i].Name != "create" || calls[i].Args["source_id"] != want {
			t.Fatalf("create %d: %+v", i, calls[i])
		}
	}
	for i, want := range []string{"NEW-A", "NEW-B"} {
		if calls[i+2].Name != "transition" || calls[i+2].Args["record_id"] != want {
			t.Fatalf("transition %d: %+v", i, calls[i+2])
		}
	}
	if !bytes.Contains(output, []byte("RESULT (exit 0)")) {
		t.Fatalf("missing successful runner result: %s", output)
	}
}

func TestGeneratedLoopResultRolesRunThroughHost(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the TAP host")
	}
	g := &codegen.ProgramGraph{CandidateID: "lc_rolejoin", Inputs: []codegen.ProgramInput{
		{Name: "records", Type: "object", List: true, Source: "caller", Fields: []codegen.ProgramInputField{{Name: "name", Path: []string{"name"}, Type: "string"}}},
		{Name: "inward_index", Type: "integer", Source: "caller chooses result role"},
		{Name: "outward_index", Type: "integer", Source: "caller chooses result role"},
	}, Steps: []codegen.ProgramStep{
		{Role: "create", Tool: "mcp:create", Binding: &codegen.ProgramToolBinding{Server: "test", Tool: "create"}, Effect: "write", Loop: "records", Args: []codegen.ProgramArg{
			{Path: []string{"name"}, Value: codegen.ProgramValue{Kind: "item", Input: "records", ResultPath: ".name"}},
		}},
		{Role: "link", Tool: "mcp:link", Binding: &codegen.ProgramToolBinding{Server: "test", Tool: "link"}, Effect: "write", Args: []codegen.ProgramArg{
			{Path: []string{"inward_id"}, Value: codegen.ProgramValue{Kind: "indexed_result", Step: 1, ResultPath: ".id", Input: "inward_index"}},
			{Path: []string{"outward_id"}, Value: codegen.ProgramValue{Kind: "indexed_result", Step: 1, ResultPath: ".id", Input: "outward_index"}},
		}, DistinctResultInputs: [][]string{{"inward_index", "outward_index"}}},
	}}
	p, err := codegen.GenerateProgramPackage(g)
	if err != nil {
		t.Fatal(err)
	}
	pkg := t.TempDir()
	for name, data := range p.Files {
		if err := os.WriteFile(filepath.Join(pkg, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	type invocation struct {
		Name string
		Args map[string]any
	}
	var mu sync.Mutex
	var calls []invocation
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)
		if msg.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{"serverInfo": map[string]any{"name": "test", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "create"}, map[string]any{"name": "link"}}}
		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(msg.Params, &params)
			mu.Lock()
			calls = append(calls, invocation{params.Name, params.Arguments})
			mu.Unlock()
			body := `{"linked":true}`
			if params.Name == "create" {
				body = `{"id":"NEW-` + params.Arguments["name"].(string) + `"}`
			}
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": body}}}
		default:
			http.Error(w, "unexpected method", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": result})
	}))
	defer server.Close()
	bin := filepath.Join(t.TempDir(), "tap")
	build := exec.Command("go", "build", "-o", bin, "./host")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build TAP host: %v\n%s", err, output)
	}
	input := map[string]any{"records": []map[string]string{{"name": "A"}, {"name": "B"}, {"name": "C"}}, "inward_index": 2, "outward_index": 0}
	encoded, _ := json.Marshal(input)
	run := exec.Command(bin, "--approve", "--mcp-url", server.URL, "--runs", t.TempDir(), pkg, string(encoded))
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("role-selected package failed: %v\n%s", err, output)
	}
	mu.Lock()
	if len(calls) != 4 || calls[3].Name != "link" || calls[3].Args["inward_id"] != "NEW-C" || calls[3].Args["outward_id"] != "NEW-A" {
		t.Fatalf("fresh result roles were not preserved: %+v", calls)
	}
	mu.Unlock()
	for _, indexes := range [][2]int{{1, 1}, {3, 0}} {
		input["inward_index"], input["outward_index"] = indexes[0], indexes[1]
		encoded, _ = json.Marshal(input)
		run = exec.Command(bin, "--approve", "--mcp-url", server.URL, "--runs", t.TempDir(), pkg, string(encoded))
		if output, err := run.CombinedOutput(); err == nil {
			t.Fatalf("invalid result selection ran: %s", output)
		}
		mu.Lock()
		if len(calls) != 4 {
			t.Fatalf("invalid result selection reached tools: %+v", calls)
		}
		mu.Unlock()
	}
}

func TestGeneratedPipelineRunsThroughHostWithFreshFile(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the TAP host")
	}
	for _, name := range []string{"cat", "grep"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s unavailable", name)
		}
	}
	g := &codegen.ProgramGraph{CandidateID: "lc_hostpipe", Inputs: []codegen.ProgramInput{
		{Name: "path", Type: "string", Source: "supplied at invocation"},
		{Name: "pattern", Type: "string", Source: "supplied at invocation"},
	}, Steps: []codegen.ProgramStep{{Role: "filter file", Tool: "shell", Effect: "read", Pipeline: []codegen.ProgramCommand{{Name: "cat", Effect: "read"}, {Name: "grep", Effect: "read", Connector: "pipe"}}, Args: []codegen.ProgramArg{
		{Path: []string{"pipe_0_argv_0"}, Value: codegen.ProgramValue{Kind: "input", Input: "path"}},
		{Path: []string{"pipe_1_argv_0"}, Value: codegen.ProgramValue{Kind: "input", Input: "pattern"}},
	}}}}
	p, err := codegen.GenerateProgramPackage(g)
	if err != nil {
		t.Fatal(err)
	}
	pkg := t.TempDir()
	for name, data := range p.Files {
		if err := os.WriteFile(filepath.Join(pkg, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data := filepath.Join(t.TempDir(), "fresh-input.txt")
	if err := os.WriteFile(data, []byte("ignore\nmatch this line\nignore too\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "tap")
	build := exec.Command("go", "build", "-o", bin, "./host")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build TAP host: %v\n%s", err, output)
	}
	inputs, _ := json.Marshal(map[string]string{"path": data, "pattern": "match"})
	run := exec.Command(bin, "--runs", t.TempDir(), pkg, string(inputs))
	output, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("generated pipeline failed through TAP host: %v\n%s", err, output)
	}
	pos := bytes.LastIndex(output, []byte("RESULT (exit 0)\n"))
	if pos < 0 {
		t.Fatalf("missing runner result: %s", output)
	}
	var result map[string]struct {
		Stdout string `json:"stdout"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(output[pos+len("RESULT (exit 0)\n"):]), &result); err != nil {
		t.Fatalf("invalid result: %v\n%s", err, output)
	}
	if result["step_1"].Stdout != "match this line\n" {
		t.Fatalf("wrong piped result: %+v\n%s", result, output)
	}
}

func TestGeneratedSuccessChainRunsThroughHost(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the TAP host")
	}
	if _, err := exec.LookPath("printf"); err != nil {
		t.Skip("printf unavailable")
	}
	g := &codegen.ProgramGraph{CandidateID: "lc_hostchain", Inputs: []codegen.ProgramInput{
		{Name: "first", Type: "string", Source: "supplied at invocation"},
		{Name: "second", Type: "string", Source: "supplied at invocation"},
	}, Steps: []codegen.ProgramStep{{Role: "emit two values in order", Tool: "shell", Effect: "read",
		Pipeline: []codegen.ProgramCommand{{Name: "printf", Effect: "read"}, {Name: "printf", Effect: "read", Connector: "and"}},
		Args: []codegen.ProgramArg{
			{Path: []string{"pipe_0_argv_0"}, Value: codegen.ProgramValue{Kind: "input", Input: "first"}},
			{Path: []string{"pipe_1_argv_0"}, Value: codegen.ProgramValue{Kind: "input", Input: "second"}},
		}}}}
	p, err := codegen.GenerateProgramPackage(g)
	if err != nil {
		t.Fatal(err)
	}
	pkg := t.TempDir()
	for name, data := range p.Files {
		if err := os.WriteFile(filepath.Join(pkg, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(t.TempDir(), "tap")
	build := exec.Command("go", "build", "-o", bin, "./host")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build TAP host: %v\n%s", err, output)
	}
	inputs, _ := json.Marshal(map[string]string{"first": "first-value", "second": "second-value"})
	run := exec.Command(bin, "--runs", t.TempDir(), pkg, string(inputs))
	output, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("generated success chain failed through TAP host: %v\n%s", err, output)
	}
	pos := bytes.LastIndex(output, []byte("RESULT (exit 0)\n"))
	if pos < 0 {
		t.Fatalf("missing runner result: %s", output)
	}
	var result map[string]struct {
		Stdout string `json:"stdout"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(output[pos+len("RESULT (exit 0)\n"):]), &result); err != nil {
		t.Fatalf("invalid result: %v\n%s", err, output)
	}
	if result["step_1"].Stdout != "second-value" {
		t.Fatalf("wrong success chain result: %+v\n%s", result, output)
	}
}
