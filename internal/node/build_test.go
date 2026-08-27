package node

import (
	"strings"
	"testing"
)

// Build has to answer something usable in every case, because the fleet view
// puts it on screen. It cannot return an empty string and it cannot panic on a
// binary that carries no VCS stamp.
func TestBuildAlwaysAnswers(t *testing.T) {
	got := Build()
	if got == "" {
		t.Fatal("Build returned an empty string")
	}
	if strings.ContainsAny(got, " \t\n") {
		t.Errorf("Build returned something with whitespace in it: %q", got)
	}
	// Under `go test` the revision is usually stamped; a short hash, or the
	// named fallback. Either is fine, anything long is not — this is read at a
	// glance beside a hostname.
	if len(got) > len("-dirty")+12 {
		t.Errorf("Build returned %q, which is too long to sit on a card", got)
	}
}

// An unrecorded build is not an out-of-date one, and two binaries that both
// failed to record a revision are not thereby the same binary. Getting this
// wrong in either direction puts a wrong answer on the fleet view: a host
// accused of being behind when nothing is known, or two different builds
// called identical.
func TestSameBuildRefusesToGuess(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b string
		want bool
	}{
		{"identical revisions", "abc123def456", "abc123def456", true},
		{"different revisions", "abc123def456", "999999999999", false},
		{"dirty is not its own commit", "abc123def456-dirty", "abc123def456", false},
		{"unknown against a revision", UnknownBuild, "abc123def456", false},
		{"a revision against unknown", "abc123def456", UnknownBuild, false},
		{"unknown against unknown", UnknownBuild, UnknownBuild, false},
		{"empty against a revision", "", "abc123def456", false},
		{"both empty", "", "", false},
		{"whitespace is trimmed, not meaningful", " abc123def456 ", "abc123def456", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SameBuild(tc.a, tc.b); got != tc.want {
				t.Errorf("SameBuild(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
