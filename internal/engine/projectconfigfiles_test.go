package engine

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// filesYaml is a project.yaml naming a default check, a skills_dir and
// the role files of the roles the import carries.
func filesYaml(check, dir string) []byte {
	return cfgFile("default_check: " + check + "\nskills_dir: " + dir +
		"\ninstructions:\n  foreman: .brigada/instructions/foreman.md\n  worker: .brigada/instructions/worker.md\n  reviewer: .brigada/instructions/reviewer.md\n")
}

// importFiles drives the whole §13.1 import — the yaml with the worker
// text it names, the role files and the skills — as the one step it is.
func (e *env) importFiles(content []byte, worker string, foreman, reviewer string, skills []Skill, accept bool) (ImportResult, error) {
	e.t.Helper()
	roles := map[string]string{RoleForeman: foreman, RoleWorker: worker, RoleReviewer: reviewer}
	return e.eng.ImportProjectConfigFiles(e.ctx, envRepo, content, &worker, roles, skills, accept)
}

// stateOf sums up what the four kinds of import hold, so a test can
// pin every previous value at once.
func (e *env) stateOf(t *testing.T) (check, dir, foreman, worker, reviewer string, names string) {
	e.t.Helper()
	ps := mustSettings(t, e)
	check = settingOf(t, *ps, SettingDefaultCheck).Value
	dir = settingOf(t, *ps, configKeySkillsDir).Value
	foreman = mustRole(t, e, RoleForeman).Text
	worker = mustRole(t, e, RoleWorker).Text
	reviewer = mustRole(t, e, RoleReviewer).Text
	got, err := e.eng.Skills(e.ctx, envRepo)
	if err != nil {
		t.Fatalf("read skills: %v", err)
	}
	for _, s := range got {
		names += s.Name + ","
	}
	return check, dir, foreman, worker, reviewer, names
}

// TestImportFilesChangesAllTogether: one import moves the four kinds of
// value together — the config keys, the skills_dir, the role texts and
// the skills (§13.1: an import is one step).
func TestImportFilesChangesAllTogether(t *testing.T) {
	e := newEnv(t)
	if _, err := e.importFiles(filesYaml("make test", "tools/skills"), "w1", "f1", "r1", []Skill{{Name: "alpha", Text: "a1"}}, false); err != nil {
		t.Fatalf("first import: %v", err)
	}
	res, err := e.importFiles(filesYaml("go test ./...", "other/skills"), "w2", "f2", "r2", []Skill{{Name: "beta", Text: "b2"}}, false)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if res.NeedsConfirm || len(res.Conflicts) != 0 {
		t.Fatalf("result = %+v, want a clean apply", res)
	}
	if check, dir, foreman, worker, reviewer, names := e.stateOf(t); check != "go test ./..." || dir != "other/skills" ||
		foreman != "f2" || worker != "w2" || reviewer != "r2" || names != "beta," {
		t.Fatalf("state = %q %q %q %q %q %q, want every kind at the file's second values", check, dir, foreman, worker, reviewer, names)
	}
	ps := mustSettings(t, e)
	for _, k := range []string{SettingDefaultCheck, configKeySkillsDir} {
		if s := settingOf(t, *ps, k); s.Source != SourceImported || s.Hash != res.Hash {
			t.Errorf("%s = %+v, want imported from the second file", k, s)
		}
	}
	for role, text := range map[string]string{RoleForeman: "f2", RoleWorker: "w2", RoleReviewer: "r2"} {
		if ri := mustRole(t, e, role); ri.Text != text || ri.Source != SourceImported {
			t.Errorf("%s = %+v, want %q imported", role, ri, text)
		}
	}
}

// TestImportFilesRollsBackOnALateFailure: a failure injected after the
// config keys and skills_dir were already written — a role text or a
// skill the engine refuses — rolls the whole import back, and the four
// kinds keep their previous values (§13.1: an import is one step).
func TestImportFilesRollsBackOnALateFailure(t *testing.T) {
	e := newEnv(t)
	if _, err := e.importFiles(filesYaml("make test", "tools/skills"), "w1", "f1", "r1", []Skill{{Name: "alpha", Text: "a1"}}, false); err != nil {
		t.Fatalf("first import: %v", err)
	}
	before := e.str("SELECT settings || '|' || settings_prov FROM projects WHERE id = ?", e.project)

	t.Run("a refused role text", func(t *testing.T) {
		_, err := e.importFiles(filesYaml("go test ./...", "other/skills"), "w2", "f2", strings.Repeat("r", MaxRoleInstructionBytes+1), []Skill{{Name: "beta", Text: "b2"}}, false)
		if !errors.Is(err, ErrInvalidSetting) || !strings.Contains(err.Error(), "reviewer") {
			t.Fatalf("err = %v, want the reviewer role named", err)
		}
	})
	t.Run("a refused skill", func(t *testing.T) {
		_, err := e.importFiles(filesYaml("go test ./...", "other/skills"), "w2", "f2", "r2",
			[]Skill{{Name: "beta", Text: "b2"}, {Name: "beta", Text: "twice"}}, false)
		if !errors.Is(err, ErrInvalidSetting) || !strings.Contains(err.Error(), "beta") {
			t.Fatalf("err = %v, want the skill named", err)
		}
	})
	t.Run("the previous values stand", func(t *testing.T) {
		if after := e.str("SELECT settings || '|' || settings_prov FROM projects WHERE id = ?", e.project); after != before {
			t.Fatalf("the settings row moved: %s -> %s", before, after)
		}
		if check, dir, foreman, worker, reviewer, names := e.stateOf(t); check != "make test" || dir != "tools/skills" ||
			foreman != "f1" || worker != "w1" || reviewer != "r1" || names != "alpha," {
			t.Fatalf("state = %q %q %q %q %q %q, want every kind back at the first values", check, dir, foreman, worker, reviewer, names)
		}
	})
}

// TestImportFilesKeptOverrideCommitsTheRest: a local override that keeps
// its role text is no failure — the conflict is reported with both
// texts, everything else the import carried commits (§5.9, §13.1).
func TestImportFilesKeptOverrideCommitsTheRest(t *testing.T) {
	e := newEnv(t)
	if _, err := e.importFiles(filesYaml("make test", "tools/skills"), "w1", "f1", "r1", []Skill{{Name: "alpha", Text: "a1"}}, false); err != nil {
		t.Fatalf("first import: %v", err)
	}
	e.setRole(RoleReviewer, "my local reviewer")

	res, err := e.importFiles(filesYaml("go test ./...", "other/skills"), "w2", "f2", "r2", []Skill{{Name: "beta", Text: "b2"}}, false)
	if err != nil {
		t.Fatalf("import over a local override: %v", err)
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0].Key != "instructions."+RoleReviewer ||
		res.Conflicts[0].Old != strconv.Quote("my local reviewer") || res.Conflicts[0].New != strconv.Quote("r2") {
		t.Fatalf("conflicts = %+v, want the reviewer's two texts", res.Conflicts)
	}
	if ri := mustRole(t, e, RoleReviewer); ri.Text != "my local reviewer" || ri.Source != SourceLocal {
		t.Errorf("reviewer = %+v, want the local override kept", ri)
	}
	if check, dir, foreman, worker, _, names := e.stateOf(t); check != "go test ./..." || dir != "other/skills" ||
		foreman != "f2" || worker != "w2" || names != "beta," {
		t.Fatalf("state = %q %q %q %q %q, want everything but the reviewer committed", check, dir, foreman, worker, names)
	}
}
