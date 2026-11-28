package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// stubRestart stands in for the API client in the task restart tests.
type stubRestart struct {
	task      int64
	note      string
	stop, cnt bool
	res       api.TaskRestartResult
	err       error
}

func (s *stubRestart) RestartTask(_ context.Context, task int64, note string, stop, cont bool, _ string) (api.TaskRestartResult, error) {
	s.task, s.note, s.stop, s.cnt = task, note, stop, cont
	return s.res, s.err
}

// TestTaskRestartContinueStopped: the command restarts through the
// client carrying the note and --continue-stopped, says what it did, and
// makes an engine refusal its own failure.
func TestTaskRestartContinueStopped(t *testing.T) {
	stub := &stubRestart{res: api.TaskRestartResult{Answer: engine.AnsCreated, JobID: 12}}
	var out, errb bytes.Buffer
	if code := printTaskRestart(context.Background(), stub, 7, "keep the cache", true, &out, &errb); code != 0 {
		t.Fatalf("restart: exit %d, stderr %q", code, errb.String())
	}
	if stub.task != 7 || stub.note != "keep the cache" || !stub.cnt || stub.stop {
		t.Errorf("client called with %+v", stub)
	}
	if want := "task 7 restarted: work job 12 created"; !strings.Contains(out.String(), want) {
		t.Errorf("stdout %q lacks %q", out.String(), want)
	}
	stub.err = errors.New("task 7 is parked by a failed run, not by a time-capped or stalled run")
	if code := printTaskRestart(context.Background(), stub, 7, "", true, &out, &errb); code != 1 || !strings.Contains(errb.String(), "not by a time-capped") {
		t.Errorf("exit %d, stderr %q on a refusal, want 1 naming it", code, errb.String())
	}
}

// TestTaskRestartUsage: one task id is required, and a misspelled flag
// is a usage error.
func TestTaskRestartUsage(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"no task", []string{"--continue-stopped"}, "usage: brigada task restart"},
		{"two tasks", []string{"7", "8"}, "usage: brigada task restart"},
		{"bad id", []string{"seven"}, "task id must be a positive integer"},
		{"misspelled flag", []string{"--continue-stoped", "7"}, "continue-stoped"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := runTaskRestart(context.Background(), c.args, &out, &errb); code != 2 {
				t.Fatalf("exit %d, stderr %q, want 2", code, errb.String())
			}
			if !strings.Contains(errb.String(), c.want) {
				t.Errorf("stderr %q lacks %q", errb.String(), c.want)
			}
		})
	}
}
