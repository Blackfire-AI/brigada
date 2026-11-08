package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/term"
)

// topJobRow is one job-shaped row of a box, held as cells until its box's
// rows are all known: topLayoutJobs gives every cell the column the widest
// row of the box needs, so the rows line up. A running row fills the kind,
// agent, age and event cells; a row without a running job puts its wait text
// in the kind column, running to the line's end.
type topJobRow struct {
	mark, id, title         string
	kind, agent, age, event string
	wait                    string
	markFn, textFn, waitFn  func(string) string // colour of the mark, of the id and title, of the wait text; nil is plain
	agentFn                 func(string) string // colour of the agent cell: its family's (topFamily)
	running                 bool                // a running job's row: tinted
	dim                     bool                // the whole row is Dim, painted once
}

// topJobEventMin is the inner width under which the last event is left out:
// the terminal's 80 columns less the box's four.
const topJobEventMin = 76

// topJobTitleMin is the title's floor: the event is cut before the title
// goes under it.
const topJobTitleMin = 8

// topJobGap separates two cells of a row.
const topJobGap = "  "

// topShortAgent is a running agent's name in its short form: a name of more
// than two `/` segments keeps its first and last, so `opencode/zai-coding-plan/glm-5.3`
// reads `opencode/glm-5.3`.
func topShortAgent(name string) string {
	segs := strings.Split(name, "/")
	if len(segs) > 2 {
		return segs[0] + "/" + segs[len(segs)-1]
	}
	return name
}

// topJobAgent is a job's agent cell as api.DescribeJob words it,
// `harness/model@effort`, with the name in its short form.
func topJobAgent(j engine.StatusJob) string {
	if j.Harness == "" {
		return ""
	}
	agent := j.Harness
	if j.Model != "" {
		agent = topShortAgent(j.Harness + "/" + j.Model)
		if j.Effort != "" {
			agent += "@" + j.Effort
		}
	}
	return agent
}

// topJobAge is how long a job has run: `45s`, `9m04s`, `1h02m03s`, the
// seconds and minutes padded to two digits once a larger unit leads. It is
// empty while the lease time is unknown or in the future.
func topJobAge(leasedAt string, now time.Time) string {
	t, err := time.Parse(engine.TimeLayout, leasedAt)
	if err != nil || now.Before(t) {
		return ""
	}
	secs := int(now.Sub(t) / time.Second)
	h, m, s := secs/3600, secs/60%60, secs%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// topRunningRow is a running job's row: the Accent mark (the pointer, or the
// frame's spinner glyph, one cell either way), the task id and
// title (a job with no task has an empty id and `job #N` as the title), the
// kind, the agent, the age and the last event with `tool_start ` trimmed. title is the
// task's title, "" when the plan that names it is not listed.
func topRunningRow(j engine.StatusJob, title string, now time.Time, style term.Style, mark string) *topJobRow {
	row := &topJobRow{
		mark: mark, markFn: style.Accent, running: true,
		title: title, kind: j.Kind, agent: topJobAgent(j), age: topJobAge(j.LeasedAt, now),
		event: strings.TrimPrefix(j.LastEvent, "tool_start "),
	}
	row.agentFn = topFamily(style, row.agent)
	if j.TaskID != 0 {
		row.id = fmt.Sprintf("#%d", j.TaskID)
	} else {
		row.title = fmt.Sprintf("job #%d", j.ID)
	}
	return row
}

// topWaitRow is a row without a running job: its mark and its wait text are
// coloured with fn, and its id and title are not.
func topWaitRow(mark, taskID, title, wait string, fn func(string) string) *topJobRow {
	return &topJobRow{mark: mark, id: taskID, title: title, wait: wait, markFn: fn, waitFn: fn}
}

// topDimRow is a queued row: Dim from the mark to the end, id and title
// included.
func topDimRow(mark, taskID, title, wait string) *topJobRow {
	return &topJobRow{mark: mark, id: taskID, title: title, wait: wait, dim: true}
}

// topJobID is a task id cell, or "" for a job with no task.
func topJobID(taskID int64) string {
	if taskID == 0 {
		return ""
	}
	return fmt.Sprintf("#%d", taskID)
}

// topLayoutJobs gives the job rows among lines the columns of their box and
// builds their text at the inner width. The mark, id and title columns are
// as wide as the widest cell; the kind, agent and age columns, which only a
// running row fills, likewise. The title gives way first, down to
// topJobTitleMin, and the last event is left out below topJobEventMin
// columns; an event still too long is cut at the line's end. Every running
// row is marked for the blue tint.
func topLayoutJobs(lines []topLine, style term.Style, inner int) {
	var rows []*topJobRow
	for _, l := range lines {
		if l.job != nil {
			rows = append(rows, l.job)
		}
	}
	if len(rows) == 0 {
		return
	}
	count := term.CellWidth
	var markW, idW, titleW, kindW, agentW, ageW, eventW, waitW int
	for _, r := range rows {
		markW, idW = max(markW, count(r.mark)), max(idW, count(r.id))
		titleW = max(titleW, count(r.title))
		kindW, agentW, ageW = max(kindW, count(r.kind)), max(agentW, count(r.agent)), max(ageW, count(r.age))
		eventW, waitW = max(eventW, count(r.event)), max(waitW, count(r.wait))
	}
	// What the left cells take, and what the running columns reserve after
	// the title: the kind, agent and age with their gaps, or a stretch of the
	// wait text when that is wider.
	prefix := 2 + markW + 1
	if idW > 0 {
		prefix += idW
		if titleW > 0 {
			prefix += len(topJobGap)
		}
	}
	reserved := 0
	for _, w := range []int{kindW, agentW, ageW} {
		if w > 0 {
			reserved += len(topJobGap) + w
		}
	}
	if waitW > 0 {
		reserved = max(reserved, len(topJobGap)+min(waitW, 28))
	}
	showEvent := inner >= topJobEventMin && eventW > 0
	title := titleW
	if titleW > 0 {
		room := inner - prefix - reserved
		if showEvent {
			room -= len(topJobGap) + eventW
		}
		switch {
		case room >= min(titleW, topJobTitleMin):
			title = min(titleW, room)
		default:
			// The title is at its floor: what the event cannot have of the line
			// it loses at the end.
			title = min(titleW, topJobTitleMin)
		}
	}
	eventRoom := 0
	if showEvent {
		eventRoom = min(eventW, inner-prefix-title-reserved-len(topJobGap))
		if eventRoom < 4 && eventRoom < eventW {
			eventRoom = 0
		}
	}
	paint := func(fn func(string) string, text string) string {
		if fn == nil || text == "" {
			return text
		}
		return fn(text)
	}
	pad := func(text string, w int) string { return text + strings.Repeat(" ", max(w-count(text), 0)) }
	for i := range lines {
		r := lines[i].job
		if r == nil {
			continue
		}
		b := "  " + paint(r.markFn, pad(r.mark, markW)) + " "
		if idW > 0 {
			b += paint(r.textFn, pad(r.id, idW))
			if titleW > 0 {
				b += topJobGap
			}
		}
		if titleW > 0 {
			b += paint(r.textFn, pad(cutLine(r.title, title), title))
		}
		if r.wait != "" {
			room := inner - (prefix + title + len(topJobGap))
			if titleW == 0 {
				room = inner - (prefix + len(topJobGap))
			}
			b += topJobGap + paint(r.waitFn, cutLine(r.wait, max(room, 0)))
		} else {
			for _, cell := range []struct {
				text string
				w    int
				fn   func(string) string
			}{{r.kind, kindW, nil}, {r.agent, agentW, r.agentFn}, {r.age, ageW, nil}} {
				if cell.w > 0 {
					b += topJobGap + paint(cell.fn, cell.text) + strings.Repeat(" ", max(cell.w-count(cell.text), 0))
				}
			}
			if eventRoom > 0 && r.event != "" {
				b += topJobGap + cutLine(r.event, eventRoom)
			}
		}
		if r.dim {
			b = "  " + style.Dim(strings.TrimPrefix(b, "  "))
		}
		lines[i].text = b
		if r.running {
			lines[i].tint = topTintRun
		}
	}
}
