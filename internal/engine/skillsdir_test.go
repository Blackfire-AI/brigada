package engine

import (
	"database/sql"
	"strings"
	"testing"
)

// importSkillsDir re-imports the project config with the given skills_dir.
func (e *env) importSkillsDir(dir string) {
	e.t.Helper()
	content := []byte("schema: 1\nrepo: " + cfgRepo + "\nskills_dir: " + dir + "\n")
	if _, err := e.eng.ImportProjectConfig(e.ctx, envRepo, content, true); err != nil {
		e.t.Fatal(err)
	}
}

// failThenReview drives a task through a failed check, its fix round and
// the review, returning the fix job's and the review job's prompts.
func (e *env) failThenReview(taskID int64) (fix, review string) {
	e.t.Helper()
	author := e.agented("author", "anthropic", "implement")
	if ans := e.complete(e.leaseAny(author), doneOn(branchOf(taskID), failCheck)); ans != AnsOK {
		e.t.Fatalf("complete the failed-check work: %s", ans)
	}
	fix = e.jobPrompt(e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'fix'", taskID))
	fg := e.authorLease(author)
	if fg == nil {
		e.t.Fatal("the fix round was not leased")
	}
	e.agented("reviewer", "google", "implement", "review")
	if ans := e.complete(fg, doneOn(branchOf(taskID), passCheck)); ans != AnsOK {
		e.t.Fatalf("complete the fixed work: %s", ans)
	}
	review = e.jobPrompt(e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review'", taskID))
	return fix, review
}

// TestTaskKeepsRecordedSkillsDir: the directory line is resolved at task
// creation (§5.10); a re-import that changes skills_dir leaves the fix
// round and the review of an existing task naming the first directory,
// and only a task created afterwards names the new one. A task with no
// recorded value gets the default, never the current setting.
func TestTaskKeepsRecordedSkillsDir(t *testing.T) {
	e := newEnv(t)
	e.importSkillsDir("tools/skills")
	e.importSkills(Skill{Name: "shared", Text: "shared text"})
	res := e.submitTask(TaskParams{RepoURL: envRepo, BaseRef: "main", Spec: "a change", Skills: []string{"shared"}})
	if got := e.str("SELECT skills_dir FROM tasks WHERE id = ?", res.TaskID); got != "tools/skills" {
		t.Fatalf("recorded skills_dir = %q, want tools/skills", got)
	}
	e.importSkillsDir("other/skills")

	fix, review := e.failThenReview(res.TaskID)
	for name, p := range map[string]string{"fix round": fix, "review": review} {
		if !strings.Contains(p, "Directory: tools/skills/shared/") || strings.Contains(p, "other/skills") {
			t.Errorf("%s prompt: %s", name, p)
		}
	}

	later := e.submitTask(TaskParams{RepoURL: envRepo, BaseRef: "main", Spec: "another change", Skills: []string{"shared"}})
	if p := e.jobPrompt(later.JobID); !strings.Contains(p, "Directory: other/skills/shared/") {
		t.Errorf("later task prompt: %s", p)
	}

	e.exec("UPDATE tasks SET skills_dir = '' WHERE id = ?", later.TaskID)
	err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		dir, err := taskSkillsDirTx(tx, later.TaskID)
		if err == nil && dir != DefaultSkillsDir {
			t.Errorf("unrecorded skills_dir = %q, want the default", dir)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestBackfilledSkillsDirSurvivesImport: a task carrying a backfilled
// value (as migration 0070 writes it) names that directory in its fix
// round and review after the project's skills_dir changes.
func TestBackfilledSkillsDirSurvivesImport(t *testing.T) {
	e := newEnv(t)
	e.importSkills(Skill{Name: "shared", Text: "shared text"})
	res := e.submitTask(TaskParams{RepoURL: envRepo, BaseRef: "main", Spec: "a change", Skills: []string{"shared"}})
	e.exec("UPDATE tasks SET skills_dir = ? WHERE id = ?", "backfilled/skills", res.TaskID)
	e.importSkillsDir("later/skills")

	fix, review := e.failThenReview(res.TaskID)
	for name, p := range map[string]string{"fix round": fix, "review": review} {
		if !strings.Contains(p, "Directory: backfilled/skills/shared/") || strings.Contains(p, "later/skills") {
			t.Errorf("%s prompt: %s", name, p)
		}
	}
}
