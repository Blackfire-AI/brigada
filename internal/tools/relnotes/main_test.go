package main

import "testing"

const log = `# Changelog

## [1.1.0] - 2026-01-01

### Added

- new

## [1.0.0-rc.1] - 2025-12-01

- rc note

## [1.0.0] - Unreleased

### Fixed

- fix
`

func TestSection(t *testing.T) {
	cases := []struct {
		tag, want string
	}{
		{"v1.1.0", "### Added\n\n- new"},
		{"v1.0.0-rc.1", "- rc note"},
		{"v1.0.0", "### Fixed\n\n- fix"},
	}
	for _, c := range cases {
		got, err := section(log, c.tag)
		if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.tag, got, err, c.want)
		}
	}
}

func TestSectionMissing(t *testing.T) {
	for _, tag := range []string{"v2.0.0", "v1.0.0-rc.2"} {
		if _, err := section(log, tag); err == nil {
			t.Errorf("%s: missing section: want error", tag)
		}
	}
	if _, err := section("## [1.0.0] - x\n\n## [0.9.0]\n- a\n", "v1.0.0"); err == nil {
		t.Error("empty section: want error")
	}
}
