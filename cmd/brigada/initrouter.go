package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/foreman"
	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// The foreman rules reach a Claude Code session through three files: the
// rules file .brigada/foreman.md, which brigada owns whole; a short router
// block in AGENTS.md between markers brigada owns, which sends the session
// to the rules and carves brigada's own workers and reviewers out; and a
// marked section of CLAUDE.md that imports AGENTS.md. Nothing imports the
// rules file: workers read the same AGENTS.md in their worktrees and must
// never receive the foreman rules (design §4.2). The shared files are
// edited minimally — only the bytes between brigada's markers change, and
// the separators init adds are recorded in the begin marker with where
// they start. Remove takes them out only while they still stand there, so
// a file the user has not edited comes back byte for byte, and an edited
// one keeps every byte that could be the user's.

const (
	wiringRulesFile  = ".brigada/foreman.md"
	wiringAgentsFile = "AGENTS.md"
	wiringClaudeFile = "CLAUDE.md"

	// The sections' template versions, written into their begin markers.
	routerVersion = 1
	importVersion = 1
)

// rulesContent is the rules file: brigada's marker with the instructions'
// version, the ownership note, and the foreman instructions.
func rulesContent() []byte {
	return []byte(fmt.Sprintf(`%s v%d -->

These are brigada's foreman rules for your own session (mode A). Brigada owns
this file and replaces it on "brigada init"; put your own instructions elsewhere.

%s`, wiringSkillMarker, foreman.InstructionsVersion, foreman.Instructions()))
}

// routerBody is the router block's text, between its markers. The
// carve-out comes first and quotes the openers of the worker and reviewer
// prompts, which internal/prompt keeps as a contract. The rules file is
// named, never imported.
func routerBody() string {
	return fmt.Sprintf(`If your prompt opens with %q or
%q, you are a worker or reviewer run by brigada:
follow that prompt; this block and the foreman rules do not apply to you.

Otherwise: in this repository, code changes go through brigada, and you are
its foreman. Read `+"`"+wiringRulesFile+"`"+` first and follow it, unless your
prompt already gives you brigada's foreman rules. Plan work with brigada's
tools; do not edit code yourself unless the user explicitly asks you to.
If brigada's tools are not available in this session, say so and tell the
user how to enable them (restart the session in this repository and approve
the `+"`brigada`"+` MCP server) instead of doing the work by hand.`, prompt.WorkerOpener, prompt.ReviewerOpener)
}

// importBody is the import section's text: Claude Code's import of
// AGENTS.md. Init adds it whether or not the user already has one — a
// redundant import at worst loads AGENTS.md twice, a missed one would
// silently break the router — so no existing import is looked for.
const importBody = "@" + wiringAgentsFile

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

// renderSection is a section's full text: begin marker (kind, version,
// record), body, end marker; no newline after the end marker.
func renderSection(k sectionKind, version int, rec sectionRecord, body string) string {
	return k.beginPrefix() + "v" + strconv.Itoa(version) + rec.String() + " -->\n" + body + "\n" + k.endMarker()
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
// fenced code block, the one piece of Markdown init reads: a fence opens
// on a line of three or more backticks or three or more tildes, indented
// by at most three spaces (a backtick fence's info string holds no
// backtick, as in CommonMark), and closes only on a line of the same
// character, at least as long, with nothing after it but whitespace.
// open is the 1-based line of a fence still open at the end of the file,
// else 0.
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

func fenceError(rel string, line int) error {
	return fmt.Errorf("%s ends inside a fenced code block opened on line %d and never closed, so anything appended would be inert; close the fence, then rerun init", rel, line)
}

// mergeSection adds kind's section to a shared Markdown file, or on a
// rerun replaces the text between its markers and nothing else. A missing
// file is created holding only the section; an existing one gets it
// appended after a blank line (and a newline first, when the last line is
// unterminated), with what was added recorded in the begin marker. A file
// that ends inside an unclosed fence is refused: the section would be
// inert there.
func mergeSection(rel string, d []byte, exists bool, k sectionKind, version int, body string) ([]byte, string, error) {
	return mergeOwned(rel, d, exists, k, version, body, false)
}

// mergeOwned is mergeSection for a file that may be init's already
// (created), which the section's record then says.
func mergeOwned(rel string, d []byte, exists bool, k sectionKind, version int, body string, created bool) ([]byte, string, error) {
	if !exists {
		return []byte(renderSection(k, version, sectionRecord{created: true}, body) + "\n"), "created", nil
	}
	lines, open := scanMarkdown(d)
	if open > 0 {
		return nil, "", fenceError(rel, open)
	}
	span, found, err := findSection(rel, lines, k)
	if err != nil {
		return nil, "", err
	}
	if found {
		rec := span.record
		rec.created = rec.created || created
		next := append([]byte(nil), d[:span.start]...)
		next = append(next, renderSection(k, version, rec, body)...)
		next = append(next, d[span.end:]...)
		if bytes.Equal(next, d) {
			return d, "unchanged", nil
		}
		return next, "replaced", nil
	}
	rec := sectionRecord{created: created, at: len(d)}
	next := append([]byte(nil), d...)
	if len(d) > 0 {
		if d[len(d)-1] != '\n' {
			rec.newline = true
			next = append(next, '\n')
		}
		rec.blank = true
		next = append(next, '\n')
	}
	next = append(next, renderSection(k, version, rec, body)+"\n"...)
	return next, "added", nil
}

// mergeMoved is mergeSection for a file whose role changed with the
// layout (initrouter.go, routerLayout): a section of the other kind that
// an earlier init put there is cut first, so a CLAUDE.md that became the
// shared file loses its import of itself, and an AGENTS.md that is a file
// again loses the router its CLAUDE.md carried. The cut section's record
// of a file init created is carried to the new section, whether or not
// the user's content remains beside it.
func mergeMoved(rel string, d []byte, exists bool, k sectionKind, version int, body string, stale sectionKind) ([]byte, string, error) {
	if !exists {
		return mergeSection(rel, d, exists, k, version, body)
	}
	lines, open := scanMarkdown(d)
	if open > 0 {
		return nil, "", fenceError(rel, open)
	}
	span, found, err := findSection(rel, lines, stale)
	if err != nil {
		return nil, "", err
	}
	if !found {
		return mergeSection(rel, d, exists, k, version, body)
	}
	cut := cutSection(d, span)
	created := span.record.created
	next, _, err := mergeOwned(rel, cut, len(cut) > 0 || !created, k, version, body, created)
	if err != nil {
		return nil, "", err
	}
	return next, "replaced", nil
}

func mergeRouter(rel string, d []byte, exists bool) ([]byte, string, error) {
	return mergeMoved(rel, d, exists, routerSection, routerVersion, routerBody(), importSection)
}

func mergeImport(rel string, d []byte, exists bool) ([]byte, string, error) {
	return mergeMoved(rel, d, exists, importSection, importVersion, importBody, routerSection)
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

// retireSkill removes the skill an older init wrote, which the rules file
// replaces. A file there without brigada's marker is the user's and stays.
func retireSkill(d []byte, exists bool) ([]byte, string, error) {
	if !exists || !bytes.Contains(d, []byte(wiringSkillMarker)) {
		return d, "unchanged", nil
	}
	return nil, "removed", nil
}

// routerLayout says which shared file gets the router block and which
// imports it. When CLAUDE.md and AGENTS.md are one file — either a symlink
// to the other, dangling or not — the block goes into the real file and
// there is no import, which would be a self-import.
type routerLayout struct {
	router string // the file that gets the router block
	imprt  string // the file that imports it; "" when the two are one file
	link   string // the symlink of the pair pointing at router; "" when none
}

func planRouterLayout(root string) routerLayout {
	agents := linksTo(root, wiringAgentsFile, wiringClaudeFile)
	claude := linksTo(root, wiringClaudeFile, wiringAgentsFile)
	switch {
	case agents && !claude:
		return routerLayout{router: wiringClaudeFile, link: wiringAgentsFile}
	case claude && !agents:
		return routerLayout{router: wiringAgentsFile, link: wiringClaudeFile}
	}
	// Any other symlink at either path is refused by wiringSafePath.
	return routerLayout{router: wiringAgentsFile, imprt: wiringClaudeFile}
}

// linksTo reports whether rel is a symlink to other under root, whether
// or not other exists.
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
