// Package term styles terminal text using capabilities supplied by logo.Detect.
// It does not inspect the environment or detect the terminal itself.
package term

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/Blackfire-AI/brigada/internal/logo"
)

// Style formats text and symbols for a terminal. Its zero value is plain ASCII
// with a width of 80. Construct a Style with New to use detected capabilities.
type Style struct {
	caps logo.Caps
}

// New builds a style from capabilities already detected by logo.Detect.
// As in logo.Render, Color controls escapes; TTY controls the layout width.
func New(caps logo.Caps) Style {
	return Style{caps: caps}
}

const reset = "\x1b[0m"

func (s Style) wrap(text, color16, color256, trueColor string) string {
	var escape string
	switch s.caps.Color {
	case logo.Color16:
		escape = color16
	case logo.Color256:
		escape = color256
	case logo.TrueColor:
		escape = trueColor
	default:
		return text
	}
	return escape + text + reset
}

// Bold emphasizes text when color is enabled.
func (s Style) Bold(text string) string {
	return s.wrap(text, "\x1b[1m", "\x1b[1m", "\x1b[1m")
}

// Dim colors secondary text grey, without the terminal's faint attribute.
func (s Style) Dim(text string) string {
	return s.wrap(text, "\x1b[90m", "\x1b[38;5;245m", "\x1b[38;2;138;143;152m")
}

// Accent colors numbers, defaults and commands cyan.
func (s Style) Accent(text string) string {
	return s.wrap(text, "\x1b[36m", "\x1b[38;5;80m", "\x1b[38;2;108;198;224m")
}

// OK colors successful results green.
func (s Style) OK(text string) string {
	return s.wrap(text, "\x1b[32m", "\x1b[38;5;77m", "\x1b[38;2;95;215;95m")
}

// Warn colors warnings yellow.
func (s Style) Warn(text string) string {
	return s.wrap(text, "\x1b[33m", "\x1b[38;5;221m", "\x1b[38;2;255;215;95m")
}

// Bad colors errors red.
func (s Style) Bad(text string) string {
	return s.wrap(text, "\x1b[31m", "\x1b[38;5;203m", "\x1b[38;2;255;95;95m")
}

// Teal colors work that is done but not yet landed, apart from Accent at
// every depth.
func (s Style) Teal(text string) string {
	return s.wrap(text, "\x1b[96m", "\x1b[38;5;37m", "\x1b[38;2;0;175;175m")
}

// Violet colors the claude family's agent names.
func (s Style) Violet(text string) string {
	return s.wrap(text, "\x1b[95m", "\x1b[38;5;141m", "\x1b[38;2;177;151;255m")
}

// Blue colors the glm family's agent names.
func (s Style) Blue(text string) string {
	return s.wrap(text, "\x1b[94m", "\x1b[38;5;69m", "\x1b[38;2;95;135;255m")
}

// Mint colors the dsh family's agent names.
func (s Style) Mint(text string) string {
	return s.wrap(text, "\x1b[92m", "\x1b[38;5;85m", "\x1b[38;2;110;231;183m")
}

// tint paints text on a background: it opens fg and bg, re-emits both after
// every reset inside text (Style.wrap ends with a full reset, which would end
// the background), and closes with a reset. Only 256 colours and true colour
// tint; 16 colours and no colour return text unchanged. The pairs are the
// escapes for those two depths.
func (s Style) tint(text, fg256, bg256, fgTrue, bgTrue string) string {
	var open string
	switch s.caps.Color {
	case logo.Color256:
		open = "\x1b[" + bg256 + "m\x1b[" + fg256 + "m"
	case logo.TrueColor:
		open = "\x1b[" + bgTrue + "m\x1b[" + fgTrue + "m"
	default:
		return text
	}
	return paintOver(text, open)
}

// paintOver wraps text in open, re-emitted after every reset inside text.
func paintOver(text, open string) string {
	return open + strings.ReplaceAll(text, reset, reset+open) + reset
}

// band paints a full-width band: text on a solid background with dark text,
// the parameters given per depth. Unlike a tint it shows at 16 colours too;
// with no colour it returns text as is. Bold spans inside keep the background.
func (s Style) band(text, c16, c256, cTrue string) string {
	var params string
	switch s.caps.Color {
	case logo.Color16:
		params = c16
	case logo.Color256:
		params = c256
	case logo.TrueColor:
		params = cTrue
	default:
		return text
	}
	return paintOver(text, "\x1b["+params+"m")
}

// BandBad paints the error band: dark text on a solid red.
func (s Style) BandBad(text string) string {
	return s.band(text, "41;30", "48;5;203;38;5;16", "48;2;255;95;95;38;2;17;17;17")
}

// BandWarn paints the paused band: dark text on a solid yellow.
func (s Style) BandWarn(text string) string {
	return s.band(text, "43;30", "48;5;221;38;5;16", "48;2;255;215;95;38;2;17;17;17")
}

// TintNeeds paints a needs-you row on a dark red, text and background both
// set, so the row reads on a light theme too. Colours inside the text keep
// the background. Without 256 colours or true colour it returns text as is.
func (s Style) TintNeeds(text string) string {
	return s.tint(text, "38;5;224", "48;5;52", "38;2;255;215;215", "48;2;59;20;24")
}

// TintRun paints a running job's row on a dark blue; see TintNeeds.
func (s Style) TintRun(text string) string {
	return s.tint(text, "38;5;153", "48;5;17", "38;2;215;227;255", "48;2;18;35;63")
}

func (s Style) symbol(unicode, ascii string) string {
	if s.caps.UTF8 {
		return unicode
	}
	return ascii
}

// Check returns a check mark, or "ok" without UTF-8.
func (s Style) Check() string { return s.symbol("✓", "ok") }

// Question returns a question mark.
func (s Style) Question() string { return "?" }

// Warning returns an exclamation mark.
func (s Style) Warning() string { return "!" }

// Pointer returns a triangular pointer, or ">" without UTF-8.
func (s Style) Pointer() string { return s.symbol("▸", ">") }

// Rule returns a short rule, or "==" without UTF-8.
func (s Style) Rule() string { return s.symbol("━━", "==") }

// Again returns a repeat arrow, or "->" without UTF-8.
func (s Style) Again() string { return s.symbol("↺", "->") }

// Bullet returns a filled circle, or "*" without UTF-8.
func (s Style) Bullet() string { return s.symbol("●", "*") }

// Filled returns a filled progress cell, or "#" without UTF-8.
func (s Style) Filled() string { return s.symbol("■", "#") }

// Empty returns an empty progress cell, or "." without UTF-8.
func (s Style) Empty() string { return s.symbol("□", ".") }

// Dash returns a rule cell, or "-" without UTF-8.
func (s Style) Dash() string { return s.symbol("─", "-") }

// Cross returns a failure mark, or "x" without UTF-8.
func (s Style) Cross() string { return s.symbol("✗", "x") }

// Dot returns a separator dot, or "|" without UTF-8.
func (s Style) Dot() string { return s.symbol("·", "|") }

// AtMost returns a less-than-or-equal sign, or "<=" without UTF-8.
func (s Style) AtMost() string { return s.symbol("≤", "<=") }

// Up returns an upward arrow, or "^" without UTF-8.
func (s Style) Up() string { return s.symbol("⇡", "^") }

// Arrow returns a rightward arrow, or "->" without UTF-8.
func (s Style) Arrow() string { return s.symbol("→", "->") }

// Meter returns a slots-meter cell, or "=" without UTF-8.
func (s Style) Meter() string { return s.symbol("━", "=") }

// BoxTopLeft returns a rounded box's top-left corner, or "+" without UTF-8.
func (s Style) BoxTopLeft() string { return s.symbol("╭", "+") }

// BoxTopRight returns a rounded box's top-right corner, or "+" without UTF-8.
func (s Style) BoxTopRight() string { return s.symbol("╮", "+") }

// BoxBottomLeft returns a rounded box's bottom-left corner, or "+" without UTF-8.
func (s Style) BoxBottomLeft() string { return s.symbol("╰", "+") }

// BoxBottomRight returns a rounded box's bottom-right corner, or "+" without UTF-8.
func (s Style) BoxBottomRight() string { return s.symbol("╯", "+") }

// BoxSide returns a box's vertical side, or "|" without UTF-8.
func (s Style) BoxSide() string { return s.symbol("│", "|") }

// OneLine returns the brand's one-line form, `▙ brigada`, as logo.String
// prints it at the style's colour depth, without its trailing newline: only
// the ▙ is coloured, and plain with no colour. Without UTF-8 the brand has
// no one-line form and the result is empty.
func (s Style) OneLine() string {
	return strings.TrimSuffix(logo.String(logo.OneLine, s.caps), "\n")
}

// Width returns the layout width, clamped to 80..100 columns. Unknown widths
// and output that is not a terminal use 80 columns.
func (s Style) Width() int {
	if !s.caps.TTY {
		return 80
	}
	return min(max(s.caps.Width, 80), 100)
}

// Match ANSI CSI sequences (including SGR styles), OSC sequences (including
// hyperlinks, terminated by BEL or ST), and two-byte escape sequences.
var escape = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-_])`)

// VisibleWidth counts runes after removing terminal escapes. This matches
// rune-based wrapping; it does not measure wide glyphs or combining characters
// in terminal columns.
func VisibleWidth(text string) int {
	return utf8.RuneCountInString(StripEscapes(text))
}

// StripEscapes removes terminal escapes, leaving the text as it reads.
func StripEscapes(text string) string {
	return escape.ReplaceAllString(text, "")
}
