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

func TestKimiReviewPolicyReachesWorker(t *testing.T) {
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
			want := kind == engine.KindReview
			found := false
			for i, arg := range ad.cmd.Args {
				if arg == "--agent-file" && i+1 < len(ad.cmd.Args) && ad.cmd.Args[i+1] == ad.ReadOnlyAgentFile {
					found = true
				}
			}
			if found != want {
				t.Fatalf("%s command = %v, want read-only agent %t", kind, ad.cmd.Args, want)
			}
		})
	}
}
