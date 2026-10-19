package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

func TestInitUseModelsLists(t *testing.T) {
	// The question names the models heaviest tier first when there are at most
	// three, and the first model with a count otherwise.
	entry := func(name string) initModelEntry { return initModelEntry{name: name, value: name} }
	for _, tc := range []struct {
		models []string
		want   string
	}{
		{[]string{"haiku", "sonnet", "opus"}, "opus, sonnet, haiku"},
		{[]string{"gpt-5"}, "gpt-5"},
		{[]string{"gpt-6.1-sol", "gpt-6.1-sol-mini", "gpt-5", "gpt-5.4-mini"}, "gpt-6.1-sol and 3 more"},
	} {
		var entries []initModelEntry
		for _, model := range tc.models {
			entries = append(entries, entry(model))
		}
		if got := initUseNames("claude", entries, adapter.AgentTier); got != tc.want {
			t.Errorf("initUseNames(%v) = %q, want %q", tc.models, got, tc.want)
		}
	}

	importRepo(t)
	fakeInitPATH(t, "claude", "codex")
	catalog := `{"models":[
	 {"slug":"gpt-6.1-sol","visibility":"list","priority":1},
	 {"slug":"gpt-6.1-sol-mini","visibility":"list","priority":2},
	 {"slug":"gpt-5","visibility":"list","priority":3},
	 {"slug":"gpt-5.4-mini","visibility":"list","priority":4}]}`
	writeInitCodexScript(t, "  debug  Diagnostic tools", "  models  Render models", "", "printf '%s\\n' '"+catalog+"'", "exit 89")
	writeInitModelFixture(t, codexConfigPath(), "model = \"gpt-6.1-sol\"\n")
	// Foreman, its effort, use claude, use codex, then decline the plan.
	code, out, errOut := initFixedParallel(t, testsupport.ShortStateDir(t), "\n\n\n\nn\n", "--signed-in", "claude,codex", "--check", "true", "--tools", "none", "--no-wiring")
	if code != 1 || errOut != "" || !strings.Contains(out, "Declined") {
		t.Fatalf("init = %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{
		"\n    Use claude's models for tasks (opus, sonnet, haiku)? [Y/n]:\n",
		"\n    Use codex's models for tasks (gpt-6.1-sol and 3 more)? [Y/n]:\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	assertInitEffortOutput(t, out)
}

func TestInitOpenCodeMultiPick(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		models       []string
		tries        int
	}{
		{"two numbers", "1,3\n", []string{"fixture/gpt-5", "opencode/deepseek-v3"}, 1},
		{"spaces and a repeat", "3, 1,3\n", []string{"fixture/gpt-5", "opencode/deepseek-v3"}, 1},
		{"typed names", "zai-coding-plan/glm-5.3,opencode/deepseek-v3\n", []string{"zai-coding-plan/glm-5.3", "opencode/deepseek-v3"}, 1},
		{"number and typed name", "2,opencode/deepseek-v3\n", []string{"fixture/glm-5.3", "opencode/deepseek-v3"}, 1},
		{"dash picks none", "-\n", nil, 1},
		{"Enter picks none", "\n", nil, 1},
		{"bad number retries", "9\n1,2\n", []string{"fixture/gpt-5", "fixture/glm-5.3"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importRepo(t)
			fakeInitPATH(t, "codex", "opencode")
			path := filepath.Join(os.Getenv("PATH"), "opencode")
			script, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			script = []byte(strings.Replace(string(script), `fixture/glm-5.3\n'`, `fixture/glm-5.3\nopencode/deepseek-v3\n'`, 1))
			if err := os.WriteFile(path, script, 0755); err != nil {
				t.Fatal(err)
			}
			// Use codex (the only foreman is its default), then the picks.
			code, out, errOut := initFixedParallel(t, testsupport.ShortStateDir(t), "\n"+tc.answer+"n\n", "--signed-in", "codex", "--accept-limited", "--check", "true", "--tools", "none", "--no-wiring")
			if code != 1 || errOut != "" || !strings.Contains(out, "Declined") {
				t.Fatalf("init = %d\n%s\n%s", code, out, errOut)
			}
			if got := strings.Count(out, "    Pick numbers (1,3) or -, or type provider/model [-]:\n"); got != tc.tries {
				t.Errorf("asked %d times, want %d:\n%s", got, tc.tries, out)
			}
			if (tc.tries > 1) != strings.Contains(out, "no entry numbered 9; try again.\n") {
				t.Errorf("bad number must be refused once:\n%s", out)
			}
			plan, _, _ := strings.Cut(strings.Split(out, "\nPlan\n")[1], "  Reviews:")
			rows := 0
			for _, line := range strings.Split(plan, "\n") {
				if strings.HasPrefix(line, "    opencode ") {
					rows++
				}
			}
			if rows != len(tc.models) {
				t.Errorf("%d OpenCode agents in the plan, want %d:\n%s", rows, len(tc.models), plan)
			}
			for _, model := range tc.models {
				if !strings.Contains(plan, model) {
					t.Errorf("plan lacks %s:\n%s", model, plan)
				}
			}
		})
	}
}

func TestInitForemanOnlyAgent(t *testing.T) {
	// A foreman whose CLI is not used for tasks is still an agent of the
	// runner, with no classes, so its turns have somewhere to run.
	binary := testsupport.BuildBrigada(t)
	old := initBinary
	initBinary = binary
	t.Cleanup(func() { initBinary = old })
	importRepo(t)
	fakeInitPATH(t, "claude", "codex")
	state := testsupport.ShortStateDir(t)
	t.Cleanup(func() {
		if rec, running, _ := runnerstate.Running(filepath.Join(state, "runner")); running {
			_ = syscall.Kill(rec.PID, syscall.SIGTERM)
		}
		data, _ := os.ReadFile(lifecycle.PathsOf(state).Lock)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			_, live, _ := runnerstate.Running(filepath.Join(state, "runner"))
			if !live && !lifecycle.EngineRunning(context.Background(), lifecycle.PathsOf(state).Socket) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Error("detached children did not stop")
	})
	// Codex (entry 4) plans; claude's models are used, codex's are not; apply.
	code, out, errOut := initFixedParallel(t, state, "4\n\nn\ny\n", "--signed-in", "claude,codex", "--accept-limited", "--check", "true", "--tools", "none", "--no-wiring")
	if code != 0 || errOut != "" || !strings.Contains(out, "Detached runner started") {
		t.Fatalf("init = %d\n%s\n%s", code, out, errOut)
	}
	plan := strings.Split(out, "\nPlan\n")[1]
	flat := strings.Join(strings.Fields(plan), " ")
	for _, want := range []string{"Foreman: codex/CLI default\n", "codex CLI default OpenAI standard foreman only", "claude opus Anthropic heavy writes code, reviews"} {
		if !strings.Contains(flat, strings.ReplaceAll(want, "\n", " ")) {
			t.Errorf("plan lacks %q:\n%s", want, plan)
		}
	}
	if strings.Contains(plan, "    codex     edit files") {
		t.Errorf("a foreman-only agent was listed as a worker:\n%s", plan)
	}
	rec, live, err := runnerstate.Running(filepath.Join(state, "runner"))
	if err != nil || !live {
		t.Fatalf("runner = %+v live=%v err=%v", rec, live, err)
	}
	var foremanOnly *api.Agent
	for i, agent := range rec.Agents {
		if agent.Harness == "codex" {
			foremanOnly = &rec.Agents[i]
		}
	}
	if foremanOnly == nil || len(foremanOnly.Classes) != 0 || len(rec.Agents) != 4 {
		t.Fatalf("runner agents = %+v", rec.Agents)
	}
	assertInitSavedAgents(t, state, rec.Specs)
	if !strings.Contains(strings.Join(rec.Specs, "\n"), "harness=codex,model=default,classes=none") {
		t.Errorf("runner specs = %v", rec.Specs)
	}
	if saved, err := runnerstate.LoadForeman(filepath.Join(state, "runner")); err != nil || saved != "codex/default" {
		t.Errorf("saved foreman = %q, %v", saved, err)
	}
}
