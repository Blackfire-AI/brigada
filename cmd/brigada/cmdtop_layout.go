package main

// topSticky is the full-screen loop's layout memory: the widest each
// section's columns have ever needed, so a row that comes or goes — a
// task settling, a job starting, an agent going off — does not resize
// the section around it. Widths only grow; a change of the box's inner
// width (a terminal resize) resets them. A nil *topSticky is no memory:
// every width is the tick's own, which is how the one-shot snapshot and
// the bare-text helpers render.
type topSticky struct {
	inner  int
	jobs   map[int]topJobWidths // by section: topSecPlans, topSecForeman, topSecJobs
	agents topAgentWidths
	events [5]int
}

// topJobWidths is the column widths of one job-row box, as
// topLayoutJobsW lays them out. wait and event are not columns but
// reservations: the stretch of wait text or last event the title gives
// way to.
type topJobWidths struct {
	mark, id, title  int
	kind, agent, age int
	wait, event      int
}

// topAgentWidths is the column widths of the AGENTS box: the name, the
// tag, the cap cells and the quota text.
type topAgentWidths struct{ name, tag, cells, quota int }

// The caps on how wide the wait and event reservations grow: one
// long-winded row must not eat the title for the rest of the session.
const (
	topStickyWaitMax  = 28
	topStickyEventMax = 32
)

// forInner resets the memory when the box's inner width changed.
func (s *topSticky) forInner(inner int) {
	if s != nil && s.inner != inner {
		*s = topSticky{inner: inner}
	}
}

// growJobs folds a tick's natural widths of one section's job rows into
// the memory and returns the widths to lay out with.
func (s *topSticky) growJobs(section int, w topJobWidths) topJobWidths {
	if s == nil {
		return w
	}
	if s.jobs == nil {
		s.jobs = make(map[int]topJobWidths)
	}
	g := s.jobs[section]
	g.mark = max(g.mark, w.mark)
	g.id = max(g.id, w.id)
	g.title = max(g.title, w.title)
	g.kind = max(g.kind, w.kind)
	g.agent = max(g.agent, w.agent)
	g.age = max(g.age, w.age)
	g.wait = max(g.wait, min(w.wait, topStickyWaitMax))
	g.event = max(g.event, min(w.event, topStickyEventMax))
	s.jobs[section] = g
	return g
}

// growAgents folds a tick's natural AGENTS widths into the memory and
// returns the widths to lay out with.
func (s *topSticky) growAgents(w topAgentWidths) topAgentWidths {
	if s == nil {
		return w
	}
	s.agents.name = max(s.agents.name, w.name)
	s.agents.tag = max(s.agents.tag, w.tag)
	s.agents.cells = max(s.agents.cells, w.cells)
	s.agents.quota = max(s.agents.quota, w.quota)
	return s.agents
}

// growEvents folds a tick's natural EVENTS column widths into the
// memory and returns the widths to lay out with.
func (s *topSticky) growEvents(w [5]int) [5]int {
	if s == nil {
		return w
	}
	for i := range s.events {
		s.events[i] = max(s.events[i], w[i])
	}
	return s.events
}
