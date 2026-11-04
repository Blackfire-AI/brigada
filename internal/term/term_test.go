package term

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/logo"
)

func TestStylesAtEveryDepth(t *testing.T) {
	for _, tc := range []struct {
		name    string
		style   func(Style, string) string
		escapes [4]string
	}{
		{"Bold", Style.Bold, [4]string{"", "\x1b[1m", "\x1b[1m", "\x1b[1m"}},
		{"Dim", Style.Dim, [4]string{"", "\x1b[90m", "\x1b[38;5;245m", "\x1b[38;2;138;143;152m"}},
		{"Accent", Style.Accent, [4]string{"", "\x1b[36m", "\x1b[38;5;80m", "\x1b[38;2;108;198;224m"}},
		{"Teal", Style.Teal, [4]string{"", "\x1b[96m", "\x1b[38;5;37m", "\x1b[38;2;0;175;175m"}},
		{"OK", Style.OK, [4]string{"", "\x1b[32m", "\x1b[38;5;77m", "\x1b[38;2;95;215;95m"}},
		{"Warn", Style.Warn, [4]string{"", "\x1b[33m", "\x1b[38;5;221m", "\x1b[38;2;255;215;95m"}},
		{"Bad", Style.Bad, [4]string{"", "\x1b[31m", "\x1b[38;5;203m", "\x1b[38;2;255;95;95m"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, depth := range []logo.Depth{logo.NoColor, logo.Color16, logo.Color256, logo.TrueColor} {
				style := New(logo.Caps{TTY: true, Color: depth, UTF8: true, Width: 90})
				for _, text := range []string{"hello ✓", "", "line one\nline two", "  spaces\t"} {
					want := text
					if depth != logo.NoColor {
						want = tc.escapes[depth] + text + "\x1b[0m"
					}
					if got := tc.style(style, text); got != want {
						t.Errorf("depth %d, text %q: got %q, want %q", depth, text, got, want)
					}
				}
			}
		})
	}
}

func assertPlain(t *testing.T, style Style) {
	t.Helper()
	const text = "unchanged ✓\n  text\t"
	for name, fn := range map[string]func(string) string{
		"Bold": style.Bold, "Dim": style.Dim, "Accent": style.Accent,
		"OK": style.OK, "Warn": style.Warn, "Bad": style.Bad,
	} {
		if got := fn(text); got != text {
			t.Errorf("%s = %q, want unchanged %q", name, got, text)
		}
	}
}

func TestPlainOutput(t *testing.T) {
	assertPlain(t, Style{})
	// logo.Detect owns the environment policy; styles consume its NoColor
	// result, whether the terminal retains UTF-8 support or falls back to ASCII.
	for _, caps := range []logo.Caps{
		{TTY: true, Color: logo.NoColor, UTF8: true, Width: 90}, // NO_COLOR
		{Color: logo.NoColor, UTF8: true},                       // non-TTY
		{TTY: true, Color: logo.NoColor, Width: 90},             // TERM=dumb
	} {
		assertPlain(t, New(caps))
	}
}

func TestDetectedPlainOutput(t *testing.T) {
	file, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	for _, tc := range []struct {
		name, noColor, term string
	}{
		{"non-TTY", "", "xterm-256color"},
		{"NO_COLOR", "1", "xterm-256color"},
		{"TERM=dumb", "", "dumb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NO_COLOR", tc.noColor)
			t.Setenv("TERM", tc.term)
			t.Setenv("COLORTERM", "truecolor")
			t.Setenv("LC_ALL", "en_US.UTF-8")
			t.Setenv("COLUMNS", "120")
			for _, caps := range []logo.Caps{logo.Detect(file), logo.Detect(&strings.Builder{})} {
				style := New(caps)
				assertPlain(t, style)
				if got := style.Width(); got != 80 {
					t.Errorf("non-TTY width = %d, want 80", got)
				}
				if tc.term == "dumb" && style.Check() != "ok" {
					t.Errorf("TERM=dumb check = %q, want ASCII", style.Check())
				}
			}
		})
	}
}

func TestSymbols(t *testing.T) {
	for _, tc := range []struct {
		name           string
		symbol         func(Style) string
		unicode, ascii string
	}{
		{"Check", Style.Check, "✓", "ok"},
		{"Question", Style.Question, "?", "?"},
		{"Warning", Style.Warning, "!", "!"},
		{"Pointer", Style.Pointer, "▸", ">"},
		{"Rule", Style.Rule, "━━", "=="},
		{"Again", Style.Again, "↺", "->"},
		{"Filled", Style.Filled, "■", "#"},
		{"Empty", Style.Empty, "□", "."},
		{"Dash", Style.Dash, "─", "-"},
		{"Cross", Style.Cross, "✗", "x"},
		{"Dot", Style.Dot, "·", "|"},
		{"AtMost", Style.AtMost, "≤", "<="},
		{"Up", Style.Up, "⇡", "^"},
		{"Arrow", Style.Arrow, "→", "->"},
		{"Meter", Style.Meter, "━", "="},
		{"BoxTL", Style.BoxTL, "╭", "+"},
		{"BoxTR", Style.BoxTR, "╮", "+"},
		{"BoxBL", Style.BoxBL, "╰", "+"},
		{"BoxBR", Style.BoxBR, "╯", "+"},
		{"BoxSide", Style.BoxSide, "│", "|"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, depth := range []logo.Depth{logo.NoColor, logo.Color16, logo.Color256, logo.TrueColor} {
				for _, utf8 := range []bool{false, true} {
					style := New(logo.Caps{TTY: true, Color: depth, UTF8: utf8})
					want := tc.ascii
					if utf8 {
						want = tc.unicode
					}
					if got := tc.symbol(style); got != want {
						t.Errorf("depth %d, UTF8 %t: got %q, want %q", depth, utf8, got, want)
					}
				}
			}
		})
	}
}

// TestOneLine: the brand's one-line mark with the flame colour on the ▙
// alone at each depth, bare with NoColor, empty without UTF-8.
func TestOneLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps logo.Caps
		want string
	}{
		{"true colour", logo.Caps{Color: logo.TrueColor, UTF8: true}, "\x1b[38;2;255;90;31m▙\x1b[0m brigada"},
		{"256 colours", logo.Caps{Color: logo.Color256, UTF8: true}, "\x1b[38;5;202m▙\x1b[0m brigada"},
		{"16 colours", logo.Caps{Color: logo.Color16, UTF8: true}, "\x1b[31m▙\x1b[0m brigada"},
		{"no colour", logo.Caps{UTF8: true}, "▙ brigada"},
		{"no UTF-8", logo.Caps{Color: logo.TrueColor}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := New(tc.caps).OneLine(); got != tc.want {
				t.Errorf("OneLine() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTints: rows are tinted at 256 colours and true colour only, the
// background and foreground opened again after every inner reset, the text
// unchanged once the escapes go; at 16 colours a needs row is Bad and a
// running row plain, with no colour both are plain.
func TestTints(t *testing.T) {
	for _, tc := range []struct {
		name  string
		depth logo.Depth
		fn    func(Style, string) string
		open  string
	}{
		{"needs 256", logo.Color256, Style.TintNeeds, "\x1b[48;5;52m\x1b[38;5;224m"},
		{"needs true", logo.TrueColor, Style.TintNeeds, "\x1b[48;2;59;20;24m\x1b[38;2;255;215;215m"},
		{"run 256", logo.Color256, Style.TintRun, "\x1b[48;5;17m\x1b[38;5;153m"},
		{"run true", logo.TrueColor, Style.TintRun, "\x1b[48;2;18;35;63m\x1b[38;2;215;227;255m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			style := New(logo.Caps{Color: tc.depth, UTF8: true})
			text := style.Accent("▸") + " job"
			got := tc.fn(style, text)
			if !strings.HasPrefix(got, tc.open) || !strings.HasSuffix(got, "\x1b[0m") {
				t.Errorf("tint = %q, want it opened with %q and closed", got, tc.open)
			}
			// Every reset of the inner span is followed by the open again.
			if n, m := strings.Count(got, "\x1b[0m"), strings.Count(got, tc.open); m != n {
				t.Errorf("%d resets but %d opens in %q", n, m, got)
			}
			if !strings.Contains(got, "\x1b[0m"+tc.open+" job") {
				t.Errorf("the background does not survive the inner span: %q", got)
			}
			if StripEscapes(got) != "▸ job" {
				t.Errorf("stripped tint = %q", StripEscapes(got))
			}
		})
	}
	for _, depth := range []logo.Depth{logo.NoColor, logo.Color16} {
		style := New(logo.Caps{Color: depth, UTF8: true})
		if got := style.TintRun("row"); got != "row" {
			t.Errorf("depth %d: run row = %q, want plain", depth, got)
		}
		if got := style.TintNeeds("row"); got != style.Bad("row") {
			t.Errorf("depth %d: needs row = %q, want Bad", depth, got)
		}
	}
}

func TestWidth(t *testing.T) {
	for _, tc := range []struct {
		width, want int
	}{
		{-1, 80}, {0, 80}, {1, 80}, {79, 80}, {80, 80},
		{81, 81}, {90, 90}, {99, 99}, {100, 100}, {101, 100}, {200, 100},
	} {
		for _, tty := range []bool{false, true} {
			want := 80
			if tty {
				want = tc.want
			}
			if got := New(logo.Caps{TTY: tty, Width: tc.width}).Width(); got != want {
				t.Errorf("TTY %t, width %d: got %d, want %d", tty, tc.width, got, want)
			}
		}
	}
}

func TestVisibleWidth(t *testing.T) {
	style := New(logo.Caps{TTY: true, Color: logo.TrueColor, UTF8: true})
	for _, tc := range []struct {
		name, text string
		want       int
	}{
		{"empty", "", 0},
		{"plain ASCII", "hello", 5},
		{"runes, not bytes", "✓ café ▸", 8},
		{"wide glyph counts as one rune", "界", 1},
		{"combining rune counts separately", "e\u0301", 2},
		{"bold", style.Bold("hello"), 5},
		{"grey", style.Dim("hello"), 5},
		{"cyan", style.Accent("hello"), 5},
		{"green", style.OK("✓"), 1},
		{"yellow", style.Warn("!"), 1},
		{"red", style.Bad("bad"), 3},
		{"adjacent styles", style.Bold("one") + " " + style.Accent("two"), 7},
		{"nested styles", style.Bold(style.Accent("hello")), 5},
		{"escapes only", "\x1b[1m\x1b[0m", 0},
		{"256 colors", "\x1b[38;5;245mgrey\x1b[0m", 4},
		{"16 colors", "\x1b[36mcyan\x1b[0m", 4},
		{"CSI controls", "\x1b[2Khello", 5},
		{"OSC hyperlink with BEL", "\x1b]8;;https://example.com\ahello\x1b]8;;\a", 5},
		{"OSC hyperlink with ST", "\x1b]8;;https://example.com\x1b\\hello\x1b]8;;\x1b\\", 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := VisibleWidth(tc.text); got != tc.want {
				t.Errorf("VisibleWidth(%q) = %d, want %d", tc.text, got, tc.want)
			}
		})
	}
}
