package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/mcp"
)

// docs/COMPATIBILITY.md freezes the CLI commands and flags and the MCP tools
// until 2.0. These tests read those lists from the document and compare them
// with what the binary registers, so neither can drift from the other. The
// parser is strict: a section it cannot find, or one that yields nothing,
// fails with the section's name rather than comparing empty lists.

// compatDoc is absolute, resolved at start: other tests change directory.
var compatDoc = func() string {
	p, err := filepath.Abs("../../docs/COMPATIBILITY.md")
	if err != nil {
		panic(err)
	}
	return p
}()

const (
	secCommands    = "**The CLI commands and their subcommands.**"
	secFlags       = "**The CLI flags.**"
	secForeman     = "**The foreman's MCP tools.**"
	secClient      = "**The client's MCP tools.**"
	secCoordinator = "**The coordinator's MCP tools**"
)

// commonFlagNames are on every command and are not listed per command.
var commonFlagNames = []string{"state-dir", "config"}

// unfrozenFlags names, by command path, the flags a command registers that
// 1.0 does not freeze, each with a one-line reason. A flag the CLI registers
// must be documented as frozen or be named here, so a new flag forces the
// choice.
var unfrozenFlags = map[string][]string{}

// unfrozenToolArgs names arguments a tool's schema offers that the document
// does not list, by tool, each with a reason.
var unfrozenToolArgs = map[string][]string{
	// The coordinator reuses one schema for plan_approve and plan_reject; a
	// rejection has no rule to change, so the document lists it for approve only.
	"plan_reject": {"and_stop_asking"},
}

var (
	tokenRE   = regexp.MustCompile("`([^`]+)`")
	argTokRE  = regexp.MustCompile("`([^`]+)`|\\((?:both |all )?required\\)")
	toolHeadR = regexp.MustCompile("^- `(\\w+)`( \\(experimental\\))?: (.*)$")
)

// docSection returns the marker's line and the lines after it, up to the next
// bold paragraph or heading.
func docSection(doc, marker string) ([]string, error) {
	lines := strings.Split(doc, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, marker) {
			if start >= 0 {
				return nil, fmt.Errorf("section %s: found twice", marker)
			}
			start = i
		}
	}
	if start < 0 {
		return nil, fmt.Errorf("section %s: not found in %s", marker, compatDoc)
	}
	out := []string{lines[start]}
	for _, l := range lines[start+1:] {
		if strings.HasPrefix(l, "**") || strings.HasPrefix(l, "## ") {
			break
		}
		out = append(out, l)
	}
	return out, nil
}

// docBullets returns the section's bullet lines; any other non-blank line
// after the first is a structure change and an error.
func docBullets(marker string, sec []string) ([]string, error) {
	var out []string
	for _, l := range sec[1:] {
		switch {
		case strings.TrimSpace(l) == "":
		case strings.HasPrefix(l, "- "):
			out = append(out, l)
		default:
			return nil, fmt.Errorf("section %s: unexpected line %q (want a bullet)", marker, l)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("section %s: no bullets", marker)
	}
	return out, nil
}

func backticked(s string) []string {
	var out []string
	for _, m := range tokenRE.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

type docCommands struct {
	top  []string // top-level command names
	subs []string // "parent sub" paths
}

func parseDocCommands(doc string) (docCommands, error) {
	sec, err := docSection(doc, secCommands)
	if err != nil {
		return docCommands{}, err
	}
	text := strings.Join(sec, " ")
	i := strings.Index(text, "The top-level commands are ")
	j := strings.Index(text, "The subcommands are ")
	k := strings.Index(text, "A bare ")
	if i < 0 || j < i || k < j {
		return docCommands{}, fmt.Errorf("section %s: want 'The top-level commands are ...', 'The subcommands are ...' and 'A bare ...' in order", secCommands)
	}
	dc := docCommands{top: backticked(text[i:j]), subs: backticked(text[j:k])}
	if len(dc.top) < 20 || len(dc.subs) < 10 {
		return dc, fmt.Errorf("section %s: parsed only %d commands and %d subcommands", secCommands, len(dc.top), len(dc.subs))
	}
	for _, s := range dc.subs {
		if strings.Count(s, " ") != 1 {
			return dc, fmt.Errorf("section %s: subcommand %q is not 'parent sub'", secCommands, s)
		}
	}
	return dc, nil
}

// parseDocFlags returns the frozen flags by command path. A backticked token
// that is not a flag starts a group of commands; the flags after it belong to
// the whole group.
func parseDocFlags(doc string) (map[string][]string, error) {
	sec, err := docSection(doc, secFlags)
	if err != nil {
		return nil, err
	}
	bullets, err := docBullets(secFlags, sec)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, b := range bullets {
		var group []string
		lastWasFlag := true
		for _, tok := range backticked(b) {
			if strings.HasPrefix(tok, "--") {
				if len(group) == 0 {
					return nil, fmt.Errorf("section %s: flag %s before any command in %q", secFlags, tok, b)
				}
				for _, p := range group {
					out[p] = append(out[p], tok)
				}
				lastWasFlag = true
				continue
			}
			if lastWasFlag {
				group = nil
			}
			group = append(group, tok)
			if _, ok := out[tok]; !ok {
				out[tok] = nil
			}
			lastWasFlag = false
		}
		if len(group) == 0 {
			return nil, fmt.Errorf("section %s: no command in %q", secFlags, b)
		}
	}
	if len(out) < 25 {
		return nil, fmt.Errorf("section %s: parsed only %d commands", secFlags, len(out))
	}
	return out, nil
}

type docTool struct {
	args, required         []string
	taskArgs, taskRequired []string // plan_create's per-task fields
}

// parseArgSentence reads one sentence of "`a` (required), `b` and `c`": the
// names and which of them are required.
func parseArgSentence(s string) (names, required []string) {
	for _, m := range argTokRE.FindAllStringSubmatch(s, -1) {
		switch {
		case m[1] != "":
			names = append(names, m[1])
		case len(names) == 0:
		case m[0] == "(required)":
			required = append(required, names[len(names)-1])
		default: // (both required) / (all required)
			required = append(required, names...)
		}
	}
	return names, required
}

func parseDocTools(doc, marker string) (map[string]docTool, error) {
	sec, err := docSection(doc, marker)
	if err != nil {
		return nil, err
	}
	bullets, err := docBullets(marker, sec)
	if err != nil {
		return nil, err
	}
	out := map[string]docTool{}
	for _, b := range bullets {
		m := toolHeadR.FindStringSubmatch(b)
		if m == nil {
			return nil, fmt.Errorf("section %s: bullet %q is not '- `tool`: arguments'", marker, b)
		}
		name, text := m[1], strings.TrimSuffix(strings.TrimSpace(m[3]), ".")
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("section %s: tool %s listed twice", marker, name)
		}
		var dt docTool
		if !strings.HasPrefix(text, "no arguments") {
			// A sentence is a tool-argument sentence when it opens with a
			// name or "It may take" and does not say "takes" (prose about a
			// nested field); a later one is prose and is skipped.
			for _, s := range strings.Split(text, ". ") {
				if strings.HasPrefix(s, "Each task takes ") {
					dt.taskArgs, dt.taskRequired = parseArgSentence(s)
					continue
				}
				if !(strings.HasPrefix(s, "`") || strings.HasPrefix(s, "It may take")) || strings.Contains(s, " takes ") {
					continue
				}
				a, r := parseArgSentence(s)
				dt.args = append(dt.args, a...)
				dt.required = append(dt.required, r...)
			}
			if len(dt.args) == 0 {
				return nil, fmt.Errorf("section %s: tool %s: no arguments found in %q", marker, name, text)
			}
		}
		out[name] = dt
	}
	return out, nil
}

// diffSets compares a documented list with a defined one. allowed names
// defined entries the document deliberately leaves out.
func diffSets(scope string, documented, defined, allowed []string) []string {
	var out []string
	for _, d := range documented {
		if !slices.Contains(defined, d) {
			out = append(out, fmt.Sprintf("%s: documented %q is not defined", scope, d))
		}
	}
	for _, d := range defined {
		if !slices.Contains(documented, d) && !slices.Contains(allowed, d) {
			out = append(out, fmt.Sprintf("%s: defined %q is neither documented nor allowlisted as unfrozen", scope, d))
		}
	}
	for _, a := range allowed {
		if !slices.Contains(defined, a) {
			out = append(out, fmt.Sprintf("%s: allowlisted %q is not defined", scope, a))
		}
		if slices.Contains(documented, a) {
			out = append(out, fmt.Sprintf("%s: %q is both documented and allowlisted", scope, a))
		}
	}
	return out
}

func readCompatDoc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(compatDoc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func report(t *testing.T, problems []string) {
	t.Helper()
	for _, p := range problems {
		t.Error(p)
	}
}

// helpFlagSet runs `brigada <path> --help` and returns the flag set the
// command registered under that name, nil for a dispatcher.
func helpFlagSet(t *testing.T, path string) *flag.FlagSet {
	t.Helper()
	sets := map[string]*flag.FlagSet{}
	factory := newFlagSet
	newFlagSet = func(name, synopsis, description string) *flag.FlagSet {
		fs := factory(name, synopsis, description)
		sets[name] = fs
		return fs
	}
	defer func() { newFlagSet = factory }()
	var out, errb bytes.Buffer
	if code := run(context.Background(), append(strings.Fields(path), "--help"), &out, &errb); code != 0 {
		t.Errorf("brigada %s --help: exit %d, stderr %q", path, code, errb.String())
		return nil
	}
	if !strings.HasPrefix(out.String(), "Usage: brigada "+path) {
		t.Errorf("brigada %s --help does not print that command's usage: %q", path, out.String())
	}
	return sets[path]
}

func TestCompatCommands(t *testing.T) {
	dc, err := parseDocCommands(readCompatDoc(t))
	if err != nil {
		t.Fatal(err)
	}
	var topDefined, subTabled []string
	for name, cmd := range commands {
		topDefined = append(topDefined, name)
		for sub := range cmd.subcommands {
			subTabled = append(subTabled, name+" "+sub)
		}
	}
	// help is handled by run itself, and hold by runKeptHold.
	topDefined = append(topDefined, "help")
	known := slices.Clone(subTabled)
	for _, p := range dc.subs {
		switch {
		case strings.HasPrefix(p, "hold "):
			if !runKeptHold(strings.Fields(p)) {
				t.Errorf("documented %q is not kept by runKeptHold", p)
			}
		case !slices.Contains(subTabled, p):
			// land approve, agents add: handled inside their command, so the
			// help is the only proof they exist.
			helpFlagSet(t, p)
		}
		known = append(known, p)
	}
	report(t, diffSets("top-level commands", dc.top, topDefined, nil))
	for _, p := range subTabled {
		if !slices.Contains(dc.subs, p) {
			t.Errorf("subcommands: defined %q is not documented", p)
		}
	}
}

func TestCompatFlags(t *testing.T) {
	doc := readCompatDoc(t)
	dc, err := parseDocCommands(doc)
	if err != nil {
		t.Fatal(err)
	}
	documented, err := parseDocFlags(doc)
	if err != nil {
		t.Fatal(err)
	}
	// Every command that can take flags: those the document lists, and the
	// leaves of the command tables. A dispatcher has no flags of its own.
	paths := map[string]bool{}
	for p := range documented {
		paths[p] = true
	}
	for name, cmd := range commands {
		if len(cmd.subcommands) == 0 {
			paths[name] = true
		}
		for sub := range cmd.subcommands {
			paths[name+" "+sub] = true
		}
	}
	for p := range paths {
		if !slices.Contains(dc.top, p) && !slices.Contains(dc.subs, p) {
			t.Errorf("flags: %q is not a documented command", p)
		}
	}
	for p := range unfrozenFlags {
		if !paths[p] {
			t.Errorf("unfrozenFlags names %q, which is not a command", p)
		}
	}
	for p := range paths {
		if strings.HasPrefix(p, "hold ") {
			if len(documented[p]) != 0 {
				t.Errorf("flags: %q takes no flags but the document lists %v", p, documented[p])
			}
			continue
		}
		fs := helpFlagSet(t, p)
		var defined []string
		if fs != nil {
			for _, common := range commonFlagNames {
				if fs.Lookup(common) == nil {
					t.Errorf("flags: %q lacks the common flag --%s", p, common)
				}
			}
			fs.VisitAll(func(f *flag.Flag) {
				if !slices.Contains(commonFlagNames, f.Name) {
					defined = append(defined, "--"+f.Name)
				}
			})
		}
		var allowed []string
		for _, f := range unfrozenFlags[p] {
			allowed = append(allowed, "--"+f)
		}
		report(t, diffSets("flags of "+p, documented[p], defined, allowed))
	}
}

// toolShape is one tool as tools/list shows it.
type toolShape struct {
	args, required         []string
	taskArgs, taskRequired []string
}

func listTools(t *testing.T, offer []mcp.Tool) map[string]toolShape {
	t.Helper()
	var out bytes.Buffer
	srv := &mcp.Server{Tools: offer}
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n")
	if err := srv.Serve(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				InputSchema struct {
					Properties map[string]struct {
						Items struct {
							Properties map[string]json.RawMessage `json:"properties"`
							Required   []string                   `json:"required"`
						} `json:"items"`
					} `json:"properties"`
					Required []string `json:"required"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("tools/list answer %q: %v", out.String(), err)
	}
	res := map[string]toolShape{}
	for _, tl := range resp.Result.Tools {
		var s toolShape
		for p := range tl.InputSchema.Properties {
			s.args = append(s.args, p)
		}
		s.required = tl.InputSchema.Required
		if tasks, ok := tl.InputSchema.Properties["tasks"]; ok && tl.Name == "plan_create" {
			for p := range tasks.Items.Properties {
				s.taskArgs = append(s.taskArgs, p)
			}
			s.taskRequired = tasks.Items.Required
		}
		res[tl.Name] = s
	}
	return res
}

func mapNames[V any](m map[string]V) []string {
	var out []string
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func compareTools(documented map[string]docTool, defined map[string]toolShape) []string {
	out := diffSets("tools", mapNames(documented), mapNames(defined), nil)
	for name, d := range documented {
		s, ok := defined[name]
		if !ok {
			continue
		}
		out = append(out, diffSets("arguments of "+name, d.args, s.args, unfrozenToolArgs[name])...)
		out = append(out, diffSets("required arguments of "+name, d.required, s.required, nil)...)
		if name == "plan_create" {
			out = append(out, diffSets("task fields of plan_create", d.taskArgs, s.taskArgs, nil)...)
			out = append(out, diffSets("required task fields of plan_create", d.taskRequired, s.taskRequired, nil)...)
		}
	}
	return out
}

func TestCompatMCPTools(t *testing.T) {
	doc := readCompatDoc(t)
	for _, c := range []struct {
		marker string
		offer  []mcp.Tool
	}{
		{secForeman, nil},
		{secClient, mcp.ClientTools},
		{secCoordinator, mcp.CoordinatorTools},
	} {
		documented, err := parseDocTools(doc, c.marker)
		if err != nil {
			t.Error(err)
			continue
		}
		for _, p := range compareTools(documented, listTools(t, c.offer)) {
			t.Errorf("%s: %s", c.marker, p)
		}
	}
}

// The comparison can fail, and the parser fails loudly on a changed document.
func TestCompatSelfCheck(t *testing.T) {
	probs := diffSets("flags of x", []string{"--a", "--gone"}, []string{"--a", "--extra", "--quiet"}, []string{"--quiet"})
	want := []string{
		`flags of x: documented "--gone" is not defined`,
		`flags of x: defined "--extra" is neither documented nor allowlisted as unfrozen`,
	}
	if !slices.Equal(probs, want) {
		t.Errorf("diffSets = %q, want %q", probs, want)
	}
	if p := diffSets("flags of x", []string{"--a"}, []string{"--a"}, []string{"--gone"}); len(p) != 1 {
		t.Errorf("stale allowlist entry not reported: %q", p)
	}
	tp := compareTools(
		map[string]docTool{"status": {args: []string{"all"}}},
		map[string]toolShape{"status": {args: []string{"all"}}, "surprise": {}},
	)
	if len(tp) != 1 || !strings.Contains(tp[0], `"surprise" is neither documented`) {
		t.Errorf("extra tool not reported: %q", tp)
	}
	tp = compareTools(map[string]docTool{"status": {args: []string{"all", "gone"}}}, map[string]toolShape{"status": {args: []string{"all"}}})
	if len(tp) != 1 {
		t.Errorf("missing tool argument not reported: %q", tp)
	}
	for _, c := range []struct{ name, doc, want string }{
		{"no flags section", "# x\n", secFlags},
		{"empty flags section", secFlags + " text\n\nno bullets\n", secFlags},
		{"no tools section", "# x\n", secForeman},
		{"bad tool bullet", secForeman + "\n\n- not a tool\n", secForeman},
	} {
		var err error
		if c.want == secForeman {
			_, err = parseDocTools(c.doc, c.want)
		} else {
			_, err = parseDocFlags(c.doc)
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v does not name section %s", c.name, err, c.want)
		}
	}
	if _, err := parseDocCommands("# x\n"); err == nil || !strings.Contains(err.Error(), secCommands) {
		t.Errorf("missing commands section: error %v", err)
	}
}
