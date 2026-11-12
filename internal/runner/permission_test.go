package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/adapter/fake"
	"github.com/Blackfire-AI/brigada/internal/api"
)

// runRecordingArgs runs one job through runWorker with a fake worker that
// records its command-line arguments, one per line, and returns them.
func runRecordingArgs(t *testing.T, cfg *Config, grant *api.JobGrant) []string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "worker.sh")
	body := "#!/bin/sh\ncat > /dev/null\nprintf '%s\\n' \"$@\" > \"" + filepath.Join(dir, "args") + "\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg.Clock.fill()
	r := &jobRun{cfg: cfg, grant: grant}
	_ = r.runWorker(context.Background(), &fake.Adapter{Script: script}, "m", dir, nil)
	got, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(got))
}

// The grant's permission value reaches the harness command line of a work
// job and of a review alike: a review has no read-only default of
// brigada's.
func TestGrantPermissionReachesTheCommandLine(t *testing.T) {
	for _, kind := range []string{"work", "fix", "review"} {
		t.Run(kind, func(t *testing.T) {
			got := runRecordingArgs(t, &Config{}, &api.JobGrant{Kind: kind, Agent: "fake", Prompt: "p", Permission: "bypassPermissions"})
			if want := []string{"--permission", "bypassPermissions"}; !reflect.DeepEqual(got, want) {
				t.Errorf("%s command line = %v, want %v", kind, got, want)
			}
		})
	}
}

func TestNoPermissionAddsNothingToTheCommandLine(t *testing.T) {
	if got := runRecordingArgs(t, &Config{}, &api.JobGrant{Kind: "work", Agent: "fake", Prompt: "p"}); len(got) != 0 {
		t.Errorf("command line = %v, want none", got)
	}
}

// An engine whose grant carries no permission leaves the named agent's own
// advertised value in force.
func TestPermissionFallsBackToTheNamedAgent(t *testing.T) {
	cfg := &Config{Agents: []api.Agent{{Adapter: "fake", Harness: "fake", Model: "m", Permission: "plan"}}}
	grant := &api.JobGrant{Kind: "review", Agent: "fake", Prompt: "p"}
	if got := cfg.permissionFor(grant); got != "plan" {
		t.Errorf("permissionFor = %q, want plan", got)
	}
	grant.Permission = "bypassPermissions"
	if got := cfg.permissionFor(grant); got != "bypassPermissions" {
		t.Errorf("permissionFor with a grant value = %q, want it", got)
	}
}

// The runner no longer appends a shell-scope paragraph to the prompt.
func TestPromptCarriesNoShellScope(t *testing.T) {
	var got string
	ad := promptCatcher{Adapter: &fake.Adapter{}, prompt: &got}
	r := &jobRun{cfg: &Config{}, grant: &api.JobGrant{Kind: "work", Prompt: "do it"}}
	_ = r.runWorker(context.Background(), ad, "", t.TempDir(), nil)
	if got != "do it" {
		t.Errorf("prompt = %q, want the grant's alone", got)
	}
}

// promptCatcher records the prompt the runner hands the adapter and stops
// the run there.
type promptCatcher struct {
	adapter.Adapter
	prompt *string
}

func (p promptCatcher) Command(job adapter.Job, _, _ string) (*exec.Cmd, error) {
	*p.prompt = job.Prompt
	return nil, errors.New("stop")
}
