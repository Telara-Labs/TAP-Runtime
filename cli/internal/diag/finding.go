// Package diag holds the shared finding/failure-taxonomy types used by
// `tap validate`, `tap test`, and `tap diff`.
//
// The taxonomy names (Class) are the six named in 04-cli.md §3: schema,
// undeclared-requirement, effect-mismatch, permission-expansion, provenance,
// content. The sixth, content, was added at G0 (CHANGELOG.md v1, CLI fix
// item 6) specifically for the 03 §3.0/§3.1 description and credential-slot
// rules -- these were previously filed under "schema" for lack of a
// dedicated class (a G0 finding from A2), but they are really about the
// textual/policy CONTENT of the manifest (what becomes attack surface via
// the projected MCP tool description, and what a credential slot's
// capability wording commits to), not structural schema validity. Every
// finding still carries a specific Blocker name identifying the exact rule
// so callers can distinguish and link to a resolution (the cascade-block UX
// convention: name the blocker, link the fix).
package diag

import "fmt"

// Class is one of the six failure-taxonomy names from 04-cli.md §3.
type Class string

const (
	ClassSchema                Class = "schema"
	ClassUndeclaredRequirement Class = "undeclared-requirement"
	ClassEffectMismatch        Class = "effect-mismatch"
	ClassPermissionExpansion   Class = "permission-expansion"
	ClassProvenance            Class = "provenance"
	ClassContent               Class = "content"
)

// Severity distinguishes hard failures from advisory lint warnings (e.g.
// the unjustified `accepts:` pin in 03 §3.0, which is "a lint warning",
// not a publish blocker).
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Finding is a single named, actionable validator result.
type Finding struct {
	Class      Class    `json:"class"`
	Blocker    string   `json:"blocker"`
	Severity   Severity `json:"severity"`
	Message    string   `json:"message"`
	Path       string   `json:"path"`
	Resolution string   `json:"resolution,omitempty"`
}

func (f Finding) String() string {
	sev := "ERROR"
	if f.Severity == SeverityWarning {
		sev = "WARN"
	}
	s := fmt.Sprintf("[%s] %s (%s/%s) at %s: %s", sev, f.Class, f.Blocker, string(f.Class), f.Path, f.Message)
	if f.Resolution != "" {
		s += " -- " + f.Resolution
	}
	return s
}

// Error constructs an error-severity finding.
func Error(class Class, blocker, path, message, resolution string) Finding {
	return Finding{Class: class, Blocker: blocker, Severity: SeverityError, Path: path, Message: message, Resolution: resolution}
}

// Warn constructs a warning-severity finding.
func Warn(class Class, blocker, path, message, resolution string) Finding {
	return Finding{Class: class, Blocker: blocker, Severity: SeverityWarning, Path: path, Message: message, Resolution: resolution}
}

// Findings is a slice with convenience helpers.
type Findings []Finding

// HasErrors reports whether any finding is error-severity.
func (fs Findings) HasErrors() bool {
	for _, f := range fs {
		if f.Severity == SeverityError {
			return true
		}
	}
	return false
}

// Errors returns only the error-severity findings.
func (fs Findings) Errors() Findings {
	var out Findings
	for _, f := range fs {
		if f.Severity == SeverityError {
			out = append(out, f)
		}
	}
	return out
}

// Warnings returns only the warning-severity findings.
func (fs Findings) Warnings() Findings {
	var out Findings
	for _, f := range fs {
		if f.Severity == SeverityWarning {
			out = append(out, f)
		}
	}
	return out
}
