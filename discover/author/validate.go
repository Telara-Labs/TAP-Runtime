package author

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/pack"
)

// CaseFile is a frozen set of cases for one package.
type CaseFile struct {
	Package  string   `json:"package"`
	Contract string   `json:"contract"`
	Oracle   []string `json:"oracle"`
	Cases    []Case   `json:"cases"`
}

// Case is one fresh fixture, the arguments to run with, and, once frozen,
// the oracle's expected result.
type Case struct {
	ID      string      `json:"id"`
	Kind    string      `json:"kind"`
	Note    string      `json:"note,omitempty"`
	Setup   []SetupStep `json:"setup"`
	Args    []string    `json:"args"`
	Approve bool        `json:"approve,omitempty"`
	Expect  *Observed   `json:"expect,omitempty"`
}

// SetupStep builds a fixture: a file written, a directory made, or a git
// command run. Paths are relative to the working directory and may climb
// one level (to the fixture root) to place a file outside it.
type SetupStep struct {
	Write string   `json:"write,omitempty"`
	Text  string   `json:"text,omitempty"`
	Mkdir string   `json:"mkdir,omitempty"`
	Run   []string `json:"run,omitempty"`
}

// Observed is one program's result: its exit code and its stdout, parsed
// when it is JSON.
type Observed struct {
	Exit   int    `json:"exit"`
	Output any    `json:"output,omitempty"`
	Raw    string `json:"raw,omitempty"`
}

// Receipts are the record of one validation run.
type Receipts struct {
	Kind          string            `json:"kind"`
	Package       string            `json:"package"`
	PackageDigest string            `json:"package_digest"`
	Runner        string            `json:"runner"`
	RunnerFlags   []string          `json:"runner_flags,omitempty"`
	RunnerSHA256  string            `json:"runner_sha256"`
	CasesSHA256   string            `json:"cases_sha256"`
	OracleSHA256  map[string]string `json:"oracle_sha256"`
	Started       time.Time         `json:"started"`
	ManifestCheck Observed          `json:"manifest_check"`
	Cases         []CaseReceipt     `json:"cases"`
	AllPassed     bool              `json:"all_passed"`
}

type CaseReceipt struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	Args     []string  `json:"args"`
	Expect   *Observed `json:"expect,omitempty"`
	Oracle   Observed  `json:"oracle"`
	Package  Observed  `json:"package"`
	HostLog  []string  `json:"host_log"`
	Actions  []any     `json:"actions"`
	Before   string    `json:"fixture_before"`
	After    string    `json:"fixture_after"`
	Effects  []string  `json:"effects,omitempty"`
	Failures []string  `json:"failures,omitempty"`
	Pass     bool      `json:"pass"`
	Elapsed  int64     `json:"package_ms"`
}

const CaseTimeout = 2 * time.Minute

// PackageDir packs a package directory the way Draft.Package packs a draft:
// a gzip tar with sorted entries and a fixed time, so the same bytes always
// have the same digest. Links and special files are refused.
func PackageDir(dir string) ([]byte, string, error) {
	files := map[string][]byte{}
	exe := map[string]bool{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", p)
		}
		rel, _ := filepath.Rel(dir, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, _ := d.Info()
		name := filepath.ToSlash(rel)
		files[name] = b
		exe[name] = info.Mode()&0o111 != 0
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	if len(files) == 0 {
		return nil, "", fmt.Errorf("%s holds no files", dir)
	}
	return pack.PackFiles(files, func(n string) bool { return exe[n] })
}

// Placeholders are markers of unfinished authoring. A package holding one is
// not validated at all.
var Placeholders = []string{"TODO:", "REPLACE_"}

// FindPlaceholders lists each file line holding a placeholder marker.
func FindPlaceholders(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		for i, line := range strings.Split(string(b), "\n") {
			for _, m := range Placeholders {
				if strings.Contains(line, m) {
					out = append(out, fmt.Sprintf("%s line %d: %s", rel, i+1, m))
				}
			}
		}
		return nil
	})
	return out, err
}

// LoadCases reads a case file and returns it with its digest.
func LoadCases(path string) (*CaseFile, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	var cf CaseFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cf); err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	if len(cf.Oracle) == 0 {
		return nil, "", fmt.Errorf("%s names no oracle", path)
	}
	seen := map[string]bool{}
	for _, c := range cf.Cases {
		if c.ID == "" || seen[c.ID] {
			return nil, "", fmt.Errorf("%s: case id %q is empty or repeated", path, c.ID)
		}
		seen[c.ID] = true
	}
	sum := sha256.Sum256(raw)
	return &cf, "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Fixture is one case's fresh directory: root holds work, the directory the
// runner and the oracle start in.
type Fixture struct {
	Root, Work string `json:"-"`
}

func NewFixture(c Case) (*Fixture, error) {
	tmp, err := os.MkdirTemp("", "tap-validate-"+c.ID+"-")
	if err != nil {
		return nil, err
	}
	// The runner resolves links in every path it is asked about; so does
	// this, so both see the same names.
	root, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		return nil, err
	}
	f := &Fixture{Root: root, Work: filepath.Join(root, "work")}
	if err := os.Mkdir(f.Work, 0o755); err != nil {
		return nil, err
	}
	for i, st := range c.Setup {
		if err := f.Apply(st); err != nil {
			f.Remove()
			return nil, fmt.Errorf("case %s setup step %d: %w", c.ID, i+1, err)
		}
	}
	return f, nil
}

func (f *Fixture) Remove() { os.RemoveAll(f.Root) }

// inside resolves a setup path, which must stay within the fixture root.
func (f *Fixture) Inside(p string) (string, error) {
	if p == "" || filepath.IsAbs(p) {
		return "", fmt.Errorf("path %q must be relative", p)
	}
	full := filepath.Clean(filepath.Join(f.Work, filepath.FromSlash(p)))
	if full != f.Root && !strings.HasPrefix(full, f.Root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q leaves the fixture", p)
	}
	return full, nil
}

func (f *Fixture) Apply(st SetupStep) error {
	n := 0
	for _, set := range []bool{st.Write != "", st.Mkdir != "", len(st.Run) > 0} {
		if set {
			n++
		}
	}
	if n != 1 {
		return errors.New("a step is exactly one of write, mkdir or run")
	}
	switch {
	case st.Write != "":
		p, err := f.Inside(st.Write)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		return os.WriteFile(p, []byte(st.Text), 0o644)
	case st.Mkdir != "":
		p, err := f.Inside(st.Mkdir)
		if err != nil {
			return err
		}
		return os.MkdirAll(p, 0o755)
	}
	// Setup runs git only: fixtures are files and repositories, and a case
	// file must not be a way to run anything else.
	if st.Run[0] != "git" {
		return fmt.Errorf("setup may run git only, not %q", st.Run[0])
	}
	cmd := exec.Command("git", st.Run[1:]...)
	cmd.Dir = f.Work
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %v: %s", strings.Join(st.Run, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

// snapshot is every file under the fixture root, by relative path, as
// sha256 and mode. Directories count too, so a created empty one shows.
func (f *Fixture) Snapshot() (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(f.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(f.Root, p)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			out[rel+"/"] = "dir"
		case d.Type()&fs.ModeSymlink != 0:
			t, _ := os.Readlink(p)
			out[rel] = "link:" + t
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			out[rel] = hex.EncodeToString(sum[:]) + " " + info.Mode().Perm().String()
		}
		return nil
	})
	return out, err
}

func SnapshotDigest(s map[string]string) string {
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s\x00%s\n", k, s[k])
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func SnapshotDiff(a, b map[string]string) []string {
	var out []string
	for k, v := range a {
		if w, ok := b[k]; !ok {
			out = append(out, "removed "+k)
		} else if w != v {
			out = append(out, "changed "+k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			out = append(out, "created "+k)
		}
	}
	sort.Strings(out)
	return out
}

func Observe(exit int, stdout string) Observed {
	o := Observed{Exit: exit}
	t := strings.TrimSpace(stdout)
	var v any
	if t != "" && json.Unmarshal([]byte(t), &v) == nil {
		o.Output = v
	} else {
		o.Raw = stdout
	}
	return o
}

func SameResult(a, b Observed) bool {
	return a.Exit == b.Exit && reflect.DeepEqual(a.Output, b.Output) && a.Raw == b.Raw
}

func RunProgram(dir string, argv []string) (exit int, stdout, stderr string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), CaseTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err = cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ctx.Err() == nil {
		return ee.ExitCode(), so.String(), se.String(), nil
	}
	if ctx.Err() != nil {
		return -1, so.String(), se.String(), fmt.Errorf("timed out after %s", CaseTimeout)
	}
	return 0, so.String(), se.String(), err
}

// OracleArgv resolves the oracle's words: one naming a file beside the case
// file becomes its absolute path.
func OracleArgv(cf *CaseFile, casesDir string) ([]string, map[string]string) {
	// The oracle runs inside each fixture, so a path to it must not be
	// relative to where this command started.
	if abs, err := filepath.Abs(casesDir); err == nil {
		casesDir = abs
	}
	argv := append([]string(nil), cf.Oracle...)
	sums := map[string]string{}
	for i, w := range argv {
		p := filepath.Join(casesDir, w)
		if b, err := os.ReadFile(p); err == nil && !filepath.IsAbs(w) {
			argv[i] = p
			sum := sha256.Sum256(b)
			sums[w] = "sha256:" + hex.EncodeToString(sum[:])
		}
	}
	return argv, sums
}

// OracleRan tells an oracle that reported a result from one that never got
// to: an oracle always prints its result, so a failing exit with nothing on
// stdout is the harness failing (a missing file, a crash), not an expected
// failure of the procedure.
func OracleRan(exit int, stdout string) error {
	if exit != 0 && strings.TrimSpace(stdout) == "" {
		return fmt.Errorf("exited %d and printed nothing", exit)
	}
	return nil
}

// Freeze records the oracle's result on a fresh fixture as each case's
// expectation. A case already frozen is left as it is.
func Freeze(casesPath string) (int, error) {
	cf, _, err := LoadCases(casesPath)
	if err != nil {
		return 0, err
	}
	argv, _ := OracleArgv(cf, filepath.Dir(casesPath))
	n := 0
	for i := range cf.Cases {
		c := &cf.Cases[i]
		if c.Expect != nil {
			continue
		}
		f, err := NewFixture(*c)
		if err != nil {
			return n, err
		}
		exit, so, se, err := RunProgram(f.Work, append(append([]string(nil), argv...), c.Args...))
		f.Remove()
		if err == nil {
			err = OracleRan(exit, so)
		}
		if err != nil {
			return n, fmt.Errorf("case %s oracle: %w: %s", c.ID, err, strings.TrimSpace(se))
		}
		o := Observe(exit, so)
		c.Expect = &o
		n++
	}
	b, err := json.MarshalIndent(cf, "", " ")
	if err != nil {
		return n, err
	}
	return n, os.WriteFile(casesPath, append(b, '\n'), 0o644)
}

// ValidateOptions says what to validate and with which runner.
type ValidateOptions struct {
	Package string
	Cases   string
	Runner  string
	// RunnerFlags go to the runner before its own flags, such as an
	// interpreter store for a machine that must not download one.
	RunnerFlags []string
}

// Validate runs every case and returns the receipts. An error means the run
// could not be carried out; a failing case is a receipt, not an error.
func Validate(o ValidateOptions) (*Receipts, error) {
	pkg, err := filepath.Abs(o.Package)
	if err != nil {
		return nil, err
	}
	if marks, err := FindPlaceholders(pkg); err != nil {
		return nil, err
	} else if len(marks) > 0 {
		return nil, fmt.Errorf("the package is not finished: %s", strings.Join(marks, "; "))
	}
	_, digest, err := PackageDir(pkg)
	if err != nil {
		return nil, err
	}
	cf, casesSum, err := LoadCases(o.Cases)
	if err != nil {
		return nil, err
	}
	runnerBytes, err := os.ReadFile(o.Runner)
	if err != nil {
		return nil, fmt.Errorf("runner: %w", err)
	}
	rs := sha256.Sum256(runnerBytes)
	argv, oracleSums := OracleArgv(cf, filepath.Dir(o.Cases))
	rec := &Receipts{Kind: "tap.validation-receipts/v1", Package: pkg, PackageDigest: digest, Runner: o.Runner, RunnerFlags: o.RunnerFlags,
		RunnerSHA256: "sha256:" + hex.EncodeToString(rs[:]), CasesSHA256: casesSum, OracleSHA256: oracleSums,
		Started: time.Now().UTC(), AllPassed: len(cf.Cases) > 0}

	exit, so, se, err := RunProgram(pkg, []string{o.Runner, "manifest", "check", pkg})
	if err != nil {
		return nil, fmt.Errorf("manifest check: %w", err)
	}
	rec.ManifestCheck = Observed{Exit: exit, Raw: strings.TrimSpace(so + se)}
	if exit != 0 {
		rec.AllPassed = false
	}
	for _, c := range cf.Cases {
		cr, err := ValidateCase(c, pkg, o.Runner, o.RunnerFlags, argv)
		if err != nil {
			return nil, err
		}
		rec.Cases = append(rec.Cases, *cr)
		rec.AllPassed = rec.AllPassed && cr.Pass
	}
	return rec, nil
}

func ValidateCase(c Case, pkg, runner string, runnerFlags, oracle []string) (*CaseReceipt, error) {
	f, err := NewFixture(c)
	if err != nil {
		return nil, err
	}
	defer f.Remove()
	cr := &CaseReceipt{ID: c.ID, Kind: c.Kind, Args: c.Args, Expect: c.Expect}
	fail := func(format string, a ...any) { cr.Failures = append(cr.Failures, fmt.Sprintf(format, a...)) }

	// The oracle runs first, on the same fresh fixture, and must itself
	// leave it as it found it.
	s0, err := f.Snapshot()
	if err != nil {
		return nil, err
	}
	exit, so, se, err := RunProgram(f.Work, append(append([]string(nil), oracle...), c.Args...))
	if err == nil {
		err = OracleRan(exit, so)
	}
	if err != nil {
		return nil, fmt.Errorf("case %s oracle: %w: %s", c.ID, err, strings.TrimSpace(se))
	}
	cr.Oracle = Observe(exit, so)
	if c.Expect != nil && !SameResult(*c.Expect, cr.Oracle) {
		fail("oracle drift: the oracle no longer gives the frozen expectation")
	}
	s1, err := f.Snapshot()
	if err != nil {
		return nil, err
	}
	if d := SnapshotDiff(s0, s1); len(d) > 0 {
		fail("the oracle changed the fixture: %s", strings.Join(d, ", "))
	}
	cr.Before = SnapshotDigest(s1)

	// The runner's own record goes outside the fixture, so the fixture
	// shows only what the package did.
	journal := filepath.Join(os.TempDir(), "tap-validate-journal-"+c.ID+"-"+strconv.FormatInt(time.Now().UnixNano(), 36)+".jsonl")
	defer os.Remove(journal)
	run := append(append([]string{runner}, runnerFlags...), "-no-record", "-journal", journal)
	if c.Approve {
		run = append(run, "-approve")
	}
	run = append(append(run, pkg), c.Args...)
	t0 := time.Now()
	_, so, se, err = RunProgram(f.Work, run)
	cr.Elapsed = time.Since(t0).Milliseconds()
	if err != nil {
		fail("runner: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(se), "\n") {
		if line != "" {
			cr.HostLog = append(cr.HostLog, line)
		}
	}
	head, body, _ := strings.Cut(so, "\n")
	var programExit int
	if _, err := fmt.Sscanf(head, "RESULT (exit %d)", &programExit); err != nil {
		fail("the runner did not run the program (no RESULT line)")
		cr.Package = Observed{Exit: -1, Raw: so}
	} else {
		cr.Package = Observe(programExit, body)
	}
	if b, err := os.ReadFile(journal); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var v any
			if line != "" && json.Unmarshal([]byte(line), &v) == nil {
				cr.Actions = append(cr.Actions, v)
			}
		}
	}
	s2, err := f.Snapshot()
	if err != nil {
		return nil, err
	}
	cr.After = SnapshotDigest(s2)
	if !c.Approve {
		if d := SnapshotDiff(s1, s2); len(d) > 0 {
			cr.Effects = d
			fail("the package changed the fixture: %s", strings.Join(d, ", "))
		}
	}
	if !SameResult(cr.Oracle, cr.Package) {
		fail("result differs from the oracle")
	}
	cr.Pass = len(cr.Failures) == 0
	return cr, nil
}

// DefaultRunner is the tap executable running this command, or tap on PATH
// when this code runs inside another program (telara's CLI).
func DefaultRunner() string {
	if self, err := os.Executable(); err == nil && strings.HasPrefix(filepath.Base(self), "tap") {
		return self
	}
	if p, err := exec.LookPath("tap"); err == nil {
		return p
	}
	return ""
}

func ValidateCommand(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("discover validate", flag.ContinueOnError)
	fs.SetOutput(errOut)
	cases := fs.String("cases", "", "the case file")
	freeze := fs.Bool("freeze", false, "record the oracle's results as the cases' expectations; runs no package")
	runner := fs.String("runner", DefaultRunner(), "the tap runner to run the package with")
	outFile := fs.String("out", "", "write the receipts here")
	pkgArgs, flagArgs := SplitPositional(args)
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	pkgArgs = append(pkgArgs, fs.Args()...)
	if *cases == "" {
		fmt.Fprintln(errOut, "discover validate: --cases is required")
		return 2
	}
	if *freeze {
		n, err := Freeze(*cases)
		if err != nil {
			fmt.Fprintln(errOut, "discover validate:", err)
			return 1
		}
		fmt.Fprintf(out, "froze %d case expectations in %s\n", n, *cases)
		return 0
	}
	if len(pkgArgs) != 1 || *runner == "" {
		fmt.Fprintln(errOut, "discover validate: give one package directory, and a runner if tap is not on PATH")
		return 2
	}
	rec, err := Validate(ValidateOptions{Package: pkgArgs[0], Cases: *cases, Runner: *runner})
	if err != nil {
		fmt.Fprintln(errOut, "discover validate:", err)
		return 1
	}
	if *outFile != "" {
		b, _ := json.MarshalIndent(rec, "", "  ")
		if err := os.WriteFile(*outFile, append(b, '\n'), 0o644); err != nil {
			fmt.Fprintln(errOut, "discover validate:", err)
			return 1
		}
	}
	fmt.Fprintf(out, "package %s\nrunner  %s\nmanifest check: exit %d\n", rec.PackageDigest, rec.RunnerSHA256, rec.ManifestCheck.Exit)
	passed := 0
	for _, c := range rec.Cases {
		verdict := "PASS"
		if c.Pass {
			passed++
		} else {
			verdict = "FAIL"
		}
		fmt.Fprintf(out, "  %-4s %-8s %-22s oracle exit %d, package exit %d, %dms\n", verdict, c.ID, c.Kind, c.Oracle.Exit, c.Package.Exit, c.Elapsed)
		for _, f := range c.Failures {
			fmt.Fprintf(out, "         %s\n", f)
		}
	}
	fmt.Fprintf(out, "%d of %d cases passed\n", passed, len(rec.Cases))
	if !rec.AllPassed {
		return 1
	}
	return 0
}

// SplitPositional lets the package come before or after the flags.
func SplitPositional(args []string) (pos, flags []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) && a != "--freeze" && a != "-freeze" {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	return pos, flags
}
