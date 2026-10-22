package logo

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// The constants are copies of docs/brand/cli; they must not drift.
func TestTextsMatchBrandFiles(t *testing.T) {
	root := moduleRoot(t)
	for file, text := range map[string]string{
		"banner.txt":  BannerText,
		"mark.txt":    MarkText,
		"oneline.txt": OneLineText,
		"ascii.txt":   ASCIIText,
	} {
		b, err := os.ReadFile(filepath.Join(root, "docs", "brand", "cli", file))
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != text {
			t.Errorf("%s differs from its constant:\nfile:\n%q\nconstant:\n%q", file, b, text)
		}
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test directory")
		}
		dir = parent
	}
}

var escape = regexp.MustCompile("\x1b\\[[0-9;]*m")

// columns is the widest line of s in terminal columns: every glyph the
// logo uses is one column wide, and escape codes take none.
func columns(s string) int {
	widest := 0
	for _, line := range strings.Split(escape.ReplaceAllString(s, ""), "\n") {
		widest = max(widest, utf8.RuneCountInString(line))
	}
	return widest
}

func TestStatedWidths(t *testing.T) {
	for _, tc := range []struct {
		name  string
		text  string
		width int
	}{
		{"banner", BannerText, BannerWidth},
		{"mark", MarkText, MarkWidth},
		{"oneline", OneLineText, OneLineWidth},
		{"ascii", ASCIIText, ASCIIWidth},
	} {
		if got := columns(tc.text); got != tc.width {
			t.Errorf("%s is %d columns wide, stated %d", tc.name, got, tc.width)
		}
	}
}

// Every form, at every depth and width, fits the terminal it is printed to.
func TestOutputFitsWidth(t *testing.T) {
	for _, form := range []Form{Banner, Mark, OneLine} {
		for _, depth := range []Depth{NoColor, Color16, Color256, TrueColor} {
			for _, utf := range []bool{true, false} {
				for width := OneLineWidth; width <= 100; width++ {
					caps := Caps{TTY: true, Color: depth, UTF8: utf, Width: width}
					out := String(form, caps)
					if got := columns(out); got > width {
						t.Fatalf("form %d at %+v is %d columns:\n%s", form, caps, got, out)
					}
					if out != "" && !strings.HasSuffix(out, "\n") {
						t.Fatalf("form %d at %+v does not end in a newline: %q", form, caps, out)
					}
				}
			}
		}
	}
}

func TestFlameCore(t *testing.T) {
	// The core is ▀ on line 2, three blanks on line 3 and ▄▄▄ on line 4,
	// each on the hi-vis background; nothing else is colored.
	paint := func(bg string) string {
		lines := strings.Split(BannerText, "\n")
		lines[2] = "██▄" + bg + "▀" + "\x1b[0m" + "███" + strings.TrimPrefix(lines[2], "██▄▀███")
		lines[3] = "██" + bg + "   " + "\x1b[0m" + "██" + strings.TrimPrefix(lines[3], "██   ██")
		lines[4] = "▀█" + bg + "▄▄▄" + "\x1b[0m" + "█▀" + strings.TrimPrefix(lines[4], "▀█▄▄▄█▀")
		return strings.Join(lines, "\n")
	}
	for depth, want := range map[Depth]string{
		TrueColor: paint("\x1b[48;2;255;90;31m"),
		Color256:  paint("\x1b[48;5;202m"),
		Color16:   paint("\x1b[41m"),
		NoColor:   BannerText,
	} {
		if got := String(Banner, Caps{Color: depth, UTF8: true, Width: 80}); got != want {
			t.Errorf("banner at depth %d = %q, want %q", depth, got, want)
		}
	}
	mark := String(Mark, Caps{Color: Color256, UTF8: true})
	if got := escape.ReplaceAllString(mark, ""); got != MarkText {
		t.Errorf("mark without escapes = %q, want %q", got, MarkText)
	}
	if got := strings.Count(mark, "\x1b[48;5;202m"); got != 3 {
		t.Errorf("mark paints %d core spans, want 3: %q", got, mark)
	}
	for depth, want := range map[Depth]string{
		TrueColor: "\x1b[38;2;255;90;31m▙\x1b[0m brigada\n",
		Color256:  "\x1b[38;5;202m▙\x1b[0m brigada\n",
		Color16:   "\x1b[31m▙\x1b[0m brigada\n",
		NoColor:   OneLineText,
	} {
		if got := String(OneLine, Caps{Color: depth, UTF8: true}); got != want {
			t.Errorf("one-line at depth %d = %q, want %q", depth, got, want)
		}
	}
}

func TestFallbacks(t *testing.T) {
	for _, tc := range []struct {
		name string
		form Form
		caps Caps
		want string
	}{
		{"no color leaves the core blank", Banner, Caps{UTF8: true, Width: 80}, BannerText},
		{"unknown width is wide", Banner, Caps{UTF8: true}, BannerText},
		{"no UTF-8 prints ASCII", Banner, Caps{Color: TrueColor, Width: 80}, ASCIIText},
		{"no UTF-8 mark prints ASCII", Mark, Caps{Width: 80}, ASCIIText},
		{"no UTF-8 one-line prints nothing", OneLine, Caps{Width: 80}, ""},
		{"no UTF-8 and too narrow for ASCII", Banner, Caps{Width: ASCIIWidth - 1}, ""},
		{"no UTF-8 at the ASCII width", Banner, Caps{Width: ASCIIWidth}, ASCIIText},
		{"narrow banner is one line", Banner, Caps{UTF8: true, Width: MinWidth - 1}, OneLineText},
		{"narrow mark is one line", Mark, Caps{UTF8: true, Width: MinWidth - 1}, OneLineText},
		{"banner at the minimum width", Banner, Caps{UTF8: true, Width: MinWidth}, BannerText},
		{"narrow and colored", Banner, Caps{Color: Color16, UTF8: true, Width: 20}, "\x1b[31m▙\x1b[0m brigada\n"},
	} {
		if got := String(tc.form, tc.caps); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestFromEnv(t *testing.T) {
	utf := map[string]string{"LANG": "en_US.UTF-8", "TERM": "xterm"}
	with := func(extra map[string]string) func(string) string {
		env := map[string]string{}
		for k, v := range utf {
			env[k] = v
		}
		for k, v := range extra {
			env[k] = v
		}
		return func(k string) string { return env[k] }
	}
	for _, tc := range []struct {
		name  string
		env   map[string]string
		tty   bool
		width int
		want  Caps
	}{
		{"16 colors", nil, true, 80, Caps{TTY: true, Color: Color16, UTF8: true, Width: 80}},
		{"256 colors", map[string]string{"TERM": "xterm-256color"}, true, 80, Caps{TTY: true, Color: Color256, UTF8: true, Width: 80}},
		{"truecolor", map[string]string{"TERM": "xterm-256color", "COLORTERM": "truecolor"}, true, 80, Caps{TTY: true, Color: TrueColor, UTF8: true, Width: 80}},
		{"24bit", map[string]string{"COLORTERM": "24bit"}, true, 80, Caps{TTY: true, Color: TrueColor, UTF8: true, Width: 80}},
		{"NO_COLOR", map[string]string{"NO_COLOR": "1", "COLORTERM": "truecolor"}, true, 80, Caps{TTY: true, UTF8: true, Width: 80}},
		{"empty NO_COLOR is unset", map[string]string{"NO_COLOR": ""}, true, 80, Caps{TTY: true, Color: Color16, UTF8: true, Width: 80}},
		{"not a terminal", map[string]string{"COLORTERM": "truecolor"}, false, 0, Caps{UTF8: true}},
		{"TERM=dumb", map[string]string{"TERM": "dumb"}, true, 80, Caps{TTY: true, Width: 80}},
		{"LC_ALL wins over LANG", map[string]string{"LC_ALL": "C"}, true, 80, Caps{TTY: true, Color: Color16, Width: 80}},
		{"LC_CTYPE wins over LANG", map[string]string{"LANG": "C", "LC_CTYPE": "en_US.utf8"}, true, 80, Caps{TTY: true, Color: Color16, UTF8: true, Width: 80}},
		{"no locale is C", map[string]string{"LANG": ""}, true, 80, Caps{TTY: true, Color: Color16, Width: 80}},
		{"COLUMNS when the size is unknown", map[string]string{"COLUMNS": "24"}, true, 0, Caps{TTY: true, Color: Color16, UTF8: true, Width: 24}},
		{"the terminal size wins over COLUMNS", map[string]string{"COLUMNS": "24"}, true, 120, Caps{TTY: true, Color: Color16, UTF8: true, Width: 120}},
		{"bad COLUMNS is unknown", map[string]string{"COLUMNS": "-5"}, true, 0, Caps{TTY: true, Color: Color16, UTF8: true}},
	} {
		if got := fromEnv(with(tc.env), tc.tty, tc.width); got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// Output that is not a terminal, such as a pipe or a file, gets no color.
func TestDetectNotATerminal(t *testing.T) {
	t.Setenv("COLORTERM", "truecolor")
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, w := range []io.Writer{f, &strings.Builder{}} {
		if caps := Detect(w); caps.TTY || caps.Color != NoColor {
			t.Errorf("Detect(%T) = %+v, want no TTY and no color", w, caps)
		}
	}
}
