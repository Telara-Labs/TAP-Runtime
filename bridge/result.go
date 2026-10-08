package bridge

import (
	"fmt"
	"strings"

	"github.com/Telara-Labs/TAP-Runtime/bind"
)

// callResult keeps a tool failure separate from successful result data. The
// host must not validate an error message against a successful-result contract.
func callResult(tool bind.Tool, result map[string]any) (string, error) {
	if failed, _ := result["isError"].(bool); failed {
		message := resultText(result)
		if content := result["content"]; content != nil {
			if text := resultText(map[string]any{"content": content}); strings.TrimSpace(text) != "" {
				message = text
			}
		}
		if strings.TrimSpace(message) == "" {
			message = "MCP tool returned an error"
		}
		return "", fmt.Errorf("%s/%s: %s", tool.Server, tool.Name, message)
	}
	return resultText(result), nil
}
