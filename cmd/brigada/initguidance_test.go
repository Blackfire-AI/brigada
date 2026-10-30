package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/defaults"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// guidanceRepo is a directory with the project.yaml init would write.
func guidanceRepo(t *testing.T) (string, engine.ProjectConfig) {
	t.Helper()
	root := t.TempDir()
	base, err := engine.ProjectConfigDefaults(nil, "example.invalid/imported", "true", "none")
	if err != nil {
		t.Fatal(err)
	}
	content, err := engine.ProjectConfigGuidance(base)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := engine.ParseProjectConfig(content)
	if err != nil {
		t.Fatal(err)
	}
	return root, cfg
}

func applyGuidance(t *testing.T, root string, cfg engine.ProjectConfig) []wireFile {
	t.Helper()
	files := planGuidance(root, cfg)
	if err := applyWiring(files); err != nil {
		t.Fatal(err)
	}
	return files
}

const workerRel = ".brigada/instructions/worker.md"
const bugfixRel = ".brigada/skills/bugfix/SKILL.md"

func readRel(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeRel(t *testing.T, root, rel, text string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fileFor(files []wireFile, rel string) wireFile {
	for _, f := range files {
		if f.rel == rel {
			return f
		}
	}
	return wireFile{}
}

// TestInitGuidanceFirstRun: the four role files and eight skills are
// written in one pass under their headers, project.yaml names them, and
// every one imports through the client's importer.
func TestInitGuidanceFirstRun(t *testing.T) {
	root, cfg := guidanceRepo(t)
	if len(cfg.RoleFiles) != 4 || cfg.SkillsDir != ".brigada/skills" {
		t.Fatalf("project.yaml guidance = %v, %q", cfg.RoleFiles, cfg.SkillsDir)
	}
	files := applyGuidance(t, root, cfg)
	if len(files) != 12 {
		t.Fatalf("planned %d files, want 4 role files and 8 skills", len(files))
	}
	for _, f := range files {
		if f.exists || f.state != "created" || !f.changed() {
			t.Errorf("%s: exists %t, state %q", f.rel, f.exists, f.state)
		}
	}

	// Role files: the header is the first line. Skills: frontmatter first,
	// the header right after its closing line.
	worker, _ := defaults.LiveRoleText("worker")
	want := "<!-- brigada: version=" + brigadaVersion() + " digest=" + defaults.Digest(worker) + " -->\n" + worker
	if got := readRel(t, root, workerRel); got != want {
		t.Errorf("worker.md = %.120q, want the header and the default", got)
	}
	skill := readRel(t, root, bugfixRel)
	end, ok := skillFrontmatterEnd(skill)
	if !ok || !strings.HasPrefix(skill, "---\nname: bugfix\n") || !strings.HasPrefix(skill[end:], "<!-- brigada: version="+brigadaVersion()) {
		t.Errorf("bugfix SKILL.md = %.200q", skill)
	}

	// Every generated file imports, with the default's own digest.
	for _, role := range engine.RoleKeys {
		text, err := readRoleFile(root, role, cfg.RoleFiles[role])
		live, _ := defaults.LiveRoleText(role)
		if err != nil || text != live || textDigest(text) != defaults.Digest(live) {
			t.Errorf("role %s imports as %.60q, %v", role, text, err)
		}
	}
	skills, err := readSkills(root, cfg.SkillsDir)
	if err != nil || len(skills) != len(defaults.Skills) {
		t.Fatalf("readSkills = %d skills, %v", len(skills), err)
	}
	for _, s := range skills {
		live, _ := defaults.LiveSkillText(s.Name)
		if s.Text != live || s.Digest != defaults.Digest(live) {
			t.Errorf("skill %s imports as a different text", s.Name)
		}
	}

	// A rerun with nothing edited changes nothing.
	for _, f := range planGuidance(root, cfg) {
		if f.changed() || f.err != nil || f.state != "unchanged" || strings.Contains(f.purpose, "edited") {
			t.Errorf("%s: rerun changed=%t state %q purpose %q", f.rel, f.changed(), f.state, f.purpose)
		}
	}
}

// TestInitGuidancePartialRoleMapKept: once the instructions map exists a
// missing key is the user's choice (§5.9 'Three states'): init adds no
// key back, generates files only for the roles the map names, and its
// plan says the roles left out are not configured with the way to
// restore them.
func TestInitGuidancePartialRoleMapKept(t *testing.T) {
	base, err := engine.ProjectConfigDefaults(nil, "example.invalid/imported", "true", "none")
	if err != nil {
		t.Fatal(err)
	}
	partial := string(base) + "instructions:\n  worker: docs/worker-rules.md  # mine\nskills_dir: .brigada/skills\n"
	content, err := engine.ProjectConfigGuidance([]byte(partial))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != partial {
		t.Fatalf("a partial map gained keys:\n%s", content)
	}
	cfg, err := engine.ParseProjectConfig(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.RoleFiles) != 1 || cfg.RoleFiles["worker"] != "docs/worker-rules.md" {
		t.Fatalf("RoleFiles = %v\n%s", cfg.RoleFiles, content)
	}
	root := t.TempDir()
	files := applyGuidance(t, root, cfg)
	if got := len(files); got != 1+len(defaults.Skills) {
		t.Fatalf("planned %d files, want the mapped role's and the %d skills", got, len(defaults.Skills))
	}
	if !strings.HasPrefix(readRel(t, root, "docs/worker-rules.md"), "<!-- brigada: ") {
		t.Errorf("the mapped role's file was not generated")
	}
	for _, rel := range []string{".brigada/instructions/foreman.md", ".brigada/instructions/plan_review.md", ".brigada/instructions/reviewer.md"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("%s was generated for a role left out (%v)", rel, err)
		}
	}
	notes := guidanceNotes(cfg)
	if len(notes) != 3 {
		t.Fatalf("notes = %q, want the three roles left out", notes)
	}
	for _, role := range []string{"foreman", "plan_review", "reviewer"} {
		if !slices.ContainsFunc(notes, func(n string) bool {
			return strings.HasPrefix(n, "instructions."+role+": not configured") && strings.Contains(n, "brigada init --take-default "+role)
		}) {
			t.Errorf("no note restores %s: %q", role, notes)
		}
	}
	// A map leaving out the worker, with no instructions_file either,
	// notes the worker too: its key is the alias's or nobody's.
	cfg.RoleFiles = map[string]string{"foreman": ".brigada/instructions/foreman.md"}
	if notes = guidanceNotes(cfg); !slices.ContainsFunc(notes, func(n string) bool { return strings.HasPrefix(n, "instructions.worker: not configured") }) {
		t.Errorf("the worker left out is not noted: %q", notes)
	}
}

// TestInitGuidanceInstructionsFileGainsThreeRoles: a project still on
// the 1.0 instructions_file gets foreman.md, plan_review.md and
// reviewer.md generated and their keys in the instructions map, the
// worker staying on its alias — never both keys, which §5.9 refuses.
// The generated text is the shipped default, the same text the upgrade
// seeds, so the next import changes nothing; and with a partial map
// beside the alias nothing is added, for those keys were left out on
// purpose.
func TestInitGuidanceInstructionsFileGainsThreeRoles(t *testing.T) {
	base, err := engine.ProjectConfigDefaults(nil, "example.invalid/imported", "true", "none")
	if err != nil {
		t.Fatal(err)
	}
	withAlias := string(base) + "instructions_file: .brigada/instructions.md\n"
	content, err := engine.ProjectConfigGuidance([]byte(withAlias))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := engine.ParseProjectConfig(content)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InstructionsFile != ".brigada/instructions.md" {
		t.Errorf("instructions_file = %q, want the alias kept", cfg.InstructionsFile)
	}
	if _, both := cfg.RoleFiles[engine.RoleWorker]; both {
		t.Errorf("the worker gained a map key beside its alias:\n%s", content)
	}
	for _, role := range []string{"foreman", "plan_review", "reviewer"} {
		if cfg.RoleFiles[role] != engine.DefaultRoleFile(role) {
			t.Errorf("role %s = %q, want the default path", role, cfg.RoleFiles[role])
		}
	}
	root := t.TempDir()
	applyGuidance(t, root, cfg)
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(".brigada/instructions/worker.md"))); !os.IsNotExist(err) {
		t.Errorf("a worker file was generated beside the alias (%v)", err)
	}
	for _, role := range []string{"foreman", "plan_review", "reviewer"} {
		rel := engine.DefaultRoleFile(role)
		live, _ := defaults.LiveRoleText(role)
		if got := readRel(t, root, rel); got != generatedRoleFile(live) {
			t.Errorf("%s = %.80q, want the default under its header", rel, got)
		}
		if text, err := readRoleFile(root, role, rel); err != nil || text != live {
			t.Errorf("role %s imports as %.60q, %v", role, text, err)
		}
	}
	if notes := guidanceNotes(cfg); len(notes) != 0 {
		t.Errorf("the alias project has not-configured notes: %q", notes)
	}

	// The alias with a partial map: no new role keys, only the mapped
	// files, and the worker setting moves as the worker's own text does.
	aliasPartial := string(base) + "instructions_file: .brigada/instructions.md\ninstructions:\n  plan_review: pr.md\nskills_dir: .brigada/skills\n"
	if content, err = engine.ProjectConfigGuidance([]byte(aliasPartial)); err != nil {
		t.Fatal(err)
	}
	if string(content) != aliasPartial {
		t.Fatalf("the alias with a partial map gained keys:\n%s", content)
	}
	if cfg, err = engine.ParseProjectConfig(content); err != nil {
		t.Fatal(err)
	}
	root = t.TempDir()
	files := applyGuidance(t, root, cfg)
	if got := len(files); got != 1+len(defaults.Skills) {
		t.Fatalf("planned %d files, want the mapped role's and the skills", got)
	}
	if !strings.HasPrefix(readRel(t, root, "pr.md"), "<!-- brigada: ") {
		t.Errorf("the mapped role's file was not generated")
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(".brigada/instructions.md"))); !os.IsNotExist(err) {
		t.Errorf("the alias's own file was generated (%v)", err)
	}
}

// TestInitGuidanceRerun: an unedited generated file is rewritten when the
// default moved; an edited one is kept and the plan names the version and
// the diff command. Role files and skills alike.
func TestInitGuidanceRerun(t *testing.T) {
	root, cfg := guidanceRepo(t)
	applyGuidance(t, root, cfg)

	// An older default: header and text from another version, unedited.
	staleRole := "<!-- brigada: version=0.9.0 digest=" + defaults.Digest("old worker\n") + " -->\nold worker\n"
	live, _ := defaults.LiveSkillText("bugfix")
	end, _ := skillFrontmatterEnd(live)
	oldSkill := live[:end] + "old body\n"
	staleSkill := live[:end] + "<!-- brigada: version=0.9.0 digest=" + defaults.Digest(oldSkill) + " -->\n" + "old body\n"
	writeRel(t, root, workerRel, staleRole)
	writeRel(t, root, bugfixRel, staleSkill)
	plan := planGuidance(root, cfg)
	for _, rel := range []string{workerRel, bugfixRel} {
		f := fileFor(plan, rel)
		if !f.changed() || f.state != "replaced" || f.err != nil {
			t.Errorf("%s unedited stale: changed %t, state %q, err %v", rel, f.changed(), f.state, f.err)
		}
	}
	if err := applyWiring(plan); err != nil {
		t.Fatal(err)
	}
	if got := readRel(t, root, workerRel); !strings.Contains(got, "version="+brigadaVersion()) || strings.Contains(got, "old worker") {
		t.Errorf("worker.md was not rewritten: %.100q", got)
	}
	if got := readRel(t, root, bugfixRel); strings.Contains(got, "old body") {
		t.Errorf("bugfix SKILL.md was not rewritten")
	}

	// Edited since an older version whose default differs: kept.
	editedRole := "<!-- brigada: version=0.9.0 digest=" + defaults.Digest("old worker\n") + " -->\nmy own worker rules\n"
	editedSkill := live[:end] + "<!-- brigada: version=0.9.0 digest=" + defaults.Digest(oldSkill) + " -->\n" + "my own body\n"
	writeRel(t, root, workerRel, editedRole)
	writeRel(t, root, bugfixRel, editedSkill)
	plan = planGuidance(root, cfg)
	for rel, text := range map[string]string{workerRel: editedRole, bugfixRel: editedSkill} {
		f := fileFor(plan, rel)
		if f.changed() || f.err != nil || string(f.next) != text {
			t.Errorf("%s edited: changed %t, err %v", rel, f.changed(), f.err)
		}
		if !strings.Contains(f.purpose, "edited since version 0.9.0, new default differs") || !strings.Contains(f.purpose, "brigada instructions diff "+map[string]string{workerRel: "worker", bugfixRel: "bugfix"}[rel]+" ") || !strings.Contains(f.purpose, "--take-default") {
			t.Errorf("%s edited: purpose %q", rel, f.purpose)
		}
	}
	if err := applyWiring(plan); err != nil {
		t.Fatal(err)
	}
	if readRel(t, root, workerRel) != editedRole || readRel(t, root, bugfixRel) != editedSkill {
		t.Errorf("an edited file was rewritten")
	}
}

// TestInitGuidanceDetectionIgnoresOnlyTheHeader: a changed header alone
// keeps a file unedited; an edit to the body, the frontmatter's description
// or its for line makes it edited.
func TestInitGuidanceDetectionIgnoresOnlyTheHeader(t *testing.T) {
	text, _ := defaults.LiveSkillText("bugfix")
	gen := generatedSkillFile(text)
	if !isUnedited(gen, true) {
		t.Fatal("a fresh skill is edited")
	}
	if !isUnedited(strings.Replace(gen, "version="+brigadaVersion(), "version=7.7.7", 1), true) {
		t.Errorf("a version change in the header made the skill edited")
	}
	for name, edit := range map[string]string{
		"body":        strings.Replace(gen, "# bugfix", "# bugfix!", 1),
		"description": strings.Replace(gen, "description: ", "description: x ", 1),
		"for":         strings.Replace(gen, "description:", "for: worker\ndescription:", 1),
		"name":        strings.Replace(gen, "name: bugfix", "name: bugfix2", 1),
	} {
		if edit == gen || isUnedited(edit, true) {
			t.Errorf("a %s edit was not detected", name)
		}
	}
	role, _ := defaults.LiveRoleText("worker")
	genRole := generatedRoleFile(role)
	if !isUnedited(genRole, false) || isUnedited(genRole+" ", false) || isUnedited(role, false) {
		t.Errorf("role file detection wrong")
	}
}

// TestRemoveGuidance: brigada remove deletes unedited generated role files
// and skills, keeps an edited one, and never touches a user's own skill.
func TestRemoveGuidance(t *testing.T) {
	root, cfg := guidanceRepo(t)
	applyGuidance(t, root, cfg)
	writeRel(t, root, ".brigada/skills/mine/SKILL.md", "---\nname: mine\ndescription: mine\n---\nmy skill\n")
	writeRel(t, root, ".brigada/skills/bugfix/extra.md", "") // keeps its directory
	_ = os.Remove(filepath.Join(root, ".brigada/skills/bugfix/extra.md"))
	edited := readRel(t, root, workerRel) + "my addition\n"
	writeRel(t, root, workerRel, edited)
	editedSkill := strings.Replace(readRel(t, root, ".brigada/skills/refactor/SKILL.md"), "description: ", "description: mine, ", 1)
	writeRel(t, root, ".brigada/skills/refactor/SKILL.md", editedSkill)
	mine := readRel(t, root, ".brigada/skills/mine/SKILL.md")

	files := planRemoval(root, false)
	for _, rel := range []string{workerRel, ".brigada/skills/refactor/SKILL.md"} {
		if f := fileFor(files, rel); f.err == nil || f.changed() {
			t.Errorf("%s edited: err %v, changed %t; want it left alone", rel, f.err, f.changed())
		}
	}
	if f := fileFor(files, ".brigada/skills/mine/SKILL.md"); f.exists || f.err != nil || f.changed() {
		t.Errorf("a user's skill is in the plan: %+v", f)
	}
	if err := applyRemoval(files); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".brigada/instructions/foreman.md", ".brigada/instructions/plan_review.md", ".brigada/instructions/reviewer.md", bugfixRel, ".brigada/skills/cli-flag/SKILL.md"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("unedited %s was not deleted (%v)", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".brigada/skills/bugfix")); !os.IsNotExist(err) {
		t.Errorf("the emptied skill directory stayed")
	}
	if readRel(t, root, workerRel) != edited || readRel(t, root, ".brigada/skills/refactor/SKILL.md") != editedSkill {
		t.Errorf("an edited file changed")
	}
	if readRel(t, root, ".brigada/skills/mine/SKILL.md") != mine {
		t.Errorf("a user's skill changed")
	}
}
