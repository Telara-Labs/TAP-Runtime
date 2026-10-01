package discover

import (
	"testing"

	"gitlab.com/telara-labs/tap-runtime/discover/shellparse"
)

func TestTypeOf(t *testing.T) {
	cases := map[string]struct {
		w    shellparse.Word
		want string
	}{
		"flag":        {shellparse.Word{Text: "--context"}, SlotFlag},
		"url":         {shellparse.Word{Text: "https://x.dev/a"}, SlotURL},
		"quoted text": {shellparse.Word{Text: "fix the thing", Quoted: true}, SlotText},
		"number":      {shellparse.Word{Text: "30"}, SlotNumber},
		"duration":    {shellparse.Word{Text: "30s"}, SlotNumber},
		"path":        {shellparse.Word{Text: "./cmd/server"}, SlotPath},
		"file":        {shellparse.Word{Text: "main.go"}, SlotPath},
		"ticket":      {shellparse.Word{Text: "TENG-3054"}, SlotID},
		"sha":         {shellparse.Word{Text: "6cecfc9"}, SlotID},
		"word":        {shellparse.Word{Text: "status"}, SlotWord},
	}
	for name, c := range cases {
		if got := typeOf(c.w); got != c.want {
			t.Errorf("%s: typeOf(%q) = %s, want %s", name, c.w.Text, got, c.want)
		}
	}
}
func TestSubcommandSkipsFlagValues(t *testing.T) {
	for in, want := range map[string]string{
		"kubectl --context minikube get pods": "get",
		"git -C /x status":                    "status",
		"go test ./...":                       "test",
		"grep -rn foo .":                      "",
		"ls":                                  "",
	} {
		if got := subcommandOf(shellparse.SimpleCommands(in)[0]); got != want {
			t.Errorf("subcommandOf(%q) = %q, want %q", in, got, want)
		}
	}
}
