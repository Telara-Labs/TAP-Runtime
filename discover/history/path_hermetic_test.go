package history

import (
	"os"
	"os/exec"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/discover/client"
)

// Detection also counts an agent's program on PATH; tests say what is
// installed, not the machine running them.
func TestMain(m *testing.M) {
	client.LookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	os.Exit(m.Run())
}
