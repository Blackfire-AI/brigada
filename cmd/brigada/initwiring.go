package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/term"
)

// Harness wiring for mode A (design §13.1 item 7, §4.6 "What the facts
// allow"): the bridge in the repository's .mcp.json, the Stop hook in
// .claude/settings.local.json, and the foreman instructions as the
// brigada-owned rules file routed to from AGENTS.md (initrouter.go). The
// files are edited minimally: only brigada's entries change, never the
// rest of the user's bytes.

const (
	wiringMCPFile            = ".mcp.json"
	wiringCodexFile          = ".codex/config.toml"
	wiringSettingsFile       = ".claude/settings.local.json"
	wiringSharedSettingsFile = ".claude/settings.json"
	wiringSkillMarker        = "<!-- brigada-managed: foreman-instructions"
	hookYieldCommand         = "brigada hold yield"
	hookConfirmCommand       = "brigada hold confirm"

	// wiringSkillFile is where an older init put the rules, as a skill;
	// init now removes it when it carries brigada's marker.
	wiringSkillFile = ".claude/skills/brigada-foreman/SKILL.md"
)

// initCodexText is init's closing word for Codex, after the wiring plan:
// init has written .codex/config.toml with brigada's table
// (planCodexWiring), which Codex loads only from a trusted folder — project
// config since Codex 0.78.0, trust-gated since 0.88.0, verified from Codex's
// documentation and history — so the user trusts the folder on the first
// codex run. The user-level command remains only as the fallback for Codex
// versions without project config.
func initCodexText(w io.Writer) string {
	style := initPresentation(w).style
	return "  " + style.Bold("Codex") + ": on the first codex run here, trust this folder when Codex asks\n" +
		"    (only a trusted folder loads .codex/config.toml), then ask Codex to plan\n" +
		"    the first task. " + style.Dim("On Codex older than 0.78 (no project config):") + "\n" +
		"    " + style.Accent("codex mcp add brigada-foreman -- brigada mcp --foreman") + "\n"
}

type mcpBridge struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

type hookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

type hookGroup struct {
	Hooks []hookCommand `json:"hooks"`
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
}

func (w wireFile) changed() bool {
	return w.err == nil && (!bytes.Equal(w.old, w.next) ||
		(w.exists && w.next == nil && w.state == "removed"))
}

// planWiring computes the Claude Code project-scope changes; it reads files
// and writes nothing.
func planWiring(root string) []wireFile {
	rules := rulesContent()
	layout := planRouterLayout(root)
	steps := []wireStep{
		{wiringMCPFile, func(d []byte, exists bool) ([]byte, string, error) { return mergeMCPServer(d, exists) }},
		{wiringSettingsFile, func(d []byte, exists bool) ([]byte, string, error) { return mergeHooks(d, exists) }},
		{wiringSharedSettingsFile, removeSharedHooks},
		{wiringRulesFile, func(d []byte, exists bool) ([]byte, string, error) {
			return mergeSkillMarked(d, exists, rules, wiringSkillMarker)
		}},
		{layout.router, func(d []byte, exists bool) ([]byte, string, error) { return mergeRouter(layout.router, d, exists) }},
	}
	if layout.imprt != "" {
		steps = append(steps, wireStep{layout.imprt, func(d []byte, exists bool) ([]byte, string, error) {
			return mergeImport(layout.imprt, d, exists)
		}})
	}
	steps = append(steps, wireStep{wiringSkillFile, retireSkill})
	files := planSteps(root, steps)
	for i := range files {
		switch f := &files[i]; f.rel {
		case layout.router:
			f.purpose = "a short block that makes your Claude session brigada's foreman"
		case wiringSkillFile:
			if f.changed() {
				f.purpose = "the rules of an older init; " + wiringRulesFile + " replaces it"
			} else {
				f.purpose = "yours (no brigada marker); left as it is"
			}
		}
	}
	if layout.link != "" {
		files = append(files, wireFile{root: root, rel: layout.link, path: filepath.Join(root, layout.link), exists: true, state: "unchanged",
			purpose: "a link to " + layout.router + ", which holds the block; no import needed"})
	}
	// Ignore the private settings before creating them. Include the exclude edit
	// in the reviewed plan and use the same concurrent-edit guard on apply.
	if settings := files[1]; !settings.exists && settings.changed() {
		if exclude := planSettingsExclude(root); exclude != nil {
			files = append([]wireFile{*exclude}, files...)
		}
	}
	return files
}

func planSettingsExclude(root string) *wireFile {
	err := exec.Command("git", "-C", root, "check-ignore", "-q", "--", wiringSettingsFile).Run()
	if err == nil {
		return nil
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		return &wireFile{rel: "git info/exclude", err: fmt.Errorf("checking private settings ignore: %w", err)}
	}
	out, err := exec.Command("git", "-C", root, "rev-parse", "--git-path", "info/exclude").Output()
	if err != nil {
		return &wireFile{rel: "git info/exclude", err: fmt.Errorf("locating exclude file: %w", err)}
	}
	path := strings.TrimSpace(string(out))
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	files := planSteps(filepath.Dir(path), []wireStep{{filepath.Base(path), func(d []byte, exists bool) ([]byte, string, error) {
		next := append([]byte(nil), d...)
		if len(next) > 0 && next[len(next)-1] != '\n' {
			next = append(next, '\n')
		}
		next = append(next, "/"+wiringSettingsFile+"\n"...)
		return next, "added", nil
	}}})
	f := files[0]
	f.rel = filepath.ToSlash(strings.TrimSpace(string(out)))
	return &f
}

// wireStep is one file a wiring plan edits: its path under the root and
// the edit that turns its current bytes into the wanted ones.
type wireStep struct {
	rel  string
	edit func([]byte, bool) ([]byte, string, error)
}

// planSteps reads each step's file under root and computes its edit;
// like planWiring it writes nothing.
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
	primary := []string{engine.ProjectConfigPath, wiringMCPFile, wiringCodexFile, wiringSettingsFile, wiringRulesFile, wiringAgentsFile, wiringClaudeFile, wiringSkillFile}
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
		case f.rel == wiringMCPFile:
			purpose = "lets Claude Code here use brigada (Claude Code asks you to approve it)"
		case f.rel == wiringCodexFile:
			purpose = "lets Codex here use brigada (Codex loads it once you trust this folder)"
		case f.rel == wiringSettingsFile:
			purpose = "private, kept out of git: when your Claude turn ends, brigada's foreman picks up what happened meanwhile"
		case f.rel == wiringRulesFile:
			purpose = "brigada's foreman rules for your Claude session (brigada's own file)"
		case f.rel == wiringClaudeFile:
			purpose = "imports AGENTS.md, so Claude Code reads that block"
		case f.rel == wiringSharedSettingsFile:
			purpose = "removes brigada hooks moved to private settings"
		default:
			purpose = "keeps private Claude Code settings out of git"
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
			} else if verbose {
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

// applyWiring writes every changed, undamaged file, and deletes the one an
// older init left behind. A file edited since the plan was printed is
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

func renderNew(v any) ([]byte, error) {
	out, err := json.MarshalIndent(v, "", "  ")
	return append(out, '\n'), err
}

// mergeMCPServer adds or replaces the "brigada" server under mcpServers.
func mergeMCPServer(d []byte, exists bool) ([]byte, string, error) {
	return mergeMCPEntry(d, exists, "brigada", []string{"mcp", "--foreman"})
}

// mergeMCPEntry adds or replaces the server called name under mcpServers
// with the `brigada` command and args; every other byte stays.
func mergeMCPEntry(d []byte, exists bool, name string, args []string) ([]byte, string, error) {
	ours := mcpBridge{Command: "brigada", Args: args}
	if !exists {
		out, err := renderNew(map[string]any{"mcpServers": map[string]any{name: ours}})
		return out, "created", err
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

// mergeHooks adds or replaces brigada's Stop hook in private settings.
func mergeHooks(d []byte, exists bool) ([]byte, string, error) {
	if !exists {
		hooks := map[string]any{"Stop": []hookGroup{{Hooks: []hookCommand{{Type: "command", Command: hookYieldCommand}}}}}
		out, err := renderNew(map[string]any{"hooks": hooks})
		return out, "created", err
	}
	return mergeHook(d, "Stop", hookYieldCommand)
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

func mergeHook(d []byte, event, command string) ([]byte, string, error) {
	ours := hookCommand{Type: "command", Command: command}
	group := hookGroup{Hooks: []hookCommand{ours}}
	root, err := jsonRoot(d)
	if err != nil {
		return nil, "", err
	}
	hi := root.lookup("hooks")
	if hi < 0 {
		out, err := jsonInsert(d, root, "hooks", map[string]any{event: []hookGroup{group}})
		return out, "added", err
	}
	hooks, err := jsonSub(d, root, hi, '{', "hooks")
	if err != nil {
		return nil, "", err
	}
	ei := hooks.lookup(event)
	if ei < 0 {
		out, err := jsonInsert(d, hooks, event, []hookGroup{group})
		return out, "added", err
	}
	list, err := jsonSub(d, hooks, ei, '[', "hooks."+event)
	if err != nil {
		return nil, "", err
	}
	for _, gspan := range list.vals {
		if d[gspan.start] != '{' {
			continue
		}
		g, err := jsonParse(d, gspan.start)
		if err != nil {
			return nil, "", err
		}
		gi := g.lookup("hooks")
		if gi < 0 || d[g.vals[gi].start] != '[' {
			continue
		}
		inner, err := jsonParse(d, g.vals[gi].start)
		if err != nil {
			return nil, "", err
		}
		for _, hspan := range inner.vals {
			if d[hspan.start] != '{' {
				continue
			}
			h, err := jsonParse(d, hspan.start)
			if err != nil {
				return nil, "", err
			}
			ci := h.lookup("command")
			if ci < 0 {
				continue
			}
			var have string
			if json.Unmarshal(d[h.vals[ci].start:h.vals[ci].end], &have) != nil {
				continue
			}
			if have != command && !strings.HasPrefix(have, command+" ") {
				continue
			}
			var typ string
			if ti := h.lookup("type"); ti >= 0 {
				_ = json.Unmarshal(d[h.vals[ti].start:h.vals[ti].end], &typ)
			}
			// A command the user tuned with flags of their own stays as it is.
			if typ == "command" {
				return d, "unchanged", nil
			}
			out, err := jsonReplace(d, hspan, ours)
			return out, "replaced", err
		}
	}
	out, err := jsonInsert(d, list, "", group)
	return out, "added", err
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
