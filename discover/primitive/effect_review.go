package primitive

import (
	"fmt"
	"strings"
)

// effectRow is one exact installed step inside a discovered family. Effects
// are selected per member because branches may have different operations.
type effectRow struct {
	MemberID string
	Step     int
	Tool     string
	Observed string
	Selected string
}

func effectRows(f Family, byID map[string]Primitive, observed map[string][]string) []effectRow {
	var rows []effectRow
	for _, id := range f.Members {
		p, ok := byID[id]
		if !ok {
			continue
		}
		for i, tool := range p.Steps {
			seen := "unknown"
			if i < len(observed[id]) && observed[id][i] != "" {
				seen = observed[id][i]
			} else if i < len(p.StepEffects) && p.StepEffects[i] != "" {
				seen = p.StepEffects[i]
			}
			selected := "write"
			if i < len(p.StepEffects) && p.StepEffects[i] == "read" {
				selected = "read"
			}
			rows = append(rows, effectRow{MemberID: id, Step: i, Tool: tool, Observed: seen, Selected: selected})
		}
	}
	return rows
}

func snapshotEffects(byID map[string]Primitive) map[string][]string {
	out := make(map[string][]string, len(byID))
	for id, p := range byID {
		out[id] = make([]string, len(p.Steps))
		for i := range p.Steps {
			out[id][i] = "unknown"
			if i < len(p.StepEffects) && p.StepEffects[i] != "" {
				out[id][i] = p.StepEffects[i]
			}
		}
	}
	return out
}

func setStepEffect(byID map[string]Primitive, row effectRow, effect string) error {
	if effect != "read" && effect != "write" {
		return fmt.Errorf("invalid effect %q", effect)
	}
	p, ok := byID[row.MemberID]
	if !ok || row.Step < 0 || row.Step >= len(p.Steps) {
		return fmt.Errorf("unknown primitive step %s/%d", row.MemberID, row.Step+1)
	}
	for len(p.StepEffects) < len(p.Steps) {
		p.StepEffects = append(p.StepEffects, "unknown")
	}
	p.StepEffects[row.Step] = effect
	p.Effect = "read"
	for _, stepEffect := range p.StepEffects {
		if stepEffect != "read" {
			p.Effect = "write"
			break
		}
	}
	byID[row.MemberID] = p
	return nil
}

func familySelectedEffect(f Family, byID map[string]Primitive) string {
	for _, id := range f.Members {
		p := byID[id]
		for i := range p.Steps {
			if i >= len(p.StepEffects) || p.StepEffects[i] != "read" {
				return "write"
			}
		}
	}
	return "read"
}

func familyHasUnknownEffect(f Family, byID map[string]Primitive) bool {
	for _, row := range effectRows(f, byID, nil) {
		if row.Observed == "unknown" {
			return true
		}
	}
	return false
}

func effectRowLabel(row effectRow) string {
	return fmt.Sprintf("%s · step %d · %s", row.MemberID, row.Step+1, strings.ReplaceAll(row.Tool, "\n", " "))
}
