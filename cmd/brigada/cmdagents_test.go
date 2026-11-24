package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/logo"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
	"github.com/Blackfire-AI/brigada/internal/store"
	"github.com/Blackfire-AI/brigada/internal/term"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// noEngine makes the engine unreachable for the test: connectAgents fails
// as it does when no engine can be started, its reason left out so a test
// can still demand an empty stderr. The commands answer as they do with no
// engine to ask.
func noEngine(t *testing.T) {
	t.Helper()
	old := connectAgents
	connectAgents = func(context.Context, *runtime, string, bool, io.Writer) (*api.Client, int) { return nil, 1 }
	t.Cleanup(func() { connectAgents = old })
}

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
	// A live directory, so shell getcwd noise from a deleted one cannot mark
	// the qwen listing not whole.
	t.Chdir(t.TempDir())
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

// A whole qwen-code listing is the one case that reports a registered model
// gone: every listed row parsed and every family was known.
func TestAgentsQwenWholeListingReportsGone(t *testing.T) {
	// A live directory, so shell getcwd noise from a deleted one cannot mark
	// the listing not whole.
	t.Chdir(t.TempDir())
	fakeInitPATH(t, "qwen")
	writeInitQwenScript(t, initQwenListModelsHelp, `printf '%s\n' 'MODEL' 'Qwen4-Max (qwen-subscription/qwen4-max)'`)
	state := testsupport.ShortStateDir(t)
	saveAgentLines(t, state, "harness=qwen-code,model=qwen-subscription/qwen3.8-max")
	lines, err := runnerstate.LoadAgents(filepath.Join(state, "runner"))
	if err != nil {
		t.Fatal(err)
	}
	offer := listOfferedAgents(context.Background())
	d := diffAgents(offer, registeredAgents(lines))
	if want := []string{"qwen-code/qwen-subscription/qwen3.8-max"}; !reflect.DeepEqual(d.gone, want) {
		t.Errorf("gone = %q, want %q", d.gone, want)
	}
	if !offer.complete["qwen-code"] {
		t.Errorf("complete[qwen-code] = false, want true")
	}
}

// One row the listing cannot parse, or one of an unknown family, leaves the
// listing not whole: a registered model it omits stays, never gone.
func TestAgentsQwenPartialListingNeverReportsGone(t *testing.T) {
	for name, listing := range map[string]string{
		"unknown family": initQwenListModels + `; printf '%s\n' 'Outside (vendor/omni-x)'`,
		"unparsed row":   initQwenListModels + `; printf '%s\n' 'Qwen3.8-Max (qwen-subscription/qwen3.8-max,extra)'`,
	} {
		t.Run(name, func(t *testing.T) {
			fakeInitPATH(t, "qwen")
			writeInitQwenScript(t, initQwenListModelsHelp, listing)
			state := testsupport.ShortStateDir(t)
			saveAgentLines(t, state, "harness=qwen-code,model=qwen-subscription/qwen3.7-max")
			lines, err := runnerstate.LoadAgents(filepath.Join(state, "runner"))
			if err != nil {
				t.Fatal(err)
			}
			offer := listOfferedAgents(context.Background())
			d := diffAgents(offer, registeredAgents(lines))
			if len(d.gone) != 0 {
				t.Errorf("gone = %q, want none", d.gone)
			}
			if offer.complete["qwen-code"] {
				t.Errorf("complete[qwen-code] = true, want false")
			}
		})
	}
}

// A qwen-code listing that fell back to the static pair is not whole: it
// never reports a registered model gone.
func TestAgentsQwenStaticFallbackNeverReportsGone(t *testing.T) {
	fakeInitPATH(t, "qwen")
	writeInitQwenScript(t, `printf '%s\n' '  --model <model>  Model for the current session'`, initQwenListModels)
	state := testsupport.ShortStateDir(t)
	saveAgentLines(t, state,
		"harness=qwen-code,model=qwen-subscription/qwen3.8-max",
		"harness=qwen-code,model=qwen-subscription/qwen3.7-max")
	lines, err := runnerstate.LoadAgents(filepath.Join(state, "runner"))
	if err != nil {
		t.Fatal(err)
	}
	offer := listOfferedAgents(context.Background())
	d := diffAgents(offer, registeredAgents(lines))
	if len(d.gone) != 0 {
		t.Errorf("gone = %q, want none", d.gone)
	}
	if offer.complete["qwen-code"] {
		t.Errorf("the static fallback claims a whole listing")
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
		"Offered by your harnesses (2):\n",
		"  opencode/fixture/gpt-5    registered      tier standard, shares account work\n",
		"  opencode/fixture/glm-5.3  not registered  tier standard, shares account work\n",
		"Nothing is registered automatically",
		"tier.<harness>/<model>",
		"Registered, but the CLI no longer lists the model (1):\n  opencode/fixture/retired\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "superseded") {
		t.Errorf("nothing is superseded here:\n%s", out)
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
		"  opencode/fixture/glm-5.3            registered      tier standard, shares account opencode\n",
		"  opencode/fixture/glm-5.3-flash      not registered  tier standard, shares account opencode\n",
		"  opencode/fixture/glm-5.3-highspeed  not registered  tier standard, shares account opencode\n",
		"A tier here comes from brigada's model table unless tier.<harness>/<model> in the config file sets it.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestAgentsMarksSupersededModels(t *testing.T) {
	fakeOpenCodeModels(t, "fixture/gpt-6.1-sol", "fixture/gpt-6-sol", "fixture/gpt-5.6-sol", "fixture/gpt-6-luna", "fixture/gpt-5.6-luna")
	state := testsupport.ShortStateDir(t)
	saved := []string{"harness=opencode,model=fixture/gpt-6-sol", "harness=opencode,model=fixture/gpt-6-luna"}
	saveAgentLines(t, state, saved...)
	code, out, errOut := agentsDo(t, "agents", "--state-dir", state)
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	for _, want := range []string{
		"Offered by your harnesses (5):\n",
		"  opencode/fixture/gpt-6.1-sol   not registered  tier standard, shares account opencode\n",
		"  opencode/fixture/gpt-6-sol     superseded      tier standard, shares account opencode  (replaced by opencode/fixture/gpt-6.1-sol)\n",
		"  opencode/fixture/gpt-5.6-sol   superseded      tier standard, shares account opencode  (replaced by opencode/fixture/gpt-6.1-sol)\n",
		"  opencode/fixture/gpt-6-luna    registered      tier light, shares account opencode\n",
		"  opencode/fixture/gpt-5.6-luna  superseded      tier light, shares account opencode  (replaced by opencode/fixture/gpt-6-luna)\n",
		"A superseded model is one a newer version of the same line replaces: brigada agents update moves a registered one to the newer model; brigada agents remove retires one.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
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
	if code != 0 {
		t.Fatalf("code=%d stdout=%q", code, out)
	}
	for _, want := range []string{
		"Offered by your harnesses (2):",
		"  opencode/fixture/gpt-5    registered",
		"  opencode/fixture/glm-5.3  registered",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "not registered") || strings.Contains(out, "superseded") {
		t.Errorf("every offered model is registered:\n%s", out)
	}
}

func TestAgentsNeedsSavedAgents(t *testing.T) {
	fakeInitPATH(t, "opencode")
	state := testsupport.ShortStateDir(t)
	for _, args := range [][]string{{"agents", "--state-dir", state}, {"agents", "add", "--state-dir", state, "claude/opus"}, {"agents", "remove", "--state-dir", state, "claude/opus"}} {
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

func TestAgentsRemove(t *testing.T) {
	fakeInitPATH(t, "opencode")
	noEngine(t)
	state := testsupport.ShortStateDir(t)
	saveAgentLines(t, state,
		"# mine",
		"harness=claude,model=sonnet",
		"harness=kimi,model=kimi-code/k3.1-pro-highspeed",
		"harness=kimi,model=kimi-code/k3.1-pro-highspeed",
		"harness=opencode,model=fixture/glm-5.3",
	)

	code, out, errOut := agentsDo(t, "agents", "remove", "--state-dir", state, "kimi/kimi-code/k3.1-pro-highspeed")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	want := []string{"# mine", "harness=claude,model=sonnet", "harness=opencode,model=fixture/glm-5.3"}
	if got := loadAgentLines(t, state); !reflect.DeepEqual(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	for _, text := range []string{
		"removed kimi/kimi-code/k3.1-pro-highspeed",
		"removed 2 copies from ",
		"If this agent was a project's foreman, its turns wait until another registered agent offers the pair",
		"No runner running; brigada start serves it.",
	} {
		if !strings.Contains(out, text) {
			t.Errorf("output lacks %q:\n%s", text, out)
		}
	}

	// A provider/model name keeps its own slash; the harness is the first part.
	if code, out, errOut = agentsDo(t, "agents", "remove", "--state-dir", state, "opencode/fixture/glm-5.3"); code != 0 || errOut != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	want = []string{"# mine", "harness=claude,model=sonnet"}
	if got := loadAgentLines(t, state); !reflect.DeepEqual(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	if !strings.Contains(out, "removed from ") || strings.Contains(out, "copies") {
		t.Errorf("output = %s", out)
	}

	// A line with no model registers the harness's default pair, and removing
	// by that pair deletes the line.
	saveAgentLines(t, state, want[0], want[1], "harness=codex")
	if code, out, errOut = agentsDo(t, "agents", "remove", "--state-dir", state, "claude/sonnet"); code != 0 || errOut != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if got := loadAgentLines(t, state); !reflect.DeepEqual(got, []string{"# mine", "harness=codex"}) {
		t.Fatalf("lines = %q", got)
	}
}

func TestAgentsRemoveRefusals(t *testing.T) {
	fakeInitPATH(t, "opencode")
	state := testsupport.ShortStateDir(t)
	saved := []string{"harness=claude,model=sonnet", "harness=kimi,model=kimi-code/k3.1-pro", "# mine"}
	saveAgentLines(t, state, saved...)
	for _, tc := range []struct {
		name     string
		args     []string
		code     int
		contains string
	}{
		{"not registered", []string{"claude/opus"}, 1, "agent claude/opus is not registered; brigada capacity lists the registered agents"},
		{"short form", []string{"kimi/k3.1-pro"}, 1, "the saved pair is kimi/kimi-code/k3.1-pro"},
		{"unknown harness", []string{"nonesuch/model"}, 1, "agent nonesuch/model is not registered"},
		{"no model", []string{"claude"}, 2, "<harness>/<model>"},
		{"nothing to remove", nil, 2, "usage: brigada agents remove"},
		{"two models", []string{"claude/opus", "claude/haiku"}, 2, "usage: brigada agents remove"},
		{"model with a comma", []string{"claude/opus,effort=low"}, 2, "commas"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"agents", "remove", "--state-dir", state}, tc.args...)
			code, out, errOut := agentsDo(t, args...)
			if code != tc.code || out != "" || !strings.Contains(errOut, tc.contains) {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
			}
			if got := loadAgentLines(t, state); !reflect.DeepEqual(got, saved) {
				t.Fatalf("agents file changed: %q", got)
			}
		})
	}
	// The last registered agent stays: the runner would have nothing to
	// serve, and the file stays as it was.
	state = testsupport.ShortStateDir(t)
	saveAgentLines(t, state, "harness=claude,model=sonnet", "# mine")
	if code, out, errOut := agentsDo(t, "agents", "remove", "--state-dir", state, "claude/sonnet"); code != 1 || !strings.Contains(errOut, "would leave no agent for the runner to serve") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if got := loadAgentLines(t, state); !reflect.DeepEqual(got, []string{"harness=claude,model=sonnet", "# mine"}) {
		t.Fatalf("agents file changed: %q", got)
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
	const want = "1 model your harnesses offer but no agent registers: brigada agents"
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
	const wantBoth = "1 model your harnesses offer but no agent registers: brigada agents\n" + "2 agents run a model a newer version replaces: brigada agents update"
	if hint := agentsHint(ctx, rt, false); hint != wantBoth {
		t.Fatalf("hint = %q, want %q", hint, wantBoth)
	}

	// Unregistered only: today's string, unchanged.
	saveAgentLines(t, state, "harness=claude,model=sonnet")
	writeAgentsCache(rt, agentsOffer{models: []string{"claude/sonnet", "claude/opus"}}.cache(time.Now()))
	const want = "1 model your harnesses offer but no agent registers: brigada agents"
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
	hint := "2 models your harnesses offer but no agent registers: brigada agents"
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
	first := "1 model your harnesses offer but no agent registers: brigada agents"
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
	noEngine(t)
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
	noEngine(t)
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
			noEngine(t)
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
			noEngine(t)
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
	noEngine(t)
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
	noEngine(t)
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

// agentsTestForeman is one project's foreman as a test chooses it.
type agentsTestForeman struct {
	repo, harness, model, effort string
}

// startAgentsEngine starts the engine in-process with each foreman chosen
// in order — the projects list oldest first — and returns the client that
// chose them.
func startAgentsEngine(t *testing.T, foremen ...agentsTestForeman) (context.Context, string, *api.Client) {
	t.Helper()
	ctx, state := startSettingsEngine(t)
	_, token, err := lifecycle.TokenForRole(lifecycle.PathsOf(state).Tokens, "client")
	if err != nil {
		t.Fatal(err)
	}
	client := api.NewClient(lifecycle.PathsOf(state).Socket, token)
	for _, f := range foremen {
		if _, err := client.SetProjectForeman(ctx, f.repo, f.harness, f.model, f.effort); err != nil {
			t.Fatal(err)
		}
	}
	return ctx, state, client
}

// foremanOf is the model and effort the engine holds for repo.
func foremanOf(t *testing.T, client *api.Client, repo string) (model, effort string) {
	t.Helper()
	setting, err := client.ProjectForeman(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	return setting.Model, setting.Effort
}

// After a removal the command asks the engine which projects have their
// foreman on the removed pair — the harness and the model both, so a
// foreman on the same model name under another harness is not one of them
// — and names each with the switch to a pair the remaining roster offers,
// that harness first. The switch carries the foreman's effort when the
// target accepts it and leaves it out when the target refuses it, since a
// switch typed without it clears the effort.
func TestAgentsRemoveNamesOrphanedForemen(t *testing.T) {
	fakeInitPATH(t, "opencode")
	_, state, _ := startAgentsEngine(t,
		agentsTestForeman{"https://example.invalid/app.git", "opencode", "fixture/gpt-6-sol", "high"},
		agentsTestForeman{"https://example.invalid/claude.git", "claude", "fixture/gpt-6-sol", "high"},
		agentsTestForeman{"https://example.invalid/legacy.git", "opencode", "fixture/glm-5.3", "minimal"},
	)
	saveAgentLines(t, state,
		"harness=opencode,model=fixture/gpt-6-sol",
		"harness=claude,model=sonnet",
		"harness=opencode,model=fixture/glm-5.3",
	)

	code, out, errOut := agentsDo(t, "agents", "remove", "--state-dir", state, "opencode/fixture/gpt-6-sol")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	const waits = "turns wait until another registered agent offers the pair, however long that takes, or the foreman is switched to a pair an agent offers:\n"
	for _, want := range []string{
		"removed opencode/fixture/gpt-6-sol\n",
		"1 project has opencode/fixture/gpt-6-sol as its foreman; its " + waits +
			"  example.invalid/app\n" +
			"    brigada project foreman --repo example.invalid/app --harness opencode --model fixture/glm-5.3 --effort high\n",
		"No runner running; brigada start serves it.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "example.invalid/claude") || strings.Contains(out, "example.invalid/legacy") {
		t.Errorf("a foreman on another pair is named:\n%s", out)
	}

	// No pair left on the foreman's harness: the switch goes to another
	// harness, which refuses the opencode variant, so it carries no effort.
	code, out, errOut = agentsDo(t, "agents", "remove", "--state-dir", state, "opencode/fixture/glm-5.3")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	want := "1 project has opencode/fixture/glm-5.3 as its foreman; its " + waits +
		"  example.invalid/legacy\n" +
		"    brigada project foreman --repo example.invalid/legacy --harness claude --model sonnet\n"
	if !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
	if strings.Contains(out, "example.invalid/app") || strings.Contains(out, "example.invalid/claude") {
		t.Errorf("a foreman on another pair is named:\n%s", out)
	}

	// A pair no project's foreman is on names no one, and says so.
	saveAgentLines(t, state, "harness=claude,model=sonnet", "harness=claude,model=opus")
	code, out, errOut = agentsDo(t, "agents", "remove", "--state-dir", state, "claude/opus")
	if code != 0 || errOut != "" || !strings.Contains(out, "No project has claude/opus as its foreman.\n") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}

// A rewrite's lines name the projects whose foreman is on the moved pair —
// the harness and the model both, so a foreman on the same model name
// under another harness is not named — with the switch to the newer model,
// which carries the effort the newer model accepts; --dry-run prints them
// too and switches nothing, listing the switches --foremen would make.
func TestAgentsUpdateNamesTheForemenOfAMovedPair(t *testing.T) {
	fakeOpenCodeModels(t, "fixture/gpt-6-sol", "fixture/gpt-6.1-sol")
	_, state, client := startAgentsEngine(t,
		agentsTestForeman{"https://example.invalid/app.git", "opencode", "fixture/gpt-6-sol", "high"},
		agentsTestForeman{"https://example.invalid/claude.git", "claude", "fixture/gpt-6-sol", "high"},
	)
	saved := []string{"harness=opencode,model=fixture/gpt-6-sol", "harness=claude,model=sonnet"}
	saveAgentLines(t, state, saved...)
	stubAgentsStdin(t, "") // a dry run reads no answers
	const tail = "Dry run: nothing written, nothing restarted.\n"
	named := "opencode/fixture/gpt-6-sol → opencode/fixture/gpt-6.1-sol\n" +
		"  harness=opencode,model=fixture/gpt-6.1-sol,classes=implement+review+answer\n" +
		"  1 project has opencode/fixture/gpt-6-sol as its foreman; no other line offers the pair once the rewrite lands, so its turns wait until the foreman is switched:\n" +
		"    example.invalid/app\n" +
		"      brigada project foreman --repo example.invalid/app --harness opencode --model fixture/gpt-6.1-sol --effort high\n"

	code, out, errOut := agentsDo(t, "agents", "update", "--state-dir", state, "--dry-run")
	if code != 0 || errOut != "" || out != named+tail {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}

	// The dry run of --foremen lists each switch and makes none.
	code, out, errOut = agentsDo(t, "agents", "update", "--state-dir", state, "--dry-run", "--foremen")
	withSwitch := named + "      with --foremen: switches the foreman to opencode/fixture/gpt-6.1-sol, keeping effort=high\n"
	if code != 0 || errOut != "" || out != withSwitch+tail {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if model, effort := foremanOf(t, client, "https://example.invalid/app.git"); model != "fixture/gpt-6-sol" || effort != "high" {
		t.Errorf("a dry run switched the foreman: model=%s effort=%s", model, effort)
	}
	if got := loadAgentLines(t, state); !reflect.DeepEqual(got, saved) {
		t.Errorf("agents file changed: %q", got)
	}

	// Without --foremen the rewrite lands and the foremen stay where they
	// were: the lines name, the flag switches.
	code, out, errOut = agentsDo(t, "agents", "update", "--state-dir", state, "--yes")
	if code != 0 || errOut != "" || !strings.HasPrefix(out, named) || strings.Contains(out, "switched to") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if model, effort := foremanOf(t, client, "https://example.invalid/app.git"); model != "fixture/gpt-6-sol" || effort != "high" {
		t.Errorf("the foreman moved without --foremen: model=%s effort=%s", model, effort)
	}

	// Another line registering the same pair keeps offering it once the
	// rewrite lands, so the foreman's turns do not wait.
	saveAgentLines(t, state, "harness=opencode,model=fixture/gpt-6-sol", "harness=opencode,model=fixture/gpt-6-sol,classes=review")
	code, out, errOut = agentsDo(t, "agents", "update", "--state-dir", state, "--dry-run")
	if code != 0 || errOut != "" || !strings.Contains(out, "  1 project has opencode/fixture/gpt-6-sol as its foreman; another line still offers the pair, so its turns do not wait:\n") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}

// refusedSwitchTransport reaches the engine as the CLI's own client does,
// but answers every foreman switch with a 500.
type refusedSwitchTransport struct{ next http.RoundTripper }

func (r refusedSwitchTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPut && req.URL.Path == "/v1/projects/foreman" {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":"the store is away","code":"internal"}`)),
			Request:    req,
		}, nil
	}
	return r.next.RoundTrip(req)
}

// --foremen switches the foremen of every applied rewrite to the newer
// model as brigada project foreman does, keeping an effort the newer model
// accepts and leaving one it refuses to the CLI's own default, and says so
// per project. A switch the engine refuses is reported with the command
// that makes it, the rewrite stands, and the command fails.
func TestAgentsUpdateForemenSwitchTheForemen(t *testing.T) {
	fakeOpenCodeModels(t, "fixture/gpt-6-sol", "fixture/gpt-6.1-sol")
	ctx, state, client := startAgentsEngine(t,
		agentsTestForeman{"https://example.invalid/kept.git", "opencode", "fixture/gpt-6-sol", "high"},
		agentsTestForeman{"https://example.invalid/dropped.git", "opencode", "fixture/gpt-6-sol", "low"},
	)
	// An effort the harness no longer accepts, as one an older brigada
	// stored: the engine validates a choice, so it goes straight into the
	// store.
	st, err := store.Open(ctx, lifecycle.PathsOf(state).Store)
	if err != nil {
		t.Fatal(err)
	}
	err = st.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE projects SET foreman_effort = 'HIGH' WHERE repo_url = 'example.invalid/dropped'`)
		return err
	})
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	saveAgentLines(t, state, "harness=opencode,model=fixture/gpt-6-sol")

	code, out, errOut := agentsDo(t, "agents", "update", "--state-dir", state, "--yes", "--foremen")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	for _, want := range []string{
		"  2 projects have opencode/fixture/gpt-6-sol as their foremen; no other line offers the pair once the rewrite lands, so their turns wait until each foreman is switched:\n" +
			"    example.invalid/kept\n" +
			"      brigada project foreman --repo example.invalid/kept --harness opencode --model fixture/gpt-6.1-sol --effort high\n" +
			"    example.invalid/dropped\n" +
			"      brigada project foreman --repo example.invalid/dropped --harness opencode --model fixture/gpt-6.1-sol\n",
		"updated opencode/fixture/gpt-6-sol → opencode/fixture/gpt-6.1-sol\n" +
			"foreman of example.invalid/kept switched to opencode/fixture/gpt-6.1-sol, keeping effort=high\n" +
			"foreman of example.invalid/dropped switched to opencode/fixture/gpt-6.1-sol; effort=HIGH is not accepted for opencode/fixture/gpt-6.1-sol, so the CLI's own default applies\n" +
			"rewritten in ",
		"No runner running; brigada start serves it.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if got := loadAgentLines(t, state); !reflect.DeepEqual(got, []string{"harness=opencode,model=fixture/gpt-6.1-sol,classes=implement+review+answer"}) {
		t.Errorf("lines = %q", got)
	}
	if model, effort := foremanOf(t, client, "https://example.invalid/kept.git"); model != "fixture/gpt-6.1-sol" || effort != "high" {
		t.Errorf("kept effort: model=%s effort=%s", model, effort)
	}
	if model, effort := foremanOf(t, client, "https://example.invalid/dropped.git"); model != "fixture/gpt-6.1-sol" || effort != "" {
		t.Errorf("dropped effort: model=%s effort=%s", model, effort)
	}

	// A switch the engine refuses: reported, the rewrite standing.
	if _, err := client.SetProjectForeman(ctx, "https://example.invalid/kept.git", "opencode", "fixture/gpt-6-sol", "high"); err != nil {
		t.Fatal(err)
	}
	saveAgentLines(t, state, "harness=opencode,model=fixture/gpt-6-sol")
	_, token, err := lifecycle.TokenForRole(lifecycle.PathsOf(state).Tokens, "client")
	if err != nil {
		t.Fatal(err)
	}
	old := connectAgents
	connectAgents = func(context.Context, *runtime, string, bool, io.Writer) (*api.Client, int) {
		socket := lifecycle.PathsOf(state).Socket
		transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}}
		return api.NewClientWithHTTPClient(&http.Client{Transport: refusedSwitchTransport{transport}}, token), 0
	}
	t.Cleanup(func() { connectAgents = old })
	code, out, errOut = agentsDo(t, "agents", "update", "--state-dir", state, "--yes", "--foremen")
	if code != 1 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	for _, want := range []string{"updated opencode/fixture/gpt-6-sol → opencode/fixture/gpt-6.1-sol\n", "rewritten in ", "No runner running; brigada start serves it.\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	refusal := "brigada: agents update: the foreman of example.invalid/kept was not switched to opencode/fixture/gpt-6.1-sol (PUT /v1/projects/foreman: 500 internal: the store is away); the rewrite stands, and brigada project foreman --repo example.invalid/kept --harness opencode --model fixture/gpt-6.1-sol --effort high switches it\n"
	if errOut != refusal {
		t.Errorf("stderr = %q, want %q", errOut, refusal)
	}
	if got := loadAgentLines(t, state); !reflect.DeepEqual(got, []string{"harness=opencode,model=fixture/gpt-6.1-sol,classes=implement+review+answer"}) {
		t.Errorf("the rewrite did not stand: %q", got)
	}
	if model, effort := foremanOf(t, client, "https://example.invalid/kept.git"); model != "fixture/gpt-6-sol" || effort != "high" {
		t.Errorf("the foreman moved: model=%s effort=%s", model, effort)
	}
}

// An engine the commands cannot ask leaves them pointing at brigada project
// list: a removal still says a removed foreman's turns wait, an update's
// lines name no one, and --foremen, with nothing it can switch, says so
// and fails while the rewrite stands.
func TestAgentsForemenNeedTheEngine(t *testing.T) {
	fakeOpenCodeModels(t, "fixture/gpt-6-sol", "fixture/gpt-6.1-sol")
	state := testsupport.ShortStateDir(t)
	old := connectAgents
	connectAgents = func(_ context.Context, _ *runtime, _ string, _ bool, stderr io.Writer) (*api.Client, int) {
		return nil, fail(stderr, errors.New("the engine did not answer"))
	}
	t.Cleanup(func() { connectAgents = old })
	const pointer = "brigada project list shows them"
	saveAgentLines(t, state, "harness=opencode,model=fixture/gpt-6-sol", "harness=claude,model=sonnet")

	code, out, errOut := agentsDo(t, "agents", "update", "--state-dir", state, "--dry-run")
	if code != 0 || !strings.Contains(errOut, "brigada: the engine did not answer") ||
		!strings.Contains(out, "  projects whose foreman is on opencode/fixture/gpt-6-sol: "+pointer+" (the engine could not be asked)\n") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}

	// --foremen cannot switch: the rewrites land and the command fails.
	code, out, errOut = agentsDo(t, "agents", "update", "--state-dir", state, "--yes", "--foremen")
	if code != 1 || !strings.Contains(errOut, "brigada: agents update: no foreman was switched: the engine could not be asked which projects have their foremen on a moved pair; "+pointer+", and brigada project foreman switches one\n") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if got := loadAgentLines(t, state); !reflect.DeepEqual(got, []string{"harness=opencode,model=fixture/gpt-6.1-sol,classes=implement+review+answer", "harness=claude,model=sonnet"}) {
		t.Errorf("lines = %q", got)
	}

	code, out, errOut = agentsDo(t, "agents", "remove", "--state-dir", state, "claude/sonnet")
	if code != 0 || !strings.Contains(errOut, "brigada: the engine did not answer") ||
		!strings.Contains(out, "If this agent was a project's foreman, its turns wait until another registered agent offers the pair ("+pointer+").\n") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}

// --foremen is a boolean flag: a misspelling and a value that is not a
// boolean are usage errors that leave the file as it was, and the help
// shows it beside --yes, --dry-run and --now.
func TestAgentsUpdateForemenFlag(t *testing.T) {
	fakeInitPATH(t, "opencode")
	state := testsupport.ShortStateDir(t)
	saved := []string{"harness=claude,model=sonnet"}
	saveAgentLines(t, state, saved...)
	code, out, errOut := agentsDo(t, "agents", "update", "--help")
	if code != 0 || errOut != "" || !strings.HasPrefix(out, "Usage: brigada agents update [--yes] [--dry-run] [--foremen] [--now]\n") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	for _, want := range []string{"  -yes\n", "  -dry-run\n", "  -foremen\n", "  -now\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("help lacks %q:\n%s", want, out)
		}
	}
	for name, tc := range map[string]struct {
		args     []string
		contains string
	}{
		"misspelled":    {[]string{"--foreman"}, "flag provided but not defined: -foreman\n"},
		"not a boolean": {[]string{"--foremen=sure"}, `invalid boolean value "sure" for -foremen`},
	} {
		t.Run(name, func(t *testing.T) {
			code, out, errOut := agentsDo(t, append([]string{"agents", "update", "--state-dir", state}, tc.args...)...)
			if code != 2 || out != "" || !strings.Contains(errOut, tc.contains) {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
			}
			if got := loadAgentLines(t, state); !reflect.DeepEqual(got, saved) {
				t.Fatalf("agents file changed: %q", got)
			}
		})
	}
}
