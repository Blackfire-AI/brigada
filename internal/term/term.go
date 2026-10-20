// Package term styles terminal text using capabilities supplied by logo.Detect.
// It does not inspect the environment or detect the terminal itself.
package term

import (
	"regexp"
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
