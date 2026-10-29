package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// setRole sets one role's text, failing the test on a refusal.
func (e *env) setRole(role, text string) *RoleInstruction {
	e.t.Helper()
	return mustSetRole(e.t, e, role, text)
}

// importRole imports one role file's text, as a project.yaml import of
// the nested instructions map does.
func (e *env) importRole(role, text string) (*RoleInstruction, error) {
	e.t.Helper()
	return e.eng.ImportRoleFile(e.ctx, envRepo, role, text)
}

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
	e.setRole(RoleWorker, strings.Repeat("w", MaxRoleInstructionBytes))
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
	e.setRole(RoleWorker, "Use tabs.")
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

// TestCreatePlanResolvesInstructions: plan_create resolves what each
// task's prompts carry — the role texts and the ordered skills — and
// copies it onto the task, the worker text in the column the first
// job's prompt reads; an unknown or repeated skill, or a prompt over
// its bound, refuses the plan whole, the task named.
func TestCreatePlanResolvesInstructions(t *testing.T) {
	e := newEnv(t)
	e.importSkills(
		Skill{Name: "cli-flag", Text: "register the flag", TreeDigest: "t1"},
		Skill{Name: "surface-parity", For: SkillForReviewer, Text: "find the siblings"},
	)
	e.setRole(RoleWorker, "Use tabs.")
	e.setRole(RoleReviewer, "Check the changelog.")

	res := e.createPlan([]PlanTask{
		{Name: "skilled", Spec: "add the flag", Skills: []string{"surface-parity", "cli-flag"}},
		{Name: "plain", Spec: "no skills"},
	})
	skilled, plain := e.snapshot(res.Tasks[0].TaskID), e.snapshot(res.Tasks[1].TaskID)
	if got := skillNames(skilled.Skills); len(got) != 2 || got[0] != "surface-parity" || got[1] != "cli-flag" {
		t.Errorf("skills = %v, want the order given", got)
	}
	if skilled.Worker.Text != "Use tabs." || skilled.Worker.Source != SourceLocal || skilled.Reviewer.Text != "Check the changelog." {
		t.Errorf("skilled snapshot = %+v, want both role texts with provenance", skilled)
	}
	if len(plain.Skills) != 0 || plain.Worker.Text != "Use tabs." {
		t.Errorf("plain snapshot = %+v, want the texts and no skills", plain)
	}
	if got := e.taskInstructions(res.Tasks[0].TaskID); got != "Use tabs." {
		t.Errorf("task instructions = %q, want the worker text in the column jobs read", got)
	}
	// The first job's prompt carries the worker text and the
	// worker-audience skills alone: surface-parity is for the reviewer.
	if p := e.jobPrompt(res.Tasks[0].JobID); !strings.Contains(p, "<worker-instructions>\nUse tabs.\n</worker-instructions>") {
		t.Errorf("the first job's prompt does not carry the worker text:\n%s", p)
	} else if !strings.Contains(p, `<skill name="cli-flag">`) || !strings.Contains(p, "register the flag") || strings.Contains(p, "surface-parity") {
		t.Errorf("the first job's prompt carries %q, want the worker-audience skills alone:\n%s", skillNames(e.snapshot(res.Tasks[0].TaskID).Skills), p)
	}

	for name, tasks := range map[string][]PlanTask{
		"unknown skill": {{Name: "a", Spec: "s", Skills: []string{"nope"}}},
		"twice":         {{Name: "a", Spec: "s", Skills: []string{"cli-flag", "cli-flag"}}},
	} {
		_, err := e.eng.CreatePlan(e.ctx, e.project, "refused", tasks)
		if !errors.Is(err, ErrTaskInstructions) || !strings.Contains(err.Error(), `"a"`) {
			t.Errorf("%s: err = %v, want the task refused by name", name, err)
		}
	}
	if got := e.int("SELECT COUNT(*) FROM plans"); got != 2 {
		t.Errorf("plans after the refusals = %d, want the seeded 1 and the one created", got)
	}

	// A prompt over its bound refuses the plan too, the size, the
	// limit and the task named (§5.10).
	e.importSkills(
		Skill{Name: "a", Text: strings.Repeat("a", MaxSkillBytes)},
		Skill{Name: "b", Text: strings.Repeat("b", MaxSkillBytes)},
		Skill{Name: "wrk", For: SkillForWorker, Text: strings.Repeat("w", MaxSkillBytes)},
	)
	e.setRole(RoleWorker, strings.Repeat("t", MaxRoleInstructionBytes))
	_, err := e.eng.CreatePlan(e.ctx, e.project, "over", []PlanTask{{Name: "big", Spec: "s", Skills: []string{"a", "b", "wrk"}}})
	if !errors.Is(err, ErrTaskInstructions) || !strings.Contains(err.Error(), `"big"`) || !strings.Contains(err.Error(), "20480") {
		t.Errorf("over the bound: err = %v, want the size, the limit and the task named", err)
	}
}

// TestAmendResolvesAndReresolves: an added task resolves at the
// amendment as a created one does; a respec or retier of a task that
// has not started re-resolves its role texts and skills — nil skills
// keep the recorded names, a list replaces them, even empty — and a
// started task keeps its record.
func TestAmendResolvesAndReresolves(t *testing.T) {
	e := newEnv(t)
	e.importSkills(Skill{Name: "bugfix", Text: "v1 bugfix"}, Skill{Name: "docs", Text: "v1 docs"})
	e.setRole(RoleWorker, "v1 worker")
	e.setRole(RoleReviewer, "v1 reviewer")
	res := e.createPlan([]PlanTask{
		{Name: "build", Spec: "build it", Class: "answer"},
		{Name: "ship", Spec: "ship it", Class: "answer", Deps: []string{"build"}, Skills: []string{"bugfix"}},
	})
	build, ship := res.Tasks[0].TaskID, res.Tasks[1].TaskID
	if snap := e.snapshot(build); snap.Worker.Text != "v1 worker" || len(snap.Skills) != 0 {
		t.Fatalf("build snapshot = %+v, want the v1 worker text, no skills", snap)
	}
	if snap := e.snapshot(ship); len(snap.Skills) != 1 || snap.Skills[0].Text != "v1 bugfix" {
		t.Fatalf("ship snapshot = %+v, want bugfix v1", snap)
	}

	am := e.amend(res.PlanID, PlanAmendment{Add: []PlanTask{{Name: "docs-task", Spec: "write docs", Class: "answer", Deps: []string{"build"}, Skills: []string{"docs"}}}})
	if snap := e.snapshot(am.Added[0].TaskID); len(snap.Skills) != 1 || snap.Skills[0].Text != "v1 docs" || snap.Worker.Text != "v1 worker" {
		t.Errorf("added task snapshot = %+v, want docs v1 and the v1 texts", snap)
	}

	// The settings move on; a task that has not started follows.
	e.setRole(RoleWorker, "v2 worker")
	e.setRole(RoleReviewer, "v2 reviewer")
	e.importSkills(Skill{Name: "bugfix", Text: "v2 bugfix"}, Skill{Name: "docs", Text: "v2 docs"})

	e.amend(res.PlanID, PlanAmendment{Respec: []PlanRespec{{Task: "ship", Spec: "ship it harder"}}})
	snap := e.snapshot(ship)
	if snap.Worker.Text != "v2 worker" || snap.Reviewer.Text != "v2 reviewer" {
		t.Errorf("after the respec snapshot = %+v, want the v2 texts", snap)
	}
	if got := skillNames(snap.Skills); len(got) != 1 || got[0] != "bugfix" || snap.Skills[0].Text != "v2 bugfix" {
		t.Errorf("after the respec skills = %+v, want bugfix kept by name with the v2 text", snap.Skills)
	}
	e.amend(res.PlanID, PlanAmendment{Respec: []PlanRespec{{Task: "ship", Spec: "ship it harder", Skills: []string{"docs"}}}})
	if got := skillNames(e.snapshot(ship).Skills); len(got) != 1 || got[0] != "docs" {
		t.Errorf("after a respec with skills = %v, want docs replacing bugfix", got)
	}
	e.amend(res.PlanID, PlanAmendment{Respec: []PlanRespec{{Task: "ship", Spec: "ship it harder", Skills: []string{}}}})
	if snap := e.snapshot(ship); len(snap.Skills) != 0 {
		t.Errorf("after a respec with [] skills = %+v, want none", snap.Skills)
	}

	e.setRole(RoleWorker, "v3 worker")
	e.amend(res.PlanID, PlanAmendment{Retier: []PlanRetier{{Task: "ship", Tier: "light"}}})
	if got := e.taskInstructions(ship); got != "v3 worker" {
		t.Errorf("after a retier task instructions = %q, want the re-resolved v3 worker", got)
	}
	// A started task — build's work job is queued — keeps its record.
	e.amend(res.PlanID, PlanAmendment{Retier: []PlanRetier{{Task: "build", Effort: "low"}}})
	if got := e.taskInstructions(build); got != "v1 worker" {
		t.Errorf("after a retier of a started task instructions = %q, want the recorded v1 worker", got)
	}
	// A skills list on a started task's retier changes nothing it
	// recorded; a respec of it is refused outright.
	e.amend(res.PlanID, PlanAmendment{Retier: []PlanRetier{{Task: "build", Effort: "medium", Skills: []string{"docs"}}}})
	if snap := e.snapshot(build); len(snap.Skills) != 0 || snap.Worker.Text != "v1 worker" {
		t.Errorf("after a retier with skills of a started task snapshot = %+v, want the recorded v1 worker and no skills", snap)
	}
	if _, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{Respec: []PlanRespec{{Task: "build", Spec: "s", Skills: []string{"docs"}}}}); err == nil {
		t.Error("a respec of a started task was accepted")
	}
	if snap := e.snapshot(build); len(snap.Skills) != 0 || snap.Worker.Text != "v1 worker" {
		t.Errorf("after a refused respec of a started task snapshot = %+v, want it unchanged", snap)
	}
	// A retier's list and [] act on an unstarted task as a respec's do.
	e.amend(res.PlanID, PlanAmendment{Retier: []PlanRetier{{Task: "ship", Tier: "standard", Skills: []string{"bugfix"}}}})
	if got := skillNames(e.snapshot(ship).Skills); len(got) != 1 || got[0] != "bugfix" {
		t.Errorf("after a retier with skills = %v, want bugfix", got)
	}
	e.amend(res.PlanID, PlanAmendment{Retier: []PlanRetier{{Task: "ship", Tier: "light", Skills: []string{}}}})
	if snap := e.snapshot(ship); len(snap.Skills) != 0 {
		t.Errorf("after a retier with [] skills = %+v, want none", snap.Skills)
	}

	for name, am := range map[string]PlanAmendment{
		"add unknown":    {Add: []PlanTask{{Name: "bad", Spec: "s", Skills: []string{"nope"}}}},
		"respec unknown": {Respec: []PlanRespec{{Task: "ship", Spec: "s", Skills: []string{"nope"}}}},
		"retier unknown": {Retier: []PlanRetier{{Task: "ship", Skills: []string{"nope"}}}},
	} {
		if _, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, am); !errors.Is(err, ErrTaskInstructions) {
			t.Errorf("%s: err = %v, want ErrTaskInstructions", name, err)
		}
	}
	if got := e.taskInstructions(build); got != "v1 worker" {
		t.Errorf("after the refusals build instructions = %q, want the recorded v1 worker", got)
	}
}

// TestSubmitTaskRecordsSnapshots: the direct submission records both
// role snapshots and its skills at creation; a later change to the
// setting, a role text or the skills import never alters its fix or
// review prompts, and its first prompt carries the recorded snapshot —
// the worker text and the worker-audience skills for the worker, the
// reviewer's own text and the reviewer-audience skills for the review.
func TestSubmitTaskRecordsSnapshots(t *testing.T) {
	e := newEnv(t)
	e.importSkills(
		Skill{Name: "bugfix", Text: "v1 bugfix", TreeDigest: "tree-v1"},
		Skill{Name: "rev-guide", For: SkillForReviewer, Text: "v1 rev guide"},
	)
	e.setRole(RoleWorker, "v1 worker")
	e.setRole(RoleReviewer, "v1 reviewer")
	res := e.submitTask(TaskParams{RepoURL: envRepo, BaseRef: "main", Spec: "fix the bug", CheckCmd: "go test ./...", Skills: []string{"bugfix", "rev-guide"}})
	snap := e.snapshot(res.TaskID)
	if snap.Worker.Text != "v1 worker" || snap.Worker.Digest != instructionsDigest("v1 worker") || snap.Reviewer.Text != "v1 reviewer" || len(snap.Skills) != 2 {
		t.Fatalf("snapshot = %+v, want both role texts and the two skills", snap)
	}
	first := e.jobPrompt(res.JobID)
	if !strings.Contains(first, "<worker-instructions>\nv1 worker\n</worker-instructions>") {
		t.Errorf("the first prompt does not carry the recorded worker text:\n%s", first)
	}
	if !strings.Contains(first, `<skill name="bugfix">`) || !strings.Contains(first, "v1 bugfix") || strings.Contains(first, "rev-guide") {
		t.Errorf("the first prompt carries the worker-audience skills alone, want bugfix without rev-guide:\n%s", first)
	}

	author := e.agented("author", "anthropic", "implement")
	g := e.leaseAny(author)
	const newer = "v2 everything"
	e.set(map[string]string{SettingInstructions: newer})
	e.setRole(RoleWorker, newer)
	e.setRole(RoleReviewer, newer)
	e.importSkills(
		Skill{Name: "bugfix", Text: "v2 bugfix", TreeDigest: "tree-v2"},
		Skill{Name: "rev-guide", For: SkillForReviewer, Text: "v2 rev guide"},
	)
	if ans := e.complete(g, doneOn(branchOf(res.TaskID), failCheck)); ans != AnsOK {
		t.Fatalf("complete the failed-check work: %s", ans)
	}
	fix := e.jobPrompt(e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'fix'", res.TaskID))
	if strings.Count(fix, "v1 worker") != 1 || strings.Contains(fix, newer) {
		t.Errorf("fix prompt carries the recorded text %d times and the new %t; want the recorded text once and none of the new:\n%s",
			strings.Count(fix, "v1 worker"), strings.Contains(fix, newer), fix)
	}
	if !strings.Contains(fix, "v1 bugfix") || strings.Contains(fix, "v2 bugfix") || strings.Contains(fix, "rev-guide") {
		t.Errorf("fix prompt skills: want the recorded bugfix text and no rev-guide:\n%s", fix)
	}
	fg := e.authorLease(author)
	if fg == nil {
		t.Fatal("the fix round was not leased")
	}
	e.agented("reviewer", "google", "implement", "review")
	if ans := e.complete(fg, doneOn(branchOf(res.TaskID), passCheck)); ans != AnsOK {
		t.Fatalf("complete the fixed work: %s", ans)
	}
	review := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review'", res.TaskID)
	if review == 0 {
		t.Fatalf("no review job; have %v", e.eventKinds())
	}
	p := e.jobPrompt(review)
	if strings.Count(p, "v1 worker") != 1 || strings.Count(p, "v1 reviewer") != 1 || strings.Contains(p, newer) {
		t.Errorf("review prompt carries the worker text %d times, the reviewer text %d times and the new %t; want each recorded text once and none of the new:\n%s",
			strings.Count(p, "v1 worker"), strings.Count(p, "v1 reviewer"), strings.Contains(p, newer), p)
	}
	if !strings.Contains(p, "<reviewer-instructions>\nv1 reviewer\n</reviewer-instructions>") {
		t.Errorf("review prompt does not carry the recorded reviewer text in its own section:\n%s", p)
	}
	if !strings.Contains(p, `<skill name="bugfix">`) || !strings.Contains(p, `<skill name="rev-guide">`) || !strings.Contains(p, "v1 rev guide") || strings.Contains(p, "v2 ") {
		t.Errorf("review prompt skills: want the recorded bugfix and rev-guide texts and none of the new:\n%s", p)
	}
	if got := e.snapshot(res.TaskID); got.Worker.Text != "v1 worker" || got.Skills[0].Text != "v1 bugfix" || got.Skills[0].TreeDigest != "tree-v1" {
		t.Errorf("the run altered the snapshot: %+v", got)
	}
}

// TestTaskSnapshotSurvivesLaterChanges: a recorded task keeps its texts
// and skills whatever the settings and the skills import say later; a
// new resolution sees the new values.
func TestTaskSnapshotSurvivesLaterChanges(t *testing.T) {
	e := newEnv(t)
	e.importSkills(Skill{Name: "bugfix", Text: "v1 bugfix", TreeDigest: "tree-v1"})
	e.setRole(RoleWorker, "v1 worker")
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
	e.setRole(RoleWorker, "v2 worker")
	e.setRole(RoleReviewer, "v2 reviewer")
	e.set(map[string]string{SettingInstructions: "v2 setting"})

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
	// The instructions setting is the worker role's text (§5.9), so the
	// last write of either is the worker text a new task resolves.
	now, err := e.resolve("t", "bugfix")
	if err != nil || now.Worker.Text != "v2 setting" || now.Reviewer.Text != "v2 reviewer" || now.Skills[0].Text != "v2 bugfix" {
		t.Errorf("a new resolution = %+v, %v, want v2", now, err)
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
	e.setRole(RoleWorker, newer)
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
	if strings.Contains(p, "<reviewer-instructions>") {
		t.Errorf("the legacy review prompt has a reviewer-instructions section; want none beside the convention the author followed:\n%s", p)
	}
}

// TestJobRecordsPromptMeta: every job of a task records beside its
// prompt what the prompt was composed from (§5.9 'Versioning'): the
// template's version and the digests of the instruction and skill
// texts it carried — the worker template's for the author's jobs, the
// review template's with both role texts for the review — so a
// recorded prompt is reproducible from the recorded snapshot.
func TestJobRecordsPromptMeta(t *testing.T) {
	e := newEnv(t)
	e.importSkills(
		Skill{Name: "bugfix", Text: "v1 bugfix"},
		Skill{Name: "rev-guide", For: SkillForReviewer, Text: "v1 rev guide"},
	)
	e.setRole(RoleWorker, "v1 worker")
	e.setRole(RoleReviewer, "v1 reviewer")
	res := e.submitTask(TaskParams{RepoURL: envRepo, BaseRef: "main", Spec: "fix the bug", CheckCmd: "go test ./...", Skills: []string{"bugfix", "rev-guide"}})

	meta := func(job int64) promptMeta {
		var m promptMeta
		if raw := e.str("SELECT prompt_meta FROM jobs WHERE id = ?", job); raw == "" {
			t.Fatalf("job %d records no prompt_meta", job)
		} else if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("parse the prompt_meta of job %d: %v", job, err)
		}
		return m
	}
	snap := e.snapshot(res.TaskID)
	if m := meta(res.JobID); m.Template != prompt.WorkerTemplateVersion ||
		m.Instructions != instructionsDigest("v1 worker") || m.Reviewer != "" ||
		len(m.Skills) != 1 || m.Skills[0] != instructionsDigest("v1 bugfix") {
		t.Errorf("the work job's meta = %+v; want the worker template, the worker digest and the worker-audience skill alone", m)
	}
	if got, want := e.str("SELECT prompt_meta FROM jobs WHERE id = ?", res.JobID), marshalPromptMeta(workerPromptMeta(snap)); got != want {
		t.Errorf("the work job's prompt_meta = %q, want %q", got, want)
	}

	author := e.agented("author", "anthropic", "implement")
	g := e.leaseAny(author)
	if ans := e.complete(g, doneOn(branchOf(res.TaskID), failCheck)); ans != AnsOK {
		t.Fatalf("complete the failed-check work: %s", ans)
	}
	fix := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'fix'", res.TaskID)
	if got, want := e.str("SELECT prompt_meta FROM jobs WHERE id = ?", fix), marshalPromptMeta(workerPromptMeta(snap)); got != want {
		t.Errorf("the fix job's prompt_meta = %q, want %q: the snapshot, not the store", got, want)
	}
	fg := e.authorLease(author)
	if fg == nil {
		t.Fatal("the fix round was not leased")
	}
	e.agented("reviewer", "google", "implement", "review")
	if ans := e.complete(fg, doneOn(branchOf(res.TaskID), passCheck)); ans != AnsOK {
		t.Fatalf("complete the fixed work: %s", ans)
	}
	review := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review'", res.TaskID)
	if review == 0 {
		t.Fatalf("no review job; have %v", e.eventKinds())
	}
	if m := meta(review); m.Template != prompt.ReviewTemplateVersion ||
		m.Instructions != instructionsDigest("v1 worker") || m.Reviewer != instructionsDigest("v1 reviewer") ||
		len(m.Skills) != 2 || m.Skills[0] != instructionsDigest("v1 bugfix") || m.Skills[1] != instructionsDigest("v1 rev guide") {
		t.Errorf("the review job's meta = %+v; want the review template, both role digests and both skills", m)
	}
}

func TestSkillDirectoriesAndAudiences(t *testing.T) {
	for _, tc := range []struct{ name, config, dir string }{
		{"configured", "skills_dir: tools/skills\n", "tools/skills"},
		{"default", "skills_dir: .brigada/skills\n", ".brigada/skills"},
		{"unset", "", ".brigada/skills"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			content := []byte("schema: 1\nrepo: " + cfgRepo + "\n" + tc.config)
			result, err := e.eng.ImportProjectConfig(e.ctx, envRepo, content, true)
			if err != nil {
				t.Fatal(err)
			}
			if tc.config != "" {
				found := false
				for _, v := range result.Settings.Settings {
					if v.Key == configKeySkillsDir {
						found = true
						if v.Value != tc.dir || v.Source != SourceImported || v.Hash == "" {
							t.Errorf("setting = %+v", v)
						}
					}
				}
				if !found {
					t.Fatal("skills_dir missing from settings")
				}
			}
			e.importSkills(Skill{Name: "shared", Text: "shared text"}, Skill{Name: "author", For: SkillForWorker, Text: "author text"}, Skill{Name: "review", For: SkillForReviewer, Text: "review text"})
			res := e.submitTask(TaskParams{RepoURL: envRepo, BaseRef: "main", Spec: "a change", Skills: []string{"shared", "author", "review"}})
			work := e.jobPrompt(res.JobID)
			if !strings.Contains(work, "Directory: "+tc.dir+"/shared/") || strings.Contains(work, "review text") {
				t.Errorf("worker prompt: %s", work)
			}
			author := e.agented("author", "anthropic", "implement")
			e.agented("reviewer", "google", "implement", "review")
			if ans := e.complete(e.leaseAny(author), doneOn(branchOf(res.TaskID), passCheck)); ans != AnsOK {
				t.Fatalf("complete: %s", ans)
			}
			review := e.jobPrompt(e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review'", res.TaskID))
			if !strings.Contains(review, "Directory: "+tc.dir+"/shared/") || !strings.Contains(review, "Skill author was given to the author and is not shown") || strings.Contains(review, "author text") || !strings.Contains(review, "review text") {
				t.Errorf("review prompt: %s", review)
			}
		})
	}
}
