package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/term"
)

// The wiring an older init left in a repository: the
// bridge in the repository's .mcp.json, the Stop hook in
// .claude/settings.local.json, the foreman rules as .brigada/foreman.md
// routed to from AGENTS.md (initrouter.go), and the Codex table in
// .codex/config.toml. Init writes none of it anymore; a rerun of init and
// brigada remove strip it (removewiring.go). The files are edited
// minimally: only brigada's entries change, never the rest of the user's
// bytes. The parent-folder coordinator's wiring (initparent.go) is still
// written.

const (
	wiringMCPFile            = ".mcp.json"
	wiringCodexFile          = ".codex/config.toml"
	wiringSettingsFile       = ".claude/settings.local.json"
	wiringSharedSettingsFile = ".claude/settings.json"
	wiringSkillMarker        = "<!-- brigada-managed: foreman-instructions"
	hookYieldCommand         = "brigada hold yield"
	hookConfirmCommand       = "brigada hold confirm"

	// wiringSkillFile is where an older init put the rules, as a skill;
	// a rerun of init and brigada remove delete it when it carries
	// brigada's marker.
	wiringSkillFile = ".claude/skills/brigada-foreman/SKILL.md"
)

// mcpBridge is one server entry in a .mcp.json file.
type mcpBridge struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// wireFile is one planned wiring change. err set means the file is damaged or
// unsafe and is left alone; next is then nil. An existing file with nil next
// and state removed is deleted.
type wireFile struct {
	root    string // safety boundary used when rechecking a removal
	rel     string
	path    string
	old     []byte
	exists  bool
	next    []byte
	state   string // created, added, replaced, removed, unchanged
	err     error
	purpose string // what the plan says the file is for; "" takes the path's default
	// generated marks init's default role files and skills: --verbose does
	// not print their full texts.
	generated bool
}

func (w wireFile) changed() bool {
	return w.err == nil && (!bytes.Equal(w.old, w.next) ||
		(w.exists && w.next == nil && w.state == "removed"))
}

// planLegacyWiring plans what a rerun of init strips from a repository an
// older init wired: everything brigada remove removes, the git exclude
// line that hid the private settings included. It reads files and writes
// nothing.
func planLegacyWiring(root string) []wireFile {
	return append([]wireFile{planExcludeRemoval(root)}, planSteps(root, legacyWiringSteps(root))...)
}

// wireStep is one file a wiring plan edits: its path under the root and
// the edit that turns its current bytes into the wanted ones.
type wireStep struct {
	rel  string
	edit func([]byte, bool) ([]byte, string, error)
}

// planSteps reads each step's file under root and computes its edit; it
// writes nothing.
func planSteps(root string, steps []wireStep) []wireFile {
	var files []wireFile
	for _, s := range steps {
		w := wireFile{root: root, rel: s.rel, path: filepath.Join(root, filepath.FromSlash(s.rel))}
		if w.err = wiringSafePath(root, s.rel); w.err == nil {
			data, err := os.ReadFile(w.path)
			switch {
			case err == nil:
				w.old, w.exists = data, true
			case !os.IsNotExist(err):
				w.err = err
			}
		}
		if w.err == nil {
			w.next, w.state, w.err = s.edit(w.old, w.exists)
		}
		files = append(files, w)
	}
	return files
}

func wiringSafePath(root, rel string) error {
	cur := root
	for _, part := range strings.Split(rel, "/") {
		cur = filepath.Join(cur, part)
		s, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if s.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink at %s; keep wiring inside the repository", cur)
		}
	}
	return nil
}

// printInitFiles prints the repository plan, including project.yaml. Existing
// files always show their changes; contents of new files are opt-in.
func printInitFiles(w io.Writer, files []wireFile, verbose bool) {
	fmt.Fprintln(w, "  "+initPresentation(w).style.Bold("Files")+" in this repository (brigada never commits them):")
	// Show the setup files first, then supporting edits such as git's exclude.
	ordered := make([]wireFile, 0, len(files))
	primary := []string{engine.ProjectConfigPath, operatorFile, operatorSkillFile, wiringMCPFile, wiringCodexFile, wiringSettingsFile, wiringRulesFile, wiringAgentsFile, wiringClaudeFile, wiringSkillFile}
	for _, rel := range primary {
		for _, f := range files {
			if f.rel == rel {
				ordered = append(ordered, f)
			}
		}
	}
	for _, f := range files {
		if !slices.Contains(primary, f.rel) {
			ordered = append(ordered, f)
		}
	}
	style := initPresentation(w).style
	pathWidth, stateWidth := 0, 0
	for _, f := range ordered {
		if f.exists || f.changed() || f.err != nil {
			pathWidth = max(pathWidth, len(f.rel))
			stateWidth = max(stateWidth, len(initFileState(f)))
		}
	}
	for _, f := range ordered {
		if !f.exists && !f.changed() && f.err == nil {
			continue
		}
		purpose := f.purpose
		switch {
		case purpose != "":
		case f.rel == engine.ProjectConfigPath:
			purpose = "the settings above"
		case f.rel == operatorFile:
			purpose = "tells your own coding agent how to work with brigada"
		case f.rel == operatorSkillFile:
			purpose = "the same guide as a Claude Code skill"
		case f.rel == wiringMCPFile:
			purpose = "removes the brigada server an older init added"
		case f.rel == wiringCodexFile:
			purpose = "removes the brigada table an older init added"
		case f.rel == wiringSettingsFile:
			purpose = "removes the Stop hook an older init added"
		case f.rel == wiringSharedSettingsFile:
			purpose = "removes the legacy brigada hooks an older init wrote"
		case f.rel == wiringRulesFile:
			purpose = "deletes the foreman rules an older init wrote (brigada's own file)"
		case f.rel == wiringAgentsFile:
			purpose = "removes the marked router block an older init added"
		case f.rel == wiringClaudeFile:
			purpose = "removes the marked import an older init added"
		case f.rel == wiringSkillFile:
			purpose = "deletes the old skill carrying brigada's marker"
		default:
			purpose = "removes the exclude line that hid the private Claude Code settings"
		}
		state := initFileState(f)
		var colored string
		switch {
		case f.err != nil:
			colored = style.Bad(state)
		case !f.exists:
			colored = style.OK(state)
		case f.changed():
			colored = style.Warn(state)
		default:
			colored = style.Dim(state)
		}
		prefix := "    " + initPad(colored, stateWidth) + "  " + fmt.Sprintf("%-*s", pathWidth, f.rel) + "  "
		if f.err != nil {
			// The reason takes the row's own line, where the purpose goes.
			printInitWrapped(w, prefix, strings.Repeat(" ", term.VisibleWidth(prefix)), f.err.Error())
			prefix = strings.Repeat(" ", term.VisibleWidth(prefix))
		}
		printInitWrapped(w, prefix, strings.Repeat(" ", term.VisibleWidth(prefix)), style.Dim(purpose))
		if f.changed() && f.next != nil {
			if f.exists {
				if f.rel == engine.ProjectConfigPath {
					printInitDiff(w, f.old, f.next)
				} else {
					printFileDiff(w, f.rel, f.old, f.next, true)
				}
			} else if verbose && !f.generated {
				for _, line := range splitDiffLines(f.next) {
					if f.rel == engine.ProjectConfigPath {
						printInitFileLine(w, "      ", line)
					} else {
						fmt.Fprintln(w, "      "+line)
					}
				}
			}
		}
	}
	fmt.Fprintln(w, "    "+style.Dim("(show full contents: --verbose)"))
}

func initFileState(f wireFile) string {
	switch {
	case f.err != nil:
		return "left alone"
	case !f.exists:
		return "new"
	case f.changed() && f.next == nil:
		return "removed"
	case f.changed():
		return "changes"
	}
	return "unchanged"
}

// printFileDiff prints a unified-style hunk of the lines that differ plus two
// lines of context.
func printFileDiff(w io.Writer, name string, old, next []byte, exists bool) {
	a, b := splitDiffLines(old), splitDiffLines(next)
	if !exists {
		a = nil
	}
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	from := max(pre-2, 0)
	to := min(suf, 2)
	oldName := "a/" + name
	if !exists {
		oldName = "/dev/null"
	}
	fmt.Fprintf(w, "      --- %s\n      +++ b/%s\n      @@ -%d,%d +%d,%d @@\n", oldName, name, from+1, len(a)-suf+to-from, from+1, len(b)-suf+to-from)
	for _, l := range a[from:pre] {
		fmt.Fprintln(w, "       "+l)
	}
	for _, l := range a[pre : len(a)-suf] {
		fmt.Fprintln(w, "      -"+l)
	}
	for _, l := range b[pre : len(b)-suf] {
		fmt.Fprintln(w, "      +"+l)
	}
	for _, l := range a[len(a)-suf : len(a)-suf+to] {
		fmt.Fprintln(w, "       "+l)
	}
}

func splitDiffLines(d []byte) []string {
	if len(d) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(d), "\n"), "\n")
}

// applyWiring writes every changed, undamaged file, and deletes the ones a
// removal plan empties. A file edited since the plan was printed is
// refused, never overwritten.
func applyWiring(files []wireFile) error {
	for _, f := range files {
		if !f.changed() {
			continue
		}
		if f.next == nil {
			if err := checkRemovalFile(f, "init"); err != nil {
				return fmt.Errorf("wiring %s: %w", f.rel, err)
			}
			if err := os.Remove(f.path); err != nil {
				return fmt.Errorf("wiring %s: %w", f.rel, err)
			}
			removeEmptyWiringParents(f.root, f.path)
			continue
		}
		if err := writeInitFile(f.path, f.old, f.next, f.exists); err != nil {
			return fmt.Errorf("wiring %s: %w", f.rel, err)
		}
	}
	return nil
}

// mergeSkillMarked owns the whole file, but only when it carries the given
// ownership marker; any other file at the path is the user's and is left
// alone.
func mergeSkillMarked(old []byte, exists bool, want []byte, marker string) ([]byte, string, error) {
	if !exists {
		return want, "created", nil
	}
	if !bytes.Contains(old, []byte(marker)) {
		return nil, "", errors.New("exists and is not brigada's (no brigada marker); rename it or remove it to let init manage the file")
	}
	if bytes.Equal(old, want) {
		return old, "unchanged", nil
	}
	return want, "replaced", nil
}

var errDamaged = errors.New("damaged or unexpected JSON")

func damaged(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errDamaged, fmt.Sprintf(format, args...))
}

// mergeMCPEntry adds or replaces the server called name under mcpServers
// with the `brigada` command and args; every other byte stays.
func mergeMCPEntry(d []byte, exists bool, name string, args []string) ([]byte, string, error) {
	ours := mcpBridge{Command: "brigada", Args: args}
	if !exists {
		out, err := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{name: ours}}, "", "  ")
		return append(out, '\n'), "created", err
	}
	root, err := jsonRoot(d)
	if err != nil {
		return nil, "", err
	}
	mi := root.lookup("mcpServers")
	if mi < 0 {
		out, err := jsonInsert(d, root, "mcpServers", map[string]any{name: ours})
		return out, "added", err
	}
	servers, err := jsonSub(d, root, mi, '{', "mcpServers")
	if err != nil {
		return nil, "", err
	}
	bi := servers.lookup(name)
	if bi < 0 {
		out, err := jsonInsert(d, servers, name, ours)
		return out, "added", err
	}
	span := servers.vals[bi]
	if jsonEqual(d[span.start:span.end], ours) {
		return d, "unchanged", nil
	}
	out, err := jsonReplace(d, span, ours)
	return out, "replaced", err
}

// removeSharedHooks migrates only exact brigada commands out of shared
// settings. Commands with user-supplied flags and all other bytes stay put.
func removeSharedHooks(d []byte, exists bool) ([]byte, string, error) {
	if !exists {
		return d, "unchanged", nil
	}
	state := "unchanged"
	for {
		next, removed, err := removeSharedHook(d)
		if err != nil {
			return nil, "", err
		}
		if !removed {
			return d, state, nil
		}
		d, state = next, "removed"
	}
}

func removeSharedHook(d []byte) ([]byte, bool, error) {
	root, err := jsonRoot(d)
	if err != nil {
		return nil, false, err
	}
	hi := root.lookup("hooks")
	if hi < 0 {
		return d, false, nil
	}
	hooks, err := jsonSub(d, root, hi, '{', "hooks")
	if err != nil {
		return nil, false, err
	}
	for ei, event := range hooks.keys {
		list, err := jsonSub(d, hooks, ei, '[', "hooks."+event)
		if err != nil {
			return nil, false, err
		}
		for gi, gspan := range list.vals {
			if d[gspan.start] != '{' {
				continue
			}
			group, err := jsonParse(d, gspan.start)
			if err != nil {
				return nil, false, err
			}
			ii := group.lookup("hooks")
			if ii < 0 || d[group.vals[ii].start] != '[' {
				continue
			}
			inner, err := jsonParse(d, group.vals[ii].start)
			if err != nil {
				return nil, false, err
			}
			for i, hspan := range inner.vals {
				var hook map[string]json.RawMessage
				var command string
				if json.Unmarshal(d[hspan.start:hspan.end], &hook) != nil || json.Unmarshal(hook["command"], &command) != nil || (command != hookYieldCommand && command != hookConfirmCommand) {
					continue
				}
				if len(inner.vals) > 1 {
					return jsonRemove(d, inner, i), true, nil
				}
				if len(list.vals) > 1 {
					return jsonRemove(d, list, gi), true, nil
				}
				return jsonRemove(d, hooks, ei), true, nil
			}
		}
	}
	return d, false, nil
}

// --- minimal JSON editing: offsets into the original bytes -------------

type jsonSpan struct{ start, end int }

type jsonContainer struct {
	open, close int
	obj         bool
	keys        []string
	keyStarts   []int
	vals        []jsonSpan
}

// jsonRemove drops a member or element and its adjacent separator, preserving
// every surviving value and its formatting.
func jsonRemove(d []byte, c *jsonContainer, i int) []byte {
	start, end := c.vals[i].start, c.vals[i].end
	if c.obj {
		start = c.keyStarts[i]
	}
	if i+1 < len(c.vals) {
		end = c.vals[i+1].start
		if c.obj {
			end = c.keyStarts[i+1]
		}
	} else if i > 0 {
		start = c.vals[i-1].end
	}
	out := append([]byte(nil), d[:start]...)
	return append(out, d[end:]...)
}

func (c *jsonContainer) lookup(key string) int {
	found := -1
	for i, k := range c.keys {
		if k == key {
			found = i // like decoders, the last duplicate wins
		}
	}
	return found
}

func jsonSkipWS(d []byte, i int) int {
	for i < len(d) && (d[i] == ' ' || d[i] == '\t' || d[i] == '\n' || d[i] == '\r') {
		i++
	}
	return i
}

func jsonStringEnd(d []byte, i int) (int, error) {
	for j := i + 1; j < len(d); j++ {
		switch d[j] {
		case '\\':
			j++
		case '"':
			return j + 1, nil
		}
	}
	return 0, damaged("unterminated string")
}

func jsonValueEnd(d []byte, i int) (int, error) {
	if i >= len(d) {
		return 0, damaged("truncated")
	}
	switch d[i] {
	case '"':
		return jsonStringEnd(d, i)
	case '{', '[':
		c, err := jsonParse(d, i)
		if err != nil {
			return 0, err
		}
		return c.close + 1, nil
	}
	j := i
	for j < len(d) && !strings.ContainsRune(" \t\r\n,]}", rune(d[j])) {
		j++
	}
	if j == i {
		return 0, damaged("unexpected character at offset %d", i)
	}
	return j, nil
}

// jsonParse lists the members or elements of the object or array opening at
// open. The caller has already validated the document with json.Valid.
func jsonParse(d []byte, open int) (*jsonContainer, error) {
	c := &jsonContainer{open: open, obj: d[open] == '{'}
	closer := byte(']')
	if c.obj {
		closer = '}'
	}
	i := jsonSkipWS(d, open+1)
	for {
		if i >= len(d) {
			return nil, damaged("truncated")
		}
		if d[i] == closer {
			c.close = i
			return c, nil
		}
		if c.obj {
			if d[i] != '"' {
				return nil, damaged("expected a key at offset %d", i)
			}
			ke, err := jsonStringEnd(d, i)
			if err != nil {
				return nil, err
			}
			var key string
			if err := json.Unmarshal(d[i:ke], &key); err != nil {
				return nil, damaged("bad key at offset %d", i)
			}
			c.keys = append(c.keys, key)
			c.keyStarts = append(c.keyStarts, i)
			i = jsonSkipWS(d, ke)
			if i >= len(d) || d[i] != ':' {
				return nil, damaged("expected ':' at offset %d", i)
			}
			i = jsonSkipWS(d, i+1)
		}
		ve, err := jsonValueEnd(d, i)
		if err != nil {
			return nil, err
		}
		c.vals = append(c.vals, jsonSpan{i, ve})
		i = jsonSkipWS(d, ve)
		if i < len(d) && d[i] == ',' {
			i = jsonSkipWS(d, i+1)
		}
	}
}

func jsonRoot(d []byte) (*jsonContainer, error) {
	if !json.Valid(d) {
		return nil, damaged("not valid JSON")
	}
	i := jsonSkipWS(d, 0)
	if i >= len(d) || d[i] != '{' {
		return nil, damaged("top level is not an object")
	}
	return jsonParse(d, i)
}

// jsonSub parses member idx of parent, which must be of the wanted kind.
func jsonSub(d []byte, parent *jsonContainer, idx int, kind byte, path string) (*jsonContainer, error) {
	start := parent.vals[idx].start
	if d[start] != kind {
		return nil, damaged("%q is not an %s", path, map[byte]string{'{': "object", '[': "array"}[kind])
	}
	return jsonParse(d, start)
}

func jsonEqual(raw []byte, want any) bool {
	var a, b any
	if json.Unmarshal(raw, &a) != nil {
		return false
	}
	enc, err := json.Marshal(want)
	if err != nil || json.Unmarshal(enc, &b) != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}

// lineLead is the leading whitespace of the line holding pos.
func lineLead(d []byte, pos int) string {
	s := bytes.LastIndexByte(d[:pos], '\n') + 1
	e := s
	for e < len(d) && (d[e] == ' ' || d[e] == '\t') {
		e++
	}
	return string(d[s:e])
}

// ownLine reports the indent of pos when it is the first thing on its line.
func ownLine(d []byte, pos int) (string, bool) {
	s := bytes.LastIndexByte(d[:pos], '\n') + 1
	if s == 0 || len(bytes.Trim(d[s:pos], " \t")) != 0 {
		return "", false
	}
	return string(d[s:pos]), true
}

func render(v any, indent, unit string, multi bool) ([]byte, error) {
	if multi {
		return json.MarshalIndent(v, indent, unit)
	}
	return json.Marshal(v)
}

// jsonInsert appends a member (key, for objects) or element to c, matching
// the container's own layout. The rest of the document is untouched.
func jsonInsert(d []byte, c *jsonContainer, key string, v any) ([]byte, error) {
	base := lineLead(d, c.open)
	indent, unit, multi := base+"  ", "  ", true
	if len(c.vals) > 0 {
		first := c.vals[0].start
		if c.obj {
			first = c.keyStarts[0]
		}
		if ind, ok := ownLine(d, first); ok {
			indent = ind
			if strings.HasPrefix(ind, base) && len(ind) > len(base) {
				unit = ind[len(base):]
			}
		} else {
			multi = false
		}
	}
	body, err := render(v, indent, unit, multi)
	if err != nil {
		return nil, err
	}
	item := string(body)
	if c.obj {
		k, _ := json.Marshal(key)
		sep := ":"
		if multi {
			sep = ": "
		}
		item = string(k) + sep + item
	}
	var out bytes.Buffer
	if len(c.vals) == 0 {
		out.Write(d[:c.open+1])
		out.WriteString("\n" + indent + item + "\n" + base)
		out.Write(d[c.close:])
		return out.Bytes(), nil
	}
	at := c.vals[len(c.vals)-1].end
	out.Write(d[:at])
	if multi {
		out.WriteString(",\n" + indent + item)
	} else {
		out.WriteString("," + item)
	}
	out.Write(d[at:])
	return out.Bytes(), nil
}

// jsonReplace swaps one value for v in place, keeping its surroundings.
func jsonReplace(d []byte, span jsonSpan, v any) ([]byte, error) {
	multi := bytes.IndexByte(d[span.start:span.end], '\n') >= 0
	base := lineLead(d, span.start)
	body, err := render(v, base, "  ", multi)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.Write(d[:span.start])
	out.Write(body)
	out.Write(d[span.end:])
	return out.Bytes(), nil
}
