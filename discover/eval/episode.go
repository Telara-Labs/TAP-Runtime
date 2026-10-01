package eval

import (
	"fmt"
	"strings"

	"gitlab.com/telara-labs/tap-runtime/discover/model"
	"gitlab.com/telara-labs/tap-runtime/discover/routine"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// EpisodeClaim is the single-request judgment.
type EpisodeClaim struct {
	ID          string   `json:"id"`
	Client      string   `json:"client"`
	Session     string   `json:"session"`
	Request     int      `json:"request"`
	Suitability string   `json:"suitability"`
	Reasons     []string `json:"reasons"`
	Steps       []string `json:"steps"`
	// Unexplained lists values no source accounts for (step: value kind).
	Unexplained []string `json:"unexplained,omitempty"`
	// Accounted is, per step, whether every value of it has a source (a
	// judgment step is false).
	Accounted []bool `json:"accounted,omitempty"`
}

// AssessEpisodes judges each episode on its own contract.
func (c *Corpus) AssessEpisodes(eps []Episode) []EpisodeClaim {
	norm := trace.Normalize(c.Sessions)
	byKey := map[string]*trace.NormSession{}
	for i := range norm {
		byKey[norm[i].Client+"/"+norm[i].ID] = &norm[i]
	}
	var out []EpisodeClaim
	for _, e := range eps {
		ec := EpisodeClaim{ID: e.ID, Client: e.Client, Session: e.Session, Request: e.Request}
		ns := byKey[e.Client+"/"+e.Session]
		if ns == nil {
			ec.Suitability, ec.Reasons = model.SuitInsufficient, []string{"no_steps"}
			out = append(out, ec)
			continue
		}
		AssessEpisode(ns, e.Request, &ec)
		out = append(out, ec)
	}
	return out
}

func AssessEpisode(ns *trace.NormSession, req int, ec *EpisodeClaim) {
	text := ""
	if req < len(ns.Requests) {
		text = ns.Requests[req]
	}
	var all []trace.Step
	for _, st := range ns.Steps {
		if st.Request == req {
			all = append(all, st)
		}
	}
	reason := func(s string) { ec.Reasons = append(ec.Reasons, s) }
	if strings.TrimSpace(text) == "" || trace.IsHarness(text) {
		ec.Suitability = model.SuitInvalid
		reason("no_request_text")
		return
	}
	// The work: replayable steps that are not the agent's bookkeeping, and
	// edits (judgment) in their position.
	type item struct {
		st    trace.Step
		human bool
	}
	var work []item
	for _, st := range all {
		switch {
		case trace.EditTools[st.Label] || strings.HasPrefix(st.Label, "patch:"):
			work = append(work, item{st, true})
		case trace.Replayable(st.Label):
			work = append(work, item{st, false})
		}
	}
	for _, w := range work {
		ec.Steps = append(ec.Steps, w.st.Label)
	}
	plain := 0
	for _, w := range work {
		if !w.human {
			plain++
		}
	}
	switch {
	case plain < 2:
		ec.Suitability = model.SuitInsufficient
		reason("fewer_than_two_steps")
		return
	}
	long := len(work) > 25
	// Judgment: human steps only at the end are the hand-back boundary.
	lastPlain, firstHuman := -1, -1
	for k, w := range work {
		if w.human {
			if firstHuman < 0 {
				firstHuman = k
			}
		} else {
			lastPlain = k
		}
	}
	judged := firstHuman >= 0 && firstHuman < lastPlain
	// Every value must have a source.
	chosenSteps, readChosen := 0, 0
	ec.Accounted = make([]bool, len(work))
	defer func() {
		for k := range ec.Accounted {
			ec.Accounted[k] = !work[k].human && !strings.Contains(strings.Join(ec.Unexplained, " "), fmt.Sprintf("step%d:", k+1))
		}
	}()
	for k, w := range work {
		if w.human {
			continue
		}
		prog := strings.Fields(strings.TrimPrefix(w.st.Label, "sh:"))[0]
		unexplained := false
		for _, sl := range w.st.Slots {
			if sl.Sub || sl.Type == trace.SlotFlag || sl.Type == trace.SlotNumber || trace.Derived(sl.Key) || sl.Key == "recv" || len(sl.Value) < 3 {
				continue
			}
			if sl.Type == trace.SlotWord && !(strings.HasPrefix(w.st.Label, "sh:") && trace.SearchPrograms[prog]) {
				continue // a fixed word of the command: a resource kind, a branch
			}
			v := sl.Value
			if trace.InRequest(v, text) || routine.ComposedFromRequest(v, text) {
				continue
			}
			from := false
			for _, prev := range all {
				if prev.Call >= w.st.Call {
					break
				}
				if trace.InResult(v, prev) {
					from = true
					break
				}
			}
			if from {
				continue
			}
			unexplained = true
			ec.Unexplained = append(ec.Unexplained, fmt.Sprintf("step%d:%s", k+1, sl.Type))
		}
		if unexplained {
			chosenSteps++
			if trace.StepEffect(w.st) != "write" {
				readChosen++
			}
		}
	}
	switch {
	case long:
		ec.Suitability = model.SuitInvestigation
		reason(fmt.Sprintf("unbounded_length:%d", len(work)))
	case judged:
		ec.Suitability = model.SuitInsufficient
		reason("judgment_step")
	case readChosen > 0 && 2*chosenSteps > plain:
		ec.Suitability = model.SuitInvestigation
		reason("values_chosen_during_run")
	case chosenSteps > 0:
		ec.Suitability = model.SuitInsufficient
		reason("unexplained_values")
	default:
		ec.Suitability = model.SuitUseful
		reason("single_episode_contract")
		if firstHuman >= 0 {
			reason("hands_back_before_judgment")
		}
	}
}
