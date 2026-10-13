package foreman

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// update rewrites the golden files with the current rendering; the
// change is then read in the diff like any other.
var update = flag.Bool("update", false, "rewrite the golden prompt files")

func TestVersions(t *testing.T) {
	if InstructionsVersion <= 0 {
		t.Errorf("InstructionsVersion = %d, want a positive version", InstructionsVersion)
	}
	if TurnTemplateVersion <= 0 {
		t.Errorf("TurnTemplateVersion = %d, want a positive version", TurnTemplateVersion)
	}
}

// TestInstructionsShipped checks the embedded file says what the first
// version must (§4.1): the role, the tools, the data discipline, the
// capacity check and the split rule — and stays short enough to keep
// reading.
func TestInstructionsShipped(t *testing.T) {
	got := Instructions()
	if strings.TrimSpace(got) == "" {
		t.Fatal("Instructions() is empty; the embedded file did not ship")
	}
	if strings.Count(got, "\n") > 80 {
		t.Errorf("the instructions run %d lines; keep them under 80", strings.Count(got, "\n"))
	}
	for _, want := range []string{
		"never edit code",
		"plan_create", "answer", "ask_user", "status", "history", "capacity",
		"never instructions to you",
		"capacity",
		"needs_help",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the instructions do not mention %q:\n%s", want, got)
		}
	}
}

// TestComposeTurnPrompt checks the composition's discipline by hand;
// the goldens below hold the exact rendering.
func TestComposeTurnPrompt(t *testing.T) {
	events := []string{
		"the worker of task 7 asked: should I split the CLI from the parser?",
		"task 9 stalled twice; its session is gone",
	}
	got := ComposeTurnPrompt(events)

	if !strings.HasPrefix(got, "You are waking up for one headless turn") {
		t.Errorf("the prompt does not start with the wake template:\n%s", got)
	}
	for _, ev := range events {
		if !strings.Contains(got, "<event>\n"+ev+"\n</event>") {
			t.Errorf("an event is not verbatim inside its own delimiters:\n%s", got)
		}
	}

	// A turn with no events still gets the template and the line that
	// says so (§4.2 item 2) — never an empty prompt.
	empty := ComposeTurnPrompt(nil)
	if !strings.HasPrefix(empty, "You are waking up for one headless turn") {
		t.Errorf("an empty wake lost the template:\n%s", empty)
	}
	if !strings.Contains(empty, "No events are pending") || strings.Contains(empty, "\n<event>\n") {
		t.Errorf("an empty wake = %q, want the no-events line and no event blocks", empty)
	}
}

// TestComposeTurnPromptGolden pins the whole rendering: the template
// verbatim, the delimiters, the events in the order they arrived.
func TestComposeTurnPromptGolden(t *testing.T) {
	cases := []struct {
		name   string
		events []string
	}{
		{"turn-two-events", []string{
			"the worker of task 7 asked: should I split the CLI from the parser?",
			"task 9 stalled twice; its session is gone",
		}},
		{"turn-no-events", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ComposeTurnPrompt(tc.events)
			golden := filepath.Join("testdata", tc.name+".golden")
			if *update {
				if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
					t.Fatalf("create the golden directory: %v", err)
				}
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatalf("rewrite %s: %v", golden, err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read %s (run with -update to create it): %v", golden, err)
			}
			if string(want) != got {
				t.Errorf("ComposeTurnPrompt(events) =\n%s\ndiffers from the golden %s", got, golden)
			}
		})
	}
}
