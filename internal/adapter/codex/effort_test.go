package codex

import (
	"slices"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

func TestEffortRidesAsTheFixedConfigKey(t *testing.T) {
	stubCLI(t)
	dir := t.TempDir()
	want := []string{"-c", `model_reasoning_effort="high"`}
	job := adapter.Job{Prompt: "p", Model: "gpt-5", Effort: "high"}

	fresh, err := (&Adapter{}).Command(job, dir, "")
	if err != nil {
		t.Fatalf("fresh: %v", err)
	}
	resume, err := (&Adapter{}).Command(job, dir, "thread-1")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	attach, err := (&Adapter{}).AttachCommand(job, dir, "thread-1", "")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	for name, args := range map[string][]string{"fresh": fresh.Args, "resume": resume.Args, "attach": attach.Args} {
		i := slices.Index(args, want[1])
		if i < 1 || args[i-1] != "-c" {
			t.Errorf("%s: args %q lack %q", name, args, want)
		}
	}
	// Flags precede the resume subcommand and the prompt, as -m does.
	if i, r := slices.Index(resume.Args, want[1]), slices.Index(resume.Args, "resume"); i > r {
		t.Errorf("resume: the effort override comes after the resume subcommand: %q", resume.Args)
	}
}

func TestNoEffortAddsNoOverride(t *testing.T) {
	stubCLI(t)
	cmd, err := (&Adapter{}).Command(adapter.Job{Prompt: "p"}, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(cmd.Args, " "), "model_reasoning_effort") {
		t.Errorf("an unset effort added an override: %q", cmd.Args)
	}
}

func TestEffortAcceptedSetAndRefusal(t *testing.T) {
	stubCLI(t)
	for _, level := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"} {
		if _, err := (&Adapter{}).Command(adapter.Job{Prompt: "p", Effort: level}, t.TempDir(), ""); err != nil {
			t.Errorf("level %q refused: %v", level, err)
		}
	}
	for _, level := range []string{"turbo", `high"`, "high\nx", "HIGH"} {
		if _, err := (&Adapter{}).Command(adapter.Job{Prompt: "p", Effort: level}, t.TempDir(), ""); err == nil {
			t.Errorf("level %q accepted", level)
		}
		if _, err := (&Adapter{}).AttachCommand(adapter.Job{Prompt: "p", Effort: level}, t.TempDir(), "", ""); err == nil {
			t.Errorf("attach accepted level %q", level)
		}
	}
}
