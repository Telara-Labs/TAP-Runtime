package bridge

import (
	"flag"
	"testing"
)

var liveClientTests = flag.Bool("live-clients", false, "opt in to tests that start installed agent clients or use configured accounts")

func requireLiveClientTests(t *testing.T) {
	t.Helper()
	if !*liveClientTests {
		t.Skip("requires installed agent clients or configured accounts; pass -live-clients to opt in")
	}
}
