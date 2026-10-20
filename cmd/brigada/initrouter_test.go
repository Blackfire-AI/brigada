package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/prompt"
)

const (
	routerBegin = "<!-- brigada-managed: router v1"
	routerEnd   = "<!-- /brigada-managed: router -->"
	importBegin = "<!-- brigada-managed: import v1"
	importEnd   = "<!-- /brigada-managed: import -->"
)

// addedBlank is the record init writes into a begin marker when it
// appends the section to a terminated file holding original.
func addedBlank(original string) string {
	return fmt.Sprintf("; init added a blank line after byte %d -->", len(original))
}

// wirePlan plans the wiring and returns the file planned for rel.
func wirePlan(t *testing.T, root, rel string) wireFile {
	t.Helper()
	for _, f := range planWiring(root) {
		if f.rel == rel {
			return f
		}
	}
	t.Fatalf("%s not planned", rel)
	return wireFile{}
}

func TestRouterBlockQuotesThePromptOpeners(t *testing.T) {
	body := routerBody()
	for _, opener := range []string{prompt.WorkerOpener, prompt.ReviewerOpener} {
		if !strings.Contains(body, "\""+opener+"\"") {
			t.Errorf("the router block does not quote %q:\n%s", opener, body)
		}
	}
	// The carve-out comes first, before any word about the foreman.
	if strings.Index(body, prompt.WorkerOpener) > strings.Index(body, "foreman") {
		t.Errorf("the carve-out is not first:\n%s", body)
	}
	if strings.Contains(body, "@") {
		t.Errorf("the router block imports something:\n%s", body)
	}
	if lines := strings.Split(body, "\n"); len(lines) > 14 {
		t.Errorf("the router block is %d lines, want about ten", len(lines))
	}
	for _, line := range strings.Split(body, "\n") {
		if len(line) >= 80 {
			t.Errorf("router line reaches 80 columns: %q", line)
		}
	}
}

func TestWiringRerunReplacesOnlyTheMarkedParts(t *testing.T) {
	root := wireRepo(t)
	agents := "# Mine\n\nRules of my own.\n"
	claude := "# Also mine\n@docs/style.md\n"
	wireWrite(t, root, wiringAgentsFile, agents)
	wireWrite(t, root, wiringClaudeFile, claude)
	wireApply(t, root)
	gotAgents, gotClaude := wireRead(t, root, wiringAgentsFile), wireRead(t, root, wiringClaudeFile)
	if !strings.HasPrefix(gotAgents, agents+"\n"+routerBegin+addedBlank(agents)+"\n") || !strings.HasSuffix(gotAgents, "\n"+routerEnd+"\n") {
		t.Fatalf("AGENTS.md = %q", gotAgents)
	}
	if !strings.HasPrefix(gotClaude, claude+"\n"+importBegin+addedBlank(claude)+"\n@AGENTS.md\n"+importEnd+"\n") {
		t.Fatalf("CLAUDE.md = %q", gotClaude)
	}
	// The user adds text after the block, and an older init's block is
	// replaced between its markers only, record included.
	after := "\n## Later\n\nmore of mine\n"
	stale := strings.Replace(gotAgents, routerBody(), "old router text", 1)
	stale = strings.Replace(stale, routerBegin+";", "<!-- brigada-managed: router v0;", 1) + after
	wireWrite(t, root, wiringAgentsFile, stale)
	wireWrite(t, root, wiringClaudeFile, gotClaude+after)
	files := planWiring(root)
	for _, f := range files {
		switch f.rel {
		case wiringAgentsFile:
			if f.err != nil || f.state != "replaced" {
				t.Errorf("AGENTS.md rerun: state %s err %v", f.state, f.err)
			}
		case wiringClaudeFile:
			if f.err != nil || f.state != "unchanged" {
				t.Errorf("CLAUDE.md rerun: state %s err %v", f.state, f.err)
			}
		}
	}
	if err := applyWiring(files); err != nil {
		t.Fatal(err)
	}
	if got := wireRead(t, root, wiringAgentsFile); got != gotAgents+after {
		t.Errorf("AGENTS.md after rerun = %q\nwant %q", got, gotAgents+after)
	}
	if got := wireRead(t, root, wiringClaudeFile); got != gotClaude+after {
		t.Errorf("CLAUDE.md after rerun = %q", got)
	}
	for _, f := range wireApply(t, root) {
		if f.err != nil || f.changed() {
			t.Errorf("third run changed %s: %s %v", f.rel, f.state, f.err)
		}
	}
}

func TestWiringImportIsAddedBesideTheUsersOwn(t *testing.T) {
	root := wireRepo(t)
	claude := "@AGENTS.md\n\n# mine\n"
	wireWrite(t, root, wiringClaudeFile, claude)
	wireApply(t, root)
	want := claude + "\n" + importBegin + addedBlank(claude) + "\n@AGENTS.md\n" + importEnd + "\n"
	if got := wireRead(t, root, wiringClaudeFile); got != want {
		t.Errorf("CLAUDE.md = %q, want %q", got, want)
	}
}

func TestWiringSameFileGetsTheRouterAndNoImport(t *testing.T) {
	for _, tc := range []struct {
		name, link, target string
		exists             bool
	}{
		{"CLAUDE.md links to AGENTS.md", wiringClaudeFile, wiringAgentsFile, true},
		{"AGENTS.md links to CLAUDE.md", wiringAgentsFile, wiringClaudeFile, true},
		{"dangling CLAUDE.md link", wiringClaudeFile, wiringAgentsFile, false},
		{"dangling AGENTS.md link", wiringAgentsFile, wiringClaudeFile, false},
		{"absolute link", wiringClaudeFile, wiringAgentsFile, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := wireRepo(t)
			if tc.exists {
				wireWrite(t, root, tc.target, "# shared\n")
			}
			target := tc.target
			if tc.name == "absolute link" {
				target = filepath.Join(root, tc.target)
			}
			if err := os.Symlink(target, filepath.Join(root, tc.link)); err != nil {
				t.Fatal(err)
			}
			files := wireApply(t, root)
			for _, f := range files {
				if (f.rel == wiringAgentsFile || f.rel == wiringClaudeFile) && f.err != nil {
					t.Errorf("%s: %v", f.rel, f.err)
				}
			}
			got := wireRead(t, root, tc.target)
			if !strings.Contains(got, routerBegin) || strings.Contains(got, "@AGENTS.md") || strings.Contains(got, importBegin) {
				t.Errorf("%s = %q", tc.target, got)
			}
			if tc.exists && !strings.HasPrefix(got, "# shared\n\n") {
				t.Errorf("user content lost: %q", got)
			}
			if st, err := os.Lstat(filepath.Join(root, tc.link)); err != nil || st.Mode()&os.ModeSymlink == 0 {
				t.Errorf("the link changed: %v %v", st, err)
			}
			through, err := os.ReadFile(filepath.Join(root, tc.link))
			if err != nil || string(through) != got {
				t.Errorf("read through the link: %q, %v", through, err)
			}
			var plan bytes.Buffer
			printInitFiles(&plan, files, false)
			if !strings.Contains(strings.Join(strings.Fields(plan.String()), " "), "unchanged "+tc.link+" a link to "+tc.target) {
				t.Errorf("plan does not explain the link:\n%s", plan.String())
			}
			if err := applyRemoval(planRemoval(root, false)); err != nil {
				t.Fatal(err)
			}
			if tc.exists {
				if got := wireRead(t, root, tc.target); got != "# shared\n" {
					t.Errorf("after remove: %q", got)
				}
			} else if fileExists(filepath.Join(root, tc.target)) {
				t.Error("remove kept the file init created")
			}
		})
	}
	t.Run("other symlinks are refused", func(t *testing.T) {
		root := wireRepo(t)
		wireWrite(t, root, "docs/agents.md", "# elsewhere\n")
		if err := os.Symlink("docs/agents.md", filepath.Join(root, wiringAgentsFile)); err != nil {
			t.Fatal(err)
		}
		f := wirePlan(t, root, wiringAgentsFile)
		if f.err == nil || !strings.Contains(f.err.Error(), "symlink") {
			t.Errorf("AGENTS.md -> docs: %v", f.err)
		}
		if f := wirePlan(t, root, wiringClaudeFile); f.err != nil || f.state != "created" {
			t.Errorf("CLAUDE.md: state %s err %v", f.state, f.err)
		}
	})
}

func TestWiringFences(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		refused    bool
	}{
		{"unclosed backticks", "# mine\n\n```sh\nmake test\n", true},
		{"unclosed tildes", "~~~\ntext\n", true},
		{"closed by a shorter run", "````\ntext\n```\n", true},
		{"closer with trailing text", "```\ntext\n``` end\n", true},
		{"closer of the other character", "```\ntext\n~~~\n", true},
		{"closer indented four spaces", "```\ntext\n    ```\n", true},
		{"closed then text", "```sh\nmake\n```\n\nmore of mine\n", false},
		{"closed by a longer run", "```\ntext\n`````\n", false},
		{"closer with trailing spaces", "~~~ info\ntext\n~~~   \n", false},
		{"opener indented three spaces", "   ```\ntext\n```\n", false},
		{"opener indented four spaces is text", "    ```\nindented code, no fence\n", false},
		{"backtick opener with a backtick in its info string is text", "```foo```\nnot a fence\n", false},
		{"tilde opener may carry backticks", "~~~ `x`\ntext\n~~~\n", false},
		{"no final newline", "```\ntext\n```", false},
	} {
		for _, rel := range []string{wiringAgentsFile, wiringClaudeFile} {
			t.Run(tc.name+" in "+rel, func(t *testing.T) {
				root := wireRepo(t)
				wireWrite(t, root, rel, tc.body)
				f := wirePlan(t, root, rel)
				if tc.refused {
					if f.err == nil || !strings.Contains(f.err.Error(), rel) || !strings.Contains(f.err.Error(), "close the fence") {
						t.Fatalf("not refused: state %s err %v", f.state, f.err)
					}
					return
				}
				if f.err != nil || f.state != "added" {
					t.Fatalf("refused: state %s err %v", f.state, f.err)
				}
				if !bytes.HasPrefix(f.next, []byte(tc.body)) {
					t.Errorf("user bytes changed: %q", f.next)
				}
			})
		}
	}
	// A rerun is refused too, once the user leaves a fence open after the
	// block; closing it is accepted again.
	root := wireRepo(t)
	wireWrite(t, root, wiringAgentsFile, "# mine\n")
	wireApply(t, root)
	wired := wireRead(t, root, wiringAgentsFile)
	wireWrite(t, root, wiringAgentsFile, wired+"\n```\nopen\n")
	if f := wirePlan(t, root, wiringAgentsFile); f.err == nil || !strings.Contains(f.err.Error(), "close the fence") {
		t.Errorf("rerun not refused: %v", f.err)
	}
	wireWrite(t, root, wiringAgentsFile, wired+"\n```\nopen\n```\n")
	if f := wirePlan(t, root, wiringAgentsFile); f.err != nil || f.state != "unchanged" {
		t.Errorf("closed fence after the block: state %s err %v", f.state, f.err)
	}
	// Markers inside a fence are text: a fenced copy beside the real block
	// is not a second block.
	wireWrite(t, root, wiringAgentsFile, wired+"\n```\n"+routerBegin+" -->\n"+routerEnd+"\n```\n")
	if f := wirePlan(t, root, wiringAgentsFile); f.err != nil || f.state != "unchanged" {
		t.Errorf("fenced markers: state %s err %v", f.state, f.err)
	}
}

func TestWiringRouterRoundTripIsByteExact(t *testing.T) {
	cases := map[string]*string{
		"missing":          nil,
		"empty":            new(""),
		"no final newline": new("# mine\n\ntext"),
		"ordinary":         new("# mine\n\ntext\n"),
		"only a newline":   new("\n"),
	}
	for name, body := range cases {
		for _, rel := range []string{wiringAgentsFile, wiringClaudeFile} {
			t.Run(name+" "+rel, func(t *testing.T) {
				root := wireRepo(t)
				other := wiringClaudeFile
				if rel == wiringClaudeFile {
					other = wiringAgentsFile
				}
				wireWrite(t, root, other, "# the other file\n")
				if body != nil {
					wireWrite(t, root, rel, *body)
				}
				before := removalSnapshot(t, root)
				wireApply(t, root)
				got := wireRead(t, root, rel)
				if body != nil && !strings.HasPrefix(got, *body) {
					t.Errorf("user bytes changed: %q", got)
				}
				if body != nil && *body != "" && !strings.Contains(got, "\n\n<!-- brigada-managed:") {
					t.Errorf("no blank line before the section: %q", got)
				}
				if err := applyRemoval(planRemoval(root, false)); err != nil {
					t.Fatal(err)
				}
				if after := removalSnapshot(t, root); !reflect.DeepEqual(before, after) {
					t.Fatalf("round trip:\nbefore %q\nafter %q", before, after)
				}
			})
		}
	}
	// Content the user adds after init stays, and a file init created is
	// kept once it holds something else.
	root := wireRepo(t)
	wireApply(t, root)
	wireWrite(t, root, wiringAgentsFile, wireRead(t, root, wiringAgentsFile)+"\n# mine, added later\n")
	if err := applyRemoval(planRemoval(root, false)); err != nil {
		t.Fatal(err)
	}
	if got := wireRead(t, root, wiringAgentsFile); got != "\n# mine, added later\n" {
		t.Errorf("AGENTS.md after remove = %q", got)
	}
	if fileExists(filepath.Join(root, wiringClaudeFile)) {
		t.Error("CLAUDE.md kept though init created it and nothing else is left")
	}
}

// withoutSection is s without brigada's section lines: what the user sees
// as their own file once the section is gone, init's separators included.
func withoutSection(t *testing.T, s string) string {
	t.Helper()
	begin := strings.Index(s, "<!-- brigada-managed:")
	end := strings.Index(s, "<!-- /brigada-managed:")
	if begin < 0 || end < begin {
		t.Fatalf("no section in %q", s)
	}
	end += strings.Index(s[end:], "-->") + len("-->")
	if end < len(s) && s[end] == '\n' {
		end++
	}
	return s[:begin] + s[end:]
}

// Once the user edits next to the section, remove keeps every byte that
// could be theirs: an addition in front of the section moves it off the
// offset its record names, so init's separators stay with the user's
// lines; an addition after it leaves the separators where init put them,
// and init's blank line goes, but not the newline ending a line that the
// addition would otherwise join.
func TestWiringRemoveKeepsAdditionsAroundSections(t *testing.T) {
	type edit struct {
		name          string
		top, at, past string // added at the top, in front of the section, after it
		aboveBlank    string // added between the original and init's blank line
	}
	edits := []edit{
		{name: "line before", at: "extra\n"},
		{name: "line and blank line before", at: "extra\n\n"},
		{name: "blank line before", at: "\n"},
		{name: "repeated line before", at: "# mine\n"},
		{name: "repeated line and blank line before", at: "# mine\n\n"},
		{name: "line above init's blank line", aboveBlank: "extra\n"},
		{name: "line at the top", top: "top\n"},
		{name: "line after", past: "tail\n"},
		{name: "blank line and line after", past: "\ntail\n"},
		{name: "repeated line after", past: "# mine\n"},
		{name: "unterminated line after", past: "tail"},
		{name: "lines on both sides", at: "extra\n", past: "tail\n"},
		{name: "blank lines on both sides", at: "extra\n\n", past: "\ntail\n"},
		{name: "repeated lines on both sides", at: "# mine\n\n", past: "# mine\n"},
	}
	for _, body := range []string{"# mine\n", "# mine"} {
		for _, e := range edits {
			for _, rel := range []string{wiringAgentsFile, wiringClaudeFile} {
				name := e.name + " " + rel
				if !strings.HasSuffix(body, "\n") {
					name += " unterminated"
				}
				t.Run(name, func(t *testing.T) {
					root := wireRepo(t)
					wireWrite(t, root, rel, body)
					wireApply(t, root)
					wired := wireRead(t, root, rel)
					begin := strings.Index(wired, "<!-- brigada-managed:")
					edited := e.top + wired[:begin-1] + e.aboveBlank + wired[begin-1:begin] + e.at + wired[begin:] + e.past
					wireWrite(t, root, rel, edited)
					if err := applyRemoval(planRemoval(root, false)); err != nil {
						t.Fatal(err)
					}
					got := wireRead(t, root, rel)
					// Everything but the section's lines is the user's or could
					// be: with anything added in front, all of it stays.
					want := withoutSection(t, edited)
					if e.top == "" && e.at == "" && e.aboveBlank == "" {
						// Only additions after: init's blank line goes, and the
						// original's last line keeps the terminator the
						// addition needs.
						want = strings.TrimSuffix(body, "\n") + "\n" + e.past
					}
					if got != want {
						t.Errorf("after remove = %q, want %q", got, want)
					}
					// No line the user wrote is lost or joined to another.
					gotLines := strings.Split(got, "\n")
					for _, line := range strings.Split(withoutSection(t, edited), "\n") {
						if line == "" {
							continue
						}
						i := slices.Index(gotLines, line)
						if i < 0 {
							t.Fatalf("line %q lost or joined: %q", line, got)
						}
						gotLines = gotLines[i+1:]
					}
				})
			}
		}
	}
}

// The record of a file init created moves with the sections when the
// layout changes, even with the user's content beside it, so remove still
// deletes the file once the user has taken their content out again.
func TestWiringCreatedFileRecordSurvivesLayoutChange(t *testing.T) {
	for _, where := range []string{"after", "before"} {
		for _, deleted := range []bool{true, false} {
			name := "content " + where + map[bool]string{true: ", deleted later", false: ", kept"}[deleted]
			t.Run(name, func(t *testing.T) {
				root := wireRepo(t)
				wireApply(t, root)
				claude := wireRead(t, root, wiringClaudeFile)
				if where == "after" {
					claude += "# mine\n"
				} else {
					claude = "# mine\n" + claude
				}
				wireWrite(t, root, wiringClaudeFile, claude)
				// AGENTS.md becomes a link to CLAUDE.md: the import goes, the
				// router comes, and the file is still init's.
				if err := os.Remove(filepath.Join(root, wiringAgentsFile)); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(wiringClaudeFile, filepath.Join(root, wiringAgentsFile)); err != nil {
					t.Fatal(err)
				}
				wireApply(t, root)
				got := wireRead(t, root, wiringClaudeFile)
				if !strings.HasPrefix(got, "# mine\n\n"+routerBegin+"; init added the file, a blank line after byte 7 -->\n") || strings.Contains(got, importBegin) {
					t.Fatalf("CLAUDE.md after the move = %q", got)
				}
				if deleted {
					wireWrite(t, root, wiringClaudeFile, strings.TrimPrefix(got, "# mine\n"))
				}
				if err := applyRemoval(planRemoval(root, false)); err != nil {
					t.Fatal(err)
				}
				if deleted {
					if fileExists(filepath.Join(root, wiringClaudeFile)) {
						t.Errorf("CLAUDE.md kept though init created it and the user's content is gone: %q", wireRead(t, root, wiringClaudeFile))
					}
				} else if got := wireRead(t, root, wiringClaudeFile); got != "# mine\n" {
					t.Errorf("CLAUDE.md after remove = %q", got)
				}
			})
		}
	}
}

// A rerun after the two files became one (or stopped being one) moves the
// sections: the shared file never imports itself, and the file that is a
// file again carries no router meant for the other.
func TestWiringRerunFollowsLayoutChanges(t *testing.T) {
	for _, own := range []bool{false, true} {
		t.Run(map[bool]string{false: "created by init", true: "user content"}[own], func(t *testing.T) {
			root := wireRepo(t)
			if own {
				wireWrite(t, root, wiringClaudeFile, "# mine\n")
			}
			wireApply(t, root)
			// AGENTS.md becomes a link to CLAUDE.md.
			if err := os.Remove(filepath.Join(root, wiringAgentsFile)); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(wiringClaudeFile, filepath.Join(root, wiringAgentsFile)); err != nil {
				t.Fatal(err)
			}
			if f := wirePlan(t, root, wiringClaudeFile); f.err != nil || f.state != "replaced" {
				t.Fatalf("CLAUDE.md: state %s err %v", f.state, f.err)
			}
			wireApply(t, root)
			got := wireRead(t, root, wiringClaudeFile)
			if strings.Contains(got, "@AGENTS.md") || strings.Contains(got, importBegin) || !strings.Contains(got, routerBody()) {
				t.Fatalf("CLAUDE.md after the move = %q", got)
			}
			if own {
				if !strings.HasPrefix(got, "# mine\n\n"+routerBegin+addedBlank("# mine\n")+"\n") {
					t.Errorf("user content or record lost: %q", got)
				}
			} else if !strings.HasPrefix(got, routerBegin+"; init added the file -->\n") {
				t.Errorf("the created record was not carried: %q", got)
			}
			for _, f := range wireApply(t, root) {
				if f.err != nil || f.changed() {
					t.Errorf("rerun changed %s: %s %v", f.rel, f.state, f.err)
				}
			}
			// And back: AGENTS.md is a file of its own again.
			if err := os.Remove(filepath.Join(root, wiringAgentsFile)); err != nil {
				t.Fatal(err)
			}
			wireWrite(t, root, wiringAgentsFile, "# agents\n")
			wireApply(t, root)
			agents, claude := wireRead(t, root, wiringAgentsFile), wireRead(t, root, wiringClaudeFile)
			if !strings.HasPrefix(agents, "# agents\n\n"+routerBegin) || strings.Contains(claude, routerBegin) || !strings.Contains(claude, "@AGENTS.md") {
				t.Fatalf("after moving back:\nAGENTS.md = %q\nCLAUDE.md = %q", agents, claude)
			}
			if err := applyRemoval(planRemoval(root, false)); err != nil {
				t.Fatal(err)
			}
			if got := wireRead(t, root, wiringAgentsFile); got != "# agents\n" {
				t.Errorf("AGENTS.md after remove = %q", got)
			}
			if own {
				if got := wireRead(t, root, wiringClaudeFile); got != "# mine\n" {
					t.Errorf("CLAUDE.md after remove = %q", got)
				}
			} else if fileExists(filepath.Join(root, wiringClaudeFile)) {
				t.Error("CLAUDE.md kept though init created it and nothing else is left")
			}
		})
	}
}

func TestWiringRetiresAnOlderInitsSkill(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		kept       bool
	}{
		{"marked", "---\nname: brigada-foreman\n---\n" + wiringSkillMarker + " v9 -->\nold rules\n", false},
		{"unmarked", "---\nname: brigada-foreman\n---\nmy own skill\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := wireRepo(t)
			wireWrite(t, root, wiringSkillFile, tc.body)
			files := planWiring(root)
			var plan bytes.Buffer
			printInitFiles(&plan, files, false)
			flat := strings.Join(strings.Fields(plan.String()), " ")
			if err := applyWiring(files); err != nil {
				t.Fatal(err)
			}
			if tc.kept {
				if !strings.Contains(flat, "unchanged "+wiringSkillFile+" yours") {
					t.Errorf("plan does not keep the user's skill:\n%s", plan.String())
				}
				if got := wireRead(t, root, wiringSkillFile); got != tc.body {
					t.Errorf("user's skill changed: %q", got)
				}
				return
			}
			if !strings.Contains(flat, "removed "+wiringSkillFile+" the rules of an older init") {
				t.Errorf("plan does not remove the old skill:\n%s", plan.String())
			}
			if fileExists(filepath.Join(root, filepath.Dir(wiringSkillFile))) {
				t.Error("old skill or its directory remains")
			}
			for _, f := range wireApply(t, root) {
				if f.rel == wiringSkillFile && (f.exists || f.changed()) {
					t.Errorf("rerun plans the skill again: %+v", f)
				}
			}
		})
	}
}

func TestInitNoWiringWritesNoRouterFiles(t *testing.T) {
	_, state := startSettingsEngine(t)
	importRepo(t)
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	remote := t.TempDir()
	wireGit(t, remote, "init", "--bare", "-q")
	wireGit(t, root, "remote", "set-url", "origin", remote)
	wireGit(t, root, "config", "core.excludesFile", os.DevNull)
	startStopHelper(t, "runner", state)
	fakeInitPATH(t, "claude")
	code, out, errs := initDo(t, state, "", "--yes", "--parallel", "1", "--accept-limited", "--agent", "harness=claude,model=sonnet", "--signed-in", "claude", "--check", "true", "--tools", "none", "--no-wiring")
	if code != 0 {
		t.Fatalf("init code=%d\n%s\n%s", code, out, errs)
	}
	for _, rel := range []string{wiringRulesFile, wiringAgentsFile, wiringClaudeFile, wiringMCPFile, wiringSettingsFile, wiringSkillFile} {
		if fileExists(filepath.Join(root, rel)) {
			t.Errorf("--no-wiring wrote %s", rel)
		}
	}
	if strings.Contains(out, "AGENTS.md") || strings.Contains(out, "Restart your agent session") {
		t.Errorf("--no-wiring plan mentions the router:\n%s", out)
	}
}

func TestRemovalRefusesDamagedBlocksAndKeepsUnmarkedFiles(t *testing.T) {
	root := wireRepo(t)
	wireApply(t, root)
	damaged := strings.Replace(wireRead(t, root, wiringAgentsFile), routerEnd, "", 1)
	wireWrite(t, root, wiringAgentsFile, damaged)
	files := planRemoval(root, false)
	for _, f := range files {
		if f.rel == wiringAgentsFile && (f.err == nil || !strings.Contains(f.err.Error(), "without its end marker")) {
			t.Errorf("damaged block not refused: state %s err %v", f.state, f.err)
		}
	}
	if err := applyRemoval(files); err != nil {
		t.Fatal(err)
	}
	if got := wireRead(t, root, wiringAgentsFile); got != damaged {
		t.Errorf("damaged file rewritten: %q", got)
	}
	if fileExists(filepath.Join(root, wiringClaudeFile)) || fileExists(filepath.Join(root, wiringRulesFile)) {
		t.Error("the other files were not removed")
	}
}
