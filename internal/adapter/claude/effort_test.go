package claude

import (
	"slices"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// flagValue returns the argument after flag, and whether the flag is there.
func flagValue(args []string, flag string) (string, bool) {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return "", false
	}
	return args[i+1], true
}

func TestEffortRidesFreshResumeAndAttach(t *testing.T) {
	stubCLI(t)
	dir := t.TempDir()
	ad := &Adapter{}
	for _, level := range []string{"low", "medium", "high", "xhigh", "max"} {
		job := adapter.Job{Prompt: "p", Model: "opus", Effort: level}
		for name, resume := range map[string]string{"fresh": "", "resume": "sess-1"} {
			cmd, err := ad.Command(job, dir, resume)
			if err != nil {
				t.Fatalf("%s %s: %v", name, level, err)
			}
			if got, ok := flagValue(cmd.Args, "--effort"); !ok || got != level {
				t.Errorf("%s %s: --effort = %q (present %v), want %q: %v", name, level, got, ok, level, cmd.Args)
			}
		}
		job.MCPConfig = "/tmp/mcp.json"
		cmd, err := ad.AttachCommand(job, dir, "sess-1", "new-1")
		if err != nil {
			t.Fatalf("attach %s: %v", level, err)
		}
		if got, ok := flagValue(cmd.Args, "--effort"); !ok || got != level {
			t.Errorf("attach %s: --effort = %q (present %v), want %q: %v", level, got, ok, level, cmd.Args)
		}
	}
}

func TestNoEffortAddsNoFlag(t *testing.T) {
	stubCLI(t)
	dir := t.TempDir()
	cmd, err := (&Adapter{}).Command(adapter.Job{Prompt: "p", Model: "opus"}, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(cmd.Args, "--effort") {
		t.Errorf("an unset effort added --effort: %v", cmd.Args)
	}
}

func TestUnsupportedEffortIsRefused(t *testing.T) {
	stubCLI(t)
	dir := t.TempDir()
	job := adapter.Job{Prompt: "p", Effort: "turbo", MCPConfig: "/tmp/mcp.json"}
	if _, err := (&Adapter{}).Command(job, dir, ""); err == nil || !strings.Contains(err.Error(), "turbo") {
		t.Errorf("Command error = %v, want a refusal naming the level", err)
	}
	if _, err := (&Adapter{}).AttachCommand(job, dir, "", "new-1"); err == nil || !strings.Contains(err.Error(), "turbo") {
		t.Errorf("AttachCommand error = %v, want a refusal naming the level", err)
	}
}
