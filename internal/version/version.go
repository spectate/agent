// Package version carries build metadata, set via -ldflags at release time.
package version

import "runtime"

var (
	// Version is the agent release (e.g. "2.0.0").
	Version = "2.0.0-dev"
	// Commit is the git commit the binary was built from.
	Commit = "unknown"
	// BuildDate is the RFC 3339 build timestamp.
	BuildDate = "unknown"
	// DefaultAPIURL is the API base URL used when none is configured.
	DefaultAPIURL = "https://api.spectate.net"
)

// UserAgent identifies the agent to the API.
func UserAgent() string {
	return "spectate-agent/" + Version + " (" + runtime.GOOS + "/" + runtime.GOARCH + ")"
}
