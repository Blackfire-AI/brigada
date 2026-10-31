package runner

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/adapter/fake"
	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// A grant that names the agent's adapter and model — a foreman turn's
// does — selects exactly that registration, whatever other agents of the
// harness come first; a grant that names a model this runner does not
// offer fails, never running on another.
func TestAgentForSelectsTheNamedModelAndAdapter(t *testing.T) {
	cfg := Config{
		Agents: []api.Agent{
			{Adapter: "fakea", Harness: "fake", Model: "fake-1", Effort: ""},
			{Adapter: "fakeb", Harness: "fake", Model: "fake-2"},
			{Adapter: "fakec", Harness: "fake", Model: "fake-2"},
		},
		Adapters: []adapter.Adapter{&modelRecorder{name: "fakea"}, &modelRecorder{name: "fakeb"}, &modelRecorder{name: "fakec"}},
	}
	cases := []struct {
		name        string
		grant       api.JobGrant
		wantAdapter string
		wantModel   string
	}{
		{"harness only keeps the first agent", api.JobGrant{Agent: "fake"}, "fakea", "fake-1"},
		{"the named model", api.JobGrant{Kind: "foreman", Agent: "fake", AgentAdapter: "fakeb", AgentModel: "fake-2"}, "fakeb", "fake-2"},
		{"the named adapter of a shared model", api.JobGrant{Kind: "foreman", Agent: "fake", AgentAdapter: "fakec", AgentModel: "fake-2"}, "fakec", "fake-2"},
	}
	for _, tc := range cases {
		ad, model, err := cfg.agentFor(&tc.grant)
		if err != nil {
			t.Errorf("%s: agentFor: %v", tc.name, err)
			continue
		}
		if ad.Name() != tc.wantAdapter || model != tc.wantModel {
			t.Errorf("%s: agentFor = %s/%s, want %s/%s", tc.name, ad.Name(), model, tc.wantAdapter, tc.wantModel)
		}
	}
	if _, _, err := cfg.agentFor(&api.JobGrant{Kind: "foreman", Agent: "fake", AgentAdapter: "fakeb", AgentModel: "fake-9"}); err == nil {
		t.Error("agentFor for a model the runner does not offer succeeded; want an error")
	}
}

// A foreman turn runs at the chosen agent's own effort unless the project
// chose a foreman effort; the agent is the one the grant names.
func TestEffortForTheNamedForemanAgent(t *testing.T) {
	cfg := &Config{Agents: []api.Agent{
		{Adapter: "claude", Harness: "claude", Model: "sonnet", Effort: "low"},
		{Adapter: "claude", Harness: "claude", Model: "opus", Effort: "high"},
	}}
	named := api.JobGrant{Kind: "foreman", Agent: "claude", AgentAdapter: "claude", AgentModel: "opus"}
	if got := cfg.effortFor(&named); got != "high" {
		t.Errorf("effortFor the named opus agent = %q, want its own high", got)
	}
	named.ForemanEffort = "max"
	if got := cfg.effortFor(&named); got != "max" {
		t.Errorf("effortFor with a project effort = %q, want max", got)
	}
}

// modelRecorder is the fake adapter under a name of its own that
// remembers the model of every command it was asked to build.
type modelRecorder struct {
	*fake.Adapter
	name string

	mu      sync.Mutex
	models  []string
	efforts []string
}

func (a *modelRecorder) Name() string { return a.name }

func (a *modelRecorder) Command(job adapter.Job, workdir, resumeSessionID string) (*exec.Cmd, error) {
	a.mu.Lock()
	a.models = append(a.models, job.Model)
	a.efforts = append(a.efforts, job.Effort)
	a.mu.Unlock()
	return a.Adapter.Command(job, workdir, resumeSessionID)
}

func (a *modelRecorder) ran() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.models...)
}

// twoFakeAgents is one runner's advertisement of two same-harness agents:
// each under an adapter of its own, so a test can tell which one ran.
func twoFakeAgents(script, modelA, modelB string) ([]api.Agent, []adapter.Adapter, *modelRecorder, *modelRecorder) {
	a := &modelRecorder{Adapter: &fake.Adapter{Script: script}, name: "fakea"}
	b := &modelRecorder{Adapter: &fake.Adapter{Script: script}, name: "fakeb"}
	classes := []string{"implement", "review", "answer"}
	agents := []api.Agent{
		{Adapter: "fakea", Harness: "fake", Model: modelA, Family: "fama", Account: "acct-a", Classes: classes},
		{Adapter: "fakeb", Harness: "fake", Model: modelB, Family: "famb", Account: "acct-b", Classes: classes},
	}
	return agents, []adapter.Adapter{a, b}, a, b
}

// One runner advertising two models of one harness runs the foreman's
// turn on the model the project chose: the command receives it.
func TestRunnerForemanTurnRunsOnTheChosenModel(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, engine.Config{})
	e.seedProject("")
	e.setForeman("fake", "fake-2")
	turn := e.submitForemanTurn("a worker asked")
	markers := markerDir(t)
	if err := os.WriteFile(filepath.Join(markers, "release"), nil, 0o644); err != nil {
		t.Fatalf("release the turn's wait: %v", err)
	}
	script := writeWorkerScript(t, turnWorker)
	agents, adapters, first, second := twoFakeAgents(script, "fake-1", "fake-2")

	p := e.startRunner(script, 100*time.Millisecond, func(cfg *Config) {
		cfg.Agents, cfg.Adapters = agents, adapters
	})
	defer p.stop(t)

	e.waitJob(turn.JobID, func(j *engine.JobInfo) bool { return j.Status == "done" })
	if got := second.ran(); len(got) != 1 || got[0] != "fake-2" {
		t.Errorf("the fake-2 agent's commands ran with models %v, want [fake-2]", got)
	}
	if got := first.ran(); len(got) != 0 {
		t.Errorf("the fake-1 agent ran %v, want nothing: the turn is on fake-2", got)
	}
}

// Two registrations may share the harness and model and differ in
// adapter: when the first is ineligible the second executes.
func TestRunnerForemanTurnRunsOnTheSecondAdapter(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, engine.Config{})
	e.seedProject("")
	e.setForeman("fake", "fake-1")
	markers := markerDir(t)
	if err := os.WriteFile(filepath.Join(markers, "release"), nil, 0o644); err != nil {
		t.Fatalf("release the turn's wait: %v", err)
	}
	script := writeWorkerScript(t, turnWorker)
	agents, adapters, first, second := twoFakeAgents(script, "fake-1", "fake-1")

	p := e.startRunner(script, 100*time.Millisecond, func(cfg *Config) {
		cfg.Agents, cfg.Adapters = agents, adapters
	})
	defer p.stop(t)

	e.waitUntil("both agents to register", func() bool {
		return e.int64Of("SELECT COUNT(*) FROM agents") == 2
	})
	if err := e.update(func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE agents SET health = 'broken', health_reason = 'test' WHERE adapter = 'fakea'")
		return err
	}); err != nil {
		t.Fatalf("break the first registration: %v", err)
	}
	turn := e.submitForemanTurn("a worker asked")

	e.waitJob(turn.JobID, func(j *engine.JobInfo) bool { return j.Status == "done" })
	if got := second.ran(); len(got) != 1 || got[0] != "fake-1" {
		t.Errorf("the second adapter ran with models %v, want [fake-1]", got)
	}
	if got := first.ran(); len(got) != 0 {
		t.Errorf("the ineligible first adapter ran %v, want nothing", got)
	}
}

func TestWorkGrantSelectsSameNamedAdapterByModel(t *testing.T) {
	sonnet := &modelRecorder{name: "claude"}
	haiku := &modelRecorder{name: "claude"}
	cfg := Config{Agents: []api.Agent{
		{Adapter: "claude", Harness: "claude", Model: "sonnet", Effort: "low"},
		{Adapter: "claude", Harness: "claude", Model: "haiku", Effort: "high"},
	}, Adapters: []adapter.Adapter{sonnet, haiku}}
	g := api.JobGrant{Kind: "work", Agent: "claude", AgentAdapter: "claude", AgentModel: "haiku"}
	ad, model, err := cfg.agentFor(&g)
	if err != nil || ad != haiku || model != "haiku" {
		t.Fatalf("agentFor = %v/%s, %v; want haiku adapter", ad, model, err)
	}
	if got := cfg.effortFor(&g); got != "high" {
		t.Errorf("effort = %q, want high", got)
	}
	g.Effort = "max"
	if got := cfg.effortFor(&g); got != "max" {
		t.Errorf("effort = %q, want max", got)
	}
}

// Same-named adapters stay paired with their models through lease and execution.
func TestRunnerWorkRunsOnTheNamedModel(t *testing.T) {
	t.Parallel()
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	task := e.seedTask("", "true")
	if err := e.update(func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE tasks SET effort = 'high' WHERE id = ?", task)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	job := e.submitWork(task, "Add work.txt.")
	markerDir(t)
	script := writeWorkerScript(t, happyWorker)
	sonnet := &modelRecorder{Adapter: &fake.Adapter{Script: script}, name: "claude"}
	haiku := &modelRecorder{Adapter: &fake.Adapter{Script: script}, name: "claude"}
	p := e.startRunner(script, 100*time.Millisecond, func(cfg *Config) {
		cfg.Agents = []api.Agent{
			{Adapter: "claude", Harness: "claude", Model: "sonnet", Family: "anthropic", Account: "claude", Classes: []string{}},
			{Adapter: "claude", Harness: "claude", Model: "haiku", Family: "anthropic", Account: "claude", Classes: []string{"implement"}, Effort: "low"},
		}
		cfg.Adapters = []adapter.Adapter{sonnet, haiku}
	})
	defer p.stop(t)
	e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "done" })
	if got := sonnet.ran(); len(got) != 0 {
		t.Errorf("sonnet ran %v, want no jobs", got)
	}
	if got := haiku.ran(); len(got) != 1 || got[0] != "haiku" {
		t.Errorf("haiku ran %v, want [haiku]", got)
	}
	haiku.mu.Lock()
	defer haiku.mu.Unlock()
	if len(haiku.efforts) != 1 || haiku.efforts[0] != "high" {
		t.Errorf("haiku efforts = %v, want [high]", haiku.efforts)
	}
}
