// Package version holds the version of the brigada binary.
package version

import (
	"runtime/debug"
	"strings"
)

// Version is reported by `brigada version`. It is overridden at build time
// with -ldflags "-X github.com/Blackfire-AI/brigada/internal/version.Version=…"
// (the release workflow sets it to the tag without its leading v); when it
// is still the default, [String] resolves the version from the build info.
var Version = "0.0.0-dev"

// defaultVersion is the unset value of Version and the fallback when the
// build carries no usable version information.
const defaultVersion = "0.0.0-dev"

// modifiedMark marks a version built from a dirty tree (vcs.modified true):
// 0.0.0-dev+<rev12>-modified.
const modifiedMark = "-modified"

// String returns the version this binary reports: the tag the release
// workflow set with -ldflags -X, else the tagged module version a
// `go install …@v1.0.0` recorded in the build info, else
// 0.0.0-dev+<first 12 characters of the VCS revision>, with modifiedMark
// appended when the tree was dirty, else 0.0.0-dev.
func String() string {
	info, _ := debug.ReadBuildInfo()
	return resolve(Version, info)
}

// resolve picks the version for an ldflags-set Version value and the
// binary's build info. It is pure, so tests pass *debug.BuildInfo values
// without a real build.
func resolve(ldflags string, info *debug.BuildInfo) string {
	if ldflags != "" && ldflags != defaultVersion {
		return ldflags
	}
	if info == nil {
		return defaultVersion
	}
	if isRelease(info.Main.Version) {
		// The release binary prints the tag without its leading v
		// (release.yml: "${GITHUB_REF_NAME#v}"), so a `go install …@v1.0.0`
		// build prints the same form.
		return strings.TrimPrefix(info.Main.Version, "v")
	}
	revision, modified := vcsState(info)
	if revision != "" {
		if len(revision) > 12 {
			revision = revision[:12]
		}
		v := defaultVersion + "+" + revision
		if modified {
			v += modifiedMark
		}
		return v
	}
	return defaultVersion
}

// isRelease reports whether v is a tagged module version in release form:
// an optional leading v, three dot-separated numbers, then optional
// prerelease and build parts. Both forms the release paths produce count:
// the release workflow strips the v (release.yml: "${GITHUB_REF_NAME#v}"),
// while `go install …@v1.0.0` records v1.0.0. Pseudo-versions
// (v0.0.0-20260102030405-abcdef, and the prerelease forms with a timestamp)
// and the `(devel)` of a plain `go build` are not release form.
func isRelease(v string) bool {
	if v == "" || v == "(devel)" {
		return false
	}
	core := strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(core, "-+"); i >= 0 {
		core = core[:i]
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	// A pseudo-version carries the 14-digit timestamp (yyyymmddhhmmss)
	// followed by its 12-hex commit hash, whatever base it grew from:
	// v0.0.0-<time>-<hash>, v1.2.3-0.<time>-<hash>, v1.2.3-pre.0.<time>-<hash>.
	// The core version numbers alone cannot form that pair.
	return !hasPseudoVersion(v)
}

// hasPseudoVersion reports whether v contains the timestamp-plus-hash pair
// every Go pseudo-version ends with.
func hasPseudoVersion(v string) bool {
	parts := strings.FieldsFunc(v, func(r rune) bool { return r == '-' || r == '.' })
	for i := range parts {
		if isPseudoTail(parts, i) {
			return true
		}
	}
	return false
}

// isPseudoTail reports whether parts[i] is the 14-digit timestamp of a Go
// pseudo-version and parts[i+1] its 12-hex commit hash.
func isPseudoTail(parts []string, i int) bool {
	if i+1 >= len(parts) {
		return false
	}
	ts, hash := parts[i], parts[i+1]
	if len(ts) != 14 {
		return false
	}
	for _, r := range ts {
		if r < '0' || r > '9' {
			return false
		}
	}
	if len(hash) != 12 {
		return false
	}
	for _, r := range hash {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// vcsState returns the commit revision and dirty flag from the build
// info's VCS settings, "" and false when the build carries none.
func vcsState(info *debug.BuildInfo) (revision string, modified bool) {
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	return revision, modified
}
