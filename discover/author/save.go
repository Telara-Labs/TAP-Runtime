package author

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	clientpkg "gitlab.com/telara-labs/tap-runtime/discover/client"
	"gitlab.com/telara-labs/tap-runtime/discover/model"
	"gitlab.com/telara-labs/tap-runtime/discover/pack"
)

// OriginAgentAuthored marks a package a host agent wrote from a brief.
const OriginAgentAuthored = "agent_authored"

// Authoring is a package's AUTHORING.json: who wrote it, from what, and
// what established each contract field.
type Authoring struct {
	Kind        string                `json:"kind"`
	Name        string                `json:"name"`
	Publisher   string                `json:"publisher"`
	Author      string                `json:"author"`
	Agent       string                `json:"agent"`
	Selection   string                `json:"selection"`
	Sources     []string              `json:"sources"`
	Candidate   string                `json:"candidate,omitempty"`
	BriefDigest string                `json:"brief_digest"`
	Contract    map[string]BriefField `json:"contract"`
	Interface   json.RawMessage       `json:"interface"`
}

// ContractFields are the fields an authored contract must establish.
var ContractFields = []string{"goal", "inputs", "scope", "procedure", "output", "oracle", "failures", "boundary"}

// ReadAuthoring reads and checks a package's AUTHORING.json. Every contract
// field must have a value and say what established it.
func ReadAuthoring(pkg string) (*Authoring, error) {
	raw, err := os.ReadFile(filepath.Join(pkg, "AUTHORING.json"))
	if err != nil {
		return nil, fmt.Errorf("an authored package needs AUTHORING.json: %w", err)
	}
	var a Authoring
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return nil, fmt.Errorf("AUTHORING.json: %w", err)
	}
	var problems []string
	if a.Kind != "tap.authoring/v1" {
		problems = append(problems, "kind is not tap.authoring/v1")
	}
	if a.Author != "host-agent" {
		problems = append(problems, `author is not "host-agent"`)
	}
	if !pack.SkillName.MatchString(a.Name) {
		problems = append(problems, fmt.Sprintf("name %q is not a usable folder name", a.Name))
	}
	if a.Selection != SelectedTask && a.Selection != DiscoverCandidate && a.Selection != DiscoverOpportunity && a.Selection != DiscoverSpan && a.Selection != DiscoverLogic {
		problems = append(problems, "selection is not a supported authoring brief selection")
	}
	if a.Selection != SelectedTask && a.Candidate == "" {
		problems = append(problems, "a package from discover must name its candidate, opportunity, span or logic group")
	}
	if len(a.Sources) == 0 || a.BriefDigest == "" {
		problems = append(problems, "sources and brief_digest are required")
	}
	for _, s := range a.Sources {
		if !strings.HasPrefix(s, "src_") {
			problems = append(problems, "sources must be opaque refs (src_...), not session locations")
			break
		}
	}
	for _, f := range ContractFields {
		v, ok := a.Contract[f]
		if !ok || v.Value == nil || strings.TrimSpace(v.EstablishedBy) == "" {
			problems = append(problems, "contract."+f+" needs a value and established_by")
		}
	}
	if len(a.Interface) == 0 {
		problems = append(problems, "interface is required")
	}
	if len(problems) > 0 {
		return nil, errors.New("AUTHORING.json: " + strings.Join(problems, "; "))
	}
	return &a, nil
}

// ValidationFor is the status receipts give the package with this digest:
// passed only when they are for this digest and every case passed.
func ValidationFor(rec *Receipts, digest string) (string, error) {
	if rec == nil {
		return model.ValidationNotRun, nil
	}
	if rec.PackageDigest != digest {
		return model.ValidationNotRun, fmt.Errorf("the receipts are for %s, not this package (%s)", rec.PackageDigest, digest)
	}
	if rec.AllPassed && len(rec.Cases) > 0 {
		return model.ValidationPassed, nil
	}
	return model.ValidationFailed, nil
}

// SavePackage installs an authored package directory into root.
func SavePackage(pkgDir, root string, rec *Receipts, receiptsDigest string) (path, validation string, unchanged bool, err error) {
	a, err := ReadAuthoring(pkgDir)
	if err != nil {
		return "", "", false, err
	}
	if marks, err := FindPlaceholders(pkgDir); err != nil {
		return "", "", false, err
	} else if len(marks) > 0 {
		return "", "", false, fmt.Errorf("the package is not finished: %s", strings.Join(marks, "; "))
	}
	pkg, digest, err := PackageDir(pkgDir)
	if err != nil {
		return "", "", false, err
	}
	validation, err = ValidationFor(rec, digest)
	if err != nil {
		return "", "", false, err
	}
	m := pack.Marker{Name: a.Publisher + "/" + a.Name, Digest: digest, Validation: validation,
		Origin: OriginAgentAuthored, Receipts: receiptsDigest}
	if rec != nil {
		m.Cases = len(rec.Cases)
	}
	path, unchanged, err = pack.Install(root, a.Name, pkg, m, AuthoredSkillMD(a, m, filepath.Join(root, a.Name)))
	return path, validation, unchanged, err
}

func AuthoredSkillMD(a *Authoring, m pack.Marker, dir string) string {
	desc := a.Name
	if g, ok := a.Contract["goal"].Value.(string); ok && g != "" {
		desc = g
	}
	descJSON, _ := json.Marshal(desc)
	status := "Validation: **" + m.Validation + "**"
	switch m.Validation {
	case model.ValidationPassed:
		status += fmt.Sprintf(" for digest %s, on %d fresh cases through the TAP runner (receipts %s).", m.Digest, m.Cases, m.Receipts)
	case model.ValidationFailed:
		status += fmt.Sprintf(": at least one of %d cases failed for digest %s. Do not rely on it.", m.Cases, m.Digest)
	default:
		status += ": it has not been run on held-out cases."
	}
	iface, _ := json.MarshalIndent(json.RawMessage(a.Interface), "", "  ")
	return fmt.Sprintf(`---
name: %s
description: %s
---

# %s

A primitive written by a host agent (%s) from a task you selected, not a
recommendation made by `+"`tap discover`"+`. Read AUTHORING.json for what
established each part of its contract, and the program before running it.

%s

Run it with the TAP runner's `+"`tap_run`"+` tool, giving this folder as the package:

    package: %s

Its interface:

    %s

If no `+"`tap_run`"+` tool is available, connect the runner: `+"`tap install --client <client>`"+`.
`, a.Name, descJSON, a.Name, a.Agent, status, dir, strings.ReplaceAll(string(iface), "\n", "\n    "))
}

// LoadReceipts reads receipts written by validate, with their digest.
func LoadReceipts(path string) (*Receipts, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	var rec Receipts
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	if rec.Kind != "tap.validation-receipts/v1" {
		return nil, "", fmt.Errorf("%s is not validation receipts", path)
	}
	sum := sha256.Sum256(raw)
	return &rec, "sha256:" + hex.EncodeToString(sum[:]), nil
}

func SaveCommand(args []string, home string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("discover save", flag.ContinueOnError)
	fs.SetOutput(errOut)
	receipts := fs.String("receipts", "", "receipts from `tap discover validate` for this package")
	client := fs.String("client", "detected", "agents that get a pointer to it: "+strings.Join(clientpkg.IDs(clientpkg.HasSkills), ", ")+", all, none, or detected (installed here and able to run it)")
	project := fs.Bool("project", false, "write pointers into this project's skills folders instead of your home's")
	pos, flags := SplitPositional(args)
	if err := fs.Parse(flags); err != nil {
		return 2
	}
	pos = append(pos, fs.Args()...)
	if len(pos) != 1 {
		fmt.Fprintln(errOut, "discover save: give one package directory")
		return 2
	}
	var rec *Receipts
	var recSum string
	if *receipts != "" {
		var err error
		if rec, recSum, err = LoadReceipts(*receipts); err != nil {
			fmt.Fprintln(errOut, "discover save:", err)
			return 1
		}
	}
	cwd, _ := os.Getwd()
	dest, err := pack.NewDestination(*client, *project, home, cwd)
	if err != nil {
		fmt.Fprintln(errOut, "discover save:", err)
		return 2
	}
	path, validation, unchanged, err := SavePackage(pos[0], dest.Collection, rec, recSum)
	if err != nil {
		fmt.Fprintln(errOut, "discover save:", err)
		return 1
	}
	note := ""
	if unchanged {
		note = " (already saved)"
	}
	fmt.Fprintf(out, "saved %s%s\nvalidation: %s\n", path, note, validation)
	ptrs, err := dest.Point(path)
	fmt.Fprint(out, pack.FormatPointers(ptrs))
	if err != nil {
		fmt.Fprintln(errOut, "discover save:", err)
		return 1
	}
	return 0
}
