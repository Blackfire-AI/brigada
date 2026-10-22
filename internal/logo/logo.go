// Package logo prints the brigada logo in a terminal: the banner (the
// Blackfire flame beside the pixel wordmark), the flame alone and the
// one-line form from docs/brand/cli, with the flame core in hi-vis orange
// where the terminal can show it, and the fallbacks docs/brand/README.md
// sets for no color, no UTF-8 and narrow terminals.
//
// Render is pure: it prints what the given Caps allow. Detect reads the
// environment and the terminal to fill those Caps.
package logo

import (
	"io"
	"strings"
	"unicode/utf8"
)

// The texts below are copies of docs/brand/cli/*.txt, byte for byte (a
// test checks it). go:embed cannot reach docs/ from this package.
const (
	// BannerText is cli/banner.txt: the flame and the wordmark, 36 × 5
	// characters. Without color the flame core is left blank.
	BannerText = `   ▄█
▄  ███   █       ▀            █
██▄▀███  █▀█ █▀▀ █ █▀█ █▀█  █▀█ █▀█
██   ██  ▀▀▀ ▀   ▀ ▀▀█ ▀▀▀▀ ▀▀▀ ▀▀▀▀
▀█▄▄▄█▀            ▀▀▀
`
	// MarkText is cli/mark.txt: the flame alone, 7 × 5 characters.
	MarkText = `   ▄█
▄  ███
██▄▀███
██   ██
▀█▄▄▄█▀
`
	// OneLineText is cli/oneline.txt, for status lines and logs.
	OneLineText = "▙ brigada\n"
	// ASCIIText is cli/ascii.txt, for terminals without UTF-8.
	ASCIIText = `|_   _  .  _   _   _|  _
|_) |   | (_| (_| (_| (_|
           _|
`
)

// Widths of the texts in terminal columns.
const (
	BannerWidth  = 36
	MarkWidth    = 7
	OneLineWidth = 9
	ASCIIWidth   = 25
	// MinWidth is the narrowest terminal that gets the banner or the mark;
	// a narrower one gets the one-line form.
	MinWidth = 37
)

// Form is the shape of the logo to print.
type Form int

const (
	Banner Form = iota
	Mark
	OneLine
)

// Depth is how many colors the terminal shows.
type Depth int

const (
	NoColor Depth = iota
	Color16
	Color256
	TrueColor
)

// Caps is what the output can show.
type Caps struct {
	// TTY is whether the output is a terminal. Render ignores it; callers
	// use it to leave the logo out of output that scripts read.
	TTY bool
	// Color is the color depth; NoColor prints no escape codes.
	Color Depth
	// UTF8 is false when the locale is not UTF-8 or TERM is dumb.
	UTF8 bool
	// Width is the terminal width in columns; 0 means unknown.
	Width int
}

const reset = "\x1b[0m"

// coreBackground and coreForeground are the hi-vis escapes per depth,
// from docs/brand/tokens.json.
var (
	coreBackground = map[Depth]string{
		TrueColor: "\x1b[48;2;255;90;31m",
		Color256:  "\x1b[48;5;202m",
		Color16:   "\x1b[41m",
	}
	coreForeground = map[Depth]string{
		TrueColor: "\x1b[38;2;255;90;31m",
		Color256:  "\x1b[38;5;202m",
		Color16:   "\x1b[31m",
	}
)

// Render writes form to w as caps allow:
//
//   - without UTF-8, the banner and the mark print as ASCIIText when it
//     fits, and nothing prints otherwise; the one-line form, which has no
//     ASCII version, prints nothing;
//   - narrower than MinWidth, the banner and the mark print as the
//     one-line form;
//   - with color, the flame core (coreCells in the banner and the mark)
//     prints on a hi-vis background: the half blocks there keep the
//     terminal's own foreground for the flame body and show orange in their
//     other half, and the blanks turn orange; the ▙ of the one-line form
//     prints in hi-vis; every other cell keeps the default foreground;
//   - with NoColor, the texts print as they are, the core left blank.
func Render(w io.Writer, form Form, caps Caps) error {
	_, err := io.WriteString(w, render(form, caps))
	return err
}

func render(form Form, caps Caps) string {
	if !caps.UTF8 {
		if form == OneLine || (caps.Width > 0 && caps.Width < ASCIIWidth) {
			return ""
		}
		return ASCIIText
	}
	if caps.Width > 0 && caps.Width < MinWidth {
		form = OneLine
	}
	text := BannerText
	switch form {
	case Mark:
		text = MarkText
	case OneLine:
		text = OneLineText
	}
	if caps.Color == NoColor {
		return text
	}
	if form == OneLine {
		_, size := utf8.DecodeRuneInString(text)
		return coreForeground[caps.Color] + text[:size] + reset + text[size:]
	}
	return paintCore(text, coreBackground[caps.Color])
}

// coreCells are the cells of the flame core, the same in the banner and
// the mark: on each line, the first and the last column (in characters).
var coreCells = []struct{ line, from, to int }{
	{2, 3, 3},
	{3, 2, 4},
	{4, 2, 4},
}

// paintCore puts the flame core of text on the background bg.
func paintCore(text, bg string) string {
	lines := strings.Split(text, "\n")
	for _, c := range coreCells {
		r := []rune(lines[c.line])
		lines[c.line] = string(r[:c.from]) + bg + string(r[c.from:c.to+1]) + reset + string(r[c.to+1:])
	}
	return strings.Join(lines, "\n")
}

// String returns what Render would write.
func String(form Form, caps Caps) string {
	var b strings.Builder
	_ = Render(&b, form, caps)
	return b.String()
}
