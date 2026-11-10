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

// topAgentCellsMax is the largest effective cap drawn as cells.
const topAgentCellsMax = 8

// topAgentLines is the AGENTS box: the agents filled row by row into two
// columns when twice the width a cell needs whole (plus the gap) fits the
// box, else one. A cell is a mark — OK `●`, Warn `↺`
// with `until HH:MM`, Bad `✗` with the reason — the name padded to the widest
// listed name, the tag (topAgentTag) padded to the widest listed tag and Dim,
// one cell per slot of the effective cap (topAgentCells) and the
// quota text. A broken agent's reason comes from its agent needs-you entry,
// which topFetch keeps in a project's status too, "broken" when the status
// has none. A running agent's name takes its family colour (topFamily), an
// idle one's is Dim.
//
// The top border carries the note (topAgentNote): the listed agents that
// are broken (Bad) and cooling (Warn) counted in front of the global-cap
// and off parts, as `1 broken · 3 cooling · all ≤ global 4 · 2 off`. A row
// yields in the idle class only when every agent in it is idle (no running
// job, quota neither cooling nor broken), so the height drops idle rows
// first, uncounted. Off agents (an effective cap of 0 and nothing running,
// whatever their quota) never list. The rest go in the order of the full harness/model string,
// byte by byte, so a row moves only when an agent is added or removed; state
// shows through the mark and the colours alone.
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
	key := func(a engine.CapacityAgent) string { return a.Harness + "/" + a.Model }
	sort.SliceStable(listed, func(i, j int) bool { return key(listed[i]) < key(listed[j]) })
	broken, cooling := 0, 0
	for _, a := range listed {
		switch a.Quota {
		case "broken":
			broken++
		case "cooling":
			cooling++
		}
	}
	box.note = topAgentNote(snap, g, off, broken, cooling, style)
	// The widest name sets the name column, but it gives way before the
	// tag, the cells and the quota text of a cooling or broken agent do.
	nameW, tagW, cellW, quotaW := 0, 0, 0, 0
	names := make([]string, len(listed))
	tags := make([]string, len(listed))
	for i, a := range listed {
		tags[i] = topAgentTag(a, style)
		tagW = max(tagW, term.CellWidth(tags[i]))
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
	// A cell needs its widest name whole; a broken agent's longer reason does
	// not count and may be cut at the end of its cell.
	need := topAgentMarkW(style) + 1 + nameW + len(topJobGap) + tagW + len(topJobGap) + cellW
	if quotaW > 0 {
		need += len(topJobGap) + quotaW
	}
	cols, colW := 1, width
	if 2*need+len(topJobGap) <= width {
		cols, colW = 2, (width-len(topJobGap))/2
	}
	room := colW - topAgentMarkW(style) - 1 - len(topJobGap) - tagW - len(topJobGap) - cellW
	if quotaW > 0 {
		room -= len(topJobGap) + quotaW
	}
	nameW = min(nameW, max(room, 8))
	cells := make([]string, len(listed))
	idle := make([]bool, len(listed))
	for i, a := range listed {
		idle[i] = a.Running == 0 && a.Quota != "broken" && a.Quota != "cooling"
		tags[i] += strings.Repeat(" ", tagW-term.CellWidth(tags[i]))
		cells[i] = topAgentCell(a, g, names[i], nameW, tags[i], colW, reasons[a.Harness+"/"+a.Model], now, style)
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
// padded to nameW, the tag (already padded, drawn Dim), the cells of its cap
// and its quota text. Each part is cut
// with cutLine to the room the cell has left, so the cell never exceeds colW.
func topAgentCell(a engine.CapacityAgent, g *int, name string, nameW int, tag string, colW int, reason string, now time.Time, style term.Style) string {
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
	put(tag, style.Dim)
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

// topAgentTag is an agent's tag: its tier's first letter in upper case (`?`
// when the tier is empty), the times sign and its cost weight, as in `H×3`.
func topAgentTag(a engine.CapacityAgent, style term.Style) string {
	tier := "?"
	if a.Tier != "" {
		tier = strings.ToUpper(a.Tier[:1])
	}
	return fmt.Sprintf("%s%s%d", tier, style.Times(), a.Cost)
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

// topAgentNote is the AGENTS top border's note, part by part: the listed
// agents that are broken (`N broken`, Bad) and cooling (`N cooling`, Warn)
// counted, then `all ≤ global N` when the global cap sits below some
// agent's own cap or an agent has none, and `M off` when M agents are off —
// each part left out when it does not apply, joined by the separator — and
// `all ready` when no agent is broken or cooling, none is off and no layer
// caps anything.
func topAgentNote(snap engine.CapacitySnapshot, g *int, off, broken, cooling int, style term.Style) []topNotePart {
	var parts []topNotePart
	if broken > 0 {
		parts = append(parts, topNotePart{fmt.Sprintf("%d broken", broken), style.Bad})
	}
	if cooling > 0 {
		parts = append(parts, topNotePart{fmt.Sprintf("%d cooling", cooling), style.Warn})
	}
	if g != nil {
		for _, a := range snap.Agents {
			if a.Cap == nil || *g < *a.Cap {
				parts = append(parts, topNotePart{fmt.Sprintf("all %s global %d", style.AtMost(), *g), style.Dim})
				break
			}
		}
	}
	if off > 0 {
		parts = append(parts, topNotePart{fmt.Sprintf("%d off", off), style.Dim})
	}
	if len(parts) > 0 {
		return parts
	}
	for _, a := range snap.Agents {
		if a.Cap != nil {
			return nil
		}
	}
	for _, l := range snap.Layers {
		if l.Cap != nil {
			return nil
		}
	}
	return []topNotePart{{"all ready", style.Dim}}
}

// topAgentOff says an agent is off: an effective cap of 0 (its own, or the
// global cap's) and no running job, whatever its quota — an agent that can
// never take a job stays hidden even while its shared account cools or it is
// broken; the header's off count carries it.
func topAgentOff(a engine.CapacityAgent, global *int) bool {
	limit := topAgentLimit(a, global)
	return limit != nil && *limit == 0 && a.Running == 0
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
