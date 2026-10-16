package main

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// startSettingsEngine runs a real engine in-process and returns its
// state directory.
func startSettingsEngine(t *testing.T) (context.Context, string) {
	t.Helper()
	state := testsupport.ShortStateDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- lifecycle.Run(ctx, lifecycle.Config{StateDir: state, Idle: time.Minute}) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("the engine stopped with an error: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the engine did not stop")
		}
	})
	socket := lifecycle.PathsOf(state).Socket
	deadline := time.Now().Add(10 * time.Second)
	for !lifecycle.EngineRunning(ctx, socket) {
		if time.Now().After(deadline) {
			t.Fatal("the engine did not answer in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return ctx, state
}

// holdDefaults are the environment step's settings (§5.7, §5.8), the
// interactive hold's two bounds (§4.6), the foreman wake budget (§4.3)
// and the project instructions (§5.9) at their defaults, the last lines
// of every settings print.
const holdDefaults = "setup=none (default)\n" + "setup_cache_key_files=none (default)\n" + "setup_timeout_minutes=15 (default)\n" +
	"extra_repos=none (default)\n" + "hold_idle_minutes=15 (default)\n" + "hold_max_minutes=240 (default)\n" +
	"foreman_wakes_per_day=20 (default)\n" + "instructions=none (default)\n"

const validSettingKeys = "approval_max_tasks, approval_classes, approval_protected_paths, default_check, worker_tools, setup, setup_cache_key_files, setup_timeout_minutes, extra_repos, hold_idle_minutes, hold_max_minutes, foreman_wakes_per_day, instructions"

// TestProjectSettingsGolden covers project set and show: the defaults,
// one override, the reset, and the usage errors for a bad key and value.
func TestProjectSettingsGolden(t *testing.T) {
	ctx, state := startSettingsEngine(t)
	repo := "https://example.invalid/settings.git"

	do := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		// Flags precede the key=value pairs: the first positional ends them.
		full := append([]string{args[0], args[1], "--state-dir", state}, args[2:]...)
		code := run(ctx, full, &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}

	// No project yet: show says so.
	if code, _, stderr := do("project", "show", "--repo", repo); code != 1 || !strings.Contains(stderr, "no project for repo "+repo) {
		t.Fatalf("show without a project: code %d, stderr %q", code, stderr)
	}

	// An override creates the project; the others stay defaults.
	code, stdout, stderr := do("project", "set", "--repo", repo, "approval_protected_paths=infra/**+db/*.sql")
	if code != 0 {
		t.Fatalf("set exit = %d, stderr = %q", code, stderr)
	}
	want := "approval_max_tasks=5 (default)\n" +
		"approval_classes=implement+review+answer (default)\n" +
		"approval_protected_paths=infra/**+db/*.sql (set)\n" +
		"default_check=none (default)\n" +
		"worker_tools=agent default (default)\n" + holdDefaults
	if stdout != want {
		t.Fatalf("set printed %q, want %q", stdout, want)
	}

	// Show reads the same back.
	if code, stdout, stderr = do("project", "show", "--repo", repo); code != 0 || stdout != want {
		t.Fatalf("show = %d %q (stderr %q), want %q", code, stdout, stderr, want)
	}

	// Two keys at once; an unnamed key keeps its value.
	code, stdout, stderr = do("project", "set", "--repo", repo, "approval_max_tasks=0", "approval_classes=implement")
	want = "approval_max_tasks=0 (set)\n" +
		"approval_classes=implement (set)\n" +
		"approval_protected_paths=infra/**+db/*.sql (set)\n" +
		"default_check=none (default)\n" +
		"worker_tools=agent default (default)\n" + holdDefaults
	if code != 0 || stdout != want {
		t.Fatalf("second set = %d %q (stderr %q), want %q", code, stdout, stderr, want)
	}

	// An empty value resets to the default.
	code, stdout, stderr = do("project", "set", "--repo", repo, "approval_max_tasks=", "approval_classes=", "approval_protected_paths=")
	want = "approval_max_tasks=5 (default)\n" +
		"approval_classes=implement+review+answer (default)\n" +
		"approval_protected_paths=none (default)\n" +
		"default_check=none (default)\n" +
		"worker_tools=agent default (default)\n" + holdDefaults
	if code != 0 || stdout != want {
		t.Fatalf("reset = %d %q (stderr %q), want %q", code, stdout, stderr, want)
	}

	// A bad key is a usage error naming the valid keys.
	code, _, stderr = do("project", "set", "--repo", repo, "approval_max=3")
	wantErr := `brigada: invalid project setting: unknown key "approval_max"; valid keys: ` + validSettingKeys + "\n"
	if code != 2 || stderr != wantErr {
		t.Fatalf("bad key = %d %q, want 2 %q", code, stderr, wantErr)
	}

	// A bad value is a usage error naming what the key takes.
	code, _, stderr = do("project", "set", "--repo", repo, "approval_max_tasks=many")
	wantErr = `brigada: invalid project setting: approval_max_tasks takes an integer (0 = every plan waits, negative = off), got "many"; valid keys: ` + validSettingKeys + "\n"
	if code != 2 || stderr != wantErr {
		t.Fatalf("bad value = %d %q, want 2 %q", code, stderr, wantErr)
	}

	// Nothing was stored by the refused calls.
	if code, stdout, _ = do("project", "show", "--repo", repo); code != 0 || strings.Contains(stdout, "(set)") {
		t.Fatalf("show after refusals = %d %q, want all defaults", code, stdout)
	}

	// A pair without "=" and a set without pairs are usage errors too.
	if code, _, stderr = do("project", "set", "--repo", repo, "approval_max_tasks"); code != 2 || !strings.Contains(stderr, "key=value") {
		t.Fatalf("no equals = %d %q", code, stderr)
	}
	if code, _, stderr = do("project", "set", "--repo", repo); code != 2 || !strings.Contains(stderr, "valid keys: approval_max_tasks") {
		t.Fatalf("no pairs = %d %q", code, stderr)
	}
}

// TestProjectSettingsDefaultRepo takes the repo from the working
// directory's origin.
func TestProjectSettingsDefaultRepo(t *testing.T) {
	ctx, state := startSettingsEngine(t)
	repo := "https://example.invalid/origin.git"
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", repo}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Skipf("git %v: %v: %s", args, err, out)
		}
	}
	t.Chdir(dir)
	var stdout, stderr bytes.Buffer
	if code := run(ctx, []string{"project", "set", "--state-dir", state, "approval_max_tasks=-1"}, &stdout, &stderr); code != 0 {
		t.Fatalf("set exit = %d, stderr = %q", code, stderr.String())
	}
	stdout.Reset()
	if code := run(ctx, []string{"project", "show", "--repo", repo, "--state-dir", state}, &stdout, &stderr); code != 0 ||
		!strings.HasPrefix(stdout.String(), "approval_max_tasks=-1 (set)\n") {
		t.Fatalf("show = %d %q, stderr %q", code, stdout.String(), stderr.String())
	}
}
