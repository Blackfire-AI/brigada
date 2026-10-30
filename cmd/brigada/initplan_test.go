package main

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/logo"
	"github.com/Blackfire-AI/brigada/internal/term"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

func TestInitFileStateColors(t *testing.T) {
	style := term.New(logo.Caps{TTY: true, Color: logo.Color16, UTF8: true, Width: 100})
	var b bytes.Buffer
	w := initOutput{Writer: &b, style: style, styled: true, utf8: true}
	files := []wireFile{
		{rel: wiringMCPFile, next: []byte("{}\n"), state: "created"},
		{rel: wiringSettingsFile, old: []byte("a\n"), exists: true, next: []byte("b\n"), state: "added"},
		{rel: wiringSkillFile, old: []byte("mine\n"), exists: true, err: fmt.Errorf("not brigada's")},
		{rel: wiringSharedSettingsFile, old: []byte("{}\n"), exists: true, next: []byte("{}\n"), state: "unchanged"},
	}
	printInitFiles(w, files, false)
	out := b.String()
	for _, want := range []string{
		style.OK("new") + " " + wiringMCPFile,
		style.Warn("changes") + " " + wiringSettingsFile,
		style.Bad("left alone") + " " + wiringSkillFile,
		style.Dim("unchanged") + " " + wiringSharedSettingsFile,
	} {
		if !strings.Contains(strings.Join(strings.Fields(out), " "), want) {
			t.Errorf("missing state-first row %q:\n%q", want, out)
		}
	}
}

func TestInitPlanAgentRowsFit80(t *testing.T) {
	agents := []agentSpec{
		{harness: "claude", model: "sonnet", effort: "high", classes: []string{"implement", "review", "answer"}},
		{harness: "codex", model: "gpt-6.1-sol", classes: []string{"implement", "review", "answer"}},
		{harness: "kimi", model: "kimi-code/k3", classes: []string{"review"}},
		{harness: "opencode", model: "zai-coding-plan/glm-5.3", effort: "low", classes: []string{"implement", "review", "answer"}},
	}
	var b bytes.Buffer
	printInitAgents(&b, agents, 4, false, agents[0], nil, adapter.AgentTier)
	rows := 0
	for _, line := range strings.Split(b.String(), "\n") {
		if term.VisibleWidth(line) >= 80 {
			t.Errorf("line reaches 80 columns: %q", line)
		}
		// Each row starts with its harness; a long role wraps under its column.
		for _, harness := range initHarnesses {
			if strings.HasPrefix(line, "    "+harness+" ") {
				rows++
			}
		}
	}
	for _, want := range []string{"sonnet @high", "standard", "reviews only"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("plan lacks %q:\n%s", want, b.String())
		}
	}
	if rows != 4 || strings.Contains(b.String(), "(default)") {
		t.Errorf("agent rows are not one per agent:\n%s", b.String())
	}
}

func TestInitPlanRoleColumn(t *testing.T) {
	for _, tc := range []struct {
		name       string
		model      string
		stacked    bool
		roleColumn int
	}{
		{"approved names", "zai-coding-plan/glm-5.3", false, 62},
		{"two-line role fits at column 79", "zai-coding-plan/glm-" + strings.Repeat("x", 8), false, 67},
		{"two-line role reaches column 80", "zai-coding-plan/glm-" + strings.Repeat("x", 9), true, 37},
		{"golden fixture width", "kimi-code/kimi-for-coding-highspeed", true, 37},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agents := []agentSpec{
				{harness: "claude", model: "opus", classes: []string{"implement", "review", "answer"}},
				{harness: "codex", model: "gpt-6.1-sol", classes: []string{"implement", "review", "answer"}},
				{harness: "opencode", model: tc.model, classes: []string{"implement", "review", "answer"}},
			}
			// Keep the same family width for every case, including Kimi's
			// golden-fixture model, so the boundary cases differ by one column.
			agents = append(agents, agentSpec{harness: "opencode", model: "zai-coding-plan/glm-5.3", classes: []string{"review"}})
			var out bytes.Buffer
			printInitAgents(&out, agents, 4, false, agents[0], nil, adapter.AgentTier)
			_, table, _ := strings.Cut(out.String(), "foreman picks the tier per task):\n")
			table, _, _ = strings.Cut(table, "  Reviews:")
			rows, families, continuations := 0, 0, 0
			familyColumn, tierColumn := -1, -1
			for _, line := range strings.Split(table, "\n") {
				if term.VisibleWidth(line) >= 80 {
					t.Errorf("line reaches 80 columns: %q", line)
				}
				isRow := strings.HasPrefix(line, "    claude ") || strings.HasPrefix(line, "    codex ") || strings.HasPrefix(line, "    opencode ")
				if isRow {
					rows++
				}
				hasFamily := false
				for _, family := range []string{"Anthropic", "OpenAI", "Moonshot", "Zhipu (GLM)"} {
					if column := strings.Index(line, family); column >= 0 {
						hasFamily = true
						families++
						if familyColumn >= 0 && familyColumn != column {
							t.Errorf("family column = %d, want %d: %q", column, familyColumn, line)
						}
						familyColumn = column
						for _, tier := range []string{"heavy", "standard", "light"} {
							if column := strings.Index(line, tier); column >= 0 {
								if tierColumn >= 0 && tierColumn != column {
									t.Errorf("tier column = %d, want %d: %q", column, tierColumn, line)
								}
								tierColumn = column
							}
						}
					}
				}
				if isRow && hasFamily == tc.stacked {
					t.Errorf("stacked = %v, wrong row layout: %q", tc.stacked, line)
				}
				if !isRow && !hasFamily && strings.TrimSpace(line) != "" {
					continuations++
					if column := len(line) - len(strings.TrimLeft(line, " ")); column != tc.roleColumn {
						t.Errorf("role continuation at %d, want %d: %q", column, tc.roleColumn, line)
					}
				}
			}
			if rows != len(agents) || families != len(agents) {
				t.Errorf("rows = %d, families = %d, want %d:\n%s", rows, families, len(agents), table)
			}
			if !tc.stacked && continuations == 0 {
				t.Errorf("role never wrapped:\n%s", table)
			}
			if tc.name == "approved names" && !strings.Contains(table, "writes code,\n"+strings.Repeat(" ", tc.roleColumn)+"reviews\n") {
				t.Errorf("role phrase did not wrap within its column:\n%s", table)
			}
		})
	}
}

func TestInitPlanLongModelStacks(t *testing.T) {
	agents := []agentSpec{
		{harness: "kimi", model: "kimi-code/kimi-for-coding-highspeed", classes: []string{"review"}},
		{harness: "claude", model: "fable", classes: []string{"implement", "review", "answer"}},
	}
	var b bytes.Buffer
	printInitAgents(&b, agents, 2, false, agents[1], nil, adapter.AgentTier)
	_, table, _ := strings.Cut(b.String(), "foreman picks the tier per task):\n")
	table, _, _ = strings.Cut(table, "  Reviews:")
	lines := strings.Split(strings.TrimRight(table, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("want two lines per agent:\n%s", table)
	}
	familyColumn, tierColumn := -1, -1
	for i, line := range lines {
		if term.VisibleWidth(line) >= 80 {
			t.Errorf("line reaches 80 columns: %q", line)
		}
		if i%2 == 0 {
			if !strings.HasPrefix(line, "    "+agents[i/2].harness+" ") || strings.Contains(line, "Moonshot") || strings.Contains(line, "Anthropic") {
				t.Errorf("first line is not the model line: %q", line)
			}
			continue
		}
		family, tier := "Moonshot", "light"
		if i == 3 {
			family, tier = "Anthropic", "heavy"
		}
		f, ti := strings.Index(line, family), strings.Index(line, tier)
		if f < 0 || ti < f {
			t.Fatalf("second line lacks family and tier: %q", line)
		}
		if familyColumn < 0 {
			familyColumn, tierColumn = f, ti
		} else if f != familyColumn || ti != tierColumn {
			t.Errorf("columns = %d, %d, want %d, %d: %q", f, ti, familyColumn, tierColumn, line)
		}
	}
	for _, role := range []string{"reviews only", "writes code, reviews"} {
		if !strings.Contains(table, role) {
			t.Errorf("role %q split over lines:\n%s", role, table)
		}
	}
}

func TestInitShortModelPlan(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		t.Run(fmt.Sprintf("wrapped=%t", wrapped), func(t *testing.T) {
			importRepo(t)
			fakeInitPATH(t, "claude", "codex")
			fakeInitCodexCatalog(t)
			args := []string{"--probe", "--signed-in", "claude,codex", "--check", "true", "--tools", "none", "--no-wiring"}
			want := []string{
				"claude opus Anthropic heavy writes code, reviews",
				"claude sonnet Anthropic standard writes code, reviews",
				"claude haiku Anthropic light writes code, reviews",
				"codex gpt-6.1-sol OpenAI standard writes code, reviews",
				"codex gpt-5 OpenAI standard writes code, reviews",
				"codex gpt-5.4-mini OpenAI light writes code, reviews",
			}
			if wrapped {
				args = append(args, "--agent", "harness=claude,model=sonnet", "--agent", "harness=codex,model=gpt-6.1-sol,effort=ultra,classes=implement+answer", "--agent", "harness=codex,model=gpt-5")
				want = []string{
					"claude sonnet Anthropic standard writes code, reviews",
					"codex gpt-6.1-sol @ultra OpenAI standard writes code, answers questions",
					"codex gpt-5 OpenAI standard writes code, reviews",
				}
			} else {
				for _, spec := range []string{
					"harness=claude,model=opus", "harness=claude,model=sonnet", "harness=claude,model=haiku",
					"harness=codex,model=gpt-6.1-sol", "harness=codex,model=gpt-5", "harness=codex,model=gpt-5.4-mini",
				} {
					args = append(args, "--agent", spec)
				}
			}
			code, out, errs := initFixedParallel(t, testsupport.ShortStateDir(t), "", args...)
			if code != 0 || errs != "" {
				t.Fatalf("init = %d:\n%s\n%s", code, out, errs)
			}
			for _, line := range strings.Split(out, "\n") {
				if term.VisibleWidth(line) >= 80 {
					t.Errorf("line reaches 80 columns: %q", line)
				}
			}
			if strings.Contains(strings.ToLower(out), "ready") {
				t.Error("claimed ready before apply")
			}
			_, table, ok := strings.Cut(out, "foreman picks the tier per task):\n")
			if !ok {
				t.Fatalf("missing agents table:\n%s", out)
			}
			table, _, _ = strings.Cut(table, "  Reviews:")
			var rows []string
			familyColumn, tierColumn, roleColumn, continuations := -1, -1, -1, 0
			for _, line := range strings.Split(strings.TrimSuffix(table, "\n"), "\n") {
				fields := strings.Fields(line)
				if len(fields) == 0 {
					continue
				}
				if fields[0] != "claude" && fields[0] != "codex" {
					continuations++
					if column := strings.Index(line, fields[0]); column != roleColumn || len(rows) == 0 {
						t.Fatalf("role continuation at %d, want %d: %q", column, roleColumn, line)
					}
					rows[len(rows)-1] += " " + strings.Join(fields, " ")
					continue
				}
				family := "Anthropic"
				if fields[0] == "codex" {
					family = "OpenAI"
				}
				column := strings.Index(line, family)
				if column < 0 {
					t.Fatalf("family must be on the agent's first line: %q", line)
				}
				if familyColumn >= 0 && column != familyColumn {
					t.Errorf("family column = %d, want %d", column, familyColumn)
				}
				familyColumn = column
				after := line[column+len(family):]
				cells := strings.Fields(after)
				column += len(family) + strings.Index(after, cells[0])
				if tierColumn >= 0 && column != tierColumn {
					t.Errorf("tier column = %d, want %d", column, tierColumn)
				}
				tierColumn = column
				roleColumn = strings.Index(line, "writes")
				rows = append(rows, strings.Join(fields, " "))
			}
			if strings.Join(rows, "\n") != strings.Join(want, "\n") || wrapped && continuations == 0 || !wrapped && continuations != 0 {
				t.Errorf("unexpected rows or role wrapping:\n%s\nwant:\n%s", table, strings.Join(want, "\n"))
			}
		})
	}
}

func TestInitProbeAndRefusalHaveNoEscapesWithNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	agents := []string{"--agent", "harness=claude,model=sonnet", "--agent", "harness=codex,model=gpt-5", "--signed-in", "claude,codex", "--check", "true", "--tools", "none"}
	for _, tc := range []struct {
		name  string
		args  []string
		stdin string
		code  int
	}{
		{"probe", append([]string{"--probe"}, agents...), "", 0},
		{"refusal", agents, "\n\nn\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importRepo(t)
			fakeInitPATH(t, "claude", "codex")
			caps := logo.Caps{TTY: true, Color: logo.NoColor, UTF8: true, Width: 100}
			original := initDetectCaps
			initDetectCaps = func(io.Writer) logo.Caps { return caps }
			t.Cleanup(func() { initDetectCaps = original })
			code, out, errs := initDo(t, testsupport.ShortStateDir(t), tc.stdin, tc.args...)
			if code != tc.code || strings.Contains(out, "\x1b") || strings.Contains(errs, "\x1b") {
				t.Fatalf("init = %d, or escapes:\n%q\n%q", code, out, errs)
			}
			if strings.Contains(out, "To change later:") || strings.Contains(out, "ready") {
				t.Errorf("stale or premature text:\n%s", out)
			}
		})
	}
}
