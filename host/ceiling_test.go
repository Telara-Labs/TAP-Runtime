package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The command line cannot ask again, so its limit is where changes stop.
func TestACommandLineLimitIsWhereChangesStop(t *testing.T) {
	dir := inDir(t)
	pkg := writePackage(t, appendManifest, appendScript)
	var asked []Ask
	o := Options{Package: pkg, Journal: io.Discard, InterpDir: interpreterStore(t), RunsDir: t.TempDir(),
		Approve: func(a Ask) Grant {
			asked = append(asked, a)
			if len(asked) == 1 {
				return Grant{OK: true, Limit: 2}
			}
			return Grant{}
		}}
	res, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if got := lines(t, filepath.Join(dir, "counter.txt")); strings.Join(got, ",") != "one,two" {
		t.Fatalf("with two allowed the file holds %v", got)
	}
	if len(asked) != 2 || asked[0].Done != 0 || asked[1].Done != 2 || asked[0].Kind != "run tee *" {
		t.Fatalf("asked %+v", asked)
	}
	if res.Ran != 2 || res.Refused != 1 {
		t.Fatalf("ran %d, refused %d", res.Ran, res.Refused)
	}
}

// Kinds are separate: agreeing to one command is not agreeing to another.
func TestAnAllowanceIsForOneKind(t *testing.T) {
	dir := inDir(t)
	os.MkdirAll("out", 0o755)
	pkg := writePackage(t, `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.test, name: two-kinds, version: 0.1.0}
execution: {entrypoint: main.sh}
files:
  - {path: out, access: write}
commands:
  - {command: tee, globals: ["-a"], args: ["*"], effect: write}
`, "echo one | tee -a counter.txt\necho two > out/x.txt && echo wrote || echo refused\n")
	var kinds []string
	o := Options{Package: pkg, Journal: io.Discard, InterpDir: interpreterStore(t), RunsDir: t.TempDir(),
		Approve: func(a Ask) Grant {
			kinds = append(kinds, a.Kind)
			return Grant{OK: a.Kind == "run tee *", Limit: 100}
		}}
	res, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(kinds, "|") != "run tee *|write files under out" {
		t.Fatalf("asked about %v", kinds)
	}
	if _, err := os.Stat(filepath.Join(dir, "out", "x.txt")); err == nil {
		t.Fatal("agreeing to a command allowed a file write")
	}
	if !strings.Contains(res.Stdout, "refused") {
		t.Fatalf("output:\n%s", res.Stdout)
	}
}
