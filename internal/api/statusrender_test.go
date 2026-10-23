package api

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

// TestStatusSectionsRunnerDraining: a pending restart drain (§5.1) is
// one line after the running jobs it waits out — plural and singular —
// and nothing is said when nothing runs or no runner drains.
func TestStatusSectionsRunnerDraining(t *testing.T) {
	st := Status{
		Running: []engine.StatusJob{
			{ID: 6, TaskID: 4, Kind: "review", Harness: "kimi"},
			{ID: 7, TaskID: 5, Kind: "work", Harness: "claude"},
		},
		RunnerDraining: true,
	}
	if got := StatusSections(st); !strings.Contains(got, "runner restarting after 2 running jobs\n") {
		t.Errorf("status sections = %q, want the drain's line after the running jobs", got)
	}
	st.Running = st.Running[:1]
	if got := StatusSections(st); !strings.Contains(got, "runner restarting after 1 running job\n") {
		t.Errorf("status sections = %q, want the singular drain line", got)
	}
	st.Running = nil
	if got := StatusSections(st); strings.Contains(got, "runner restarting") {
		t.Errorf("status sections = %q, want no drain line once nothing runs", got)
	}
	st.RunnerDraining = false
	st.Running = []engine.StatusJob{{ID: 6, TaskID: 4, Kind: "review", Harness: "kimi"}}
	if got := StatusSections(st); strings.Contains(got, "runner restarting") {
		t.Errorf("status sections = %q, want no drain line while no runner drains", got)
	}
}

// TestStatusSectionsShowGateHolds: a task held at the land gate or
// waiting at the push gate is a needs-you line naming its task id and
// the rule that tripped.
func TestStatusSectionsShowGateHolds(t *testing.T) {
	st := Status{NeedsYou: []engine.NeedsYouItem{
		{Kind: "task", ID: 8, State: "done", Reason: "held by rule gate_land: gate_land is ask; release: brigada land approve 8; change the rule: brigada land approve --and-stop-asking 8"},
		{Kind: "task", ID: 9, State: "done", Reason: "waits to push: task/9-1@abc123"},
	}}
	want := "needs you:\ntask #8 done: held by rule gate_land: gate_land is ask; release: brigada land approve 8; change the rule: brigada land approve --and-stop-asking 8\ntask #9 done: waits to push: task/9-1@abc123"
	if got := StatusSections(st); !strings.HasSuffix(got, want) {
		t.Errorf("status sections end %q, want the needs-you block %q", got, want)
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

// TestStatusSectionsShowsAnInfraRetry: a job waiting out an infra
// backoff reads as the retry with its wait and the failure's words.
func TestStatusSectionsShowsAnInfraRetry(t *testing.T) {
	nb := time.Now().Add(5 * time.Minute).UTC().Format(engine.TimeLayout)
	st := Status{Retries: []engine.InfraRetry{{ID: 9, TaskID: 4, Kind: "work", NotBefore: nb, Reason: "could not resolve github.com"}}}
	want := "waiting: task #4 work: retry in 5m: could not resolve github.com"
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

func TestStatusSectionsTaskTierEffort(t *testing.T) {
	st := Status{Plans: []engine.StatusPlan{{ID: 1, Tasks: []engine.StatusTask{
		{ID: 1, Title: "plain", State: "ready", Tier: "standard", EffectiveTier: "standard"},
		{ID: 2, Title: "build", State: "ready", Tier: "heavy", EffectiveTier: "heavy", Effort: "high"},
		{ID: 3, Title: "bumped", State: "ready", Tier: "standard", EffectiveTier: "heavy"},
		{ID: 4, Title: "think", State: "ready", Tier: "standard", EffectiveTier: "standard", Effort: "max", EffectiveEffort: "max"},
		{ID: 5, Title: "stepped", State: "ready", Tier: "standard", EffectiveTier: "standard", Effort: "low", EffectiveEffort: "medium"},
		{ID: 6, Title: "default", State: "ready", Tier: "standard", EffectiveTier: "standard", EffectiveEffort: "high"},
		{ID: 7, Title: "parked", State: "needs_foreman", Tier: "heavy", EffectiveTier: "heavy", Caps: map[string]int{"restart": 4}},
	}}}}
	got := StatusSections(st)
	for _, want := range []string{"  task #7 needs_foreman parked  heavy (restarts 4)\n", "  task #1 ready plain\n", "  task #2 ready build  heavy @high\n", "  task #3 ready bumped  standard->heavy\n", "  task #4 ready think  @max\n",
		"  task #5 ready stepped  @low->medium\n", "  task #6 ready default  @default->high\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("status lacks %q:\n%s", want, got)
		}
	}
}

// TestStatusSectionsCountsHiddenFinishedPlans: the trimmed plans list
// says in one line, after the plans, how many older finished plans it
// left out and what lists them (§10); nothing is said when none hide.
func TestStatusSectionsCountsHiddenFinishedPlans(t *testing.T) {
	st := Status{Plans: []engine.StatusPlan{{ID: 3, State: "done"}}, HiddenFinishedPlans: 5}
	want := "#3 done \n5 older finished plans hidden; pass all=true\n"
	if got := StatusSections(st); !strings.Contains(got, want) {
		t.Errorf("status sections = %q, want the hidden count after the last plan:\n%s", got, want)
	}
	if got := StatusSections(Status{Plans: []engine.StatusPlan{{ID: 3, State: "done"}}}); strings.Contains(got, "hidden") {
		t.Errorf("status sections = %q, want no hidden line when nothing hides", got)
	}
}

// TestStatusSectionsPlanReviewFindings: a parked or reviewing plan names
// its round, and its review line cuts long findings at render time,
// ending with the pointer to the review job's history (§6.2, §9); short
// findings show whole, with no pointer.
func TestStatusSectionsPlanReviewFindings(t *testing.T) {
	long := strings.Repeat("the findings go on and on. ", 40) // well past the line's 200-byte bound
	st := Status{Plans: []engine.StatusPlan{
		{ID: 7, Title: "parked", State: "needs_foreman", ReviewRound: 1, ReviewVerdict: "changes", ReviewFindings: long, ReviewJob: 12},
		{ID: 8, Title: "short", State: "reviewing", ReviewRound: 2, ReviewVerdict: "changes", ReviewFindings: "split the storage layer out"},
		{ID: 9, Title: "amended", State: "reviewing", ReviewRound: 1, WaitingAmendment: true},
	}}
	got := StatusSections(st)
	for _, want := range []string{
		"#7 needs_foreman (round 1) parked",
		"#8 reviewing (round 2) short",
		"#9 reviewing (round 1) amended\n  amendment waits for the review round\n",
		"… (full: brigada history --job 12)",
		"findings: split the storage layer out\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status sections lack %q:\n%s", want, got)
		}
	}
	cut := strings.Index(got, "findings: the findings go on")
	pointer := strings.Index(got, "… (full: brigada history --job 12)")
	if cut < 0 || pointer < cut || pointer-cut > 400 {
		t.Errorf("the long findings are not cut just before the pointer (cut %d, pointer %d):\n%s", cut, pointer, got)
	}
	if strings.Count(got, "(full: brigada history") != 1 {
		t.Errorf("short findings grew a pointer:\n%s", got)
	}
}

// TestOneLineCutsOnARune: a cut that falls inside a multi-byte character,
// on its lead byte or after it, backs off to the character's start, so
// the line stays valid UTF-8.
func TestOneLineCutsOnARune(t *testing.T) {
	for _, tc := range []struct {
		s    string
		max  int
		want string
	}{
		{"abécd", 3, "ab"},  // é is 2 bytes: the cut falls after its lead byte
		{"ab€cd", 3, "ab"},  // € is 3 bytes: the cut falls after its lead byte
		{"ab€cd", 4, "ab"},  // inside €, after a continuation byte
		{"ab€cd", 5, "ab€"}, // right after €
		{"a  b\n c", 10, "a b c"},
	} {
		got := oneLine(tc.s, tc.max)
		if got != tc.want || !utf8.ValidString(got) {
			t.Errorf("oneLine(%q, %d) = %q, want %q", tc.s, tc.max, got, tc.want)
		}
	}
}
