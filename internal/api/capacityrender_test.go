package api

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/logo"
	"github.com/Blackfire-AI/brigada/internal/term"
)

// capacityFixture is one snapshot with every quota state, a capped and an
// uncapped agent, and each kind of layer.
func capacityFixture(t *testing.T) (engine.CapacitySnapshot, time.Time) {
	t.Helper()
	// The cooling clock is shown in now's zone; a UTC now keeps the golden
	// the same on every machine.
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	two := 2
	four := 4
	eight := 8
	return engine.CapacitySnapshot{
		Agents: []engine.CapacityAgent{
			{Harness: "claude", Model: "opus", Tier: "heavy", Effort: "high", Cost: 3,
				Cap: nil, CapLayer: "none", Running: 1, Quota: "ready"},
			{Harness: "codex", Model: "gpt-x", Tier: "standard", Effort: "default", Cost: 1,
				Cap: &two, CapLayer: "account", Running: 2, Quota: "cooling",
				CoolingUntil: "2026-10-20T12:30:00.000000000Z"},
			{Harness: "kimi", Model: "k2", Tier: "light", Effort: "default", Cost: 1,
				Cap: nil, CapLayer: "none", Running: 0, Quota: "broken"},
		},
		Layers: []engine.CapacityLayer{
			{Layer: "runner", RunnerID: 1, Cap: &four, Use: 3},
			{Layer: "account", Account: "claude", Use: 1},
			{Layer: "account", Account: "codex", Cap: &two, Use: 2, Queue: 1},
			{Layer: "model", Model: "gpt-x", Cap: &two, Use: 2, Queue: 1},
			{Layer: "plan", PlanID: 7, Use: 1},
			{Layer: "global", Cap: &eight, Use: 3},
		},
	}, now
}

// capacityGolden is the rendering with a plain style at the default 80
// columns: the agents table, then the layers table.
const capacityGolden = `AGENTS
AGENT        TIER      EFFORT   COST  RUNNING/CAP    QUOTA
claude/opus  heavy     high     3     1/8 (global)   ready
codex/gpt-x  standard  default  1     2/2 (account)  cooling until 12:30
kimi/k2      light     default  1     0/8 (global)   broken

LAYERS
LAYER    KEY     CAP   USE  QUEUE
runner   1       4     3    0
account  claude  none  1    0
account  codex   2     2    1
model    gpt-x   2     2    1
plan     7       none  1    0
global   -       8     3    0`

// TestRunningCapFoldsGlobal is the effective-cap rule as a table: the
// agent's own layer binds, the global layer binds, a tie names the
// agent's own layer, an uncapped agent under a capped global, a global
// cap of 0, no global layer at all, and a plan layer left out of the
// fold.
func TestRunningCapFoldsGlobal(t *testing.T) {
	two := 2
	four := 4
	zero := 0
	global := func(cap *int) []engine.CapacityLayer {
		return []engine.CapacityLayer{{Layer: "global", Cap: cap}}
	}
	tests := []struct {
		name   string
		agent  engine.CapacityAgent
		layers []engine.CapacityLayer
		want   string
	}{
		{"the agent's own layer binds", engine.CapacityAgent{Running: 1, Cap: &two, CapLayer: "account"}, global(&four), "1/2 (account)"},
		{"the global layer binds", engine.CapacityAgent{Running: 1, Cap: &four, CapLayer: "runner"}, global(&two), "1/2 (global)"},
		{"a tie names the agent's own layer", engine.CapacityAgent{Running: 1, Cap: &two, CapLayer: "model"}, global(&two), "1/2 (model)"},
		{"an uncapped agent under a capped global", engine.CapacityAgent{Running: 1, CapLayer: "none"}, global(&four), "1/4 (global)"},
		{"a global cap of 0 binds over any agent cap", engine.CapacityAgent{Running: 1, Cap: &four, CapLayer: "runner"}, global(&zero), "1/0 (global)"},
		{"no global layer", engine.CapacityAgent{Running: 1, Cap: &two, CapLayer: "account"}, nil, "1/2 (account)"},
		{"neither caps", engine.CapacityAgent{Running: 1, CapLayer: "none"}, nil, "1/none"},
		{"a plan layer does not cap an agent", engine.CapacityAgent{Running: 1, CapLayer: "none"}, []engine.CapacityLayer{{Layer: "plan", PlanID: 7, Cap: &two}}, "1/none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := engine.CapacitySnapshot{Layers: tt.layers}
			if got := RunningCap(tt.agent, GlobalCap(snap)); got != tt.want {
				t.Errorf("RunningCap = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCapacityTablesGolden(t *testing.T) {
	snap, now := capacityFixture(t)
	got := CapacityTables(snap, now, term.New(logo.Caps{}), 80)
	if got != capacityGolden {
		t.Errorf("the rendering:\n%s\nwant the golden:\n%s", got, capacityGolden)
	}
}

func TestCapacityTablesWidthCutsLastColumn(t *testing.T) {
	snap, now := capacityFixture(t)
	got := CapacityTables(snap, now, term.New(logo.Caps{}), 60)
	// The quota column starts at 53; 60 columns leave 7 for it.
	if !strings.Contains(got, "codex/gpt-x  standard  default  1     2/2 (account)  cool...") {
		t.Errorf("a 60-column rendering should cut the quota cell to 7 runes:\n%s", got)
	}
	if strings.Contains(got, "12:30") {
		t.Errorf("the cut quota cell should not show the cooling clock:\n%s", got)
	}
}

// TestCapacityTablesColourKeepsText checks that colour only adds escapes:
// with them stripped, the text is the plain golden, so the columns line up
// the same way.
func TestCapacityTablesColourKeepsText(t *testing.T) {
	snap, now := capacityFixture(t)
	coloured := CapacityTables(snap, now, term.New(logo.Caps{Color: logo.Color256, TTY: true, Width: 80}), 80)
	if !strings.Contains(coloured, "\x1b[") {
		t.Fatalf("a colour style should write escapes:\n%q", coloured)
	}
	escape := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	if plain := escape.ReplaceAllString(coloured, ""); plain != capacityGolden {
		t.Errorf("the coloured rendering without escapes:\n%s\nwant the golden:\n%s", plain, capacityGolden)
	}
}
