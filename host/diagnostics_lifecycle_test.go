package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/bridge"
)

// A real WASI guest exercises the native stderr writer and host-command
// lifetime separately from the interpreters' JSON result protocol.
const diagnosticsGuest = `package main
import("bytes";"encoding/json";"fmt";"os";"time")
func main() {
 mode:=os.Args[1]
 if mode=="pending" {
  enc,dec:=json.NewEncoder(os.Stdout),json.NewDecoder(os.Stdin)
  enc.Encode(map[string]any{"id":"owned","method":"exec","command":os.Args[2],"args":[]string{"-test.run=^TestPendingProgramLifetimeHelper$","--",os.Args[3]}})
  ready:=false
  for i:=0;i<600;i++ {
   id:=fmt.Sprintf("ready-%d",i)
   enc.Encode(map[string]any{"id":id,"method":"read","path":os.Args[3]})
   var reply map[string]any
   if err:=dec.Decode(&reply);err!=nil {panic(err)}
   if reply["id"]=="owned" {panic("owned child exited before guest completion")}
   if reply["id"]!=id {panic("unexpected readiness reply")}
   if result,ok:=reply["result"].(string);ok&&result!="" {ready=true;break}
   time.Sleep(50*time.Millisecond)
  }
  if !ready {panic("owned child never became ready")}
 } else {
  chunk:=bytes.Repeat([]byte("synthetic diagnostic\n"),256)
  for i:=0;i<4000;i++ {os.Stderr.Write(chunk)}
  if mode=="memory" {os.Stderr.Write([]byte("Memory"));os.Stderr.Write([]byte("Error\n"));os.Exit(2)}
 }
 json.NewEncoder(os.Stdout).Encode(map[string]any{"method":"return","stdout":"complete\n","stderr":"result diagnostic\n","exit":0})
}`

type pendingProgramMarker struct {
	PID     int
	Address string
}

// This helper is the direct host child, not a child of a shell or sleep. Its
// owned socket proves it is alive before the guest returns, and disappears
// when the runner terminates it. Arguments select helper mode without adding
// environment variables or changing the user's process environment.
func TestPendingProgramLifetimeHelper(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--" {
		return
	}
	marker := os.Args[len(os.Args)-1]
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	// A broken runner cannot leave the fixture running without a bound.
	listener.(*net.TCPListener).SetDeadline(time.Now().Add(time.Minute))
	data, err := json.Marshal(pendingProgramMarker{PID: os.Getpid(), Address: listener.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	temporary := marker + ".tmp"
	if err := os.WriteFile(temporary, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporary, marker); err != nil {
		t.Fatal(err)
	}
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		fmt.Fprintln(connection, marker)
		connection.Close()
	}
}

func TestRealGuestDiagnosticsAndPendingProgramLifetime(t *testing.T) {
	work := t.TempDir()
	source, binary := filepath.Join(work, "main.go"), filepath.Join(work, "main.wasm")
	if err := os.WriteFile(source, []byte(diagnosticsGuest), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", binary, source)
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm") // existing Go target settings
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile real WASI diagnostics guest: %v\n%s", err, out)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command, markerPath := filepath.Base(executable), filepath.Join(work, "child-ready.json")
	manifest := fmt.Sprintf("apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.test, name: diagnostics, version: 0.1.0}\nexecution: {entrypoint: main.wasm}\ncommands:\n  - {command: %q, args: [\"-test.run=^TestPendingProgramLifetimeHelper$\", \"--\", %q], effect: destructive}\nfiles:\n  - {path: %q, access: read}\n", command, markerPath, markerPath)
	if err := os.WriteFile(filepath.Join(work, "primitive.yaml"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	run := func(ctx context.Context, mode string, journal io.Writer) (*Result, error) {
		return Run(ctx, Options{Package: work, Args: []string{mode, command, markerPath}, Journal: journal, CacheDir: cache, RunsDir: t.TempDir(),
			Proc:    bridge.Proc{Env: []string{"PATH=" + filepath.Dir(executable)}},
			Approve: func(Ask) Grant { return Grant{OK: true, Limit: 1} }})
	}
	res, err := run(context.Background(), "complete", io.Discard)
	if err != nil || res.Exit != 0 || res.Stdout != "complete\n" || res.Stderr != "result diagnostic\n" {
		t.Fatalf("bounded native diagnostics altered the result: %+v %v", res, err)
	}
	if _, err := run(context.Background(), "memory", io.Discard); err == nil || !strings.Contains(err.Error(), "ran out of memory") {
		t.Fatalf("late memory failure diagnostic was lost: %v", err)
	}
	readMarker := func() (pendingProgramMarker, error) {
		var marker pendingProgramMarker
		data, err := os.ReadFile(markerPath)
		if err == nil {
			err = json.Unmarshal(data, &marker)
		}
		return marker, err
	}
	// Cleanup signals only a process still serving this fixture's unique
	// marker. An already terminated child, or a reused PID/port, is left alone.
	killOwnedChild := func() {
		marker, err := readMarker()
		if err != nil {
			return
		}
		connection, err := net.DialTimeout("tcp", marker.Address, time.Second)
		if err != nil {
			return
		}
		connection.SetReadDeadline(time.Now().Add(time.Second))
		response, _ := io.ReadAll(connection)
		connection.Close()
		if strings.TrimSpace(string(response)) == markerPath {
			if process, err := os.FindProcess(marker.PID); err == nil {
				process.Kill()
			}
		}
	}
	t.Cleanup(killOwnedChild)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	watchdogDone := make(chan struct{})
	stopWatchdog := context.AfterFunc(ctx, func() { killOwnedChild(); close(watchdogDone) })
	defer func() {
		if !stopWatchdog() {
			<-watchdogDone
		}
	}()
	// Audit and dispatch goroutines may write concurrently. A real file keeps
	// each write intact without introducing a racy test-only bytes.Buffer.
	journal, err := os.CreateTemp(t.TempDir(), "pending-journal")
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	res, err = run(ctx, "pending", journal)
	if err != nil || res.Stdout != "complete\n" || res.Stderr != "result diagnostic\n" || ctx.Err() != nil {
		t.Fatalf("completed guest's result changed while canceling pending work: %+v %v", res, err)
	}
	marker, err := readMarker()
	if err != nil || marker.PID <= 0 || marker.Address == "" {
		t.Fatalf("owned direct child did not start: %+v %v", marker, err)
	}
	if connection, err := net.DialTimeout("tcp", marker.Address, time.Second); err == nil {
		connection.Close()
		t.Fatal("finished guest left its direct host child alive")
	}
	journalData, err := os.ReadFile(journal.Name())
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(journalData))
	terminated := false
	for decoder.More() {
		var event map[string]any
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event["command"] == command {
			exit, ok := event["exit"].(float64)
			terminated = ok && exit != 0
		}
	}
	if !terminated {
		t.Fatalf("owned child did not record termination: %s", journalData)
	}
}
