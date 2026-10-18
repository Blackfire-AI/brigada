package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/adapter/kimi"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/store"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

func TestRunnerKimiReviewAgentFile(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(strconv.FormatBool(explicit), func(t *testing.T) {
			state := t.TempDir()
			spec := agentSpec{harness: "kimi", classes: splitList(defaultClasses)}
			if explicit {
				spec.agentFile = filepath.Join(t.TempDir(), "custom.md")
				if err := os.WriteFile(spec.agentFile, []byte("custom agent"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			ads, _, err := resolveRunnerAgents(&runtime{stateDir: state}, []agentSpec{spec}, "")
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(state, "runner", "kimi-readonly.md")
			if _, err := os.Stat(file); !os.IsNotExist(err) {
				t.Fatalf("resolving agents wrote the file: %v", err)
			}
			if err := writeKimiReviewAgentFiles(ads); err != nil {
				t.Fatal(err)
			}
			if explicit {
				if _, err := os.Stat(file); !os.IsNotExist(err) {
					t.Fatalf("explicit agent caused default file write: %v", err)
				}
			} else {
				data, err := os.ReadFile(file)
				if err != nil || string(data) != kimi.ReadOnlyAgentDefinition {
					t.Fatalf("review agent = %q, %v", data, err)
				}
				for _, want := range []string{"description:", "tools: [Read, Grep, Glob]", "disallowedTools: [Bash]", "You are a read-only reviewer"} {
					if !strings.Contains(string(data), want) {
						t.Errorf("agent missing %q", want)
					}
				}
				info, err := os.Stat(file)
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatalf("review agent permissions: %v, %v", info, err)
				}
			}
			ad := ads[0].(*kimi.Adapter)
			ad.Binary = "/unused/kimi"
			for _, review := range []bool{false, true} {
				cmd, err := ad.Command(adapter.Job{Prompt: "p", Review: review}, t.TempDir(), "")
				if err != nil {
					t.Fatal(err)
				}
				i := slices.Index(cmd.Args, "--agent-file")
				want := spec.agentFile
				if want == "" && review {
					want = file
				}
				if want == "" {
					if i >= 0 {
						t.Fatalf("work got a review agent: %v", cmd.Args)
					}
				} else if i < 0 || i+1 >= len(cmd.Args) || cmd.Args[i+1] != want {
					t.Fatalf("args = %v, want agent file %q", cmd.Args, want)
				}
			}
		})
	}
}

// syncBuffer is a bytes.Buffer a running goroutine writes to while the
// test reads: the runner's startup line is read while the runner runs.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestRunnerHarnessRegisters covers both runner flag forms and verifies that
// provider/model survives advertisement unchanged. No worker CLI is run.
func TestRunnerHarnessRegisters(t *testing.T) {
	for _, tc := range []struct {
		name, harness, model, family string
		agentForm                    bool
	}{
		{"kimi default", "kimi", "default", "moonshot", false},
		{"codex default", "codex", "default", "openai", false},
		{"opencode default", "opencode", "default", "", false},
		{"opencode default agent flag", "opencode", "default", "", true},
		{"opencode model shorthand", "opencode", "provider/model", "opencode", false},
		{"opencode model agent flag", "opencode", "provider/model", "opencode", true},
		{"opencode GLM provider", "opencode", "zai-coding-plan/glm-5.3", "zhipu", true},
		{"opencode GLM bare", "opencode", "glm-5.3", "zhipu", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testRunnerHarnessRegisters(t, tc.harness, tc.model, tc.family, tc.agentForm)
		})
	}
}

func TestResolveRunnerAgentModels(t *testing.T) {
	for _, tc := range []struct {
		harness, model, wantModel, wantFamily string
	}{
		{"claude", "", "sonnet", "anthropic"},
		{"codex", "", "default", "openai"},
		{"kimi", "", "default", "moonshot"},
		{"opencode", "", "default", ""},
		{"opencode", "default", "default", ""},
		{"opencode", "zai-coding-plan/glm-5.3", "zai-coding-plan/glm-5.3", "zhipu"},
		{"opencode", "glm-5.3", "glm-5.3", "zhipu"},
		{"opencode", "provider/model", "provider/model", "opencode"},
	} {
		t.Run(tc.harness+"/"+tc.model, func(t *testing.T) {
			spec := agentSpec{harness: tc.harness, model: tc.model, classes: splitList(defaultClasses)}
			ads, agents, err := resolveRunnerAgents(&runtime{}, []agentSpec{spec}, "")
			if err != nil {
				t.Fatal(err)
			}
			if len(ads) != 1 || len(agents) != 1 {
				t.Fatalf("got %d adapters and %d agents", len(ads), len(agents))
			}
			if ads[0].Name() != tc.harness || agents[0].Model != tc.wantModel || agents[0].Family != tc.wantFamily {
				t.Fatalf("adapter=%s agent=%+v; want harness=%s model=%s family=%s", ads[0].Name(), agents[0], tc.harness, tc.wantModel, tc.wantFamily)
			}
		})
	}
}

func testRunnerHarnessRegisters(t *testing.T, harness, wantModel, wantFamily string, agentForm bool) {
	state := testsupport.ShortStateDir(t)
	agentFile := filepath.Join(t.TempDir(), "worker.md")
	if err := os.WriteFile(agentFile, []byte("# the agent\n"), 0o644); err != nil {
		t.Fatalf("write the agent file: %v", err)
	}

	// The store handle the test polls: opened before the engine starts,
	// read while it runs — one reader beside the engine's writer.
	st, err := store.Open(context.Background(), lifecycle.PathsOf(state).Store)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- lifecycle.Run(ctx, lifecycle.Config{StateDir: state, Idle: time.Minute}) }()
	socket := lifecycle.PathsOf(state).Socket
	deadline := time.Now().Add(10 * time.Second)
	for !lifecycle.EngineRunning(ctx, socket) {
		if time.Now().After(deadline) {
			t.Fatal("the engine did not answer in time")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The runner runs until stopped; registering is the first thing it
	// does, so the row it leaves is the test's signal to stop it.
	rctx, stop := context.WithCancel(context.Background())
	defer stop()
	out := &syncBuffer{}
	runDone := make(chan int, 1)
	args := []string{"runner", "--state-dir", state, "--no-start"}
	if agentForm {
		args = append(args, "--agent", "harness="+harness+",model="+wantModel)
	} else {
		args = append(args, "--harness", harness)
		if wantModel != "default" {
			args = append(args, "--model", wantModel)
		}
		if harness == "kimi" {
			args = append(args, "--agent-file", agentFile)
		}
	}
	go func() { runDone <- run(rctx, args, out, io.Discard) }()
	var adapterName, family, model string
	deadline = time.Now().Add(10 * time.Second)
	for {
		err := st.View(ctx, func(tx *sql.Tx) error {
			return tx.QueryRow("SELECT adapter, family, model FROM agents WHERE harness = ?", harness).
				Scan(&adapterName, &family, &model)
		})
		if err == nil {
			break
		}
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("read the registered agent: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the runner did not register its agent in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the runner did not stop")
	}

	if adapterName != harness || family != wantFamily {
		t.Errorf("the registered agent = adapter %s, family %s; want %s, %s", adapterName, family, harness, wantFamily)
	}
	if model != wantModel {
		t.Errorf("the registered model = %q, want %q", model, wantModel)
	}
	if line := out.String(); !strings.Contains(line, harness+" (model "+wantModel+", classes implement,review,answer)") {
		t.Errorf("the startup line = %q, want the harness and model with the classes", line)
	}
	warning := harness + "/" + wantModel + ": model family unknown; it takes no review that needs another family (give provider/model to fix)\n"
	wantWarnings := 0
	if wantFamily == "" {
		wantWarnings = 1
	}
	if got := strings.Count(out.String(), warning); got != wantWarnings {
		t.Errorf("startup output = %q; want %d unknown-family warnings", out.String(), wantWarnings)
	}

	// The engine stops cleanly once the runner is gone.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the engine stopped with an error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("the engine did not stop")
	}
}

// TestRunnerHarnessFlags covers the harness flag's rules at startup:
// --agent-file belongs to kimi alone and is a usage error under every
// other harness, an agent file that does not exist fails at once rather
// than at the first job, and an unknown harness names every supported adapter.
func TestRunnerHarnessFlags(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	dir := t.TempDir()
	agentFile := filepath.Join(dir, "worker.md")
	if err := os.WriteFile(agentFile, []byte("# the agent\n"), 0o644); err != nil {
		t.Fatalf("write the agent file: %v", err)
	}
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantErr  string
	}{
		{
			name:     "an unknown harness names all five",
			args:     []string{"runner", "--harness", "gemini", "--state-dir", state},
			wantCode: 2,
			wantErr:  `unknown harness "gemini"; want claude, kimi, codex, opencode or fake`,
		},
		{
			name:     "agent file with the claude harness is a usage error",
			args:     []string{"runner", "--harness", "claude", "--agent-file", agentFile, "--state-dir", state},
			wantCode: 2,
			wantErr:  "--agent-file is a --harness kimi flag",
		},
		{
			name:     "agent file with the opencode harness is a usage error",
			args:     []string{"runner", "--harness", "opencode", "--agent-file", agentFile, "--state-dir", state},
			wantCode: 2,
			wantErr:  "--agent-file is a --harness kimi flag",
		},
		{
			name: "agent file with the fake harness is a usage error",
			args: []string{"runner", "--harness", "fake", "--worker-script", "worker.sh",
				"--agent-file", agentFile, "--state-dir", state},
			wantCode: 2,
			wantErr:  "--agent-file is a --harness kimi flag",
		},
		{
			name:     "tools with the fake harness is a usage error",
			args:     []string{"runner", "--agent", "harness=fake,tools=none", "--state-dir", state},
			wantCode: 2,
			wantErr:  "takes no tools scope",
		},
		{
			name:     "tools with the codex harness is a usage error",
			args:     []string{"runner", "--agent", "harness=codex,tools=Bash(ls:*)", "--state-dir", state},
			wantCode: 2,
			wantErr:  "codex harness takes no tools scope",
		},
		{
			name: "a missing agent file fails at startup",
			args: []string{"runner", "--harness", "kimi", "--agent-file", filepath.Join(dir, "nope.md"),
				"--state-dir", state},
			wantCode: 1,
			wantErr:  "nope.md",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("run(%q) exit code = %d, want %d", tt.args, code, tt.wantCode)
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Errorf("run(%q) stderr = %q, want it to contain %q", tt.args, stderr.String(), tt.wantErr)
			}
		})
	}
}

// registeredAccounts starts an engine on a fresh state directory, runs a
// runner with the given args against it, and returns the account stored
// for each offered harness together with the runner's own name — what
// the engine holds, not what the flag parser produced.
func registeredAccounts(t *testing.T, harnesses []string, args ...string) (accounts map[string]string, runnerName string) {
	t.Helper()
	state := testsupport.ShortStateDir(t)
	st, err := store.Open(context.Background(), lifecycle.PathsOf(state).Store)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- lifecycle.Run(ctx, lifecycle.Config{StateDir: state, Idle: time.Minute}) }()
	socket := lifecycle.PathsOf(state).Socket
	deadline := time.Now().Add(10 * time.Second)
	for !lifecycle.EngineRunning(ctx, socket) {
		if time.Now().After(deadline) {
			t.Fatal("the engine did not answer in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
	runnerName, _, err = lifecycle.TokenForRole(lifecycle.PathsOf(state).Tokens, "runner")
	if err != nil {
		t.Fatalf("read the runner token: %v", err)
	}

	rctx, stop := context.WithCancel(context.Background())
	defer stop()
	runDone := make(chan int, 1)
	full := append([]string{"runner", "--state-dir", state, "--no-start"}, args...)
	go func() { runDone <- run(rctx, full, io.Discard, io.Discard) }()
	accounts = map[string]string{}
	deadline = time.Now().Add(10 * time.Second)
	for len(accounts) < len(harnesses) {
		for _, h := range harnesses {
			var account string
			err := st.View(ctx, func(tx *sql.Tx) error {
				return tx.QueryRow("SELECT account FROM agents WHERE harness = ?", h).Scan(&account)
			})
			if err == nil {
				accounts[h] = account
			} else if !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("read the registered account: %v", err)
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the runner did not register its agents in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the runner did not stop")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Error("the engine did not stop")
	}
	return accounts, runnerName
}

// TestRunnerAccountLabels: the account an agent registers under is its
// account= label, else its harness name — never the runner's name — so
// one agent's quota cooling cannot reach another harness's login.
func TestRunnerAccountLabels(t *testing.T) {
	worker := filepath.Join(t.TempDir(), "worker.sh")
	if err := os.WriteFile(worker, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write the worker script: %v", err)
	}

	t.Run("default and explicit labels", func(t *testing.T) {
		accounts, runnerName := registeredAccounts(t, []string{"claude", "fake"},
			"--agent", "harness=claude",
			"--agent", "harness=fake,account=second-login",
			"--worker-script", worker)
		if accounts["claude"] != "claude" {
			t.Errorf("claude's account = %q, want its harness name", accounts["claude"])
		}
		if accounts["fake"] != "second-login" {
			t.Errorf("fake's account = %q, want second-login", accounts["fake"])
		}
		for h, a := range accounts {
			if a == runnerName {
				t.Errorf("%s's account is the runner's name %q", h, runnerName)
			}
		}
	})

	t.Run("shorthand flags default to the harness", func(t *testing.T) {
		accounts, runnerName := registeredAccounts(t, []string{"fake"}, "--harness", "fake", "--worker-script", worker)
		if accounts["fake"] != "fake" || accounts["fake"] == runnerName {
			t.Errorf("fake's account = %q (runner %q), want the harness name", accounts["fake"], runnerName)
		}
	})

	// A runner offers one agent per harness, so two agents of one harness
	// sharing an account live on two runners.
	t.Run("two agents of one harness share an explicit account", func(t *testing.T) {
		first, _ := registeredAccounts(t, []string{"claude"}, "--agent", "harness=claude,account=team")
		second, _ := registeredAccounts(t, []string{"claude"}, "--agent", "harness=claude,model=opus,account=team")
		if first["claude"] != "team" || second["claude"] != "team" {
			t.Errorf("accounts = %q and %q, want both team", first["claude"], second["claude"])
		}
	})
}

// TestAgentSpecAccount: the --agent parser keeps the account= key and
// round-trips it.
func TestAgentSpecAccount(t *testing.T) {
	s := &agentSpecs{}
	if err := s.Set("harness=claude,account=a,classes=review"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := s.specs[0].accountLabel(); got != "a" {
		t.Errorf("accountLabel = %q, want a", got)
	}
	if got := s.specs[0].String(); !strings.Contains(got, "account=a") {
		t.Errorf("String() = %q, want it to carry account=a", got)
	}
	if err := s.Set("harness=kimi"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := s.specs[1].accountLabel(); got != "kimi" {
		t.Errorf("default accountLabel = %q, want kimi", got)
	}
}

// TestAgentSpecTools: the tools= key parses +-separated patterns, none
// as an explicit empty scope distinct from unset, and round-trips.
func TestAgentSpecTools(t *testing.T) {
	s := &agentSpecs{}
	for _, v := range []string{
		"harness=claude,tools=Bash(npm test:*)+Bash(npx jest:*)",
		"harness=claude,model=opus,tools=none",
		"harness=claude,model=haiku",
	} {
		if err := s.Set(v); err != nil {
			t.Fatalf("Set(%q): %v", v, err)
		}
	}
	if got := s.specs[0].tools; len(got) != 2 || got[0] != "Bash(npm test:*)" || got[1] != "Bash(npx jest:*)" {
		t.Errorf("tools = %v", got)
	}
	if got := s.specs[1].tools; got == nil || len(got) != 0 {
		t.Errorf("tools=none = %#v, want an empty non-nil scope", got)
	}
	if s.specs[2].tools != nil {
		t.Errorf("unset tools = %#v, want nil (the default)", s.specs[2].tools)
	}
	if got := s.specs[1].String(); !strings.Contains(got, "tools=none") {
		t.Errorf("String() = %q, want tools=none", got)
	}
	if err := s.Set("harness=claude,tools=+"); err == nil {
		t.Error("an empty tools list was accepted")
	}
}

// TestRunnerAgentFlag covers the --agent flag's rules at startup: two
// agents register as two rows with their own models and classes, a
// duplicate harness is a usage error, mixing --agent with the
// single-agent shorthand flags is a usage error, and a missing agent
// file or an empty classes list fails at once rather than at the first
// job.
func TestRunnerAgentFlag(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	dir := t.TempDir()
	agentFile := filepath.Join(dir, "worker.md")
	if err := os.WriteFile(agentFile, []byte("# the agent\n"), 0o644); err != nil {
		t.Fatalf("write the agent file: %v", err)
	}
	worker := filepath.Join(dir, "worker.sh")
	if err := os.WriteFile(worker, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write the worker script: %v", err)
	}

	// Two --agent values, no engine: the usage errors all fire before
	// the runner would start one.
	bad := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "a duplicate harness is refused",
			args:    []string{"runner", "--agent", "harness=claude", "--agent", "harness=claude,model=opus", "--state-dir", state},
			wantErr: `harness "claude" is offered twice`,
		},
		{
			name:    "mixing --agent with --harness is refused",
			args:    []string{"runner", "--agent", "harness=claude", "--harness", "kimi", "--state-dir", state},
			wantErr: "--agent and --harness cannot be mixed",
		},
		{
			name:    "mixing --agent with --model is refused",
			args:    []string{"runner", "--agent", "harness=claude", "--model", "opus", "--state-dir", state},
			wantErr: "--agent and --model cannot be mixed",
		},
		{
			name:    "mixing --agent with --classes is refused",
			args:    []string{"runner", "--agent", "harness=claude", "--classes", "implement", "--state-dir", state},
			wantErr: "--agent and --classes cannot be mixed",
		},
		{
			name:    "a missing harness is refused",
			args:    []string{"runner", "--agent", "model=opus", "--state-dir", state},
			wantErr: "harness=<h> is required",
		},
		{
			name:    "an unknown field is refused",
			args:    []string{"runner", "--agent", "harness=claude,budget=5", "--state-dir", state},
			wantErr: `unknown field "budget"`,
		},
		{
			name:    "an unknown harness names all five",
			args:    []string{"runner", "--agent", "harness=gemini", "--state-dir", state},
			wantErr: `unknown harness "gemini"; want claude, kimi, codex, opencode or fake`,
		},
		{
			name:    "an agent file under the claude harness is refused",
			args:    []string{"runner", "--agent", "harness=claude,agent-file=" + agentFile, "--state-dir", state},
			wantErr: "--agent-file is a --harness kimi flag",
		},
		{
			name:    "a missing agent file fails at startup",
			args:    []string{"runner", "--agent", "harness=kimi,agent-file=" + filepath.Join(dir, "nope.md"), "--state-dir", state},
			wantErr: "nope.md",
		},
		{
			name:    "tools under codex names its sandbox",
			args:    []string{"runner", "--agent", "harness=codex,tools=none", "--state-dir", state},
			wantErr: "-s workspace-write",
		},
		{
			name:    "tools under kimi names its headless mode",
			args:    []string{"runner", "--agent", "harness=kimi,tools=Bash(npm test:*)", "--state-dir", state},
			wantErr: "no approval prompts",
		},
		{
			name:    "tools under opencode names its permissions",
			args:    []string{"runner", "--agent", "harness=opencode,tools=none", "--state-dir", state},
			wantErr: "denies bash by default",
		},
		{
			name:    "an empty tools list is refused",
			args:    []string{"runner", "--agent", "harness=claude,tools=", "--state-dir", state},
			wantErr: "tool patterns",
		},
		{
			name:    "an empty classes list is refused",
			args:    []string{"runner", "--agent", "harness=claude,classes=", "--state-dir", state},
			wantErr: `classes is empty`,
		},
		{
			name:    "an effort outside the claude levels is refused",
			args:    []string{"runner", "--agent", "harness=claude,effort=turbo", "--state-dir", state},
			wantErr: `effort "turbo" is not supported for harness "claude"; want one of low, medium, high, xhigh, max`,
		},
		{
			name:    "kimi has no effort control",
			args:    []string{"runner", "--agent", "harness=kimi,effort=high", "--state-dir", state},
			wantErr: `effort is not supported for harness "kimi"`,
		},
		{
			name:    "an opencode variant brigada does not map is refused",
			args:    []string{"runner", "--agent", "harness=opencode,effort=low", "--state-dir", state},
			wantErr: `effort "low" is not supported for harness "opencode"; want one of minimal, high, max`,
		},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tt.args, &stdout, &stderr)
			if code == 0 {
				t.Errorf("run(%q) exit code = 0, want a refusal", tt.args)
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Errorf("run(%q) stderr = %q, want it to contain %q", tt.args, stderr.String(), tt.wantErr)
			}
		})
	}

	// Two agents against a real engine: both register, each with its
	// own model and classes, and the startup line lists them both.
	st, err := store.Open(context.Background(), lifecycle.PathsOf(state).Store)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- lifecycle.Run(ctx, lifecycle.Config{StateDir: state, Idle: time.Minute}) }()
	socket := lifecycle.PathsOf(state).Socket
	deadline := time.Now().Add(10 * time.Second)
	for !lifecycle.EngineRunning(ctx, socket) {
		if time.Now().After(deadline) {
			t.Fatal("the engine did not answer in time")
		}
		time.Sleep(20 * time.Millisecond)
	}

	rctx, stop := context.WithCancel(context.Background())
	defer stop()
	out := &syncBuffer{}
	runDone := make(chan int, 1)
	go func() {
		runDone <- run(rctx, []string{"runner",
			"--agent", "harness=claude,model=sonnet,effort=high,classes=implement+review+answer",
			"--agent", "harness=fake,classes=review",
			"--worker-script", worker,
			"--state-dir", state, "--no-start"}, out, io.Discard)
	}()
	deadline = time.Now().Add(10 * time.Second)
	for {
		var n int
		err := st.View(ctx, func(tx *sql.Tx) error {
			return tx.QueryRow("SELECT COUNT(*) FROM agents WHERE harness IN ('claude', 'fake')").Scan(&n)
		})
		if err != nil {
			t.Fatalf("count the registered agents: %v", err)
		}
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the runner did not register both agents in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var fakeClasses string
	if err := st.View(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT classes FROM agents WHERE harness = 'fake'").Scan(&fakeClasses)
	}); err != nil {
		t.Fatalf("read the fake agent's classes: %v", err)
	}
	// A second runner process on the same state directory is refused at
	// the lock — after the engine answered and the token was read, but
	// before it can announce itself up: the startup line belongs to a
	// runner that is running.
	secondArgs := []string{"runner",
		"--agent", "harness=claude,model=sonnet,effort=high,classes=implement+review+answer",
		"--agent", "harness=fake,classes=review",
		"--worker-script", worker,
		"--state-dir", state, "--no-start"}
	var secondOut, secondErr bytes.Buffer
	if code := run(context.Background(), secondArgs, &secondOut, &secondErr); code == 0 {
		t.Error("the second runner process on one state dir exited 0, want a refusal")
	}
	if err := secondErr.String(); !strings.Contains(err, "another runner") {
		t.Errorf("the second runner's stderr = %q, want it to name another runner", err)
	}
	if up := secondOut.String(); strings.Contains(up, "up on") {
		t.Errorf("the second runner printed a startup line before failing: %q", up)
	}
	stop()
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the runner did not stop")
	}
	var claudeEffort, fakeEffort string
	if err := st.View(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRow("SELECT effort FROM agents WHERE harness = 'claude'").Scan(&claudeEffort); err != nil {
			return err
		}
		return tx.QueryRow("SELECT effort FROM agents WHERE harness = 'fake'").Scan(&fakeEffort)
	}); err != nil {
		t.Fatalf("read the agents' effort: %v", err)
	}
	if claudeEffort != "high" || fakeEffort != "" {
		t.Errorf("registered effort = claude %q, fake %q; want high and unset", claudeEffort, fakeEffort)
	}
	if !strings.Contains(fakeClasses, `"review"`) {
		t.Errorf("the fake agent's classes = %s, want only review", fakeClasses)
	}
	line := out.String()
	for _, want := range []string{"claude (model sonnet@high, classes implement,review,answer)", "fake (model fake-1, classes review)",
		"claude (model sonnet@high, classes implement,review,answer) tools Bash(go build:*)+Bash(go test:*)+Bash(go vet:*)+Bash(gofmt:*);"} {
		if !strings.Contains(line, want) {
			t.Errorf("the startup line = %q, want it to list %q", line, want)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the engine stopped with an error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("the engine did not stop")
	}
}
