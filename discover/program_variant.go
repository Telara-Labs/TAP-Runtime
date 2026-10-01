package discover

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"

	"gitlab.com/telara-labs/tap-runtime/discover/shellparse"
)

// GroupProgramVariants separates incompatible invocation contracts inside one
// recurring logic family, then merges argument-presence variants only when
// graph synthesis and code generation both succeed for their combined spans.
// Values never enter identity. Different tool bindings remain separate;
// optional arguments do not make one process look like several primitives.
func GroupProgramVariants(c LogicCandidate, proposals []SpanProposal, sessions []trace.Session) ([]LogicCandidate, error) {
	bySpan := map[string]SpanProposal{}
	for _, p := range proposals {
		bySpan[p.ID] = p
	}
	wanted := map[string]bool{}
	for _, id := range c.Members {
		p, ok := bySpan[id]
		if !ok {
			return nil, fmt.Errorf("candidate %s names absent span %s", c.ID, id)
		}
		wanted[p.Client+"\x00"+p.Session] = true
	}
	cp := make([]trace.Session, 0, len(wanted))
	for _, s := range sessions {
		if !wanted[s.Client+"\x00"+s.ID] {
			continue
		}
		s.Calls = append([]trace.Call(nil), s.Calls...)
		cp = append(cp, s)
	}
	trace.DropCopiedCalls(cp)
	bySession := map[string]trace.Session{}
	for _, s := range cp {
		bySession[s.Client+"\x00"+s.ID] = s
	}
	type bucket struct {
		candidate LogicCandidate
		sessions  map[string]bool
		core      string
	}
	buckets := map[string]*bucket{}
	for _, id := range c.Members {
		p, ok := bySpan[id]
		if !ok {
			return nil, fmt.Errorf("candidate %s names absent span %s", c.ID, id)
		}
		s, ok := bySession[p.Client+"\x00"+p.Session]
		if !ok {
			return nil, fmt.Errorf("source for span %s is unavailable", id)
		}
		var requestCalls []trace.Call
		for _, call := range s.Calls {
			if call.Request == p.Request {
				requestCalls = append(requestCalls, call)
			}
		}
		var steps []string
		var coreSteps []string
		for i, ordinal := range p.Calls {
			if ordinal < 1 || ordinal > len(requestCalls) || i >= len(p.CallHashes) {
				return nil, fmt.Errorf("span %s call %d is unavailable", id, ordinal)
			}
			call := requestCalls[ordinal-1]
			if spanCallHash(call) != p.CallHashes[i] {
				return nil, fmt.Errorf("span %s source call %d changed", id, ordinal)
			}
			sig := programCallSignature(call)
			if len(steps) == 0 || steps[len(steps)-1] != sig {
				steps = append(steps, sig)
			}
			core := programCallCoreSignature(call)
			if len(coreSteps) == 0 || coreSteps[len(coreSteps)-1] != core {
				coreSteps = append(coreSteps, core)
			}
		}
		if len(steps) == 0 {
			continue
		}
		if p.CodeShape != "" {
			// A broad call-order motif is only retrieval evidence. Keep the
			// exact code syntax shape separate until AST and data-flow proof.
			steps = []string{"inline_python:" + p.CodeShape}
			coreSteps = append([]string(nil), steps...)
		}
		signature := strings.Join(steps, " -> ")
		b := buckets[signature]
		if b == nil {
			sum := sha256.Sum256([]byte(signature))
			variant := c
			variant.ID = c.ID + "-v" + hex.EncodeToString(sum[:4])
			variant.Key = signature
			variant.Members = nil
			variant.Executions = 0
			variant.Sessions = 0
			variant.Proposals = 0
			variant.Example = p
			b = &bucket{candidate: variant, sessions: map[string]bool{}, core: strings.Join(coreSteps, " -> ")}
			buckets[signature] = b
		}
		b.candidate.Members = append(b.candidate.Members, id)
		b.candidate.Proposals++
		b.candidate.Executions++
		b.sessions[p.Client+"\x00"+p.Session] = true
	}
	byCore := map[string][]*bucket{}
	for _, b := range buckets {
		b.candidate.Sessions = len(b.sessions)
		sort.Strings(b.candidate.Members)
		byCore[b.core] = append(byCore[b.core], b)
	}
	var out []LogicCandidate
	for core, group := range byCore {
		sort.Slice(group, func(i, j int) bool {
			if group[i].candidate.Sessions != group[j].candidate.Sessions {
				return group[i].candidate.Sessions > group[j].candidate.Sessions
			}
			return group[i].candidate.ID < group[j].candidate.ID
		})
		var merged []LogicCandidate
		var mergedShapeKeys [][]string
		for _, b := range group {
			joined := false
			for i := range merged {
				probe := merged[i]
				probe.Members = append(append([]string(nil), probe.Members...), b.candidate.Members...)
				sort.Strings(probe.Members)
				graph, err := SynthesizeProgramGraph(probe, proposals, sessions)
				if err != nil {
					return nil, err
				}
				if len(graph.Problems) > 0 {
					continue
				}
				if _, err := GenerateProgramPackage(graph); err != nil {
					continue
				}
				probe.Proposals += b.candidate.Proposals
				probe.Executions += b.candidate.Executions
				sessionSet := map[string]bool{}
				for _, id := range probe.Members {
					p := bySpan[id]
					sessionSet[p.Client+"\x00"+p.Session] = true
				}
				probe.Sessions = len(sessionSet)
				merged[i] = probe
				mergedShapeKeys[i] = append(mergedShapeKeys[i], b.candidate.Key)
				joined = true
				break
			}
			if !joined {
				merged = append(merged, b.candidate)
				mergedShapeKeys = append(mergedShapeKeys, []string{b.candidate.Key})
			}
		}
		for i, v := range merged {
			if len(mergedShapeKeys[i]) > 1 {
				sort.Strings(mergedShapeKeys[i])
				sum := sha256.Sum256([]byte(strings.Join(mergedShapeKeys[i], "|")))
				v.ID = c.ID + "-m" + hex.EncodeToString(sum[:4])
				v.Key = fmt.Sprintf("%s [optional fields aligned across %d shapes]", core, len(mergedShapeKeys[i]))
			}
			v.Executions = variantIndependentExecutions(v.Members, bySpan)
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sessions != out[j].Sessions {
			return out[i].Sessions > out[j].Sessions
		}
		if out[i].Executions != out[j].Executions {
			return out[i].Executions > out[j].Executions
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func variantIndependentExecutions(members []string, bySpan map[string]SpanProposal) int {
	spans := make([]SpanProposal, 0, len(members))
	for _, id := range members {
		spans = append(spans, bySpan[id])
	}
	sort.Slice(spans, func(i, j int) bool {
		a, b := spans[i], spans[j]
		if a.Client != b.Client {
			return a.Client < b.Client
		}
		if a.Session != b.Session {
			return a.Session < b.Session
		}
		if a.Request != b.Request {
			return a.Request < b.Request
		}
		if len(a.Calls) != len(b.Calls) {
			return len(a.Calls) > len(b.Calls)
		}
		return a.ID < b.ID
	})
	used := map[string]bool{}
	count := 0
	for _, p := range spans {
		overlap := false
		for _, call := range p.Calls {
			if used[logicCallID(p, call)] {
				overlap = true
				break
			}
		}
		if overlap {
			continue
		}
		count++
		for _, call := range p.Calls {
			used[logicCallID(p, call)] = true
		}
	}
	return count
}

func programCallSignature(call trace.Call) string {
	var fields []string
	for path, field := range trace.ObservedArgs(call) {
		value := path + ":" + field.TypeName
		if field.JsonString {
			value += ":json_string"
		}
		if trace.OperationSelector(call, path) {
			value += "=" + field.Value
		}
		fields = append(fields, value)
	}
	sort.Strings(fields)
	return programCallToolIdentity(call) + "@" + call.MCPServer + "/" + call.MCPTool + "(" + strings.Join(fields, ",") + ")"
}

func programCallCoreSignature(call trace.Call) string {
	var selectors []string
	for path, field := range trace.ObservedArgs(call) {
		if trace.OperationSelector(call, path) {
			selectors = append(selectors, path+"="+field.Value)
		}
	}
	sort.Strings(selectors)
	return programCallToolIdentity(call) + "@" + call.MCPServer + "/" + call.MCPTool + "(" + strings.Join(selectors, ",") + ")"
}

func programCallToolIdentity(call trace.Call) string {
	if call.Tool == "shell" {
		if plan, err := shellparse.ProgramShellPlan(call.Command); err == nil {
			var names []string
			for _, stage := range plan {
				if stage.Connector != "" {
					names = append(names, stage.Connector)
				}
				names = append(names, stage.Words[0])
			}
			return "shell:" + strings.Join(names, ":")
		}
	}
	return call.Tool
}
