package runner

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/adapter/kimi"
	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// Capture the command at the process boundary without running a model.
type kimiCommandRecorder struct {
	kimi.Adapter
	cmd *exec.Cmd
}

func (a *kimiCommandRecorder) Command(job adapter.Job, workdir, resume string) (*exec.Cmd, error) {
	cmd, err := a.Adapter.Command(job, workdir, resume)
	a.cmd = cmd
	if err != nil {
		return nil, err
	}
	return nil, errors.New("command captured")
}

// TestKimiReviewBuildsTheWorkCommand: kimi has no permission mechanism to
// vary by job kind (§5.2), so a review, a work and a fix job build the
// identical command.
func TestKimiReviewBuildsTheWorkCommand(t *testing.T) {
	var commands []*exec.Cmd
	for _, kind := range []engine.Kind{engine.KindReview, engine.KindWork, engine.KindFix} {
		t.Run(string(kind), func(t *testing.T) {
			ad := &kimiCommandRecorder{Adapter: kimi.Adapter{Binary: "/unused/kimi"}}
			r := &jobRun{cfg: &Config{}, grant: &api.JobGrant{Kind: string(kind), Prompt: "p"}}
			if err := r.runWorker(context.Background(), ad, "default", t.TempDir(), nil); err == nil {
				t.Fatal("capture did not stop the worker")
			}
			if ad.cmd == nil {
				t.Fatal("command was not built")
			}
			commands = append(commands, ad.cmd)
		})
	}
	for i := 1; i < len(commands); i++ {
		if len(commands[i].Args) != len(commands[0].Args) {
			t.Fatalf("command %d args = %v, want the same shape as %v", i, commands[i].Args, commands[0].Args)
		}
		for j, arg := range commands[0].Args {
			if commands[i].Args[j] != arg {
				t.Fatalf("command %d args = %v, want the same as %v", i, commands[i].Args, commands[0].Args)
			}
		}
	}
}
