package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// stubRetire stands in for the API client in the task retire tests
// (§6.2).
type stubRetire struct {
	tasks  []int64
	reason string
	res    engine.RetireResult
	err    error
}

func (s *stubRetire) RetireTasks(_ context.Context, tasks []int64, reason, _ string) (engine.RetireResult, error) {
	s.tasks, s.reason = tasks, reason
	return s.res, s.err
}

// TestTaskRetirePrintsWhatItDid: the command retires through the client
// and says what it did — each retired task with the reason, and the
// state each touched plan reached; a client error is the command's own
// failure.
func TestTaskRetirePrintsWhatItDid(t *testing.T) {
	stub := &stubRetire{res: engine.RetireResult{
		Retired: []int64{7, 8},
		Plans:   []engine.RetiredPlan{{PlanID: 3, State: "done"}},
	}}
	var out, errb bytes.Buffer
	if code := printTaskRetire(context.Background(), stub, []int64{7, 8}, "superseded by task 274", &out, &errb); code != 0 {
		t.Fatalf("retire: exit %d, stderr %q", code, errb.String())
	}
	if len(stub.tasks) != 2 || stub.tasks[0] != 7 || stub.reason != "superseded by task 274" {
		t.Errorf("client called with %v %q", stub.tasks, stub.reason)
	}
	for _, want := range []string{"task 7 retired: superseded by task 274", "task 8 retired", "plan 3 is done"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout %q lacks %q", out.String(), want)
		}
	}
	stub.err = errors.New("task 7 is landed (main@abc123): a landed task is not retired")
	out.Reset()
	if code := printTaskRetire(context.Background(), stub, []int64{7}, "late", &out, &errb); code != 1 {
		t.Errorf("exit %d on a client error, want 1", code)
	}
}

// TestTaskRetireUsage: the reason is required and at least one task id
// with it, both as usage errors the command prints itself (§6.2).
func TestTaskRetireUsage(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"no reason", []string{"7"}, "task retire needs --reason TEXT"},
		{"blank reason", []string{"--reason", "  ", "7"}, "task retire needs --reason TEXT"},
		{"no tasks", []string{"--reason", "superseded"}, "usage: brigada task retire --reason TEXT <task-id>..."},
		{"bad id", []string{"--reason", "superseded", "seven"}, "task id must be a positive integer"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := runTaskRetire(context.Background(), c.args, &out, &errb); code != 2 {
				t.Fatalf("exit %d, stderr %q, want 2", code, errb.String())
			}
			if !strings.Contains(errb.String(), c.want) {
				t.Errorf("stderr %q lacks %q", errb.String(), c.want)
			}
		})
	}
}

// TestTaskRetireFlagsAfterIds: ids and flags interleave, as `land`'s do
// — a reason after the ids is parsed, not lost to the flag package's
// stop at the first positional. The trailing bad id is what proves it:
// the parse reached it, so the failure names the id, not a missing
// reason.
func TestTaskRetireFlagsAfterIds(t *testing.T) {
	var out, errb bytes.Buffer
	code := runTaskRetire(context.Background(), []string{"7", "--reason", "superseded by task 274", "nine"}, &out, &errb)
	if code != 2 {
		t.Fatalf("exit %d, stderr %q, want 2", code, errb.String())
	}
	if !strings.Contains(errb.String(), "task id must be a positive integer") {
		t.Errorf("stderr %q names the bad id, want the parse to have reached it", errb.String())
	}
	if strings.Contains(errb.String(), "needs --reason") {
		t.Errorf("stderr %q says the reason is missing, want the reason after the ids parsed", errb.String())
	}
}
