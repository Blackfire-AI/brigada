package main

import (
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/logo"
	"github.com/Blackfire-AI/brigada/internal/term"
)

// TestTopStickyGrowsOnly: the memory keeps each width at the widest seen,
// caps the wait and event reservations, resets on a new inner width, and a
// nil memory is the tick's own widths.
func TestTopStickyGrowsOnly(t *testing.T) {
	var nilSticky *topSticky
	w := topJobWidths{title: 9, wait: 40, event: 40}
	if got := nilSticky.growJobs(topSecPlans, w); got != w {
		t.Errorf("nil growJobs = %+v, want the tick's own", got)
	}
	s := new(topSticky)
	s.forInner(96)
	got := s.growJobs(topSecPlans, w)
	if got.title != 9 || got.wait != topStickyWaitMax || got.event != topStickyEventMax {
		t.Errorf("growJobs = %+v, want title 9, wait and event capped", got)
	}
	if got := s.growJobs(topSecPlans, topJobWidths{title: 4}); got.title != 9 {
		t.Errorf("growJobs shrunk the title to %d, want 9", got.title)
	}
	if got := s.growJobs(topSecForeman, topJobWidths{title: 4}); got.title != 4 {
		t.Errorf("another section inherited the width: %d, want 4", got.title)
	}
	a := s.growAgents(topAgentWidths{name: 12, quota: 11})
	if a.name != 12 || s.growAgents(topAgentWidths{name: 5}).name != 12 {
		t.Errorf("growAgents did not hold the widest name: %+v", s.agents)
	}
	if got := s.growEvents([5]int{5, 4, 3, 2, 1}); got != [5]int{5, 4, 3, 2, 1} ||
		s.growEvents([5]int{1, 1, 1, 1, 9})[4] != 9 {
		t.Errorf("growEvents = %+v, want elementwise max", s.events)
	}
	s.forInner(80)
	if s.jobs != nil || s.agents.name != 0 || s.events[0] != 0 || s.inner != 80 {
		t.Errorf("a new inner width did not reset: %+v", s)
	}
}

// TestTopStickyPlansHoldColumns: a PLANS column sized for a row keeps its
// width after the row goes, where the tick's own widths would let it
// shrink: the queued line of the task that stays does not move.
func TestTopStickyPlansHoldColumns(t *testing.T) {
	plain := term.New(logo.Caps{UTF8: true})
	long := engine.StatusTask{ID: 1, Title: "a very long task title that widens the column", State: "ready"}
	short := engine.StatusTask{ID: 2, Title: "short", State: "ready"}
	settled := engine.StatusTask{ID: 1, Title: long.Title, State: "done", Landed: true}
	status := func(tasks ...engine.StatusTask) api.Status {
		return api.Status{Plans: []engine.StatusPlan{{ID: 1, Title: "p", State: "active", Tasks: tasks}}}
	}
	queuedAt := func(lines []string) int {
		for _, l := range lines {
			if strings.Contains(l, "#2") {
				return strings.Index(l, "(queued)")
			}
		}
		t.Fatalf("no line for task #2:\n%s", strings.Join(lines, "\n"))
		return -1
	}
	sticky := new(topSticky)
	first := renderTop(topModel{status: status(long, short), statusOK: true, layout: sticky}, 80, 0, topNow, plain)
	after := renderTop(topModel{status: status(settled, short), statusOK: true, layout: sticky}, 80, 0, topNow, plain)
	if queuedAt(first) != queuedAt(after) {
		t.Errorf("the queued line moved: column %d, then %d\n%s\n%s", queuedAt(first), queuedAt(after),
			strings.Join(first, "\n"), strings.Join(after, "\n"))
	}
	bare := renderTop(topModel{status: status(settled, short), statusOK: true}, 80, 0, topNow, plain)
	if queuedAt(bare) == queuedAt(first) {
		t.Errorf("the test is empty: without the memory the column did not shrink either")
	}
}
