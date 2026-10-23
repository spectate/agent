// Package version carries build metadata, set via -ldflags at release time.
package version

import (
	"runtime"
	"strconv"
	"strings"
)

var (
	// Version is the agent release (e.g. "2.0.0").
	Version = "2.0.0-dev"
	// Commit is the git commit the binary was built from.
	Commit = "unknown"
	// BuildDate is the RFC 3339 build timestamp.
	BuildDate = "unknown"
	// DefaultAPIURL is the API base URL used when none is configured.
	DefaultAPIURL = "https://api.spectate.net"
	// UpdatePublicKey is the base64 ed25519 public key that signs release
	// checksums (`spectate-agent update` verifies checksums.txt.sig with it).
	// Empty in development builds.
	UpdatePublicKey = ""
)

// UserAgent identifies the agent to the API.
func UserAgent() string {
	return "spectate-agent/" + Version + " (" + runtime.GOOS + "/" + runtime.GOARCH + ")"
}

// IsDev reports whether this is a development build (version "x.y.z-dev…").
// Release builds never carry "-dev".
func IsDev() bool { return strings.Contains(Version, "-dev") }

// Semver is a parsed MAJOR.MINOR.PATCH[-PRERELEASE] version (build metadata
// is ignored).
type Semver struct {
	Major, Minor, Patch int
	Pre                 string
}

// Parse parses "2.1.0", "v2.1.0" or "2.1.0-rc.1".
func Parse(s string) (Semver, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var v Semver
	core, pre, hasPre := strings.Cut(s, "-")
	if hasPre {
		if pre == "" {
			return Semver{}, false
		}
		v.Pre = pre
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Semver{}, false
	}
	nums := [3]*int{&v.Major, &v.Minor, &v.Patch}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" || (len(p) > 1 && p[0] == '0') {
			return Semver{}, false
		}
		*nums[i] = n
	}
	return v, true
}

// Compare returns -1, 0 or +1 as a is older than, equal to or newer than b
// (semver precedence; a pre-release is older than its release). Unparsable
// versions sort before parsable ones.
func Compare(a, b string) int {
	va, okA := Parse(a)
	vb, okB := Parse(b)
	switch {
	case !okA && !okB:
		return 0
	case !okA:
		return -1
	case !okB:
		return 1
	}
	for _, d := range [3]int{va.Major - vb.Major, va.Minor - vb.Minor, va.Patch - vb.Patch} {
		if d != 0 {
			return sign(d)
		}
	}
	return comparePre(va.Pre, vb.Pre)
}

func comparePre(a, b string) int {
	switch {
	case a == b:
		return 0
	case a == "":
		return 1
	case b == "":
		return -1
	}
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		na, errA := strconv.Atoi(pa[i])
		nb, errB := strconv.Atoi(pb[i])
		switch {
		case errA == nil && errB == nil:
			if na != nb {
				return sign(na - nb)
			}
		case errA == nil: // numeric identifiers sort before alphanumeric ones
			return -1
		case errB == nil:
			return 1
		default:
			if c := strings.Compare(pa[i], pb[i]); c != 0 {
				return c
			}
		}
	}
	return sign(len(pa) - len(pb))
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}
