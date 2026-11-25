package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// legacyOperatorHeader is the header an older init put at the top of the
// operator guide, and after the frontmatter of the operator skill.
const legacyOperatorHeader = operatorHeaderPrefix + ` init overwrites it, brigada remove
deletes it. Do not edit -->

`

// legacyOperatorGuide and legacyOperatorSkill rebuild the two operator
// artifacts an older init generated.
const legacyOperatorGuide = legacyOperatorHeader + "# Operating brigada from your coding agent\n"

const legacyOperatorSkill = "---\nname: brigada-operator\n---\n" + legacyOperatorHeader + "# Operating brigada from your coding agent\n"

const legacyOperatorSkillDir = ".claude/skills/brigada-operator"

// operatorPath is one of the two legacy paths under root.
func operatorPath(root, rel string) string {
	return filepath.Join(root, filepath.FromSlash(rel))
}

// fakeNPX puts an npx first on PATH that creates marker when it runs.
func fakeNPX(t *testing.T) string {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "npx-ran")
	dir := t.TempDir()
	script := "#!/bin/sh\n: > \"" + marker + "\"\necho 'unexpected npx invocation' >&2\nexit 89\n"
	if err := os.WriteFile(filepath.Join(dir, "npx"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return marker
}

// operatorPlan is one of the two plans that clean up an older init's
// operator artifacts, with how its printed output, spaces collapsed,
// words a removal, a kept file and a skill brigada does not manage, and
// how a refused path shows.
type operatorPlan struct {
	name      string
	plan      func(string) []wireFile
	apply     func([]wireFile) error
	print     func([]wireFile) string
	removed   func(string) string
	kept      func(string) string
	unmanaged func(string) string
	fault     string
}

func operatorPlans() []operatorPlan {
	return []operatorPlan{
		{
			name: "rerun", plan: planLegacyOperator, apply: applyWiring,
			print: func(files []wireFile) string {
				var out bytes.Buffer
				printInitFiles(&out, files, false)
				return strings.Join(strings.Fields(out.String()), " ")
			},
			removed: func(rel string) string {
				return "removed " + rel + " removes the operator " + operatorArtifact(rel) + " an older init generated"
			},
			kept:      func(rel string) string { return "left alone " + rel + " no brigada header; keeping your file" },
			unmanaged: func(rel string) string { return "unchanged " + rel + " " + installedSkillPurpose },
			fault:     "refusing symlink",
		},
		{
			name:  "remove",
			plan:  func(root string) []wireFile { return planRemoval(root, false) },
			apply: applyRemoval,
			print: func(files []wireFile) string {
				var out bytes.Buffer
				printRemovalPlan(&out, "example.invalid/repo", files, 0, false, 0, false)
				return strings.Join(strings.Fields(out.String()), " ")
			},
			removed:   func(rel string) string { return rel + " delete (brigada's own file)" },
			kept:      func(rel string) string { return rel + " left alone: no brigada header; keeping your file" },
			unmanaged: func(rel string) string { return rel + " keep (" + installedSkillPurpose + ")" },
			fault:     "refusing symlink",
		},
	}
}

// operatorArtifact names the operator artifact at a legacy path.
func operatorArtifact(rel string) string {
	if rel == operatorFile {
		return "guide"
	}
	return "skill"
}

// operatorRow returns a plan's row for a legacy path, nil when it has none.
func operatorRow(files []wireFile, rel string) *wireFile {
	for i := range files {
		if files[i].rel == rel {
			return &files[i]
		}
	}
	return nil
}

// A fresh init plans neither operator artifact, writes nothing under
// .claude, and closes its plan with the installation command.
func TestInitDoesNotPlanOperatorArtifacts(t *testing.T) {
	importRepo(t)
	wireGit(t, ".", "config", "core.excludesFile", os.DevNull)
	fakeInitPATH(t, "claude")
	for _, f := range planLegacyOperator(".") {
		if f.exists || f.changed() || f.err != nil {
			t.Errorf("a fresh repository plans %s: %+v", f.rel, f)
		}
	}
	code, out, errOut := initDo(t, filepath.Join(testsupport.ShortStateDir(t), "absent"), "", "--probe", "--accept-limited", "--agent", "harness=claude,model=sonnet", "--signed-in", "claude", "--check", "true", "--tools", "none")
	if code != 0 {
		t.Fatalf("got %d\n%s\n%s", code, out, errOut)
	}
	flat := strings.Join(strings.Fields(out), " ")
	for _, rel := range []string{operatorFile, operatorSkillFile, ".claude/"} {
		if strings.Contains(out, rel) {
			t.Errorf("the plan names %s:\n%s", rel, out)
		}
	}
	for _, want := range []string{
		"Operator skill: your own coding agent works with brigada through the brigada-operator skill, which init does not install.",
		"\n    " + operatorSkillInstall + "\n",
	} {
		if !strings.Contains(flat, strings.Join(strings.Fields(want), " ")) {
			t.Errorf("the plan does not close with the installation command, lacks %q:\n%s", want, out)
		}
	}
	for _, dir := range []string{".brigada", ".claude"} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("the probe wrote %s: %v", dir, err)
		}
	}
}

// A rerun of init and brigada remove share one plan for the operator
// artifacts an older init generated: a file carrying init's header goes
// with the emptied skill directory, and any other file at the two paths is
// the user's and stays. Each plan says which it did.
func TestLegacyOperatorRerunAndRemove(t *testing.T) {
	for _, tc := range []struct {
		name, guide, skill string
		gone               bool
	}{
		{"marked", legacyOperatorGuide, legacyOperatorSkill, true},
		{"unmarked", "# mine\n", "---\nname: brigada-operator\n---\nmine\n", false},
	} {
		for _, p := range operatorPlans() {
			t.Run(p.name+"/"+tc.name, func(t *testing.T) {
				root := wireRepo(t)
				wireWrite(t, root, operatorFile, tc.guide)
				wireWrite(t, root, operatorSkillFile, tc.skill)
				files := p.plan(root)
				plan := p.print(files)
				for _, rel := range []string{operatorFile, operatorSkillFile} {
					row := operatorRow(files, rel)
					if row == nil {
						t.Fatalf("%s is not planned:\n%s", rel, plan)
					}
					if (row.err != nil) != !tc.gone || row.changed() != tc.gone {
						t.Errorf("wrong plan for %s: %+v", rel, *row)
					}
					want, other := p.kept(rel), p.removed(rel)
					if tc.gone {
						want, other = other, want
					}
					if !strings.Contains(plan, want) || strings.Contains(plan, other) {
						t.Errorf("plan lacks %q or has %q:\n%s", want, other, plan)
					}
				}
				if err := p.apply(files); err != nil {
					t.Fatal(err)
				}
				for rel, body := range map[string]string{operatorFile: tc.guide, operatorSkillFile: tc.skill} {
					got, err := os.ReadFile(operatorPath(root, rel))
					if tc.gone != os.IsNotExist(err) || !tc.gone && (err != nil || string(got) != body) {
						t.Errorf("%s after %s: %q, %v", rel, p.name, got, err)
					}
				}
				// The emptied skill directory, and .claude with it, go with
				// the skill.
				if _, err := os.Stat(operatorPath(root, legacyOperatorSkillDir)); tc.gone != os.IsNotExist(err) {
					t.Errorf("skill directory after %s: %v", p.name, err)
				}
				if _, err := os.Stat(filepath.Join(root, ".claude")); tc.gone != os.IsNotExist(err) {
					t.Errorf(".claude after %s: %v", p.name, err)
				}
				for _, f := range p.plan(root) {
					if f.changed() {
						t.Errorf("a second %s plans %s again: %+v", p.name, f.rel, f)
					}
				}
			})
		}
	}
	t.Run("user content keeps the skill directory", func(t *testing.T) {
		root := wireRepo(t)
		wireWrite(t, root, operatorFile, legacyOperatorGuide)
		wireWrite(t, root, operatorSkillFile, legacyOperatorSkill)
		wireWrite(t, root, legacyOperatorSkillDir+"/notes.txt", "my notes\n")
		if err := applyRemoval(planRemoval(root, false)); err != nil {
			t.Fatal(err)
		}
		if got := wireRead(t, root, legacyOperatorSkillDir+"/notes.txt"); got != "my notes\n" {
			t.Fatalf("user content lost: %q", got)
		}
		if fileExists(operatorPath(root, operatorFile)) || fileExists(operatorPath(root, operatorSkillFile)) {
			t.Error("the marked artifacts remain")
		}
	})
}

// A skill the skills CLI installed at the legacy path is not brigada's. A
// copy, and the canonical skill behind a symlinked directory, survive a
// rerun and brigada remove. The symlinked one is one quiet row, where the
// symlink would otherwise be a fault on every rerun.
func TestInstalledOperatorSkillIsLeftAlone(t *testing.T) {
	canonical, err := os.ReadFile(operatorSkillPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(canonical), operatorHeaderPrefix) {
		t.Fatal("the canonical skill carries init's generated header")
	}
	linkSkill := func(t *testing.T, root, target string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(operatorPath(root, legacyOperatorSkillDir)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("..", "..", ".agents", "skills", target), operatorPath(root, legacyOperatorSkillDir)); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name    string
		install func(*testing.T, string)
		state   string // how both plans show the skill
	}{
		{"copy", func(t *testing.T, root string) {
			wireWrite(t, root, operatorSkillFile, string(canonical))
		}, "kept"},
		{"symlinked directory", func(t *testing.T, root string) {
			wireWrite(t, root, ".agents/skills/brigada-operator/SKILL.md", string(canonical))
			linkSkill(t, root, "brigada-operator")
		}, "unmanaged"},
		// A link that leads to no skill is a path brigada refuses, as before.
		{"link to nothing", func(t *testing.T, root string) {
			linkSkill(t, root, "absent")
		}, "refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := wireRepo(t)
			tc.install(t, root)
			for _, p := range operatorPlans() {
				t.Run(p.name, func(t *testing.T) {
					// Each plan removes the marked guide, so each gets its own.
					wireWrite(t, root, operatorFile, legacyOperatorGuide)
					files := p.plan(root)
					row := operatorRow(files, operatorSkillFile)
					if row == nil {
						t.Fatal("the skill is not planned")
					}
					if row.changed() {
						t.Errorf("the plan changes the installed skill: %+v", *row)
					}
					plan := p.print(files)
					if !strings.Contains(plan, p.removed(operatorFile)) {
						t.Errorf("the marked guide is not removed:\n%s", plan)
					}
					switch tc.state {
					case "unmanaged":
						if row.err != nil || row.purpose != installedSkillPurpose {
							t.Errorf("the installed skill is not one quiet row: %+v", *row)
						}
						if !strings.Contains(plan, p.unmanaged(operatorSkillFile)) || strings.Contains(plan, "left alone") || strings.Contains(plan, p.fault) {
							t.Errorf("the installed skill is not one quiet row:\n%s", plan)
						}
					case "refused":
						if row.err == nil || !strings.Contains(row.err.Error(), p.fault) {
							t.Errorf("a link to nothing is not refused: %+v", *row)
						}
					default:
						if row.err == nil || !strings.Contains(plan, p.kept(operatorSkillFile)) {
							t.Errorf("the kept copy is not reported as left alone: %+v\n%s", *row, plan)
						}
					}
					if err := p.apply(files); err != nil {
						t.Fatal(err)
					}
					// The marked guide still goes; the installed skill does not.
					if fileExists(operatorPath(root, operatorFile)) {
						t.Error("the marked guide remains")
					}
					if tc.state == "refused" {
						if target, err := os.Readlink(operatorPath(root, legacyOperatorSkillDir)); err != nil || filepath.Clean(target) != filepath.Join("..", "..", ".agents", "skills", "absent") {
							t.Errorf("the dangling link changed: %q, %v", target, err)
						}
						return
					}
					if got, err := os.ReadFile(operatorPath(root, operatorSkillFile)); err != nil || !bytes.Equal(got, canonical) {
						t.Fatalf("the installed skill changed: %q, %v", got, err)
					}
					if tc.state != "unmanaged" {
						return
					}
					if target, err := os.Readlink(operatorPath(root, legacyOperatorSkillDir)); err != nil || filepath.Clean(target) != filepath.Join("..", "..", ".agents", "skills", "brigada-operator") {
						t.Errorf("the install's symlink changed: %q, %v", target, err)
					}
					if got, err := os.ReadFile(filepath.Join(root, ".agents", "skills", "brigada-operator", "SKILL.md")); err != nil || !bytes.Equal(got, canonical) {
						t.Errorf("the canonical skill changed: %q, %v", got, err)
					}
					// The row is as quiet on the next run.
					if again := operatorRow(p.plan(root), operatorSkillFile); again.err != nil || again.changed() {
						t.Errorf("a second %s faults the installed skill: %+v", p.name, *again)
					}
				})
			}
		})
	}
}

// An applied init removes the marked operator artifacts an older init left,
// under --no-wiring too, which the design carves out of the wiring strip
// only, and leaves unmarked ones. A fresh init writes nothing under
// .claude. Init never runs the installer: an npx first on PATH leaves a
// marker when it runs, and no init run leaves one.
func TestInitRerunRemovesLegacyOperatorArtifacts(t *testing.T) {
	for _, tc := range []struct {
		name         string
		args         []string
		guide, skill string
	}{
		{"fresh", nil, "", ""},
		{"marked", nil, legacyOperatorGuide, legacyOperatorSkill},
		{"marked, --no-wiring", []string{"--no-wiring"}, legacyOperatorGuide, legacyOperatorSkill},
		{"unmarked", nil, "# mine\n", "---\nname: brigada-operator\n---\nmine\n"},
		{"unmarked, --no-wiring", []string{"--no-wiring"}, "# mine\n", "---\nname: brigada-operator\n---\nmine\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The remote is made before the engine: the plan-file mirror
			// pushes into it, and the engine's cleanup waits that push out.
			remote := t.TempDir()
			wireGit(t, remote, "init", "--bare", "-q")
			_, state := startSettingsEngine(t)
			importRepo(t)
			root, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			wireGit(t, root, "remote", "set-url", "origin", remote)
			wireGit(t, root, "config", "core.excludesFile", os.DevNull)
			if tc.guide != "" {
				wireWrite(t, root, operatorFile, tc.guide)
				wireWrite(t, root, operatorSkillFile, tc.skill)
			}
			startStopHelper(t, "runner", state)
			fakeInitPATH(t, "claude")
			marker := fakeNPX(t)
			args := append([]string{"--yes", "--parallel", "1", "--accept-limited", "--agent", "harness=claude,model=sonnet", "--signed-in", "claude", "--check", "true", "--tools", "none"}, tc.args...)
			code, out, errs := initDo(t, state, "", args...)
			if code != 0 {
				t.Fatalf("init code=%d\n%s\n%s", code, out, errs)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("init ran npx: marker %s, %v", marker, err)
			}
			next := "Next:\n  1. Talk to the foreman: brigada attach\n  2. Or, without a conversation: brigada ask \"what you want built\"\n  3. Or through your own coding agent: give it the operator skill with\n     " + operatorSkillInstall + "\n"
			if !strings.Contains(out, next) {
				t.Errorf("the next steps do not give the installation command:\n%s", out)
			}
			marked := strings.HasPrefix(tc.guide, operatorHeaderPrefix)
			flat := strings.Join(strings.Fields(out), " ")
			for _, rel := range []string{operatorFile, operatorSkillFile} {
				removed := "removed " + rel + " removes the operator " + operatorArtifact(rel) + " an older init generated"
				if marked != strings.Contains(flat, removed) {
					t.Errorf("the plan misreports %s:\n%s", rel, out)
				}
			}
			if tc.guide == "" {
				if _, err := os.Lstat(filepath.Join(root, ".claude")); !os.IsNotExist(err) {
					t.Errorf("a fresh init wrote under .claude: %v", err)
				}
				if _, err := os.Stat(operatorPath(root, operatorFile)); !os.IsNotExist(err) {
					t.Errorf("a fresh init wrote %s: %v", operatorFile, err)
				}
				return
			}
			for rel, body := range map[string]string{operatorFile: tc.guide, operatorSkillFile: tc.skill} {
				_, err := os.Stat(operatorPath(root, rel))
				switch {
				case marked && !os.IsNotExist(err):
					t.Errorf("init left the marked %s: %v", rel, err)
				case !marked && (err != nil || wireRead(t, root, rel) != body):
					t.Errorf("init touched the unmarked %s: %v", rel, err)
				}
			}
			// The emptied legacy skill directory goes with the skill.
			if _, err := os.Stat(operatorPath(root, legacyOperatorSkillDir)); marked != os.IsNotExist(err) {
				t.Errorf("legacy skill directory: %v", err)
			}
		})
	}
}
