package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/logo"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
	"github.com/Blackfire-AI/brigada/internal/version"
)

// asTerminal makes run print as it would to a terminal with caps.
func asTerminal(t *testing.T, caps logo.Caps) {
	t.Helper()
	detect := detectCaps
	detectCaps = func(io.Writer) logo.Caps { return caps }
	t.Cleanup(func() { detectCaps = detect })
}

func runOut(t *testing.T, args ...string) string {
	t.Helper()
	var out, errb bytes.Buffer
	if code := run(context.Background(), args, &out, &errb); code != 0 || errb.Len() != 0 {
		t.Fatalf("run(%q): code=%d stdout=%q stderr=%q", args, code, out.String(), errb.String())
	}
	return out.String()
}

// Scripts read `brigada version` from a pipe: they get the one plain line,
// whatever color the environment offers.
func TestVersionPlainWhenNotATerminal(t *testing.T) {
	want := fmt.Sprintf("brigada %s\n", version.Version)
	if got := runOut(t, "version"); got != want {
		t.Errorf("version to a buffer = %q, want %q", got, want)
	}
	asTerminal(t, logo.Caps{Color: logo.TrueColor, UTF8: true, Width: 80})
	if got := runOut(t, "version"); got != want {
		t.Errorf("version with color but no terminal = %q, want %q", got, want)
	}
}

func TestVersionBannerOnATerminal(t *testing.T) {
	line := fmt.Sprintf("brigada %s\n", version.Version)
	for _, tc := range []struct {
		name string
		caps logo.Caps
		logo string
	}{
		{"truecolor", logo.Caps{TTY: true, Color: logo.TrueColor, UTF8: true, Width: 80}, "\x1b[48;2;255;90;31m▄\x1b[0m" + strings.TrimPrefix(logo.BannerText, "█")},
		{"NO_COLOR", logo.Caps{TTY: true, UTF8: true, Width: 80}, logo.BannerText},
		{"narrow", logo.Caps{TTY: true, UTF8: true, Width: 20}, logo.OneLineText},
		{"no UTF-8", logo.Caps{TTY: true, Width: 80}, logo.ASCIIText},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asTerminal(t, tc.caps)
			if got, want := runOut(t, "version"), tc.logo+"\n"+line; got != want {
				t.Errorf("version = %q, want %q", got, want)
			}
		})
	}
	t.Run("no logo fits", func(t *testing.T) {
		asTerminal(t, logo.Caps{TTY: true, Width: 20})
		if got := runOut(t, "version"); got != line {
			t.Errorf("version = %q, want %q", got, line)
		}
	})
}

// Interactive init on a terminal opens with the banner; --yes and --probe
// runs, and init to a pipe, print none.
func TestInitBanner(t *testing.T) {
	caps := logo.Caps{TTY: true, Color: logo.TrueColor, UTF8: true, Width: 80}
	banner := logo.String(logo.Banner, caps) + "\n"
	for _, tc := range []struct {
		name     string
		terminal bool
		args     []string
		want     bool
	}{
		{"interactive", true, nil, true},
		{"probe", true, []string{"--probe"}, false},
		{"yes", true, []string{"--yes"}, false},
		{"not a terminal", false, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importRepo(t)
			fakeInitPATH(t, initHarnesses...)
			if tc.terminal {
				asTerminal(t, caps)
			}
			state := filepath.Join(testsupport.ShortStateDir(t), "absent")
			args := append([]string{"--signed-in", "claude,codex,kimi,opencode", "--accept-limited", "--check", "true", "--tools", "none"}, tc.args...)
			_, out, _ := initDo(t, state, "-\n-\n-\n-\n", args...)
			if got := strings.HasPrefix(out, banner); got != tc.want {
				t.Errorf("starts with the banner = %v, want %v:\n%s", got, tc.want, out)
			}
			if !tc.want && strings.Contains(out, "\x1b[") {
				t.Errorf("escape codes without the banner:\n%q", out)
			}
		})
	}
}

var ansi = regexp.MustCompile("\x1b\\[[0-9;]*m")

// columns is the widest line of s in a terminal, with tabs at every
// eighth column and escape codes taking none.
func columns(s string) (int, string) {
	widest, line := 0, ""
	for _, l := range strings.Split(ansi.ReplaceAllString(s, ""), "\n") {
		n := 0
		for _, r := range l {
			if r == '\t' {
				n += 8 - n%8
			} else {
				n++
			}
		}
		if n > widest {
			widest, line = n, l
		}
	}
	return widest, line
}

func TestTopHelpHeader(t *testing.T) {
	asTerminal(t, logo.Caps{TTY: true, Color: logo.Color256, UTF8: true, Width: 80})
	header := "\x1b[38;5;202m▙\x1b[0m brigada\n\n"
	for _, args := range [][]string{{"help"}, {"-h"}, {"--help"}, {"-help"}} {
		out := runOut(t, args...)
		if out != header+usage {
			t.Errorf("%q = %q, want the one-line logo, a blank line and the usage", args, out)
		}
		if n, line := columns(out); n > 80 {
			t.Errorf("%q: a line is %d columns: %q", args, n, line)
		}
	}
	// A command's own help starts with its usage line, logo or not.
	if out := runOut(t, "version", "--help"); !strings.HasPrefix(out, "Usage: brigada version") {
		t.Errorf("version --help = %q", out)
	}
	// Usage errors go to stderr without the logo.
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"frobnicate"}, &out, &errb); code != 2 || strings.Contains(errb.String(), "▙") {
		t.Errorf("unknown command: code=%d stderr=%q", code, errb.String())
	}
}

func TestTopHelpWithoutLogo(t *testing.T) {
	if got := runOut(t, "help"); got != usage {
		t.Errorf("help to a buffer is not the plain usage: %q", got)
	}
	asTerminal(t, logo.Caps{TTY: true, Width: 80})
	if got := runOut(t, "help"); got != usage {
		t.Errorf("help without UTF-8 is not the plain usage: %q", got)
	}
}
