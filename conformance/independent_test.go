package conformance

import (
	"strings"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/contract/glob"
	"gitlab.com/telara-labs/tap-runtime/contract/manifest"
)

// TestIndependentCorpus runs cases written from the spec text by an author
// who did not write the runner (TENG-3033). A failure is a disagreement
// between doc 34 and the code, to be settled in one or the other.
func TestIndependentCorpus(t *testing.T) {
	var c struct {
		Manifest []struct {
			Name, Manifest string
			MayRun         bool `json:"may_run"`
		}
		Args []struct {
			Name          string
			Pattern, Args []string
			Match         bool
		}
		Hosts []struct {
			Name, Pattern, Host string
			Match               bool
		}
		Paths []struct {
			Name, Pattern, Path string
			Match               bool
		}
		Widening []struct {
			Name, Approved, Next string
			Widens               bool
		}
	}
	read(t, "independent.json", &c)
	for _, k := range c.Manifest {
		m, err := manifest.Parse([]byte(k.Manifest))
		ok := err == nil && len(m.RunProblems()) == 0
		if ok != k.MayRun {
			detail := ""
			if err != nil {
				detail = err.Error()
			} else {
				detail = strings.Join(m.RunProblems(), "; ")
			}
			t.Errorf("manifest %q: may run = %v, want %v (%s)", k.Name, ok, k.MayRun, detail)
		}
	}
	for _, k := range c.Args {
		if got := glob.Args(k.Pattern, k.Args); got != k.Match {
			t.Errorf("args %q: %v against %v = %v, want %v", k.Name, k.Pattern, k.Args, got, k.Match)
		}
	}
	for _, k := range c.Hosts {
		if got := glob.Host(k.Pattern, k.Host); got != k.Match {
			t.Errorf("host %q: %s against %s = %v, want %v", k.Name, k.Pattern, k.Host, got, k.Match)
		}
	}
	for _, k := range c.Paths {
		if got := glob.Path(k.Pattern, k.Path); got != k.Match {
			t.Errorf("path %q: %s against %s = %v, want %v", k.Name, k.Pattern, k.Path, got, k.Match)
		}
	}
	for _, k := range c.Widening {
		a, err1 := manifest.Parse([]byte(k.Approved))
		b, err2 := manifest.Parse([]byte(k.Next))
		if err1 != nil || err2 != nil {
			t.Errorf("widening %q: fixture does not parse: %v %v", k.Name, err1, err2)
			continue
		}
		w := manifest.Widening(a, b)
		if (len(w) > 0) != k.Widens {
			t.Errorf("widening %q: widens = %v (%v), want %v", k.Name, len(w) > 0, w, k.Widens)
		}
	}
}
