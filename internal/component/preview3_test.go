package component

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero"
)

// section encodes one component section.
func section(id byte, body []byte) []byte {
	return append(appendU32([]byte{id}, uint32(len(body))), body...)
}

func importSection(names ...string) []byte {
	b := appendU32(nil, uint32(len(names)))
	for _, n := range names {
		b = append(b, 0x00)
		b = appendName(b, n)
		b = append(b, 0x05, 0x00) // an instance of type 0
	}
	return section(10, b)
}

func exportSection(names ...string) []byte {
	b := appendU32(nil, uint32(len(names)))
	for _, n := range names {
		b = append(b, 0x00)
		b = appendName(b, n)
		b = append(b, sortInstance, 0x00, 0x00) // instance 0, no type ascription
	}
	return section(11, b)
}

func componentOf(sections ...[]byte) []byte {
	b := append([]byte{}, componentHeader...)
	for _, s := range sections {
		b = append(b, s...)
	}
	return b
}

func TestPreview3IsRecognisedAndItsImportsAreChecked(t *testing.T) {
	run := exportSection("wasi:cli/run@0.3.0")
	for _, c := range []struct {
		name    string
		b       []byte
		p3      bool
		why     string
		refused string
	}{
		{"WASI 0.3 import", componentOf(importSection("wasi:cli/environment@0.3.0", "wasi:clocks/monotonic-clock@0.3.0"), run), true, "imports wasi:cli/environment@0.3.0", ""},
		{"release candidate", componentOf(importSection("wasi:cli/stdout@0.3.0-rc-2025-09-16"), run), true, "imports wasi:cli/stdout", ""},
		{"WASI 0.3 run export only", componentOf(run), true, "exports wasi:cli/run@0.3.0", ""},
		// A stream type is Preview 3 even when every name is 0.2.
		{"stream type", componentOf(section(7, []byte{0x01, 0x66, 0x00}), exportSection("wasi:cli/run@0.2.0")), true, "streams or futures", ""},
		{"HTTP", componentOf(importSection("wasi:cli/environment@0.3.0", "wasi:http/types@0.3.0"), run), true, "", "no direct network access"},
		{"another package", componentOf(importSection("wasi:cli/environment@0.3.0", "wasi:keyvalue/store@0.3.0"), run), true, "", "does not provide"},
		{"not WASI", componentOf(importSection("wasi:cli/environment@0.3.0", "example:custom/thing@0.3.0"), run), true, "", "does not provide"},
		{"not a command", componentOf(importSection("wasi:cli/environment@0.3.0")), true, "", "does not export wasi:cli/run"},
		{"Preview 2 names", componentOf(importSection("wasi:cli/environment@0.2.3"), exportSection("wasi:cli/run@0.2.3")), false, "", ""},
		{"core module", append([]byte{}, moduleHeader...), false, "", ""},
		{"truncated", componentOf([]byte{10, 0x7f}), false, "", ""},
	} {
		p3, why, err := Preview3(c.b)
		switch {
		case c.refused != "":
			if err == nil || !strings.Contains(err.Error(), c.refused) {
				t.Errorf("%s: want refusal %q, got %v %v", c.name, c.refused, p3, err)
			}
		case err != nil:
			t.Errorf("%s: %v", c.name, err)
		case p3 != c.p3 || !strings.Contains(why, c.why):
			t.Errorf("%s: got %v %q", c.name, p3, why)
		}
	}
}

// Load never runs a Preview 3 component; it says why, in an error a caller
// can recognise.
func TestLoadSaysAPreview3ComponentNeedsPreview3(t *testing.T) {
	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	defer rt.Close(ctx)
	_, err := Load(ctx, rt, componentOf(section(7, []byte{0x01, 0x66, 0x00})))
	if !errors.Is(err, ErrPreview3) || !strings.Contains(err.Error(), "streams or futures") {
		t.Fatalf("%v", err)
	}
}
