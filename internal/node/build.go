package node

import (
	"runtime/debug"
	"strings"
)

// UnknownBuild is what Build reports when there is nothing to report: a binary
// compiled outside a checkout, from an unpacked archive or a vendored tree.
// Named rather than left as a bare string so the browser can recognise it and
// say nothing instead of showing it.
const UnknownBuild = "unknown"

// Build identifies the binary this is compiled into.
//
// The agent used to report a hard-coded "1", with a comment saying it existed
// so that a fleet part-way through an upgrade could be seen to be part-way
// through an upgrade. A constant cannot do that: every agent ever built
// reported the same string, so the field was there and empty of information.
//
// Read from the build's own VCS stamp rather than set by a linker flag,
// because a flag is a thing every build path has to remember — update.sh,
// setup.sh, and whatever somebody types by hand — and the one that forgets
// produces a binary that lies about which commit it is. go build records the
// revision on its own when it runs inside a checkout.
//
// The server and the agent are built from the same tree on the same pass, so
// their answers match when a node is current. That is what lets the fleet view
// say which hosts are behind without a version registry to keep in step.
func Build() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return UnknownBuild
	}
	var revision string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if revision == "" {
		return UnknownBuild
	}
	// Short, because this is read at a glance in a table and the full hash
	// tells the reader nothing the first twelve characters do not.
	if len(revision) > 12 {
		revision = revision[:12]
	}
	// A build from a modified tree is not the commit it names, and on a fleet
	// that difference is the whole question.
	if dirty {
		return revision + "-dirty"
	}
	return revision
}

// SameBuild reports whether two build identities are known to be the same.
//
// Unknown is not equal to anything, itself included: two binaries that both
// failed to record a revision are not thereby the same binary, and saying they
// match would be an answer invented rather than found.
func SameBuild(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" || a == UnknownBuild || b == UnknownBuild {
		return false
	}
	return a == b
}
