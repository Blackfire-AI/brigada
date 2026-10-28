package engine

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// importSkills imports the project's skills, failing the test on a
// refusal.
func (e *env) importSkills(skills ...Skill) []Skill {
	e.t.Helper()
	got, err := e.eng.ImportSkills(e.ctx, envRepo, skills)
	if err != nil {
		e.t.Fatalf("import skills: %v", err)
	}
	return got
}

// resolve runs the resolver for the fixture's project in a transaction.
func (e *env) resolve(task string, names ...string) (TaskInstructions, error) {
	e.t.Helper()
	var ti TaskInstructions
	err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		var err error
		ti, err = resolveTaskInstructionsTx(tx, e.project, task, names)
		return err
	})
	return ti, err
}

// snapshot reads a task's recorded snapshot.
func (e *env) snapshot(task int64) TaskInstructions {
	e.t.Helper()
	var ti TaskInstructions
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		var err error
		ti, err = taskSnapshotTx(tx, task)
		return err
	}); err != nil {
		e.t.Fatalf("read the snapshot of task %d: %v", task, err)
	}
	return ti
}

func skillNames(s []TaskSkill) []string {
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = x.Name
	}
	return out
}

// TestImportSkillsBoundAndReplace: an import stores each skill's text,
// description, audience, digest and tree digest; a skill over 4096 bytes,
// not UTF-8, with a NUL, a bad name, a wrong digest or a name given twice
// refuses the import whole, naming the skill (and the size and limit for
// the bound), and the previous skills stay; a re-import replaces the set.
func TestImportSkillsBoundAndReplace(t *testing.T) {
	e := newEnv(t)
	atLimit := strings.Repeat("a", MaxSkillBytes)
	got := e.importSkills(
		Skill{Name: "cli-flag", Description: "add a flag", Text: atLimit, TreeDigest: "tree1"},
		Skill{Name: "bugfix", Description: "fix a bug", For: SkillForWorker, Text: "reproduce first", Digest: instructionsDigest("reproduce first")},
	)
	if len(got) != 2 || got[0].Name != "bugfix" || got[1].Name != "cli-flag" {
		t.Fatalf("imported = %+v, want bugfix and cli-flag", got)
	}
	if got[1].For != SkillForBoth || got[1].Digest != instructionsDigest(atLimit) || got[1].TreeDigest != "tree1" || got[1].Text != atLimit {
		t.Errorf("cli-flag = %+v, want both, its digest and tree digest", got[1])
	}
	if got[0].For != SkillForWorker || got[0].Description != "fix a bug" {
		t.Errorf("bugfix = %+v, want for worker with its description", got[0])
	}

	for name, tc := range map[string]struct {
		skills []Skill
		want   []string
	}{
		"over the bound": {[]Skill{{Name: "ok", Text: "fine"}, {Name: "big", Text: atLimit + "b"}}, []string{"big", "4097", "4096"}},
		"nul":            {[]Skill{{Name: "nul", Text: "a\x00b"}}, []string{"nul", "NUL"}},
		"not utf-8":      {[]Skill{{Name: "bad", Text: "a\xffb"}}, []string{"bad", "UTF-8"}},
		"bad name":       {[]Skill{{Name: "Bad Name", Text: "x"}}, []string{"Bad Name"}},
		"bad for":        {[]Skill{{Name: "x", For: "foreman", Text: "x"}}, []string{"x", "foreman"}},
		"wrong digest":   {[]Skill{{Name: "x", Text: "x", Digest: "00"}}, []string{"x", "digest"}},
		"twice":          {[]Skill{{Name: "x", Text: "x"}, {Name: "x", Text: "y"}}, []string{"x", "twice"}},
		"empty":          {[]Skill{{Name: "x"}}, []string{"x", "no text"}},
	} {
		_, err := e.eng.ImportSkills(e.ctx, envRepo, tc.skills)
		if !errors.Is(err, ErrInvalidSetting) {
			t.Errorf("%s: err = %v, want ErrInvalidSetting", name, err)
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: err = %v, want %q named", name, err, w)
			}
		}
	}
	if got, err := e.eng.Skills(e.ctx, envRepo); err != nil || len(got) != 2 || got[1].Text != atLimit {
		t.Fatalf("after refusals skills = %+v, %v, want the previous two untouched", got, err)
	}

	// A re-import replaces the set: a skill absent from it is gone.
	got = e.importSkills(Skill{Name: "refactor", Text: "no API change"})
	if len(got) != 1 || got[0].Name != "refactor" {
		t.Errorf("after the re-import skills = %+v, want refactor alone", got)
	}
	if got = e.importSkills(); len(got) != 0 {
		t.Errorf("after an empty import skills = %+v, want none", got)
	}
}

// TestResolveTaskInstructionsBounds: the worker prompt carries the worker
// text and the skills not for the reviewer alone, at most 16384 bytes;
// the reviewer prompt carries both role texts and the skills not for the
// worker alone, at most 24576 bytes. Two skills at their maxima are
// always admitted; above a bound the task is refused with the size, the
// limit and the task named.
func TestResolveTaskInstructionsBounds(t *testing.T) {
	e := newEnv(t)
	full := strings.Repeat("s", MaxSkillBytes)
	e.importSkills(
		Skill{Name: "a", Text: full},
		Skill{Name: "b", Text: full},
		Skill{Name: "rev", For: SkillForReviewer, Text: full},
		Skill{Name: "wrk", For: SkillForWorker, Text: full},
		Skill{Name: "tiny", Text: "t"},
	)
	e.set(map[string]string{SettingInstructions: strings.Repeat("w", MaxInstructionsBytes)})
	e.setRole(RoleReviewer, strings.Repeat("r", MaxSkillBytes))

	// Worker 8192 + two skills = 16384; reviewer 4096 + 8192 + 8192 + a
	// reviewer skill 4096 = 24576: both at their limits, admitted.
	ti, err := e.resolve("at-limit", "a", "b", "rev")
	if err != nil {
		t.Fatalf("at the limits: %v", err)
	}
	if ti.WorkerBytes() != MaxWorkerPromptBytes || ti.ReviewerBytes() != MaxReviewerPromptBytes {
		t.Errorf("bytes worker %d reviewer %d, want %d and %d", ti.WorkerBytes(), ti.ReviewerBytes(), MaxWorkerPromptBytes, MaxReviewerPromptBytes)
	}

	// A worker skill counts only in the worker prompt: one more byte of
	// it there refuses, naming the worker prompt.
	_, err = e.resolve("worker-over", "a", "b", "wrk")
	if !errors.Is(err, ErrTaskInstructions) {
		t.Fatalf("worker over: err = %v, want ErrTaskInstructions", err)
	}
	for _, w := range []string{`"worker-over"`, "worker prompt", "20480", "16384", "nothing is truncated"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("worker over: err = %v, want %q", err, w)
		}
	}

	// The reviewer prompt over by one byte refuses, naming it.
	e.setRole(RoleReviewer, strings.Repeat("r", MaxSkillBytes+1))
	_, err = e.resolve("reviewer-over", "a", "b", "rev")
	if !errors.Is(err, ErrTaskInstructions) {
		t.Fatalf("reviewer over: err = %v, want ErrTaskInstructions", err)
	}
	for _, w := range []string{`"reviewer-over"`, "reviewer prompt", "24577", "24576"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("reviewer over: err = %v, want %q", err, w)
		}
	}
	// Without the reviewer skill the same task fits.
	if _, err := e.resolve("fits", "a", "b"); err != nil {
		t.Errorf("without the reviewer skill: %v", err)
	}
}

// TestResolveTaskInstructionsRefusesAndOrders: an unknown or repeated
// skill refuses the task whole, naming it; the skills are copied in the
// order given with their digests, and the role texts with provenance.
func TestResolveTaskInstructionsRefusesAndOrders(t *testing.T) {
	e := newEnv(t)
	e.importSkills(
		Skill{Name: "cli-flag", Text: "register the flag", TreeDigest: "t1"},
		Skill{Name: "surface-parity", For: SkillForReviewer, Text: "find the siblings"},
	)
	e.set(map[string]string{SettingInstructions: "Use tabs."})
	e.setRole(RoleReviewer, "Check the changelog.")

	for name, names := range map[string][]string{
		"unknown": {"cli-flag", "nope"},
		"twice":   {"cli-flag", "cli-flag"},
	} {
		_, err := e.resolve("t1", names...)
		if !errors.Is(err, ErrTaskInstructions) || !strings.Contains(err.Error(), `"t1"`) {
			t.Errorf("%s: err = %v, want the task refused by name", name, err)
		}
	}
	if _, err := e.resolve("t1", "nope"); err == nil || !strings.Contains(err.Error(), "cli-flag, surface-parity") {
		t.Errorf("unknown skill: err = %v, want the known skills listed", err)
	}

	ti, err := e.resolve("t1", "surface-parity", "cli-flag")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := skillNames(ti.Skills); len(got) != 2 || got[0] != "surface-parity" || got[1] != "cli-flag" {
		t.Errorf("skills = %v, want the order given", got)
	}
	if s := ti.Skills[1]; s.Text != "register the flag" || s.Digest != instructionsDigest(s.Text) || s.TreeDigest != "t1" || s.For != SkillForBoth {
		t.Errorf("cli-flag copy = %+v, want text, digest, tree digest and audience", s)
	}
	if ti.Worker.Text != "Use tabs." || ti.Worker.Source != SourceLocal || ti.Worker.Digest != instructionsDigest("Use tabs.") {
		t.Errorf("worker = %+v, want the set text with its provenance", ti.Worker)
	}
	if ti.Reviewer.Text != "Check the changelog." || ti.Reviewer.Bytes != len("Check the changelog.") {
		t.Errorf("reviewer = %+v, want the set text", ti.Reviewer)
	}
	if ti, err := e.resolve("none"); err != nil || len(ti.Skills) != 0 {
		t.Errorf("no skills: %+v, %v", ti.Skills, err)
	}
}

// TestTaskSnapshotSurvivesLaterChanges: a recorded task keeps its texts
// and skills whatever the settings and the skills import say later; a
// new resolution sees the new values.
func TestTaskSnapshotSurvivesLaterChanges(t *testing.T) {
	e := newEnv(t)
	e.importSkills(Skill{Name: "bugfix", Text: "v1 bugfix", TreeDigest: "tree-v1"})
	e.set(map[string]string{SettingInstructions: "v1 worker"})
	e.setRole(RoleReviewer, "v1 reviewer")
	ti, err := e.resolve("t", "bugfix")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		return recordTaskInstructionsTx(tx, e.task, ti, e.clock.Now())
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if got := e.taskInstructions(e.task); got != "v1 worker" {
		t.Fatalf("task instructions = %q, want the worker text in the column jobs read", got)
	}

	e.importSkills(Skill{Name: "bugfix", Text: "v2 bugfix", TreeDigest: "tree-v2"})
	e.set(map[string]string{SettingInstructions: "v2 setting"})
	e.setRole(RoleWorker, "v2 role row")
	e.setRole(RoleReviewer, "v2 reviewer")

	got := e.snapshot(e.task)
	if got.Worker.Text != "v1 worker" || got.Worker.Digest != instructionsDigest("v1 worker") || got.Worker.Source != SourceLocal {
		t.Errorf("worker = %+v, want v1 with its provenance", got.Worker)
	}
	if got.Reviewer.Text != "v1 reviewer" {
		t.Errorf("reviewer = %+v, want v1", got.Reviewer)
	}
	if len(got.Skills) != 1 || got.Skills[0].Text != "v1 bugfix" || got.Skills[0].TreeDigest != "tree-v1" {
		t.Errorf("skills = %+v, want v1", got.Skills)
	}
	now, err := e.resolve("t", "bugfix")
	if err != nil || now.Worker.Text != "v2 setting" || now.Reviewer.Text != "v2 reviewer" || now.Skills[0].Text != "v2 bugfix" {
		t.Errorf("a new resolution = %+v, %v, want the new texts and skill", now, err)
	}
}

// TestLegacyTaskKeepsFirstGuidance: a task recorded before the skills
// arrived — its instructions and provenance in the old columns, its
// reviewer snapshot explicitly empty — that has already run keeps what
// it was first given: its fix round and its review carry the old text,
// none of the new, and the review shows it exactly once, as the
// convention the author followed.
func TestLegacyTaskKeepsFirstGuidance(t *testing.T) {
	e := newEnv(t)
	const old = "Legacy rule: every exported name gets a doc comment."
	e.exec(`UPDATE tasks SET instructions = ?, instructions_meta = ?, reviewer_instructions = '',
		reviewer_instructions_meta = '{"source":"legacy","bytes":0}', skills = '' WHERE id = ?`,
		old, `{"source":"imported","hash":"cafe01","digest":"`+instructionsDigest(old)+`","bytes":52}`, e.task)
	if snap := e.snapshot(e.task); snap.Worker.Text != old || snap.Worker.Version != "cafe01" || snap.Reviewer.Text != "" || len(snap.Skills) != 0 {
		t.Fatalf("legacy snapshot = %+v, want the old worker text, its import hash, an empty reviewer, no skills", snap)
	}
	author := e.agented("author", "anthropic", "implement")
	e.submit(e.work())
	g := e.leaseAny(author)

	// The settings change once the task has run, before its fix round
	// and its review are composed.
	const newer = "New rule: never write doc comments."
	e.set(map[string]string{SettingInstructions: newer})
	e.setRole(RoleReviewer, newer)
	if ans := e.complete(g, doneOn("task/1-1", failCheck)); ans != AnsOK {
		t.Fatalf("complete the failed-check work: %s", ans)
	}

	fix := e.jobPrompt(e.fixJob())
	if strings.Count(fix, old) != 1 || strings.Contains(fix, newer) {
		t.Errorf("fix prompt carries old %d times, new %t; want the old text once and none of the new:\n%s",
			strings.Count(fix, old), strings.Contains(fix, newer), fix)
	}
	fg := e.authorLease(author)
	if fg == nil {
		t.Fatal("the fix round was not leased")
	}
	e.agented("reviewer", "google", "implement", "review")
	if ans := e.complete(fg, doneOn("task/1-2", passCheck)); ans != AnsOK {
		t.Fatalf("complete the fixed work: %s", ans)
	}
	review := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review'", e.task)
	if review == 0 {
		t.Fatalf("no review job; have %v", e.eventKinds())
	}
	p := e.jobPrompt(review)
	if strings.Count(p, old) != 1 || strings.Contains(p, newer) {
		t.Errorf("review prompt carries old %d times, new %t; want the old text exactly once and none of the new:\n%s",
			strings.Count(p, old), strings.Contains(p, newer), p)
	}
}
