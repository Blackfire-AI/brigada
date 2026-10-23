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
		"-highspeed is a faster serving of the same model, not a light one",
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

func TestAgentsNameTierIsAGuess(t *testing.T) {
	fakeOpenCodeModels(t, "fixture/glm-5.3", "fixture/glm-5.3-highspeed")
	state := testsupport.ShortStateDir(t)
	saveAgentLines(t, state, "harness=opencode,model=fixture/glm-5.3")
	code, out, errOut := agentsDo(t, "agents", "--state-dir", state)
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if !strings.Contains(out, "  opencode/fixture/glm-5.3-highspeed  tier light, shares account opencode\n") ||
		!strings.Contains(out, "A tier here is a guess from the model name") {
		t.Errorf("output = %s", out)
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
		models:   []string{"codex/gpt-6", "codex/gpt-5", "claude/opus"},
		defaults: map[string]string{"codex": "gpt-6"},
		complete: map[string]bool{"codex": true},
	}
	registered := registeredAgents([]string{"harness=codex,model=default", "harness=codex,model=gpt-4", "harness=claude,model=sonnet", "junk"})
	got := diffAgents(offer, registered)
	if want := []string{"codex/gpt-5", "claude/opus"}; !reflect.DeepEqual(got.unregistered, want) {
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
	lines := topHeader(topModel{hint: hint}, time.Now(), style, 80)
	if len(lines) != 2 || lines[1] != hint {
		t.Fatalf("header = %q", lines)
	}
	if lines := topHeader(topModel{}, time.Now(), style, 80); len(lines) != 1 {
		t.Fatalf("header without a hint = %q", lines)
	}
}
