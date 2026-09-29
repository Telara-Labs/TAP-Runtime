// Package buildbox runs a command an author wrote where it can do no harm.
//
// A compiled primitive is checked by building its source again (package
// rebuild), and the build command is the author's. A service that holds
// credentials cannot run that beside them. A Box runs it in namespaces of its
// own, inside the same container, with:
//
//   - no network: a new network namespace, in which only a loopback that is
//     down exists. The service's own ports and its Vault agent are not there;
//   - no files of the service: the root is replaced by a toolchain directory,
//     mounted read-only, so the container's keys and tokens are not in the
//     tree at all. The package is the one directory that can be written;
//   - no identity: the command is root only inside a user namespace, where
//     that maps to the unprivileged user the service runs as;
//   - no environment of the service: what the command is given is listed
//     here, in full;
//   - limits on time, memory, processes and file size.
//
// It needs no privilege, no second container and no daemon. It needs a
// kernel that lets an unprivileged process create a user namespace. Where
// that is refused Available says so, and nothing is built.
//
// Linux only. On anything else Available reports why not.
package buildbox

import "time"

// Box is where and within what a command runs.
type Box struct {
	// Helper is the tap-buildbox program.
	Helper string
	// Root is a directory holding the toolchain: what the command sees as /.
	Root string
	// Timeout is how long the command may take. Zero means DefaultTimeout.
	Timeout time.Duration
	// MemoryBytes bounds the address space of each process. Zero means
	// DefaultMemory.
	MemoryBytes uint64
}

const (
	DefaultTimeout = 4 * time.Minute
	DefaultMemory  = 3 << 30 // address space, not resident memory: Go reserves far more than it uses
	maxProcesses   = 256
	maxFileBytes   = 256 << 20
	// WorkDir is where the package is, inside the box.
	WorkDir = "/work"
)

// Env is everything the command is given. Nothing of the caller's
// environment is passed on.
func Env() []string {
	return []string{
		"PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=" + WorkDir + "/.home",
		"TMPDIR=/tmp",
		"GOCACHE=/gocache",
		"GOMODCACHE=" + WorkDir + "/.home/mod",
		"GOPATH=" + WorkDir + "/.home/go",
		"GOFLAGS=-mod=mod",
		"GOWORK=off",
		"GOPROXY=off", // there is no network; say so at once instead of timing out
		"GOSUMDB=off",
		"GOTOOLCHAIN=local",
		// Said outright, because without a /proc the go command cannot
		// find itself to work it out.
		"GOROOT=/usr/local/go",
		"CGO_ENABLED=0",
	}
}
