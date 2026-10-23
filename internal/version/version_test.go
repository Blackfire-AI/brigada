package version

import (
	"fmt"
	"runtime/debug"
	"strings"
	"testing"
)

// rev returns a deterministic 40-character git revision.
func rev(n int) string { return fmt.Sprintf("%040x", n) }

// info builds a *debug.BuildInfo with a main module version and VCS
// settings, the way a real build records them.
func info(main string, settings ...debug.BuildSetting) *debug.BuildInfo {
	return &debug.BuildInfo{
		Main:     debug.Module{Version: main},
		Settings: settings,
	}
}

func TestResolve(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ldflags string
		info    *debug.BuildInfo
		want    string
	}{
		{
			name:    "ldflags set wins over everything",
			ldflags: "1.2.3",
			info: info("v1.2.3",
				debug.BuildSetting{Key: "vcs.revision", Value: rev(1)},
				debug.BuildSetting{Key: "vcs.modified", Value: "true"}),
			want: "1.2.3",
		},
		{
			name: "ldflags left at the default falls through to the tag",
			info: info("v1.2.3"),
			want: "1.2.3",
		},
		{
			name: "tagged release from go install",
			info: info("v1.0.0",
				debug.BuildSetting{Key: "vcs.revision", Value: rev(1)}),
			want: "1.0.0",
		},
		{
			name: "tagged prerelease release",
			info: info("v1.0.0-rc.1"),
			want: "1.0.0-rc.1",
		},
		{
			name: "tagged release without the leading v",
			info: info("1.0.0"),
			want: "1.0.0",
		},
		{
			name: "revision, clean tree",
			info: info("",
				debug.BuildSetting{Key: "vcs.revision", Value: rev(2)},
				debug.BuildSetting{Key: "vcs.modified", Value: "false"}),
			want: "0.0.0-dev+" + rev(2)[:12],
		},
		{
			name: "revision, dirty tree",
			info: info("",
				debug.BuildSetting{Key: "vcs.revision", Value: rev(3)},
				debug.BuildSetting{Key: "vcs.modified", Value: "true"}),
			want: "0.0.0-dev+" + rev(3)[:12] + modifiedMark,
		},
		{
			name: "revision longer than 12 characters is cut",
			info: info("",
				debug.BuildSetting{Key: "vcs.revision", Value: rev(4)}),
			want: "0.0.0-dev+" + rev(4)[:12],
		},
		{
			name: "pseudo-version falls back to the revision",
			info: info("v0.0.0-20261023000000-abcdefabcdef",
				debug.BuildSetting{Key: "vcs.revision", Value: rev(5)}),
			want: "0.0.0-dev+" + rev(5)[:12],
		},
		{
			name: "pseudo-version with no revision is the default",
			info: info("v0.0.0-20261023000000-abcdefabcdef"),
			want: defaultVersion,
		},
		{
			name: "pseudo-version of a tagged base falls back to the revision",
			info: info("v1.2.3-pre.0.20261023000000-abcdefabcdef",
				debug.BuildSetting{Key: "vcs.revision", Value: rev(6)},
				debug.BuildSetting{Key: "vcs.modified", Value: "true"}),
			want: "0.0.0-dev+" + rev(6)[:12] + modifiedMark,
		},
		{
			name: "(devel) with no revision is the default",
			info: info("(devel)"),
			want: defaultVersion,
		},
		{
			name: "nil build info is the default",
			info: nil,
			want: defaultVersion,
		},
		{
			name: "empty main version with no settings is the default",
			info: info(""),
			want: defaultVersion,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ldflags := tc.ldflags
			if ldflags == "" {
				ldflags = defaultVersion
			}
			if got := resolve(ldflags, tc.info); got != tc.want {
				t.Errorf("resolve(%q, main %q) = %q, want %q", ldflags, tc.info, got, tc.want)
			}
		})
	}
}

func TestIsRelease(t *testing.T) {
	for _, tc := range []struct {
		v    string
		want bool
	}{
		{"v1.0.0", true},
		{"1.0.0", true},
		{"v1.0.0-rc.1", true},
		{"v1.0.0+build.5", true},
		{"v1.0.0-20261023000000", true},
		{"(devel)", false},
		{"", false},
		{"v0.0.0-20261023000000-abcdefabcdef", false},
		{"v1.2.3-pre.0.20261023000000-abcdefabcdef", false},
		{"dev", false},
		{"v1", false},
		{"v1.2", false},
	} {
		if got := isRelease(tc.v); got != tc.want {
			t.Errorf("isRelease(%q) = %v, want %v", tc.v, got, tc.want)
		}
	}
}

func TestString(t *testing.T) {
	// String returns a non-empty version without whitespace, whatever the
	// binary's real build info is.
	s := String()
	if s == "" {
		t.Fatal("String is empty")
	}
	if strings.ContainsAny(s, " \t\n") {
		t.Fatalf("String %q contains whitespace", s)
	}
}
