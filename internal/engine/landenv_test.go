package engine

import (
	"fmt"
	"strings"
	"testing"
)

// The environmental land-cause tests (issue #116): a landing that fails
// on the environment — a fetch the network would not carry, refused
// credentials, a broken remote — parks its task under a cause key shared
// by every task the same cause fails, and the foreman is woken once per
// cause while it is active, the tasks listed in the wake and the
// needs-you queue. A failure about the task's own content keeps waking
// per task.

// landEnvFetchErr is a land job's error as a network failure would word
// it: the runner's fetch of the target and the task's branch, with the
// host that would not resolve.
const landEnvFetchErr = "fetch main, brigada/task-1 from https://example.invalid/brigada.git: exit 128: could not resolve host example.invalid"

// landEnvAuthErr is a land job's error as missing credentials word it:
// git's own refusal to ask for the username.
const landEnvAuthErr = "clone https://example.invalid/brigada.git: could not read Username for 'https://example.invalid': terminal prompts disabled"

// landEnvKey is the cause key both errors' classes derive to for the
// fixture's repo: the class and the shared remote.
const landEnvNetKey = "network:https://example.invalid/brigada.git"

// needsYouTaskItems is the needs-you queue's task-kind entries.
func (e *env) needsYouTaskItems() []NeedsYouItem {
	e.t.Helper()
	info, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		e.t.Fatalf("status: %v", err)
	}
	var items []NeedsYouItem
	for _, it := range info.NeedsYou {
		if it.Kind == "task" {
			items = append(items, it)
		}
	}
	return items
}

// TestLandEnvFailureWakesOncePerCause: N tasks failing on one
// environmental cause produce one wake, whose line and needs-you entry
// list every task the cause parks; each task still gets its own
// task_land_failed event in the log.
func TestLandEnvFailureWakesOncePerCause(t *testing.T) {
	e := newEnv(t)
	author := e.agented("landenv-author", "anthropic", "implement")
	reviewer := e.agented("landenv-reviewer", "google", "implement", "review")
	t1, t2, t3 := e.task, e.addTask("{}"), e.addTask("{}")

	for _, task := range []int64{t1, t2, t3} {
		e.runTaskToDone(task, author, reviewer)
	}
	for _, task := range []int64{t1, t2, t3} {
		g := e.leaseGit(author)
		if g == nil || g.Kind != KindLand || g.TaskID != task {
			t.Fatalf("the land job of task %d was not leased: %+v", task, g)
		}
		if ans := e.complete(g, Completion{Error: landEnvFetchErr}); ans != AnsOK {
			t.Fatalf("complete the land job of task %d: %s", task, ans)
		}
	}
	if got := e.foremanJobs(); got != 1 {
		t.Errorf("foreman turns after three failures on one cause = %d, want 1", got)
	}
	if got := e.countEvents(evLandEnvFailed); got != 1 {
		t.Errorf("land_env_failed events = %d, want 1", got)
	}
	if got := e.countEvents(evTaskLandFailed); got != 3 {
		t.Errorf("task_land_failed events = %d, want 3: the log keeps one per task", got)
	}
	for _, task := range []int64{t1, t2, t3} {
		if state := e.str("SELECT state FROM tasks WHERE id = ?", task); state != taskNeedsForeman {
			t.Errorf("task %d state = %q, want needs_foreman", task, state)
		}
		if cause := e.str("SELECT land_env_cause FROM tasks WHERE id = ?", task); cause != landEnvNetKey {
			t.Errorf("task %d land cause = %q, want %q", task, cause, landEnvNetKey)
		}
	}
	// One needs-you line for the cause, listing all three tasks; no task
	// stands alone.
	items := e.needsYouTaskItems()
	if len(items) != 1 {
		t.Fatalf("needs-you task entries = %d, want the one cause line:\n%v", len(items), items)
	}
	if items[0].ID != t1 || items[0].State != taskNeedsForeman {
		t.Errorf("the cause line = %+v, want it on the first failed task", items[0])
	}
	for _, task := range []int64{t1, t2, t3} {
		if want := fmt.Sprintf("task %d (", task); !strings.Contains(items[0].Reason, want) {
			t.Errorf("the cause line = %q, want it to list %s", items[0].Reason, want)
		}
	}
	if it, ok := e.needsYouTask(t2); ok {
		t.Errorf("task %d also has its own needs-you entry: %+v", t2, it)
	}
	// The wake's line lists the tasks parked at its reading.
	prompt := e.str("SELECT prompt FROM jobs WHERE kind = 'foreman' ORDER BY id DESC LIMIT 1")
	for _, task := range []int64{t1, t2, t3} {
		if want := fmt.Sprintf("task %d (", task); !strings.Contains(prompt, want) {
			t.Errorf("the wake prompt = %q, want it to list %s", prompt, want)
		}
	}
}

// TestLandEnvFailureTwoCausesTwoWakes: two different environment causes
// wake apart — one episode event each — and the needs-you queue carries
// one line per cause.
func TestLandEnvFailureTwoCausesTwoWakes(t *testing.T) {
	e := newEnv(t)
	author := e.agented("landenv-author", "anthropic", "implement")
	reviewer := e.agented("landenv-reviewer", "google", "implement", "review")
	t1, t2 := e.task, e.addTask("{}")

	for _, task := range []int64{t1, t2} {
		e.runTaskToDone(task, author, reviewer)
	}
	g := e.leaseGit(author)
	if ans := e.complete(g, Completion{Error: landEnvFetchErr}); ans != AnsOK {
		t.Fatalf("complete the network-failed landing: %s", ans)
	}
	if got := e.foremanJobs(); got != 1 {
		t.Fatalf("foreman turns after the first cause = %d, want 1", got)
	}
	g = e.leaseGit(author)
	if ans := e.complete(g, Completion{Error: landEnvAuthErr}); ans != AnsOK {
		t.Fatalf("complete the auth-failed landing: %s", ans)
	}
	// The second cause's row waits for the live turn's completion, as
	// any wake does while a turn runs.
	if got := e.foremanJobs(); got != 1 {
		t.Fatalf("foreman turns while the first runs = %d, want 1", got)
	}
	if ans := e.complete(e.leaseForeman(), Completion{Result: "judged both"}); ans != AnsOK {
		t.Fatalf("complete the first turn: %s", ans)
	}
	if got := e.foremanJobs(); got != 2 {
		t.Errorf("foreman turns after the second cause = %d, want 2", got)
	}
	if got := e.countEvents(evLandEnvFailed); got != 2 {
		t.Errorf("land_env_failed events = %d, want 2", got)
	}
	items := e.needsYouTaskItems()
	if len(items) != 2 {
		t.Fatalf("needs-you task entries = %d, want one per cause:\n%v", len(items), items)
	}
	if !strings.Contains(items[0].Reason, "cannot reach") || !strings.Contains(items[1].Reason, "credentials") {
		t.Errorf("the cause lines = %q / %q, want the network and the auth wording", items[0].Reason, items[1].Reason)
	}
}

// TestLandEnvFailureRecurrenceWakesAfterRecovery: a later landing that
// succeeds ends the cause's episode, and a failure after it wakes anew.
func TestLandEnvFailureRecurrenceWakesAfterRecovery(t *testing.T) {
	e := newEnv(t)
	author := e.agented("landenv-author", "anthropic", "implement")
	reviewer := e.agented("landenv-reviewer", "google", "implement", "review")
	t1, t2, t3 := e.task, e.addTask("{}"), e.addTask("{}")

	for _, task := range []int64{t1, t2, t3} {
		e.runTaskToDone(task, author, reviewer)
	}
	g := e.leaseGit(author)
	if g.TaskID != t1 {
		t.Fatalf("the first land job = task %d, want %d", g.TaskID, t1)
	}
	if ans := e.complete(g, Completion{Error: landEnvFetchErr}); ans != AnsOK {
		t.Fatalf("complete the first failed landing: %s", ans)
	}
	if ans := e.complete(e.leaseForeman(), Completion{Result: "judged"}); ans != AnsOK {
		t.Fatalf("complete the wake turn: %s", ans)
	}
	// A landing that succeeds proves the environment again: the episode
	// ends with its event.
	g = e.leaseGit(author)
	if g.TaskID != t2 {
		t.Fatalf("the second land job = task %d, want %d", g.TaskID, t2)
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"abc123","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("complete the successful landing: %s", ans)
	}
	if got := e.countEvents(evLandEnvRecovered); got != 1 {
		t.Fatalf("land_env_recovered events = %d, want 1", got)
	}
	// The same cause failing again is a recurrence: a new episode, a new
	// wake.
	g = e.leaseGit(author)
	if g.TaskID != t3 {
		t.Fatalf("the third land job = task %d, want %d", g.TaskID, t3)
	}
	if ans := e.complete(g, Completion{Error: landEnvFetchErr}); ans != AnsOK {
		t.Fatalf("complete the recurring failure: %s", ans)
	}
	if got := e.countEvents(evLandEnvFailed); got != 2 {
		t.Errorf("land_env_failed events = %d, want 2", got)
	}
	if got := e.foremanJobs(); got != 2 {
		t.Errorf("foreman turns = %d, want 2: the recurrence woke anew", got)
	}
	// The parked tasks of both episodes share the one cause line.
	items := e.needsYouTaskItems()
	if len(items) != 1 {
		t.Fatalf("needs-you task entries = %d, want the one cause line:\n%v", len(items), items)
	}
	for _, task := range []int64{t1, t3} {
		if want := fmt.Sprintf("task %d (", task); !strings.Contains(items[0].Reason, want) {
			t.Errorf("the cause line = %q, want it to list %s", items[0].Reason, want)
		}
	}
}

// TestLandEnvFailureRecurrenceAfterForemanActs: when the foreman has
// taken every task the cause parked off the queue, a later failure on
// the same cause wakes anew.
func TestLandEnvFailureRecurrenceAfterForemanActs(t *testing.T) {
	e := newEnv(t)
	author := e.agented("landenv-author", "anthropic", "implement")
	reviewer := e.agented("landenv-reviewer", "google", "implement", "review")
	t1, t2, t3 := e.task, e.addTask("{}"), e.addTask("{}")

	for _, task := range []int64{t1, t2, t3} {
		e.runTaskToDone(task, author, reviewer)
	}
	for _, task := range []int64{t1, t2} {
		g := e.leaseGit(author)
		if g.TaskID != task {
			t.Fatalf("the land job = task %d, want %d", g.TaskID, task)
		}
		if ans := e.complete(g, Completion{Error: landEnvFetchErr}); ans != AnsOK {
			t.Fatalf("complete the failed landing of task %d: %s", task, ans)
		}
	}
	if got := e.countEvents(evLandEnvFailed); got != 1 {
		t.Fatalf("land_env_failed events = %d, want 1", got)
	}
	// The foreman restarts both parked tasks: the cause holds nothing.
	for _, task := range []int64{t1, t2} {
		if _, err := e.eng.RestartTask(e.ctx, e.project, task, "the network is back"); err != nil {
			t.Fatalf("restart task %d: %v", task, err)
		}
	}
	if ans := e.complete(e.leaseForeman(), Completion{Result: "judged"}); ans != AnsOK {
		t.Fatalf("complete the wake turn: %s", ans)
	}
	// The same cause failing a third task is a new episode.
	g := e.leaseGit(author)
	if g.TaskID != t3 {
		t.Fatalf("the land job = task %d, want %d", g.TaskID, t3)
	}
	if ans := e.complete(g, Completion{Error: landEnvFetchErr}); ans != AnsOK {
		t.Fatalf("complete the recurring failure: %s", ans)
	}
	if got := e.countEvents(evLandEnvFailed); got != 2 {
		t.Errorf("land_env_failed events = %d, want 2", got)
	}
	if got := e.foremanJobs(); got != 2 {
		t.Errorf("foreman turns = %d, want 2: the recurrence woke anew", got)
	}
}

// TestLandContentFailureStillWakesPerTask: a failure about the task's
// own content — a conflict — parks and wakes per task as always, beside
// the cause's one line.
func TestLandContentFailureStillWakesPerTask(t *testing.T) {
	e := newEnv(t)
	author := e.agented("landenv-author", "anthropic", "implement")
	reviewer := e.agented("landenv-reviewer", "google", "implement", "review")
	t1, t2 := e.task, e.addTask("{}")

	for _, task := range []int64{t1, t2} {
		e.runTaskToDone(task, author, reviewer)
	}
	g := e.leaseGit(author)
	if g.TaskID != t1 {
		t.Fatalf("the first land job = task %d, want %d", g.TaskID, t1)
	}
	if ans := e.complete(g, Completion{Error: landEnvFetchErr}); ans != AnsOK {
		t.Fatalf("complete the environment-failed landing: %s", ans)
	}
	g = e.leaseGit(author)
	if g.TaskID != t2 {
		t.Fatalf("the second land job = task %d, want %d", g.TaskID, t2)
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"","check_passed":false,"conflict_files":["a.go"]}`}); ans != AnsOK {
		t.Fatalf("complete the conflicting landing: %s", ans)
	}
	// The conflict's row waits for the live turn, then wakes its own
	// turn: one wake per task, as content failures always did.
	if got := e.foremanJobs(); got != 1 {
		t.Fatalf("foreman turns while the first runs = %d, want 1", got)
	}
	if ans := e.complete(e.leaseForeman(), Completion{Result: "judged"}); ans != AnsOK {
		t.Fatalf("complete the first turn: %s", ans)
	}
	if got := e.foremanJobs(); got != 2 {
		t.Errorf("foreman turns after the conflict = %d, want 2", got)
	}
	if got := e.countEvents(evLandEnvFailed); got != 1 {
		t.Errorf("land_env_failed events = %d, want 1: the conflict is no cause", got)
	}
	if cause := e.str("SELECT land_env_cause FROM tasks WHERE id = ?", t2); cause != "" {
		t.Errorf("the conflicted task's land cause = %q, want none", cause)
	}
	// The queue carries the conflict's own line beside the cause's.
	it, ok := e.needsYouTask(t2)
	if !ok || !strings.Contains(it.Reason, "land conflict") {
		t.Errorf("the conflicted task's needs-you entry = %+v (%t), want its own line", it, ok)
	}
	items := e.needsYouTaskItems()
	if len(items) != 2 {
		t.Errorf("needs-you task entries = %d, want the cause line and the conflict's:\n%v", len(items), items)
	}
}

// TestLandEnvClass is the classifier's table (issue #116): the
// environment's own words and kinds sort, the landing's own content
// does not.
func TestLandEnvClass(t *testing.T) {
	for _, tc := range []struct {
		kind, msg, want string
	}{
		{"", "fetch main, brigada/task-1 from https://example.invalid/brigada.git: exit 128", "remote"},
		{"", "fetch the waiting landings from https://example.invalid/brigada.git: exit 128", "remote"},
		{"", "look for staging on https://example.invalid/brigada.git: exit 128", "remote"},
		{"", "clone https://example.invalid/brigada.git: fatal: the remote end hung up", "remote"},
		{"", "fetch main from https://example.invalid/brigada.git: could not resolve host example.invalid", "network"},
		{"", "fetch main from https://example.invalid/brigada.git: connection timed out", "network"},
		{"", "clone https://example.invalid/brigada.git: could not read Username for 'https://example.invalid': terminal prompts disabled", "auth"},
		{"", "fetch main from https://example.invalid/brigada.git: permission denied (publickey)", "auth"},
		{ErrorKindInfra, "the plumbing failed", "network"},
		{ErrorKindSetup, "the setup script exited 3", "setup"},
		{"runner_lost", "runner lost 3 times", "runner"},
		{"", "the task's diff no longer applies onto main: the merge conflicted", ""},
		{"", "brigada/task-1 is at abc, not the approved head def: the branch moved after its review; nothing landed", ""},
		{"", "the push was rejected after 5 attempts (the target moved): non-fast-forward", ""},
		{"", "read the fetched head of brigada/task-1: exit 128", ""},
		{"", "detach the landing clone: exit 1", ""},
		{"", "the land job is incomplete: target \"main\", branch \"\", base \"main\"", ""},
	} {
		if got := landEnvClass(tc.kind, tc.msg); got != tc.want {
			t.Errorf("landEnvClass(%q, %q) = %q, want %q", tc.kind, tc.msg, got, tc.want)
		}
	}
}
