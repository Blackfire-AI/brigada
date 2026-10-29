package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// The docs verifier. It checks, for GUIDE.md, COMPATIBILITY.md and
// CHANGELOG.md, that every relative link and heading anchor resolves, and
// for GUIDE.md's code blocks that every `brigada ...` line names a command
// that exists with flags that command accepts. A third part asserts what the
// docs must say about instructions and skills.
//
// FLAG METHOD. The command table (commands, and the subcommand tables of
// main.go and dispatch.go) holds names and no flags, and the flags are
// defined inline in each command's run function. The verifier executes no
// command, not even its flag parsing or --help: it reads the flag-definition
// code itself. flagSetsFromSource parses the package's own non-test Go files
// and, in every function that builds a set with `fs := newFlagSet("name",
// ...)`, collects the names given to that set's definer calls (fs.Bool,
// fs.StringVar, fs.Var, fs.Func, ...). A name built as "plan "+verb stands
// for every path under that prefix. Every set also gets the common
// --state-dir and --config, which addCommon defines on all of them, and -h
// and --help. A documented flag must be in its command's set, so the docs
// cannot drift from the code that defines the flags. A documented
// subcommand must be in the command's table, or in COMPATIBILITY.md's list
// of subcommands a command handles inside itself (land approve, agents add).

// docsFiles are the documents the verifier covers, relative to this package.
var docsFiles = []string{docPath("docs/GUIDE.md"), docPath("docs/COMPATIBILITY.md"), docPath("CHANGELOG.md")}

// repoRoot is absolute, resolved at start: other tests change directory.
var repoRoot, _ = filepath.Abs("../..")

// docPath is a repository file's absolute path.
func docPath(rel string) string { return filepath.Join(repoRoot, filepath.FromSlash(rel)) }

// requireNewDocs is flipped to true by the docs task, when GUIDE.md,
// COMPATIBILITY.md and CHANGELOG.md say what checkNewDocs asks for. While it
// is false the content assertions are proven against sample text only, so
// the verifier lands green before the docs do.
const requireNewDocs = true

// --- links and anchors -------------------------------------------------

var (
	linkRE    = regexp.MustCompile("!?\\[[^\\]]*\\]\\(([^)\\s]+)(?:\\s+\"[^\"]*\")?\\)")
	headingRE = regexp.MustCompile(`^#{1,6}\s+(.*?)\s*#*\s*$`)
	htmlIDRE  = regexp.MustCompile(`(?:id|name)="([^"]+)"`)
	codeSpan  = regexp.MustCompile("`[^`]*`")
	mdLinkRE  = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
)

// proseLines returns the lines of a markdown text outside fenced code blocks.
func proseLines(text string) []string {
	var out []string
	fenced := false
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			fenced = !fenced
			continue
		}
		if !fenced {
			out = append(out, l)
		}
	}
	return out
}

// slug is GitHub's heading anchor: formatting dropped, lowercase, every
// character but letters, digits, hyphens and underscores removed, spaces
// turned into hyphens.
func slug(heading string) string {
	h := mdLinkRE.ReplaceAllString(heading, "$1")
	h = strings.NewReplacer("`", "", "*", "").Replace(h)
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(h)) {
		switch {
		case r == ' ':
			b.WriteByte('-')
		case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// anchorsOf lists the anchors a markdown text offers: its headings' slugs,
// a repeated heading getting -1, -2 as GitHub does, and explicit HTML ids.
func anchorsOf(text string) map[string]bool {
	out := map[string]bool{}
	seen := map[string]int{}
	for _, l := range proseLines(text) {
		if m := headingRE.FindStringSubmatch(l); m != nil {
			s := slug(m[1])
			if n := seen[s]; n > 0 {
				out[fmt.Sprintf("%s-%d", s, n)] = true
			} else {
				out[s] = true
			}
			seen[s]++
		}
		for _, m := range htmlIDRE.FindAllStringSubmatch(l, -1) {
			out[strings.ToLower(m[1])] = true
		}
	}
	return out
}

// checkLinks resolves every relative link and anchor in the markdown files
// and returns one problem per failure, naming the file and the link.
func checkLinks(files []string) []string {
	var problems []string
	anchors := map[string]map[string]bool{}
	anchorsFor := func(path string) (map[string]bool, error) {
		if a, ok := anchors[path]; ok {
			return a, nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		anchors[path] = anchorsOf(string(b))
		return anchors[path], nil
	}
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", file, err))
			continue
		}
		for _, l := range proseLines(string(b)) {
			l = codeSpan.ReplaceAllString(l, "")
			for _, m := range linkRE.FindAllStringSubmatch(l, -1) {
				target := strings.Trim(m[1], "<>")
				if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
					continue
				}
				path, anchor, _ := strings.Cut(target, "#")
				dest := file
				if path != "" {
					dest = filepath.Join(filepath.Dir(file), filepath.FromSlash(path))
					if _, err := os.Stat(dest); err != nil {
						problems = append(problems, fmt.Sprintf("%s: link %q: %s does not exist", file, target, path))
						continue
					}
				}
				if anchor == "" || !strings.HasSuffix(dest, ".md") {
					continue
				}
				a, err := anchorsFor(dest)
				if err != nil {
					problems = append(problems, fmt.Sprintf("%s: link %q: %v", file, target, err))
				} else if !a[strings.ToLower(anchor)] {
					problems = append(problems, fmt.Sprintf("%s: link %q: %s has no heading with the anchor #%s", file, target, filepath.Base(dest), anchor))
				}
			}
		}
	}
	return problems
}

// --- brigada command lines in code blocks -----------------------------

// codeBlockCommands returns the `brigada ...` lines of a markdown text's
// fenced code blocks, continuation lines joined.
func codeBlockCommands(text string) []string {
	var out []string
	fenced := false
	pending := ""
	for _, l := range strings.Split(text, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") {
			fenced, pending = !fenced, ""
			continue
		}
		if !fenced {
			continue
		}
		t = strings.TrimPrefix(t, "$ ")
		if pending != "" {
			t = pending + " " + t
			pending = ""
		}
		if strings.HasSuffix(t, "\\") {
			pending = strings.TrimSpace(strings.TrimSuffix(t, "\\"))
			continue
		}
		if strings.HasPrefix(t, "brigada ") {
			out = append(out, t)
		}
	}
	return out
}

// shellWords splits a command line into words the way a shell would for
// this purpose: quotes group, a `#` that starts a word ends the line (an
// inline comment), and at bracket depth 0 a pipe, list or redirect operator
// ends the command. Inside the synopsis brackets of `[--probe | --yes]` the
// bar is an alternative, not a pipe.
func shellWords(line string) []string {
	var words []string
	var cur strings.Builder
	var quote rune
	inWord := false
	flush := func() {
		if inWord {
			words = append(words, cur.String())
		}
		cur.Reset()
		inWord = false
	}
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			flush()
		case r == '#' && !inWord:
			flush()
			return cutAtOperator(words)
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	flush()
	return cutAtOperator(words)
}

func cutAtOperator(words []string) []string {
	depth := 0
	for i, w := range words {
		if depth == 0 {
			switch {
			case w == "|", w == "||", w == "&&", w == ";", strings.HasPrefix(w, ">"), strings.HasPrefix(w, "<<"), strings.HasPrefix(w, "2>"):
				return words[:i]
			}
		}
		depth += strings.Count(w, "[") - strings.Count(w, "]")
	}
	return words
}

// docFlagName reads a flag token as the synopsis writes it ([--repo, --yes],
// --check=CMD) and returns its name, or "" for a word that is no flag.
func docFlagName(w string) string {
	w = strings.TrimLeft(w, "[(")
	w = strings.TrimRight(w, "])...,")
	if !strings.HasPrefix(w, "-") || w == "-" || w == "--" {
		return ""
	}
	name, _, _ := strings.Cut(strings.TrimLeft(w, "-"), "=")
	return name
}

var plainWord = regexp.MustCompile(`^[a-z][a-z-]*$`)

// commandChecker resolves documented command lines against the code.
type commandChecker struct {
	t    *testing.T
	subs []string // "parent sub" paths COMPATIBILITY.md documents
	sets []sourceFlagSet
}

func newCommandChecker(t *testing.T) *commandChecker {
	t.Helper()
	dc, err := parseDocCommands(readCompatDoc(t))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if n := e.Name(); strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			b, err := os.ReadFile(filepath.Join(sourceDir, n))
			if err != nil {
				t.Fatal(err)
			}
			files[n] = string(b)
		}
	}
	sets, err := flagSetsFromSource(files)
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) < 25 {
		t.Fatalf("found only %d flag sets in the command sources", len(sets))
	}
	return &commandChecker{t: t, subs: dc.subs, sets: sets}
}

// sourceDir is this package's directory, absolute, resolved at start.
var sourceDir, _ = filepath.Abs(".")

// sourceFlagSet is one newFlagSet call's flags. prefix means the name ended
// in a variable ("plan "+verb): it stands for every path under name.
type sourceFlagSet struct {
	name   string
	prefix bool
	flags  map[string]bool
}

// flagNameArg is the position of the flag name among the arguments of each
// flag.FlagSet definer: first for Bool/String/Func..., second for the *Var
// forms and Var.
func flagNameArg(method string) (int, bool) {
	switch method {
	case "Bool", "String", "Int", "Int64", "Uint", "Uint64", "Float64", "Duration", "Func", "BoolFunc":
		return 0, true
	case "BoolVar", "StringVar", "IntVar", "Int64Var", "UintVar", "Uint64Var", "Float64Var", "DurationVar", "Var", "TextVar":
		return 1, true
	}
	return 0, false
}

// flagSetsFromSource reads Go source (file name to text) without running
// any of it and returns the flag names each newFlagSet call's set defines.
func flagSetsFromSource(files map[string]string) ([]sourceFlagSet, error) {
	var out []sourceFlagSet
	fset := token.NewFileSet()
	for name, src := range files {
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			return nil, err
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			// The variable each newFlagSet result is assigned to.
			sets := map[string]*sourceFlagSet{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
					return true
				}
				call, ok := as.Rhs[0].(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "newFlagSet" {
					return true
				}
				v, ok := as.Lhs[0].(*ast.Ident)
				if !ok {
					return true
				}
				set := &sourceFlagSet{flags: map[string]bool{"h": true, "help": true, "state-dir": true, "config": true}}
				switch a := call.Args[0].(type) {
				case *ast.BasicLit:
					set.name, _ = strconv.Unquote(a.Value)
				case *ast.BinaryExpr:
					if l, ok := a.X.(*ast.BasicLit); ok && a.Op == token.ADD {
						set.name, _ = strconv.Unquote(l.Value)
						set.prefix = true
					}
				}
				if set.name != "" {
					sets[v.Name] = set
				}
				return true
			})
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				recv, ok := sel.X.(*ast.Ident)
				if !ok || sets[recv.Name] == nil {
					return true
				}
				if i, ok := flagNameArg(sel.Sel.Name); ok && i < len(call.Args) {
					if lit, ok := call.Args[i].(*ast.BasicLit); ok {
						if s, err := strconv.Unquote(lit.Value); err == nil {
							sets[recv.Name].flags[s] = true
						}
					}
				}
				return true
			})
			for _, s := range sets {
				out = append(out, *s)
			}
		}
	}
	return out, nil
}

// flagsOf is the flag names a command path defines, from the source; nil
// for a dispatcher that defines none. A prefix set (plan approve and plan
// reject share one function) applies to every path under its prefix.
func (c *commandChecker) flagsOf(path string) map[string]bool {
	var out map[string]bool
	for _, s := range c.sets {
		if s.name == path || (s.prefix && strings.HasPrefix(path, s.name) && len(path) > len(s.name)) {
			if out == nil {
				out = map[string]bool{}
			}
			for f := range s.flags {
				out[f] = true
			}
		}
	}
	return out
}

// check returns the problems of one documented command line.
func (c *commandChecker) check(line string) []string {
	words := shellWords(line)
	if len(words) < 2 || words[0] != "brigada" || strings.HasPrefix(words[1], "-") {
		return nil
	}
	top := words[1]
	switch top {
	case "help", "hold":
		return nil
	}
	cmd, ok := commands[top]
	if !ok {
		return []string{fmt.Sprintf("%q: unknown command %q", line, top)}
	}
	path, rest := top, words[2:]
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		sub := rest[0]
		switch {
		case len(cmd.subcommands) > 0:
			if _, ok := cmd.subcommands[sub]; !ok {
				return []string{fmt.Sprintf("%q: %q is not a subcommand of %s", line, sub, top)}
			}
			path, rest = top+" "+sub, rest[1:]
		case plainWord.MatchString(sub) && slices.Contains(c.subs, top+" "+sub):
			path, rest = top+" "+sub, rest[1:]
		}
	} else if len(cmd.subcommands) > 0 && len(rest) == 0 {
		return nil // a bare dispatcher in a synopsis
	}
	known := c.flagsOf(path)
	var problems []string
	for _, w := range rest {
		name := docFlagName(w)
		if name == "" || known[name] {
			continue
		}
		problems = append(problems, fmt.Sprintf("%q: %s has no flag --%s", line, path, name))
	}
	return problems
}

// --- what the docs must say --------------------------------------------

// Headings GUIDE.md must have (matched case-insensitively against the
// heading text, curly apostrophes read as straight ones), and the forms it
// must show.
var (
	requiredGuideHeadings = []string{"what init writes", "tuning how brigada's agents judge", "skills"}
	requiredGuideForms    = []string{"brigada instructions diff <role>", "brigada init --take-default <role>"}
)

// checkNewDocs returns what the documents lack: GUIDE.md headings for what
// init writes, tuning how brigada's agents judge, and skills, and the forms
// `brigada instructions diff <role>` and `brigada init --take-default
// <role>`; COMPATIBILITY.md naming the instructions_file alias and the
// refusal when both it and instructions.worker are set; CHANGELOG.md's
// Unreleased section mentioning instructions and skills.
func checkNewDocs(guide, compat, changelog string) []string {
	var problems []string
	var headings []string
	for _, l := range proseLines(guide) {
		if m := headingRE.FindStringSubmatch(l); m != nil {
			headings = append(headings, strings.ToLower(strings.NewReplacer("’", "'", "`", "").Replace(m[1])))
		}
	}
	for _, want := range requiredGuideHeadings {
		found := false
		for _, h := range headings {
			if strings.Contains(h, want) {
				found = true
			}
		}
		if !found {
			problems = append(problems, fmt.Sprintf("GUIDE.md has no heading about %q", want))
		}
	}
	for _, form := range requiredGuideForms {
		if !strings.Contains(guide, form) {
			problems = append(problems, fmt.Sprintf("GUIDE.md never shows %q", form))
		}
	}
	if !strings.Contains(compat, "instructions_file") || !strings.Contains(compat, "instructions.worker") {
		problems = append(problems, "COMPATIBILITY.md does not name the instructions_file alias of instructions.worker")
	}
	if !regexp.MustCompile(`(?is)both.{0,200}refus|refus.{0,200}both`).MatchString(compat) {
		problems = append(problems, "COMPATIBILITY.md does not say that setting both keys is refused")
	}
	unreleased := ""
	if i := strings.Index(changelog, "## [Unreleased]"); i >= 0 {
		unreleased = changelog[i+len("## [Unreleased]"):]
		if j := strings.Index(unreleased, "\n## "); j >= 0 {
			unreleased = unreleased[:j]
		}
	}
	lower := strings.ToLower(unreleased)
	if unreleased == "" || !strings.Contains(lower, "instructions") || !strings.Contains(lower, "skills") {
		problems = append(problems, "CHANGELOG.md's Unreleased section does not mention instructions and skills")
	}
	return problems
}

// checkNoStaleForemanPath: the agent files must not send readers to the
// internal foreman text as where the foreman's instructions live.
func checkNoStaleForemanPath(name, text string) []string {
	if strings.Contains(text, "internal/foreman/instructions.md") {
		return []string{name + " names internal/foreman/instructions.md as where the foreman's instructions live"}
	}
	return nil
}

// --- tests ---------------------------------------------------------------

func TestDocsLinksAndAnchors(t *testing.T) {
	report(t, checkLinks(docsFiles))
}

func TestDocsCommandLines(t *testing.T) {
	text, err := os.ReadFile(docPath("docs/GUIDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	lines := codeBlockCommands(string(text))
	if len(lines) < 5 {
		t.Fatalf("found only %d brigada command lines in GUIDE.md's code blocks", len(lines))
	}
	c := newCommandChecker(t)
	for _, l := range lines {
		report(t, c.check(l))
	}
}

func TestDocsContent(t *testing.T) {
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for _, f := range []string{docPath("AGENTS.md"), docPath("CLAUDE.md")} {
		report(t, checkNoStaleForemanPath(filepath.Base(f), read(f)))
	}
	if !requireNewDocs {
		t.Skip("requireNewDocs is false: the docs task flips it once GUIDE, COMPATIBILITY and CHANGELOG say it")
	}
	report(t, checkNewDocs(read(docPath("docs/GUIDE.md")), read(docPath("docs/COMPATIBILITY.md")), read(docPath("CHANGELOG.md"))))
}

// The negative tests prove the helpers fail when they should.

func TestDocsLinkCheckerFails(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	other := write("other.md", "# Other\n\n## Two `words` & More\n\n## Dup\n\n## Dup\n")
	good := write("good.md", "# Good\n\n## Sub head\n\n[a](#sub-head) [b](other.md#two-words--more) [c](other.md#dup-1) [d](https://example.invalid/x) [e](other.md)\n"+
		"```\n[not a link](missing.md)\n```\n`[nor](this.md)`\n")
	if p := checkLinks([]string{good, other}); len(p) != 0 {
		t.Errorf("valid links reported: %v", p)
	}
	for name, tc := range map[string]struct{ text, want string }{
		"broken link":    {"[x](nowhere.md)\n", "nowhere.md does not exist"},
		"missing anchor": {"# H\n\n[x](#absent)\n", "no heading with the anchor #absent"},
		"other anchor":   {"[x](other.md#absent)\n", "no heading with the anchor #absent"},
	} {
		p := checkLinks([]string{write("bad.md", tc.text)})
		if len(p) != 1 || !strings.Contains(p[0], tc.want) {
			t.Errorf("%s: problems %v, want one with %q", name, p, tc.want)
		}
	}
	if slug("The `--probe` flag, v1.0 (beta)") != "the---probe-flag-v10-beta" && slug("The `--probe` flag, v1.0 (beta)") != "the--probe-flag-v10-beta" {
		t.Errorf("slug = %q", slug("The `--probe` flag, v1.0 (beta)"))
	}
}

func TestDocsCommandCheckerFails(t *testing.T) {
	c := newCommandChecker(t)
	for _, ok := range []string{
		"brigada status --line",
		"brigada status --line   # --bogus is in a comment",
		"brigada init [--repo URL] [--probe | --yes] [--verbose]",
		"brigada project set --repo URL key=value ...",
		"brigada project set [--repo URL] key=value",
		"brigada instructions diff worker",
		"brigada init --take-default worker --state-dir /tmp/x",
		"brigada land approve --and-stop-asking 3",
		`brigada ask - <<'EOF'`,
		"brigada status | grep --bogus",
		"brigada runner --agent 'harness=claude,model=sonnet' --max-parallel 4",
	} {
		if p := c.check(ok); len(p) != 0 {
			t.Errorf("%q reported %v", ok, p)
		}
	}
	for line, want := range map[string]string{
		"brigada frobnicate":                                  "unknown command",
		"brigada project frobnicate":                          "not a subcommand of project",
		"brigada status --bogus":                              "no flag --bogus",
		"brigada init --yes --probe --bogus":                  "no flag --bogus",
		"brigada init --yes --bogus --probe":                  "no flag --bogus",
		"brigada project set --repo URL --bogus key=value":    "project set has no flag --bogus",
		"brigada instructions diff --bogus worker":            "instructions diff has no flag --bogus",
		"brigada project set [--repo URL] [--nope] key=value": "no flag --nope",
		"brigada init --take-default=worker --bogus=1":        "no flag --bogus",
	} {
		p := c.check(line)
		if len(p) == 0 || !strings.Contains(strings.Join(p, "\n"), want) {
			t.Errorf("%q: problems %v, want one with %q", line, p, want)
		}
	}
	// A comment is dropped, but a quoted # is not a comment.
	if w := shellWords(`brigada ask "fix #12" # later`); len(w) != 3 || w[2] != "fix #12" {
		t.Errorf("shellWords = %q", w)
	}
}

// TestDocsFlagExtraction proves the source reader: every definer form, a
// prefix name, a set that is not ours, and the real command table.
func TestDocsFlagExtraction(t *testing.T) {
	sets, err := flagSetsFromSource(map[string]string{"x.go": `package main
func runX(verb string) {
	fs := newFlagSet("x "+verb, "s", "d")
	other := somethingElse("not-a-flag")
	a := fs.Bool("alpha", false, "u")
	fs.StringVar(&v, "beta", "", "u")
	fs.Var(val, "gamma", "u")
	fs.Func("delta", "u", nil)
	other.Bool("nope", false, "u")
	_ = a
	g := newFlagSet("plain", "s", "d")
	g.IntVar(&n, "eps", 0, "u")
}`})
	if err != nil || len(sets) != 2 {
		t.Fatalf("sets = %v, %v", sets, err)
	}
	c := &commandChecker{sets: sets}
	px := c.flagsOf("x set")
	for _, want := range []string{"alpha", "beta", "gamma", "delta", "state-dir", "config", "help"} {
		if !px[want] {
			t.Errorf("x set lacks --%s: %v", want, px)
		}
	}
	if px["nope"] || px["eps"] || c.flagsOf("x") != nil || !c.flagsOf("plain")["eps"] || c.flagsOf("plain")["alpha"] {
		t.Errorf("extraction crossed sets: %v %v", px, c.flagsOf("plain"))
	}
	real := newCommandChecker(t)
	for path, want := range map[string]string{"init": "take-default", "instructions diff": "state-dir", "project set": "repo", "plan approve": "and-stop-asking", "land approve": "and-stop-asking"} {
		if !real.flagsOf(path)[want] {
			t.Errorf("%s lacks --%s in the command sources", path, want)
		}
	}
	if real.flagsOf("init")["bogus"] {
		t.Errorf("init accepts --bogus")
	}
}

func TestDocsContentCheckerFails(t *testing.T) {
	guide := "# G\n\n## What init writes\n\n## Tuning how brigada’s agents judge\n\n## Task skills\n\n```sh\nbrigada instructions diff <role>\nbrigada init --take-default <role>\n```\n"
	compat := "`instructions_file` is the alias of `instructions.worker`; setting both is refused.\n"
	changelog := "# Changelog\n\n## [Unreleased]\n\n- Role instructions and skills.\n\n## [1.0.0]\n\n- Old.\n"
	if p := checkNewDocs(guide, compat, changelog); len(p) != 0 {
		t.Errorf("good docs reported %v", p)
	}
	for name, tc := range map[string]struct{ guide, compat, changelog, want string }{
		"no init heading":  {strings.Replace(guide, "What init writes", "Other", 1), compat, changelog, "what init writes"},
		"no tuning":        {strings.Replace(guide, "Tuning how", "Tuning", 1), compat, changelog, "tuning how"},
		"no skills":        {strings.Replace(guide, "Task skills", "Task", 1), compat, changelog, `"skills"`},
		"no diff form":     {strings.Replace(guide, "diff <role>", "diff", 1), compat, changelog, "instructions diff <role>"},
		"no take-default":  {strings.Replace(guide, "--take-default <role>", "--take-default", 1), compat, changelog, "take-default <role>"},
		"no alias":         {guide, "nothing", changelog, "instructions_file alias"},
		"no refusal":       {guide, "`instructions_file` is the alias of `instructions.worker`.", changelog, "both keys is refused"},
		"changelog old":    {guide, compat, strings.Replace(changelog, "skills", "tools", 1), "Unreleased section"},
		"changelog empty":  {guide, compat, "# Changelog\n", "Unreleased section"},
		"unreleased scope": {guide, compat, strings.Replace(changelog, "- Role instructions and skills.", "", 1) + "instructions skills", "Unreleased section"},
	} {
		p := checkNewDocs(tc.guide, tc.compat, tc.changelog)
		if len(p) == 0 || !strings.Contains(strings.Join(p, "\n"), tc.want) {
			t.Errorf("%s: problems %v, want one with %q", name, p, tc.want)
		}
	}
	if len(checkNoStaleForemanPath("AGENTS.md", "see internal/foreman/instructions.md")) != 1 || len(checkNoStaleForemanPath("AGENTS.md", "fine")) != 0 {
		t.Errorf("checkNoStaleForemanPath wrong")
	}
}
