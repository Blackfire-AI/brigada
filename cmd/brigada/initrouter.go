package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The foreman rules once reached a Claude Code session through sections of
// the shared Markdown files, between markers brigada owned: a router
// block in AGENTS.md pointing at the rules file, and a marked import of
// AGENTS.md in CLAUDE.md (design §4.2). Init no longer writes them; a
// rerun of init and brigada remove strip them. Removal takes a section
// out only while it still stands where init put it, so a file the user
// has not edited comes back byte for byte, and an edited one keeps every
// byte that could be the user's.

const (
	wiringRulesFile  = ".brigada/foreman.md"
	wiringAgentsFile = "AGENTS.md"
	wiringClaudeFile = "CLAUDE.md"
)

// sectionKind names one brigada-managed section of a shared Markdown file.
type sectionKind string

const (
	routerSection sectionKind = "router"
	importSection sectionKind = "import"
)

func (k sectionKind) beginPrefix() string { return "<!-- brigada-managed: " + string(k) + " " }
func (k sectionKind) endMarker() string   { return "<!-- /brigada-managed: " + string(k) + " -->" }

// sectionRecord is what init added around a section besides the section
// itself. It lives in the begin marker, survives reruns, and tells remove
// which bytes are init's: the file, the newline completing an
// unterminated last line, the blank line before the section. The
// separators are recorded with the byte offset they start at, the length
// of the file init found, so remove can tell them from newlines the user
// wrote in front of the section later: once anything before the section
// changed length, the offset no longer leads to it and they stay.
type sectionRecord struct {
	created, newline, blank bool
	at                      int // where the separators start; -1 when unknown
}

const (
	recordLead = "; init added "
	recordAt   = " after byte "
)

// separators is the number of newlines init added before the section.
func (r sectionRecord) separators() int {
	n := 0
	if r.newline {
		n++
	}
	if r.blank {
		n++
	}
	return n
}

func (r sectionRecord) String() string {
	var parts []string
	if r.created {
		parts = append(parts, "the file")
	}
	if r.newline {
		parts = append(parts, "a newline")
	}
	if r.blank {
		parts = append(parts, "a blank line")
	}
	if len(parts) == 0 {
		return ""
	}
	s := recordLead + strings.Join(parts, ", ")
	if r.separators() > 0 {
		s += recordAt + strconv.Itoa(r.at)
	}
	return s
}

func parseSectionRecord(marker string) sectionRecord {
	r := sectionRecord{at: -1}
	_, rest, ok := strings.Cut(strings.TrimSuffix(marker, "-->"), recordLead)
	if !ok {
		return r
	}
	rest = strings.TrimSpace(rest)
	if i := strings.LastIndex(rest, recordAt); i >= 0 {
		if n, err := strconv.Atoi(rest[i+len(recordAt):]); err == nil && n >= 0 {
			r.at = n
		}
		rest = rest[:i]
	}
	for _, part := range strings.Split(rest, ", ") {
		switch strings.TrimSpace(part) {
		case "the file":
			r.created = true
		case "a newline":
			r.newline = true
		case "a blank line":
			r.blank = true
		}
	}
	return r
}

// mdLine is one line of a Markdown file: its bytes in the file, without
// the newline, and whether it belongs to a fenced code block (fence lines
// included).
type mdLine struct {
	start, end int
	text       string
	fenced     bool
}

// scanMarkdown splits a file into lines and marks the ones inside a
// fenced code block: a fence opens on a line of three or more backticks
// or three or more tildes, indented by at most three spaces (a backtick
// fence's info string holds no backtick, as in CommonMark), and closes
// only on a line of the same character, at least as long, with nothing
// after it but whitespace.
func scanMarkdown(d []byte) (lines []mdLine, open int) {
	var fence byte
	fenceLen := 0
	for start, n := 0, 1; ; n++ {
		end := len(d)
		if i := bytes.IndexByte(d[start:], '\n'); i >= 0 {
			end = start + i
		}
		line := mdLine{start: start, end: end, text: strings.TrimSuffix(string(d[start:end]), "\r")}
		ch, run, rest := fenceRun(line.text)
		switch {
		case fence != 0:
			line.fenced = true
			if ch == fence && run >= fenceLen && strings.TrimSpace(rest) == "" {
				fence = 0
			}
		case ch != 0 && (ch == '~' || !strings.Contains(rest, "`")):
			line.fenced = true
			fence, fenceLen, open = ch, run, n
		}
		lines = append(lines, line)
		if end == len(d) {
			break
		}
		start = end + 1
	}
	if fence == 0 {
		open = 0
	}
	return lines, open
}

// fenceRun reads a possible fence line: up to three spaces, then a run of
// at least three backticks or tildes. ch is 0 when the line is no fence.
func fenceRun(text string) (ch byte, run int, rest string) {
	i := 0
	for i < len(text) && i < 3 && text[i] == ' ' {
		i++
	}
	if i >= len(text) || (text[i] != '`' && text[i] != '~') {
		return 0, 0, ""
	}
	ch = text[i]
	for i+run < len(text) && text[i+run] == ch {
		run++
	}
	if run < 3 {
		return 0, 0, ""
	}
	return ch, run, text[i+run:]
}

// sectionSpan is where a section sits: [start, end) covers the begin
// marker line through the end marker, without the end line's newline.
type sectionSpan struct {
	start, end int
	record     sectionRecord
}

// findSection locates kind's section by its marker lines outside any
// fence (a marker inside a code block is text, not a marker). found is
// false when the file has none; err reports damage — a begin without its
// end, an end before its begin, or two blocks.
func findSection(rel string, lines []mdLine, k sectionKind) (span sectionSpan, found bool, err error) {
	var begins, ends []int
	for i, l := range lines {
		if l.fenced {
			continue
		}
		text := strings.TrimSpace(l.text)
		switch {
		case strings.HasPrefix(text, k.beginPrefix()) && strings.HasSuffix(text, "-->"):
			begins = append(begins, i)
		case text == k.endMarker():
			ends = append(ends, i)
		}
	}
	fix := "; delete the block, or restore its markers, then rerun init"
	switch {
	case len(begins) == 0 && len(ends) == 0:
		return span, false, nil
	case len(begins) > 1 || len(ends) > 1:
		return span, false, fmt.Errorf("%s holds more than one brigada %s block%s", rel, k, fix)
	case len(begins) == 0:
		return span, false, fmt.Errorf("%s holds the end marker of brigada's %s block without its begin marker%s", rel, k, fix)
	case len(ends) == 0:
		return span, false, fmt.Errorf("%s holds the begin marker of brigada's %s block without its end marker%s", rel, k, fix)
	case ends[0] < begins[0]:
		return span, false, fmt.Errorf("%s holds the end marker of brigada's %s block before its begin marker%s", rel, k, fix)
	}
	span.start, span.end = lines[begins[0]].start, lines[ends[0]].end
	span.record = parseSectionRecord(strings.TrimSpace(lines[begins[0]].text))
	return span, true, nil
}

// cutSection removes a section, its end line's newline, and the
// separators its record names, but only while they stand where init put
// them: the section still starts right after them at the recorded offset,
// and they are all newlines, the last byte before them ending a line
// unless init added the newline to end it. Any edit in front of the
// section that changes its length moves the section off that offset, and
// then every separator stays: one of them could be the user's. Init's
// newline also stays when anything follows the section, which would
// otherwise join the line it ends. The result is never nil, so an emptied
// file that init did not create stays.
func cutSection(d []byte, span sectionSpan) []byte {
	start, end := span.start, span.end
	if end < len(d) && d[end] == '\n' {
		end++
	}
	if r := span.record; separatorsInPlace(d, span.start, r) {
		if r.blank {
			start--
		}
		if r.newline && end == len(d) {
			start--
		}
	}
	return append(append([]byte{}, d[:start]...), d[end:]...)
}

// separatorsInPlace reports whether the separators r records still stand
// right in front of a section starting at start.
func separatorsInPlace(d []byte, start int, r sectionRecord) bool {
	// Init adds no separator to an empty file, so at is never 0.
	n := r.separators()
	if n == 0 || r.at <= 0 || r.at+n != start {
		return false
	}
	for _, c := range d[r.at:start] {
		if c != '\n' {
			return false
		}
	}
	return (d[r.at-1] == '\n') != r.newline
}

// removeSections takes brigada's sections out of a shared Markdown file,
// each with the separators its begin marker records, the last one first
// so an earlier one's offsets still hold. nil next deletes the file, only
// when init created it and nothing else is left in it but line breaks no
// more than the separators init added.
func removeSections(rel string, d []byte, exists bool) ([]byte, string, error) {
	if !exists {
		return d, "unchanged", nil
	}
	state, created, seps := "unchanged", false, 0
	for {
		lines, _ := scanMarkdown(d)
		var last sectionSpan
		found := false
		for _, k := range []sectionKind{routerSection, importSection} {
			span, ok, err := findSection(rel, lines, k)
			if err != nil {
				return nil, "", err
			}
			if ok && (!found || span.start > last.start) {
				last, found = span, true
			}
		}
		if !found {
			break
		}
		created = created || last.record.created
		seps += last.record.separators()
		d = cutSection(d, last)
		state = "removed"
	}
	if state == "removed" && created && len(d) <= seps && len(bytes.Trim(d, "\n")) == 0 {
		return nil, "removed", nil
	}
	return d, state, nil
}

// linksTo reports whether rel is a symlink to other under root, whether
// or not other exists. A shared file that is a link to the other one
// holds no section of its own: the other is the file.
func linksTo(root, rel, other string) bool {
	path := filepath.Join(root, rel)
	st, err := os.Lstat(path)
	if err != nil || st.Mode()&os.ModeSymlink == 0 {
		return false
	}
	target, err := os.Readlink(path)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	target = filepath.Clean(target)
	if target == filepath.Join(root, other) {
		return true
	}
	// The link may spell the repository's directory through a path of its
	// own (a resolved symlink above the root); the target's directory decides.
	dir, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		return false
	}
	real, err := filepath.EvalSymlinks(root)
	return err == nil && dir == real && filepath.Base(target) == other
}
