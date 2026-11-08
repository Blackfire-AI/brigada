package main

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/logo"
	"github.com/Blackfire-AI/brigada/internal/term"
)

// column is the rune column where sub starts in row, -1 when it is absent.
func column(row, sub string) int {
	i := strings.Index(row, sub)
	if i < 0 {
		return -1
	}
	return utf8.RuneCountInString(row[:i])
}

// TestTopJobColumnsLineUp: the running rows of a box share their columns —
// kind, agent, age and last event start in one column each — whatever the
// agent and the age, and the age pads its minutes and seconds.
func TestTopJobColumnsLineUp(t *testing.T) {
	at := func(ago time.Duration) string { return topNow.Add(-ago).UTC().Format(engine.TimeLayout) }
	st := api.Status{
		Plans: []engine.StatusPlan{{ID: 7, Title: "p", State: "active", Tasks: []engine.StatusTask{
			{ID: 342, Title: "clip frame to terminal height", State: "running"},
			{ID: 343, Title: "show plan state word", State: "running"},
			{ID: 344, Title: "agent order", State: "running"},
		}}},
		Running: []engine.StatusJob{
			{ID: 1, TaskID: 342, Kind: "work", Harness: "opencode", Model: "zai-coding-plan/glm-5.3", LeasedAt: at(3*time.Minute + 12*time.Second), LastEvent: "tool_start Edit cmdtop.go"},
			{ID: 2, TaskID: 343, Kind: "fix", Harness: "claude", Model: "sonnet", Effort: "high", LeasedAt: at(time.Hour + 2*time.Minute + 3*time.Second), LastEvent: "tool_start Bash go test"},
			{ID: 3, TaskID: 344, Kind: "review", Harness: "claude", Model: "opus", LeasedAt: at(45 * time.Second)},
		},
	}
	rows := topPlanLines(st, 100)[1:]
	if len(rows) != 3 {
		t.Fatalf("rows = %q", rows)
	}
	for _, c := range []struct{ a, b, c string }{
		{"work", "fix", "review"},
		{"opencode/glm-5.3", "claude/sonnet@high", "claude/opus"},
		{"3m12s", "1h02m03s", "45s"},
	} {
		if x, y, z := column(rows[0], c.a), column(rows[1], c.b), column(rows[2], c.c); x < 0 || x != y || x != z {
			t.Errorf("the column of %q is ragged (%d, %d, %d):\n%s", c.a, x, y, z, strings.Join(rows, "\n"))
		}
	}
	if x, y := column(rows[0], "Edit cmdtop.go"), column(rows[1], "Bash go test"); x < 0 || x != y {
		t.Errorf("the event column is ragged:\n%s", strings.Join(rows, "\n"))
	}
	if strings.Contains(rows[0], "zai-coding-plan") || strings.Contains(rows[0], "tool_start") {
		t.Errorf("agent or event not trimmed: %q", rows[0])
	}
}

// TestTopJobAge: seconds, then padded minutes and seconds, then hours; and
// the short form of an agent name.
func TestTopJobAge(t *testing.T) {
	for d, want := range map[time.Duration]string{
		45 * time.Second:                               "45s",
		9*time.Minute + 4*time.Second:                  "9m04s",
		time.Hour + 2*time.Minute + 3*time.Second:      "1h02m03s",
		10*time.Hour + 20*time.Minute + 30*time.Second: "10h20m30s",
	} {
		if got := topJobAge(topNow.Add(-d).UTC().Format(engine.TimeLayout), topNow); got != want {
			t.Errorf("age %s = %q, want %q", d, got, want)
		}
	}
	if got := topJobAge("", topNow); got != "" {
		t.Errorf("age of no lease = %q", got)
	}
	for in, want := range map[string]string{
		"opencode/zai-coding-plan/glm-5.3": "opencode/glm-5.3",
		"a/b/c/d":                          "a/d",
		"claude/sonnet":                    "claude/sonnet",
		"glm-5.3":                          "glm-5.3",
	} {
		if got := topShortAgent(in); got != want {
			t.Errorf("short %q = %q, want %q", in, got, want)
		}
	}
}

// TestTopPlanCellColours: one cell per task, cancelled and retired left
// out, each in the colour of its state; more than a third of the width and
// the tail is the count alone.
func TestTopPlanCellColours(t *testing.T) {
	style := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	p := engine.StatusPlan{ID: 1, Title: "p", State: "active", Tasks: []engine.StatusTask{
		{ID: 1, State: "done", Class: "implement", Landed: true},
		{ID: 2, State: "done", Class: "implement"},
		{ID: 3, State: "done", Class: "research"},
		{ID: 4, State: "running"},
		{ID: 5, State: "needs_user"},
		{ID: 6, State: "failed"},
		{ID: 7, State: "ready"},
		{ID: 8, State: "cancelled"},
		{ID: 9, State: "retired"},
	}}
	got := topPlanCells(p, map[int64]bool{4: true}, style)
	want := style.OK("■") + style.Teal("■") + style.OK("■") + style.Accent("■") + style.Bad("■") + style.Bad("■") + style.Dim("□")
	if got != want {
		t.Errorf("cells = %q, want %q", got, want)
	}
	if style.Teal("x") == style.Accent("x") {
		t.Error("teal is not distinct from Accent at 16 colours")
	}
	if tail := topPlanTail(p, map[int64]bool{4: true}, style, 80); term.StripEscapes(tail) != "■■■■■■□  2/7" {
		t.Errorf("tail = %q", term.StripEscapes(tail))
	}
	if tail := topPlanTail(p, nil, style, 18); term.StripEscapes(tail) != "2/7" {
		t.Errorf("tail with cells over a third of the width = %q, want the count alone", term.StripEscapes(tail))
	}
}

// TestTopPlansLegend: the PLANS border carries the legend, each cell in its
// colour, and leaves it out whole when it does not fit.
func TestTopPlansLegend(t *testing.T) {
	plain := term.New(logo.Caps{UTF8: true})
	box := topBox{title: "PLANS", note: "3 live · 98 finished", legend: topPlanLegend(plain)}
	if got := topBoxTop(box, plain, 92); !strings.Contains(got, " ■ landed  ▣ done  ◆ running  □ queued ─╮") || term.CellWidth(got) != 92 {
		t.Errorf("wide border = %q", got)
	}
	got := topBoxTop(box, plain, 50)
	if strings.Contains(got, "landed") || term.CellWidth(got) != 50 {
		t.Errorf("narrow border = %q, want the legend left out", got)
	}
	colour := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	legend := topPlanLegend(colour)
	for _, c := range []string{colour.OK("■"), colour.Teal("■"), colour.Accent("■"), colour.Dim("□")} {
		if !strings.Contains(legend, c) {
			t.Errorf("legend %q lacks the cell %q", legend, c)
		}
	}
	ascii := topPlanLegend(term.New(logo.Caps{}))
	if ascii != "# landed  = done  > running  . queued" {
		t.Errorf("ASCII legend = %q", ascii)
	}
}

// TestTopPlanGlyphsTellStatesApart: without colour, or without UTF-8, the
// cells and the legend give landed, done, running and queued a glyph each;
// a parked task shows `!` in the cells and is not in the legend; every
// glyph is one cell wide.
func TestTopPlanGlyphsTellStatesApart(t *testing.T) {
	p := engine.StatusPlan{ID: 1, Title: "p", State: "active", Tasks: []engine.StatusTask{
		{ID: 1, State: "done", Class: "implement", Landed: true},
		{ID: 2, State: "done", Class: "implement"},
		{ID: 3, State: "running"},
		{ID: 4, State: "ready"},
		{ID: 5, State: "needs_foreman"},
	}}
	for _, tt := range []struct {
		name          string
		caps          logo.Caps
		cells, legend string
	}{
		{"ASCII", logo.Caps{}, "#=>.!", "# landed  = done  > running  . queued"},
		{"ASCII with colour", logo.Caps{Color: logo.Color256}, "#=>.!", "# landed  = done  > running  . queued"},
		{"Unicode no colour", logo.Caps{UTF8: true}, "■▣◆□!", "■ landed  ▣ done  ◆ running  □ queued"},
		{"Unicode NO_COLOR", logo.Caps{Color: logo.NoColor, UTF8: true}, "■▣◆□!", "■ landed  ▣ done  ◆ running  □ queued"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			style := term.New(tt.caps)
			cells := term.StripEscapes(topPlanCells(p, map[int64]bool{3: true}, style))
			if cells != tt.cells {
				t.Errorf("cells = %q, want %q", cells, tt.cells)
			}
			legend := term.StripEscapes(topPlanLegend(style))
			if legend != tt.legend {
				t.Errorf("legend = %q, want %q", legend, tt.legend)
			}
			seen := map[rune]bool{}
			for _, r := range cells {
				if seen[r] {
					t.Errorf("cells %q repeat the glyph %q", cells, r)
				}
				seen[r] = true
				if n := term.CellWidth(string(r)); n != 1 {
					t.Errorf("glyph %q is %d cells wide", r, n)
				}
			}
			if strings.Contains(legend, "!") {
				t.Errorf("legend %q lists the parked glyph", legend)
			}
		})
	}
}

// TestTopTints: a needs-you row and a running row carry their tint at 256
// colours and true colour, none at 16 colours or without colour; the
// background survives the spans inside the row, and the text without
// escapes is the plain row.
func TestTopTints(t *testing.T) {
	model := agentsModel()
	plain := renderTop(model, 100, 0, topNow, term.New(logo.Caps{UTF8: true}))
	rowWith := func(t *testing.T, lines []string, sub string) string {
		t.Helper()
		for _, l := range lines {
			if strings.Contains(l, "│") && strings.Contains(term.StripEscapes(l), sub) {
				return l
			}
		}
		t.Fatalf("no row with %q", sub)
		return ""
	}
	for _, tt := range []struct {
		name                           string
		caps                           logo.Caps
		needsBg, runBg, needsFg, runFg string
	}{
		{"256", logo.Caps{Color: logo.Color256, UTF8: true}, "\x1b[48;5;52m", "\x1b[48;5;17m", "\x1b[38;5;224m", "\x1b[38;5;153m"},
		{"true", logo.Caps{Color: logo.TrueColor, UTF8: true}, "\x1b[48;2;59;20;24m", "\x1b[48;2;18;35;63m", "\x1b[38;2;255;215;215m", "\x1b[38;2;215;227;255m"},
		{"16", logo.Caps{Color: logo.Color16, UTF8: true}, "", "", "", ""},
		{"NO_COLOR", logo.Caps{Color: logo.NoColor, UTF8: true}, "", "", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			lines := renderTop(model, 100, 0, topNow, term.New(tt.caps))
			for i := range lines {
				if got := term.StripEscapes(lines[i]); normPlanGlyphs(got) != normPlanGlyphs(plain[i]) {
					t.Errorf("line %d reads %q, want %q", i, got, plain[i])
				}
			}
			need := rowWith(t, lines, "! #2 should the sprocket be blue?")
			run := rowWith(t, lines, "▸ #3")
			for _, c := range []struct{ name, row, bg, fg string }{{"needs", need, tt.needsBg, tt.needsFg}, {"run", run, tt.runBg, tt.runFg}} {
				if c.bg == "" {
					if strings.Contains(c.row, "\x1b[48;") {
						t.Errorf("%s row has a background: %q", c.name, c.row)
					}
					continue
				}
				if !strings.Contains(c.row, c.bg+c.fg) {
					t.Errorf("%s row lacks its tint %q: %q", c.name, c.bg+c.fg, c.row)
				}
				// The tint ends in a reset right before the closing border, so
				// it covers the margins and the padding inside the borders.
				if !strings.Contains(c.row, "  \x1b[0m") {
					t.Errorf("%s row's tint does not cover the padding: %q", c.name, c.row)
				}
			}
			// The Accent pointer inside the running row ends in a reset; the
			// tint is re-opened right after it.
			if tt.runBg != "" && !strings.Contains(run, "\x1b[0m"+tt.runBg+tt.runFg) {
				t.Errorf("the running row loses its background after an inner span: %q", run)
			}
		})
	}
}

// TestRenderTopFrameHeights: the frame at 12, 24 and 40 rows never has more
// lines than the height, never a line over the width, the header first and
// NEEDS YOU kept.
func TestRenderTopFrameHeights(t *testing.T) {
	model := agentsModel()
	model.statusOK = true
	for _, height := range []int{12, 24, 40} {
		for _, width := range []int{60, 80, 100, 120} {
			lines := renderTop(model, width, height, topNow, term.New(logo.Caps{UTF8: true}))
			if len(lines) > height {
				t.Errorf("%dx%d: %d lines", width, height, len(lines))
			}
			if !strings.HasPrefix(lines[0], " ▙ brigada") || !strings.Contains(strings.Join(lines, "\n"), "╭─ NEEDS YOU") {
				t.Errorf("%dx%d: header or NEEDS YOU lost:\n%s", width, height, strings.Join(lines, "\n"))
			}
			for _, l := range lines {
				if term.CellWidth(l) > width {
					t.Errorf("%dx%d: %q is wider", width, height, l)
				}
			}
		}
	}
}
