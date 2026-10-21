package engine

import (
	"strings"
	"testing"
)

// The land gate's protected-path rule on the task's recorded changed
// paths (§6.3, §7.1): the runner reports them with the work job's
// result, the engine stores them with the task, and the rule matches
// the real paths — the spec+check text stands in only when none are
// recorded.

// runWorkToReviewed drives the fixture's task through its work job to
// the point the review decides: done under a passing check, its author
// job complete, so the test can seed the task's recorded paths before
// the approving verdict fires the land rule.
func (e *env) runWorkToReviewed(author int64) {
	e.t.Helper()
	e.submit(e.work())
	g := e.leaseAny(author)
	if g == nil {
		e.t.Fatal("the work job was not leased")
	}
	if ans := e.complete(g, doneOn("task/1-1", passCheck)); ans != AnsOK {
		e.t.Fatalf("complete the work: %s", ans)
	}
}

// TestLandGateMatchesRecordedPaths: a task whose recorded paths touch a
// protected pattern waits at the land gate, though its spec and check
// never name the path.
func TestLandGateMatchesRecordedPaths(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingApprovalProtectedPaths: "infra/**"})
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.exec("UPDATE tasks SET spec = 'a quiet refactor' WHERE id = ?", e.task)
	e.runWorkToReviewed(author)
	e.exec(`UPDATE tasks SET changed_paths = ? WHERE id = ?`,
		`["internal/store/store.go","infra/gateway.yaml"]`, e.task)
	e.runReview(reviewer, `{"verdict":"approve","findings":"`+substantiveFindings+`"}`)

	if n := e.landJobs(); n != 0 {
		t.Fatalf("land jobs = %d, want none while held", n)
	}
	if reason := e.str("SELECT reason FROM land_holds WHERE task_id = ?", e.task); !strings.Contains(reason, "touches infra/**") {
		t.Errorf("land hold reason = %q, want the protected path the work touched", reason)
	}
}

// TestLandGateRecordedPathsWinOverProxy: recorded paths that match
// nothing let the landing run even when the spec's words would have
// tripped the old proxy — the real paths are what the rule reads.
func TestLandGateRecordedPathsWinOverProxy(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingApprovalProtectedPaths: "infra/**"})
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.exec("UPDATE tasks SET spec = 'rewrite the infra/gateway' WHERE id = ?", e.task)
	e.runWorkToReviewed(author)
	e.exec(`UPDATE tasks SET changed_paths = '["docs/notes.md"]' WHERE id = ?`, e.task)
	e.runReview(reviewer, `{"verdict":"approve","findings":"`+substantiveFindings+`"}`)

	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ?", e.task); n != 0 {
		t.Fatalf("land holds = %d, want none: the recorded paths match nothing", n)
	}
	if n := e.landJobs(); n != 1 {
		t.Errorf("land jobs = %d, want 1: the recorded paths are the rule's input", n)
	}
}

// TestWorkCompletionStoresChangedPaths: a work job's result carrying
// changed_paths stores them with the task, as the land gate reads them.
func TestWorkCompletionStoresChangedPaths(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	e.submit(e.work())
	g := e.leaseAny(author)
	if g == nil {
		t.Fatal("the work job was not leased")
	}
	c := doneOn("task/1-1", passCheck)
	c.Result = strings.Replace(c.Result, `"status":"done"`,
		`"status":"done","changed_paths":["a.go","infra/b.go"]`, 1)
	if ans := e.complete(g, c); ans != AnsOK {
		t.Fatalf("complete the work: %s", ans)
	}
	if got := e.str("SELECT changed_paths FROM tasks WHERE id = ?", e.task); got != `["a.go","infra/b.go"]` {
		t.Errorf("task changed_paths = %q, want the recorded array", got)
	}
}
