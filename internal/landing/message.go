package landing

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Bounds of the commit message (§7.1).
const (
	SubjectRunes  = 72
	SpecBytes     = 600
	SummaryBytes  = 2000
	MessageBytes  = 4096
	TruncatedMark = " [truncated]"
)

// Addresses is the family-to-address table of the Co-Authored-By
// trailer (§7.1, open in §16 item 10): a known family whose address
// brigada does not have gets the model name alone, and is reported.
var Addresses = map[string]string{
	"anthropic": "noreply@anthropic.com",
}

// Task is the plain data a squash commit message is built from (§7.1),
// read through the API by the caller; never the branch's own commit
// messages.
type Task struct {
	ID      int64
	Title   string
	Branch  string // the result branch's name
	Head    string // the result branch's fetched head sha
	Spec    string
	Summary string // the worker's summary, which is data, never executed
	Family  string // the authoring model family
	Model   string
}

// Message builds the squash commit message (§7.1): subject, indented
// spec summary and worker summary, then the trailers. The second result
// names a known family whose address brigada lacks; empty when none.
func Message(t Task) (string, string) {
	subject := Subject(t.Title, t.ID)
	var trailers []string
	trailers = append(trailers,
		fmt.Sprintf("Brigada-Task: %d", t.ID),
		fmt.Sprintf("Brigada-Branch: %s@%s", cleanLine(t.Branch), t.Head))
	noAddr := ""
	switch {
	case t.Family == "":
		trailers = append(trailers, "Brigada-Author: unknown")
	default:
		name := cleanLine(t.Model)
		if name == "" {
			name = cleanLine(t.Family)
		}
		if addr, ok := Addresses[t.Family]; ok {
			trailers = append(trailers, fmt.Sprintf("Co-Authored-By: %s <%s>", name, addr))
		} else {
			trailers = append(trailers, "Co-Authored-By: "+name)
			noAddr = name
		}
	}
	tr := strings.Join(trailers, "\n")
	spec := sanitize(firstParagraph(t.Spec))
	summary := sanitize(t.Summary)
	build := func(summaryMax int) string {
		var b strings.Builder
		b.WriteString(subject + "\n")
		if spec != "" {
			b.WriteString("\nSpec:\n" + indent(truncate(spec, SpecBytes)) + "\n")
		}
		if summary != "" {
			b.WriteString("\nWorker summary:\n" + indent(truncate(summary, summaryMax)) + "\n")
		}
		b.WriteString("\n" + tr + "\n")
		return b.String()
	}
	max := SummaryBytes
	msg := build(max)
	for len(msg) > MessageBytes && max > 64 {
		max -= len(msg) - MessageBytes + 16
		if max < 64 {
			max = 64
		}
		msg = build(max)
	}
	return msg, noAddr
}

// Subject is the task title on one line, at most 72 characters.
func Subject(title string, id int64) string {
	s := cleanLine(title)
	if s == "" {
		s = fmt.Sprintf("task %d", id)
	}
	if utf8.RuneCountInString(s) > SubjectRunes {
		r := []rune(s)
		s = strings.TrimRight(string(r[:SubjectRunes-1]), " ") + "…"
	}
	return s
}

// cleanLine folds text to one line with no control characters.
func cleanLine(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// sanitize strips control characters other than newline and tab
// (NUL and CR included).
func sanitize(s string) string {
	s = strings.ToValidUTF8(s, "")
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s))
}

// firstParagraph is the text up to the first blank line.
func firstParagraph(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "\n\n"); i >= 0 {
		s = s[:i]
	}
	return s
}

// indent indents every line four spaces, so none can read as a
// trailer or a # comment.
func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}

// truncate cuts s to at most max bytes at a character boundary,
// marking the cut.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max - len(TruncatedMark)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + TruncatedMark
}
