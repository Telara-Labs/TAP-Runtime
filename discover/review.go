package discover

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// The review lists the primitives that passed every check once, then asks
// which to save and, when the caller can publish, which to publish. Nothing
// is saved or sent without being picked.

// ReviewActions are the side effects the review can take. Publish may be nil
// (the runner alone has no registry); CanPublish then need not be set.
type ReviewActions struct {
	// Save installs a draft and returns where it went.
	Save func(d *Draft) (string, error)
	// CanPublish reports why publishing is unavailable ("" when it is).
	CanPublish func() string
	// Publish sends a draft to "user" or "tenant" and returns the
	// registry's answer and whether it accepted.
	Publish func(d *Draft, audience string) (string, bool, error)
}

// ReviewConfig limits the list and names the publisher for publishing.
type ReviewConfig struct {
	Top       int
	Publisher string
}

// Pick reads "1 3 5", "2-4", "all" or blank against n items (1-based) and
// returns 0-based indexes, each once, in the order given.
func Pick(answer string, n int) []int {
	answer = strings.ToLower(strings.TrimSpace(answer))
	var out []int
	if answer == "all" {
		for i := 0; i < n; i++ {
			out = append(out, i)
		}
		return out
	}
	seen := map[int]bool{}
	for _, f := range strings.Fields(strings.ReplaceAll(answer, ",", " ")) {
		lo, hi := f, f
		if a, b, ok := strings.Cut(f, "-"); ok {
			lo, hi = a, b
		}
		x, e1 := strconv.Atoi(lo)
		y, e2 := strconv.Atoi(hi)
		if e1 != nil || e2 != nil {
			continue
		}
		for k := x; k <= y; k++ {
			if k >= 1 && k <= n && !seen[k] {
				seen[k] = true
				out = append(out, k-1)
			}
		}
	}
	return out
}

// Review runs the pick-list over rep's primitives.
func Review(in io.Reader, out io.Writer, rep *Report, cfg ReviewConfig, act ReviewActions) error {
	sc := bufio.NewScanner(in)
	ask := func(prompt string) (string, bool) {
		fmt.Fprint(out, prompt)
		if !sc.Scan() {
			return "", false
		}
		return strings.TrimSpace(sc.Text()), true
	}
	prims := rep.Primitives()
	if cfg.Top > 0 && len(prims) > cfg.Top {
		prims = prims[:cfg.Top]
	}
	if len(prims) == 0 {
		fmt.Fprintln(out, "No routine was recognized as a useful procedure with a complete draft, so there is nothing to save.")
		return nil
	}
	fmt.Fprintln(out, "\nRecommended procedures, drafted. UNVALIDATED: none has been executed.")
	for i, p := range prims {
		d := p.Draft()
		fmt.Fprintf(out, "\n%d. %s  (%d requests, %d weeks", i+1, d.Name, p.Requests, p.Weeks)
		if p.Measured > 0 {
			fmt.Fprintf(out, ", saves %s tokens per run", humanTokens(p.SavedPerRun.Total()))
		}
		fmt.Fprintf(out, ")\n   asked as: %s\n", trace.OneLine(Redact(p.Example), 120))
		if _, digest, err := d.Package(); err == nil {
			fmt.Fprintf(out, "   status: unvalidated (validation not run for %s)\n", digest)
		}
		var srcs []string
		for _, in := range p.Contract.Inputs {
			srcs = append(srcs, in.Name+"<-"+in.Source)
		}
		fmt.Fprintf(out, "   contract: effect %s, goal %s, inputs %s", p.Contract.Effect, p.Contract.Goal, orNone(srcs))
		if p.Contract.Boundary != "" {
			fmt.Fprintf(out, "; %s", p.Contract.Boundary)
		}
		fmt.Fprintln(out)
		for _, s := range d.Steps {
			fmt.Fprintf(out, "   %d. [%s] %s\n", s.N, s.Kind, trace.OneLine(Redact(s.Line), 130))
		}
		for _, in := range d.Inputs {
			if in.Sensitive {
				fmt.Fprintf(out, "   input $%d %s: a credential, supplied by the caller (recorded value not kept)\n", in.Position, in.Name)
			} else if in.Extract != "" {
				fmt.Fprintf(out, "   value %s (%s): taken from step %d's output, not an input\n", in.Name, in.Type, in.DerivedFrom)
			} else {
				fmt.Fprintf(out, "   input $%d %s (%s), e.g. %s\n", in.Position, in.Name, in.Type, trace.OneLine(Redact(in.Example), 60))
			}
		}
		if len(d.Blocked) > 0 {
			fmt.Fprintf(out, "   BLOCKED: still credential-shaped (%s); it cannot be saved or published\n", strings.Join(d.Blocked, "; "))
		}
		if len(d.Problems) > 0 {
			fmt.Fprintf(out, "   publish checks: %d problem(s): %s\n", len(d.Problems), trace.OneLine(d.Problems[0], 100))
		}
	}
	fmt.Fprintln(out, "\nEvery step is drafted as a change: the runner asks before running it. Read main.sh in a saved folder before you run it.")

	answer, ok := ask("\nSave which? Numbers (1 3, 2-4), all, or blank for none > ")
	if !ok {
		return nil
	}
	for _, i := range Pick(answer, len(prims)) {
		d := prims[i].Draft()
		where, err := act.Save(d)
		if err != nil {
			fmt.Fprintf(out, "%d. %s not saved: %v\n", i+1, d.Name, err)
			continue
		}
		fmt.Fprintf(out, "%d. %s saved -> %s\n", i+1, d.Name, where)
	}

	if act.Publish == nil {
		return nil
	}
	if act.CanPublish != nil {
		if why := act.CanPublish(); why != "" {
			fmt.Fprintf(out, "\nPublishing is unavailable: %s\n", why)
			return nil
		}
	}
	answer, ok = ask("\nPublish which? Numbers, all, or blank for none > ")
	chosen := Pick(answer, len(prims))
	if !ok || len(chosen) == 0 {
		return nil
	}
	publisher := cfg.Publisher
	if publisher == "" || publisher == DefaultPublisher {
		if publisher, ok = ask("Publisher namespace your organisation publishes under (reverse-DNS, e.g. com.acme) > "); !ok || publisher == "" {
			fmt.Fprintln(out, "Not published: a publisher namespace is required.")
			return nil
		}
	}
	aud, ok := ask("Audience: [u]ser (only you) or [t]enant (everyone; an admin approves it first) > ")
	if !ok {
		return nil
	}
	audience := "user"
	if strings.HasPrefix(strings.ToLower(aud), "t") {
		audience = "tenant"
	}
	for _, i := range chosen {
		d := prims[i].DraftAs(publisher, nil)
		if _, err := d.Artifacts(); err != nil {
			fmt.Fprintf(out, "%d. %s not published: %v\n", i+1, d.Name, err)
			continue
		}
		if len(d.Problems) > 0 {
			fmt.Fprintf(out, "%d. %s not published: %s\n", i+1, d.Name, strings.Join(d.Problems, "; "))
			continue
		}
		msg, accepted, err := act.Publish(d, audience)
		switch {
		case err != nil:
			fmt.Fprintf(out, "%d. %s not published: %v\n", i+1, d.Name, err)
		case !accepted:
			fmt.Fprintf(out, "%d. %s refused by the registry:\n%s\n", i+1, d.Name, msg)
		default:
			fmt.Fprintf(out, "%d. published %s/%s@0.1.0 to %s. %s\n", i+1, d.Publisher, d.Name, audience, trace.OneLine(msg, 200))
		}
	}
	return nil
}
