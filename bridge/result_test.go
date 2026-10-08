package bridge

import (
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/bind"
)

func TestMCPFailurePreservesActionableMessageInsteadOfSuccessData(t *testing.T) {
	tool := bind.Tool{Server: "gateway", Name: "execute_action"}
	for _, structured := range []any{nil, map[string]any{"status": "error"}} {
		r := map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "Action failed: organization scope does not authorize draft creation"}}}
		if structured != nil {
			r["structuredContent"] = structured
		}
		data, err := callResult(tool, r)
		if data != "" || err == nil || !strings.Contains(err.Error(), "scope does not authorize draft creation") {
			t.Fatalf("failed MCP call became success data: data=%q err=%v", data, err)
		}
	}
}

func TestMCPStructuredSuccessStillPreserved(t *testing.T) {
	data, err := callResult(bind.Tool{}, map[string]any{"isError": false, "structuredContent": map[string]any{"id": "draft-1"}, "content": []any{map[string]any{"type": "text", "text": "Action completed"}}})
	if err != nil || data != `{"id":"draft-1"}` {
		t.Fatalf("data=%q err=%v", data, err)
	}
}
