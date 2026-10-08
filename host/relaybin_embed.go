//go:build relayembed && !windows

package main

import _ "embed"

// relayBinary is the small relay (cmd/tap-relay) for this platform, built
// into releases: go build -tags relayembed, with an -overlay that supplies
// relaybin/tap-relay (release.buildRelay); the file is never in the source.
// tap serve becomes it, so a session a client keeps open holds a few
// megabytes instead of the whole runner.
//
//go:embed relaybin/tap-relay
var relayBinary []byte
