package mcp

import (
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// TestAskOutcomeLine: each outcome of an ask reads as one line naming the
// message and the project, and a limited foreman names its reset time
// when the engine knows it.
func TestAskOutcomeLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  engine.AskResult
		want string
	}{
		{"woken", engine.AskResult{EventID: 7, ProjectID: 2, Outcome: engine.WakeWoken},
			"message #7 sent to the foreman of project 2: it is waking up to act on it"},
		{"attached", engine.AskResult{EventID: 7, ProjectID: 2, Outcome: engine.WakeAttached, HoldID: 3},
			"message #7 queued for the foreman of project 2: a chat is attached (hold 3)"},
		{"duplicate", engine.AskResult{EventID: 7, ProjectID: 2, Outcome: engine.WakeDuplicate},
			"message #7 is the same message sent to the foreman of project 2 moments ago"},
		{"no foreman", engine.AskResult{EventID: 7, ProjectID: 2, Outcome: engine.WakeNoForeman},
			"message #7 queued for project 2, which has no foreman yet"},
		{"budget spent", engine.AskResult{EventID: 7, ProjectID: 2, Outcome: engine.WakeBudgetSpent},
			"message #7 queued for the foreman of project 2: today's wake budget is spent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AskOutcomeLine(tc.res); !strings.HasPrefix(got, tc.want) {
				t.Fatalf("AskOutcomeLine = %q, want prefix %q", got, tc.want)
			}
		})
	}
}

// TestAskOutcomeLineNamesTheLimitReset: a limited foreman's line carries
// the reset time when the engine has one, and says it is unknown otherwise.
func TestAskOutcomeLineNamesTheLimitReset(t *testing.T) {
	known := AskOutcomeLine(engine.AskResult{EventID: 1, ProjectID: 4, Outcome: engine.WakeLimited, LimitedUntil: "14:00 UTC"})
	if !strings.Contains(known, "the foreman is limited until 14:00 UTC") {
		t.Fatalf("known reset line = %q", known)
	}
	unknown := AskOutcomeLine(engine.AskResult{EventID: 1, ProjectID: 4, Outcome: engine.WakeLimited})
	if !strings.Contains(unknown, "the foreman is limited, reset time unknown") {
		t.Fatalf("unknown reset line = %q", unknown)
	}
}
