// Package version holds the version of the brigada binary.
package version

// Version is reported by `brigada version`. It is overridden at build time
// with -ldflags "-X github.com/Blackfire-AI/brigada/internal/version.Version=…".
var Version = "0.0.0-dev"
