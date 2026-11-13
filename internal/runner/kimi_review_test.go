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

// TestKimiReviewPolicyReachesWorker: a review job no longer picks the
// runner's read-only agent file over the configured one — kimi has no
// permission mechanism to vary by job kind (§5.2; task 773's note), so a
// review, a work and a fix job build the identical command.
func TestKimiReviewPolicyReachesWorker(t *testing.T) {
	var commands []*exec.Cmd
	for _, kind := range []engine.Kind{engine.KindReview, engine.KindWork, engine.KindFix} {
		t.Run(string(kind), func(t *testing.T) {
			ad := &kimiCommandRecorder{Adapter: kimi.Adapter{Binary: "/unused/kimi", ReadOnlyAgentFile: "/runner/kimi-readonly.md"}}
			r := &jobRun{cfg: &Config{}, grant: &api.JobGrant{Kind: string(kind), Prompt: "p"}}
			if err := r.runWorker(context.Background(), ad, "default", t.TempDir(), nil); err == nil {
				t.Fatal("capture did not stop the worker")
			}
			if ad.cmd == nil {
				t.Fatal("command was not built")
			}
			for i, arg := range ad.cmd.Args {
				if arg == "--agent-file" && i+1 < len(ad.cmd.Args) && ad.cmd.Args[i+1] == ad.ReadOnlyAgentFile {
					t.Fatalf("%s command = %v, carries the read-only agent file it no longer should", kind, ad.cmd.Args)
				}
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
