package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
	"github.com/Blackfire-AI/brigada/internal/term"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

func initTasksArgs(extra ...string) []string {
	return append([]string{
		"--agent", "harness=claude", "--agent", "harness=codex",
		"--signed-in", "claude,codex", "--foreman", "claude/sonnet@medium",
		"--check", "true", "--tools", "none", "--no-wiring",
	}, extra...)
}

func TestInitTasksAtOnce(t *testing.T) {
	oldCPU := initCPUCount
	initCPUCount = func() int { return 14 }
	t.Cleanup(func() { initCPUCount = oldCPU })
	t.Run("default and installed CLI order", func(t *testing.T) {
		importRepo(t)
		fakeInitPATH(t, initHarnessesNoDsh...)
		state := filepath.Join(testsupport.ShortStateDir(t), "absent")
		code, out, errs := initDo(t, state, "\nn\n", initTasksArgs()...)
		flat := strings.Join(strings.Fields(out), " ")
		for _, want := range []string{
			"Four short steps follow.", "Step 3 of 4: tasks at once",
			"This one has 14 cores.", "Tasks at once [4]:",
			"claude 3, codex 2, kimi 1, opencode 2, brigada's defaults",
			"At once: 4 tasks on this machine; per account: claude 3, codex 2",
		} {
			if !strings.Contains(flat, want) {
				t.Errorf("missing %q:\n%s", want, out)
			}
		}
		if code != 1 || errs != "" || !strings.Contains(out, "Declined") {
			t.Fatalf("code=%d\n%s\n%s", code, out, errs)
		}
		for _, line := range strings.Split(out, "\n") {
			if term.VisibleWidth(line) >= 80 {
				t.Errorf("line reaches 80 columns: %q", line)
			}
		}
		for _, path := range []string{state, ".brigada", lifecycle.DefaultConfigPath()} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("refusal wrote %s: %v", path, err)
			}
		}
	})
	for _, tc := range []struct{ cores, want int }{
		{0, 1}, {1, 1}, {3, 1}, {6, 3}, {14, 4},
	} {
		t.Run(fmt.Sprintf("cores=%d", tc.cores), func(t *testing.T) {
			initCPUCount = func() int { return tc.cores }
			defer func() { initCPUCount = func() int { return 14 } }()
			importRepo(t)
			fakeInitPATH(t, "claude", "codex")
			code, out, errs := initDo(t, testsupport.ShortStateDir(t), "\nn\n", initTasksArgs()...)
			if code != 1 || errs != "" || !strings.Contains(out, fmt.Sprintf("Tasks at once [%d]:", tc.want)) {
				t.Fatalf("code=%d\n%s\n%s", code, out, errs)
			}
		})
	}
	t.Run("retry limit", func(t *testing.T) {
		importRepo(t)
		fakeInitPATH(t, "claude", "codex")
		state := filepath.Join(testsupport.ShortStateDir(t), "absent")
		code, out, errs := initDo(t, state, "0\nx\n100\n", initTasksArgs()...)
		if code != 1 || !strings.Contains(errs, "--parallel N") ||
			strings.Count(out, "Tasks at once [4]:") != 3 ||
			strings.Count(out, "try again") != 2 || strings.Contains(out, "\nPlan\n") {
			t.Fatalf("code=%d\n%s\n%s", code, out, errs)
		}
		if _, err := os.Stat(state); !os.IsNotExist(err) {
			t.Fatalf("bad answers wrote state: %v", err)
		}
	})
	t.Run("retry succeeds and clamps", func(t *testing.T) {
		importRepo(t)
		fakeInitPATH(t, "claude", "codex")
		code, out, errs := initDo(t, testsupport.ShortStateDir(t), "x\n1\nn\n", initTasksArgs()...)
		flat := strings.Join(strings.Fields(out), " ")
		if code != 1 || errs != "" || !strings.Contains(flat,
			"At once: 1 tasks on this machine; per account: claude 1, codex 1") {
			t.Fatalf("code=%d\n%s\n%s", code, out, errs)
		}
	})
	t.Run("saved choice and flag", func(t *testing.T) {
		importRepo(t)
		fakeInitPATH(t, "claude", "codex")
		state := testsupport.ShortStateDir(t)
		if err := runnerstate.SaveParallel(filepath.Join(state, "runner"), 2); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			extra []string
			input string
			want  int
			code  int
		}{
			{nil, "\nn\n", 2, 1},
			{[]string{"--probe"}, "", 2, 0},
			{[]string{"--parallel", "3"}, "n\n", 3, 1},
			{[]string{"--probe", "--parallel", "3"}, "", 3, 0},
		} {
			code, out, errs := initDo(t, state, tc.input, initTasksArgs(tc.extra...)...)
			if code != tc.code || errs != "" || !strings.Contains(out,
				fmt.Sprintf("At once: %d tasks", tc.want)) {
				t.Fatalf("%v: code=%d\n%s\n%s", tc.extra, code, out, errs)
			}
			if len(tc.extra) == 0 {
				if !strings.Contains(out, "Tasks at once [2]:") {
					t.Fatalf("saved default missing:\n%s", out)
				}
			} else if strings.Contains(out, "Tasks at once [") {
				t.Fatalf("flag/probe asked the question:\n%s", out)
			}
		}
	})
	t.Run("invalid flags", func(t *testing.T) {
		importRepo(t)
		fakeInitPATH(t, "claude", "codex")
		for _, answer := range []string{"0", "-1", "65", "x"} {
			code, out, errs := initDo(t, testsupport.ShortStateDir(t), "",
				initTasksArgs("--parallel", answer, "--probe")...)
			if code == 0 || !strings.Contains(errs, "parallel") || strings.Contains(out, "\nPlan\n") {
				t.Fatalf("%s: code=%d\n%s\n%s", answer, code, out, errs)
			}
		}
	})
	t.Run("symlink before any write", func(t *testing.T) {
		importRepo(t)
		fakeInitPATH(t, "claude", "codex")
		dir := t.TempDir()
		target, link := filepath.Join(dir, "target"), filepath.Join(dir, "config")
		const old = "# keep\nnotify = none\n"
		if err := os.WriteFile(target, []byte(old), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		state := filepath.Join(testsupport.ShortStateDir(t), "absent")
		code, out, errs := initDo(t, state, "", initTasksArgs("--yes", "--config", link)...)
		if code != 1 || !strings.Contains(errs, "symlink") {
			t.Fatalf("code=%d\n%s\n%s", code, out, errs)
		}
		data, _ := os.ReadFile(target)
		if string(data) != old {
			t.Fatalf("target changed: %q", data)
		}
		for _, path := range []string{state, ".brigada", ".mcp.json", ".claude"} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("symlink refusal wrote %s: %v", path, err)
			}
		}
	})
}

func TestInitTasksAtOnceApply(t *testing.T) {
	binary := testsupport.BuildBrigada(t)
	oldBinary := initBinary
	t.Cleanup(func() { initBinary = oldBinary })
	importRepo(t)
	fakeInitPATH(t, "claude", "codex")
	// Record argv at the process boundary, then run the real detached roles.
	calls := filepath.Join(t.TempDir(), "runner-args")
	t.Setenv("INIT_RUNNER_ARGS", calls)
	wrapper := filepath.Join(t.TempDir(), "brigada")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$INIT_RUNNER_ARGS\"\nexec '" +
		strings.ReplaceAll(binary, "'", "'\\''") + "' \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	initBinary = wrapper
	state := testsupport.ShortStateDir(t)
	config := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(config, []byte("# my settings\nnotify = none\n\ncaps.global = 9\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if code, out, errs := stopDo(t, state, "--engine"); code != 0 {
			t.Errorf("cleanup: %d\n%s\n%s", code, out, errs)
		}
	})
	args := initTasksArgs("--config", config)
	code, out, errs := initDo(t, state, "2\ny\n", args...)
	if code != 0 || errs != "" || !strings.Contains(out, "Detached runner started") {
		t.Fatalf("code=%d\n%s\n%s", code, out, errs)
	}
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# my settings\nnotify = none\n\n", "caps.global = 2\n",
		"caps.plan = 3\n", "caps.account.claude = 2\n", "caps.account.codex = 2\n"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("missing %q in %s", want, data)
		}
	}
	argv, _ := os.ReadFile(calls)
	if !strings.Contains(string(argv), "--max-parallel 2") {
		t.Fatalf("runner args: %s", argv)
	}
	rec, live, err := runnerstate.Running(filepath.Join(state, "runner"))
	if err != nil || !live || rec.MaxParallel != 2 {
		t.Fatalf("runner=%+v live=%v err=%v", rec, live, err)
	}
	if saved, err := runnerstate.LoadParallel(filepath.Join(state, "runner")); saved != 2 || err != nil {
		t.Fatalf("saved=%d err=%v", saved, err)
	}
	code, out, errs = initDo(t, state, "\nn\n", args...)
	if code != 1 || errs != "" || !strings.Contains(out, "Tasks at once [2]:") {
		t.Fatalf("rerun: %d\n%s\n%s", code, out, errs)
	}
	code, out, errs = initDo(t, state, "", append(args, "--yes", "--parallel", "3")...)
	flat := strings.Join(strings.Fields(out), " ")
	if code != 0 || errs != "" || !strings.Contains(out, "Runner restarted") ||
		!strings.Contains(flat, "the engine reads caps when it starts: run brigada stop --engine, then any command starts it with the new values") {
		t.Fatalf("changed limit: %d\n%s\n%s", code, out, errs)
	}
	again, live, err := runnerstate.Running(filepath.Join(state, "runner"))
	if err != nil || !live || again.PID == rec.PID || again.MaxParallel != 3 {
		t.Fatalf("runner not restarted with capacity 3: %+v live=%v err=%v", again, live, err)
	}
	code, out, errs = initDo(t, state, "", append(args, "--yes")...)
	if code != 0 || errs != "" || !strings.Contains(out, "Runner already running") ||
		strings.Contains(out, "the engine reads caps") || !strings.Contains(out, "At once: 3 tasks") {
		t.Fatalf("saved --yes: %d\n%s\n%s", code, out, errs)
	}
	// Config's parsed values agree with the persisted runner choice.
	cfg, err := lifecycle.LoadConfigFile(config)
	if err != nil || cfg.Caps.Global == nil || strconv.Itoa(*cfg.Caps.Global) != "3" {
		t.Fatalf("config=%+v err=%v", cfg, err)
	}
	if !lifecycle.EngineRunning(context.Background(), lifecycle.PathsOf(state).Socket) {
		t.Fatal("engine stopped during init")
	}
}
