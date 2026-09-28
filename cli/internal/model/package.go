package model

import (
	"fmt"
	"os"
	"path/filepath"
)

// Package is a loaded source package: primitive.yaml + workflow.yaml + the
// directory they live in (so validators/executors can resolve schemas/ and
// src/ references).
type Package struct {
	Dir         string
	Manifest    *Manifest
	Workflow    *Workflow // nil if execution.entrypoint failed to load
	WorkflowErr error
}

// LoadPackage loads a package rooted at dir (default "." if dir == "").
func LoadPackage(dir string) (*Package, error) {
	if dir == "" {
		dir = "."
	}
	manifestPath := filepath.Join(dir, "primitive.yaml")
	if _, err := os.Stat(manifestPath); err != nil {
		return nil, fmt.Errorf("no primitive.yaml in %s: %w", dir, err)
	}
	m, err := LoadManifest(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("parsing primitive.yaml: %w", err)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		absDir = dir
	}
	pkg := &Package{Dir: absDir, Manifest: m}

	entry := m.Execution.Entrypoint
	if entry == "" {
		entry = "workflow.yaml"
	}
	wfPath := filepath.Join(dir, entry)
	wf, werr := LoadWorkflow(wfPath)
	if werr != nil {
		pkg.WorkflowErr = werr
	} else {
		pkg.Workflow = wf
	}
	return pkg, nil
}

// ResolvePath resolves a path relative to the package directory.
func (p *Package) ResolvePath(rel string) string {
	return filepath.Join(p.Dir, rel)
}

// InputSchemaPath returns the absolute path to the input schema file if the
// manifest uses a $ref, else "".
func (m *Manifest) InputSchemaPath() string {
	if m.Interface.InputSchemaRef == "" {
		return ""
	}
	return filepath.Join(m.Dir, m.Interface.InputSchemaRef)
}

// OutputSchemaPath returns the absolute path to the output schema file if
// the manifest uses a $ref, else "".
func (m *Manifest) OutputSchemaPath() string {
	if m.Interface.OutputSchemaRef == "" {
		return ""
	}
	return filepath.Join(m.Dir, m.Interface.OutputSchemaRef)
}
