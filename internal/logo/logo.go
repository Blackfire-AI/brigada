// Package logo prints the brigada pixel logo in a terminal: the banner,
// the compact mark and the one-line form from docs/brand/cli, with the
// foreman cell in hi-vis orange where the terminal can show it, and the
// fallbacks docs/brand/README.md sets for no color, no UTF-8 and narrow
// terminals.
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
	// BannerText is cli/banner.txt: the wordmark, 27 × 4 characters.
	BannerText = `█       ▀            █
█▀█ █▀▀ █ █▀█ █▀█  █▀█ █▀█
▀▀▀ ▀   ▀ ▀▀█ ▀▀▀▀ ▀▀▀ ▀▀▀▀
          ▀▀▀
`
	// MarkText is cli/mark.txt: the b alone, 3 × 3 characters.
	MarkText = `█
█▀█
▀▀▀
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
	BannerWidth  = 27
	MarkWidth    = 3
	OneLineWidth = 9
	ASCIIWidth   = 25
	// MinWidth is the narrowest terminal that gets the banner or the mark;
	// a narrower one gets the one-line form.
	MinWidth = 28
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

// foremanBackground and foremanForeground are the hi-vis escapes per depth,
// from docs/brand/tokens.json.
var (
	foremanBackground = map[Depth]string{
		TrueColor: "\x1b[48;2;255;90;31m",
		Color256:  "\x1b[48;5;202m",
		Color16:   "\x1b[41m",
	}
	foremanForeground = map[Depth]string{
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
//   - with color, the foreman cell (the first character of the banner and
//     the mark) prints as ▄ on a hi-vis background, so its lower half takes
//     the terminal's own foreground, and the ▙ of the one-line form prints
//     in hi-vis; every other cell keeps the default foreground;
//   - with NoColor, the texts print as they are, the foreman cell as █.
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
	_, size := utf8.DecodeRuneInString(text)
	if form == OneLine {
		return foremanForeground[caps.Color] + text[:size] + reset + text[size:]
	}
	return foremanBackground[caps.Color] + "▄" + reset + text[size:]
}

// String returns what Render would write.
func String(form Form, caps Caps) string {
	var b strings.Builder
	_ = Render(&b, form, caps)
	return b.String()
}
