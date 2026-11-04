package term

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/logo"
)

func TestCellWidth(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       int
	}{
		{"ascii", "brigada", 7},
		{"empty", "", 0},
		{"cjk", "日本語", 6},
		{"hangul", "한글", 4},
		{"fullwidth", "ＡＢ１", 6},
		{"fullwidth yen", "￥", 2},
		{"emoji", "🚀", 2},
		{"emoji in text", "ok ✅", 5},
		{"combining", "é", 1},
		{"combining after cjk", "日́", 2},
		{"variation selector", "️", 0},
		{"box glyphs", "╭─│╮╰╯", 6},
		{"block glyphs", "■□━▸↺≤…", 7},
		{"escapes", "\x1b[1m日本\x1b[0m", 4},
		{"mixed", "a日b🚀", 6},
	} {
		if got := CellWidth(tc.text); got != tc.want {
			t.Errorf("%s: CellWidth(%q) = %d, want %d", tc.name, tc.text, got, tc.want)
		}
	}
	if got := VisibleWidth("日本"); got != 2 {
		t.Errorf("VisibleWidth counts runes: got %d, want 2", got)
	}
}

func TestCellTablesAreSorted(t *testing.T) {
	for name, table := range map[string][]cellRange{"zero": zeroCells, "wide": wideCells} {
		for i, r := range table {
			if r.lo > r.hi || (i > 0 && r.lo <= table[i-1].hi) {
				t.Errorf("%s table entry %d (%U-%U) is out of order", name, i, r.lo, r.hi)
			}
		}
	}
}

func TestStylesAtEveryDepth(t *testing.T) {
	for _, tc := range []struct {
		name    string
		style   func(Style, string) string
		escapes [4]string
	}{
		{"Bold", Style.Bold, [4]string{"", "\x1b[1m", "\x1b[1m", "\x1b[1m"}},
		{"Dim", Style.Dim, [4]string{"", "\x1b[90m", "\x1b[38;5;245m", "\x1b[38;2;138;143;152m"}},
		{"Accent", Style.Accent, [4]string{"", "\x1b[36m", "\x1b[38;5;80m", "\x1b[38;2;108;198;224m"}},
		{"OK", Style.OK, [4]string{"", "\x1b[32m", "\x1b[38;5;77m", "\x1b[38;2;95;215;95m"}},
		{"Warn", Style.Warn, [4]string{"", "\x1b[33m", "\x1b[38;5;221m", "\x1b[38;2;255;215;95m"}},
		{"Bad", Style.Bad, [4]string{"", "\x1b[31m", "\x1b[38;5;203m", "\x1b[38;2;255;95;95m"}},
		{"Teal", Style.Teal, [4]string{"", "\x1b[96m", "\x1b[38;5;37m", "\x1b[38;2;0;175;175m"}},
		{"Violet", Style.Violet, [4]string{"", "\x1b[95m", "\x1b[38;5;141m", "\x1b[38;2;177;151;255m"}},
		{"Blue", Style.Blue, [4]string{"", "\x1b[94m", "\x1b[38;5;69m", "\x1b[38;2;95;135;255m"}},
		{"Mint", Style.Mint, [4]string{"", "\x1b[92m", "\x1b[38;5;85m", "\x1b[38;2;110;231;183m"}},
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
		"OK": style.OK, "Warn": style.Warn, "Bad": style.Bad, "Teal": style.Teal,
		"Violet": style.Violet, "Blue": style.Blue, "Mint": style.Mint,
		"TintNeeds": style.TintNeeds, "TintRun": style.TintRun,
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
		{"Bullet", Style.Bullet, "●", "*"},
		{"Filled", Style.Filled, "■", "#"},
		{"Empty", Style.Empty, "□", "."},
		{"Dash", Style.Dash, "─", "-"},
		{"Cross", Style.Cross, "✗", "x"},
		{"Dot", Style.Dot, "·", "|"},
		{"AtMost", Style.AtMost, "≤", "<="},
		{"Up", Style.Up, "⇡", "^"},
		{"Arrow", Style.Arrow, "→", "->"},
		{"Meter", Style.Meter, "━", "="},
		{"BoxTopLeft", Style.BoxTopLeft, "╭", "+"},
		{"BoxTopRight", Style.BoxTopRight, "╮", "+"},
		{"BoxBottomLeft", Style.BoxBottomLeft, "╰", "+"},
		{"BoxBottomRight", Style.BoxBottomRight, "╯", "+"},
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

// TestOneLine: the brand's one-line form at each depth, only the ▙ coloured
// and the escape closed; plain with no colour; empty without UTF-8.
func TestOneLine(t *testing.T) {
	for _, tc := range []struct {
		caps logo.Caps
		want string
	}{
		{logo.Caps{Color: logo.TrueColor, UTF8: true}, "\x1b[38;2;255;90;31m▙\x1b[0m brigada"},
		{logo.Caps{Color: logo.Color256, UTF8: true}, "\x1b[38;5;202m▙\x1b[0m brigada"},
		{logo.Caps{Color: logo.Color16, UTF8: true}, "\x1b[31m▙\x1b[0m brigada"},
		{logo.Caps{Color: logo.NoColor, UTF8: true}, "▙ brigada"},
		{logo.Caps{Color: logo.TrueColor}, ""},
		{logo.Caps{}, ""},
	} {
		if got := New(tc.caps).OneLine(); got != tc.want {
			t.Errorf("caps %+v: OneLine = %q, want %q", tc.caps, got, tc.want)
		}
	}
}

// TestTint: both colours open the row at 256 colours and true colour, the
// background survives an inner styled span, the escapes strip to the plain
// text, and 16 colours and no colour leave the text alone.
func TestTint(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tint  func(Style, string) string
		depth logo.Depth
		open  string
	}{
		{"needs 256", Style.TintNeeds, logo.Color256, "\x1b[48;5;52m\x1b[38;5;224m"},
		{"needs true", Style.TintNeeds, logo.TrueColor, "\x1b[48;2;59;20;24m\x1b[38;2;255;215;215m"},
		{"run 256", Style.TintRun, logo.Color256, "\x1b[48;5;17m\x1b[38;5;153m"},
		{"run true", Style.TintRun, logo.TrueColor, "\x1b[48;2;18;35;63m\x1b[38;2;215;227;255m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			style := New(logo.Caps{TTY: true, Color: tc.depth, UTF8: true})
			text := "a " + style.Bad("b") + " c"
			got := tc.tint(style, text)
			if !strings.HasPrefix(got, tc.open) || !strings.HasSuffix(got, "\x1b[0m") {
				t.Errorf("got %q, want it opened by %q and closed by a reset", got, tc.open)
			}
			if n := strings.Count(got, tc.open); n != 2 {
				t.Errorf("%q opens the tint %d times, want 2 (start and after the inner reset)", got, n)
			}
			if !strings.Contains(got, "\x1b[0m"+tc.open+" c") {
				t.Errorf("%q does not restore the tint after the inner reset", got)
			}
			if StripEscapes(got) != "a b c" {
				t.Errorf("StripEscapes(%q) = %q", got, StripEscapes(got))
			}
		})
	}
	for _, depth := range []logo.Depth{logo.NoColor, logo.Color16} {
		style := New(logo.Caps{TTY: true, Color: depth, UTF8: true})
		if got := style.TintNeeds("row"); got != "row" {
			t.Errorf("depth %d: TintNeeds = %q, want unchanged", depth, got)
		}
		if got := style.TintRun("row"); got != "row" {
			t.Errorf("depth %d: TintRun = %q, want unchanged", depth, got)
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

// TestBands: the error and paused bands paint dark text on a solid colour at
// 16 colours, 256 colours and true colour, keep the background through a Bold
// span and are plain text without colour.
func TestBands(t *testing.T) {
	for _, tc := range []struct {
		name  string
		paint func(Style, string) string
		open  [4]string
	}{
		{"BandBad", Style.BandBad, [4]string{"", "41;30", "48;5;203;38;5;16", "48;2;255;95;95;38;2;17;17;17"}},
		{"BandWarn", Style.BandWarn, [4]string{"", "43;30", "48;5;221;38;5;16", "48;2;255;215;95;38;2;17;17;17"}},
	} {
		for _, depth := range []logo.Depth{logo.NoColor, logo.Color16, logo.Color256, logo.TrueColor} {
			style := New(logo.Caps{Color: depth, UTF8: true})
			in := "a " + style.Bold("b") + " c"
			got := tc.paint(style, in)
			if depth == logo.NoColor {
				if got != "a b c" {
					t.Errorf("%s with no colour = %q, want plain", tc.name, got)
				}
				continue
			}
			open := "\x1b[" + tc.open[depth] + "m"
			if !strings.HasPrefix(got, open) || !strings.HasSuffix(got, "\x1b[0m") {
				t.Errorf("%s depth %d = %q, want the opening %q and a reset", tc.name, depth, got, open)
			}
			if strings.Count(got, "\x1b[0m"+open) != 1 {
				t.Errorf("%s depth %d = %q: the reset inside lost the band", tc.name, depth, got)
			}
			if StripEscapes(got) != "a b c" {
				t.Errorf("%s depth %d text = %q", tc.name, depth, StripEscapes(got))
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
