package runner

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/adapter/fake"
	"github.com/Blackfire-AI/brigada/internal/api"
)

// argCatcher records the fake adapter's harness command line and stops the
// run there.
type argCatcher struct {
	adapter.Adapter
	args *[]string
	job  *adapter.Job
}

func (c argCatcher) Command(job adapter.Job, workdir, resume string) (*exec.Cmd, error) {
	cmd, err := c.Adapter.Command(job, workdir, resume)
	if err != nil {
		return nil, err
	}
	*c.args, *c.job = cmd.Args, job
	return nil, errors.New("stop")
}

// TestRunWorkerPassesTheGrantsPermission: the grant's resolved permission
// value reaches the harness command line for a worker and for a review
// alike, and the prompt is the grant's own, with no scope paragraph.
func TestRunWorkerPassesTheGrantsPermission(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"work", "fix", "review"} {
		var args []string
		var job adapter.Job
		ad := argCatcher{Adapter: &fake.Adapter{Script: "/bin/true"}, args: &args, job: &job}
		r := &jobRun{cfg: &Config{}, grant: &api.JobGrant{Kind: kind, Prompt: "do it", Permission: "acceptEdits"}}
		_ = r.runWorker(context.Background(), ad, "", t.TempDir(), nil)
		if !slices.Equal(args[1:], []string{"--permission", "acceptEdits"}) {
			t.Errorf("%s: command line = %v, want the grant's permission on it", kind, args)
		}
		if job.Prompt != "do it" {
			t.Errorf("%s: prompt = %q, want the grant's prompt untouched", kind, job.Prompt)
		}
		if kind == "review" && !job.Review {
			t.Errorf("review job not marked a review")
		}
	}
}

func TestRunWorkerWithoutPermissionAddsNothing(t *testing.T) {
	t.Parallel()
	var args []string
	var job adapter.Job
	ad := argCatcher{Adapter: &fake.Adapter{Script: "/bin/true"}, args: &args, job: &job}
	r := &jobRun{cfg: &Config{}, grant: &api.JobGrant{Kind: "work", Prompt: "p"}}
	_ = r.runWorker(context.Background(), ad, "", t.TempDir(), nil)
	if len(args) != 1 {
		t.Errorf("command line = %v, want the script alone", args)
	}
}
