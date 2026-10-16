package opencode

import (
	"slices"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// opencode run --variant is the CLI's provider-specific reasoning effort;
// brigada maps the three variants the help names.
func TestEffortMapsToVariant(t *testing.T) {
	stubCLI(t)
	for _, level := range []string{"minimal", "high", "max"} {
		for name, resume := range map[string]string{"fresh": "", "resume": "ses_1"} {
			cmd, err := (&Adapter{}).Command(adapter.Job{Prompt: "p", Model: "anthropic/claude", Effort: level}, t.TempDir(), resume)
			if err != nil {
				t.Fatalf("%s %s: %v", name, level, err)
			}
			i := slices.Index(cmd.Args, "--variant")
			if i < 0 || i+1 >= len(cmd.Args) || cmd.Args[i+1] != level {
				t.Errorf("%s %s: args %q lack --variant %s", name, level, cmd.Args, level)
			}
			if slices.Contains(cmd.Args, "--auto") {
				t.Errorf("%s %s: a permission flag rode in: %q", name, level, cmd.Args)
			}
		}
	}
}

func TestNoEffortAddsNoVariant(t *testing.T) {
	stubCLI(t)
	cmd, err := (&Adapter{}).Command(adapter.Job{Prompt: "p"}, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(cmd.Args, "--variant") {
		t.Errorf("an unset effort added --variant: %q", cmd.Args)
	}
}

func TestUnsupportedEffortIsRefused(t *testing.T) {
	stubCLI(t)
	for _, level := range []string{"low", "medium", "xhigh", "turbo"} {
		_, err := (&Adapter{}).Command(adapter.Job{Prompt: "p", Effort: level}, t.TempDir(), "")
		if err == nil || !strings.Contains(err.Error(), level) {
			t.Errorf("level %q: error = %v, want a refusal naming it", level, err)
		}
	}
}
