package api

import (
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// TestStatusLine pins the one-line status a terminal status bar shows
// (§10): idle when nothing runs or waits; the first running job in a
// few words, the rest counted; then the queue and what needs the user.
func TestStatusLine(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	leased := now.Add(-100 * time.Second).Format(engine.TimeLayout)
	work := engine.StatusJob{ID: 5, TaskID: 3, Kind: "work", Status: "running",
		Harness: "claude", Model: "sonnet", LeasedAt: leased, LastEvent: "tool_start Edit"}
	review := engine.StatusJob{ID: 6, TaskID: 4, Kind: "review", Status: "leased", Harness: "kimi"}
	cases := []struct {
		name string
		st   Status
		want string
	}{
		{"idle", Status{Jobs: map[string]int{}}, "brigada: idle"},
		{"idle after plans finished", Status{Plans: []engine.StatusPlan{
			{ID: 1, State: "done"}, {ID: 3, State: "done"}, {ID: 2, State: "done"},
		}}, "brigada: idle · plan #3 done"},
		{"an active plan's job hides the finished one", Status{
			Running: []engine.StatusJob{{ID: 9, Kind: "foreman"}},
			Plans:   []engine.StatusPlan{{ID: 1, State: "done"}},
		}, "brigada: job #9 foreman"},
		{"one job", Status{Running: []engine.StatusJob{work}},
			"brigada: task #3 work · claude/sonnet · 1m40s · Edit"},
		{"a non-tool event shows as it is", Status{Running: []engine.StatusJob{
			{ID: 7, TaskID: 5, Kind: "work", Status: "running", LastEvent: "text thinking"}}},
			"brigada: task #5 work · text thinking"},
		{"two jobs, a queue and a parked task", Status{
			Running:  []engine.StatusJob{work, review},
			Jobs:     map[string]int{"queued": 2},
			NeedsYou: []engine.NeedsYouItem{{Kind: "task", ID: 7}},
		}, "brigada: task #3 work · claude/sonnet · 1m40s · Edit (+1 more) · 2 queued · needs you: 1"},
		{"a foreman turn", Status{Running: []engine.StatusJob{{ID: 9, Kind: "foreman", Status: "running"}}},
			"brigada: job #9 foreman"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StatusLine(tc.st, now); got != tc.want {
				t.Errorf("StatusLine = %q\nwant         %q", got, tc.want)
			}
		})
	}
}

// TestStatusSectionsRunning: the full status lists every running job
// under `running:`, before the plans, and says none when no runner
// holds one.
func TestStatusSectionsRunning(t *testing.T) {
	if got := StatusSections(Status{}); !strings.HasPrefix(got, "running: none\nplans: none") {
		t.Errorf("empty status sections begin %q, want running: none then plans: none", got)
	}
	st := Status{Running: []engine.StatusJob{{ID: 6, TaskID: 4, Kind: "review", Harness: "kimi"}}}
	if got := StatusSections(st); !strings.HasPrefix(got, "running:\n  task #4 review · kimi\nplans: none") {
		t.Errorf("status sections begin %q, want the running job's line", got)
	}
}

// TestStatusSectionsNamesAMissingForemanPair: a foreman turn held because
// no runner offers its harness/model says so, naming the pair.
func TestStatusSectionsNamesAMissingForemanPair(t *testing.T) {
	st := Status{Held: []engine.HeldJob{{ID: 9, Kind: "foreman", Layer: "no_pair", Agent: "claude/opus"}}}
	want := "held: job #9 foreman: foreman turn waiting: no runner offers claude/opus"
	if got := StatusSections(st); !strings.Contains(got, want) {
		t.Errorf("status sections = %q, want the line %q", got, want)
	}
}

// TestStatusLineCountsMessagesForTheForeman: a waiting user message is
// no idle state (§4.2 item 3): the bar counts it.
func TestStatusLineCountsMessagesForTheForeman(t *testing.T) {
	if got := StatusLine(Status{ForemanMessages: 1}, time.Now()); got != "brigada: 1 message for the foreman" {
		t.Errorf("StatusLine = %q", got)
	}
	if got := StatusLine(Status{ForemanMessages: 2}, time.Now()); got != "brigada: 2 messages for the foreman" {
		t.Errorf("StatusLine = %q", got)
	}
}
