package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWebBuildUsesNativeRequestLimit(t *testing.T) {
	store := jsStore(t)
	for _, tc := range []struct {
		name, limits string
		want         int
	}{{"default", "", defaultDispatches}, {"declared", ", limits: {max_dispatches: 3}", 3}, {"zero", ", limits: {max_dispatches: 0}", 0}, {"negative", ", limits: {max_dispatches: -1}", -1}} {
		t.Run(tc.name, func(t *testing.T) {
			manifest := strings.Replace(webHead, "%s", tc.name, 1) + "execution: {entrypoint: main.js" + tc.limits + "}\n"
			pkg := webPackage(t, manifest, "main.js", "print(1)")
			page, _, err := buildWebPage(store, []string{pkg})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(page), fmt.Sprintf(`"max_dispatches": %d`, tc.want)) {
				t.Fatalf("compiled page lost the manifest request limit %d", tc.want)
			}
		})
	}
}

// Run the real page job handler and QuickJS WASM with a controlled provider.
// This is source integration evidence, not a live claude.ai connector result.
func TestWebConnectorAttemptsAndRequestBudget(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	wasm, _, sum, err := obtain(interpreterStore(t), "main.js")
	if err != nil {
		t.Fatal(err)
	}
	wasmFile := filepath.Join(t.TempDir(), "qjs.wasm")
	if err := os.WriteFile(wasmFile, wasm, 0o600); err != nil {
		t.Fatal(err)
	}
	guardStart, guardEnd := strings.Index(webWorkerPage, "// TAP-GUARD-BEGIN"), strings.Index(webWorkerPage, "// TAP-GUARD-END")
	jobStart, jobEnd := strings.Index(webWorkerPage, "async function handleJob("), strings.Index(webWorkerPage, "async function start(")
	if guardStart < 0 || guardEnd <= guardStart || jobStart < 0 || jobEnd <= jobStart {
		t.Fatal("the real page guard and job handler are not available")
	}
	script := `const fs = require('fs');
(async () => {
const module = await WebAssembly.compile(fs.readFileSync(` + quote(wasmFile) + `));
const PRELUDE = ` + quote(jsPrelude) + `;
const RUNNER = 'source-test';
` + strings.Replace(webRunner, "export async function runPrimitive", "async function runPrimitive", 1) + "\n" + webWorkerPage[guardStart:guardEnd] + `
let PRIMITIVES, mcp, updates, backend, describe;
const jobs = [], db = {collection: () => ({doc: () => ({update: async (value) => {updates.push(value);}})})};
function renderJobs() {}
` + webWorkerPage[jobStart:jobEnd] + `
const batch = (n, alias = 'drive') => 'const r = tap.callMany(Array.from({length:' + n + '}, () => ["' + alias + '", {}])); print(r.filter(x => x instanceof Error && x.code === "refused").length);';
const cases = [
  {name:'success', script:'print(tap.call("drive", {}));', mode:'success', calls:1, refused:0, state:'done', result:'ok'},
  {name:'provider error', script:'tap.call("drive", {});', mode:'failure', calls:1, refused:0, state:'failed'},
  {name:'provider refused error still dispatched', script:'tap.call("drive", {});', mode:'provider-refused', calls:1, refused:0, state:'failed'},
  ...['missing annotation','explicit write','destructive','describe error'].map(mode => ({name:mode, mode, script:'tap.call("drive", {});', calls:0, refused:1, state:'failed'})),
  {name:'undeclared alias', mode:'success', script:'tap.call("absent", {});', calls:0, refused:1, state:'failed'},
  {name:'default cap success', mode:'success', script:batch(1001), calls:1000, refused:1, state:'done', result:'1'},
  {name:'default cap provider failure', mode:'failure', script:batch(1001), calls:1000, refused:1, state:'done', result:'1'},
  {name:'declared cap provider failure', mode:'failure', limit:1, script:batch(2), calls:1, refused:1, state:'done', result:'1'},
  {name:'zero cap', mode:'success', limit:0, script:batch(1), calls:0, refused:1, state:'done', result:'1'},
  {name:'negative cap', mode:'success', limit:-1, script:batch(1), calls:0, refused:1, state:'done', result:'1'},
  {name:'alias refusals consume budget', mode:'success', limit:2, script:'const r=tap.callMany([["absent",{}],["drive",{}],["drive",{}]]); print(r.filter(x=>x instanceof Error&&x.code==="refused").length);', calls:1, refused:2, state:'done', result:'2'},
  {name:'annotation refusals consume budget', mode:'missing annotation', limit:1, script:batch(2), calls:0, refused:2, state:'done', result:'2', descriptions:1},
  {name:'unavailable methods consume budget', mode:'success', limit:1, script:'try { tap.read("file"); } catch {} print(tap.callMany([["drive",{}]]).filter(x=>x instanceof Error&&x.code==="refused").length);', calls:0, refused:2, state:'done', result:'1'},
  {name:'discovery and replay are free', mode:'success', limit:1, script:'tap.tools(); tap.tools(); print(tap.call("drive", {}));', calls:1, refused:0, state:'done', result:'ok'},
  {name:'replay guard remains separate', mode:'success', script:'for(let i=0;i<201;i++) tap.call("drive",{});', calls:200, refused:0, state:'failed'},
];
const outcomes=[];
for (const c of cases) {
  backend=0; describe=0; updates=[];
  mcp={
    describeTool: async () => {
      describe++;
      if(c.mode==='describe error') throw new Error('describe unavailable');
      const annotations=c.mode==='missing annotation'?{}:c.mode==='explicit write'?{readOnlyHint:false}:c.mode==='destructive'?{readOnlyHint:true,destructiveHint:true}:{readOnlyHint:true};
      return {annotations};
    },
    callTool: async (_, __, ___, options) => {
      backend++;
      if(options.cache!==false) throw new Error('the real page changed connector cache semantics');
      if(c.mode==='failure'||c.mode==='provider-refused') {const e=new Error('provider unavailable');e.code=c.mode==='provider-refused'?'refused':'provider_failed';throw e;}
      return {payload:'ok'};
    },
  };
  PRIMITIVES={probe:{program:c.script,bindings:{drive:{server:'Drive',tool:'search'}},max_dispatches:c.limit??1000}};
  await handleJob(c.name,{primitive:'probe'});
  const got=updates[updates.length-1];
  if(got.connector_calls!==c.calls||got.refused!==c.refused||backend!==c.calls||got.state!==c.state||('result'in c&&got.result!==c.result)||('descriptions'in c&&describe!==c.descriptions))
    throw new Error(JSON.stringify({case:c,got,backend,describe}));
  if(c.name==='provider error'&&!got.stderr.includes('provider_failed: provider unavailable')) throw new Error('provider error was lost');
  if(c.name==='replay guard remains separate'&&!got.stderr.includes('stopped after 200 runs')) throw new Error('replay bound changed');
  outcomes.push({name:c.name,calls:got.connector_calls,refused:got.refused,backend,describe,state:got.state});
}
console.log(JSON.stringify(outcomes));
})().catch(e=>{console.error(e);process.exitCode=1;});
`
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("real web runner accounting: %v\n%s", err, output)
	}
	var outcomes []map[string]any
	if err := json.Unmarshal(output, &outcomes); err != nil || len(outcomes) != 18 {
		t.Fatalf("invalid accounting evidence: %v\n%s", err, output)
	}
	t.Logf("real QuickJS interpreter sha256 %s; 18 actual page-handler scenarios: %s", sum, output)
}
