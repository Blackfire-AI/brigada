package main

import (
	"strings"
	"unicode/utf8"

	"github.com/Blackfire-AI/brigada/internal/term"
)

// topBox is a section's top border: the title, the note after it and the
// note at the right end, all optional but the title. bad paints the whole
// border and the title Bad. legend is a styled note for the right end that
// is left out whole when it does not fit.
type topBox struct {
	title, note, right string
	legend             string
	bad                bool
}

// topBoxed wraps a section's rows in its borders: the top border first,
// the bottom border last, both in the pinned class, which only an empty
// section drops (dropEmptyBorders).
func topBoxed(section int, box topBox, rows []topLine) []topLine {
	lines := make([]topLine, 0, len(rows)+2)
	lines = append(lines, topLine{section: section, box: &box, bad: box.bad})
	for _, r := range rows {
		r.section, r.boxed, r.bad = section, true, box.bad
		lines = append(lines, r)
	}
	return append(lines, topLine{section: section, foot: true, bad: box.bad})
}

// topBoxTop is a section's top border at width: `╭─ TITLE  note ──── right ─╮`,
// the border Dim (Bad for a Bad box), the title Bold, the notes Dim. What
// does not fit gives way — the right note, then the note, the title last —
// each cut with cutLine or left out.
func topBoxTop(b topBox, style term.Style, width int) string {
	border, title := style.Dim, style.Bold
	if b.bad {
		border, title = style.Bad, style.Bad
	}
	count := utf8.RuneCountInString
	t, note, right := b.title, b.note, b.right
	// The corners, the dash and space after the first, the space before the
	// fill and one fill cell: 6 columns around the title.
	if room := width - 6; count(t) > room {
		if room >= 4 {
			t = cutLine(t, room)
		} else {
			t = string([]rune(t)[:max(room, 0)])
		}
	}
	used := count(t) + 6
	if note != "" {
		room := width - used - 2
		switch {
		case count(note) <= room:
		case room >= 4:
			note = cutLine(note, room)
		default:
			note = ""
		}
	}
	if note != "" {
		used += 2 + count(note)
	}
	if right != "" {
		room := width - used - 3
		switch {
		case count(right) <= room:
		case room >= 4:
			right = cutLine(right, room)
		default:
			right = ""
		}
	}
	// The right end, styled: the plain note in Dim, or the legend when it
	// fits whole.
	rightOut, rightW := "", 0
	switch {
	case right != "":
		rightOut, rightW = style.Dim(right), count(right)
	case b.legend != "" && term.VisibleWidth(b.legend) <= width-used-3:
		rightOut, rightW = b.legend, term.VisibleWidth(b.legend)
	}
	fill := width - used + 1
	if rightW > 0 {
		fill -= rightW + 3
	}
	fill = max(fill, 1)
	out := border(style.BoxTopLeft()+style.Dash()) + " " + title(t)
	if note != "" {
		out += "  " + style.Dim(note)
	}
	out += " " + border(strings.Repeat(style.Dash(), fill))
	if rightW > 0 {
		out += " " + rightOut + " " + border(style.Dash())
	}
	return out + border(style.BoxTopRight())
}

// topBoxBottom is a section's bottom border at width.
func topBoxBottom(bad bool, style term.Style, width int) string {
	border := style.Dim
	if bad {
		border = style.Bad
	}
	return border(style.BoxBottomLeft() + strings.Repeat(style.Dash(), max(width-2, 0)) + style.BoxBottomRight())
}

// The tints a row can carry.
const (
	topTintNone  = iota
	topTintNeeds // a needs-you entry: a dark red
	topTintRun   // a running job: a dark blue
)

// topTints says whether the style tints rows at all: 256 colours and true
// colour do, 16 colours and no colour do not.
func topTints(style term.Style) bool { return style.TintNeeds("x") != "x" }

// topBoxRow is one row of a box: `│ text │`, the text cut to the inner
// width and padded to it by its visible width. A tinted row is tinted after
// the cut and the padding, so the tint covers the margins and the padding
// inside the borders.
func topBoxRow(text string, bad bool, tint int, style term.Style, width int) string {
	border := style.Dim
	if bad {
		border = style.Bad
	}
	inner := max(width-4, 0)
	text = topFit(text, inner)
	pad := max(inner-term.VisibleWidth(text), 0)
	body := " " + text + strings.Repeat(" ", pad) + " "
	switch tint {
	case topTintNeeds:
		body = style.TintNeeds(body)
	case topTintRun:
		body = style.TintRun(body)
	}
	return border(style.BoxSide()) + body + border(style.BoxSide())
}
