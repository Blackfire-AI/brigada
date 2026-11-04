package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/term"
)

// topAgents is the AGENTS section as bare text, without its borders: the
// rows of topAgentLines.
func topAgents(model topModel, now time.Time, style term.Style, width int) []string {
	frame := &topFrame{lines: topAgentLines(model, now, style, width), style: style}
	return frame.texts()
}

// topAgentColumns is the inner width from which the AGENTS box has two
// columns of equal width; under it there is one.
const topAgentColumns = 72

// topAgentCellsMax is the largest effective cap drawn as cells.
const topAgentCellsMax = 8

// topAgentLines is the AGENTS box: the agents filled row by row into two
// columns, or one under topAgentColumns. A cell is a mark — OK `●`, Warn `↺`
// with `until HH:MM`, Bad `✗` with the reason — the name padded to the widest
// listed name, one cell per slot of the effective cap (topAgentCells) and the
// quota text. A broken agent's reason comes from its agent needs-you entry,
// which topFetch keeps in a project's status too, "broken" when the status
// has none. A running agent's name takes its family colour (topFamily), an
// idle one's is Dim.
//
// The top border carries the note (topAgentNote). A row yields in the idle
// class only when every agent in it is idle (no running job, quota neither
// cooling nor broken), so the height drops idle rows first, uncounted. Off
// agents (an effective cap of 0, nothing running, quota ready) never list.
// The rest go broken and cooling first, then running, then idle, stable
// inside a group.
func topAgentLines(model topModel, now time.Time, style term.Style, width int) []topLine {
	line := func(text string, class topYield) topLine {
		return topLine{text: text, class: class, section: topSecAgents}
	}
	box := topBox{title: "AGENTS"}
	snap := model.capacity
	g := api.GlobalCap(snap)
	if len(snap.Agents) == 0 {
		return topBoxed(topSecAgents, box, []topLine{line(style.Dim("no agents advertised"), yieldRest)})
	}
	reasons := make(map[string]string)
	for _, n := range model.status.NeedsYou {
		if n.Kind == "agent" {
			reasons[n.Title] = n.Reason
		}
	}
	var listed []engine.CapacityAgent
	off := 0
	for _, a := range snap.Agents {
		if topAgentOff(a, g) {
			off++
			continue
		}
		listed = append(listed, a)
	}
	box.note = topAgentNote(snap, g, off, style)
	group := func(a engine.CapacityAgent) int {
		switch {
		case a.Quota == "broken" || a.Quota == "cooling":
			return 0
		case a.Running > 0:
			return 1
		}
		return 2
	}
	sort.SliceStable(listed, func(i, j int) bool { return group(listed[i]) < group(listed[j]) })
	cols, colW := 1, width
	if width >= topAgentColumns {
		cols, colW = 2, (width-len(topJobGap))/2
	}
	// The widest name sets the name column, but it gives way before the
	// cells and the quota text of a cooling or broken agent do.
	nameW, cellW, quotaW := 0, 0, 0
	names := make([]string, len(listed))
	for i, a := range listed {
		names[i] = topShortAgent(a.Harness + "/" + a.Model)
		nameW = max(nameW, term.CellWidth(names[i]))
		if slots, _, ok := topAgentCells(a, g); ok {
			cellW = max(cellW, slots)
		} else {
			cellW = max(cellW, term.CellWidth(api.RunningCap(a, g)))
		}
		switch a.Quota {
		case "cooling":
			quotaW = max(quotaW, len("until HH:MM"))
		case "broken":
			quotaW = max(quotaW, len("broken"))
		}
	}
	room := colW - topAgentMarkW(style) - 1 - len(topJobGap) - cellW
	if quotaW > 0 {
		room -= len(topJobGap) + quotaW
	}
	nameW = min(nameW, max(room, 8))
	cells := make([]string, len(listed))
	idle := make([]bool, len(listed))
	for i, a := range listed {
		idle[i] = a.Running == 0 && a.Quota != "broken" && a.Quota != "cooling"
		cells[i] = topAgentCell(a, g, names[i], nameW, colW, reasons[a.Harness+"/"+a.Model], now, style)
	}
	var rows []topLine
	for i := 0; i < len(cells); i += cols {
		text, class := cells[i], yieldIdle
		if !idle[i] {
			class = yieldRest
		}
		if cols == 2 && i+1 < len(cells) {
			text += strings.Repeat(" ", max(colW-term.CellWidth(text), 0)) + topJobGap + cells[i+1]
			if !idle[i+1] {
				class = yieldRest
			}
		}
		rows = append(rows, line(text, class))
	}
	return topBoxed(topSecAgents, box, rows)
}

// topAgentCell is one agent's cell within colW columns: the mark, the name
// padded to nameW, the cells of its cap and its quota text. Each part is cut
// with cutLine to the room the cell has left, so the cell never exceeds colW.
func topAgentCell(a engine.CapacityAgent, g *int, name string, nameW, colW int, reason string, now time.Time, style term.Style) string {
	var b strings.Builder
	used := 0
	put := func(text string, fn func(string) string) {
		room := max(colW-used, 0)
		text = cutLine(text, room)
		if term.CellWidth(text) > room {
			text = cutCells(text, room)
		}
		used += term.CellWidth(text)
		if fn != nil && text != "" {
			text = fn(text)
		}
		b.WriteString(text)
	}
	gap := func() { put(topJobGap, nil) }
	mark, markFn := style.Bullet(), style.OK
	quota, quotaFn := "", style.Dim
	switch a.Quota {
	case "broken":
		if reason == "" {
			reason = "broken"
		}
		mark, markFn = style.Cross(), style.Bad
		quota, quotaFn = reason, style.Bad
	case "cooling":
		mark, markFn = style.Again(), style.Warn
		quota, quotaFn = "until "+api.LocalClock(a.CoolingUntil, now), style.Warn
	}
	put(mark, markFn)
	put(strings.Repeat(" ", topAgentMarkW(style)-term.CellWidth(mark)+1), nil)
	nameFn := style.Dim
	if a.Running > 0 {
		nameFn = topFamily(style, name)
	}
	shown := cutLine(name, nameW)
	put(shown+strings.Repeat(" ", max(nameW-term.CellWidth(shown), 0)), nameFn)
	gap()
	if slots, filled, ok := topAgentCells(a, g); ok {
		put(strings.Repeat(style.Filled(), filled), style.Accent)
		put(strings.Repeat(style.Empty(), slots-filled), style.Dim)
	} else {
		put(api.RunningCap(a, g), style.Dim)
	}
	if quota != "" {
		gap()
		put(quota, quotaFn)
	}
	return b.String()
}

// topAgentMarkW is the width of the widest mark, which every mark pads to:
// one with UTF-8, two without.
func topAgentMarkW(style term.Style) int {
	return max(term.CellWidth(style.Bullet()), term.CellWidth(style.Cross()), term.CellWidth(style.Again()))
}

// topAgentCells says how an agent's running count draws as cells: one per
// slot of the effective cap, filled up to the running count. ok is false —
// the cap's own text shows instead — when nothing caps the agent, or the cap
// is 0 or over topAgentCellsMax.
func topAgentCells(a engine.CapacityAgent, g *int) (slots, filled int, ok bool) {
	limit := topAgentLimit(a, g)
	if limit == nil || *limit < 1 || *limit > topAgentCellsMax {
		return 0, 0, false
	}
	return *limit, min(a.Running, *limit), true
}

// topAgentLimit is an agent's effective cap: the smaller of its own and the
// global cap, as api.RunningCap folds them; nil when nothing caps it.
func topAgentLimit(a engine.CapacityAgent, global *int) *int {
	limit := a.Cap
	if global != nil && (limit == nil || *global < *limit) {
		limit = global
	}
	return limit
}

// topAgentNote is the AGENTS top border's note: `all ≤ global N` when the
// global cap sits below some agent's own cap or an agent has none, `M off`
// when M agents are off, joined by the separator or either alone, and
// `all ready` when no agent is broken or cooling, none is off and no layer
// caps anything.
func topAgentNote(snap engine.CapacitySnapshot, g *int, off int, style term.Style) string {
	var parts []string
	if g != nil {
		for _, a := range snap.Agents {
			if a.Cap == nil || *g < *a.Cap {
				parts = append(parts, fmt.Sprintf("all %s global %d", style.AtMost(), *g))
				break
			}
		}
	}
	if off > 0 {
		parts = append(parts, fmt.Sprintf("%d off", off))
	}
	if len(parts) > 0 {
		return strings.Join(parts, topEventSep(style))
	}
	for _, a := range snap.Agents {
		if a.Quota == "broken" || a.Quota == "cooling" || a.Cap != nil {
			return ""
		}
	}
	for _, l := range snap.Layers {
		if l.Cap != nil {
			return ""
		}
	}
	return "all ready"
}

// topAgentOff says an agent is off: an effective cap of 0 (its own, or the
// global cap's), no running job and a ready quota.
func topAgentOff(a engine.CapacityAgent, global *int) bool {
	limit := topAgentLimit(a, global)
	return limit != nil && *limit == 0 && a.Running == 0 && a.Quota == "ready"
}

// topFamily is the colour of an agent's name by its family, from the name as
// a row shows it: harness claude is violet, harness dsh mint, a last segment
// starting with glm blue, any other Dim. A trailing `@effort` is not part of
// the name. Agent names in AGENTS and in job lines both take it.
func topFamily(style term.Style, name string) func(string) string {
	name, _, _ = strings.Cut(name, "@")
	segs := strings.Split(name, "/")
	switch {
	case segs[0] == "claude":
		return style.Violet
	case segs[0] == "dsh":
		return style.Mint
	case strings.HasPrefix(segs[len(segs)-1], "glm"):
		return style.Blue
	}
	return style.Dim
}
