package trace

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// An item with more scalar leaves than the bound keeps the same leaves on
// every read; map order once decided which (found by TENG-3111's
// before/after comparison of real Codex history).
func TestResultCollectionsAreDeterministicPastTheLeafBound(t *testing.T) {
	// The first item sets the fields; a later item with more leaves than the
	// bound once lost a random subset of them.
	small, big := map[string]any{}, map[string]any{}
	for i := 0; i < 10; i++ {
		small[fmt.Sprintf("f%02d", i)] = fmt.Sprint(i)
	}
	for i := 0; i < 90; i++ {
		big[fmt.Sprintf("f%02d", i)] = fmt.Sprint(i)
	}
	b, _ := json.Marshal(map[string]any{"items": []any{small, big}})
	first := ResultCollections(string(b))
	for i := 0; i < 50; i++ {
		if got := ResultCollections(string(b)); !reflect.DeepEqual(got, first) {
			t.Fatalf("read %d differs", i)
		}
	}
	if len(first) != 1 || !strings.Contains(fmt.Sprint(first[0].Fields), ".f00") {
		t.Fatalf("collections %+v", first)
	}
}
