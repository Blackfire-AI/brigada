package api

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Blackfire-AI/brigada/internal/caps"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/term"
)

// CapacityTables renders the capacity snapshot (§12.2) as two aligned
// tables for a terminal. AGENTS: each agent's harness/model, tier, effort,
// cost weight, running jobs against its cap with the layer that sets it,
// and its quota — ready, cooling until a local time, or broken. LAYERS:
// each cap layer's name and key, its cap, use and queue depth. style
// supplies the colour; with a plain style the text is the same and has no
// escapes. The last column of each table is cut to fit width.
func CapacityTables(snap engine.CapacitySnapshot, now time.Time, style term.Style, width int) string {
	var lines []string
	lines = append(lines, style.Bold("AGENTS"))
	if len(snap.Agents) == 0 {
		lines = append(lines, "  no agents advertised")
	} else {
		rows := [][]capCell{capHeader(style, "AGENT", "TIER", "EFFORT", "COST", "RUNNING/CAP", "QUOTA")}
		for _, a := range snap.Agents {
			rows = append(rows, []capCell{
				plainCell(a.Harness + "/" + a.Model),
				plainCell(a.Tier),
				plainCell(a.Effort),
				plainCell(strconv.Itoa(a.Cost)),
				plainCell(runningCap(a)),
				quotaCell(a, now, style),
			})
		}
		lines = append(lines, alignCapacity(rows, width)...)
	}
	lines = append(lines, "", style.Bold("LAYERS"))
	rows := [][]capCell{capHeader(style, "LAYER", "KEY", "CAP", "USE", "QUEUE")}
	for _, l := range snap.Layers {
		rows = append(rows, []capCell{
			plainCell(l.Layer),
			plainCell(layerKey(l)),
			plainCell(capLimit(l.Cap)),
			plainCell(strconv.Itoa(l.Use)),
			plainCell(strconv.Itoa(l.Queue)),
		})
	}
	lines = append(lines, alignCapacity(rows, width)...)
	return strings.Join(lines, "\n")
}

// capCell is one table cell: its plain text, and the style that colours
// it. Padding is worked out from the text, so the colour adds no width.
type capCell struct {
	text  string
	style func(string) string
}

func plainCell(text string) capCell {
	return capCell{text: text, style: func(s string) string { return s }}
}

func capHeader(style term.Style, names ...string) []capCell {
	cells := make([]capCell, len(names))
	for i, name := range names {
		cells[i] = capCell{text: name, style: style.Dim}
	}
	return cells
}

// quotaCell is an agent's quota state: ready, cooling until the local
// time the cooling lapses, or broken.
func quotaCell(a engine.CapacityAgent, now time.Time, style term.Style) capCell {
	switch a.Quota {
	case "broken":
		return capCell{text: "broken", style: style.Bad}
	case "cooling":
		return capCell{text: "cooling until " + localClock(a.CoolingUntil, now), style: style.Warn}
	}
	return plainCell("ready")
}

// localClock is a stored UTC stamp as a clock time in now's zone (the
// local zone when now is time.Now): the hour and minute on the day of now,
// the date too on any other day. A stamp that does not parse is shown as
// it is.
func localClock(stamp string, now time.Time) string {
	t, err := time.Parse(engine.TimeLayout, stamp)
	if err != nil {
		return stamp
	}
	t = t.In(now.Location())
	if t.Format("2006-01-02") == now.Format("2006-01-02") {
		return t.Format("15:04")
	}
	return t.Format("2006-01-02 15:04")
}

// runningCap is an agent's running jobs against its cap, with the layer
// that sets the cap: "1/4 (runner)", or "1/none" when no layer caps it.
func runningCap(a engine.CapacityAgent) string {
	text := strconv.Itoa(a.Running) + "/" + capLimit(a.Cap)
	if a.Cap != nil {
		text += " (" + a.CapLayer + ")"
	}
	return text
}

// capLimit is one cap as the tables show it: "none" for an uncapped
// layer, the number otherwise. 0 stays "0", a layer capped at nothing.
func capLimit(cap *int) string {
	if cap == nil {
		return "none"
	}
	return strconv.Itoa(*cap)
}

// layerKey names a layer row's instance: the runner or plan id, the
// account or model label, "-" for a layer with no instance (global).
func layerKey(l engine.CapacityLayer) string {
	key := ""
	switch l.Layer {
	case caps.LayerRunner:
		key = strconv.FormatInt(l.RunnerID, 10)
	case caps.LayerAccount:
		key = l.Account
	case caps.LayerModel:
		key = l.Model
	case caps.LayerPlan:
		key = strconv.FormatInt(l.PlanID, 10)
	}
	if key == "" {
		return "-"
	}
	return key
}

// alignCapacity lays rows out in columns two spaces apart. Every column
// but the last is padded to its widest cell; the last is not padded and
// is cut with "..." to the width the others leave.
func alignCapacity(rows [][]capCell, width int) []string {
	last := len(rows[0]) - 1
	widths := make([]int, last)
	for _, r := range rows {
		for c := range last {
			widths[c] = max(widths[c], utf8.RuneCountInString(r[c].text))
		}
	}
	used := 0
	for _, w := range widths {
		used += w + 2
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		var b strings.Builder
		for c := range last {
			b.WriteString(r[c].style(r[c].text))
			b.WriteString(strings.Repeat(" ", widths[c]-utf8.RuneCountInString(r[c].text)+2))
		}
		b.WriteString(r[last].style(fitCell(r[last].text, width-used)))
		out = append(out, b.String())
	}
	return out
}

// fitCell cuts text to room runes, ending it with "...", when it is longer
// and room leaves space for the cut; otherwise it is returned whole.
func fitCell(text string, room int) string {
	if n := utf8.RuneCountInString(text); n > room && room > 3 {
		return string([]rune(text)[:room-3]) + "..."
	}
	return text
}
