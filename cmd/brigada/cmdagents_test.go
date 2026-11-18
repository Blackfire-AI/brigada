package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/logo"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
	"github.com/Blackfire-AI/brigada/internal/term"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

func agentsDo(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func saveAgentLines(t *testing.T, state string, lines ...string) {
	t.Helper()
	if err := runnerstate.SaveAgents(filepath.Join(state, "runner"), lines); err != nil {
		t.Fatal(err)
	}
}

func loadAgentLines(t *testing.T, state string) []string {
	t.Helper()
	lines, err := runnerstate.LoadAgents(filepath.Join(state, "runner"))
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

// fakeOpenCodeModels replaces the opencode CLI with one listing the given
// models, one per line.
func fakeOpenCodeModels(t *testing.T, models ...string) {
	t.Helper()
	fakeInitPATH(t, "opencode")
	script := "#!/bin/sh\nprintf 'opencode %s\\n' \"$*\" >> \"$INIT_CALLS\"\ncase \"$*\" in\n --help) printf '  opencode models [provider]\\n' ;;\n models) printf '%s\\n' '" + strings.Join(models, "' '") + "' ;;\n *) exit 89 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(os.Getenv("PATH"), "opencode"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
}

// The listing covers every harness with an adapter: kimi through its config
// tables, dsh through its static list, qwen-code through `qwen --list-models`.
// Only qwen-code's listing is whole enough to say a registered model is gone.
func TestListOfferedAgentsCoversKimiDshAndQwen(t *testing.T) {
	fakeInitPATH(t, "kimi", "dsh", "qwen")
	writeInitQwenScript(t, initQwenListModelsHelp, initQwenListModels)
	writeInitModelFixture(t, filepath.Join(os.Getenv("HOME"), ".kimi-code", "config.toml"),
		"default_model = \"kimi-code/k3\"\n[models.\"kimi-code/k3\"]\n[models.\"kimi-code/k2.8-preview\"]\n")
	offer := listOfferedAgents(context.Background())
	wantModels := []string{
		"kimi/kimi-code/k3", "kimi/kimi-code/k2.8-preview",
		"dsh/deepseek-flash", "dsh/deepseek-v4-pro",
		"qwen-code/qwen-subscription/qwen4-max", "qwen-code/qwen-subscription/qwen3.8-max", "qwen-code/qwen-subscription/qwen3.8-flash",
	}
	if !reflect.DeepEqual(offer.models, wantModels) {
		t.Errorf("models = %q, want %q", offer.models, wantModels)
	}
	if want := map[string]string{"kimi": "kimi-code/k3"}; !reflect.DeepEqual(offer.defaults, want) {
		t.Errorf("defaults = %v, want %v", offer.defaults, want)
	}
	if want := map[string]bool{"qwen-code": true}; !reflect.DeepEqual(offer.complete, want) {
		t.Errorf("complete = %v, want %v", offer.complete, want)
	}
}

func TestAgentsShowsUnregisteredAndGone(t *testing.T) {
	fakeInitPATH(t, "opencode")
	state := testsupport.ShortStateDir(t)
	saved := []string{"# mine", "harness=opencode,model=fixture/gpt-5,account=work", "harness=opencode,model=fixture/retired"}
	saveAgentLines(t, state, saved...)
	code, out, errOut := agentsDo(t, "agents", "--state-dir", state)
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	for _, want := range []string{
		"Offered by your CLIs, not registered (1):\n  opencode/fixture/glm-5.3  tier standard, shares account work\n",
		"Nothing is registered automatically",
		"tier.<harness>/<model>",
		"Models a newer version of the same line replaces are left out.",
		"Registered, but the CLI no longer lists the model (1):\n  opencode/fixture/retired\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "opencode/fixture/gpt-5 ") {
		t.Errorf("a registered model is listed as unregistered:\n%s", out)
	}
	if got := loadAgentLines(t, state); !reflect.DeepEqual(got, saved) {
		t.Errorf("agents file changed: %q", got)
	}
}

func TestAgentsTierFromTableAndConfig(t *testing.T) {
	fakeOpenCodeModels(t, "fixture/glm-5.3", "fixture/glm-5.3-highspeed", "fixture/glm-5.3-flash")
	writeInitModelFixture(t, lifecycle.DefaultConfigPath(), "tier.opencode/fixture/glm-5.3-flash = standard\n")
	state := testsupport.ShortStateDir(t)
	saveAgentLines(t, state, "harness=opencode,model=fixture/glm-5.3")
	code, out, errOut := agentsDo(t, "agents", "--state-dir", state)
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	// A faster serving keeps the model's tier, and the config's tier wins
	// over the table's light for flash.
	for _, want := range []string{
		"  opencode/fixture/glm-5.3-highspeed  tier standard, shares account opencode\n",
		"  opencode/fixture/glm-5.3-flash      tier standard, shares account opencode\n",
		"A tier here comes from brigada's model table unless tier.<harness>/<model> in the config file sets it.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestAgentsLeavesOutSupersededModels(t *testing.T) {
	fakeOpenCodeModels(t, "fixture/gpt-6.1-sol", "fixture/gpt-6-sol", "fixture/gpt-5.6-sol", "fixture/gpt-6-luna", "fixture/gpt-5.6-luna")
	state := testsupport.ShortStateDir(t)
	saved := []string{"harness=opencode,model=fixture/gpt-6-sol", "harness=opencode,model=fixture/gpt-6-luna"}
	saveAgentLines(t, state, saved...)
	code, out, errOut := agentsDo(t, "agents", "--state-dir", state)
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	for _, want := range []string{
		"Offered by your CLIs, not registered (1):\n  opencode/fixture/gpt-6.1-sol  tier standard",
		"Registered, but a newer version of the model is listed or registered (1):\n  opencode/fixture/gpt-6-sol  replaced by opencode/fixture/gpt-6.1-sol\n",
		"To retire one, remove its line from",
		"To move one to the newer model, run brigada agents update: it rewrites each line you accept and restarts the runner.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "gpt-5.6") {
		t.Errorf("a superseded model is offered:\n%s", out)
	}
	if got := loadAgentLines(t, state); !reflect.DeepEqual(got, saved) {
		t.Errorf("agents file changed: %q", got)
	}
}

func TestAgentsAllRegistered(t *testing.T) {
	fakeInitPATH(t, "opencode")
	state := testsupport.ShortStateDir(t)
	saveAgentLines(t, state, "harness=opencode,model=fixture/gpt-5", "harness=opencode,model=fixture/glm-5.3")
	code, out, _ := agentsDo(t, "agents", "--state-dir", state)
	if code != 0 || out != "Every model your CLIs offer is registered.\n" {
		t.Fatalf("code=%d stdout=%q", code, out)
	}
}

func TestAgentsNeedsSavedAgents(t *testing.T) {
	fakeInitPATH(t, "opencode")
	state := testsupport.ShortStateDir(t)
	for _, args := range [][]string{{"agents", "--state-dir", state}, {"agents", "add", "--state-dir", state, "claude/opus"}} {
		if code, _, errOut := agentsDo(t, args...); code != 1 || !strings.Contains(errOut, "no saved agents") {
			t.Errorf("%q: code=%d stderr=%q", args, code, errOut)
		}
	}
}

func TestAgentsRejectsArguments(t *testing.T) {
	fakeInitPATH(t, "opencode")
	state := testsupport.ShortStateDir(t)
	saveAgentLines(t, state, "harness=claude")
	if code, _, errOut := agentsDo(t, "agents", "--state-dir", state, "claude/opus"); code != 2 || !strings.Contains(errOut, "agents add") {
		t.Errorf("code=%d stderr=%q", code, errOut)
	}
}

func TestAgentsAdd(t *testing.T) {
	fakeInitPATH(t, "opencode")
	state := testsupport.ShortStateDir(t)
	saved := []string{"harness=claude,model=sonnet", "# mine", "harness=opencode,model=fixture/gpt-5"}
	saveAgentLines(t, state, saved...)

	code, out, errOut := agentsDo(t, "agents", "add", "--state-dir", state, "claude/opus", "--classes", "review,answer", "--effort", "high")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	line := "harness=claude,model=opus,classes=review+answer,effort=high"
	want := append(append([]string{}, saved...), line)
	if got := loadAgentLines(t, state); !reflect.DeepEqual(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	for _, text := range []string{"registered claude/opus (tier heavy, classes review,answer)", line, "No runner running; brigada start serves it."} {
		if !strings.Contains(out, text) {
			t.Errorf("output lacks %q:\n%s", text, out)
		}
	}

	// A provider/model name keeps its own slash; the harness is the first part.
	if code, _, errOut := agentsDo(t, "agents", "add", "--state-dir", state, "opencode/fixture/glm-5.3", "--classes", "none"); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	got := loadAgentLines(t, state)
	if last := got[len(got)-1]; last != "harness=opencode,model=fixture/glm-5.3,classes=none" || len(got) != len(want)+1 {
		t.Fatalf("lines = %q", got)
	}
}

func TestAgentsAddRefusals(t *testing.T) {
	fakeInitPATH(t, "opencode")
	state := testsupport.ShortStateDir(t)
	saved := []string{"harness=claude,model=sonnet", "harness=codex", "# mine"}
	saveAgentLines(t, state, saved...)
	for _, tc := range []struct {
		name     string
		args     []string
		code     int
		contains string
	}{
		{"already registered", []string{"claude/sonnet"}, 1, "agent claude/sonnet is already registered"},
		{"registered by its default", []string{"codex/default"}, 1, "agent codex/default is already registered"},
		{"no model", []string{"claude"}, 2, "<harness>/<model>"},
		{"nothing to add", nil, 2, "usage: brigada agents add"},
		{"two models", []string{"claude/opus", "claude/haiku"}, 2, "usage: brigada agents add"},
		{"unknown harness", []string{"nonesuch/model"}, 2, "unknown harness"},
		{"bad effort", []string{"claude/opus", "--effort", "bogus"}, 2, "effort"},
		{"no classes", []string{"claude/opus", "--classes", ","}, 2, "--classes"},
		{"model with a comma", []string{"claude/opus,effort=low"}, 2, "commas"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"agents", "add", "--state-dir", state}, tc.args...)
			code, out, errOut := agentsDo(t, args...)
			if code != tc.code || out != "" || !strings.Contains(errOut, tc.contains) {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
			}
			if got := loadAgentLines(t, state); !reflect.DeepEqual(got, saved) {
				t.Fatalf("agents file changed: %q", got)
			}
		})
	}
}

func TestAgentsAddQwenCodeDefaultsToEveryClass(t *testing.T) {
	fakeInitPATH(t, "opencode")
	state := testsupport.ShortStateDir(t)
	saveAgentLines(t, state, "harness=claude,model=sonnet")

	code, out, errOut := agentsDo(t, "agents", "add", "--state-dir", state, "qwen-code/qwen3.8-max")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	got := loadAgentLines(t, state)
	if last := got[len(got)-1]; last != "harness=qwen-code,model=qwen3.8-max,classes=implement+review+answer" {
		t.Fatalf("lines = %q", got)
	}
	if !strings.Contains(out, "registered qwen-code/qwen3.8-max (tier standard, classes implement,review,answer)") {
		t.Errorf("output = %s", out)
	}
	// An explicit review class is accepted too.
	if code, out, errOut := agentsDo(t, "agents", "add", "--state-dir", state, "qwen-code/qwen3.8-flash", "--classes", "review"); code != 0 || errOut != "" || !strings.Contains(out, "classes review") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if got := loadAgentLines(t, state); len(got) != 3 {
		t.Fatalf("lines = %q", got)
	}
}

func TestAgentsAddCarriesTheHarnessAccount(t *testing.T) {
	fakeInitPATH(t, "opencode")
	state := testsupport.ShortStateDir(t)
	saveAgentLines(t, state, "harness=opencode,model=fixture/gpt-5,account=work", "harness=claude,model=sonnet")
	if code, out, errOut := agentsDo(t, "agents", "add", "--state-dir", state, "opencode/fixture/glm-5.3"); code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if code, out, errOut := agentsDo(t, "agents", "add", "--state-dir", state, "claude/opus"); code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	got := loadAgentLines(t, state)
	want := []string{
		"harness=opencode,model=fixture/gpt-5,account=work",
		"harness=claude,model=sonnet",
		"harness=opencode,model=fixture/glm-5.3,classes=implement+review+answer,account=work",
		"harness=claude,model=opus,classes=implement+review+answer",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
}

// A listing that left out a model of unknown family cannot show that a
// registered model is gone.
func TestAgentsUnknownFamilyListingNeverCallsAModelGone(t *testing.T) {
	fakeOpenCodeModels(t, "fixture/glm-5.3", "fixture/unrecognized-model")
	state := testsupport.ShortStateDir(t)
	saveAgentLines(t, state, "harness=opencode,model=fixture/glm-5.3", "harness=opencode,model=fixture/unrecognized-model", "harness=opencode,model=fixture/retired")
	code, out, errOut := agentsDo(t, "agents", "--state-dir", state)
	if code != 0 || errOut != "" || strings.Contains(out, "no longer lists") || strings.Contains(out, "unrecognized-model") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}

func TestAgentsHintReadsTheCache(t *testing.T) {
	calls := fakeInitPATH(t, "opencode")
	state := testsupport.ShortStateDir(t)
	rt := &runtime{stateDir: state, paths: lifecycle.PathsOf(state)}
	ctx := context.Background()
	called := func() int {
		data, _ := os.ReadFile(calls)
		return strings.Count(string(data), "\n")
	}

	// Without saved agents there is nothing to compare and no CLI runs.
	if hint := agentsHint(ctx, rt, true); hint != "" || called() != 0 {
		t.Fatalf("no agents file: hint=%q, %d CLI calls", hint, called())
	}
	saveAgentLines(t, state, "harness=opencode,model=fixture/gpt-5")
	// Without a cache, a caller that must not refresh gets no hint and no CLI runs.
	if hint := agentsHint(ctx, rt, false); hint != "" || called() != 0 {
		t.Fatalf("no cache, no refresh: hint=%q, %d CLI calls", hint, called())
	}
	const want = "1 model your CLIs offer but no agent registers: brigada agents"
	if hint := agentsHint(ctx, rt, true); hint != want {
		t.Fatalf("hint = %q, want %q", hint, want)
	}
	first := called()
	if first == 0 {
		t.Fatal("the first hint did not list the models")
	}
	// A fresh cache is read, never refreshed.
	if hint := agentsHint(ctx, rt, true); hint != want || called() != first {
		t.Fatalf("fresh cache: hint=%q, %d CLI calls, want %d", hint, called(), first)
	}
	if hint := agentsHint(ctx, rt, false); hint != want || called() != first {
		t.Fatalf("no refresh: hint=%q, %d CLI calls, want %d", hint, called(), first)
	}
	// An expired cache is listed again.
	cache, ok := readAgentsCache(rt)
	if !ok {
		t.Fatal("no cache written")
	}
	cache.Checked = time.Now().Add(-2 * agentsCacheTTL)
	data, err := json.Marshal(cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agentsCachePath(rt), data, 0600); err != nil {
		t.Fatal(err)
	}
	if hint := agentsHint(ctx, rt, true); hint != want || called() == first {
		t.Fatalf("expired cache: hint=%q, %d CLI calls, want more than %d", hint, called(), first)
	}
	// Registering the model ends the hint without another listing.
	after := called()
	saveAgentLines(t, state, "harness=opencode,model=fixture/gpt-5", "harness=opencode,model=fixture/glm-5.3")
	if hint := agentsHint(ctx, rt, true); hint != "" || called() != after {
		t.Fatalf("all registered: hint=%q, %d CLI calls, want %d", hint, called(), after)
	}
}

// The hint also counts registered models a newer version replaces, pointing
// at brigada agents update; the cache is written, so no CLI ever runs.
func TestAgentsHintCountsSuperseded(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	rt := &runtime{stateDir: state, paths: lifecycle.PathsOf(state)}
	ctx := context.Background()

	// Only superseded pairs: the new sentence, singular, on its own.
	saveAgentLines(t, state, "harness=claude,model=claude-fable-5-1[1m]", "harness=claude,model=claude-fable-5-2[1m]")
	writeAgentsCache(rt, agentsOffer{models: []string{"claude/claude-fable-5-2[1m]"}}.cache(time.Now()))
	const wantSuperseded = "1 agent runs a model a newer version replaces: brigada agents update"
	if hint := agentsHint(ctx, rt, false); hint != wantSuperseded {
		t.Fatalf("hint = %q, want %q", hint, wantSuperseded)
	}

	// Both counts: unregistered first, then superseded.
	saveAgentLines(t, state,
		"harness=claude,model=claude-fable-5-1[1m]", "harness=claude,model=claude-fable-5-2[1m]",
		"harness=codex,model=gpt-5", "harness=codex,model=gpt-5.5")
	writeAgentsCache(rt, agentsOffer{models: []string{"claude/claude-fable-5-2[1m]", "claude/opus", "codex/gpt-5.5"}}.cache(time.Now()))
	const wantBoth = "1 model your CLIs offer but no agent registers: brigada agents\n" + "2 agents run a model a newer version replaces: brigada agents update"
	if hint := agentsHint(ctx, rt, false); hint != wantBoth {
		t.Fatalf("hint = %q, want %q", hint, wantBoth)
	}

	// Unregistered only: today's string, unchanged.
	saveAgentLines(t, state, "harness=claude,model=sonnet")
	writeAgentsCache(rt, agentsOffer{models: []string{"claude/sonnet", "claude/opus"}}.cache(time.Now()))
	const want = "1 model your CLIs offer but no agent registers: brigada agents"
	if hint := agentsHint(ctx, rt, false); hint != want {
		t.Fatalf("hint = %q, want %q", hint, want)
	}
}

func TestAgentsListRefreshesTheCache(t *testing.T) {
	fakeInitPATH(t, "opencode")
	state := testsupport.ShortStateDir(t)
	saveAgentLines(t, state, "harness=opencode,model=fixture/gpt-5")
	if code, _, errOut := agentsDo(t, "agents", "--state-dir", state); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	cache, ok := readAgentsCache(&runtime{stateDir: state})
	if !ok || !reflect.DeepEqual(cache.Models, []string{"opencode/fixture/gpt-5", "opencode/fixture/glm-5.3"}) || time.Since(cache.Checked) > time.Minute {
		t.Fatalf("cache = %+v, %v", cache, ok)
	}
}

func TestDiffAgentsCoversTheDefaultModel(t *testing.T) {
	offer := agentsOffer{
		models:   []string{"codex/gpt-6", "codex/o3", "claude/opus"},
		defaults: map[string]string{"codex": "gpt-6"},
		complete: map[string]bool{"codex": true},
	}
	registered := registeredAgents([]string{"harness=codex,model=default", "harness=codex,model=gpt-4", "harness=claude,model=sonnet", "junk"})
	got := diffAgents(offer, registered)
	if want := []string{"codex/o3", "claude/opus"}; !reflect.DeepEqual(got.unregistered, want) {
		t.Errorf("unregistered = %q, want %q", got.unregistered, want)
	}
	// Claude's list is not whole, so its sonnet is never called gone.
	if want := []string{"codex/gpt-4"}; !reflect.DeepEqual(got.gone, want) {
		t.Errorf("gone = %q, want %q", got.gone, want)
	}
}

func TestTopHeaderShowsTheAgentsHint(t *testing.T) {
	style := term.New(logo.Caps{})
	hint := "2 models your CLIs offer but no agent registers: brigada agents"
	lines := topHint(topModel{hint: hint}, style, 80)
	if len(lines) != 1 || lines[0] != hint {
		t.Fatalf("hint = %q", lines)
	}
	if lines := topHint(topModel{}, style, 80); len(lines) != 0 {
		t.Fatalf("hint without one = %q", lines)
	}
	if rows := topHeader(topModel{hint: hint}, time.Now(), style, 80); len(rows) != 1 {
		t.Fatalf("header = %q, want the hint under the meters row, not in it", rows)
	}
}

// The hint can carry two lines; top renders one Dim row per line, each cut
// to the width.
func TestTopHintRendersEachHintLine(t *testing.T) {
	style := term.New(logo.Caps{})
	first := "1 model your CLIs offer but no agent registers: brigada agents"
	second := "1 agent runs a model a newer version replaces: brigada agents update"
	lines := topHint(topModel{hint: first + "\n" + second}, style, 80)
	if len(lines) != 2 || lines[0] != first || lines[1] != second {
		t.Fatalf("hint = %q, want [%q %q]", lines, first, second)
	}
	if lines := topHint(topModel{hint: first + "\n" + second}, style, 40); len(lines) != 2 ||
		lines[0] != cutLine(first, 40) || lines[1] != cutLine(second, 40) {
		t.Fatalf("narrow hint = %q, want each line cut to 40", lines)
	}
}

func TestDiffAgentsReportsSupersededRegistered(t *testing.T) {
	offer := agentsOffer{
		models:   []string{"claude/claude-fable-5-2[1m]", "codex/gpt-6.1-sol"},
		complete: map[string]bool{"codex": true},
	}
	// Claude's list is not whole, so its older model is superseded, not gone;
	// codex's whole list no longer has gpt-5.6-sol, so it is gone.
	registered := registeredAgents([]string{"harness=claude,model=claude-fable-5-1[1m]", "harness=claude,model=fable", "harness=codex,model=gpt-5.6-sol"})
	got := diffAgents(offer, registered)
	if want := map[string]string{"claude/claude-fable-5-1[1m]": "claude/claude-fable-5-2[1m]"}; !reflect.DeepEqual(got.superseded, want) {
		t.Errorf("superseded = %q, want %q", got.superseded, want)
	}
	if want := []string{"codex/gpt-5.6-sol"}; !reflect.DeepEqual(got.gone, want) {
		t.Errorf("gone = %q, want %q", got.gone, want)
	}
	if want := []string{"claude/claude-fable-5-2[1m]", "codex/gpt-6.1-sol"}; !reflect.DeepEqual(got.unregistered, want) {
		t.Errorf("unregistered = %q, want %q", got.unregistered, want)
	}
}

// stubAgentsStdin feeds the update prompts the given answers.
func stubAgentsStdin(t *testing.T, input string) {
	t.Helper()
	old := agentsStdin
	agentsStdin = strings.NewReader(input)
	t.Cleanup(func() { agentsStdin = old })
}

func TestAgentsUpdateAppliesWithYes(t *testing.T) {
	fakeOpenCodeModels(t, "fixture/gpt-6-sol", "fixture/gpt-6.1-sol")
	state := testsupport.ShortStateDir(t)
	saved := []string{"# mine", "harness=opencode,model=fixture/gpt-6-sol,classes=review,account=work,effort=high", "harness=opencode,model=fixture/glm-5.3", "not an agent line"}
	saveAgentLines(t, state, saved...)

	code, out, errOut := agentsDo(t, "agents", "update", "--state-dir", state, "--yes")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	// Only the superseded line changes: classes, account, effort, the
	// comment, the unparsable line and the order stay.
	want := []string{"# mine", "harness=opencode,model=fixture/gpt-6.1-sol,classes=review,account=work,effort=high", "harness=opencode,model=fixture/glm-5.3", "not an agent line"}
	if got := loadAgentLines(t, state); !reflect.DeepEqual(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	for _, text := range []string{
		"opencode/fixture/gpt-6-sol → opencode/fixture/gpt-6.1-sol\n  harness=opencode,model=fixture/gpt-6.1-sol,classes=review,account=work,effort=high\n",
		"updated opencode/fixture/gpt-6-sol → opencode/fixture/gpt-6.1-sol\n",
		"rewritten in ",
		"No runner running; brigada start serves it.",
	} {
		if !strings.Contains(out, text) {
			t.Errorf("output lacks %q:\n%s", text, out)
		}
	}
}

// qwen-code's own listing drives the upgrade: a registered qwen3.8-max moves
// to the listed qwen4-max, the flash line untouched.
func TestAgentsUpdateAppliesQwenSuperseded(t *testing.T) {
	fakeInitPATH(t, "qwen")
	writeInitQwenScript(t, initQwenListModelsHelp, initQwenListModels)
	state := testsupport.ShortStateDir(t)
	saveAgentLines(t, state, "harness=qwen-code,model=qwen-subscription/qwen3.8-max,classes=review,account=work")

	code, out, errOut := agentsDo(t, "agents", "update", "--state-dir", state, "--yes")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	want := []string{"harness=qwen-code,model=qwen-subscription/qwen4-max,classes=review,account=work"}
	if got := loadAgentLines(t, state); !reflect.DeepEqual(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	for _, text := range []string{
		"qwen-code/qwen-subscription/qwen3.8-max → qwen-code/qwen-subscription/qwen4-max\n",
		"updated qwen-code/qwen-subscription/qwen3.8-max → qwen-code/qwen-subscription/qwen4-max\n",
	} {
		if !strings.Contains(out, text) {
			t.Errorf("output lacks %q:\n%s", text, out)
		}
	}
}

func TestAgentsUpdateDeclines(t *testing.T) {
	for name, input := range map[string]string{"n": "n\n", "enter": "\n", "eof": ""} {
		t.Run(name, func(t *testing.T) {
			fakeOpenCodeModels(t, "fixture/gpt-6-sol", "fixture/gpt-6.1-sol")
			state := testsupport.ShortStateDir(t)
			saved := []string{"harness=opencode,model=fixture/gpt-6-sol"}
			saveAgentLines(t, state, saved...)
			stubAgentsStdin(t, input)
			code, out, errOut := agentsDo(t, "agents", "update", "--state-dir", state)
			if code != 0 || errOut != "" || !strings.Contains(out, "Nothing updated.") {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
			}
			if got := loadAgentLines(t, state); !reflect.DeepEqual(got, saved) {
				t.Fatalf("agents file changed: %q", got)
			}
		})
	}
}

func TestAgentsUpdateAcceptAllAndPerAnswer(t *testing.T) {
	offered := []string{"fixture/gpt-6-sol", "fixture/gpt-6.1-sol", "fixture/glm-5.3", "fixture/glm-5.4"}
	// Sorted order asks for glm first; the answer to it governs the rest.
	for name, tc := range map[string]struct {
		input string
		want  []string
	}{
		"all":        {"a\n", []string{"harness=opencode,model=fixture/gpt-6.1-sol,classes=implement+review+answer", "harness=opencode,model=fixture/glm-5.4,classes=implement+review+answer"}},
		"first only": {"y\nn\n", []string{"harness=opencode,model=fixture/gpt-6-sol", "harness=opencode,model=fixture/glm-5.4,classes=implement+review+answer"}},
	} {
		t.Run(name, func(t *testing.T) {
			fakeOpenCodeModels(t, offered...)
			state := testsupport.ShortStateDir(t)
			saveAgentLines(t, state, "harness=opencode,model=fixture/gpt-6-sol", "harness=opencode,model=fixture/glm-5.3")
			stubAgentsStdin(t, tc.input)
			code, out, errOut := agentsDo(t, "agents", "update", "--state-dir", state)
			if code != 0 || errOut != "" {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
			}
			if got := loadAgentLines(t, state); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("lines = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAgentsUpdateDryRun(t *testing.T) {
	fakeOpenCodeModels(t, "fixture/gpt-6-sol", "fixture/gpt-6.1-sol")
	state := testsupport.ShortStateDir(t)
	saved := []string{"harness=opencode,model=fixture/gpt-6-sol"}
	saveAgentLines(t, state, saved...)
	stubAgentsStdin(t, "") // a dry run reads no answers
	code, out, errOut := agentsDo(t, "agents", "update", "--state-dir", state, "--dry-run")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	for _, text := range []string{
		"opencode/fixture/gpt-6-sol → opencode/fixture/gpt-6.1-sol",
		"harness=opencode,model=fixture/gpt-6.1-sol,classes=implement+review+answer",
		"Dry run",
	} {
		if !strings.Contains(out, text) {
			t.Errorf("output lacks %q:\n%s", text, out)
		}
	}
	if strings.Contains(out, "updated ") || strings.Contains(out, "Nothing updated.") {
		t.Errorf("a dry run reported a change:\n%s", out)
	}
	if got := loadAgentLines(t, state); !reflect.DeepEqual(got, saved) {
		t.Fatalf("agents file changed: %q", got)
	}
}

func TestAgentsUpdateNothingSuperseded(t *testing.T) {
	fakeOpenCodeModels(t, "fixture/gpt-5")
	state := testsupport.ShortStateDir(t)
	saved := []string{"harness=opencode,model=fixture/gpt-5"}
	saveAgentLines(t, state, saved...)
	code, out, errOut := agentsDo(t, "agents", "update", "--state-dir", state)
	if code != 0 || errOut != "" || out != "No agent runs a model a newer version replaces.\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if got := loadAgentLines(t, state); !reflect.DeepEqual(got, saved) {
		t.Fatalf("agents file changed: %q", got)
	}
}

func TestAgentsUpdateRefusesAnAlreadyRegisteredNewPair(t *testing.T) {
	fakeOpenCodeModels(t, "fixture/gpt-6-sol", "fixture/gpt-6.1-sol")
	state := testsupport.ShortStateDir(t)
	saved := []string{"harness=opencode,model=fixture/gpt-6-sol", "harness=opencode,model=fixture/gpt-6.1-sol"}
	saveAgentLines(t, state, saved...)
	code, out, errOut := agentsDo(t, "agents", "update", "--state-dir", state, "--yes")
	if code != 1 || out != "" || !strings.Contains(errOut, "agent opencode/fixture/gpt-6.1-sol is already registered") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if got := loadAgentLines(t, state); !reflect.DeepEqual(got, saved) {
		t.Fatalf("agents file changed: %q", got)
	}
}

func TestAgentsUpdateRefusesTwoCandidatesUpdatingToTheSameTarget(t *testing.T) {
	fakeOpenCodeModels(t, "fixture/gpt-5.9-sol", "fixture/gpt-6-sol", "fixture/gpt-6.1-sol")
	state := testsupport.ShortStateDir(t)
	saved := []string{"harness=opencode,model=fixture/gpt-5.9-sol", "harness=opencode,model=fixture/gpt-6-sol"}
	saveAgentLines(t, state, saved...)
	code, out, errOut := agentsDo(t, "agents", "update", "--state-dir", state, "--yes")
	if code != 1 || out != "" || !strings.Contains(errOut, "opencode/fixture/gpt-5.9-sol and opencode/fixture/gpt-6-sol both update to opencode/fixture/gpt-6.1-sol; remove one line and run again") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if got := loadAgentLines(t, state); !reflect.DeepEqual(got, saved) {
		t.Fatalf("agents file changed: %q", got)
	}
}

func TestAgentsUpdateEffortFallsBackToTheCLIDefault(t *testing.T) {
	fakeOpenCodeModels(t, "fixture/gpt-6-sol", "fixture/gpt-6.1-sol")
	state := testsupport.ShortStateDir(t)
	// HIGH is not a variant opencode accepts; the rewrite drops it.
	saveAgentLines(t, state, "harness=opencode,model=fixture/gpt-6-sol,effort=HIGH")
	code, out, errOut := agentsDo(t, "agents", "update", "--state-dir", state, "--yes")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	want := []string{"harness=opencode,model=fixture/gpt-6.1-sol,classes=implement+review+answer"}
	if got := loadAgentLines(t, state); !reflect.DeepEqual(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	if !strings.Contains(out, "effort=HIGH is not accepted for opencode/fixture/gpt-6.1-sol") {
		t.Errorf("output lacks the effort fallback note:\n%s", out)
	}
}

func TestAgentsUpdateNeedsSavedAgents(t *testing.T) {
	fakeInitPATH(t, "opencode")
	state := testsupport.ShortStateDir(t)
	if code, _, errOut := agentsDo(t, "agents", "update", "--state-dir", state); code != 1 || !strings.Contains(errOut, "no saved agents") {
		t.Errorf("code=%d stderr=%q", code, errOut)
	}
}

func TestAgentsUpdateRejectsArguments(t *testing.T) {
	fakeInitPATH(t, "opencode")
	state := testsupport.ShortStateDir(t)
	saveAgentLines(t, state, "harness=claude")
	for _, args := range [][]string{{"extra"}, {"--bogus"}} {
		if code, _, errOut := agentsDo(t, append([]string{"agents", "update", "--state-dir", state}, args...)...); code != 2 || errOut == "" {
			t.Errorf("%q: code=%d stderr=%q", args, code, errOut)
		}
	}
}
