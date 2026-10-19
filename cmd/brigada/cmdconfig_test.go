package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

func configDo(t *testing.T, ctx context.Context, args ...string) string {
	t.Helper()
	var out, errs bytes.Buffer
	if code := run(ctx, append([]string{"config"}, args...), &out, &errs); code != 0 || errs.Len() != 0 {
		t.Fatalf("config: code=%d stdout=%q stderr=%q", code, out.String(), errs.String())
	}
	assertConfigWidth(t, out.String())
	return out.String()
}

func assertConfigWidth(t *testing.T, output string) {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if utf8.RuneCountInString(line) >= 80 {
			t.Errorf("config line has %d columns: %q", utf8.RuneCountInString(line), line)
		}
	}
}

func TestConfigSavedAgentsAndRunningRunnerOutsideRepo(t *testing.T) {
	calls := fakeInitPATH(t)
	t.Chdir(t.TempDir())
	home := os.Getenv("HOME")
	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, ".codex", "config.toml"), "model = 'gpt-6.1-sol'\nmodel_reasoning_effort = 'low'\napi_key = 'never-print-this'\n")
	write(filepath.Join(home, ".kimi-code", "config.toml"), "default_model = 'kimi-code/k3'\n[models.\"kimi-code/k3\"]\ndefault_effort = 'high'\napi_key = 'never-print-this'\n")
	state := testsupport.ShortStateDir(t)
	config := filepath.Join(state, "machine-config")
	write(config, "notify = none\nstate_dir = file-state\ncaps.global = 0\ncaps.account.work = 3\ncaps.model.opus = 2\ncost.claude/opus = 2\ntools.claude/sonnet = none\n")
	dir := filepath.Join(state, "runner")
	if err := runnerstate.SaveAgents(dir, []string{
		"harness=claude,model=sonnet,tools=none,account=work",
		"harness=codex,model=default",
		"harness=kimi,model=default,classes=review,agent-file=reviewer.md",
	}); err != nil {
		t.Fatal(err)
	}
	release, err := runnerstate.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { release() }()
	// The live record deliberately differs: the command must show the saved
	// agents, not the live advertisements or the record's old --agent specs.
	if err := runnerstate.Publish(dir, []api.Agent{{Harness: "fake", Model: "old-live-model"}}, "harness=fake,model=old-live-model"); err != nil {
		t.Fatal(err)
	}
	args := []string{"--state-dir", state, "--config", config}
	out := configDo(t, context.Background(), args...)
	flat := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{
		"Agents (runner; change: brigada init)",
		"claude sonnet effort: Claude's default writes code, reviews",
		"codex gpt-6.1-sol effort: low (codex default) writes code, reviews",
		"kimi kimi-code/k3 effort: high (kimi config) reviews only",
		"account: work", "tools: none", "agent-file: reviewer.md",
		fmt.Sprintf("runner: running (pid %d) · after a reboot: brigada start", os.Getpid()),
		"saved in:",
		"This project: run inside a repository brigada knows (brigada init)",
		"This machine (",
		"edit the file)",
		"notify = none", "caps.global = 0",
		"caps.account.work = 3", "caps.model.opus = 2", "cost.claude/opus = 2",
		"tools.claude/sonnet = none", "idle_minutes = 30 (default)",
		"task_auto_retry = true (default)", "headroom = true (default)",
		"foreman_wakes_per_day = 20 (default)", "default_reviewer = none (default)",
		"snapshot_dir = none (default)", "caps.plan = no cap (default)",
		"caps.runner = no cap (default)", "caps.account.<label> = no cap (default)",
		"caps.model.<model> = no cap (default)", "cost.<harness>/<model> = 1 (default)",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Long unbroken paths can wrap in the middle of an identifier.
	compact := strings.Join(strings.Fields(out), "")
	if want := "state_dir=" + initHomePath(state) + "(--state-dir)"; !strings.Contains(compact, want) {
		t.Errorf("state_dir does not show the directory in use, want %q in:\n%s", want, out)
	}
	if strings.Contains(compact, "file-state") {
		t.Errorf("state_dir shows the config file's value despite --state-dir:\n%s", out)
	}
	for _, path := range []string{filepath.Join(dir, "agents"), config} {
		if !strings.Contains(compact, path) {
			t.Errorf("missing location %q in:\n%s", path, out)
		}
	}
	for _, bad := range []string{"never-print-this", "old-live-model", "Foreman (", "notify = none (default)", "caps.global = 0 (default)"} {
		if strings.Contains(out, bad) {
			t.Errorf("unexpected %q in:\n%s", bad, out)
		}
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Errorf("config ran a harness despite locally available metadata: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, "engine.log")); !os.IsNotExist(err) {
		t.Errorf("config outside a repository started an engine: %v", err)
	}
	release()
	release = func() {}
	out = configDo(t, context.Background(), args...)
	if !strings.Contains(out, "runner: stopped") || !strings.Contains(out, "kimi-code/k3") {
		t.Fatalf("stopped runner lost saved choices:\n%s", out)
	}
}

func TestConfigProjectProvenance(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx, state := startSettingsEngine(t)
	dir := filepath.Join(state, "runner")
	if err := runnerstate.SaveAgents(dir, []string{"harness=claude,model=sonnet,effort=high"}); err != nil {
		t.Fatal(err)
	}
	release, err := runnerstate.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := runnerstate.Publish(dir, nil); err != nil {
		t.Fatal(err)
	}
	_, secret, err := lifecycle.TokenForRole(lifecycle.PathsOf(state).Tokens, "client")
	if err != nil {
		t.Fatal(err)
	}
	client := api.NewClient(lifecycle.PathsOf(state).Socket, secret)
	write := importRepo(t)
	do := func(args ...string) {
		t.Helper()
		if code, out, errs := runWithStdin(t, ctx, state, "", args); code != 0 || errs != "" {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q", args, code, out, errs)
		}
	}
	// A repository alone does not make a project, even with a config file.
	write(projectFile("default_check: uv run pytest\n"))
	out := configDo(t, ctx, "--state-dir", state)
	if !strings.Contains(out, "This project: run inside") || strings.Contains(out, "Foreman (") {
		t.Fatalf("unregistered repository:\n%s", out)
	}
	do("project", "import")
	if _, err := client.SetProjectForeman(ctx, importOrigin, "claude", "sonnet", "high"); err != nil {
		t.Fatal(err)
	}
	do("project", "set", "approval_max_tasks=7", "setup=make setup")
	// Provenance and values must come from the engine, even if the file was
	// subsequently changed without an import.
	write(projectFile("default_check: changed without import\n"))
	if err := os.Mkdir("nested", 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir("nested")
	out = configDo(t, ctx, "--state-dir", state)
	flat := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{
		fmt.Sprintf("runner: running (pid %d)", os.Getpid()),
		"claude sonnet effort: high writes code, reviews",
		"Foreman (change: brigada project foreman --harness H --model M [--effort E])",
		"claude/sonnet@high", "This project (example.invalid/imported)",
		"default_check: uv run pytest from .brigada/project.yaml",
		"approval_max_tasks: 7 set by hand", "setup: make setup set by hand",
		"approval_classes: implement+review+answer default",
		"change: edit .brigada/project.yaml, then brigada project import",
		"set by hand/local only: brigada project set key=value",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, key := range engine.SettingKeys {
		if key == engine.SettingDefaultCheck || key == engine.SettingApprovalMaxTasks {
			continue
		}
		if !strings.Contains(out, key+":") {
			t.Errorf("missing project setting %s", key)
		}
	}
	do("project", "set", "approval_max_tasks=")
	out = configDo(t, ctx, "--state-dir", state)
	if !strings.Contains(out, "approval_max_tasks: 5 default") {
		t.Fatalf("reset approval limit did not show the default:\n%s", out)
	}
	if _, err := client.ArchiveProject(ctx, importOrigin, false); err != nil {
		t.Fatal(err)
	}
	out = configDo(t, ctx, "--state-dir", state)
	if !strings.Contains(out, "This project: run inside") || strings.Contains(out, "Foreman (") {
		t.Fatalf("archived repository still shown as registered:\n%s", out)
	}
}

func TestConfigDefaultsAndLongLines(t *testing.T) {
	fakeInitPATH(t)
	t.Chdir(t.TempDir())
	state := testsupport.ShortStateDir(t)
	out := configDo(t, context.Background(), "--state-dir", state)
	if !strings.Contains(out, "no saved agents; run brigada init") || !strings.Contains(out, "notify = auto (default)") {
		t.Fatalf("missing defaults:\n%s", out)
	}
	if !strings.Contains(out, "tier.<harness>/<model> = from the model name (default)") {
		t.Fatalf("missing tier default:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(state, "runner")); !os.IsNotExist(err) {
		t.Errorf("reading absent agents created runner directory: %v", err)
	}
	if err := runnerstate.SaveAgents(filepath.Join(state, "runner"), []string{
		"harness=opencode,model=provider/" + strings.Repeat("long-model", 20) + ",effort=high,classes=review,account=" + strings.Repeat("a", 120),
	}); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), strings.Repeat("long-config-", 15))
	if err := os.WriteFile(config, []byte("idle_minutes = 0\ntools.claude/sonnet = Bash("+strings.Repeat("long-command", 20)+")\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out = configDo(t, context.Background(), "--state-dir", state, "--config", config)
	if !strings.Contains(out, "idle_minutes = 30 (default; file has 0)") {
		t.Fatalf("zero idle is not the engine default:\n%s", out)
	}
}

func TestConfigProjectOutput(t *testing.T) {
	var out bytes.Buffer
	printConfigProjectSettings(&out, engine.ProjectSettings{
		RepoURL: "github.com/example/app",
		Settings: []engine.SettingValue{
			{Key: engine.SettingDefaultCheck, Value: "uv run pytest", Source: engine.SourceImported},
			{Key: engine.SettingApprovalMaxTasks, Value: "5", Default: true, Source: engine.SourceDefault},
			{Key: engine.SettingSetup, Value: "make setup", Source: engine.SourceLocal},
			{Key: engine.SettingExtraRepos, Value: strings.Repeat("very-long-repository/", 10), Source: engine.SourceImported},
		},
	}, engine.ForemanSetting{Harness: "claude", Model: "sonnet", Effort: "high"})
	assertConfigWidth(t, out.String())
	for _, want := range []string{
		"Foreman (change: brigada project foreman --harness H --model M\n         [--effort E])\n  claude/sonnet@high\n",
		"This project (github.com/example/app)\n",
		"  default_check: uv run pytest from .brigada/project.yaml\n",
		"  approval_max_tasks: 5 default\n",
		"  setup: make setup set by hand\n",
		"  change: edit .brigada/project.yaml, then brigada project import\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
}

func TestConfigRefusesBadInputs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"extra"}, 2, "Run 'brigada config --help' for usage."},
		{[]string{"--bad-flag"}, 2, "Run 'brigada config --help' for usage."},
		{[]string{"--config", filepath.Join(t.TempDir(), "missing")}, 1, "read config"},
	} {
		var out, errs bytes.Buffer
		if code := run(context.Background(), append([]string{"config"}, tc.args...), &out, &errs); code != tc.code || !strings.Contains(errs.String(), tc.want) || out.Len() != 0 {
			t.Errorf("%v: code=%d stdout=%q stderr=%q", tc.args, code, out.String(), errs.String())
		}
	}
}
