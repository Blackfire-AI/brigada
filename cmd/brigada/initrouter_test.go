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
)

const (
	routerBegin = "<!-- brigada-managed: router v1"
	routerEnd   = "<!-- /brigada-managed: router -->"
	importBegin = "<!-- brigada-managed: import v1"
	importEnd   = "<!-- /brigada-managed: import -->"
)

// addedBlank is the record an older init wrote into a begin marker when
// it appended the section to a terminated file holding original.
func addedBlank(original string) string {
	return fmt.Sprintf("; init added a blank line after byte %d -->", len(original))
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
	// Content the user adds after the wiring stays, and a file init
	// created is kept once it holds something else.
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

// A shared file that is a link to the other one holds no section of its
// own: the other is the file, and remove cleans it through the link.
func TestRemovalCleansTheFileBehindTheLink(t *testing.T) {
	for _, tc := range []struct {
		name, link, target string
		exists             bool
	}{
		{"CLAUDE.md links to AGENTS.md", wiringClaudeFile, wiringAgentsFile, true},
		{"AGENTS.md links to CLAUDE.md", wiringAgentsFile, wiringClaudeFile, true},
		{"dangling CLAUDE.md link", wiringClaudeFile, wiringAgentsFile, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := wireRepo(t)
			if tc.exists {
				wireWrite(t, root, tc.target, "# shared\n")
			}
			if err := os.Symlink(tc.target, filepath.Join(root, tc.link)); err != nil {
				t.Fatal(err)
			}
			wireApply(t, root)
			files := planRemoval(root, false)
			for _, f := range files {
				if (f.rel == wiringAgentsFile || f.rel == wiringClaudeFile) && f.err != nil {
					t.Errorf("%s: %v", f.rel, f.err)
				}
			}
			if err := applyRemoval(files); err != nil {
				t.Fatal(err)
			}
			if tc.exists {
				if got := wireRead(t, root, tc.target); got != "# shared\n" {
					t.Errorf("after remove: %q", got)
				}
			} else if fileExists(filepath.Join(root, tc.target)) {
				t.Error("remove kept the file init created")
			}
			if st, err := os.Lstat(filepath.Join(root, tc.link)); err != nil || st.Mode()&os.ModeSymlink == 0 {
				t.Errorf("the link changed: %v %v", st, err)
			}
		})
	}
	t.Run("other symlinks are refused", func(t *testing.T) {
		root := wireRepo(t)
		wireWrite(t, root, "docs/agents.md", "# elsewhere\n")
		if err := os.Symlink("docs/agents.md", filepath.Join(root, wiringAgentsFile)); err != nil {
			t.Fatal(err)
		}
		files := planRemoval(root, false)
		for _, f := range files {
			if f.rel == wiringAgentsFile && (f.err == nil || !strings.Contains(f.err.Error(), "symlink")) {
				t.Errorf("AGENTS.md -> docs: state %s err %v", f.state, f.err)
			}
		}
		if err := applyRemoval(files); err != nil {
			t.Fatal(err)
		}
		if got := wireRead(t, root, "docs/agents.md"); got != "# elsewhere\n" {
			t.Errorf("symlink target rewritten: %q", got)
		}
	})
}

// Markers inside a fenced code block are text, not markers: a fenced copy
// beside no real block leaves nothing to remove.
func TestRemovalIgnoresFencedMarkers(t *testing.T) {
	root := wireRepo(t)
	body := "# mine\n\n```\n" + routerBegin + " -->\n" + routerEnd + "\n```\n"
	wireWrite(t, root, wiringAgentsFile, body)
	for _, f := range planRemoval(root, false) {
		if f.rel == wiringAgentsFile && (f.err != nil || f.changed()) {
			t.Errorf("planned a change: state %s err %v", f.state, f.err)
		}
	}
	if err := applyRemoval(planRemoval(root, false)); err != nil {
		t.Fatal(err)
	}
	if got := wireRead(t, root, wiringAgentsFile); got != body {
		t.Errorf("file rewritten: %q", got)
	}
}

func TestLegacyPlanRetiresAnOlderInitsSkill(t *testing.T) {
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
			files := planLegacyWiring(root)
			var plan bytes.Buffer
			printInitFiles(&plan, files, false)
			flat := strings.Join(strings.Fields(plan.String()), " ")
			if err := applyWiring(files); err != nil {
				t.Fatal(err)
			}
			if tc.kept {
				if !strings.Contains(flat, "left alone "+wiringSkillFile) {
					t.Errorf("plan does not keep the user's skill:\n%s", plan.String())
				}
				if got := wireRead(t, root, wiringSkillFile); got != tc.body {
					t.Errorf("user's skill changed: %q", got)
				}
				return
			}
			if !strings.Contains(flat, "removed "+wiringSkillFile+" deletes the old skill") {
				t.Errorf("plan does not remove the old skill:\n%s", plan.String())
			}
			if fileExists(filepath.Join(root, filepath.Dir(wiringSkillFile))) {
				t.Error("old skill or its directory remains")
			}
			for _, f := range planLegacyWiring(root) {
				if f.rel == wiringSkillFile && (f.exists || f.changed()) {
					t.Errorf("rerun plans the skill again: %+v", f)
				}
			}
		})
	}
}

func TestInitWritesNoHarnessFiles(t *testing.T) {
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
	code, out, errs := initDo(t, state, "", "--yes", "--parallel", "1", "--accept-limited", "--agent", "harness=claude,model=sonnet", "--signed-in", "claude", "--check", "true", "--tools", "none")
	if code != 0 {
		t.Fatalf("init code=%d\n%s\n%s", code, out, errs)
	}
	for _, rel := range []string{wiringRulesFile, wiringAgentsFile, wiringClaudeFile, wiringMCPFile, wiringSettingsFile, wiringSkillFile, wiringCodexFile} {
		if fileExists(filepath.Join(root, rel)) {
			t.Errorf("init wrote %s", rel)
		}
	}
	if strings.Contains(out, "Restart your agent session") || !strings.Contains(out, "brigada attach") {
		t.Errorf("closing text does not point at attach:\n%s", out)
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
