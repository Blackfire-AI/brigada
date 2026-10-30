package engine

import (
	"strings"
	"testing"
)

// impSkill builds one valid skill for an import.
func impSkill(name, text string) Skill {
	return Skill{Name: name, Description: "d " + name, Text: text}
}

// TestImportProjectAllOrNothing: one import is one transaction (§13.1) —
// the config keys, the skills_dir, the role texts and the skills land
// together or not at all. A failure injected into the skills write, the
// last step, after the steps before it already wrote in the same
// transaction, rolls the whole import back and every previous value
// stands (§5.9); the same import then succeeds whole, changing all four
// kinds of value together.
func TestImportProjectAllOrNothing(t *testing.T) {
	e := newEnv(t)
	do := func(body string, roles map[string]string, skills []Skill) (ImportResult, error) {
		return e.eng.ImportProject(e.ctx, envRepo, cfgFile(body), nil, roles, &skills, true)
	}
	if _, err := do("approval_max_tasks: 2\nskills_dir: .brigada/skills\ninstructions:\n  foreman: f.md\n  worker: w.md\n",
		map[string]string{RoleForeman: "foreman one", RoleWorker: "worker one"}, []Skill{impSkill("alpha", "alpha v1")}); err != nil {
		t.Fatalf("first import: %v", err)
	}

	// The skills write fails after the config keys, the skills_dir and
	// the role texts were already written in the same transaction.
	e.exec("CREATE TRIGGER fail_skills BEFORE INSERT ON skills BEGIN SELECT RAISE(ABORT, 'injected skills failure'); END")
	_, err := do("approval_max_tasks: 5\nskills_dir: tools/skills\ninstructions:\n  foreman: f.md\n  worker: w.md\n",
		map[string]string{RoleForeman: "foreman two", RoleWorker: "worker two"}, []Skill{impSkill("beta", "beta v1")})
	if err == nil || !strings.Contains(err.Error(), "injected skills failure") {
		t.Fatalf("err = %v, want the injected skills-write failure", err)
	}
	ps := mustSettings(t, e)
	if sv := settingOf(t, *ps, SettingApprovalMaxTasks); sv.Value != "2" {
		t.Errorf("approval_max_tasks after the rolled-back import = %+v, want the previous 2", sv)
	}
	if sv := settingOf(t, *ps, configKeySkillsDir); sv.Value != ".brigada/skills" || sv.Source != SourceImported {
		t.Errorf("skills_dir after the rolled-back import = %+v, want the previous import's", sv)
	}
	for role, want := range map[string]string{RoleForeman: "foreman one", RoleWorker: "worker one"} {
		if ri := mustRole(t, e, role); ri.Source != SourceImported || ri.Text != want {
			t.Errorf("%s after the rolled-back import = %+v, want the previous %q", role, ri, want)
		}
	}
	if got, err := e.eng.Skills(e.ctx, envRepo); err != nil || len(got) != 1 || got[0].Name != "alpha" {
		t.Fatalf("skills after the rolled-back import = %+v, %v; want the previous alpha alone", got, err)
	}

	// Without the failure the same import succeeds whole: all four kinds
	// of value move together.
	e.exec("DROP TRIGGER fail_skills")
	res, err := do("approval_max_tasks: 5\nskills_dir: tools/skills\ninstructions:\n  foreman: f.md\n  worker: w.md\n",
		map[string]string{RoleForeman: "foreman two", RoleWorker: "worker two"}, []Skill{impSkill("beta", "beta v1")})
	if err != nil {
		t.Fatalf("import after the failure was lifted: %v", err)
	}
	if res.NeedsConfirm {
		t.Fatalf("result = %+v, want the import applied", res)
	}
	ps = mustSettings(t, e)
	if sv := settingOf(t, *ps, SettingApprovalMaxTasks); sv.Value != "5" {
		t.Errorf("approval_max_tasks after the import = %+v, want 5", sv)
	}
	if sv := settingOf(t, *ps, configKeySkillsDir); sv.Value != "tools/skills" {
		t.Errorf("skills_dir after the import = %+v, want tools/skills", sv)
	}
	for role, want := range map[string]string{RoleForeman: "foreman two", RoleWorker: "worker two"} {
		if ri := mustRole(t, e, role); ri.Text != want {
			t.Errorf("%s after the import = %+v, want the file's %q", role, ri, want)
		}
	}
	if got, err := e.eng.Skills(e.ctx, envRepo); err != nil || len(got) != 1 || got[0].Name != "beta" {
		t.Fatalf("skills after the import = %+v, %v; want beta alone", got, err)
	}
}

// TestImportProjectKeptOverrideCommits: a local override that keeps its
// text is a reported conflict, not a failure (§5.9): one import commits
// everything else — the config keys, the skills_dir, the other roles'
// texts, the skills — and reports the kept role in Conflicts.
func TestImportProjectKeptOverrideCommits(t *testing.T) {
	e := newEnv(t)
	body := "approval_max_tasks: 2\nskills_dir: .brigada/skills\ninstructions:\n  foreman: f.md\n  reviewer: r.md\n  worker: w.md\n"
	roles := func(reviewer string) map[string]string {
		return map[string]string{RoleForeman: "foreman one", RoleReviewer: reviewer, RoleWorker: "worker one"}
	}
	if _, err := e.eng.ImportProject(e.ctx, envRepo, cfgFile(body), nil,
		roles("reviewer one"), &[]Skill{impSkill("alpha", "alpha v1")}, true); err != nil {
		t.Fatalf("first import: %v", err)
	}
	mustSetRole(t, e, RoleReviewer, "my local reviewer text")
	res, err := e.eng.ImportProject(e.ctx, envRepo,
		cfgFile("approval_max_tasks: 5\nskills_dir: tools/skills\ninstructions:\n  foreman: f.md\n  reviewer: r.md\n  worker: w.md\n"),
		nil, map[string]string{RoleForeman: "foreman two", RoleReviewer: "reviewer two", RoleWorker: "worker two"},
		&[]Skill{impSkill("beta", "beta v1")}, true)
	if err != nil {
		t.Fatalf("import over the override: %v", err)
	}
	var conflict *ConfigChange
	for i, c := range res.Conflicts {
		if c.Key == "instructions."+RoleReviewer {
			conflict = &res.Conflicts[i]
		}
	}
	if conflict == nil || !strings.Contains(conflict.Old, "my local reviewer text") || !strings.Contains(conflict.New, "reviewer two") {
		t.Fatalf("the kept override was not reported as a conflict: %+v", res.Conflicts)
	}
	if ri := mustRole(t, e, RoleReviewer); ri.Source != SourceLocal || ri.Text != "my local reviewer text" {
		t.Errorf("reviewer = %+v, want the local text kept", ri)
	}
	if ri := mustRole(t, e, RoleForeman); ri.Text != "foreman two" {
		t.Errorf("foreman = %+v, want the file's new text", ri)
	}
	if ri := mustRole(t, e, RoleWorker); ri.Text != "worker two" {
		t.Errorf("worker = %+v, want the file's new text", ri)
	}
	ps := mustSettings(t, e)
	if sv := settingOf(t, *ps, SettingApprovalMaxTasks); sv.Value != "5" {
		t.Errorf("approval_max_tasks = %+v, want 5", sv)
	}
	if sv := settingOf(t, *ps, configKeySkillsDir); sv.Value != "tools/skills" {
		t.Errorf("skills_dir = %+v, want tools/skills", sv)
	}
	if got, err := e.eng.Skills(e.ctx, envRepo); err != nil || len(got) != 1 || got[0].Name != "beta" {
		t.Fatalf("skills = %+v, %v; want beta alone, the override kept nothing back", got, err)
	}
}
