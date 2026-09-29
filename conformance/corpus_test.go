package conformance

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/bind"
	"gitlab.com/telara-labs/tap-runtime/contract/manifest"
	"gitlab.com/telara-labs/tap-runtime/satisfy"
)

// The corpus is data, so that a runner written by somebody else, in another
// language, can be held to it. These tests hold this runner to it.

func read(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile("corpus/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func TestBindCorpus(t *testing.T) {
	var c struct {
		Floor float64
		Cases []struct {
			Capability, Declared, Inventory string
			Bind                            *string
			TreatedAsWrite                  bool `json:"treated_as_write"`
		}
	}
	read(t, "bind.json", &c)
	if c.Floor != bind.Floor {
		t.Fatalf("the corpus is written for a floor of %v and this runner uses %v", c.Floor, bind.Floor)
	}
	if len(c.Cases) < 40 {
		t.Fatalf("only %d cases", len(c.Cases))
	}
	inventories := map[string][]bind.Tool{}
	for _, k := range c.Cases {
		inv, ok := inventories[k.Inventory]
		if !ok {
			var raw []struct {
				Server, Name string
				Annotated    bind.Effect
			}
			read(t, k.Inventory, &raw)
			for _, r := range raw {
				inv = append(inv, bind.Tool{Server: r.Server, Name: r.Name, Annotated: r.Annotated})
			}
			inventories[k.Inventory] = inv
		}
		got := bind.Resolve(k.Capability, bind.Effect(k.Declared), inv)
		switch {
		case k.Bind == nil && got.Bound != nil:
			t.Errorf("%s as %s on %s: bound %s, must refuse", k.Capability, k.Declared, k.Inventory, got.Bound.Name)
		case k.Bind != nil && (got.Bound == nil || got.Bound.Name != *k.Bind):
			t.Errorf("%s as %s on %s: want %s, got %+v (%s)", k.Capability, k.Declared, k.Inventory, *k.Bind, got.Bound, got.Refused)
		case k.Bind != nil && got.Gated != k.TreatedAsWrite:
			t.Errorf("%s: treated as write = %v, want %v", k.Capability, got.Gated, k.TreatedAsWrite)
		}
	}
}

func TestSatisfyCorpus(t *testing.T) {
	var c struct {
		Arguments []struct {
			Name      string
			Contract  map[string]any
			Connector map[string]any
			Satisfies bool
		}
		ResultContract map[string]any `json:"result_contract"`
		Results        []struct {
			Name, Answer string
			Conforms     bool
		}
	}
	read(t, "satisfy.json", &c)
	for _, k := range c.Arguments {
		if got := satisfy.Arguments(k.Contract, k.Connector); (len(got) == 0) != k.Satisfies {
			t.Errorf("%s: satisfies = %v, want %v (%s)", k.Name, len(got) == 0, k.Satisfies, strings.Join(got, "; "))
		}
	}
	for _, k := range c.Results {
		if got := satisfy.Result(c.ResultContract, k.Answer); (got == "") != k.Conforms {
			t.Errorf("%s: conforms = %v, want %v (%s)", k.Name, got == "", k.Conforms, got)
		}
	}
	if len(c.Arguments) < 10 || len(c.Results) < 5 {
		t.Fatal("the corpus is thinner than it was")
	}
}

func TestManifestCorpus(t *testing.T) {
	var c struct {
		Cases []struct {
			Name, Manifest string
			MayRun         bool `json:"may_run"`
		}
	}
	read(t, "manifest.json", &c)
	for _, k := range c.Cases {
		m, err := manifest.Parse([]byte(k.Manifest))
		ok := err == nil && len(m.RunProblems()) == 0
		if ok != k.MayRun {
			t.Errorf("%s: may run = %v, want %v", k.Name, ok, k.MayRun)
		}
	}
	if len(c.Cases) < 15 {
		t.Fatal("the corpus is thinner than it was")
	}
}

// The id of a capability must be what a host in any language computes. The
// expected values were computed with an RFC 8785 encoder outside this
// repository.
func TestCapabilityIDCorpus(t *testing.T) {
	var cases []struct {
		Name, Question string
		Args, Result   map[string]any
		JCSID          string `json:"jcs_id"`
	}
	read(t, "capability-id.json", &cases)
	if len(cases) < 3 {
		t.Fatalf("only %d vectors", len(cases))
	}
	for _, c := range cases {
		if got := manifest.CapabilityID(c.Question, c.Args, c.Result); got != c.JCSID {
			t.Errorf("%s: %s, want %s", c.Name, got, c.JCSID)
		}
	}
}
